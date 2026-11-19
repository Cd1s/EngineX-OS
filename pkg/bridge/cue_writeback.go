package bridge

import (
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
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// WriteBackOptions configures direct stick cue/loop write-back.
type WriteBackOptions struct {
	// DateDir overrides the YYYY-MM-DD subdirectory under /.enginex-backup/.
	DateDir string
	// SkipPreparingCheck skips checking /run/enginex-usb-preparing (for tests).
	SkipPreparingCheck bool
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
	if !writable(absMount) {
		return res, fmt.Errorf("writeback: stick is read-only: %s", absMount)
	}
	if !opts.SkipPreparingCheck {
		if _, err := os.Stat(usbPreparingFlagPath); err == nil {
			return res, errors.New("writeback: conversion in progress")
		}
	}

	unlock, err := lockVolume(absMount)
	if err != nil {
		return res, fmt.Errorf("writeback: volume busy: %w", err)
	}
	defer unlock()

	mDBPath := filepath.Join(absMount, "Engine Library", "Database2", "m.db")
	stateDBPath := filepath.Join(absMount, "Engine Library", "Database2", ".rb-bridge-state.db")
	if _, err := os.Stat(mDBPath); err != nil {
		return res, fmt.Errorf("writeback: Engine library not found: %w", err)
	}
	if _, err := os.Stat(stateDBPath); err != nil {
		return res, fmt.Errorf("writeback: bridge state not found: %w", err)
	}

	rbLib, oneLibOK, err := readStickRekordboxLibrary(ctx, absMount)
	if err != nil {
		return res, err
	}
	byID := make(map[uint32]rekordbox.Track, len(rbLib.Tracks))
	byPath := make(map[string]rekordbox.Track, len(rbLib.Tracks))
	for _, tr := range rbLib.Tracks {
		byID[tr.ID] = tr
		if tr.Path != "" {
			norm := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(tr.Path)), "/")
			byPath[norm] = tr
		}
	}

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

	rows, err := db.QueryContext(ctx, `
		SELECT bs.path, bs.trackId, ba.sourceJSON,
		       pd.trackData, pd.quickCues, pd.loops,
		       COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,'')
		FROM bridge.BridgeState bs
		JOIN Track t ON t.id = bs.trackId
		JOIN PerformanceData pd ON pd.trackId = bs.trackId
		LEFT JOIN bridge.BridgeAnalysis ba ON ba.trackId = bs.trackId
	`)
	if err != nil {
		return res, fmt.Errorf("writeback: query tracks: %w", err)
	}
	defer rows.Close()

	dateDir := opts.DateDir
	if dateDir == "" {
		dateDir = time.Now().Format("2006-01-02")
	}

	type trackChange struct {
		rbTrackID    uint32
		analysisPath string
		cues         []rekordbox.Cue
		cuesDiff     bool
		metadata     rekordbox.Metadata
		metadataDiff bool
	}
	var changes []trackChange

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var enginePath string
		var engineID int64
		var srcJSON, trackData, quickCues, loopsBlob []byte
		var genre, comment, label string
		if err := rows.Scan(&enginePath, &engineID, &srcJSON, &trackData, &quickCues, &loopsBlob, &genre, &comment, &label); err != nil {
			return res, err
		}
		res.TracksChecked++

		rbTrack, ok := resolveRekordboxTrack(srcJSON, enginePath, byID, byPath)
		if !ok || rbTrack.ID == 0 {
			continue
		}
		var nextCues []rekordbox.Cue
		cuesDiff := false
		if rbTrack.AnalysisPath != "" && len(trackData) > 0 {
			dcues, dloops, err := enginedb.ParseDisplayCuesAndLoops(trackData, quickCues, loopsBlob)
			if err == nil {
				origCues := readStickTrackCues(absMount, rbTrack.AnalysisPath)
				nextCues = mergeEngineCues(origCues, dcues, dloops)
				cuesDiff = !cuesEquivalent(origCues, nextCues)
			}
		}
		metadata := rekordbox.Metadata{Genre: genre, Comment: comment, Label: label}
		metadataDiff := metadata != (rekordbox.Metadata{Genre: rbTrack.Genre, Comment: rbTrack.Comment, Label: rbTrack.Label})
		if !cuesDiff && !metadataDiff {
			continue
		}
		changes = append(changes, trackChange{
			rbTrackID:    rbTrack.ID,
			analysisPath: rbTrack.AnalysisPath,
			cues:         nextCues,
			cuesDiff:     cuesDiff,
			metadata:     metadata,
			metadataDiff: metadataDiff,
		})
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	if len(changes) == 0 {
		return res, nil
	}

	backupRoot := filepath.Join(absMount, ".enginex-backup")
	oneLibUpdates := make(map[uint32][]rekordbox.Cue, len(changes))
	oneLibMetadata := make(map[uint32]rekordbox.Metadata, len(changes))
	pdbMetadata := make(map[uint32]rekordbox.Metadata, len(changes))

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
		datRel, err := safeANLZRelPath(ch.analysisPath)
		if err != nil {
			continue
		}
		oneLibUpdates[ch.rbTrackID] = ch.cues

		baseRel := strings.TrimSuffix(datRel, filepath.Ext(datRel))
		targets := []struct {
			rel                   string
			extended              bool
			onlyIfCueSectionExist bool
		}{
			{rel: baseRel + ".DAT", extended: false, onlyIfCueSectionExist: false},
			{rel: baseRel + ".EXT", extended: true, onlyIfCueSectionExist: false},
			{rel: baseRel + ".2EX", extended: true, onlyIfCueSectionExist: true},
		}

		for _, tg := range targets {
			fullPath := filepath.Join(absMount, filepath.FromSlash(tg.rel))
			raw, err := os.ReadFile(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return res, err
			}
			updated, changed, err := rekordbox.RewriteANLZCues(raw, ch.cues, tg.extended, tg.onlyIfCueSectionExist)
			if err != nil {
				return res, fmt.Errorf("writeback: %s: %w", tg.rel, err)
			}
			if !changed {
				continue
			}
			if err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, tg.rel); err != nil {
				return res, err
			}
			if err := atomicWriteFile(fullPath, updated, opts.BeforeRename); err != nil {
				return res, err
			}
			res.FilesUpdated++
		}
	}

	oneLibRel := "PIONEER/rekordbox/exportLibrary.db"
	oneLibFull := filepath.Join(absMount, filepath.FromSlash(oneLibRel))
	if oneLibOK && (len(oneLibUpdates) > 0 || len(oneLibMetadata) > 0) {
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
			if opts.BeforeRename != nil {
				if err := opts.BeforeRename(oneLibFull); err != nil {
					_ = os.Remove(tmpPath)
					return res, err
				}
			}
			if err := os.Rename(tmpPath, oneLibFull); err != nil {
				_ = os.Remove(tmpPath)
				return res, err
			}
			_ = syncDir(filepath.Dir(oneLibFull))
			res.OneLibraryUpdated = true
			res.FilesUpdated++
		}
	}

	if !oneLibOK && len(pdbMetadata) > 0 {
		pdbRel := "PIONEER/rekordbox/export.pdb"
		pdbFull := filepath.Join(absMount, filepath.FromSlash(pdbRel))
		if _, err := os.Stat(pdbFull); err == nil {
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
			if opts.BeforeRename != nil {
				if err := opts.BeforeRename(pdbFull); err != nil {
					_ = os.Remove(tmpPath)
					return res, err
				}
			}
			if err := os.Rename(tmpPath, pdbFull); err != nil {
				_ = os.Remove(tmpPath)
				return res, err
			}
			_ = syncDir(filepath.Dir(pdbFull))
			res.FilesUpdated++
		}
	}

	if res.FilesUpdated > 0 {
		res.BackupDir = filepath.Join(".enginex-backup", dateDir)
	}
	return res, nil
}

func readStickRekordboxLibrary(ctx context.Context, mountPath string) (*rekordbox.Library, bool, error) {
	var errs []error
	oneLibPath := filepath.Join(mountPath, "PIONEER", "rekordbox", "exportLibrary.db")
	if _, err := os.Stat(oneLibPath); err == nil {
		lib, err := rekordbox.ReadSQLite(ctx, oneLibPath, nil)
		if err == nil {
			return lib, true, nil
		}
		errs = append(errs, err)
	}
	pdbPath := filepath.Join(mountPath, "PIONEER", "rekordbox", "export.pdb")
	if _, err := os.Stat(pdbPath); err == nil {
		lib, err := rekordbox.ReadPDB(pdbPath)
		if err == nil {
			return lib, false, nil
		}
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, false, errors.Join(errs...)
	}
	return nil, false, fmt.Errorf("writeback: no rekordbox export found on %s", mountPath)
}

func resolveRekordboxTrack(srcJSON []byte, enginePath string, byID map[uint32]rekordbox.Track, byPath map[string]rekordbox.Track) (rekordbox.Track, bool) {
	if len(srcJSON) > 0 {
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
	norm := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(enginePath)), "../")
	if tr, ok := byPath[norm]; ok {
		return tr, true
	}
	return rekordbox.Track{}, false
}

func readStickTrackCues(mountPath, analysisPath string) []rekordbox.Cue {
	rel, err := safeANLZRelPath(analysisPath)
	if err != nil {
		return nil
	}
	base := strings.TrimSuffix(rel, filepath.Ext(rel))
	for _, ext := range []string{".EXT", ".DAT"} {
		b, err := os.ReadFile(filepath.Join(mountPath, filepath.FromSlash(base+ext)))
		if err != nil {
			continue
		}
		a, err := rekordbox.ParseAnalysis(b)
		if err == nil && a != nil {
			return a.Cues
		}
	}
	return nil
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

func mergeEngineCues(orig []rekordbox.Cue, dcues []enginedb.DisplayCue, dloops []enginedb.DisplayLoop) []rekordbox.Cue {
	var out []rekordbox.Cue
	usedPads := make(map[uint32]bool)

	for _, c := range dcues {
		if c.MS < 0 {
			continue
		}
		ms := uint32(math.Round(c.MS))
		rgb := parseHexRGBA(c.C)
		code := rekordbox.NearestCueColorCode(rgb)
		if c.I >= 0 && c.I < 8 {
			pad := uint32(c.I + 1)
			usedPads[pad] = true
			out = append(out, rekordbox.Cue{
				HotCue:    pad,
				TimeMS:    ms,
				Name:      c.N,
				Color:     rgb,
				ColorCode: code,
				ColorID:   code,
				Extended:  true,
			})
		} else {
			out = append(out, rekordbox.Cue{
				HotCue:    0,
				TimeMS:    ms,
				Name:      c.N,
				Color:     rgb,
				ColorCode: code,
				Extended:  true,
			})
		}
	}

	for idx, l := range dloops {
		if l.StartMS < 0 || l.EndMS <= l.StartMS {
			continue
		}
		rgb := parseHexRGBA(l.C)
		code := rekordbox.NearestCueColorCode(rgb)
		hotCue := uint32(0)
		if idx < 8 && !usedPads[uint32(idx+1)] {
			hotCue = uint32(idx + 1)
			usedPads[hotCue] = true
		}
		out = append(out, rekordbox.Cue{
			HotCue:    hotCue,
			TimeMS:    uint32(math.Round(l.StartMS)),
			EndMS:     uint32(math.Round(l.EndMS)),
			Loop:      true,
			Name:      l.N,
			Color:     rgb,
			ColorCode: code,
			ColorID:   code,
			Extended:  true,
		})
	}

	for _, oc := range orig {
		if oc.HotCue != 0 {
			continue
		}
		dup := false
		for _, nc := range out {
			if nc.HotCue == 0 && nc.Loop == oc.Loop && absDiffU32(nc.TimeMS, oc.TimeMS) <= 2 &&
				(!nc.Loop || absDiffU32(nc.EndMS, oc.EndMS) <= 2) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, oc)
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
		if absDiffU32(x.TimeMS, y.TimeMS) > 2 {
			return false
		}
		if x.Loop && absDiffU32(x.EndMS, y.EndMS) > 2 {
			return false
		}
		if x.Color != y.Color && x.ColorCode != y.ColorCode {
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

func parseHexRGBA(s string) [4]byte {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return [4]byte{}
	}
	r, err1 := strconv.ParseUint(s[0:2], 16, 8)
	g, err2 := strconv.ParseUint(s[2:4], 16, 8)
	b, err3 := strconv.ParseUint(s[4:6], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return [4]byte{}
	}
	return [4]byte{255, byte(r), byte(g), byte(b)}
}

func safeANLZRelPath(p string) (string, error) {
	p = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(p)), "/")
	clean := filepath.ToSlash(filepath.Clean(p))
	if !strings.HasPrefix(clean, "PIONEER/USBANLZ/") || strings.Contains(clean, "..") {
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
