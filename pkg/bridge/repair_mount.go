package bridge

import (
	"bytes"
	"context"
	"errors"
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

// Gen-2 64-bit (PRIME 4 G2 / SYSTEM ONE, !OnDemand) mount-time repair for rekordbox-converted
// rows that stock Engine OfflineAnalyzer overwrote. Runs strictly inside rbstate.LockVolume
// and the 4 s insertBudget, shares enginedb.IsRepairableStockOverwritten with the arm32
// on-load hook path, and hands off any rows left over after budget expiry to FillInBackground.

var (
	repairItemReserve = 500 * time.Millisecond
	repairBeforeBuild func(ctx context.Context, key string) error
)

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

func filterStockRepairMatches(candidates []enginedb.StockOACandidate, snap *rbstate.Snapshot) []stockRepairMatch {
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
		row := c.Row
		row.FastKnown = t.FastKnown
		row.FastCues = t.FastCues
		row.FastLoops = t.FastLoops
		row.FastActive = t.FastActive
		prov := enginedb.TrackRepairProvenance(t, c.Genre, c.Comment, c.Label, c.Row.QuickCues, c.Row.Loops, c.Row.ActiveOnLoadLoops)
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

	rec, err = enginedb.BuildTrack(r, rt, src, enginedb.ReadOptions{})
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
			HasSyncedColorTag: len(t.SyncedColorTag) > 0,
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

func markRemainingForBackground(snap *rbstate.Snapshot, remaining []stockRepairMatch) {
	if len(remaining) == 0 || snap == nil {
		return
	}
	for _, rem := range remaining {
		rem.track.Perf = rbstate.PerfNone
		rem.track.Built = rbstate.TrackSig{}
	}
	if snap.Pending.Perf < len(remaining) {
		snap.Pending.Perf = len(remaining)
	}
}

func repairStockOverwrittenAtMount(ctx context.Context, mount, dir string, r *compat.Resolver, head rbstate.Header, st *steps, startBG func()) (repaired int, pending rbstate.Pending, err error) {
	if ctx.Err() != nil {
		return 0, head.Pending, ctx.Err()
	}
	if !st.kill.IsZero() && !st.must(200*time.Millisecond) {
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
	if liveUUID == "" || liveUUID != head.LibraryUUID || len(candidates) == 0 {
		return 0, head.Pending, nil
	}
	snap, lerr := rbstate.Load(dir)
	if lerr != nil || snap.LibraryUUID != liveUUID || snap.Rules != rbstate.Rules {
		return 0, head.Pending, nil
	}
	matches := filterStockRepairMatches(candidates, snap)
	if len(matches) == 0 {
		return 0, head.Pending, nil
	}

	var built []builtStockRepair
	var remaining []stockRepairMatch
	for idx, m := range matches {
		if ctx.Err() != nil || !st.more(repairItemReserve) || (!st.kill.IsZero() && !st.must(saveReserve)) {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		if repairBeforeBuild != nil {
			if herr := repairBeforeBuild(ctx, m.track.Key); herr != nil {
				if errors.Is(herr, errWindowDeadline) {
					remaining = append(remaining, matches[idx:]...)
					break
				}
				return 0, head.Pending, herr
			}
			if ctx.Err() != nil || !st.more(repairItemReserve) || (!st.kill.IsZero() && !st.must(saveReserve)) {
				remaining = append(remaining, matches[idx:]...)
				break
			}
		}
		rec, rt, ok, deferBG, berr := buildStockRepairTrack(ctx, mount, r, m.cand, m.track, nil)
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

	if len(built) == 0 {
		if len(remaining) == 0 {
			return 0, head.Pending, nil
		}
		markRemainingForBackground(snap, remaining)
		if st.kill.IsZero() || time.Until(st.kill) >= saveReserve {
			if saveErr := snap.SaveCtx(ctx, dir); saveErr != nil {
				return 0, head.Pending, saveErr
			}
		}
		startBG()
		st.done("repair", "0 inline, %d stock-overwritten rows handed to background", len(remaining))
		return 0, snap.Pending, nil
	}

	if !st.kill.IsZero() && time.Until(st.kill) < saveReserve {
		allRem := make([]stockRepairMatch, 0, len(built)+len(remaining))
		for _, item := range built {
			allRem = append(allRem, item.match)
		}
		allRem = append(allRem, remaining...)
		markRemainingForBackground(snap, allRem)
		startBG()
		return 0, snap.Pending, errWindowDeadline
	}

	lib, oerr := enginedb.OpenLibraryFile(ctx, mdb, mount)
	if oerr != nil {
		return 0, head.Pending, oerr
	}
	defer lib.Close()

	b, berr := lib.Begin(ctx)
	if berr != nil {
		return 0, head.Pending, berr
	}
	defer b.Rollback()

	repaired, overviews, aerr := applyBuiltStockRepairs(ctx, b, built)
	if aerr != nil {
		return 0, head.Pending, aerr
	}
	if repaired == 0 && len(remaining) == 0 {
		return 0, head.Pending, nil
	}
	markRemainingForBackground(snap, remaining)
	if repaired > 0 {
		snap.Applied, snap.Commit = time.Now().Unix(), b.CommitID()
		if !st.kill.IsZero() && time.Until(st.kill) < saveReserve {
			return 0, snap.Pending, errWindowDeadline
		}
		if err = snap.SaveNextCtx(ctx, dir); err != nil {
			return 0, head.Pending, err
		}
		cut("repair next saved")
		if err = b.Commit(ctx); err != nil {
			_ = rbstate.DropNext(dir)
			return 0, head.Pending, err
		}
		cut("repair committed")
		if err = rbstate.PromoteCtx(ctx, dir); err != nil {
			return repaired, snap.Pending, err
		}
		cut("repair promoted")
		for id, body := range overviews {
			if ctx.Err() != nil || !st.more(0) {
				break
			}
			writeOverview(dir, lib.UUID(), id, body)
		}
	} else if len(remaining) > 0 {
		if st.kill.IsZero() || time.Until(st.kill) >= saveReserve {
			if saveErr := snap.SaveCtx(ctx, dir); saveErr != nil {
				return 0, head.Pending, saveErr
			}
		}
	}
	if len(remaining) > 0 {
		startBG()
	}
	st.done("repair", "%d stock-overwritten rows repaired inline, %d left for background", repaired, len(remaining))
	return repaired, snap.Pending, nil
}

func (w *window) repairStockOverwritten() error {
	if w.ctx.Err() != nil || w.lib == nil || w.snap == nil {
		return nil
	}
	if !w.st.kill.IsZero() && !w.st.must(200*time.Millisecond) {
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
	matches := filterStockRepairMatches(candidates, w.snap)
	if len(matches) == 0 {
		return nil
	}
	r, err := compat.NewResolver(w.mount)
	if err != nil {
		return err
	}
	var built []builtStockRepair
	var remaining []stockRepairMatch
	for idx, m := range matches {
		if w.ctx.Err() != nil || !w.st.more(repairItemReserve) || (!w.st.kill.IsZero() && !w.st.must(saveReserve)) {
			remaining = append(remaining, matches[idx:]...)
			break
		}
		if repairBeforeBuild != nil {
			if herr := repairBeforeBuild(w.ctx, m.track.Key); herr != nil {
				if errors.Is(herr, errWindowDeadline) {
					remaining = append(remaining, matches[idx:]...)
					break
				}
				return herr
			}
			if w.ctx.Err() != nil || !w.st.more(repairItemReserve) || (!w.st.kill.IsZero() && !w.st.must(saveReserve)) {
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
		rec, rt, ok, deferBG, berr := buildStockRepairTrack(w.ctx, w.mount, r, m.cand, m.track, vt)
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
	for _, rem := range remaining {
		rem.track.Perf = rbstate.PerfNone
		rem.track.Built = rbstate.TrackSig{}
		w.dirty = true
	}
	if len(built) == 0 {
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
	if repaired == 0 {
		return nil
	}
	if err = w.commitBatch(b); err != nil {
		return err
	}
	for id, body := range overviews {
		if w.ctx.Err() != nil || !w.st.more(0) {
			break
		}
		writeOverview(w.dir, w.lib.UUID(), id, body)
	}
	w.res.Applied += repaired
	w.st.done("repair", "%d stock-overwritten rows repaired inline, %d left for background", repaired, len(remaining))
	return nil
}
