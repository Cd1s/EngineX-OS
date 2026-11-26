package bridge

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

var (
	errVolumeBusy        = errors.New("writeback: volume busy")
	errConverting        = errors.New("writeback: conversion in progress")
	usbPreparingFlagMu   sync.RWMutex
	usbPreparingFlagPath = "/run/enginex-usb-preparing"
	writeBackCut         = func(string) {} // tests: a pull between two writes
)

func getUSBPreparingFlagPath() string {
	usbPreparingFlagMu.RLock()
	defer usbPreparingFlagMu.RUnlock()
	return usbPreparingFlagPath
}

func setUSBPreparingFlagPath(p string) string {
	usbPreparingFlagMu.Lock()
	defer usbPreparingFlagMu.Unlock()
	old := usbPreparingFlagPath
	usbPreparingFlagPath = p
	return old
}

// WriteBackOptions configures a cue write-back pass.
type WriteBackOptions struct {
	DateDir            string // the backup folder; today by default
	SkipPreparingCheck bool
	OnEject            bool
	BeforeRename       func(dst string) error // tests: a pull after staging
	Session            *CueSession
	Settled            func() error // the watcher's: an error when m.db changed while it was read
	DryRun             bool         // report would-be changes without locking or modifying the stick
}

// WriteBackTrackDiff describes why a track counts as a deck edit during write-back.
type WriteBackTrackDiff struct {
	EngineID int64    `json:"engineId"`
	Key      string   `json:"key"`
	Rel      string   `json:"rel"`
	Fields   []string `json:"fields"`
}

// WriteBackResult summarizes one pass.
type WriteBackResult struct {
	TracksChecked     int                  `json:"tracksChecked"`
	TracksRead        int                  `json:"tracksRead"`
	TracksChanged     int                  `json:"tracksChanged"`
	Changes           []WriteBackTrackDiff `json:"changes,omitempty"`
	FilesUpdated      int                  `json:"filesUpdated"`
	OneLibraryUpdated bool                 `json:"oneLibraryUpdated"`
	BackupDir         string               `json:"backupDir,omitempty"`
	PhasesMS          map[string]int64     `json:"phasesMs,omitempty"`
	BytesWritten      int64                `json:"bytesWritten,omitempty"`
}

var phaseNames = []string{"candidates", "key", "decrypt", "edit", "encrypt", "write", "total"}

// noted is a row as a pass found it in sync with the export: rbstate.Track.Edit, Synced, and
// (when known/synced) Written.
type noted struct {
	edit    int64
	digest  []byte
	written []byte
}

type recordHashes map[string]map[rbstate.LibraryKind]string

func applyRecordHashes(snap *rbstate.Snapshot, hashes recordHashes) {
	for key, byKind := range hashes {
		if t, ok := snap.Tracks[key]; ok {
			for i := range t.Refs {
				if hash, ok := byKind[t.Refs[i].Kind]; ok {
					t.Refs[i].RecordHash = hash
				}
			}
		}
	}
}

// CueSession keeps, for a watcher, the rows its passes found in sync without reading the export
// (they reach the snapshot with the next pass that saves it), and the stick's OneLibrary keys.
type CueSession struct {
	mu   sync.Mutex
	rows map[string]noted
	keys *rekordbox.KeyCache
}

func (s *CueSession) has(key string, edit int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key]
	return ok && r.edit == edit
}

func (s *CueSession) add(rows map[string]noted) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]noted{}
	}
	maps.Copy(s.rows, rows)
}

// take returns what the session kept, with rows over it, and forgets it.
func (s *CueSession) take(rows map[string]noted) map[string]noted {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.rows
	s.rows = nil
	if out == nil {
		return rows
	}
	maps.Copy(out, rows)
	return out
}

// State is the import stamp used by the write-back gate.
type State struct {
	Header  rbstate.Header
	Current string
}

func (s State) Matches() bool { return s.Header.Stamp != "" && s.Header.Stamp == s.Current }

// exportNow is the export files on the stick and their stamp.
func exportNow(abs, layout string) ([]rbstate.FileSig, string, error) {
	resolver, err := compat.NewResolver(abs)
	if err != nil {
		return nil, "", err
	}
	sources, err := FindSources(resolver, abs)
	if err != nil {
		return nil, "", err
	}
	return sources, rbstate.Stamp(layout, sources, ""), nil
}

// ImportState reads only the snapshot header and current export file signatures.
func ImportState(mount string) (State, error) {
	var out State
	abs, err := filepath.Abs(mount)
	if err != nil {
		return out, err
	}
	if out.Header, err = rbstate.ReadHeader(filepath.Join(abs, "Engine Library", "Database2")); err != nil {
		return out, err
	}
	if _, out.Current, err = exportNow(abs, out.Header.Layout); err != nil {
		return out, err
	}
	// The job has compared every track's files with what was built for this stamp: verified,
	// although no window has run since (a re-export that changed no track file stages nothing).
	if h := out.Header; h.Pending.Verify && out.Matches() {
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
func NoteWriteBack(mount string, written map[string][]rbstate.FileSig, rows map[string]noted, hashes recordHashes) error {
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
			t.Built.ANLZ = sigs
		}
	}
	for key, n := range rows {
		if t, ok := snap.Tracks[key]; ok {
			t.Edit, t.Synced = n.edit, n.digest
			if len(n.written) > 0 {
				t.Written = n.written
			}
		}
	}
	applyRecordHashes(snap, hashes)
	if snap.Header.Sources, snap.Header.Stamp, err = exportNow(abs, snap.Header.Layout); err != nil {
		return err
	}
	snap.Header.Applied = time.Now().Unix()
	return snap.Save(dbDir)
}

// WriteBackCues writes changed deck cues, loops, and editable metadata to the ANLZ files and
// every source library named by the snapshot (design 1.6). It never uses pdbImportKey.
func WriteBackCues(ctx context.Context, mountPath string, opts WriteBackOptions) (WriteBackResult, error) {
	start := time.Now()
	p := &pass{opts: opts, phase: map[string]time.Duration{}}
	if p.opts.Session == nil {
		p.opts.Session = &CueSession{}
	}
	skip, err := p.run(ctx, mountPath)
	res := p.res
	p.phase["total"] = time.Since(start)
	res.PhasesMS = map[string]int64{}
	for _, n := range phaseNames {
		res.PhasesMS[n] = p.phase[n].Milliseconds()
	}
	if skip != "" {
		log.Printf("rb-bridge: cue write-back skipped on %s: %s", mountPath, skip)
		return res, err
	}
	kind, outcome := "manual", "done"
	if opts.DryRun {
		kind = "dry-run"
	} else if opts.OnEject {
		kind = "eject"
	} else if opts.Settled != nil {
		kind = "watcher"
	}
	if err != nil {
		outcome = "stopped: " + err.Error()
	}
	ms := res.PhasesMS
	log.Printf("rb-bridge: cue write-back of %s %s: %d tracks, %d read, %d changed; ms: candidates %d, key %d, decrypt %d, edit %d, encrypt %d, write %d, total %d; %d files, %d bytes written; pass: %s",
		mountPath, outcome, res.TracksChecked, res.TracksRead, res.TracksChanged,
		ms["candidates"], ms["key"], ms["decrypt"], ms["edit"], ms["encrypt"], ms["write"], ms["total"], res.FilesUpdated, res.BytesWritten, kind)
	for i, ch := range res.Changes {
		if i >= 20 {
			log.Printf("rb-bridge: cue write-back %s: ... (%d more changed tracks)", mountPath, len(res.Changes)-20)
			break
		}
		log.Printf("rb-bridge: cue write-back %s changed track %d (%s): %s", mountPath, ch.EngineID, ch.Rel, strings.Join(ch.Fields, ", "))
	}
	return res, err
}

// pass is one write-back pass on a stick.
type pass struct {
	opts  WriteBackOptions
	mount string // absolute
	r     *compat.Resolver
	keys  *rekordbox.KeyCache
	res   WriteBackResult
	phase map[string]time.Duration
}

// dbCandidate is an Engine row a pass compares with the export.
type dbCandidate struct {
	key                 string
	engineID            int64
	track               *rbstate.Track
	beatData, trackData []byte
	quickCues, loops    []byte
	activeLoops         int64
	metadata            rekordbox.Metadata
	writtenDigest       []byte
	rawWrittenDigest    []byte
	filledWritten       []byte
	filledWrittenNoMain []byte
	filledBeats         []byte
	filledCues          []byte
	filledLoops         []byte
	now                 noted // the row as this pass read it
}

type trackChange struct {
	candidate     dbCandidate
	refs          map[rbstate.LibraryKind]rekordbox.Track
	cues          []rekordbox.Cue
	beats         []rekordbox.Beat
	bpm           float64
	analysis      string // set when cuesDiff, anlzCueRepair, or gridDiff
	cuesDiff      bool
	anlzCueRepair bool // ANLZ cue-section layout repair only (does not rewrite OneLibrary cue rows)
	gridDiff      bool
	metadata      map[rbstate.LibraryKind]bool
	diff          WriteBackTrackDiff
}

// run is the pass; a non-empty skip is why the gate refused it.
func (p *pass) run(ctx context.Context, mountPath string) (skip string, err error) {
	if p.mount, err = filepath.Abs(mountPath); err != nil {
		return "", err
	}
	if st, err := os.Stat(p.mount); err != nil || !st.IsDir() {
		return "", fmt.Errorf("writeback: mount path unavailable: %s", mountPath)
	}
	if !p.opts.DryRun {
		if fs := (syscall.Statfs_t{}); syscall.Statfs(p.mount, &fs) != nil || fs.Flags&0x1 != 0 { // ST_RDONLY
			return "", fmt.Errorf("writeback: stick is read-only: %s", p.mount)
		}
	}
	if !p.opts.SkipPreparingCheck && !p.opts.OnEject {
		if _, err := os.Stat(getUSBPreparingFlagPath()); err == nil {
			return "conversion is in progress", errConverting
		}
	}
	// Design 1.6: no write unless the snapshot is the export's and nothing is pending.
	switch st, err := ImportState(p.mount); {
	case err != nil:
		return "import state unavailable", nil
	case !st.Matches():
		return "import stamp does not match the current export", nil
	case !st.Header.Pending.None():
		return "import has pending work", nil
	}
	dbDir := filepath.Join(p.mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		return "snapshot is unavailable", nil
	}

	if !p.opts.DryRun {
		unlock, err := rbstate.LockVolume(p.mount, 0)
		if err != nil {
			return "", fmt.Errorf("%w: %w", errVolumeBusy, err)
		}
		defer unlock()
	}
	if p.keys = p.opts.Session.keys; p.keys == nil {
		p.keys = newKeyCache()
		defer p.keys.Clear()
	}
	locked := time.Now()
	defer func() { p.phase["key"] = time.Since(locked) }()

	mDB := filepath.Join(dbDir, "m.db")
	if _, err := os.Stat(mDB); err != nil {
		return "", fmt.Errorf("writeback: Engine library not found: %w", err)
	}
	if p.r, err = compat.NewResolver(p.mount); err != nil {
		return "", err
	}
	query, settle := "mode=ro&_pragma=busy_timeout(5000)", func() error { return nil }
	if p.opts.Settled != nil {
		// The watcher reads m.db as it is after a quiet period, and drops what it read when
		// Engine wrote meanwhile.
		query, settle = "mode=ro&immutable=1", p.opts.Settled
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mDB, RawQuery: query}).String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, _ = db.ExecContext(ctx, "PRAGMA cache_size=-16384")

	started := time.Now()
	cs, err := readCandidates(ctx, db, snap, p.opts.Session)
	if err != nil {
		return "", err
	}
	p.res.TracksChecked, p.res.TracksRead = cs.checked, cs.read
	p.phase["candidates"] = time.Since(started)
	var libs map[rbstate.LibraryKind]SourceLibrary
	var changes []trackChange
	var matched map[string]bool
	if len(cs.rows) > 0 {
		libs, err = p.readLibraries(ctx, cs.rows)
		if err != nil {
			return "", err
		}
		changes, matched, err = buildChanges(ctx, p.r, libs, cs.rows)
		if err != nil {
			return "", err
		}
	}
	changes = applyRepairChanges(p.r, p.mount, snap, changes)
	if len(cs.rows) == 0 && len(changes) == 0 {
		// Nothing to compare with the export and no EngineX-written ANLZ files need repair:
		// no file of the stick is read or written.
		if settle() != nil {
			return "", errNotQuiet
		}
		if !p.opts.DryRun {
			p.opts.Session.add(cs.insync)
		}
		return "", nil
	}
	if err := settle(); err != nil {
		return "", err
	}
	// A row compared with every library that holds it is in sync once this pass is done.
	for _, row := range cs.rows {
		if matched[row.key] {
			n := row.now
			if len(row.writtenDigest) > 0 {
				n.written = row.writtenDigest
			}
			cs.insync[row.key] = n
		}
	}
	if len(changes) > 0 {
		p.res.TracksChanged = len(changes)
		p.res.Changes = make([]WriteBackTrackDiff, 0, len(changes))
		for _, ch := range changes {
			p.res.Changes = append(p.res.Changes, ch.diff)
		}
	}
	if p.opts.DryRun {
		return "", nil
	}
	if len(changes) == 0 {
		return "", NoteWriteBack(p.mount, nil, p.opts.Session.take(cs.insync), nil)
	}
	written, hashes, err := p.write(ctx, libs, changes)
	if err != nil {
		return "", err
	}
	if err := NoteWriteBack(p.mount, written, p.opts.Session.take(cs.insync), hashes); err != nil {
		return "", err
	}
	writeBackCut("state")
	_ = rbstate.RemoveFile(filepath.Join(dbDir, writeBackMarkName))
	return "", nil
}

type candidateSet struct {
	rows    []dbCandidate    // differ from what the stick is known to hold: compared with the export
	insync  map[string]noted // read, and still what the stick holds
	checked int
	read    int
}

type filledStageRow struct {
	id            int64
	written       []byte
	writtenNoMain []byte
	beats         []byte
	cues          []byte
	loops         []byte
}

func validStageUUID(uuid string) bool {
	return uuid != "" && uuid != "." && uuid != ".." && !strings.ContainsAny(uuid, "/\\\x00")
}

func readFilledStage(snap *rbstate.Snapshot) map[string]filledStageRow {
	out := map[string]filledStageRow{}
	root, err := rbstate.StageRoot()
	if err != nil || snap == nil || !validStageUUID(snap.LibraryUUID) {
		return out
	}
	b, err := os.ReadFile(filepath.Join(root, snap.LibraryUUID, "filled.jsonl"))
	if err != nil {
		return out
	}
	var stage *rbstate.Stage
	var entries map[string]*rbstate.Staged
	if s, err := rbstate.OpenStage(snap.LibraryUUID); err == nil {
		stage = s
		defer stage.Close()
		entries, _, _ = stage.Entries()
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var f struct {
			ID   int64  `json:"id"`
			K    string `json:"k"`
			WR   []byte `json:"wr"`
			WRNM []byte `json:"wrnm"`
			V    int    `json:"v"`
			W    string `json:"w"`
		}
		if json.Unmarshal(line, &f) != nil || f.ID == 0 || f.K == "" || (len(f.WR) == 0 && len(f.WRNM) == 0) {
			continue
		}
		row := filledStageRow{id: f.ID, written: f.WR, writtenNoMain: f.WRNM}
		if stage != nil && entries != nil {
			if e := entries[f.K]; e != nil {
				full, rerr := stage.Read(e)
				if rerr != nil || full.Digest == nil || !bytes.Equal(full.Digest, f.WR) {
					// A newer stage entry exists and disagrees (or cannot be read); never accept
					// an older filled.jsonl digest (wr or wrnm) in its place.
					continue
				}
				row.beats, row.cues, row.loops = full.Perf.Beats, full.Perf.QuickCues, full.Perf.Loops
				out[f.K] = row
				continue
			}
		}
		if f.V != 0 && f.V != rbstate.Rules {
			continue
		}
		if tr := snap.Tracks[f.K]; tr != nil && tr.Row != "" && f.W != "" && f.W != tr.Row {
			continue
		}
		out[f.K] = row
	}
	return out
}

func writtenDigestIgnoringMainCueTail(beats, cues, loops []byte) []byte {
	h := sha256.New()
	for i, blob := range [][]byte{beats, cues} {
		raw, err := enginedb.QUncompress(blob, 8<<20)
		if err != nil {
			raw = blob
		}
		if i == 1 {
			raw = normalizeMainCueForWrittenDigest(raw)
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(raw)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(raw)
	}
	_, _ = h.Write(loops)
	return h.Sum(nil)[:16]
}

func normalizeMainCueForWrittenDigest(raw []byte) []byte {
	if len(raw) < 8 || binary.BigEndian.Uint64(raw) != 8 {
		return raw
	}
	off := 8
	for i := 0; i < 8; i++ {
		if off >= len(raw) || len(raw)-off-1 < int(raw[off])+12 {
			return raw
		}
		off += 1 + int(raw[off]) + 12
	}
	if off+17 != len(raw) {
		return raw
	}
	out := append([]byte(nil), raw...)
	if out[off+8] == 0 {
		clear(out[off : off+8])
	}
	clear(out[off+9:])
	return out
}

func needsCueRead(row dbCandidate) bool {
	if len(row.trackData) == 0 {
		return false
	}
	if row.track.Synced != nil && len(row.now.digest) > 0 && bytes.Equal(row.track.Synced, row.now.digest) {
		return false
	}
	if len(row.filledCues) > 0 {
		return !bytes.Equal(writtenDigestIgnoringMainCueTail(row.beatData, row.quickCues, row.loops), writtenDigestIgnoringMainCueTail(row.filledBeats, row.filledCues, row.filledLoops))
	}
	if len(row.filledWritten) > 0 || len(row.filledWrittenNoMain) > 0 {
		if len(row.filledWritten) > 0 && (bytes.Equal(row.writtenDigest, row.filledWritten) || bytes.Equal(row.rawWrittenDigest, row.filledWritten)) {
			return false
		}
		if len(row.filledWrittenNoMain) > 0 && bytes.Equal(writtenDigestIgnoringMainCueTail(row.beatData, row.quickCues, row.loops), row.filledWrittenNoMain) {
			return false
		}
		return true
	}
	if len(row.track.Written) == 0 {
		return true
	}
	if bytes.Equal(row.writtenDigest, row.track.Written) || bytes.Equal(row.rawWrittenDigest, row.track.Written) {
		return row.track.Synced != nil
	}
	if row.track.Synced == nil && enginedb.UnadjustedLegacyMatches(row.beatData, row.quickCues, row.loops, nil, row.track.Written) {
		return false
	}
	return true
}

func beatDataBlobsEqual(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	ra, ea := enginedb.QUncompress(a, 8<<20)
	if ea != nil {
		ra = a
	}
	rb, eb := enginedb.QUncompress(b, 8<<20)
	if eb != nil {
		rb = b
	}
	return bytes.Equal(ra, rb)
}

func needsGridRead(row dbCandidate) bool {
	if len(row.beatData) == 0 {
		return false
	}
	if len(row.filledBeats) > 0 {
		return !beatDataBlobsEqual(row.beatData, row.filledBeats)
	}
	if len(row.filledWritten) > 0 || len(row.filledWrittenNoMain) > 0 {
		if len(row.filledWritten) > 0 && (bytes.Equal(row.writtenDigest, row.filledWritten) || bytes.Equal(row.rawWrittenDigest, row.filledWritten)) {
			return false
		}
		if len(row.filledWrittenNoMain) > 0 && bytes.Equal(writtenDigestIgnoringMainCueTail(row.beatData, row.quickCues, row.loops), row.filledWrittenNoMain) {
			return false
		}
		return beatDataHasAdjustedEdit(row.beatData)
	}
	if len(row.track.Written) > 0 {
		if bytes.Equal(row.writtenDigest, row.track.Written) || bytes.Equal(row.rawWrittenDigest, row.track.Written) {
			return false
		}
		if row.track.Synced == nil && enginedb.UnadjustedLegacyMatches(row.beatData, row.quickCues, row.loops, nil, row.track.Written) {
			return false
		}
		// Written hashes beats+cues+loops together: only read the beat grid when the deck
		// explicitly wrote an adjusted grid (adjusted_grid != default_grid), so cue-only
		// edits and Engine auto-analysis never trigger spurious grid rewrites.
		return beatDataHasAdjustedEdit(row.beatData)
	}
	// No bridge beatData baseline: only treat explicit deck adjustments (adjusted != default)
	// as a grid edit so Engine's own auto-analysis never overwrites rekordbox's grid.
	return beatDataHasAdjustedEdit(row.beatData)
}

func appendCandidateRow(c dbCandidate, filled map[string]filledStageRow, out *candidateSet) {
	out.read++
	c.now.digest = enginedb.SyncedDigest(nil, c.quickCues, c.loops, c.activeLoops, c.metadata.Genre, c.metadata.Comment, c.metadata.Label)
	c.writtenDigest = enginedb.WrittenDigest(c.beatData, c.quickCues, c.loops)
	c.rawWrittenDigest = enginedb.RawWrittenDigest(c.beatData, c.quickCues, c.loops)
	if f, ok := filled[c.key]; ok && f.id == c.engineID && len(c.track.Written) == 0 {
		c.filledWritten, c.filledWrittenNoMain = f.written, f.writtenNoMain
		c.filledBeats, c.filledCues, c.filledLoops = f.beats, f.cues, f.loops
	}
	if c.track.Synced != nil && bytes.Equal(c.track.Synced, c.now.digest) && !needsGridRead(c) {
		out.insync[c.key] = c.now
		return
	}
	out.rows = append(out.rows, c)
}

// readCandidates finds the rows a pass has to compare with the export. A row whose lastEditTime
// is the one the snapshot noted with its digest is not read at all (#321: Engine's triggers date
// every write of a row's cues, loops and tags, so an untouched track's PerformanceData pages are
// never read). A row written without a change to them (an analysis, a grid) needs no export.
func readCandidates(ctx context.Context, db *sql.DB, snap *rbstate.Snapshot, session *CueSession) (candidateSet, error) {
	out := candidateSet{insync: map[string]noted{}}
	filled := readFilledStage(snap)
	var reads []dbCandidate
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(path,''), COALESCE(lastEditTime,0) FROM Track WHERE path IS NOT NULL`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c dbCandidate
		var path string
		if err = rows.Scan(&c.engineID, &path, &c.now.edit); err != nil {
			rows.Close()
			return out, err
		}
		tr, ok := snap.TrackAt(path)
		if !ok || tr.Native || tr.EngineID != c.engineID {
			continue
		}
		out.checked++
		if (c.now.edit != tr.Edit || tr.Synced == nil) && !session.has(tr.Key, c.now.edit) {
			c.key, c.track = tr.Key, tr
			reads = append(reads, c)
		}
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if len(reads) > 64 {
		// On legacy take-overs or large dirty batches, one sequential join replaces N point lookups.
		byID := make(map[int64]dbCandidate, len(reads))
		for _, c := range reads {
			byID[c.engineID] = c
		}
		qrows, err := db.QueryContext(ctx, `SELECT t.id, COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,''), p.beatData, p.trackData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops,0)
			FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id ORDER BY t.id`)
		if err != nil {
			return out, err
		}
		scanned := 0
		for qrows.Next() {
			if scanned&255 == 0 {
				if err := ctx.Err(); err != nil {
					qrows.Close()
					return out, err
				}
			}
			scanned++
			var id int64
			var c dbCandidate
			if err = qrows.Scan(&id, &c.metadata.Genre, &c.metadata.Comment, &c.metadata.Label, &c.beatData, &c.trackData, &c.quickCues, &c.loops, &c.activeLoops); err != nil {
				qrows.Close()
				return out, err
			}
			base, ok := byID[id]
			if !ok {
				continue
			}
			c.engineID, c.key, c.track, c.now = base.engineID, base.key, base.track, base.now
			appendCandidateRow(c, filled, &out)
		}
		if err = qrows.Close(); err != nil {
			return out, err
		}
	} else {
		for _, c := range reads {
			err = db.QueryRowContext(ctx, `SELECT COALESCE(t.genre,''), COALESCE(t.comment,''), COALESCE(t.label,''), p.beatData, p.trackData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops,0)
				FROM Track t LEFT JOIN PerformanceData p ON p.trackId=t.id WHERE t.id=?`, c.engineID).Scan(&c.metadata.Genre, &c.metadata.Comment, &c.metadata.Label, &c.beatData, &c.trackData, &c.quickCues, &c.loops, &c.activeLoops)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return out, err
			}
			appendCandidateRow(c, filled, &out)
		}
	}
	sort.Slice(out.rows, func(i, j int) bool { return out.rows[i].key < out.rows[j].key })
	return out, nil
}

var writeBackReadLibsHook func()

// readLibraries reads the export libraries the rows have references in.
func (p *pass) readLibraries(ctx context.Context, rows []dbCandidate) (map[rbstate.LibraryKind]SourceLibrary, error) {
	if writeBackReadLibsHook != nil {
		writeBackReadLibsHook()
	}
	out := map[rbstate.LibraryKind]SourceLibrary{}
	needed := map[rbstate.LibraryKind]bool{}
	for _, row := range rows {
		for _, ref := range row.track.Refs {
			needed[ref.Kind] = true
		}
	}
	if len(needed) == 0 {
		return out, nil
	}
	files, err := FindSources(p.r, p.mount)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	read, _, err := ReadSources(ctx, p.r, p.mount, files, p.keys)
	p.phase["decrypt"] += time.Since(started)
	if err != nil {
		return nil, err
	}
	for _, lib := range read {
		if needed[lib.Kind] {
			out[lib.Kind] = lib
		}
	}
	return out, nil
}

// buildChanges compares each row with its references (same ID, same path, same record) and
// returns the rows whose cues or metadata differ, and the rows that had a reference at all.
func buildChanges(ctx context.Context, resolver *compat.Resolver, libs map[rbstate.LibraryKind]SourceLibrary, rows []dbCandidate) ([]trackChange, map[string]bool, error) {
	var changes []trackChange
	matched := make(map[string]bool, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, matched, err
		}
		takeOver := row.track.Synced == nil
		change := trackChange{candidate: row, refs: map[rbstate.LibraryKind]rekordbox.Track{}, metadata: map[rbstate.LibraryKind]bool{}}
		var primary rekordbox.Track
		metaDiff := false
		matchedAnyMeta := false
		for _, ref := range row.track.Refs {
			src, ok := libs[ref.Kind]
			if !ok || src.Lib == nil {
				continue
			}
			for _, tr := range src.Lib.Tracks {
				if tr.ID != ref.ID || rbstate.FoldPath(tr.Path) != row.key || ref.RecordHash != "" && tr.RecordHash != ref.RecordHash {
					continue
				}
				change.refs[ref.Kind] = tr
				if primary.Path == "" {
					primary = tr
				}
				diff := row.metadata != (rekordbox.Metadata{Genre: tr.Genre, Comment: tr.Comment, Label: tr.Label})
				if !diff {
					matchedAnyMeta = true
				}
				change.metadata[ref.Kind], metaDiff = diff, metaDiff || diff
				break
			}
		}
		if len(change.refs) == 0 {
			continue
		}
		if takeOver && matchedAnyMeta {
			for k := range change.metadata {
				change.metadata[k] = false
			}
			metaDiff = false
		}
		matched[row.key] = true
		// needsCue: a row that still holds the import's written digest needs no ANLZ read. A
		// missing baseline, another written digest, or a row noted before and changed now is
		// compared with the ANLZ, so a deleted cue or a loop no longer active on load is written
		// (port3 regressions) although the import's baseline is unchanged.
		var orig []rekordbox.Cue
		var origBeats []rekordbox.Beat
		anlzDiff, oneDiff := false, false
		needsCue := needsCueRead(row)
		needsGrid := needsGridRead(row)
		if (needsCue || needsGrid) && primary.AnalysisPath != "" {
			offset, offsetOK := trackRekordboxOffset(resolver, primary.Path)
			if offsetOK && needsCue {
				if deck, err := enginedb.ParseDeckCuesAndLoops(row.trackData, row.quickCues, row.loops, row.activeLoops); err == nil {
					orig = readANLZCues(resolver, primary.AnalysisPath)
					if len(orig) == 0 {
						if one, hasOne := change.refs[rbstate.OneLibrary]; hasOne && len(one.Cues) > 0 {
							orig = one.Cues
						}
					}
					cuesMatchedLegacy := false
					if takeOver && len(row.track.Written) > 0 {
						cands := make([]float64, 0, len(orig)*2+1)
						cands = append(cands, 0)
						for _, c := range orig {
							if c.Loop || c.HotCue != 0 {
								continue
							}
							if pos := math.Round(float64(c.TimeMS)*deck.SampleRate/1000.0 - offset); pos >= 0 {
								cands = append(cands, pos)
							}
							if pos := math.Round(float64(c.TimeMS) * deck.SampleRate / 1000.0); pos >= 0 {
								cands = append(cands, pos)
							}
						}
						cuesMatchedLegacy = enginedb.UnadjustedLegacyMatches(row.beatData, row.quickCues, row.loops, nil, row.track.Written, cands...)
					}
					if !cuesMatchedLegacy {
						legacyMP3 := len(row.track.Written) == 0 && strings.HasSuffix(strings.ToLower(primary.Path), ".mp3")
						merged := mergeEngineCues(orig, deck, offset, legacyMP3)
						anlzDiff = !cuesEquivalent(orig, merged)
						if takeOver && anlzDiff && cuesEquivalent(orig, mergeEngineCues(orig, deck, 0, true)) {
							anlzDiff = false
						}
						if one, hasOne := change.refs[rbstate.OneLibrary]; hasOne && !cuesEquivalent(one.Cues, merged) {
							if anlzDiff || len(orig) == 0 || (!takeOver && len(one.Cues) > 0) {
								oneDiff = true
							}
						}
						if anlzDiff || oneDiff {
							change.cues = merged
							change.cuesDiff = true
							change.analysis = primary.AnalysisPath
						}
					}
				}
			}
			if offsetOK && needsGrid {
				if deckBeats, adjDiff, err := decodeEngineBeatGrid(row.beatData, offset); err == nil && len(deckBeats) > 0 {
					origBeats = readANLZBeats(resolver, primary.AnalysisPath)
					gridChanged := !beatsEquivalent(origBeats, deckBeats)
					if takeOver && gridChanged && !adjDiff {
						if deckZero, _, zerr := decodeEngineBeatGrid(row.beatData, 0); zerr == nil && beatsEquivalent(origBeats, deckZero) {
							gridChanged = false
						}
					}
					if gridChanged {
						change.beats = deckBeats
						change.bpm = dominantBeatBPM(deckBeats)
						change.gridDiff = true
						change.analysis = primary.AnalysisPath
					}
				}
			}
		}
		if change.cuesDiff || change.gridDiff || metaDiff {
			change.diff = describeTrackDiff(row, change, primary, orig, origBeats, anlzDiff, oneDiff)
			changes = append(changes, change)
		}
	}
	return changes, matched, nil
}

func describeTrackDiff(row dbCandidate, change trackChange, primary rekordbox.Track, orig []rekordbox.Cue, origBeats []rekordbox.Beat, anlzDiff, oneDiff bool) WriteBackTrackDiff {
	rel := row.track.Rel
	if rel == "" {
		rel = primary.Path
	}
	var fields []string
	var genreDone, commentDone, labelDone bool
	for _, kind := range []rbstate.LibraryKind{rbstate.DeviceLibrary, rbstate.OneLibrary} {
		if !change.metadata[kind] {
			continue
		}
		tr, ok := change.refs[kind]
		if !ok {
			continue
		}
		if !genreDone && row.metadata.Genre != tr.Genre {
			fields = append(fields, fmt.Sprintf("genre(db=%q lib=%q)", row.metadata.Genre, tr.Genre))
			genreDone = true
		}
		if !commentDone && row.metadata.Comment != tr.Comment {
			fields = append(fields, fmt.Sprintf("comment(db=%q lib=%q)", row.metadata.Comment, tr.Comment))
			commentDone = true
		}
		if !labelDone && row.metadata.Label != tr.Label {
			fields = append(fields, fmt.Sprintf("label(db=%q lib=%q)", row.metadata.Label, tr.Label))
			labelDone = true
		}
	}
	if anlzDiff {
		fields = append(fields, fmt.Sprintf("cues:anlz(orig=%d merged=%d)", len(orig), len(change.cues)))
	} else if oneDiff {
		one := change.refs[rbstate.OneLibrary]
		fields = append(fields, fmt.Sprintf("cues:onelibrary(orig=%d merged=%d)", len(one.Cues), len(change.cues)))
	}
	if change.gridDiff {
		fields = append(fields, fmt.Sprintf("grid:anlz(orig=%d deck=%d bpm=%.2f)", len(origBeats), len(change.beats), change.bpm))
	}
	return WriteBackTrackDiff{
		EngineID: row.engineID,
		Key:      row.key,
		Rel:      rel,
		Fields:   fields,
	}
}

// trackRekordboxOffset is the offset rekordbox's cue times have against Engine's samples; false
// when the audio cannot be read (a cue is then not written).
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

// anlzBase is an analysis path without its extension, if it is a relative path in USBANLZ.
func anlzBase(p string) (string, bool) {
	clean, err := compat.CleanRelative(p)
	upper := strings.ToUpper(clean)
	if err != nil || !strings.HasPrefix(upper, "PIONEER/USBANLZ/") && !strings.HasPrefix(upper, ".PIONEER/USBANLZ/") {
		return "", false
	}
	return strings.TrimSuffix(clean, filepath.Ext(clean)), true
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

// readANLZCues is the cues of a track's .DAT and .EXT together.
func readANLZCues(r *compat.Resolver, analysisPath string) []rekordbox.Cue {
	merged := readANLZPair(r, analysisPath)
	if merged == nil {
		return nil
	}
	return merged.Cues
}

// readANLZBeats is the beat grid of a track's .DAT (PQTZ) merged with .EXT (PQT2 sub-ms).
func readANLZBeats(r *compat.Resolver, analysisPath string) []rekordbox.Beat {
	merged := readANLZPair(r, analysisPath)
	if merged == nil {
		return nil
	}
	return merged.Beats
}

func readANLZPair(r *compat.Resolver, analysisPath string) *rekordbox.Analysis {
	base, ok := anlzBase(analysisPath)
	if !ok {
		return nil
	}
	var merged *rekordbox.Analysis
	for _, ext := range []string{".DAT", ".EXT"} {
		rel, ok := resolveANLZVariant(r, base, ext)
		if !ok {
			continue
		}
		b, err := os.ReadFile(r.FullPath(rel))
		if err != nil {
			continue
		}
		if a, err := rekordbox.ParseAnalysis(b); err == nil && merged == nil {
			merged = a
		} else if err == nil {
			merged.Merge(a)
		}
	}
	return merged
}

func foldANLZBase(base string) string {
	return strings.TrimPrefix(rbstate.FoldPath(base), ".")
}

// applyRepairChanges inspects ONLY ANLZ files previously modified by EngineX (present in
// .enginex-backup/ and owned by a non-Native track in snap.Tracks) and queues in-place
// ANLZ layout repairs when they carry swapped cue lists, old .EXT PCOB entries, or old
// b29=0 PCP2 bytes. First-original backups and untouched/third-party tracks are never touched.
func applyRepairChanges(r *compat.Resolver, mount string, snap *rbstate.Snapshot, changes []trackChange) []trackChange {
	if r == nil || snap == nil {
		return changes
	}
	backupRoot := filepath.Join(mount, ".enginex-backup")
	backedBases := backedUpANLZBases(backupRoot)
	if len(backedBases) == 0 {
		return changes
	}
	existingIdx := make(map[string]int, len(changes))
	seenBase := map[string]bool{}
	for i, ch := range changes {
		existingIdx[ch.candidate.key] = i
		if ch.cuesDiff {
			if base, ok := anlzBase(ch.analysis); ok {
				seenBase[foldANLZBase(base)] = true
			}
		}
	}

	keys := make([]string, 0, len(snap.Tracks))
	for k := range snap.Tracks {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		tr := snap.Tracks[k]
		if tr == nil || tr.Native {
			continue
		}
		if idx, ok := existingIdx[k]; ok && changes[idx].cuesDiff {
			continue
		}
		analysisPath := ""
		matchedBase := ""
		checkPath := func(p string) {
			if matchedBase != "" || p == "" {
				return
			}
			if base, ok := anlzBase(p); ok {
				folded := foldANLZBase(base)
				if backedBases[folded] {
					matchedBase = folded
					analysisPath = base + ".DAT"
				}
			}
		}
		for _, sig := range tr.Built.ANLZ {
			checkPath(sig.Rel)
		}
		checkPath(tr.AnalysisPath)
		if matchedBase == "" || seenBase[matchedBase] {
			continue
		}
		seenBase[matchedBase] = true

		base, ok := anlzBase(analysisPath)
		if !ok {
			continue
		}
		needsRepair := false
		hadLegacyB29Zero := false
		for _, ext := range []string{".DAT", ".EXT"} {
			rel, ok := resolveANLZVariant(r, base, ext)
			if !ok {
				continue
			}
			raw, err := os.ReadFile(r.FullPath(rel))
			if err != nil {
				continue
			}
			if anlzMatchesBackup(backupRoot, rel, raw) {
				continue
			}
			isExt := ext == ".EXT"
			if rekordbox.ANLZNeedsCueRepair(raw, isExt) {
				needsRepair = true
				if isExt && extHasLegacyB29Zero(raw) {
					hadLegacyB29Zero = true
				}
			}
		}
		if !needsRepair {
			continue
		}
		cues := readANLZCues(r, analysisPath)
		if hadLegacyB29Zero {
			for i := range cues {
				if cues[i].HotCue > 0 && isDefaultEnginePadName(cues[i].HotCue, cues[i].Name) {
					cues[i].Name = ""
					cues[i].ColorCode = 0
				}
			}
		}
		if idx, ok := existingIdx[k]; ok {
			changes[idx].cues = cues
			changes[idx].anlzCueRepair = true
			if changes[idx].analysis == "" {
				changes[idx].analysis = analysisPath
			}
			changes[idx].diff.Fields = append(changes[idx].diff.Fields, "cues:anlz-repair")
			continue
		}
		rel := tr.Rel
		if rel == "" {
			rel = tr.Key
		}
		changes = append(changes, trackChange{
			candidate:     dbCandidate{key: tr.Key, engineID: tr.EngineID, track: tr},
			refs:          map[rbstate.LibraryKind]rekordbox.Track{},
			cues:          cues,
			analysis:      analysisPath,
			anlzCueRepair: true,
			metadata:      map[rbstate.LibraryKind]bool{},
			diff: WriteBackTrackDiff{
				EngineID: tr.EngineID,
				Key:      tr.Key,
				Rel:      rel,
				Fields:   []string{"cues:anlz-repair"},
			},
		})
	}
	return changes
}

func anlzMatchesBackup(backupRoot, rel string, raw []byte) bool {
	entries, err := os.ReadDir(backupRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(backupRoot, entry.Name(), filepath.FromSlash(rel)))
		if err == nil && bytes.Equal(b, raw) {
			return true
		}
	}
	return false
}

func backedUpANLZBases(backupRoot string) map[string]bool {
	dateDirs, err := os.ReadDir(backupRoot)
	if err != nil || len(dateDirs) == 0 {
		return nil
	}
	bases := map[string]bool{}
	for _, d := range dateDirs {
		if !d.IsDir() {
			continue
		}
		dateRoot := filepath.Join(backupRoot, d.Name())
		for _, pioneer := range []string{"PIONEER", ".PIONEER"} {
			anlzRoot := filepath.Join(dateRoot, pioneer, "USBANLZ")
			if st, err := os.Stat(anlzRoot); err != nil || !st.IsDir() {
				continue
			}
			_ = filepath.WalkDir(anlzRoot, func(p string, ent os.DirEntry, werr error) error {
				if werr != nil || ent.IsDir() {
					return nil
				}
				upper := strings.ToUpper(filepath.Ext(ent.Name()))
				if upper != ".DAT" && upper != ".EXT" {
					return nil
				}
				rel, err := filepath.Rel(dateRoot, p)
				if err != nil {
					return nil
				}
				if base, ok := anlzBase(filepath.ToSlash(rel)); ok {
					bases[foldANLZBase(base)] = true
				}
				return nil
			})
		}
	}
	return bases
}

func extHasLegacyB29Zero(raw []byte) bool {
	if len(raw) < 12 || string(raw[:4]) != "PMAI" {
		return false
	}
	h, n := int(binary.BigEndian.Uint32(raw[4:])), int(binary.BigEndian.Uint32(raw[8:]))
	if h < 12 || h > n || n > len(raw) {
		return false
	}
	for off := h; off+12 <= n; {
		sn := int(binary.BigEndian.Uint32(raw[off+8:]))
		if sn < 12 || off+sn > n {
			return false
		}
		if string(raw[off:off+4]) == "PCO2" && sn >= 20 {
			cnt := int(binary.BigEndian.Uint16(raw[off+16:]))
			poff := off + 20
			for i := 0; i < cnt && poff+44 <= off+sn; i++ {
				psn := int(binary.BigEndian.Uint32(raw[poff+8:]))
				if psn < 44 || poff+psn > off+sn {
					return false
				}
				if raw[poff+29] == 0 {
					return true
				}
				poff += psn
			}
		}
		off += sn
	}
	return false
}

// engineBeatMarker is one decoded 24-byte Engine beatData grid marker.
type engineBeatMarker struct {
	pos       float64
	beatIndex int64
	numBeats  uint32
}

func parseEngineBeatDataMarkers(blob []byte) (sampleRate, gridSamples float64, def, adj []engineBeatMarker, err error) {
	raw, err := enginedb.QUncompress(blob, 8<<20)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(raw) < 33 || raw[16] != 1 {
		return 0, 0, nil, nil, fmt.Errorf("beatData: invalid or unanalyzed header")
	}
	sampleRate = math.Float64frombits(binary.BigEndian.Uint64(raw[0:8]))
	gridSamples = math.Float64frombits(binary.BigEndian.Uint64(raw[8:16]))
	if math.IsNaN(sampleRate) || math.IsInf(sampleRate, 0) || sampleRate < 8000 || sampleRate > 384000 ||
		math.IsNaN(gridSamples) || math.IsInf(gridSamples, 0) || gridSamples <= 0 {
		return 0, 0, nil, nil, fmt.Errorf("beatData: invalid sample rate or length")
	}
	readGrid := func(off int) ([]engineBeatMarker, int, error) {
		if len(raw)-off < 8 {
			return nil, 0, fmt.Errorf("beatData: truncated grid count")
		}
		n := binary.BigEndian.Uint64(raw[off:])
		off += 8
		if n > 65536 || n > uint64((len(raw)-off)/24) {
			return nil, 0, fmt.Errorf("beatData: invalid grid count %d", n)
		}
		markers := make([]engineBeatMarker, 0, n)
		stopped := false
		for i := uint64(0); i < n; i++ {
			p := math.Float64frombits(binary.LittleEndian.Uint64(raw[off:]))
			idx := int64(binary.LittleEndian.Uint64(raw[off+8:]))
			nb := binary.LittleEndian.Uint32(raw[off+16:])
			off += 24
			if stopped {
				continue
			}
			// Engine 5.3.0 can append a trailing NaN sentinel marker after a numBeats==0
			// terminator when a DJ shifts a grid on the deck; stop cleanly there.
			if math.IsNaN(p) || math.IsInf(p, 0) {
				if len(markers) >= 2 && markers[len(markers)-1].numBeats == 0 {
					stopped = true
					continue
				}
				return nil, 0, fmt.Errorf("beatData: non-finite marker")
			}
			if math.Abs(p) > 1e12 || idx < -1000000 || idx > 1000000 {
				return nil, 0, fmt.Errorf("beatData: out-of-range marker")
			}
			if len(markers) > 0 {
				prev := markers[len(markers)-1]
				if p <= prev.pos || idx <= prev.beatIndex {
					if len(markers) >= 2 && prev.numBeats == 0 {
						stopped = true
						continue
					}
					return nil, 0, fmt.Errorf("beatData: unordered markers")
				}
			}
			markers = append(markers, engineBeatMarker{pos: p, beatIndex: idx, numBeats: nb})
		}
		return markers, off, nil
	}
	var off int
	def, off, err = readGrid(17)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	adj, _, err = readGrid(off)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	return sampleRate, gridSamples, def, adj, nil
}

func engineBeatMarkersEqual(a, b []engineBeatMarker) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].beatIndex != b[i].beatIndex || math.Abs(a[i].pos-b[i].pos) > 1e-6 {
			return false
		}
	}
	return true
}

func beatDataHasAdjustedEdit(blob []byte) bool {
	_, _, def, adj, err := parseEngineBeatDataMarkers(blob)
	if err != nil || len(adj) < 2 {
		return false
	}
	return !engineBeatMarkersEqual(def, adj)
}

// decodeEngineBeatGrid expands Engine PerformanceData.beatData (preferring adjusted_grid
// over default_grid) back into rekordbox beats with sub-millisecond timing and offsetSamples
// added back to convert from Engine's decoded-audio origin to rekordbox's file origin.
func decodeEngineBeatGrid(blob []byte, offsetSamples float64) ([]rekordbox.Beat, bool, error) {
	sr, _, def, adj, err := parseEngineBeatDataMarkers(blob)
	if err != nil {
		return nil, false, err
	}
	adjustedDiffers := len(adj) >= 2 && !engineBeatMarkersEqual(def, adj)
	m := adj
	if len(m) < 2 {
		m = def
	}
	if len(m) < 2 {
		return nil, adjustedDiffers, nil
	}
	totalBeats := m[len(m)-1].beatIndex - m[0].beatIndex + 1
	if totalBeats <= 0 || totalBeats > 65536 {
		return nil, adjustedDiffers, fmt.Errorf("beatData: excessive beat span %d", totalBeats)
	}
	beats := make([]rekordbox.Beat, 0, totalBeats)
	appendBeat := func(pos float64, beatIndex int64, bpm float64) {
		tUS := math.Round((pos + offsetSamples) * 1e6 / sr)
		if tUS < 0 || tUS > float64(math.MaxUint32)*1000 {
			return
		}
		us := uint64(tUS)
		timeMS := uint32(us / 1000)
		subUS := uint16(us % 1000)
		if len(beats) > 0 && timeMS <= beats[len(beats)-1].TimeMS {
			return
		}
		num := uint16(((beatIndex%4)+4)%4 + 1)
		beats = append(beats, rekordbox.Beat{
			Number: num,
			BPM:    bpm,
			TimeMS: timeMS,
			SubUS:  subUS,
		})
	}
	var lastBPM float64
	for i := 0; i+1 < len(m); i++ {
		span := m[i+1].beatIndex - m[i].beatIndex
		dp := m[i+1].pos - m[i].pos
		if span <= 0 || dp <= 0 {
			return nil, adjustedDiffers, fmt.Errorf("beatData: invalid segment")
		}
		stepSamples := dp / float64(span)
		bpm := 60.0 * sr / stepSamples
		if math.IsNaN(bpm) || math.IsInf(bpm, 0) || bpm <= 0 || bpm > 655.35 {
			return nil, adjustedDiffers, fmt.Errorf("beatData: invalid segment bpm %v", bpm)
		}
		bpm = math.Round(bpm*100) / 100
		lastBPM = bpm
		for b := m[i].beatIndex; b < m[i+1].beatIndex; b++ {
			pos := m[i].pos + float64(b-m[i].beatIndex)*stepSamples
			appendBeat(pos, b, bpm)
		}
	}
	if lastBPM > 0 {
		last := m[len(m)-1]
		appendBeat(last.pos, last.beatIndex, lastBPM)
	}
	return beats, adjustedDiffers, nil
}

func beatsEquivalent(a, b []rekordbox.Beat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Number != b[i].Number {
			return false
		}
		if math.Abs(a[i].BPM-b[i].BPM) > 0.05 {
			return false
		}
		ta := float64(a[i].TimeMS) + float64(a[i].SubUS)/1000.0
		tb := float64(b[i].TimeMS) + float64(b[i].SubUS)/1000.0
		if math.Abs(ta-tb) > 2.5 {
			return false
		}
	}
	return true
}

func dominantBeatBPM(beats []rekordbox.Beat) float64 {
	if len(beats) == 0 {
		return 0
	}
	counts := map[int]int{}
	bestKey, bestCount := 0, 0
	for _, b := range beats {
		k := int(math.Round(b.BPM * 100))
		if k <= 0 {
			continue
		}
		counts[k]++
		if counts[k] > bestCount {
			bestKey, bestCount = k, counts[k]
		}
	}
	if bestKey == 0 {
		return 0
	}
	return float64(bestKey) / 100.0
}

// near: two cue times the same within the rounding of a sample position to milliseconds.
func near(a, b uint32) bool { return max(a, b)-min(a, b) <= 2 }

func cueColorEquivalent(x, y rekordbox.Cue) bool {
	if x.HotCue == 0 {
		return x.ColorID == y.ColorID
	}
	if uncoloured(x) && uncoloured(y) {
		return true
	}
	if uncoloured(x) != uncoloured(y) {
		return false
	}
	if x.Color != ([4]byte{}) && x.Color == y.Color {
		return true
	}
	cx := x.ColorCode
	if cx == 0 && x.Color != ([4]byte{}) {
		cx = rekordbox.NearestCueColorCode(x.Color)
	}
	cy := y.ColorCode
	if cy == 0 && y.Color != ([4]byte{}) {
		cy = rekordbox.NearestCueColorCode(y.Color)
	}
	return cx != 0 && cx == cy
}

func cuesEquivalent(a, b []rekordbox.Cue) bool {
	na, nb := sortedCues(a), sortedCues(b)
	if len(na) != len(nb) {
		return false
	}
	for i, x := range na {
		y := nb[i]
		if x.HotCue != y.HotCue || x.Loop != y.Loop || x.Name != y.Name || x.Active != y.Active || !near(x.TimeMS, y.TimeMS) || x.Loop && !near(x.EndMS, y.EndMS) {
			return false
		}
		if !cueColorEquivalent(x, y) {
			return false
		}
	}
	return true
}

func sortedCues(in []rekordbox.Cue) []rekordbox.Cue {
	return slices.SortedStableFunc(slices.Values(in), func(a, b rekordbox.Cue) int {
		loop := func(c rekordbox.Cue) int {
			if c.Loop {
				return 1
			}
			return 0
		}
		return cmp.Or(cmp.Compare(a.HotCue, b.HotCue), cmp.Compare(loop(a), loop(b)), cmp.Compare(a.TimeMS, b.TimeMS), cmp.Compare(a.EndMS, b.EndMS))
	})
}

// The colours Engine gives a hot cue and a loop that had none: an imported cue without a colour
// keeps none while the deck shows it so.
var (
	engineDefaultHotCueColor = [4]byte{255, 0, 255, 0}
	engineDefaultLoopColor   = [4]byte{255, 255, 127, 0}
)

func uncoloured(c rekordbox.Cue) bool {
	return c.Color == ([4]byte{}) && c.ColorCode == 0 && c.ColorID == 0
}

// isDefaultEnginePadName reports Engine's auto-generated unnamed hot-cue label ("Cue 1".."Cue 8").
// Stripping it for unnamed deck pads yields rekordbox's compact 44/48-byte unnamed PCP2 layout.
func isDefaultEnginePadName(pad uint32, name string) bool {
	return pad >= 1 && pad <= 8 && name == fmt.Sprintf("Cue %d", pad)
}

// mergeEngineCues is the cue list the export gets for the deck's cues: the deck's hot cues and
// loops, with what the import had and Engine cannot hold kept (clamped starts, loop fractions,
// colour codes, no colour), and the imported memory points as they are.
func mergeEngineCues(orig []rekordbox.Cue, deck enginedb.DeckCues, offset float64, legacy bool) []rekordbox.Cue {
	origHot := map[uint32]rekordbox.Cue{}
	var memPoints, memLoops []rekordbox.Cue
	for _, c := range orig {
		switch {
		case c.HotCue > 0:
			origHot[c.HotCue] = c
		case c.Loop:
			memLoops = append(memLoops, c)
		default:
			memPoints = append(memPoints, c)
		}
	}
	toMS := func(samples float64) uint32 {
		v := math.Round((max(samples, 0) + offset) * 1000 / deck.SampleRate)
		return uint32(min(max(v, 0), math.MaxUint32))
	}
	// atZero: the deck clamped to zero a cue the import has before the track's first sample.
	atZero := func(samples float64, oc rekordbox.Cue) bool {
		return samples <= 0.5 && float64(oc.TimeMS)*deck.SampleRate/1000 <= offset+0.5
	}
	// sameLoop: the deck's loop is the imported loop oc; start is the one to write.
	sameLoop := func(samples float64, oc rekordbox.Cue, start, end uint32) (uint32, bool) {
		if atZero(samples, oc) && near(oc.EndMS, end) && oc.TimeMS < end {
			start = oc.TimeMS
		}
		return start, near(oc.TimeMS, start) && near(oc.EndMS, end)
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
		oc, hasOrig := origHot[pad]
		if lp := deck.Loops[s]; lp != nil && near(start, toMS(lp.StartSamples)) {
			consumed[s] = true
			end := max(toMS(lp.EndSamples), start+1)
			name, rgb := cmp.Or(lp.Name, hc.Name), cmp.Or(lp.Color, hc.Color)
			if isDefaultEnginePadName(pad, name) && (!hasOrig || oc.Name == "") {
				name = ""
			}
			var code byte
			if name != "" {
				code = rekordbox.NearestCueColorCode(rgb)
			}
			var num, den uint16
			if st, ok := sameLoop(hc.Samples, oc, start, end); hasOrig && oc.Loop && ok {
				start, num, den = st, oc.LoopNumerator, oc.LoopDenominator
				if uncoloured(oc) && rgb == engineDefaultLoopColor {
					rgb, code = [4]byte{}, 0
				} else if oc.Color == rgb && oc.ColorCode != 0 {
					code = oc.ColorCode
				} else if name != "" && code == 0 {
					code = rekordbox.NearestCueColorCode(rgb)
				}
			}
			out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, EndMS: end, Loop: true, Active: lp.Active, Name: name, Color: rgb, ColorCode: code, LoopNumerator: num, LoopDenominator: den, Extended: true})
			continue
		}
		rgb := hc.Color
		name := hc.Name
		if isDefaultEnginePadName(pad, name) && (!hasOrig || oc.Name == "") {
			name = ""
		}
		var code byte
		if name != "" {
			code = rekordbox.NearestCueColorCode(rgb)
		}
		if hasOrig && !oc.Loop {
			if atZero(hc.Samples, oc) || legacy && hc.Name == "" && oc.Name == "" && max(oc.TimeMS, start)-min(oc.TimeMS, start) <= 60 {
				start = oc.TimeMS
			}
			if uncoloured(oc) && near(oc.TimeMS, start) && rgb == engineDefaultHotCueColor {
				rgb, code = [4]byte{}, 0
			} else if oc.Color == rgb && oc.ColorCode != 0 {
				code = oc.ColorCode
			} else if name != "" && code == 0 {
				code = rekordbox.NearestCueColorCode(rgb)
			}
		}
		out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, Name: name, Color: rgb, ColorCode: code, Extended: true})
	}
	for s, lp := range deck.Loops {
		if consumed[s] || lp == nil {
			continue
		}
		start, end := toMS(lp.StartSamples), toMS(lp.EndSamples)
		if end <= start {
			continue
		}
		id := rekordbox.NearestMemoryColorID(lp.Color)
		var num, den uint16
		for _, oc := range memLoops {
			if st, ok := sameLoop(lp.StartSamples, oc, start, end); ok {
				start, num, den = st, oc.LoopNumerator, oc.LoopDenominator
				if uncoloured(oc) && lp.Color == engineDefaultLoopColor {
					id = 0
				} else if oc.ColorID != 0 {
					id = oc.ColorID
				}
				break
			}
		}
		out = append(out, rekordbox.Cue{TimeMS: start, EndMS: end, Loop: true, Active: lp.Active, Name: lp.Name, ColorID: id, LoopNumerator: num, LoopDenominator: den, Extended: true})
	}
	return append(out, memPoints...)
}
