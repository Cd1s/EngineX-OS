package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// One-time insert-time repair for old-version stock Engine OfflineAnalyzer overwrites across
// all 16 models (both arm32 OnDemand and arm64 Gen-2). Runs strictly inside rbstate.LockVolume
// and the 4 s insertBudget:
//   - Clean sticks cost only the read-only SQL prefilter (~1.4 ms on 2,440 rows, 0 writes).
//   - Matching rows (verified through enginedb.IsRepairableStockOverwritten, including
//     filled.jsonl exclusion and the maintainer's idle-settled restore rule) are repaired once
//     in place to full rekordbox performance data (PQTZ grid, BPM, cues, loops, waveforms, RGB)
//     and marked PerfApplied with Written/Built set so they are never touched again.
//   - If an extreme stick overflows the 4 s insertBudget: !OnDemand hands leftover rows to
//     FillInBackground, while OnDemand resets leftover rows to clean untouched import stubs
//     so Step 2's normal on-demand load hook fills them on load without a 24-min background pass.

var (
	repairItemReserve   = 500 * time.Millisecond
	repairCommitReserve = 750 * time.Millisecond // bounded fallback window reserved inside insertBudget for stub reset + commit
	repairFlushReserve  = 250 * time.Millisecond // minimum margin left before finishLimit required to open/commit m.db
	repairBeforeBuild   func(ctx context.Context, key string) error
	errPendingSave      = errors.New("rb-bridge: save pending repair snapshot")
)

// repairDeadlines computes the hard commit ceiling (finishLimit), the earlier inline-build
// cutoff (inlineLimit = finishLimit - commitReserve) that reserves a bounded fallback window
// inside the budget for OnDemand stub reset + commit, and the minimum flush reserve.
func repairDeadlines(ctx context.Context, st *steps) (finishLimit, inlineLimit time.Time, flushRes time.Duration) {
	if !st.optional.IsZero() {
		finishLimit = st.optional
	}
	if dl, ok := ctx.Deadline(); ok && (finishLimit.IsZero() || dl.Before(finishLimit)) {
		finishLimit = dl
	}
	if finishLimit.IsZero() {
		return time.Time{}, time.Time{}, 0
	}
	commitRes := repairCommitReserve
	if insertBudget > 0 && insertBudget/2 < commitRes {
		commitRes = insertBudget / 2
	}
	flushRes = repairFlushReserve
	if commitRes/2 < flushRes {
		flushRes = commitRes / 2
	}
	inlineLimit = finishLimit.Add(-commitRes)
	return finishLimit, inlineLimit, flushRes
}

func saveDurablePending(parentCtx context.Context, dir string, snap *rbstate.Snapshot, st *steps) error {
	if snap == nil || parentCtx.Err() != nil {
		return parentCtx.Err()
	}
	if !st.kill.IsZero() && time.Until(st.kill) < saveReserve {
		return errWindowDeadline
	}
	saveCtx, cancel := context.WithTimeout(parentCtx, saveReserve)
	defer cancel()
	return snap.SaveCtx(saveCtx, dir)
}

// hasPendingStockRepair reports whether an OnDemand stick with durably recorded Pending.Perf > 0
// still has stock-OA-overwritten rows needing repair (used by FillInBackground to decide whether
// to build a clean next library while idle).
func hasPendingStockRepair(ctx context.Context, dbDir string, snap *rbstate.Snapshot) bool {
	if snap == nil || snap.Pending.Perf <= 0 || snap.LibraryUUID == "" || snap.Rules != rbstate.Rules {
		return false
	}
	liveUUID, candidates, err := enginedb.ScanStockOACandidatesRO(ctx, filepath.Join(dbDir, "m.db"))
	if err != nil || liveUUID != snap.LibraryUUID || len(candidates) == 0 {
		return false
	}
	filled := loadFilledJournal(liveUUID)
	return len(filterStockRepairMatches(candidates, snap, filled)) > 0
}

type stockRepairMatch struct {
	cand  enginedb.StockOACandidate
	track *rbstate.Track
}

type builtStockRepair struct {
	match stockRepairMatch
	rec   *rbstate.Staged
	rt    rekordbox.Track
}

func cleanCandidateRel(p string) string {
	clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "../")
	return strings.TrimPrefix(path.Clean("/"+clean), "/")
}

func loadFilledJournal(uuid string) map[string]rbstate.Filled {
	if !validStageUUID(uuid) {
		return nil
	}
	root, err := rbstate.StageRoot()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(root, uuid, "filled.jsonl"))
	if err != nil {
		return nil
	}
	out := map[string]rbstate.Filled{}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var f struct {
			ID   int64  `json:"id"`
			K    string `json:"k"`
			WR   []byte `json:"wr"`
			WRNM []byte `json:"wrnm"`
		}
		if json.Unmarshal(line, &f) != nil || f.K == "" || (len(f.WR) == 0 && len(f.WRNM) == 0) {
			continue
		}
		wr := f.WR
		if len(wr) == 0 {
			wr = f.WRNM
		}
		out[f.K] = rbstate.Filled{EngineID: f.ID, Key: f.K, Written: wr}
	}
	return out
}

func filterStockRepairMatches(candidates []enginedb.StockOACandidate, snap *rbstate.Snapshot, filled map[string]rbstate.Filled) []stockRepairMatch {
	if len(candidates) == 0 || snap == nil {
		return nil
	}
	var out []stockRepairMatch
	for _, c := range candidates {
		key := rbstate.FoldPath(cleanCandidateRel(c.Path))
		t := snap.Tracks[key]
		if t == nil || t.EngineID != c.ID {
			continue
		}
		f, hasFilled := filled[key]
		inFilled := hasFilled && (f.EngineID == 0 || f.EngineID == c.ID)
		row := c.Row
		row.FastKnown = t.FastKnown
		row.FastCues = t.FastCues
		row.FastLoops = t.FastLoops
		row.FastActive = t.FastActive
		prov := enginedb.TrackRepairProvenance(t, inFilled, c.Genre, c.Comment, c.Label, c.Row.QuickCues, c.Row.Loops, c.Row.ActiveOnLoadLoops)
		if !enginedb.IsRepairableStockOverwritten(row, prov) {
			continue
		}
		out = append(out, stockRepairMatch{cand: c, track: t})
	}
	return out
}

func findPDBTrackForMount(ctx context.Context, mount string, r *compat.Resolver, tr *rbstate.Track) (rekordbox.Track, bool) {
	if tr == nil || prolink.GeneratedExport(mount) {
		return rekordbox.Track{}, false
	}
	rel, err := r.ResolveContext(ctx, devicePDB)
	if err != nil {
		return rekordbox.Track{}, false
	}
	f, err := os.Open(r.FullPath(rel))
	if err != nil {
		return rekordbox.Track{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > onDemandMaxExportDBBytes {
		return rekordbox.Track{}, false
	}
	var wantDeviceID uint32
	for _, ref := range tr.Refs {
		if ref.Kind == rbstate.DeviceLibrary && ref.ID != 0 {
			wantDeviceID = ref.ID
			break
		}
	}
	found, ok, err := rekordbox.FindPDBTrack(&ctxReaderAt{ctx: ctx, r: f, mount: mount}, st.Size(), func(id uint32, trackPath string) bool {
		if wantDeviceID != 0 && id == wantDeviceID && rbstate.FoldPath(trackPath) == tr.Key {
			return true
		}
		return rbstate.FoldPath(trackPath) == tr.Key
	})
	if err != nil || !ok {
		return rekordbox.Track{}, false
	}
	return found, true
}

func extractFastCues(tr *rbstate.Track, sr int, dur float64) []rekordbox.Cue {
	if tr == nil || !tr.FastKnown || (len(tr.FastCues) == 0 && len(tr.FastLoops) == 0) {
		return nil
	}
	var a rekordbox.Analysis
	mergeFastCues(&a, tr, sr, 0, dur)
	return a.Cues
}

func buildStockRepairTrack(ctx context.Context, mount string, r *compat.Resolver, cand enginedb.StockOACandidate, tr *rbstate.Track, vt *ViewTrack) (rec *rbstate.Staged, rt rekordbox.Track, ok bool, deferBG bool, err error) {
	if vt != nil {
		rt = vt.Track
	} else {
		rel := tr.Rel
		if rel == "" {
			rel = cleanCandidateRel(cand.Path)
		}
		rt = rekordbox.Track{
			ID:           uint32(tr.EngineID),
			Path:         "/" + rel,
			AnalysisPath: tr.AnalysisPath,
			ArtworkPath:  tr.ArtworkPath,
			Duration:     tr.Duration,
			SampleRate:   tr.SampleRate,
			Genre:        cand.Genre,
			Comment:      cand.Comment,
			Label:        cand.Label,
		}
		if rt.Duration <= 0 && cand.Length > 0 {
			rt.Duration = float64(cand.Length)
		}
		if cand.Key >= 0 && cand.Key < len(engineKeyToCamelot) {
			rt.Key = engineKeyToCamelot[cand.Key]
		}
		if rt.AnalysisPath == "" {
			if found, fok := findPDBTrackForMount(ctx, mount, r, tr); fok {
				if found.AnalysisPath != "" {
					rt.AnalysisPath = found.AnalysisPath
				}
				if rt.ArtworkPath == "" {
					rt.ArtworkPath = found.ArtworkPath
				}
				if rt.Duration <= 0 {
					rt.Duration = found.Duration
				}
				if rt.SampleRate <= 0 {
					rt.SampleRate = found.SampleRate
				}
				if rt.BPM <= 0 {
					rt.BPM = found.BPM
				}
				if rt.Key == "" {
					rt.Key = found.Key
				}
				if len(found.Cues) > 0 {
					rt.Cues = found.Cues
				}
			}
		}
		if rt.AnalysisPath == "" {
			// Only defer to FillInBackground for a legacy pre-hydration OneLibrary snapshot
			// that lacked AnalysisPath/Duration/SampleRate; otherwise the track genuinely has
			// no rekordbox ANLZ to repair from.
			hasOneLib := false
			for _, ref := range tr.Refs {
				if ref.Kind == rbstate.OneLibrary {
					hasOneLib = true
					break
				}
			}
			if hasOneLib && tr.Duration == 0 && tr.SampleRate == 0 {
				return nil, rt, false, true, nil
			}
			return nil, rt, false, false, nil
		}
		if len(rt.Cues) == 0 && tr.FastKnown && (len(tr.FastCues) > 0 || len(tr.FastLoops) > 0) {
			rt.Cues = extractFastCues(tr, rt.SampleRate, rt.Duration)
		}
	}

	src, err := enginedb.StatTrack(ctx, r, rt)
	if err != nil {
		if ctx.Err() != nil {
			return nil, rt, false, true, ctx.Err()
		}
		return nil, rt, false, true, nil
	}
	if src.Missing || src.Broken != nil {
		return nil, rt, false, false, nil
	}
	src.Sig.Row = tr.Row

	rec, err = enginedb.BuildTrack(r, rt, src, enginedb.ReadOptions{
		Pause: func(int, time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, rt, false, true, ctx.Err()
		}
		return nil, rt, false, true, nil
	}
	if rec == nil || rec.Missing || rec.Degraded != "" || !rec.Perf.Complete || len(rec.Perf.TrackData) == 0 || len(rec.Perf.Beats) == 0 {
		return nil, rt, false, false, nil
	}
	if rec.Facts.BPM <= 0 && rt.BPM <= 0 {
		if bpm := enginedb.BeatDataFirstBPM(rec.Perf.Beats); bpm > 0 {
			rec.Facts.BPM = bpm
		} else {
			rt.BPM = cand.BPM
		}
	}
	return rec, rt, true, false, nil
}

func applyBuiltStockRepairs(ctx context.Context, b *enginedb.Batch, built []builtStockRepair) (repaired int, overviews map[int64][]byte, err error) {
	overviews = make(map[int64][]byte, len(built))
	for _, item := range built {
		t := item.match.track
		c := item.match.cand
		prevBuiltANLZ := len(t.Built.ANLZ) > 0
		prev := enginedb.PerfPrev{
			State:             t.Perf,
			Written:           t.Written,
			FastKnown:         t.FastKnown,
			FastCues:          t.FastCues,
			FastLoops:         t.FastLoops,
			FastActive:        t.FastActive,
			Edit:              t.Edit,
			Synced:            t.Synced,
			HasBuiltANLZ:      prevBuiltANLZ,
			HasSyncedColorTag: len(t.SyncedColorTag) > 0 || len(t.SyncedHotColors) > 0 || len(t.SyncedLoopColors) > 0,
		}
		p := &item.rec.Perf
		perf := enginedb.Performance{
			TrackData:   p.TrackData,
			Overview:    p.Overview,
			Beats:       p.Beats,
			QuickCues:   p.QuickCues,
			Loops:       p.Loops,
			ActiveLoops: p.ActiveLoops,
			Complete:    p.Complete,
			Digest:      item.rec.Digest,
		}
		digest, outcome, aerr := b.ApplyPerformance(ctx, c.ID, perf, item.rec.Facts, prev, item.rt)
		if aerr != nil {
			return repaired, nil, aerr
		}
		if outcome != enginedb.PerfWritten {
			continue
		}
		if item.rec.Rel != "" && item.rec.Rel != t.Rel && rbstate.FoldPath(item.rec.Rel) == t.Key {
			if merr := b.MoveTracks(ctx, map[int64]string{c.ID: item.rec.Rel}); merr != nil {
				return repaired, nil, merr
			}
			t.Rel = item.rec.Rel
		}
		t.Resolved = true
		t.Built = item.rec.Sig
		t.Written = digest
		t.Perf = rbstate.PerfApplied
		t.FastCues, t.FastLoops, t.FastActive, t.FastKnown = nil, nil, 0, false
		if len(item.rec.Art) > 0 {
			if aerr := b.PutArtwork(ctx, c.ID, item.rec.Art); aerr != nil {
				log.Printf("rb-bridge: cover of repaired track %d not written: %v", c.ID, aerr)
			}
		}
		newEdit, newDigest, serr := b.Synced(ctx, c.ID)
		if serr != nil {
			return repaired, nil, serr
		}
		tagsInSync := len(t.MetaDigest) > 0 && bytes.Equal(t.MetaDigest, enginedb.MetadataDigest(c.Genre, c.Comment, c.Label))
		if c.LastEditTime == t.Edit || tagsInSync {
			t.Edit, t.Synced = newEdit, newDigest
		} else {
			t.Edit, t.Synced = newEdit, nil
		}
		if len(item.rec.RGB) > 0 {
			overviews[c.ID] = item.rec.RGB
		}
		repaired++
	}
	return repaired, overviews, nil
}

// resetRemainingToImportStub resets over-budget OnDemand rows back to their clean untouched
// import stub so Step 2's normal on-demand load hook fills them cleanly on load without
// triggering a full background perf pass. Stops before finishLimit - flushRes (or saveReserve
// before kill) and returns any un-reset tail so the caller can commit inside the budget and
// record leftovers durably.
func resetRemainingToImportStub(ctx context.Context, b *enginedb.Batch, remaining []stockRepairMatch, finishLimit time.Time, flushRes time.Duration, st *steps) (int, []stockRepairMatch, error) {
	resetCount := 0
	for i, rem := range remaining {
		if ctx.Err() != nil ||
			(!finishLimit.IsZero() && time.Until(finishLimit) < flushRes) ||
			(!st.kill.IsZero() && time.Until(st.kill) < saveReserve) {
			return resetCount, remaining[i:], nil
		}
		t := rem.track
		c := rem.cand
		if err := b.ResetToImportStub(ctx, c.ID, t.FastCues, t.FastLoops, t.FastActive); err != nil {
			return resetCount, remaining[i:], err
		}
		newEdit, newDigest, err := b.Synced(ctx, c.ID)
		if err != nil {
			return resetCount, remaining[i:], err
		}
		t.Perf = rbstate.PerfNone
		t.Built = rbstate.TrackSig{}
		t.Written = nil
		tagsInSync := len(t.MetaDigest) > 0 && bytes.Equal(t.MetaDigest, enginedb.MetadataDigest(c.Genre, c.Comment, c.Label))
		if c.LastEditTime == t.Edit || tagsInSync {
			t.Edit, t.Synced = newEdit, newDigest
		} else {
			t.Edit, t.Synced = newEdit, nil
		}
		resetCount++
	}
	return resetCount, nil, nil
}

func markRemainingForBackground(snap *rbstate.Snapshot, remaining []stockRepairMatch) {
	if len(remaining) == 0 || snap == nil {
		return
	}
	for _, rem := range remaining {
		if t := snap.Tracks[rem.track.Key]; t != nil {
			t.Perf = rbstate.PerfNone
			t.Built = rbstate.TrackSig{}
		}
	}
	if snap.Pending.Perf < len(remaining) {
		snap.Pending.Perf = len(remaining)
	}
}

func clearStaleOnDemandPending(parentCtx context.Context, dir string, snap *rbstate.Snapshot, st *steps) (rbstate.Pending, error) {
	if snap == nil || snap.Pending.Perf <= 0 {
		return rbstate.Pending{}, nil
	}
	snap.Pending.Perf = 0
	if err := saveDurablePending(parentCtx, dir, snap, st); err != nil {
		return rbstate.Pending{Perf: 1}, nil
	}
	return snap.Pending, nil
}

func repairStockOverwrittenAtMount(parentCtx, ctx context.Context, mount, dir string, r *compat.Resolver, head rbstate.Header, st *steps, onDemand bool, startBG func()) (repaired int, pending rbstate.Pending, err error) {
	if ctx.Err() != nil {
		return 0, head.Pending, ctx.Err()
	}
	if !st.kill.IsZero() && time.Until(st.kill) < saveReserve {
		return 0, head.Pending, nil
	}
	mdb := filepath.Join(dir, "m.db")
	liveUUID, candidates, serr := enginedb.ScanStockOACandidatesRO(ctx, mdb)
	if serr != nil {
		if ctx.Err() != nil {
			return 0, head.Pending, ctx.Err()
		}
		log.Printf("rb-bridge: mount repair prefilter skipped: %v", serr)
		return 0, head.Pending, nil
	}
	if liveUUID == "" || liveUUID != head.LibraryUUID {
		return 0, head.Pending, nil
	}
	if len(candidates) == 0 {
		if onDemand && head.Pending.Perf > 0 {
			if snap, lerr := rbstate.Load(dir); lerr == nil && snap.LibraryUUID == liveUUID && snap.Rules == rbstate.Rules {
				p, _ := clearStaleOnDemandPending(parentCtx, dir, snap, st)
				return 0, p, nil
			}
		}
		return 0, head.Pending, nil
	}
	snap, lerr := rbstate.Load(dir)
	if lerr != nil || snap.LibraryUUID != liveUUID || snap.Rules != rbstate.Rules {
		return 0, head.Pending, nil
	}
	filled := loadFilledJournal(liveUUID)
	matches := filterStockRepairMatches(candidates, snap, filled)
	if len(matches) == 0 {
		if onDemand && head.Pending.Perf > 0 {
			p, _ := clearStaleOnDemandPending(parentCtx, dir, snap, st)
			return 0, p, nil
		}
		return 0, head.Pending, nil
	}

	finishLimit, inlineLimit, flushRes := repairDeadlines(ctx, st)

	var built []builtStockRepair
	var remaining []stockRepairMatch
	for idx, m := range matches {
		if ctx.Err() != nil ||
			(!inlineLimit.IsZero() && !time.Now().Before(inlineLimit)) ||
			(!st.kill.IsZero() && !st.must(saveReserve)) {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		buildCtx := ctx
		var buildCancel context.CancelFunc
		if !inlineLimit.IsZero() {
			buildCtx, buildCancel = context.WithDeadline(ctx, inlineLimit)
		}
		if repairBeforeBuild != nil {
			herr := repairBeforeBuild(buildCtx, m.track.Key)
			if herr != nil {
				if buildCancel != nil {
					buildCancel()
				}
				if errors.Is(herr, errWindowDeadline) || errors.Is(herr, context.DeadlineExceeded) || buildCtx.Err() != nil {
					remaining = append(remaining, matches[idx:]...)
					break
				}
				return 0, head.Pending, herr
			}
			if ctx.Err() != nil ||
				(!inlineLimit.IsZero() && !time.Now().Before(inlineLimit)) ||
				(!st.kill.IsZero() && !st.must(saveReserve)) {
				if buildCancel != nil {
					buildCancel()
				}
				remaining = append(remaining, matches[idx:]...)
				break
			}
		}
		rec, rt, ok, deferBG, berr := buildStockRepairTrack(buildCtx, mount, r, m.cand, m.track, nil)
		if buildCancel != nil {
			buildCancel()
		}
		if berr != nil {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		if ok {
			built = append(built, builtStockRepair{match: m, rec: rec, rt: rt})
		} else if deferBG {
			remaining = append(remaining, m)
		}
	}

	if len(built) == 0 && len(remaining) == 0 {
		if onDemand && head.Pending.Perf > 0 {
			p, _ := clearStaleOnDemandPending(parentCtx, dir, snap, st)
			return 0, p, nil
		}
		return 0, head.Pending, nil
	}

	allWork := make([]stockRepairMatch, 0, len(built)+len(remaining))
	for _, item := range built {
		allWork = append(allWork, item.match)
	}
	allWork = append(allWork, remaining...)

	var lib *enginedb.Library
	var b *enginedb.Batch
	txDirty := false
	closeTx := func() {
		if b != nil {
			b.Rollback()
			b = nil
		}
		if lib != nil {
			_ = lib.Close()
			lib = nil
		}
	}
	defer closeTx()

	deferOutstanding := func(hardDeadline bool) (int, rbstate.Pending, error) {
		closeTx()
		if len(allWork) == 0 {
			return 0, head.Pending, nil
		}
		baseSnap := snap
		if txDirty {
			// A rolled-back transaction may have mutated snap in memory (Written, Edit, Synced,
			// Commit, Applied). Reload the last committed snapshot from disk so we never persist
			// uncommitted provenance.
			reloaded, rerr := rbstate.Load(dir)
			if rerr != nil {
				return 0, head.Pending, fmt.Errorf("%w: reload committed snapshot: %w", errPendingSave, rerr)
			}
			baseSnap = reloaded
		}
		markRemainingForBackground(baseSnap, allWork)
		if saveErr := saveDurablePending(parentCtx, dir, baseSnap, st); saveErr != nil {
			if parentCtx.Err() != nil {
				return 0, head.Pending, parentCtx.Err()
			}
			if errors.Is(saveErr, errWindowDeadline) {
				startBG()
				return 0, head.Pending, errWindowDeadline
			}
			return 0, head.Pending, fmt.Errorf("%w: %w", errPendingSave, saveErr)
		}
		startBG()
		st.done("repair", "0 inline, %d stock-overwritten rows recorded durably for background/next insert", len(allWork))
		if hardDeadline {
			return 0, baseSnap.Pending, errWindowDeadline
		}
		return 0, baseSnap.Pending, nil
	}

	// Non-OnDemand with nothing built inline: hand off directly to FillInBackground without opening m.db RW.
	if len(built) == 0 && !onDemand {
		return deferOutstanding(ctx.Err() != nil)
	}

	hardExpired := ctx.Err() != nil || (!st.kill.IsZero() && time.Until(st.kill) < saveReserve)
	softExhausted := !finishLimit.IsZero() && time.Until(finishLimit) < flushRes
	if hardExpired || softExhausted {
		return deferOutstanding(hardExpired)
	}

	commitCtx := ctx
	var commitCancel context.CancelFunc
	if !finishLimit.IsZero() {
		commitCtx, commitCancel = context.WithDeadline(ctx, finishLimit)
		defer commitCancel()
	}

	var oerr error
	lib, oerr = enginedb.OpenLibraryFile(commitCtx, mdb, mount)
	if oerr != nil {
		if errors.Is(oerr, context.DeadlineExceeded) || commitCtx.Err() != nil {
			return deferOutstanding(ctx.Err() != nil)
		}
		return 0, head.Pending, oerr
	}

	var berr error
	b, berr = lib.Begin(commitCtx)
	if berr != nil {
		if errors.Is(berr, context.DeadlineExceeded) || commitCtx.Err() != nil {
			return deferOutstanding(ctx.Err() != nil)
		}
		return 0, head.Pending, berr
	}

	txDirty = true
	repaired, overviews, aerr := applyBuiltStockRepairs(commitCtx, b, built)
	if aerr != nil {
		if errors.Is(aerr, context.DeadlineExceeded) || commitCtx.Err() != nil {
			return deferOutstanding(ctx.Err() != nil)
		}
		return 0, head.Pending, aerr
	}
	stubReset := 0
	if onDemand && len(remaining) > 0 {
		var unreset []stockRepairMatch
		stubReset, unreset, err = resetRemainingToImportStub(commitCtx, b, remaining, finishLimit, flushRes, st)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || commitCtx.Err() != nil {
				return deferOutstanding(ctx.Err() != nil)
			}
			return 0, head.Pending, err
		}
		remaining = unreset
	}
	if repaired == 0 && stubReset == 0 {
		if len(remaining) == 0 {
			return 0, head.Pending, nil
		}
		return deferOutstanding(ctx.Err() != nil)
	}
	if len(remaining) > 0 {
		markRemainingForBackground(snap, remaining)
	} else if onDemand {
		snap.Pending.Perf = 0
	}
	snap.Applied, snap.Commit = time.Now().Unix(), b.CommitID()
	if commitCtx.Err() != nil || (!st.kill.IsZero() && time.Until(st.kill) < saveReserve) {
		return deferOutstanding(ctx.Err() != nil || (!st.kill.IsZero() && time.Until(st.kill) < saveReserve))
	}
	if err = snap.SaveNextCtx(commitCtx, dir); err != nil {
		_ = rbstate.DropNext(dir)
		if errors.Is(err, context.DeadlineExceeded) || commitCtx.Err() != nil {
			return deferOutstanding(ctx.Err() != nil)
		}
		return 0, head.Pending, err
	}
	cut("repair next saved")
	if err = b.Commit(commitCtx); err != nil {
		_ = rbstate.DropNext(dir)
		if errors.Is(err, context.DeadlineExceeded) || commitCtx.Err() != nil {
			return deferOutstanding(ctx.Err() != nil)
		}
		return 0, head.Pending, err
	}
	b = nil
	cut("repair committed")
	promoteCtx, promoteCancel := context.WithTimeout(parentCtx, saveReserve)
	err = rbstate.PromoteCtx(promoteCtx, dir)
	promoteCancel()
	if err != nil {
		return repaired + stubReset, snap.Pending, err
	}
	cut("repair promoted")
	for id, body := range overviews {
		if commitCtx.Err() != nil || (!finishLimit.IsZero() && !time.Now().Before(finishLimit)) {
			break
		}
		writeOverview(dir, lib.UUID(), id, body)
	}
	if snap.Mode == rbstate.ModeOnDemand && commitCtx.Err() == nil &&
		(finishLimit.IsZero() || time.Now().Before(finishLimit)) &&
		(st.kill.IsZero() || time.Until(st.kill) > spare/2) {
		if serr := syncBrowseIndexCtx(commitCtx, dir, snap); serr != nil {
			log.Printf("rb-bridge: browse index sync after mount repair skipped: %v", serr)
		}
	}
	if len(remaining) > 0 {
		startBG()
	}
	st.done("repair", "%d repaired inline, %d reset to stub, %d left for background", repaired, stubReset, len(remaining))
	return repaired + stubReset, snap.Pending, nil
}

func (w *window) repairStockOverwritten() error {
	if w.ctx.Err() != nil || w.lib == nil || w.snap == nil {
		return nil
	}
	if !w.next && !w.st.kill.IsZero() && !w.st.must(200*time.Millisecond) {
		return nil
	}
	candidates, err := w.lib.ScanStockOACandidates(w.ctx)
	if err != nil {
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		log.Printf("rb-bridge: window repair prefilter skipped: %v", err)
		return nil
	}
	filled := loadFilledJournal(w.lib.UUID())
	matches := filterStockRepairMatches(candidates, w.snap, filled)
	if len(matches) == 0 {
		return nil
	}
	r, err := compat.NewResolver(w.mount)
	if err != nil {
		return err
	}
	var finishLimit, inlineLimit time.Time
	var flushRes time.Duration
	if !w.next {
		finishLimit, inlineLimit, flushRes = repairDeadlines(w.ctx, w.st)
	}

	var built []builtStockRepair
	var remaining []stockRepairMatch
	for idx, m := range matches {
		if w.rest != nil {
			if err := w.rest(0); err != nil {
				return err
			}
		}
		if w.ctx.Err() != nil ||
			(!inlineLimit.IsZero() && !time.Now().Before(inlineLimit)) ||
			(!w.next && !w.st.kill.IsZero() && !w.st.must(saveReserve)) {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		buildCtx := w.ctx
		var buildCancel context.CancelFunc
		if !inlineLimit.IsZero() {
			buildCtx, buildCancel = context.WithDeadline(w.ctx, inlineLimit)
		}
		if repairBeforeBuild != nil {
			herr := repairBeforeBuild(buildCtx, m.track.Key)
			if herr != nil {
				if buildCancel != nil {
					buildCancel()
				}
				if errors.Is(herr, errWindowDeadline) || errors.Is(herr, context.DeadlineExceeded) || buildCtx.Err() != nil {
					remaining = append(remaining, matches[idx:]...)
					break
				}
				return herr
			}
			if w.ctx.Err() != nil ||
				(!inlineLimit.IsZero() && !time.Now().Before(inlineLimit)) ||
				(!w.next && !w.st.kill.IsZero() && !w.st.must(saveReserve)) {
				if buildCancel != nil {
					buildCancel()
				}
				remaining = append(remaining, matches[idx:]...)
				break
			}
		}
		var vt *ViewTrack
		if w.view != nil {
			if v, ok := w.view.Track(m.track.Key); ok {
				vt = v
			}
		}
		rec, rt, ok, deferBG, berr := buildStockRepairTrack(buildCtx, w.mount, r, m.cand, m.track, vt)
		if buildCancel != nil {
			buildCancel()
		}
		if berr != nil {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		if ok {
			built = append(built, builtStockRepair{match: m, rec: rec, rt: rt})
		} else if deferBG {
			remaining = append(remaining, m)
		}
	}
	noteWindowRemaining := func(remList []stockRepairMatch) {
		for _, rem := range remList {
			rem.track.Perf = rbstate.PerfNone
			rem.track.Built = rbstate.TrackSig{}
			w.dirty = true
		}
		if w.o.OnDemand && len(remList) > 0 {
			w.pendingRepairs += len(remList)
			w.dirty = true
		}
	}
	if !w.next && (w.ctx.Err() != nil || (!finishLimit.IsZero() && time.Until(finishLimit) < flushRes) || (!w.st.kill.IsZero() && time.Until(w.st.kill) < saveReserve)) {
		allRem := make([]stockRepairMatch, 0, len(built)+len(remaining))
		for _, item := range built {
			allRem = append(allRem, item.match)
		}
		allRem = append(allRem, remaining...)
		noteWindowRemaining(allRem)
		return nil
	}
	if len(built) == 0 && !(w.o.OnDemand && len(remaining) > 0) {
		noteWindowRemaining(remaining)
		return nil
	}
	b, err := w.lib.Begin(w.ctx)
	if err != nil {
		return err
	}
	defer b.Rollback()
	repaired, overviews, err := applyBuiltStockRepairs(w.ctx, b, built)
	if err != nil {
		return err
	}
	stubReset := 0
	if w.o.OnDemand && len(remaining) > 0 {
		var unreset []stockRepairMatch
		stubReset, unreset, err = resetRemainingToImportStub(w.ctx, b, remaining, finishLimit, flushRes, w.st)
		if err != nil {
			return err
		}
		remaining = unreset
	}
	noteWindowRemaining(remaining)
	if repaired == 0 && stubReset == 0 {
		return nil
	}
	if err = w.commitBatch(b); err != nil {
		return err
	}
	for id, body := range overviews {
		if w.rest != nil {
			if w.rest(0) != nil {
				break
			}
		}
		if w.ctx.Err() != nil || (!w.next && !finishLimit.IsZero() && !time.Now().Before(finishLimit)) {
			break
		}
		writeOverview(w.dir, w.lib.UUID(), id, body)
	}
	w.res.Applied += repaired + stubReset
	w.st.done("repair", "%d repaired inline, %d reset to stub, %d left for background", repaired, stubReset, len(remaining))
	return nil
}
