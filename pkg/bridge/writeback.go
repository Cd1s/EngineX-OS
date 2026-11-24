package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
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

// noted is a row as a pass found it in sync with the stick's export: its lastEditTime and
// SyncedDigest (rbstate.Track.Edit and Synced).
type noted struct {
	edit   int64
	digest []byte
}

// CueSession keeps, for a watcher, the rows its passes read and found in sync without reading
// the export. They reach the snapshot with the next pass that saves it.
type CueSession struct {
	mu   sync.Mutex
	rows map[string]noted
}

func (s *CueSession) has(key string, edit int64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key]
	return ok && r.edit == edit
}

// add keeps rows; take returns everything kept, with rows over it, and forgets it.
func (s *CueSession) add(rows map[string]noted) {
	if s == nil || len(rows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]noted{}
	}
	maps.Copy(s.rows, rows)
}

func (s *CueSession) take(rows map[string]noted) map[string]noted {
	if s == nil {
		return rows
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.rows
	if out == nil {
		out = map[string]noted{}
	}
	maps.Copy(out, rows)
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
	// The job has compared every track's files with what was built for this stamp: the import
	// is verified although no window has run since to say so in the snapshot (a re-export that
	// changed no track file stages nothing, so no remount follows it).
	if h.Pending.Verify && out.Matches() {
		if stage, err := rbstate.OpenStage(h.LibraryUUID); err == nil {
			if stage.Verified() == h.Stamp {
				out.Header.Pending.Verify = false
			}
			stage.Close()
		}
	}
	return out, nil
}

// NoteWriteBack refreshes the snapshot after a pass that read the export: the ANLZ files it
// wrote, the rows it found or brought in sync, and the export files as they are now, so the
// next insert does not take the pass's own writes for a re-export. The caller holds LockVolume.
func NoteWriteBack(mount string, written map[string][]rbstate.FileSig, rows map[string]noted) error {
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
	for key, n := range rows {
		if t, ok := snap.Tracks[key]; ok {
			t.Edit, t.Synced = n.edit, n.digest
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
	key, enginePath     string
	engineID            int64
	track               *rbstate.Track
	beatData, trackData []byte
	quickCues, loops    []byte
	activeLoops         int64
	metadata            rekordbox.Metadata
	writtenDigest       []byte
	now                 noted // the row as this pass read it
}

type trackChange struct {
	candidate dbCandidate
	refs      map[rbstate.LibraryKind]rekordbox.Track
	cues      []rekordbox.Cue
	analysis  string
	cuesDiff  bool
	metadata  map[rbstate.LibraryKind]bool
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
		keys = newKeyCache()
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
	candidates, err := readCandidates(ctx, db, snapshot, opts.Session)
	if err != nil {
		return res, err
	}
	res.TracksChecked = candidates.checked
	res.TracksRead = candidates.read
	tm.candidates = time.Since(tmStart)
	insync := candidates.insync
	if len(candidates.rows) == 0 {
		// Nothing to compare with the export: no file of the stick is read or written.
		if settle() != nil {
			return res, errNotQuiet
		}
		opts.Session.add(insync)
		return res, nil
	}

	libraries, err := readNeededLibraries(ctx, absMount, candidates.rows, keys, &tm.decrypt)
	if err != nil {
		return res, err
	}
	changes, matched, err := buildChanges(ctx, resolver, libraries, candidates.rows)
	if err != nil {
		return res, err
	}
	if err := settle(); err != nil {
		return res, err
	}
	// A row compared with every library that holds it is in sync once this pass is done.
	for _, row := range candidates.rows {
		if matched[row.key] {
			insync[row.key] = row.now
		}
	}
	if len(changes) == 0 {
		return res, NoteWriteBack(absMount, nil, opts.Session.take(insync))
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
	if err := NoteWriteBack(absMount, written, opts.Session.take(insync)); err != nil {
		return res, err
	}
	writeBackCut("state")
	finished = true
	res.TracksChanged = len(changes)
	return res, nil
}

type candidateSet struct {
	rows    []dbCandidate    // differ from what the stick is known to hold: compared with the export
	insync  map[string]noted // read, and still what the stick holds
	checked int
	read    int
}

// readCandidates finds the rows a pass has to compare with the export. A row whose
// lastEditTime is the one the snapshot noted with its digest is not read at all (#321: Engine's triggers date
// every write of a row's cues, loops and tags, so the scan reads Track and never the
// PerformanceData pages of an untouched track). A row that was written but whose cues, loops
// and tags are the ones noted (an analysis, a grid, a rating) is read and needs no export.
func readCandidates(ctx context.Context, db *sql.DB, snap *rbstate.Snapshot, session *CueSession) (candidateSet, error) {
	out := candidateSet{insync: map[string]noted{}}
	type moved struct {
		id   int64
		path string
		edit int64
		t    *rbstate.Track
	}
	var reads []moved
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,''), COALESCE(lastEditTime,0) FROM Track WHERE path IS NOT NULL`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m moved
		if err = rows.Scan(&m.id, &m.path, &m.edit); err != nil {
			rows.Close()
			return out, err
		}
		tr, ok := snap.TrackAt(m.path)
		if !ok || tr.Native || tr.EngineID != m.id {
			continue
		}
		out.checked++
		if (m.edit != tr.Edit || tr.Synced == nil) && !session.has(tr.Key, m.edit) {
			m.t = tr
			reads = append(reads, m)
		}
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	for _, m := range reads {
		c := dbCandidate{key: m.t.Key, enginePath: m.path, engineID: m.id, track: m.t}
		err = db.QueryRowContext(ctx, `SELECT COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,''), p.beatData, p.trackData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops,0)
			FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.id=?`, m.id).Scan(&c.metadata.Genre, &c.metadata.Comment, &c.metadata.Label, &c.beatData, &c.trackData, &c.quickCues, &c.loops, &c.activeLoops)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		out.read++
		c.now = noted{m.edit, enginedb.SyncedDigest(nil, c.quickCues, c.loops, c.activeLoops, c.metadata.Genre, c.metadata.Comment, c.metadata.Label)}
		if m.t.Synced != nil && bytes.Equal(m.t.Synced, c.now.digest) {
			out.insync[c.key] = c.now
			continue
		}
		c.writtenDigest = enginedb.WrittenDigest(c.beatData, c.quickCues, c.loops)
		out.rows = append(out.rows, c)
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i].key < out.rows[j].key })
	return out, nil
}

func readNeededLibraries(ctx context.Context, mount string, rows []dbCandidate, keys *rekordbox.KeyCache, decrypt *time.Duration) (map[rbstate.LibraryKind]SourceLibrary, error) {
	needed := map[rbstate.LibraryKind]bool{}
	for _, row := range rows {
		for _, ref := range row.track.Refs {
			needed[ref.Kind] = true
		}
	}
	if len(needed) == 0 {
		return map[rbstate.LibraryKind]SourceLibrary{}, nil
	}
	resolver, err := compat.NewResolver(mount)
	if err != nil {
		return nil, err
	}
	files, err := FindSources(resolver, mount)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	read, failed, err := ReadSources(ctx, resolver, mount, files, keys)
	*decrypt += time.Since(started)
	if err != nil {
		return nil, err
	}
	_ = failed
	out := map[rbstate.LibraryKind]SourceLibrary{}
	for _, lib := range read {
		if needed[lib.Kind] {
			out[lib.Kind] = lib
		}
	}
	return out, nil
}

func buildChanges(ctx context.Context, resolver *compat.Resolver, libs map[rbstate.LibraryKind]SourceLibrary, rows []dbCandidate) ([]trackChange, map[string]bool, error) {
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
		change := trackChange{candidate: row, refs: refs, metadata: metadata}
		// A candidate already differs in the digest of the deck-editable fields. Parse its
		// performance data whenever it has track data so a deletion or active-loop change can
		// be compared with the ANLZ even when the imported baseline is unchanged.
		// Rows that still hold the import's written digest need no ANLZ read. A missing
		// baseline, a changed written digest, or a previously noted row that differs now
		// covers cue deletion and active-on-load deactivation (port3 regressions).
		needsCue := len(row.trackData) > 0 && (len(row.track.Written) == 0 ||
			!bytes.Equal(row.writtenDigest, row.track.Written) || row.track.Synced != nil)
		if needsCue && len(row.trackData) > 0 && primary.AnalysisPath != "" {
			deck, err := enginedb.ParseDeckCuesAndLoops(row.trackData, row.quickCues, row.loops, row.activeLoops)
			if err == nil {
				offset, ok := trackRekordboxOffset(resolver, primary.Path)
				if ok {
					orig := readANLZCues(resolver, primary.AnalysisPath)
					legacyMP3 := len(row.track.Written) == 0 && strings.HasSuffix(strings.ToLower(primary.Path), ".mp3")
					next := mergeEngineCues(orig, deck, offset, legacyMP3)
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
			if oc, ok := origHot[pad]; ok && oc.Loop {
				if hc.Samples <= 0.5 && float64(oc.TimeMS)*deck.SampleRate/1000 <= offset+0.5 && absDiffU32(oc.EndMS, end) <= 2 && oc.TimeMS < end {
					start = oc.TimeMS
				}
				if absDiffU32(oc.TimeMS, start) <= 2 && absDiffU32(oc.EndMS, end) <= 2 {
					num, den = oc.LoopNumerator, oc.LoopDenominator
					if preserveUncolouredColor(oc, rgb, start, end, true) {
						rgb, code = [4]byte{}, 0
					} else if oc.Color == rgb && oc.ColorCode != 0 {
						code = oc.ColorCode
					}
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
			if preserveUncolouredColor(oc, rgb, start, 0, false) {
				rgb, code = [4]byte{}, 0
			} else if oc.Color == rgb && oc.ColorCode != 0 {
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
			if lp.StartSamples <= 0.5 && float64(oc.TimeMS)*deck.SampleRate/1000 <= offset+0.5 && absDiffU32(oc.EndMS, end) <= 2 && oc.TimeMS < end {
				start = oc.TimeMS
			}
			if absDiffU32(oc.TimeMS, start) <= 2 && absDiffU32(oc.EndMS, end) <= 2 {
				if preserveUncolouredColor(oc, lp.Color, start, end, true) {
					id = 0
				} else if oc.ColorID != 0 {
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
