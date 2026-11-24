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
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

const unix_ST_RDONLY = 0x1

var (
	errVolumeBusy = errors.New("writeback: volume busy")
	errConverting = errors.New("writeback: conversion in progress")
)

var usbPreparingFlagPath = "/run/enginex-usb-preparing"
var writeBackCut = func(string) {}

// WriteBackOptions configures a cue write-back pass.
type WriteBackOptions struct {
	DateDir            string
	SkipPreparingCheck bool
	OnEject            bool
	BeforeRename       func(dst string) error
	Session            *CueSession
	Keys               *rekordbox.KeyCache
	Settled            func() error
}

// WriteBackResult summarizes one pass.
type WriteBackResult struct {
	TracksChecked     int              `json:"tracksChecked"`
	TracksRead        int              `json:"tracksRead"`
	TracksChanged     int              `json:"tracksChanged"`
	FilesUpdated      int              `json:"filesUpdated"`
	OneLibraryUpdated bool             `json:"oneLibraryUpdated"`
	BackupDir         string           `json:"backupDir,omitempty"`
	PhasesMS          map[string]int64 `json:"phasesMs,omitempty"`
	BytesWritten      int64            `json:"bytesWritten,omitempty"`
}

type passTimes struct {
	candidates, key, decrypt, edit, encrypt, write, total time.Duration
	bytes                                                 int64
	skipReason                                            string
}

func (t *passTimes) ms() map[string]int64 {
	return map[string]int64{
		"candidates": t.candidates.Milliseconds(),
		"key":        t.key.Milliseconds(),
		"decrypt":    t.decrypt.Milliseconds(),
		"edit":       t.edit.Milliseconds(),
		"encrypt":    t.encrypt.Milliseconds(),
		"write":      t.write.Milliseconds(),
		"total":      t.total.Milliseconds(),
	}
}

// CueSession keeps rows read by a watcher between quiet passes.
type CueSession struct {
	mu   sync.Mutex
	rows map[string]syncedRow
}

type syncedRow struct {
	key    string
	id     int64
	edit   string
	digest []byte
}

func (s *CueSession) has(key, edit string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key]
	return ok && r.edit == edit
}

func (s *CueSession) add(rows []syncedRow) {
	if s == nil || len(rows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]syncedRow)
	}
	for _, r := range rows {
		s.rows[r.key] = r
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

// State is the import stamp used by the write-back gate.
type State struct {
	Header  rbstate.Header
	Current string
}

func (s State) Matches() bool { return s.Header.Stamp != "" && s.Header.Stamp == s.Current }

// ImportState reads only the snapshot header and current export file signatures.
func ImportState(mount string) (State, error) {
	var out State
	abs, err := filepath.Abs(mount)
	if err != nil {
		return out, err
	}
	dbDir := filepath.Join(abs, "Engine Library", "Database2")
	h, err := rbstate.ReadHeader(dbDir)
	if err != nil {
		return out, err
	}
	resolver, err := compat.NewResolver(abs)
	if err != nil {
		return out, err
	}
	sigs, err := FindSources(resolver, abs)
	if err != nil {
		return out, err
	}
	sources := append([]rbstate.FileSig(nil), sigs...)
	out.Header = h
	out.Current = rbstate.Stamp(h.Layout, sources, stagedStamp(abs))
	return out, nil
}

func snapshotStagedPart(stamp string) string {
	const marker = " | staged "
	if i := strings.Index(stamp, marker); i >= 0 {
		return stamp[i+len(marker):]
	}
	return ""
}

// NoteWriteBack refreshes the snapshot after files were written. The caller holds LockVolume.
func NoteWriteBack(mount string, written map[string][]rbstate.FileSig) error {
	abs, err := filepath.Abs(mount)
	if err != nil {
		return err
	}
	dbDir := filepath.Join(abs, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		return err
	}
	for key, sigs := range written {
		if t, ok := snap.Tracks[rbstate.FoldPath(key)]; ok {
			t.Built.ANLZ = append([]rbstate.FileSig(nil), sigs...)
		}
	}
	resolver, err := compat.NewResolver(abs)
	if err != nil {
		return err
	}
	sources, err := FindSources(resolver, abs)
	if err != nil {
		return err
	}
	snap.Header.Sources = sources
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, sources, stagedStamp(abs))
	snap.Header.Applied = time.Now().Unix()
	return snap.Save(dbDir)
}

type exportFile struct {
	Kind rbstate.LibraryKind
	Rel  string
	Full string
	Sig  rbstate.FileSig
}

func discoverExports(mount string) ([]exportFile, error) {
	r, err := compat.NewResolver(mount)
	if err != nil {
		return nil, err
	}
	var out []exportFile
	files, err := FindSources(r, mount)
	if err != nil {
		return nil, err
	}
	for _, sig := range files {
		rel := sig.Rel
		st, e := os.Stat(r.FullPath(rel))
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			return nil, e
		}
		out = append(out, exportFile{Kind: KindOf(rel), Rel: rel, Full: r.FullPath(rel), Sig: rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()}})
	}
	return out, nil
}

func mountRootWritable(mount string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(mount, &st) == nil && st.Flags&unix_ST_RDONLY == 0
}

// WriteBackCues writes changed deck cues, loops, and editable metadata to the ANLZ files and
// every source library named by the snapshot. It never uses pdbImportKey for identity.
func WriteBackCues(ctx context.Context, mountPath string, opts WriteBackOptions) (WriteBackResult, error) {
	var tm passTimes
	start := time.Now()
	res, err := writeBackCues(ctx, mountPath, opts, &tm)
	tm.total = time.Since(start)
	res.PhasesMS = tm.ms()
	res.BytesWritten = tm.bytes
	if tm.skipReason != "" {
		log.Printf("rb-bridge: cue write-back skipped on %s: %s", mountPath, tm.skipReason)
		return res, err
	}
	kind := "manual"
	if opts.OnEject {
		kind = "eject"
	} else if opts.Settled != nil {
		kind = "watcher"
	}
	outcome := "done"
	if err != nil {
		outcome = "stopped: " + err.Error()
	}
	log.Printf("rb-bridge: cue write-back of %s %s: %d tracks, %d read, %d changed; ms: candidates %d, key %d, decrypt %d, edit %d, encrypt %d, write %d, total %d; %d files, %d bytes written; pass: %s",
		mountPath, outcome, res.TracksChecked, res.TracksRead, res.TracksChanged,
		res.PhasesMS["candidates"], res.PhasesMS["key"], res.PhasesMS["decrypt"], res.PhasesMS["edit"], res.PhasesMS["encrypt"], res.PhasesMS["write"], res.PhasesMS["total"], res.FilesUpdated, res.BytesWritten, kind)
	return res, err
}

type dbCandidate struct {
	key, enginePath, edit string
	engineID              int64
	track                 *rbstate.Track
	beatData, trackData   []byte
	quickCues, loops      []byte
	activeLoops           int64
	metadata              rekordbox.Metadata
	writtenDigest         []byte
	syncedDigest          []byte
}

type trackChange struct {
	candidate dbCandidate
	refs      map[rbstate.LibraryKind]rekordbox.Track
	cues      []rekordbox.Cue
	analysis  string
	cuesDiff  bool
	metadata  map[rbstate.LibraryKind]bool
	seen      syncedRow
}

func writeBackCues(ctx context.Context, mountPath string, opts WriteBackOptions, tm *passTimes) (WriteBackResult, error) {
	var res WriteBackResult
	absMount, err := filepath.Abs(mountPath)
	if err != nil {
		return res, err
	}
	info, err := os.Stat(absMount)
	if err != nil || !info.IsDir() {
		return res, fmt.Errorf("writeback: mount path unavailable: %s", mountPath)
	}
	if !mountRootWritable(absMount) {
		return res, fmt.Errorf("writeback: stick is read-only: %s", absMount)
	}
	if !opts.SkipPreparingCheck && !opts.OnEject {
		if _, e := os.Stat(usbPreparingFlagPath); e == nil {
			tm.skipReason = "conversion is in progress"
			return res, errConverting
		}
	}

	importState, e := ImportState(absMount)
	if e != nil {
		tm.skipReason = "import state unavailable"
		return res, nil
	}
	if !importState.Matches() || !importState.Header.Pending.None() {
		if !importState.Matches() {
			tm.skipReason = "import stamp does not match the current export"
		} else {
			tm.skipReason = "import has pending work"
		}
		return res, nil
	}
	snapshot, err := rbstate.Load(filepath.Join(absMount, "Engine Library", "Database2"))
	if err != nil {
		tm.skipReason = "snapshot is unavailable"
		return res, nil
	}

	unlock, err := rbstate.LockVolume(absMount, 0)
	if err != nil {
		return res, fmt.Errorf("%w: %w", errVolumeBusy, err)
	}
	defer unlock()
	keys := opts.Keys
	if keys == nil {
		keys = rekordbox.NewKeyCache()
		defer keys.Clear()
	}
	keyStart := time.Now()
	defer func() { tm.key = time.Since(keyStart) }()

	engineDir := filepath.Join(absMount, "Engine Library", "Database2")
	mDBPath := filepath.Join(engineDir, "m.db")
	if _, err := os.Stat(mDBPath); err != nil {
		return res, fmt.Errorf("writeback: Engine library not found: %w", err)
	}
	resolver, err := compat.NewResolver(absMount)
	if err != nil {
		return res, err
	}

	dbURI := url.URL{Scheme: "file", Path: mDBPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	settle := func() error { return nil }
	if opts.Settled != nil {
		dbURI.RawQuery = "mode=ro&immutable=1"
		settle = opts.Settled
	}
	db, err := sql.Open("sqlite", dbURI.String())
	if err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	tmStart := time.Now()
	candidates, confirmed, err := readCandidates(ctx, db, snapshot, opts, filepath.Join(engineDir, rbstate.SyncedName))
	if err != nil {
		return res, err
	}
	res.TracksChecked = candidates.checked
	res.TracksRead = candidates.read
	tm.candidates = time.Since(tmStart)
	if len(candidates.rows) == 0 {
		if settle() != nil {
			return res, errNotQuiet
		}
		if err := saveSynced(engineDir, append(confirmed, candidates.confirmed...)); err != nil {
			return res, err
		}
		opts.Session.add(append(confirmed, candidates.confirmed...))
		return res, nil
	}

	libraries, err := readNeededLibraries(ctx, absMount, importState.Current, candidates.rows, keys, &tm.decrypt)
	if err != nil {
		return res, err
	}
	changes, matched, err := buildChanges(ctx, resolver, libraries, candidates.rows)
	if err != nil {
		return res, err
	}
	if len(changes) == 0 {
		if settle() != nil {
			return res, errNotQuiet
		}
		all := confirmedRows(confirmed, candidates.confirmed, candidates.rows, nil, matched)
		if err := saveSynced(engineDir, all); err != nil {
			return res, err
		}
		opts.Session.add(all)
		return res, nil
	}
	if err := settle(); err != nil {
		return res, err
	}

	dateDir := opts.DateDir
	if dateDir == "" {
		dateDir = time.Now().Format("2006-01-02")
	}
	backupRoot := filepath.Join(absMount, ".enginex-backup")
	plan, err := prepareWritePlan(absMount, resolver, changes, libraries, dateDir)
	if err != nil {
		return res, err
	}
	if err := markWriteBack(engineDir, dateDir, plan.dirs, plan.targets); err != nil {
		return res, err
	}
	finished := false
	defer func() {
		if finished {
			_ = rbstate.RemoveFile(filepath.Join(engineDir, writeBackMarkName))
		}
	}()

	staged, written, err := stageChanges(ctx, absMount, resolver, backupRoot, dateDir, changes, libraries, plan, keys, tm)
	if err != nil {
		return res, err
	}
	for _, item := range staged {
		if opts.BeforeRename != nil {
			if err := opts.BeforeRename(item.dst); err != nil {
				return res, err
			}
		}
	}
	writeStart := time.Now()
	for len(staged) > 0 {
		item := staged[0]
		if err := replaceFile(item.tmp, item.dst); err != nil {
			return res, err
		}
		staged = staged[1:]
		res.FilesUpdated++
		if item.exportKind == rbstate.OneLibrary {
			res.OneLibraryUpdated = true
		}
	}
	tm.write = time.Since(writeStart)
	if res.FilesUpdated > 0 {
		res.BackupDir = filepath.Join(".enginex-backup", dateDir)
	}
	for key, sigs := range written {
		for i := range sigs {
			if info, statErr := os.Stat(resolver.FullPath(sigs[i].Rel)); statErr == nil {
				sigs[i].Size = info.Size()
				sigs[i].MTimeNS = info.ModTime().UnixNano()
			}
		}
		written[key] = sigs
	}
	evictWriteBackExportCache(importState.Current)
	if err := NoteWriteBack(absMount, written); err != nil {
		return res, err
	}
	all := confirmedRows(confirmed, candidates.confirmed, candidates.rows, changes, matched)
	if err := saveSynced(engineDir, all); err != nil {
		return res, err
	}
	opts.Session.add(all)
	writeBackCut("state")
	finished = true
	res.TracksChanged = len(changes)
	return res, nil
}

type candidateSet struct {
	rows      []dbCandidate
	confirmed []syncedRow
	checked   int
	read      int
}

func confirmedRows(existing, session []syncedRow, rows []dbCandidate, changes []trackChange, matched map[string]bool) []syncedRow {
	all := append(append([]syncedRow{}, existing...), session...)
	changed := make(map[string]bool, len(changes))
	for _, ch := range changes {
		changed[ch.candidate.key] = true
	}
	for _, row := range rows {
		if matched[row.key] && !changed[row.key] {
			all = append(all, syncedRow{key: row.key, id: row.engineID, edit: row.edit, digest: row.syncedDigest})
		}
	}
	for _, ch := range changes {
		all = append(all, ch.seen)
	}
	return all
}

func readCandidates(ctx context.Context, db *sql.DB, snap *rbstate.Snapshot, opts WriteBackOptions, syncedPath string) (candidateSet, []syncedRow, error) {
	var out candidateSet
	state, _ := loadSynced(syncedPath)
	rows, err := db.QueryContext(ctx, `SELECT t.id, COALESCE(t.path,''), COALESCE(t.lastEditTime,0), COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,''),
		p.beatData, p.trackData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops,0)
		FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.path IS NOT NULL`)
	if err != nil {
		return out, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path, edit string
		var md rekordbox.Metadata
		var beatData, trackData, quickCues, loops []byte
		var active int64
		if err := rows.Scan(&id, &path, &edit, &md.Genre, &md.Comment, &md.Label, &beatData, &trackData, &quickCues, &loops, &active); err != nil {
			return out, nil, err
		}
		tr, ok := snap.TrackAt(path)
		if !ok || tr == nil || tr.Native {
			continue
		}
		out.checked++
		digest := enginedb.WrittenDigest(beatData, quickCues, loops)
		syncDigest := enginedb.SyncedDigest(beatData, quickCues, loops, active, md.Genre, md.Comment, md.Label)
		key := tr.Key
		sr, found := state.Rows[key]
		if !found && state.Rows != nil {
			sr, found = state.Rows[rbstate.FoldPath(path)]
		}
		if found && sr.Edit == edit && bytes.Equal(sr.Digest, syncDigest) || opts.Session.has(key, edit) {
			out.confirmed = append(out.confirmed, syncedRow{key: key, id: id, edit: edit, digest: syncDigest})
			continue
		}
		out.read++
		if len(tr.Written) > 0 && bytes.Equal(digest, tr.Written) && found && bytes.Equal(sr.Digest, syncDigest) {
			continue
		}
		out.rows = append(out.rows, dbCandidate{key: key, enginePath: path, edit: edit, engineID: id, track: tr, beatData: beatData, trackData: trackData, quickCues: quickCues, loops: loops, activeLoops: active, metadata: md, writtenDigest: digest, syncedDigest: syncDigest})
	}
	if err := rows.Err(); err != nil {
		return out, nil, err
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i].key < out.rows[j].key })
	return out, out.confirmed, nil
}

// syncedState is deliberately independent of m.db. A torn or missing file is treated as an
// empty baseline and is rebuilt after a complete pass.
type syncedState struct {
	Version int                          `json:"version"`
	Rows    map[string]syncedStateRecord `json:"rows"`
}

type syncedStateRecord struct {
	EngineID int64  `json:"id"`
	Edit     string `json:"edit"`
	Digest   []byte `json:"digest"`
}

func loadSynced(path string) (syncedState, error) {
	out := syncedState{Version: 1, Rows: map[string]syncedStateRecord{}}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	if json.Unmarshal(b, &out) != nil || out.Version != 1 || out.Rows == nil {
		return syncedState{Version: 1, Rows: map[string]syncedStateRecord{}}, nil
	}
	return out, nil
}

func saveSynced(dbDir string, rows []syncedRow) error {
	path := filepath.Join(dbDir, rbstate.SyncedName)
	state, _ := loadSynced(path)
	if state.Rows == nil {
		state.Rows = map[string]syncedStateRecord{}
	}
	for _, r := range rows {
		state.Rows[r.key] = syncedStateRecord{EngineID: r.id, Edit: r.edit, Digest: append([]byte(nil), r.digest...)}
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return rbstate.ReplaceFile(path, append(b, '\n'))
}

func readNeededLibraries(ctx context.Context, mount, stamp string, rows []dbCandidate, keys *rekordbox.KeyCache, decrypt *time.Duration) (map[rbstate.LibraryKind]sourceLibrary, error) {
	needed := map[rbstate.LibraryKind]bool{}
	for _, row := range rows {
		for _, ref := range row.track.Refs {
			needed[ref.Kind] = true
		}
	}
	if len(needed) == 0 {
		return map[rbstate.LibraryKind]sourceLibrary{}, nil
	}
	if cached, ok := getWriteBackExportCache(stamp); ok {
		return cached, nil
	}
	files, err := discoverExports(mount)
	if err != nil {
		return nil, err
	}
	out := map[rbstate.LibraryKind]sourceLibrary{}
	var errs []error
	for _, f := range files {
		if !needed[f.Kind] {
			continue
		}
		started := time.Now()
		var lib *rekordbox.Library
		if f.Kind == rbstate.DeviceLibrary {
			lib, err = rekordbox.ReadPDB(f.Full)
		} else {
			lib, err = rekordbox.ReadSQLiteKeys(ctx, f.Full, nil, keys)
		}
		*decrypt += time.Since(started)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Rel, err))
			continue
		}
		out[f.Kind] = sourceLibrary{Kind: f.Kind, Rel: f.Rel, Full: f.Full, Lib: lib}
	}
	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	putWriteBackExportCache(stamp, out)
	return out, nil
}

type sourceLibrary struct {
	Kind rbstate.LibraryKind
	Rel  string
	Full string
	Lib  *rekordbox.Library
}

var writeBackExportCache = struct {
	sync.Mutex
	entries map[string]map[rbstate.LibraryKind]sourceLibrary
	order   []string
}{entries: map[string]map[rbstate.LibraryKind]sourceLibrary{}}

func cloneLibrary(in *rekordbox.Library) *rekordbox.Library {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out rekordbox.Library
	if json.Unmarshal(b, &out) != nil {
		return in
	}
	return &out
}

func getWriteBackExportCache(stamp string) (map[rbstate.LibraryKind]sourceLibrary, bool) {
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	got, ok := writeBackExportCache.entries[stamp]
	if !ok {
		return nil, false
	}
	out := map[rbstate.LibraryKind]sourceLibrary{}
	for k, v := range got {
		v.Lib = cloneLibrary(v.Lib)
		out[k] = v
	}
	return out, true
}

func putWriteBackExportCache(stamp string, libs map[rbstate.LibraryKind]sourceLibrary) {
	if stamp == "" || len(libs) == 0 {
		return
	}
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	if _, ok := writeBackExportCache.entries[stamp]; !ok {
		writeBackExportCache.order = append(writeBackExportCache.order, stamp)
	}
	copyMap := map[rbstate.LibraryKind]sourceLibrary{}
	for k, v := range libs {
		v.Lib = cloneLibrary(v.Lib)
		copyMap[k] = v
	}
	writeBackExportCache.entries[stamp] = copyMap
	for len(writeBackExportCache.order) > 4 {
		old := writeBackExportCache.order[0]
		writeBackExportCache.order = writeBackExportCache.order[1:]
		delete(writeBackExportCache.entries, old)
	}
}

func evictWriteBackExportCache(stamp string) {
	writeBackExportCache.Lock()
	defer writeBackExportCache.Unlock()
	delete(writeBackExportCache.entries, stamp)
	for i, v := range writeBackExportCache.order {
		if v == stamp {
			writeBackExportCache.order = append(writeBackExportCache.order[:i], writeBackExportCache.order[i+1:]...)
			break
		}
	}
}

func buildChanges(ctx context.Context, resolver *compat.Resolver, libs map[rbstate.LibraryKind]sourceLibrary, rows []dbCandidate) ([]trackChange, map[string]bool, error) {
	var changes []trackChange
	matched := make(map[string]bool, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, matched, err
		}
		refs := map[rbstate.LibraryKind]rekordbox.Track{}
		metadata := map[rbstate.LibraryKind]bool{}
		var primary rekordbox.Track
		for _, ref := range row.track.Refs {
			src, ok := libs[ref.Kind]
			if !ok || src.Lib == nil {
				continue
			}
			for _, tr := range src.Lib.Tracks {
				if tr.ID != ref.ID || rbstate.FoldPath(tr.Path) != row.key || ref.RecordHash != "" && tr.RecordHash != ref.RecordHash {
					continue
				}
				refs[ref.Kind] = tr
				if primary.Path == "" {
					primary = tr
				}
				metadata[ref.Kind] = row.metadata != (rekordbox.Metadata{Genre: tr.Genre, Comment: tr.Comment, Label: tr.Label})
				break
			}
		}
		if len(refs) == 0 {
			continue
		}
		matched[row.key] = true
		change := trackChange{candidate: row, refs: refs, metadata: metadata, seen: syncedRow{key: row.key, id: row.engineID, edit: row.edit, digest: row.syncedDigest}}
		needsCue := len(row.track.Written) == 0 || !bytes.Equal(row.writtenDigest, row.track.Written)
		if needsCue && len(row.trackData) > 0 && primary.AnalysisPath != "" {
			deck, err := enginedb.ParseDeckCuesAndLoops(row.trackData, row.quickCues, row.loops, row.activeLoops)
			if err == nil {
				offset, ok := trackRekordboxOffset(resolver, primary.Path)
				if ok {
					orig := readANLZCues(resolver, primary.AnalysisPath)
					legacyMP3 := len(row.track.Written) == 0 && strings.HasSuffix(strings.ToLower(primary.Path), ".mp3")
					next := mergeEngineCuesOpts(orig, deck, offset, legacyMP3)
					change.cues = next
					change.cuesDiff = !cuesEquivalent(orig, next)
					for kind, sourceTrack := range refs {
						if kind == rbstate.OneLibrary && !cuesEquivalent(sourceTrack.Cues, next) {
							change.cuesDiff = true
						}
					}
					change.analysis = primary.AnalysisPath
				}
			}
		}
		if !change.cuesDiff {
			needsMeta := false
			for _, v := range metadata {
				needsMeta = needsMeta || v
			}
			if !needsMeta {
				continue
			}
		}
		changes = append(changes, change)
	}
	return changes, matched, nil
}

func trackRekordboxOffset(r *compat.Resolver, trackPath string) (float64, bool) {
	if trackPath == "" {
		return 0, false
	}
	rel, err := r.Resolve(trackPath)
	if err != nil {
		return 0, false
	}
	info, err := compat.AudioHeader(r.FullPath(rel))
	if err != nil {
		return 0, false
	}
	lower := strings.ToLower(rel)
	if (strings.HasSuffix(lower, ".mp3") || compat.IsMP4AudioPath(lower)) && info.SampleRate <= 0 {
		return 0, false
	}
	return float64(info.RekordboxOffset()), true
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

func readStickTrackCues(r *compat.Resolver, tr rekordbox.Track) []rekordbox.Cue {
	fromANLZ := readANLZCues(r, tr.AnalysisPath)
	if len(tr.Cues) == 0 {
		return fromANLZ
	}
	merged := &rekordbox.Analysis{Cues: fromANLZ}
	merged.Merge(&rekordbox.Analysis{Cues: tr.Cues})
	return merged.Cues
}

func readANLZCues(r *compat.Resolver, analysisPath string) []rekordbox.Cue {
	clean, err := safeANLZRelPath(analysisPath)
	if err != nil {
		return nil
	}
	base := strings.TrimSuffix(clean, filepath.Ext(clean))
	var merged *rekordbox.Analysis
	if rel, ok := resolveANLZVariant(r, base, ".DAT"); ok {
		if b, err := os.ReadFile(r.FullPath(rel)); err == nil {
			if a, err := rekordbox.ParseAnalysis(b); err == nil {
				merged = a
			}
		}
	}
	if rel, ok := resolveANLZVariant(r, base, ".EXT"); ok {
		if b, err := os.ReadFile(r.FullPath(rel)); err == nil {
			if a, err := rekordbox.ParseAnalysis(b); err == nil {
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

func normalizeRekordboxPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimLeft(p, "/")
	for strings.HasPrefix(p, "../") {
		p = strings.TrimPrefix(p, "../")
	}
	return norm.NFC.String(filepath.ToSlash(filepath.Clean(p)))
}

func sameRekordboxPath(a, b string) bool {
	a, b = normalizeRekordboxPath(a), normalizeRekordboxPath(b)
	return a != "." && a != "" && cases.Fold().String(a) == cases.Fold().String(b)
}

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

func ensureFirstOriginalBackup(mountRoot, backupRoot, dateDir, relSlash string) (int64, error) {
	if hasFirstBackup(backupRoot, relSlash) {
		return 0, nil
	}
	src := filepath.Join(mountRoot, filepath.FromSlash(relSlash))
	dst := filepath.Join(backupRoot, dateDir, filepath.FromSlash(relSlash))
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
			_ = rbstate.RemoveFile(tmpPath)
		}
	}()
	n, err := io.Copy(tmp, in)
	if err != nil {
		return 0, err
	}
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

func hasFirstBackup(root, rel string) bool {
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), filepath.FromSlash(rel))); err == nil {
			return true
		}
	}
	return false
}

func cuesEquivalent(a, b []rekordbox.Cue) bool {
	na, nb := normalizeCues(a), normalizeCues(b)
	if len(na) != len(nb) {
		return false
	}
	for i := range na {
		x, y := na[i], nb[i]
		if x.HotCue != y.HotCue || x.Loop != y.Loop || x.Name != y.Name || x.Active != y.Active || absDiffU32(x.TimeMS, y.TimeMS) > 2 || x.Loop && absDiffU32(x.EndMS, y.EndMS) > 2 {
			return false
		}
		if x.HotCue == 0 {
			if x.ColorID != y.ColorID || x.Color != y.Color && x.ColorCode != y.ColorCode {
				return false
			}
		} else if x.Color != y.Color && x.ColorCode != y.ColorCode {
			return false
		}
	}
	return true
}

func normalizeCues(in []rekordbox.Cue) []rekordbox.Cue {
	out := append([]rekordbox.Cue(nil), in...)
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

func mergeEngineCues(orig []rekordbox.Cue, deck enginedb.DeckCues, offset float64, legacy bool) []rekordbox.Cue {
	origHot := map[uint32]rekordbox.Cue{}
	var memPoints, memLoops []rekordbox.Cue
	for _, c := range orig {
		if c.HotCue > 0 {
			origHot[c.HotCue] = c
		} else if c.Loop {
			memLoops = append(memLoops, c)
		} else {
			memPoints = append(memPoints, c)
		}
	}
	toMS := func(samples float64) uint32 {
		if samples < 0 {
			samples = 0
		}
		v := math.Round((samples + offset) * 1000 / deck.SampleRate)
		if v < 0 {
			return 0
		}
		if v > math.MaxUint32 {
			return math.MaxUint32
		}
		return uint32(v)
	}
	var out []rekordbox.Cue
	var consumed [8]bool
	for s := 0; s < 8; s++ {
		hc := deck.HotCues[s]
		if hc == nil {
			continue
		}
		pad := uint32(s + 1)
		start := toMS(hc.Samples)
		lp := deck.Loops[s]
		if lp != nil && absDiffU32(start, toMS(lp.StartSamples)) <= 2 {
			consumed[s] = true
			end := toMS(lp.EndSamples)
			if end <= start {
				end = start + 1
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
			var num, den uint16
			if oc, ok := origHot[pad]; ok && oc.Loop && absDiffU32(oc.TimeMS, start) <= 2 && absDiffU32(oc.EndMS, end) <= 2 {
				num, den = oc.LoopNumerator, oc.LoopDenominator
				if oc.Color == rgb && oc.ColorCode != 0 {
					code = oc.ColorCode
				}
			}
			out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, EndMS: end, Loop: true, Active: lp.Active, Name: name, Color: rgb, ColorCode: code, LoopNumerator: num, LoopDenominator: den, Extended: true})
			continue
		}
		rgb := hc.Color
		code := rekordbox.NearestCueColorCode(rgb)
		if oc, ok := origHot[pad]; ok && !oc.Loop {
			if hc.Samples <= 0.5 && float64(oc.TimeMS)*deck.SampleRate/1000 <= offset+0.5 {
				start = oc.TimeMS
			} else if legacy && hc.Name == "" && oc.Name == "" && absDiffU32(oc.TimeMS, start) <= 60 {
				start = oc.TimeMS
			}
			if oc.Color == rgb && oc.ColorCode != 0 {
				code = oc.ColorCode
			}
		}
		out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, Name: hc.Name, Color: rgb, ColorCode: code, Extended: true})
	}
	for s := 0; s < 8; s++ {
		if consumed[s] || deck.Loops[s] == nil {
			continue
		}
		lp := deck.Loops[s]
		start, end := toMS(lp.StartSamples), toMS(lp.EndSamples)
		if end <= start {
			continue
		}
		id := rekordbox.NearestMemoryColorID(lp.Color)
		var num, den uint16
		for _, oc := range memLoops {
			if absDiffU32(oc.TimeMS, start) <= 2 && absDiffU32(oc.EndMS, end) <= 2 {
				if oc.ColorID != 0 {
					id = oc.ColorID
				}
				num, den = oc.LoopNumerator, oc.LoopDenominator
				break
			}
		}
		out = append(out, rekordbox.Cue{TimeMS: start, EndMS: end, Loop: true, Active: lp.Active, Name: lp.Name, ColorID: id, LoopNumerator: num, LoopDenominator: den, Extended: true})
	}
	out = append(out, memPoints...)
	return out
}

// Keep the old helper name used by test adapters and callers.
func mergeEngineCuesOpts(orig []rekordbox.Cue, deck enginedb.DeckCues, offset float64, legacy bool) []rekordbox.Cue {
	return mergeEngineCues(orig, deck, offset, legacy)
}
