package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// WriteBackOptions configures direct stick cue/loop write-back.
type WriteBackOptions struct {
	// DateDir overrides the YYYY-MM-DD subdirectory under /.enginex-backup/.
	DateDir string
	// SkipPreparingCheck skips checking /run/enginex-usb-preparing (for tests).
	SkipPreparingCheck bool
	// OnEject indicates invocation from the edisksd eject hook before unmounting;
	// skips the global preparing-flag check (another stick may be mounting) while
	// still holding the per-volume lock.
	OnEject bool
	// BeforeRename is an optional test hook invoked after writing a temp file
	// and before os.Rename replaces dst.
	BeforeRename func(dst string) error
}

// WriteBackResult summarizes a deck-edit write-back pass on a stick.
type WriteBackResult struct {
	TracksChecked     int    `json:"tracksChecked"`
	TracksChanged     int    `json:"tracksChanged"`
	FilesUpdated      int    `json:"filesUpdated"`
	OneLibraryUpdated bool   `json:"oneLibraryUpdated"`
	BackupDir         string `json:"backupDir,omitempty"`
}

const usbPreparingFlagPath = "/run/enginex-usb-preparing"

// mountRootWritable reports whether the mounted volume itself (not only an overlay
// over Engine Library) is mounted read-write so PIONEER/.PIONEER can be updated.
func mountRootWritable(mount string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(mount, &st) == nil && st.Flags&unix_ST_RDONLY == 0
}

// WriteBackCues writes Engine deck Hot Cues, Memory Cues, Saved Loops, and
// editable track metadata
// that differ from the stick's current rekordbox export back to the stick's
// ANLZ files (.DAT, .EXT, and .2EX if present) and rekordbox 7 exportLibrary.db
// (if present). Originals are backed up once under /.enginex-backup/<date>/.
func WriteBackCues(ctx context.Context, mountPath string, opts WriteBackOptions) (WriteBackResult, error) {
	var res WriteBackResult
	absMount, err := filepath.Abs(mountPath)
	if err != nil {
		return res, err
	}
	st, err := os.Stat(absMount)
	if err != nil || !st.IsDir() {
		return res, fmt.Errorf("writeback: mount path unavailable: %s", mountPath)
	}
	if !mountRootWritable(absMount) {
		return res, fmt.Errorf("writeback: stick is read-only: %s", absMount)
	}
	if !opts.SkipPreparingCheck && !opts.OnEject {
		if _, err := os.Stat(usbPreparingFlagPath); err == nil {
			return res, errors.New("writeback: conversion in progress")
		}
	}

	unlock, err := lockVolume(absMount)
	if err != nil {
		return res, fmt.Errorf("writeback: volume busy: %w", err)
	}
	defer unlock()

	engineDBDir := filepath.Join(absMount, "Engine Library", "Database2")
	mDBPath := filepath.Join(engineDBDir, "m.db")
	stateDBPath := filepath.Join(engineDBDir, stateName)
	if _, err := os.Stat(mDBPath); err != nil {
		return res, fmt.Errorf("writeback: Engine library not found: %w", err)
	}
	if _, err := os.Stat(stateDBPath); err != nil {
		return res, fmt.Errorf("writeback: bridge state not found: %w", err)
	}

	resolver, err := compat.NewResolver(absMount)
	if err != nil {
		return res, err
	}
	candidates, err := exportCandidates(resolver, absMount)
	if err != nil {
		return res, err
	}
	if len(candidates) == 0 {
		return res, fmt.Errorf("writeback: no rekordbox export found on %s", absMount)
	}
	preStamp, _ := exportStamp(resolver, absMount)

	mURI := url.URL{Scheme: "file", Path: mDBPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", mURI.String())
	if err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	stateURI := url.URL{Scheme: "file", Path: stateDBPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	if _, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS bridge`, stateURI.String()); err != nil {
		return res, fmt.Errorf("writeback: attach bridge state: %w", err)
	}

	hasWrittenTable := 0
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name='BridgeWritten'`).Scan(&hasWrittenTable)
	hasAnalysisTable := 0
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name='BridgeAnalysis'`).Scan(&hasAnalysisTable)

	query := `
		SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0),
		       pd.beatData, pd.trackData, pd.quickCues, pd.loops, COALESCE(pd.activeOnLoadLoops, 0),
		       NULL, COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,'')
		FROM bridge.BridgeState bs
		JOIN Track t ON t.id = bs.trackId
		JOIN PerformanceData pd ON pd.trackId = bs.trackId
	`
	if hasWrittenTable > 0 {
		query = `
			SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0),
			       pd.beatData, pd.trackData, pd.quickCues, pd.loops, COALESCE(pd.activeOnLoadLoops, 0),
			       bw.digest, COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,'')
			FROM bridge.BridgeState bs
			JOIN Track t ON t.id = bs.trackId
			JOIN PerformanceData pd ON pd.trackId = bs.trackId
			LEFT JOIN bridge.BridgeWritten bw ON bw.trackId = bs.trackId
		`
	}

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return res, fmt.Errorf("writeback: query tracks: %w", err)
	}
	defer rows.Close()

	type candidateTrack struct {
		enginePath   string
		engineID     int64
		pdbImportKey int64
		deck         enginedb.DeckCues
		cues         bool // the deck's cues may differ from the stick's
		metadata     rekordbox.Metadata
	}
	var dirty []candidateTrack

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var enginePath string
		var engineID, pdbImportKey, activeLoops int64
		var beatData, trackData, quickCues, loopsBlob, writtenDigest []byte
		var md rekordbox.Metadata
		if err := rows.Scan(&enginePath, &engineID, &pdbImportKey, &beatData, &trackData, &quickCues, &loopsBlob, &activeLoops, &writtenDigest, &md.Genre, &md.Comment, &md.Label); err != nil {
			return res, err
		}
		res.TracksChecked++
		c := candidateTrack{enginePath: enginePath, engineID: engineID, pdbImportKey: pdbImportKey, metadata: md}
		// Fast path: tracks untouched since the last import stay at their BridgeWritten
		// digest. Skip their cues without decrypting exportLibrary.db or reading ANLZ off USB.
		// Tracks with loops are still inspected because activeOnLoadLoops is outside the digest.
		// The track's metadata (#314) is compared with the export's either way.
		if len(trackData) > 0 && !(len(writtenDigest) > 0 && activeLoops == 0 && !enginedb.HasAnyLoop(loopsBlob) &&
			bytes.Equal(writtenDigest, enginedb.WrittenDigest(beatData, quickCues, loopsBlob))) {
			if deck, err := enginedb.ParseDeckCuesAndLoops(trackData, quickCues, loopsBlob, activeLoops); err == nil {
				c.deck, c.cues = deck, true
			}
		}
		dirty = append(dirty, c)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	_ = rows.Close()

	if len(dirty) == 0 {
		return res, nil
	}

	rbLib, oneLibRel, err := readStickRekordboxLibrary(ctx, resolver, absMount)
	if err != nil {
		return res, err
	}
	byID := make(map[uint32]rekordbox.Track, len(rbLib.Tracks))
	byPath := make(map[string]rekordbox.Track, len(rbLib.Tracks))
	byPathFold := make(map[string]rekordbox.Track, len(rbLib.Tracks))
	for _, tr := range rbLib.Tracks {
		byID[tr.ID] = tr
		if tr.Path != "" {
			norm := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(tr.Path)), "/")
			byPath[norm] = tr
			byPathFold[strings.ToLower(norm)] = tr
		}
	}

	dateDir := opts.DateDir
	if dateDir == "" {
		dateDir = time.Now().Format("2006-01-02")
	}

	type trackChange struct {
		engineID     int64
		rbTrackID    uint32
		analysisPath string
		cues         []rekordbox.Cue
		cuesDiff     bool
		metadata     rekordbox.Metadata
		metadataDiff bool
	}
	var changes []trackChange

	for _, cand := range dirty {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rbTrack, ok := resolveRekordboxTrack(ctx, db, hasAnalysisTable > 0, cand.engineID, cand.pdbImportKey, cand.enginePath, byID, byPath, byPathFold)
		if !ok || rbTrack.ID == 0 {
			continue
		}
		var nextCues []rekordbox.Cue
		cuesDiff := false
		if cand.cues && rbTrack.AnalysisPath != "" {
			offsetSamples := trackRekordboxOffset(resolver, rbTrack.Path)
			origCues := readStickTrackCues(resolver, rbTrack)
			nextCues = mergeEngineCues(origCues, cand.deck, offsetSamples)
			cuesDiff = !cuesEquivalent(origCues, nextCues)
		}
		metadataDiff := cand.metadata != (rekordbox.Metadata{Genre: rbTrack.Genre, Comment: rbTrack.Comment, Label: rbTrack.Label})
		if !cuesDiff && !metadataDiff {
			continue
		}
		changes = append(changes, trackChange{
			engineID:     cand.engineID,
			rbTrackID:    rbTrack.ID,
			analysisPath: rbTrack.AnalysisPath,
			cues:         nextCues,
			cuesDiff:     cuesDiff,
			metadata:     cand.metadata,
			metadataDiff: metadataDiff,
		})
	}
	_ = db.Close()

	if len(changes) == 0 {
		return res, nil
	}

	backupRoot := filepath.Join(absMount, ".enginex-backup")
	oneLibUpdates := make(map[uint32][]rekordbox.Cue, len(changes))
	oneLibMetadata := make(map[uint32]rekordbox.Metadata, len(changes))
	pdbMetadata := make(map[uint32]rekordbox.Metadata, len(changes))
	var writtenIDs []int64
	// Every file is staged (written and synced beside it) before any is renamed over its
	// original (#314): a failure before the renames leaves the stick as it was.
	type stagedWrite struct {
		dst    string
		tmp    string
		export bool // the export, whose stamp the bridge keeps
	}
	var staged []stagedWrite
	defer func() {
		for _, item := range staged {
			_ = os.Remove(item.tmp)
		}
	}()
	stageBytes := func(dst string, data []byte) error {
		tmp, err := stageWriteFile(dst, data)
		if err != nil {
			return err
		}
		staged = append(staged, stagedWrite{dst: dst, tmp: tmp})
		return nil
	}

	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.TracksChanged++
		if ch.metadataDiff {
			oneLibMetadata[ch.rbTrackID] = ch.metadata
			pdbMetadata[ch.rbTrackID] = ch.metadata
		}
		if !ch.cuesDiff || ch.analysisPath == "" {
			continue
		}
		cleanAnlz, err := safeANLZRelPath(ch.analysisPath)
		if err != nil {
			continue
		}
		oneLibUpdates[ch.rbTrackID] = ch.cues
		writtenIDs = append(writtenIDs, ch.engineID)

		baseRel := strings.TrimSuffix(cleanAnlz, filepath.Ext(cleanAnlz))
		targets := []struct {
			ext                   string
			extended              bool
			onlyIfCueSectionExist bool
		}{
			{ext: ".DAT", extended: false, onlyIfCueSectionExist: false},
			{ext: ".EXT", extended: true, onlyIfCueSectionExist: false},
			{ext: ".2EX", extended: true, onlyIfCueSectionExist: true},
		}

		for _, tg := range targets {
			rel, ok := resolveANLZVariant(resolver, baseRel, tg.ext)
			if !ok {
				continue
			}
			fullPath := resolver.FullPath(rel)
			raw, err := os.ReadFile(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return res, err
			}
			updated, changed, err := rekordbox.RewriteANLZCues(raw, ch.cues, tg.extended, tg.onlyIfCueSectionExist)
			if err != nil {
				return res, fmt.Errorf("writeback: %s: %w", rel, err)
			}
			if !changed {
				continue
			}
			if err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, rel); err != nil {
				return res, err
			}
			if err := stageBytes(fullPath, updated); err != nil {
				return res, err
			}
		}
	}

	oneLibFull := ""
	if oneLibRel != "" && (len(oneLibUpdates) > 0 || len(oneLibMetadata) > 0) {
		oneLibFull = resolver.FullPath(oneLibRel)
		if _, err := os.Stat(oneLibFull); err == nil {
			if err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, oneLibRel); err != nil {
				return res, err
			}
			tmpOut, err := os.CreateTemp(filepath.Dir(oneLibFull), ".enginex-onelib-*.tmp")
			if err != nil {
				return res, err
			}
			tmpPath := tmpOut.Name()
			_ = tmpOut.Close()
			if err := rekordbox.UpdateOneLibraryEdits(ctx, oneLibFull, oneLibUpdates, oneLibMetadata, tmpPath); err != nil {
				_ = os.Remove(tmpPath)
				return res, err
			}
			staged = append(staged, stagedWrite{dst: oneLibFull, tmp: tmpPath, export: true})
		}
	}

	// A stick without a OneLibrary export carries its metadata in export.pdb (#314).
	if oneLibRel == "" && len(pdbMetadata) > 0 {
		if pdbRel, err := resolver.Resolve("PIONEER/rekordbox/export.pdb"); err == nil {
			pdbFull := resolver.FullPath(pdbRel)
			if err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, pdbRel); err != nil {
				return res, err
			}
			tmpOut, err := os.CreateTemp(filepath.Dir(pdbFull), ".enginex-pdb-*.tmp")
			if err != nil {
				return res, err
			}
			tmpPath := tmpOut.Name()
			_ = tmpOut.Close()
			if err := updatePDBMetadata(ctx, pdbFull, pdbMetadata, tmpPath); err != nil {
				_ = os.Remove(tmpPath)
				return res, err
			}
			staged = append(staged, stagedWrite{dst: pdbFull, tmp: tmpPath, export: true})
		}
	}

	for _, item := range staged {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if opts.BeforeRename != nil {
			if err := opts.BeforeRename(item.dst); err != nil {
				return res, err
			}
		}
	}
	exportWritten := false
	for _, item := range staged {
		if err := os.Rename(item.tmp, item.dst); err != nil {
			return res, err
		}
		_ = syncDir(filepath.Dir(item.dst))
		res.FilesUpdated++
		if item.dst == oneLibFull {
			res.OneLibraryUpdated = true
		}
		exportWritten = exportWritten || item.export
	}
	staged = nil

	// Refresh the bridge stamp if the stick was already in sync before
	// our write-back, so re-inserting into EngineX fast-bypasses instead
	// of re-importing the whole stick.
	if exportWritten && preStamp != "" {
		if postStamp, err := exportStamp(resolver, absMount); err == nil && postStamp != "" {
			if readMark(engineDBDir, stampName) == preStamp {
				_ = writeMark(engineDBDir, stampName, postStamp)
			}
			if readMark(engineDBDir, pendingName) == preStamp {
				_ = writeMark(engineDBDir, pendingName, postStamp)
			}
		}
	}

	if res.FilesUpdated > 0 {
		res.BackupDir = filepath.Join(".enginex-backup", dateDir)
		if hasWrittenTable > 0 && len(writtenIDs) > 0 {
			clearBridgeWritten(ctx, stateDBPath, writtenIDs)
		}
	}
	return res, nil
}

// clearBridgeWritten drops BridgeWritten digests for tracks whose cues were
// written back to the stick. Otherwise, deleting a previously written-back cue
// on a later session would restore the original import digest and be skipped by
// the untouched-track fast path.
func clearBridgeWritten(ctx context.Context, stateDBPath string, trackIDs []int64) {
	u := url.URL{Scheme: "file", Path: stateDBPath, RawQuery: "_pragma=busy_timeout(5000)"}
	sdb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM BridgeWritten WHERE trackId = ?`)
	if err != nil {
		return
	}
	defer stmt.Close()
	for _, id := range trackIDs {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func readStickRekordboxLibrary(ctx context.Context, r *compat.Resolver, mountPath string) (*rekordbox.Library, string, error) {
	var errs []error
	for _, p := range []string{"PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/exportOneLibrary.db"} {
		rel, err := r.Resolve(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		lib, err := rekordbox.ReadSQLite(ctx, r.FullPath(rel), nil)
		if err == nil {
			return lib, rel, nil
		}
		errs = append(errs, err)
	}
	if rel, err := r.Resolve("PIONEER/rekordbox/export.pdb"); err == nil {
		lib, err := rekordbox.ReadPDB(r.FullPath(rel))
		if err == nil {
			return lib, "", nil
		}
		errs = append(errs, err)
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, "", errors.Join(errs...)
	}
	return nil, "", fmt.Errorf("writeback: no rekordbox export found on %s", mountPath)
}

func resolveRekordboxTrack(
	ctx context.Context,
	db *sql.DB,
	hasAnalysis bool,
	engineID, pdbImportKey int64,
	enginePath string,
	byID map[uint32]rekordbox.Track,
	byPath map[string]rekordbox.Track,
	byPathFold map[string]rekordbox.Track,
) (rekordbox.Track, bool) {
	if pdbImportKey > 0 && pdbImportKey <= math.MaxUint32 {
		if tr, ok := byID[uint32(pdbImportKey)]; ok {
			return tr, true
		}
	}
	norm := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(enginePath)), "../")
	if tr, ok := byPath[norm]; ok {
		return tr, true
	}
	if tr, ok := byPathFold[strings.ToLower(norm)]; ok {
		return tr, true
	}
	if hasAnalysis {
		var srcJSON []byte
		if err := db.QueryRowContext(ctx, `SELECT sourceJSON FROM bridge.BridgeAnalysis WHERE trackId = ?`, engineID).Scan(&srcJSON); err == nil && len(srcJSON) > 0 {
			if raw, err := enginedb.UnpackAnalysis(srcJSON, 64<<20); err == nil {
				var decoded struct {
					Track struct {
						ID uint32
					}
				}
				if json.Unmarshal(raw, &decoded) == nil && decoded.Track.ID != 0 {
					if tr, ok := byID[decoded.Track.ID]; ok {
						return tr, true
					}
				}
			}
		}
	}
	return rekordbox.Track{}, false
}

func trackRekordboxOffset(r *compat.Resolver, trackPath string) float64 {
	if trackPath == "" {
		return 0
	}
	rel, err := r.Resolve(trackPath)
	if err != nil {
		return 0
	}
	info, err := compat.AudioHeader(r.FullPath(rel))
	if err != nil {
		return 0
	}
	return float64(info.RekordboxOffset())
}

func resolveANLZVariant(r *compat.Resolver, baseRel, ext string) (string, bool) {
	if rel, err := r.Resolve(baseRel + ext); err == nil {
		return rel, true
	}
	if strings.HasPrefix(strings.ToUpper(baseRel), ".PIONEER/") {
		if rel, err := r.Resolve("PIONEER/" + baseRel[len(".PIONEER/"):] + ext); err == nil {
			return rel, true
		}
	}
	return "", false
}

func readStickTrackCues(r *compat.Resolver, rbTrack rekordbox.Track) []rekordbox.Cue {
	clean, err := safeANLZRelPath(rbTrack.AnalysisPath)
	if err != nil {
		return rbTrack.Cues
	}
	base := strings.TrimSuffix(clean, filepath.Ext(clean))
	var merged *rekordbox.Analysis
	// Merge in order: .DAT (carries PCOB active-loop flags), OneLibrary cues (if any),
	// then .EXT (carries PCOB + PCO2 extended comments and exact RGB hot-cue colors).
	if rel, ok := resolveANLZVariant(r, base, ".DAT"); ok {
		if b, err := os.ReadFile(r.FullPath(rel)); err == nil {
			if a, err := rekordbox.ParseAnalysis(b); err == nil && a != nil {
				merged = a
			}
		}
	}
	if len(rbTrack.Cues) > 0 {
		if merged == nil {
			merged = &rekordbox.Analysis{}
		}
		merged.Merge(&rekordbox.Analysis{Cues: rbTrack.Cues})
	}
	if rel, ok := resolveANLZVariant(r, base, ".EXT"); ok {
		if b, err := os.ReadFile(r.FullPath(rel)); err == nil {
			if a, err := rekordbox.ParseAnalysis(b); err == nil && a != nil {
				if merged == nil {
					merged = a
				} else {
					merged.Merge(a)
				}
			}
		}
	}
	if merged == nil {
		return nil
	}
	return merged.Cues
}

func updatePDBMetadata(ctx context.Context, path string, updates map[uint32]rekordbox.Metadata, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := rekordbox.RewritePDBMetadata(raw, updates)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, updated, 0o644)
}

func mergeEngineCues(orig []rekordbox.Cue, deck enginedb.DeckCues, offsetSamples float64) []rekordbox.Cue {
	origHot := make(map[uint32]rekordbox.Cue, 8)
	var origMemPoints []rekordbox.Cue
	var origMemLoops []rekordbox.Cue
	for _, oc := range orig {
		if oc.HotCue > 0 {
			origHot[oc.HotCue] = oc
		} else if oc.Loop {
			origMemLoops = append(origMemLoops, oc)
		} else {
			origMemPoints = append(origMemPoints, oc)
		}
	}

	toMS := func(samples float64) uint32 {
		if samples < 0 {
			samples = 0
		}
		ms := math.Round((samples + offsetSamples) * 1000.0 / deck.SampleRate)
		if ms < 0 {
			return 0
		}
		if ms > math.MaxUint32 {
			return math.MaxUint32
		}
		return uint32(ms)
	}

	var out []rekordbox.Cue
	var consumedLoop [8]bool

	// 1. Hot Cue pads 1..8 (Slot 0..7 -> Rekordbox Hot Cue A..H = 1..8).
	// A Hot Loop imported by EncodePerformance occupies both quickCues[slot] and
	// loops[slot] at the same start position; pair them into a single Hot Loop cue.
	for s := 0; s < 8; s++ {
		hc := deck.HotCues[s]
		if hc == nil {
			continue
		}
		pad := uint32(s + 1)
		startMS := toMS(hc.Samples)
		lp := deck.Loops[s]
		if lp != nil && absDiffU32(startMS, toMS(lp.StartSamples)) <= 2 {
			consumedLoop[s] = true
			endMS := toMS(lp.EndSamples)
			if endMS <= startMS {
				endMS = startMS + 1
			}
			name := lp.Name
			if name == "" {
				name = hc.Name
			}
			rgb := lp.Color
			if rgb == ([4]byte{}) {
				rgb = hc.Color
			}
			code := rekordbox.NearestCueColorCode(rgb)
			var loopNum, loopDen uint16
			if oc, ok := origHot[pad]; ok && oc.Loop && absDiffU32(oc.TimeMS, startMS) <= 2 && absDiffU32(oc.EndMS, endMS) <= 2 {
				loopNum, loopDen = oc.LoopNumerator, oc.LoopDenominator
				if oc.Color == rgb && oc.ColorCode != 0 {
					code = oc.ColorCode
				}
			}
			out = append(out, rekordbox.Cue{
				HotCue:          pad,
				TimeMS:          startMS,
				EndMS:           endMS,
				Loop:            true,
				Active:          lp.Active,
				Name:            name,
				Color:           rgb,
				ColorCode:       code,
				LoopNumerator:   loopNum,
				LoopDenominator: loopDen,
				Extended:        true,
			})
			continue
		}

		rgb := hc.Color
		code := rekordbox.NearestCueColorCode(rgb)
		if oc, ok := origHot[pad]; ok && !oc.Loop && oc.Color == rgb && oc.ColorCode != 0 {
			code = oc.ColorCode
		}
		out = append(out, rekordbox.Cue{
			HotCue:    pad,
			TimeMS:    startMS,
			Name:      hc.Name,
			Color:     rgb,
			ColorCode: code,
			Extended:  true,
		})
	}

	// 2. Unpaired Engine Saved Loops map to Rekordbox Memory Loops (HotCue=0, Loop=true).
	hotLoopCount := 0
	for _, consumed := range consumedLoop {
		if consumed {
			hotLoopCount++
		}
	}
	for s := 0; s < 8; s++ {
		if consumedLoop[s] || deck.Loops[s] == nil {
			continue
		}
		lp := deck.Loops[s]
		startMS := toMS(lp.StartSamples)
		endMS := toMS(lp.EndSamples)
		if endMS <= startMS {
			continue
		}
		var loopNum, loopDen uint16
		colorID := rekordbox.NearestMemoryColorID(lp.Color)
		var memColor [4]byte
		var memColorCode byte
		for _, oc := range origMemLoops {
			if absDiffU32(oc.TimeMS, startMS) <= 2 && absDiffU32(oc.EndMS, endMS) <= 2 {
				loopNum, loopDen = oc.LoopNumerator, oc.LoopDenominator
				if oc.ColorID != 0 && oc.ColorID <= 8 {
					colorID = oc.ColorID
				} else if oc.ColorID == 0 && oc.Color != ([4]byte{}) && oc.Color == lp.Color {
					colorID = 0
					memColor = oc.Color
					memColorCode = oc.ColorCode
				}
				break
			}
		}
		out = append(out, rekordbox.Cue{
			HotCue:          0,
			TimeMS:          startMS,
			EndMS:           endMS,
			Loop:            true,
			Active:          lp.Active,
			Name:            lp.Name,
			ColorID:         colorID,
			Color:           memColor,
			ColorCode:       memColorCode,
			LoopNumerator:   loopNum,
			LoopDenominator: loopDen,
			Extended:        true,
		})
	}

	// 3. Preserve all original non-loop Memory Cues (Engine does not expose multi-memory-cue editing,
	// and Engine's transient MainCue is never exported as a synthetic "CUE" Hot Cue or Memory Cue).
	out = append(out, origMemPoints...)

	// Preserve any extra original Memory Loops that could not fit in Engine's 8 loop slots.
	maxFitMemLoops := 8 - hotLoopCount
	if len(origMemLoops) > maxFitMemLoops {
		for _, oc := range origMemLoops[maxFitMemLoops:] {
			dup := false
			for _, nc := range out {
				if nc.HotCue == 0 && nc.Loop && absDiffU32(nc.TimeMS, oc.TimeMS) <= 2 && absDiffU32(nc.EndMS, oc.EndMS) <= 2 {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, oc)
			}
		}
	}
	return out
}

func cuesEquivalent(a, b []rekordbox.Cue) bool {
	na := normalizeCues(a)
	nb := normalizeCues(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		x, y := na[i], nb[i]
		if x.HotCue != y.HotCue || x.Loop != y.Loop || x.Name != y.Name {
			return false
		}
		if x.Loop && x.Active != y.Active {
			return false
		}
		if absDiffU32(x.TimeMS, y.TimeMS) > 2 {
			return false
		}
		if x.Loop && absDiffU32(x.EndMS, y.EndMS) > 2 {
			return false
		}
		if x.HotCue == 0 {
			if x.ColorID != y.ColorID {
				return false
			}
			if x.ColorID == 0 && x.Color != y.Color && x.ColorCode != y.ColorCode {
				return false
			}
		} else if x.Color != y.Color && x.ColorCode != y.ColorCode {
			return false
		}
	}
	return true
}

func normalizeCues(in []rekordbox.Cue) []rekordbox.Cue {
	out := make([]rekordbox.Cue, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		if out[i].HotCue != out[j].HotCue {
			return out[i].HotCue < out[j].HotCue
		}
		if out[i].Loop != out[j].Loop {
			return !out[i].Loop
		}
		if out[i].TimeMS != out[j].TimeMS {
			return out[i].TimeMS < out[j].TimeMS
		}
		return out[i].EndMS < out[j].EndMS
	})
	return out
}

func absDiffU32(a, b uint32) uint32 {
	if a >= b {
		return a - b
	}
	return b - a
}

func safeANLZRelPath(p string) (string, error) {
	clean, err := compat.CleanRelative(p)
	if err != nil {
		return "", fmt.Errorf("unsafe ANLZ path: %s", p)
	}
	upper := strings.ToUpper(clean)
	if !strings.HasPrefix(upper, "PIONEER/USBANLZ/") && !strings.HasPrefix(upper, ".PIONEER/USBANLZ/") {
		return "", fmt.Errorf("unsafe ANLZ path: %s", p)
	}
	return clean, nil
}

func ensureFirstOriginalBackup(mountRoot, backupRoot, dateDir, relSlash string) error {
	relOS := filepath.FromSlash(relSlash)
	if entries, err := os.ReadDir(backupRoot); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			existing := filepath.Join(backupRoot, ent.Name(), relOS)
			if _, err := os.Stat(existing); err == nil {
				return nil
			}
		}
	}
	src := filepath.Join(mountRoot, relOS)
	dst := filepath.Join(backupRoot, dateDir, relOS)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bak-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	_ = syncDir(filepath.Dir(dst))
	ok = true
	return nil
}

func stageWriteFile(dst string, data []byte) (string, error) {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".enginex-wb-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	ok = true
	return tmpPath, nil
}

func atomicWriteFile(dst string, data []byte, beforeRename func(string) error) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".enginex-wb-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(dst); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	_ = syncDir(dir)
	ok = true
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
