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
	"sync"
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
	// Session is what earlier passes of a long-lived caller (the stick's watcher, #321) found
	// the stick holding; nil for a single pass.
	Session *CueSession
	// Keys keeps the exportLibrary.db keys of a long-lived caller (in memory only). Without it
	// the pass derives them once for its decrypt and encrypt and forgets them (#321).
	Keys *rekordbox.KeyCache
	// Settled, for a pass that reads m.db while Engine holds it (the watcher, #321), says
	// whether m.db stayed as it was during the read. m.db is then opened immutable (no lock, no
	// journal), and an error drops the pass before it records or writes anything.
	Settled func() error
}

var (
	errVolumeBusy = errors.New("writeback: volume busy")
	errConverting = errors.New("writeback: conversion in progress")
)

// WriteBackResult summarizes a deck-edit write-back pass on a stick.
type WriteBackResult struct {
	TracksChecked int `json:"tracksChecked"`
	// TracksRead counts the PerformanceData rows the pass read: those whose Track.lastEditTime
	// moved since the stick last held them (#321).
	TracksRead        int    `json:"tracksRead"`
	TracksChanged     int    `json:"tracksChanged"`
	FilesUpdated      int    `json:"filesUpdated"`
	OneLibraryUpdated bool   `json:"oneLibraryUpdated"`
	BackupDir         string `json:"backupDir,omitempty"`
}

var usbPreparingFlagPath = "/run/enginex-usb-preparing" // the edisksd hook's mark of a conversion at mount (tests move it)

// CueSession is what the passes over one mounted stick found it holding since the mount: a row
// by its Track.lastEditTime, in memory only. It goes to the state with the next pass that writes
// the stick anyway (#321).
type CueSession struct {
	mu   sync.Mutex
	rows map[int64]syncedRow
}

// syncedRow is a BridgeSynced row: track id, Track.lastEditTime as read, SyncedDigest.
type syncedRow struct {
	id     int64
	edit   any
	digest []byte
}

func (s *CueSession) has(id int64, edit any) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	return ok && sameEdit(r.edit, edit)
}

func (s *CueSession) add(rows []syncedRow) {
	if s == nil || len(rows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[int64]syncedRow{}
	}
	for _, r := range rows {
		s.rows[r.id] = r
	}
}

func (s *CueSession) take() []syncedRow {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]syncedRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	s.rows = nil
	return out
}

// sameEdit compares two Track.lastEditTime values as SQLite returned them (an integer of
// strftime('%s') for Engine's own writes; compared as text so any other form still matches itself).
func sameEdit(a, b any) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

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
	// A record dated in this second or the one before is not trusted, nor made: Engine may
	// write the row again within the same second, and its date would not move (#321).
	start := time.Now().Unix()
	trusted := func(edit any) bool {
		if v, ok := edit.(int64); ok {
			return v < start-1
		}
		return true
	}
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
			return res, errConverting
		}
	}

	unlock, err := lockVolume(absMount)
	if err != nil {
		return res, fmt.Errorf("%w: %w", errVolumeBusy, err)
	}
	defer unlock()
	keys := opts.Keys
	if keys == nil {
		keys = rekordbox.NewKeyCache()
		defer keys.Clear()
	}

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
	settle := func() error { return nil }
	if opts.Settled != nil {
		// Engine holds m.db and takes no locks (#301 sandbox): read the file as it is, without
		// a lock or the journal, and let Settled tell whether it moved meanwhile.
		mURI.RawQuery = "mode=ro&immutable=1"
		settle = opts.Settled
	}
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

	hasWrittenTable, hasAnalysisTable, hasSyncedTable := 0, 0, 0
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name='BridgeWritten'`).Scan(&hasWrittenTable)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name='BridgeAnalysis'`).Scan(&hasAnalysisTable)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM bridge.sqlite_master WHERE type='table' AND name='BridgeSynced'`).Scan(&hasSyncedTable)

	// Only a row whose Track.lastEditTime is not the one BridgeSynced has is read (#321):
	// Engine's trigger dates every write of a row's analysis, cues and loops, so this scan
	// reads Track and the state, never PerformanceData's pages of an untouched track.
	query := `SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0), COALESCE(t.lastEditTime, 0), NULL, NULL
		FROM bridge.BridgeState bs JOIN Track t ON t.id = bs.trackId`
	if hasSyncedTable > 0 {
		query = `SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0), COALESCE(t.lastEditTime, 0), sy.lastEditTime, sy.digest
			FROM bridge.BridgeState bs JOIN Track t ON t.id = bs.trackId
			LEFT JOIN bridge.BridgeSynced sy ON sy.trackId = bs.trackId`
	}
	written := `NULL`
	if hasWrittenTable > 0 {
		written = `(SELECT digest FROM bridge.BridgeWritten WHERE trackId = pd.trackId)`
	}
	perfStmt, err := db.PrepareContext(ctx, `SELECT pd.beatData, pd.trackData, pd.quickCues, pd.loops, COALESCE(pd.activeOnLoadLoops, 0), `+written+`,
		       COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,'')
		FROM PerformanceData pd JOIN Track t ON t.id = pd.trackId WHERE pd.trackId = ?`)
	if err != nil {
		return res, fmt.Errorf("writeback: query tracks: %w", err)
	}
	defer perfStmt.Close()

	type candidateTrack struct {
		enginePath   string
		engineID     int64
		pdbImportKey int64
		edit         any
	}
	var reads []candidateTrack
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return res, fmt.Errorf("writeback: query tracks: %w", err)
	}
	defer rows.Close()
	synced := map[int64][]byte{}
	for rows.Next() {
		var c candidateTrack
		var syncedEdit any
		var syncedDigest []byte
		if err := rows.Scan(&c.enginePath, &c.engineID, &c.pdbImportKey, &c.edit, &syncedEdit, &syncedDigest); err != nil {
			return res, err
		}
		res.TracksChecked++
		if syncedEdit != nil && sameEdit(syncedEdit, c.edit) && trusted(syncedEdit) || opts.Session.has(c.engineID, c.edit) {
			continue
		}
		synced[c.engineID] = syncedDigest
		reads = append(reads, c)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	_ = rows.Close()

	type dirtyTrack struct {
		candidateTrack
		deck     enginedb.DeckCues
		cues     bool // the deck's cues may differ from the stick's
		metadata rekordbox.Metadata
		seen     syncedRow
	}
	var dirty []dirtyTrack
	var confirmed []syncedRow // rows the stick holds as they are now
	for _, c := range reads {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		var activeLoops int64
		var beatData, trackData, quickCues, loopsBlob, writtenDigest []byte
		var md rekordbox.Metadata
		if err := perfStmt.QueryRowContext(ctx, c.engineID).Scan(&beatData, &trackData, &quickCues, &loopsBlob, &activeLoops, &writtenDigest, &md.Genre, &md.Comment, &md.Label); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return res, err
		}
		res.TracksRead++
		seen := syncedRow{id: c.engineID, edit: c.edit, digest: enginedb.SyncedDigest(beatData, quickCues, loopsBlob, activeLoops, md.Genre, md.Comment, md.Label)}
		if bytes.Equal(synced[c.engineID], seen.digest) {
			confirmed = append(confirmed, seen)
			continue
		}
		d := dirtyTrack{candidateTrack: c, metadata: md, seen: seen}
		// Fast path: tracks untouched since the last import stay at their BridgeWritten
		// digest. Their cues are not read again; their metadata (#314) is compared with the
		// export's. Tracks with loops are still inspected because activeOnLoadLoops is outside
		// the digest.
		if len(trackData) > 0 && !(len(writtenDigest) > 0 && activeLoops == 0 && !enginedb.HasAnyLoop(loopsBlob) &&
			bytes.Equal(writtenDigest, enginedb.WrittenDigest(beatData, quickCues, loopsBlob))) {
			if deck, err := enginedb.ParseDeckCuesAndLoops(trackData, quickCues, loopsBlob, activeLoops); err == nil {
				d.deck, d.cues = deck, true
			}
		}
		dirty = append(dirty, d)
	}
	// What the pass found the stick holding is kept for the session and written to the state
	// with a pass that writes the stick anyway, or once on a stick whose state has no record
	// yet (an earlier build's import): without it every later pass would read every row.
	record := func(written []int64) {
		if len(written) == 0 && settle() != nil {
			return // the read may be torn: nothing it found is kept
		}
		kept := confirmed[:0]
		for _, r := range confirmed {
			if trusted(r.edit) {
				kept = append(kept, r)
			}
		}
		confirmed = kept
		if res.FilesUpdated > 0 || hasSyncedTable == 0 {
			// What the session knew of a row written now is stale: the stick holds the row as
			// this pass read it, or (dated too recently to say so) the row goes unrecorded.
			drop := map[int64]bool{}
			for _, id := range written {
				drop[id] = true
			}
			var rows []syncedRow
			for _, r := range opts.Session.take() {
				if !drop[r.id] {
					rows = append(rows, r)
				}
			}
			recordSynced(ctx, stateDBPath, append(rows, confirmed...), written)
			return
		}
		opts.Session.add(confirmed)
	}

	if len(dirty) == 0 {
		record(nil)
		return res, nil
	}

	rbLib, oneLibRel, err := readStickRekordboxLibrary(ctx, resolver, absMount, keys)
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
		seen         syncedRow
	}
	var changes []trackChange

	for _, cand := range dirty {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rbTrack, ok := resolveRekordboxTrack(ctx, db, hasAnalysisTable > 0, cand.engineID, cand.pdbImportKey, cand.enginePath, byID, byPath, byPathFold)
		if !ok || rbTrack.ID == 0 {
			confirmed = append(confirmed, cand.seen) // not in the export: nothing to write
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
			confirmed = append(confirmed, cand.seen)
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
			seen:         cand.seen,
		})
	}
	_ = db.Close()

	if len(changes) == 0 {
		record(nil)
		return res, nil
	}
	if err := settle(); err != nil {
		return res, err
	}
	// Where this pass may leave a temporary file if the stick goes in the middle: the next
	// mount removes them (CleanWriteBackLeftovers). The mark goes once everything is written.
	var tmpDirs []string
	for _, ch := range changes {
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(strings.TrimPrefix(ch.analysisPath, "/"))))
	}
	if oneLibRel != "" {
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(oneLibRel)))
	} else if pdbRel, err := resolver.Resolve("PIONEER/rekordbox/export.pdb"); err == nil {
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(pdbRel))) // its metadata (#314)
	}
	if err := markWriteBack(engineDBDir, dateDir, tmpDirs); err != nil {
		return res, err
	}
	finished := false
	defer func() {
		if finished {
			_ = os.Remove(filepath.Join(engineDBDir, writeBackMarkName))
		}
	}()

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
		writtenIDs = append(writtenIDs, ch.engineID)
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
			if _, err := rekordbox.UpdateOneLibraryEditsKeys(ctx, oneLibFull, oneLibUpdates, oneLibMetadata, tmpPath, keys); err != nil {
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

	for _, ch := range changes {
		confirmed = append(confirmed, ch.seen)
	}
	if res.FilesUpdated > 0 {
		res.BackupDir = filepath.Join(".enginex-backup", dateDir)
	} else {
		writtenIDs = nil
	}
	record(writtenIDs)
	finished = true
	return res, nil
}

// writeBackMarkName lists, one per line, the stick's directories where a write-back in progress
// may leave a temporary file (.enginex-*.tmp, and .bak-*.tmp under .enginex-backup/<date>/).
const writeBackMarkName = ".rb-bridge-writeback"

func markWriteBack(engineDBDir, dateDir string, dirs []string) error {
	seen := map[string]bool{}
	var b strings.Builder
	for _, d := range dirs {
		for _, x := range []string{d, filepath.Join(".enginex-backup", dateDir, d)} {
			if x = filepath.ToSlash(filepath.Clean(x)); !seen[x] {
				seen[x] = true
				b.WriteString(x + "\n")
			}
		}
	}
	return atomicWriteFile(filepath.Join(engineDBDir, writeBackMarkName), []byte(b.String()), nil)
}

// CleanWriteBackLeftovers removes what a write-back cut by a pull left on the stick at mount:
// its temporary files in the directories its mark lists, then the mark. One stat when there
// is no mark. It returns the number of files removed.
func CleanWriteBackLeftovers(mount string) int {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	mark := filepath.Join(dir, writeBackMarkName)
	b, err := os.ReadFile(mark)
	if err != nil {
		return 0
	}
	n := 0
	for _, rel := range strings.Split(string(b), "\n") {
		rel = filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel)))
		if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		for _, pat := range []string{".enginex-*.tmp", ".bak-*.tmp"} {
			matches, _ := filepath.Glob(filepath.Join(mount, rel, pat))
			for _, m := range matches {
				if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() && os.Remove(m) == nil {
					n++
				}
			}
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".enginex-*.tmp")) // the mark's own temp
	for _, m := range matches {
		if os.Remove(m) == nil {
			n++
		}
	}
	_ = os.Remove(mark)
	return n
}

// recordSynced notes in the state what the stick holds of rows (BridgeSynced, #321) and drops
// the BridgeWritten digests of tracks whose cues were written back to the stick. Otherwise,
// deleting a previously written-back cue on a later session would restore the original import
// digest and be skipped by the untouched-track fast path. One transaction; a failure leaves
// the rows to be read again by the next pass.
func recordSynced(ctx context.Context, stateDBPath string, rows []syncedRow, written []int64) {
	if len(rows) == 0 && len(written) == 0 {
		return
	}
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
	if _, err = tx.ExecContext(ctx, strings.Replace(enginedb.BridgeSyncedSchema, "bridge.", "", 1)); err != nil {
		return
	}
	if len(written) > 0 {
		var n int
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeWritten'`).Scan(&n) != nil {
			return
		}
		for _, id := range written {
			if n > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM BridgeWritten WHERE trackId = ?`, id); err != nil {
					return
				}
			}
			// The stick now holds the row as written; an older record would let a later edit
			// that restores the older cues pass for what the stick holds.
			if _, err := tx.ExecContext(ctx, `DELETE FROM BridgeSynced WHERE trackId = ?`, id); err != nil {
				return
			}
		}
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeSynced(trackId, lastEditTime, digest) VALUES(?, ?, ?)`, r.id, r.edit, r.digest); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func readStickRekordboxLibrary(ctx context.Context, r *compat.Resolver, mountPath string, keys *rekordbox.KeyCache) (*rekordbox.Library, string, error) {
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
		lib, err := rekordbox.ReadSQLiteKeys(ctx, r.FullPath(rel), nil, keys)
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
