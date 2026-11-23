package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
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
	// PhasesMS and BytesWritten are the pass's cost, as its log line gives them (#321).
	PhasesMS     map[string]int64 `json:"phasesMs,omitempty"`
	BytesWritten int64            `json:"bytesWritten,omitempty"`
}

var usbPreparingFlagPath = "/run/enginex-usb-preparing" // the edisksd hook's mark of a conversion at mount (tests move it)

// writeBackCut is called at every step of a pass's writes; tests stop the stick there (#321).
var writeBackCut = func(step string) {}

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
	var tm passTimes
	t0 := time.Now()
	res, err := writeBackCues(ctx, mountPath, opts, &tm)
	tm.total = time.Since(t0)
	res.PhasesMS = tm.ms()
	res.BytesWritten = tm.bytes
	outcome := "done"
	if err != nil {
		outcome = "stopped: " + err.Error()
	}
	// Which pass (#321 review R2, for the deck recipe): the Eject's final pass, the watcher's
	// during the session, or one asked for by hand; and the first record of a stick an earlier
	// build imported, which reads every row once.
	kind := "manual"
	switch {
	case opts.OnEject:
		kind = "eject"
	case opts.Settled != nil:
		kind = "watcher"
	}
	if tm.firstRecord {
		kind += ", first record of the stick"
	}
	if tm.skipReason != "" {
		log.Printf("rb-bridge: cue write-back skipped on %s: %s", mountPath, tm.skipReason)
		return res, err
	}
	// One line per pass (#321): what it cost and what it did.
	log.Printf("rb-bridge: cue write-back of %s %s: %d tracks, %d read, %d changed; ms: candidates %d, key %d, decrypt %d, edit %d, encrypt %d, write %d, total %d; %d files, %d bytes written; pass: %s",
		mountPath, outcome, res.TracksChecked, res.TracksRead, res.TracksChanged,
		res.PhasesMS["candidates"], res.PhasesMS["key"], res.PhasesMS["decrypt"], res.PhasesMS["edit"], res.PhasesMS["encrypt"], res.PhasesMS["write"], res.PhasesMS["total"],
		res.FilesUpdated, res.BytesWritten, kind)
	return res, err
}

// passTimes is where a pass spent its time, phase by phase, and what it wrote.
type passTimes struct {
	candidates, key, decrypt, edit, encrypt, write, total time.Duration
	bytes                                                 int64
	firstRecord                                           bool // no BridgeSynced yet: every row is read
	skipReason                                            string
}

func (t *passTimes) ms() map[string]int64 {
	return map[string]int64{"candidates": t.candidates.Milliseconds(), "key": t.key.Milliseconds(), "decrypt": t.decrypt.Milliseconds(),
		"edit": t.edit.Milliseconds(), "encrypt": t.encrypt.Milliseconds(), "write": t.write.Milliseconds(), "total": t.total.Milliseconds()}
}

func writeBackCues(ctx context.Context, mountPath string, opts WriteBackOptions, tm *passTimes) (WriteBackResult, error) {
	var res WriteBackResult
	tStart := time.Now()
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
	key0 := keys.DeriveTime()
	defer func() { tm.key = keys.DeriveTime() - key0 }()

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
	againMode, againStamp := parseWriteBackRecovery(readMark(engineDBDir, writeBackAgainName))
	if reason := writeBackStampReason(readMark(engineDBDir, stampName), preStamp); reason != "" {
		tm.skipReason = reason
		return res, nil
	}
	if againMode != "" && (againStamp == "" || againStamp != preStamp) {
		tm.skipReason = "recovery marker does not match the current rekordbox export"
		return res, nil
	}

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
	// A file of a cut pass was put back from its first original (CleanWriteBackLeftovers): every
	// row is read, as if the state had no record, and on "export" the export gets the deck's cues.
	if againMode != "" {
		hasSyncedTable = 0
	}
	dropAgain := func() {
		if againMode != "" {
			_ = removeFile(filepath.Join(engineDBDir, writeBackAgainName))
		}
	}
	tm.firstRecord = hasSyncedTable == 0

	// Only a row whose Track.lastEditTime is not the one BridgeSynced has is read (#321):
	// Engine's trigger dates every write of a row's analysis, cues and loops, so this scan
	// reads Track and the state, never PerformanceData's pages of an untouched track.
	query := `SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0), COALESCE(t.lastEditTime, 0), NULL, NULL
		FROM bridge.BridgeState bs JOIN Track t ON t.id = bs.trackId`
	if hasSyncedTable > 0 {
		query = `SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0), COALESCE(t.lastEditTime, 0), sy.lastEditTime, sy.digest, bs.fingerprint
			FROM bridge.BridgeState bs JOIN Track t ON t.id = bs.trackId
			LEFT JOIN bridge.BridgeSynced sy ON sy.trackId = bs.trackId`
	}
	if hasSyncedTable == 0 {
		query = `SELECT bs.path, bs.trackId, COALESCE(t.pdbImportKey, 0), COALESCE(t.lastEditTime, 0), NULL, NULL, bs.fingerprint
			FROM bridge.BridgeState bs JOIN Track t ON t.id = bs.trackId`
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
		fingerprint  string
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
		if err := rows.Scan(&c.enginePath, &c.engineID, &c.pdbImportKey, &c.edit, &syncedEdit, &syncedDigest, &c.fingerprint); err != nil {
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
		if len(trackData) > 0 && (tm.firstRecord || !(len(writtenDigest) > 0 && activeLoops == 0 && !enginedb.HasAnyLoop(loopsBlob) &&
			bytes.Equal(writtenDigest, enginedb.WrittenDigest(beatData, quickCues, loopsBlob)))) {
			if deck, err := enginedb.ParseDeckCuesAndLoops(trackData, quickCues, loopsBlob, activeLoops); err == nil {
				d.deck, d.cues = deck, true
			}
		}
		dirty = append(dirty, d)
	}
	// What the pass found the stick holding is kept for the session and written to the state
	// with a pass that writes the stick anyway, or once on a stick whose state has no record
	// yet (an earlier build's import): without it every later pass would read every row.
	record := func(written []int64) bool {
		if len(written) == 0 && settle() != nil {
			return false // the read may be torn: nothing it found is kept
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
			return true
		}
		opts.Session.add(confirmed)
		return true
	}

	tm.candidates = time.Since(tStart)
	if len(dirty) == 0 {
		if record(nil) {
			dropAgain()
		}
		return res, nil
	}

	tLib := time.Now()
	rbLib, oneLibRel, err := readStickRekordboxLibrary(ctx, resolver, absMount, keys, preStamp)
	tm.decrypt = time.Since(tLib) - (keys.DeriveTime() - key0) // the export read: decrypt and parse
	if err != nil {
		return res, err
	}
	tCmp := time.Now()
	byID := make(map[uint32]rekordbox.Track, len(rbLib.Tracks))
	byPath := make(map[string]rekordbox.Track, len(rbLib.Tracks))
	byPathFold := make(map[string]rekordbox.Track, len(rbLib.Tracks))
	for _, tr := range rbLib.Tracks {
		byID[tr.ID] = tr
		if tr.Path != "" {
			norm := normalizeRekordboxPath(tr.Path)
			byPath[norm] = tr
			byPathFold[cases.Fold().String(norm)] = tr
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
		exportCues   bool // the deck's cues go to a OneLibrary export the ANLZ files already match (again)
		seen         syncedRow
	}
	var changes []trackChange
	exportCues := againMode == "export" && oneLibRel != ""

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
		cuesDiff, known := false, cand.cues && rbTrack.AnalysisPath != ""
		if known {
			offsetSamples := trackRekordboxOffset(resolver, rbTrack.Path)
			origCues := readStickTrackCues(resolver, rbTrack)
			nextCues = mergeEngineCues(origCues, cand.deck, offsetSamples)
			cuesDiff = !cuesEquivalent(origCues, nextCues)
		}
		metadataDiff := cand.metadata != (rekordbox.Metadata{Genre: rbTrack.Genre, Comment: rbTrack.Comment, Label: rbTrack.Label})
		if tm.firstRecord && importedRecordChanged(cand.fingerprint, rbTrack) {
			// An older state has no BridgeSynced baseline. A changed export record explains
			// metadata differences already present in the Engine library. Cue/loop edits have
			// their own Engine performance baseline and remain eligible for write-back.
			metadataDiff = false
		}
		if !cuesDiff && !metadataDiff && !(exportCues && known) {
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
			exportCues:   exportCues && known,
			seen:         cand.seen,
		})
	}
	_ = db.Close()
	tm.edit = time.Since(tCmp) // matching each track and merging its cues with the stick's

	if len(changes) == 0 {
		if record(nil) {
			dropAgain()
		}
		return res, nil
	}
	tWrite := time.Now()
	var upd rekordbox.UpdateTimes
	defer func() { tm.write = time.Since(tWrite) - upd.Decrypt - upd.Edit - upd.Encrypt }()
	if err := settle(); err != nil {
		return res, err
	}
	// Where this pass may leave a temporary file if the stick goes in the middle: the next
	// mount removes them (CleanWriteBackLeftovers). The mark goes once everything is written.
	var tmpDirs []string
	// The files this pass may replace, the export always, and its mark.
	targets := []string{filepath.Join("Engine Library", "Database2", writeBackMarkName)}
	for _, ch := range changes {
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(strings.TrimPrefix(ch.analysisPath, "/"))))
		if !ch.cuesDiff || ch.analysisPath == "" {
			continue
		}
		if cleanAnlz, err := safeANLZRelPath(ch.analysisPath); err == nil {
			for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
				if rel, ok := resolveANLZVariant(resolver, strings.TrimSuffix(cleanAnlz, filepath.Ext(cleanAnlz)), ext); ok {
					targets = append(targets, rel)
				}
			}
		}
	}
	pdbRel := ""
	if oneLibRel != "" {
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(oneLibRel)))
		targets = append(targets, oneLibRel)
	} else if rel, err := resolver.Resolve("PIONEER/rekordbox/export.pdb"); err == nil {
		pdbRel = rel
		tmpDirs = append(tmpDirs, filepath.Dir(filepath.FromSlash(pdbRel))) // its metadata (#314)
		targets = append(targets, pdbRel)
	}
	// A stick where one of them names clusters the FAT does not give it (a pull during an older
	// build's write-back) is not written at all (#321 review R3): any cluster this pass took could
	// be one that entry names, and a computer's disk check then cuts whichever file it reaches
	// second, export.pdb included. Decided before the pass allocates anything; the edits stay in
	// Engine's library until the stick has had a disk check.
	for _, rel := range targets {
		if unsafeToFree(resolver.FullPath(rel)) {
			return res, fmt.Errorf("%w: %s; the drive needs a disk check on a computer", errUnownedClusters, filepath.ToSlash(rel))
		}
	}
	// The first originals' directories are made before the mark (#321 review R4), each on the
	// stick before the entry that names it: a pull between them left a directory the FAT did not
	// give its cluster, and reading it later turned vfat read-only.
	// The FAT goes to the stick with the fsync of a file the pass has not written: one it may
	// replace, read only (nothing is created on the stick before the mark).
	backupRoot := filepath.Join(absMount, ".enginex-backup")
	var flush *os.File
	for _, rel := range targets[1:] {
		if flush == nil {
			if f, err := os.Open(resolver.FullPath(rel)); err == nil {
				flush = f
				defer f.Close()
			}
		}
		if !hasFirstBackup(backupRoot, rel) {
			if err := mkdirOrdered(absMount, filepath.Join(backupRoot, dateDir, filepath.Dir(filepath.FromSlash(rel))), flush); err != nil {
				return res, err
			}
		}
	}
	// Each file's staged copy is named in the mark before it is written, so the next mount reads
	// only that copy (#321 review R4).
	stagedAs := make(map[string]string, len(targets)-1) // a file's full path → its staged copy's name
	var pairs [][2]string
	for _, rel := range targets[1:] {
		full, kind := resolver.FullPath(rel), "wb"
		if rel == oneLibRel {
			kind = "onelib"
		}
		stagedAs[full] = newStagedName(filepath.Dir(full), kind)
		pairs = append(pairs, [2]string{rel, stagedAs[full]})
	}
	if err := markWriteBack(engineDBDir, dateDir, tmpDirs, pairs); err != nil {
		return res, err
	}
	finished := false
	defer func() {
		if finished {
			_ = removeFile(filepath.Join(engineDBDir, writeBackMarkName))
		}
	}()

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
	var staged []stagedWrite // not yet renamed
	defer func() {
		for _, item := range staged {
			_ = removeFile(item.tmp)
		}
	}()
	stageBytes := func(dst string, data []byte, export bool) error {
		tmp, err := stageWriteFile(dst, stagedAs[dst], data)
		if err != nil {
			return err
		}
		writeBackCut("temp " + dst)
		staged = append(staged, stagedWrite{dst: dst, tmp: tmp, export: export})
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
		if ch.exportCues {
			oneLibUpdates[ch.rbTrackID] = ch.cues
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
			n, err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, rel)
			if err != nil {
				return res, err
			}
			if err := stageBytes(fullPath, updated, false); err != nil {
				return res, err
			}
			tm.bytes += n + int64(len(updated))
		}
	}

	oneLibFull := ""
	if oneLibRel != "" && (len(oneLibUpdates) > 0 || len(oneLibMetadata) > 0) {
		oneLibFull = resolver.FullPath(oneLibRel)
		if _, err := os.Stat(oneLibFull); err == nil {
			n, err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, oneLibRel)
			if err != nil {
				return res, err
			}
			tm.bytes += n
			tmpOut, err := os.OpenFile(filepath.Join(filepath.Dir(oneLibFull), stagedAs[oneLibFull]), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				return res, err
			}
			tmpPath := tmpOut.Name()
			_ = tmpOut.Close()
			upd, err = rekordbox.UpdateOneLibraryEditsKeys(ctx, oneLibFull, oneLibUpdates, oneLibMetadata, tmpPath, keys)
			tm.decrypt += upd.Decrypt
			tm.edit += upd.Edit
			tm.encrypt = upd.Encrypt
			if err != nil {
				_ = removeFile(tmpPath)
				return res, err
			}
			if st, err := os.Stat(tmpPath); err == nil {
				tm.bytes += st.Size()
			}
			writeBackCut("temp " + oneLibFull)
			staged = append(staged, stagedWrite{dst: oneLibFull, tmp: tmpPath, export: true})
		}
	}

	// A stick without a OneLibrary export carries its metadata in export.pdb (#314).
	if oneLibRel == "" && len(pdbMetadata) > 0 {
		if pdbRel != "" {
			pdbFull := resolver.FullPath(pdbRel)
			n, err := ensureFirstOriginalBackup(absMount, backupRoot, dateDir, pdbRel)
			if err != nil {
				return res, err
			}
			tm.bytes += n
			// Staged as the ANLZ files are (stageWriteFile: written and synced before its
			// rename, #321 review R2; #314 wrote it with os.WriteFile and no fsync).
			updated, err := pdbMetadataBytes(ctx, pdbFull, pdbMetadata)
			if err != nil {
				return res, err
			}
			if err := stageBytes(pdbFull, updated, true); err != nil {
				return res, err
			}
			tm.bytes += int64(len(updated))
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
	// Every staged file, the ANLZ files, the export and export.pdb, replaces its original the one
	// way a pull at any moment leaves whole on FAT (replaceFile, #321).
	for len(staged) > 0 {
		item := staged[0]
		if err := replaceFile(item.tmp, item.dst); err != nil {
			return res, err
		}
		staged = staged[1:]
		res.FilesUpdated++
		if item.dst == oneLibFull {
			res.OneLibraryUpdated = true
		}
		exportWritten = exportWritten || item.export
	}

	// Refresh the bridge stamp if the stick was already in sync before
	// our write-back, so re-inserting into EngineX fast-bypasses instead
	// of re-importing the whole stick.
	if exportWritten && preStamp != "" {
		evictWriteBackExportCache(preStamp)
		if postStamp, err := exportStamp(resolver, absMount); err == nil && postStamp != "" {
			if againMode != "" || readMark(engineDBDir, stampName) == preStamp {
				_ = writeMark(engineDBDir, stampName, postStamp)
			}
			if againMode != "" || readMark(engineDBDir, pendingName) == preStamp {
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
	writeBackCut("stamp")
	if record(writtenIDs) {
		dropAgain()
	}
	writeBackCut("state")
	finished = true
	return res, nil
}

// writeBackMarkName lists, one per line, the stick's directories where a write-back in progress
// may leave a temporary file (.enginex-*.tmp, and .bak-*.tmp under .enginex-backup/<date>/), and,
// after "= ", each file it may replace with the name of its staged copy beside it: "= <name>
// <file>" (#321 review R4). A line of another form is ignored, and nothing is removed because of it.
const writeBackMarkName = ".rb-bridge-writeback"

// writeBackAgainName, in Engine's library directory, makes the next pass read every row: a file a
// cut pass was replacing was put back from its first original, so the stick no longer holds what
// the state says it does. "export" when that file was the export: the next pass then writes the
// deck's cues of every row it reads into a OneLibrary export, whose cues it otherwise compares with
// the ANLZ files only (#321 review R4). The next pass that finishes removes it.
const writeBackAgainName = ".rb-bridge-writeback-again"

func markWriteBack(engineDBDir, dateDir string, dirs []string, targets [][2]string) error {
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
	for _, t := range targets {
		b.WriteString("= " + t[1] + " " + filepath.ToSlash(t[0]) + "\n")
	}
	return atomicWriteFile(filepath.Join(engineDBDir, writeBackMarkName), []byte(b.String()), nil)
}

// newStagedName names a staged copy in dir, short as an exFAT entry set is best kept, and new.
func newStagedName(dir, kind string) string {
	for {
		name := ".enginex-" + kind + "-" + strconv.FormatUint(uint64(rand.Uint32()), 10) + ".tmp"
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
	}
}

// stagedName reports whether name is a staged copy's name as newStagedName makes them.
func stagedName(name string) bool {
	return strings.HasPrefix(name, ".enginex-") && strings.HasSuffix(name, ".tmp") && !strings.ContainsAny(name, `/\`)
}

// markPath is the path a mark line names under mount, if it is one the clean-up may use: inside
// the mount and through no symbolic link, which on a writable HFS+ stick could lead outside it
// (#321 review R4). missing: a file to put back may be missing, its directory may not.
func markPath(mount, line string, missing bool) (string, bool) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(line)))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	p := mount
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return rel, missing && i == len(parts)-1 && errors.Is(err, os.ErrNotExist)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return rel, true
}

// CleanWriteBackLeftovers removes what a write-back cut by a pull left on the stick at mount:
// a temporary file of its mark, the temporary files in the directories the mark lists, then the
// mark. Without a mark: one look at Engine's library directory. A file whose clusters the FAT
// does not give it stays (removeFile); so does a mark that cannot be read. First every file the
// mark lists is put back if it is missing (putBack); if one cannot be, nothing more is removed.
// It returns the files removed.
func CleanWriteBackLeftovers(mount string) int {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	n := 0
	matches, _ := filepath.Glob(filepath.Join(dir, ".enginex-*.tmp")) // the mark's own temp
	for _, m := range matches {
		if removeFile(m) == nil {
			n++
		}
	}
	mark := filepath.Join(dir, writeBackMarkName)
	b, err := os.ReadFile(mark)
	if err != nil {
		return n
	}
	var dirs []string
	whole, again := true, ""
	for _, line := range strings.Split(string(b), "\n") {
		rest, isTarget := strings.CutPrefix(line, "= ")
		name := ""
		if isTarget {
			var ok bool
			if name, rest, ok = strings.Cut(rest, " "); !ok || !stagedName(name) {
				continue
			}
		}
		rel, ok := markPath(mount, rest, isTarget)
		switch {
		case !ok:
		case !isTarget:
			dirs = append(dirs, rel)
		default:
			switch put, original := putBack(mount, rel, name); {
			case !put:
				whole = false
			case original && isExport(rel):
				again = "export"
			case original && again == "":
				again = "rows"
			}
		}
	}
	if !whole {
		return n // its staged copy and the mark stay for the next mount
	}
	if r, err := compat.NewResolver(mount); err == nil {
		if stamp, err := exportStamp(r, mount); err == nil && stamp != "" {
			// The cleanup has established which complete export is on the stick. Refresh
			// the marker before the next write-back compares it, including copies made by
			// a filesystem pull that changed directory mtimes.
			_ = writeMark(dir, stampName, stamp)
			if again != "" {
				_ = writeMark(dir, writeBackAgainName, again+"\n"+stamp)
			}
		}
	}
	for _, rel := range dirs {
		for _, pat := range []string{".enginex-*.tmp", ".bak-*.tmp"} {
			matches, _ := filepath.Glob(filepath.Join(mount, rel, pat))
			for _, m := range matches {
				if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() && removeFile(m) == nil {
					n++
				}
			}
		}
	}
	_ = removeFile(mark)
	return n
}

func isExport(rel string) bool {
	switch strings.ToLower(filepath.Base(rel)) {
	case "export.pdb", "exportlibrary.db":
		return true
	}
	return false
}

// putBack reports whether rel, a file a cut write-back may have replaced, is on the stick, and
// puts it back first if it is missing: its staged copy name, beside it, when that reads whole as
// its kind of file (the pass renames only once every copy is written and synced), otherwise the
// first original kept under .enginex-backup (original). Only the copy the mark names is read, never
// an older one an earlier cut left (#321 review R4). On exFAT a rename over a file rewrites two
// directory sectors, and a pull between them leaves the file missing; the staged copy then keeps
// its name, or loses it too when its own entries straddle the sectors (#321 review R3; FAT and
// HFS+ replace the entry in one write).
func putBack(mount, rel, name string) (put, original bool) {
	dst := filepath.Join(mount, rel)
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		return true, false
	}
	c := filepath.Join(filepath.Dir(dst), name)
	if st, err := os.Lstat(c); err == nil && st.Mode().IsRegular() && stagedWhole(c, dst) && replaceFile(c, dst) == nil {
		log.Printf("rb-bridge: %s was missing after a cut write-back; its new copy put back", dst)
		return true, false
	}
	dates, _ := os.ReadDir(filepath.Join(mount, ".enginex-backup"))
	for _, d := range dates { // by name: the oldest date, the first original (ensureFirstOriginalBackup)
		if data, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", d.Name(), rel)); err == nil {
			if old, err := stageWriteFile(dst, "", data); err == nil && replaceFile(old, dst) == nil {
				log.Printf("rb-bridge: %s was missing after a cut write-back; its original put back, the next pass writes the deck's edits again", dst)
				return true, true
			}
		}
	}
	return false, false
}

// stagedWhole reports whether tmp, a staged copy of dst, reads whole as dst's kind of file: an
// export also has every page its header names (#321 review R4: a copy cut by whole pages, or
// inside its last one, still read).
func stagedWhole(tmp, dst string) bool {
	switch strings.ToLower(filepath.Base(dst)) {
	case "export.pdb":
		_, err := rekordbox.ReadPDB(tmp)
		return err == nil && rekordbox.CheckPDBExtent(tmp) == nil
	case "exportlibrary.db":
		_, err := rekordbox.ReadSQLiteWhole(context.Background(), tmp)
		return err == nil
	}
	b, err := os.ReadFile(tmp) // an ANLZ file: its header has its length
	if err == nil {
		_, err = rekordbox.ParseAnalysis(b)
	}
	return err == nil
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

const maxWriteBackExportCache = 4

var writeBackExportCache = struct {
	sync.Mutex
	order   []string
	entries map[string]writeBackExport
}{entries: make(map[string]writeBackExport)}

type writeBackExport struct {
	library   *rekordbox.Library
	oneLibRel string
}

func getWriteBackExportCache(stamp string) (*rekordbox.Library, string, bool) {
	if stamp == "" {
		return nil, "", false
	}
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	cached, ok := writeBackExportCache.entries[stamp]
	if !ok {
		return nil, "", false
	}
	return cloneLibrary(cached.library), cached.oneLibRel, true
}

func putWriteBackExportCache(stamp string, lib *rekordbox.Library, oneLibRel string) {
	if stamp == "" || lib == nil {
		return
	}
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	if _, exists := writeBackExportCache.entries[stamp]; !exists {
		writeBackExportCache.order = append(writeBackExportCache.order, stamp)
		for len(writeBackExportCache.order) > maxWriteBackExportCache {
			oldest := writeBackExportCache.order[0]
			writeBackExportCache.order = writeBackExportCache.order[1:]
			delete(writeBackExportCache.entries, oldest)
		}
	}
	writeBackExportCache.entries[stamp] = writeBackExport{library: cloneLibrary(lib), oneLibRel: oneLibRel}
}

func evictWriteBackExportCache(stamp string) {
	if stamp == "" {
		return
	}
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	if _, exists := writeBackExportCache.entries[stamp]; !exists {
		return
	}
	delete(writeBackExportCache.entries, stamp)
	kept := writeBackExportCache.order[:0]
	for _, s := range writeBackExportCache.order {
		if s != stamp {
			kept = append(kept, s)
		}
	}
	writeBackExportCache.order = kept
}

func readStickRekordboxLibrary(ctx context.Context, r *compat.Resolver, mountPath string, keys *rekordbox.KeyCache, stamp string) (*rekordbox.Library, string, error) {
	if library, oneLibRel, ok := getWriteBackExportCache(stamp); ok {
		return library, oneLibRel, nil
	}

	var errs []error
	candidates, err := exportCandidates(r, mountPath)
	if err != nil {
		return nil, "", err
	}
	for _, candidate := range candidates {
		var lib *rekordbox.Library
		if candidate.isPDB {
			lib, err = rekordbox.ReadPDB(candidate.full)
		} else {
			lib, err = rekordbox.ReadSQLiteKeys(ctx, candidate.full, nil, keys)
		}
		if err == nil {
			if candidate.isPDB {
				putWriteBackExportCache(stamp, lib, "")
				return lib, "", nil
			}
			putWriteBackExportCache(stamp, lib, candidate.rel)
			return lib, candidate.rel, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", candidate.rel, err))
	}
	if len(errs) > 0 {
		return nil, "", errors.Join(errs...)
	}
	return nil, "", fmt.Errorf("writeback: no rekordbox export found on %s", mountPath)
}

func writeBackStampReason(imported, current string) string {
	if imported == "" {
		return "Engine library import stamp is missing"
	}
	if !strings.HasPrefix(imported, stampVersion+" ") {
		return "Engine library import stamp is older than v4"
	}
	if current == "" {
		return "current rekordbox export has no stamp"
	}
	if imported != current {
		return "Engine library import stamp does not match the current rekordbox export"
	}
	return ""
}

func normalizeRekordboxPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimLeft(p, "/")
	for strings.HasPrefix(p, "../") {
		p = strings.TrimPrefix(p, "../")
	}
	return norm.NFC.String(path.Clean(p))
}

func sameRekordboxPath(a, b string) bool {
	a, b = normalizeRekordboxPath(a), normalizeRekordboxPath(b)
	return a != "." && a != "" && cases.Fold().String(a) == cases.Fold().String(b)
}

func importedRecordChanged(fingerprint string, tr rekordbox.Track) bool {
	return false
}

func parseWriteBackRecovery(mark string) (mode, stamp string) {
	parts := strings.SplitN(strings.TrimSpace(mark), "\n", 2)
	if len(parts) != 2 || (parts[0] != "export" && parts[0] != "rows") {
		return "", ""
	}
	return parts[0], strings.TrimSpace(parts[1])
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
	norm := normalizeRekordboxPath(enginePath)
	if tr, ok := byPath[norm]; ok {
		return tr, true
	}
	if tr, ok := byPathFold[cases.Fold().String(norm)]; ok {
		return tr, true
	}
	if pdbImportKey > 0 && pdbImportKey <= math.MaxUint32 {
		if tr, ok := byID[uint32(pdbImportKey)]; ok {
			if sameRekordboxPath(tr.Path, enginePath) {
				return tr, true
			}
		}
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
					if tr, ok := byID[decoded.Track.ID]; ok && sameRekordboxPath(tr.Path, enginePath) {
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

var (
	engineDefaultHotCueColor = [4]byte{255, 0, 255, 0}
	engineDefaultLoopColor   = [4]byte{255, 255, 127, 0}
)

func uncolouredCue(c rekordbox.Cue) bool {
	return c.Color == ([4]byte{}) && c.ColorCode == 0 && c.ColorID == 0
}

func preserveUncolouredColor(orig rekordbox.Cue, color [4]byte, startMS, endMS uint32, loop bool) bool {
	if !uncolouredCue(orig) || absDiffU32(orig.TimeMS, startMS) > 2 {
		return false
	}
	if loop && (!orig.Loop || absDiffU32(orig.EndMS, endMS) > 2) {
		return false
	}
	if !loop && orig.Loop {
		return false
	}
	if loop {
		return color == engineDefaultLoopColor
	}
	return color == engineDefaultHotCueColor
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

// pdbMetadataBytes is export.pdb at path with the metadata of updates (#314).
func pdbMetadataBytes(ctx context.Context, path string, updates map[uint32]rekordbox.Metadata) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return rekordbox.RewritePDBMetadata(raw, updates)
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
				if preserveUncolouredColor(oc, rgb, startMS, endMS, true) {
					rgb, code = [4]byte{}, 0
				} else if oc.Color == rgb && oc.ColorCode != 0 {
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
		if oc, ok := origHot[pad]; ok && !oc.Loop {
			if preserveUncolouredColor(oc, rgb, startMS, 0, false) {
				rgb, code = [4]byte{}, 0
			} else if oc.Color == rgb && oc.ColorCode != 0 {
				code = oc.ColorCode
			}
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
				if preserveUncolouredColor(oc, lp.Color, startMS, endMS, true) {
					colorID = 0
					memColor = [4]byte{}
					memColorCode = 0
				} else if oc.ColorID != 0 && oc.ColorID <= 8 {
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

// hasFirstBackup reports whether the first original of relSlash is kept under backupRoot (any date).
func hasFirstBackup(backupRoot, relSlash string) bool {
	entries, _ := os.ReadDir(backupRoot)
	for _, ent := range entries {
		if _, err := os.Stat(filepath.Join(backupRoot, ent.Name(), filepath.FromSlash(relSlash))); ent.IsDir() && err == nil {
			return true
		}
	}
	return false
}

// mkdirOrdered makes dir and its missing parents under root one at a time. On vfat an fsync writes
// a file's own entry before the FAT, so the FAT that gives a new directory its cluster goes first
// with the fsync of a file that has nothing to write (flush, when there is one), then the parent
// with the entry that names it (#321 review R4).
func mkdirOrdered(root, dir string, flush *os.File) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if p = filepath.Join(p, part); os.Mkdir(p, 0o755) == nil {
			if flush != nil {
				_ = flush.Sync()
			}
			_ = syncDir(filepath.Dir(p))
		} else if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return fmt.Errorf("writeback: cannot make the backup directory %s", p)
		}
	}
	return nil
}

// ensureFirstOriginalBackup copies the stick's original of relSlash once; it returns the bytes copied.
func ensureFirstOriginalBackup(mountRoot, backupRoot, dateDir, relSlash string) (int64, error) {
	relOS := filepath.FromSlash(relSlash)
	if hasFirstBackup(backupRoot, relSlash) {
		return 0, nil
	}
	src := filepath.Join(mountRoot, relOS)
	dst := filepath.Join(backupRoot, dateDir, relOS)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bak-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = removeFile(tmpPath)
		}
	}()
	n, err := io.Copy(tmp, in)
	if err != nil {
		return 0, err
	}
	// The directory first: its fsync writes the volume's allocation of the clusters this file was
	// just given (the exFAT bitmap, the FAT) before the file's own fsync writes the entry that names
	// them. A pull between the two leaves lost clusters, never an entry naming clusters the volume
	// does not give it, so the next insert can remove the file (#321 review R4b).
	_ = syncDir(filepath.Dir(tmpPath))
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	writeBackCut("backup temp " + dst)
	if err := replaceFile(tmpPath, dst); err != nil {
		return 0, err
	}
	writeBackCut("backup " + dst)
	ok = true
	return n, nil
}

// stageWriteFile writes data, synced, to the staged copy of dst beside it: a new file called name,
// or, without a name, one named as os.CreateTemp names them.
func stageWriteFile(dst, name string, data []byte) (string, error) {
	var tmp *os.File
	var err error
	if name == "" {
		tmp, err = os.CreateTemp(filepath.Dir(dst), ".enginex-wb-*.tmp")
	} else {
		tmp, err = os.OpenFile(filepath.Join(filepath.Dir(dst), name), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	}
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = removeFile(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", err
	}
	_ = syncDir(filepath.Dir(tmpPath)) // the allocation before the entry, as in ensureFirstOriginalBackup
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
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".enginex-wb-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = removeFile(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	_ = syncDir(filepath.Dir(tmpPath)) // the allocation before the entry, as in ensureFirstOriginalBackup
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	writeBackCut("temp " + dst)
	if beforeRename != nil {
		if err := beforeRename(dst); err != nil {
			return err
		}
	}
	if err := replaceFile(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// replaceFile renames tmp, written and synced, over dst so that a pull at any moment leaves dst
// whole, old or new (#321, #253 review 3). On FAT a rename frees the replaced file's clusters at
// once, but the entry that names the new ones reaches the stick only with the renamed inode's
// writeback (the unmount, or about 30 s later): a pull in between left dst naming freed clusters,
// and reading it gave EIO (measured by replaying every write of a pass on a FAT32 volume). The
// replaced file is held open until the directory (the temporary entry gone, dst still naming the
// old clusters) and then dst itself (naming the new ones) are on the stick, so its clusters are
// freed only after that.
//
// dst must own its clusters in the FAT: the pass checks every file it replaces, and its mark,
// before it allocates anything (unsafeToFree, #321 review R3); the other files replaced are new.
func replaceFile(tmp, dst string) error {
	held, _ := os.Open(dst)
	defer func() {
		if held != nil {
			_ = held.Close()
		}
	}()
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	writeBackCut("replacing " + dst)
	_ = syncDir(filepath.Dir(dst))
	_ = syncFile(dst)
	writeBackCut("renamed " + dst)
	return nil
}

// errUnownedClusters: a file whose clusters the FAT does not give it is not removed, and a pass
// that would replace one does not write the stick (unsafeToFree).
var errUnownedClusters = errors.New("bridge: the file's clusters are not its own in the FAT")

// removeFile removes path so that a pull at any moment leaves either the file or no entry for
// it (#321). On FAT an unlink frees the clusters and drops the directory entry in two writes, and
// a sync writes the FAT first: a pull in between left an entry naming free clusters, which reads
// EIO, and whose next removal or replacement is a file-system error (vfat then remounts the stick
// read-only by default). Held open, the file keeps its clusters until the directory is synced.
// A file that already names clusters it does not own is left (errUnownedClusters).
func removeFile(path string) error {
	if unsafeToFree(path) {
		return errUnownedClusters
	}
	held, _ := os.Open(path)
	err := os.Remove(path)
	if err == nil {
		_ = syncDir(filepath.Dir(path))
		writeBackCut("removed " + path)
	}
	if held != nil {
		_ = held.Close()
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
