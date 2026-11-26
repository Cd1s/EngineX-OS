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

// noted is a row as a pass found it in sync with the export: rbstate.Track.Edit and Synced.
type noted struct {
	edit   int64
	digest []byte
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
	candidate dbCandidate
	refs      map[rbstate.LibraryKind]rekordbox.Track
	cues      []rekordbox.Cue
	analysis  string // set when cuesDiff
	cuesDiff  bool
	metadata  map[rbstate.LibraryKind]bool
	diff      WriteBackTrackDiff
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
	if len(cs.rows) == 0 {
		// Nothing to compare with the export: no file of the stick is read or written.
		if settle() != nil {
			return "", errNotQuiet
		}
		if !p.opts.DryRun {
			p.opts.Session.add(cs.insync)
		}
		return "", nil
	}

	libs, err := p.readLibraries(ctx, cs.rows)
	if err != nil {
		return "", err
	}
	changes, matched, err := buildChanges(ctx, p.r, libs, cs.rows)
	if err != nil {
		return "", err
	}
	if err := settle(); err != nil {
		return "", err
	}
	// A row compared with every library that holds it is in sync once this pass is done.
	for _, row := range cs.rows {
		if matched[row.key] {
			cs.insync[row.key] = row.now
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

func appendCandidateRow(c dbCandidate, filled map[string]filledStageRow, out *candidateSet) {
	out.read++
	c.now.digest = enginedb.SyncedDigest(nil, c.quickCues, c.loops, c.activeLoops, c.metadata.Genre, c.metadata.Comment, c.metadata.Label)
	if c.track.Synced != nil && bytes.Equal(c.track.Synced, c.now.digest) {
		out.insync[c.key] = c.now
		return
	}
	c.writtenDigest = enginedb.WrittenDigest(c.beatData, c.quickCues, c.loops)
	c.rawWrittenDigest = enginedb.RawWrittenDigest(c.beatData, c.quickCues, c.loops)
	if f, ok := filled[c.key]; ok && f.id == c.engineID && len(c.track.Written) == 0 {
		c.filledWritten, c.filledWrittenNoMain = f.written, f.writtenNoMain
		c.filledBeats, c.filledCues, c.filledLoops = f.beats, f.cues, f.loops
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
		anlzDiff, oneDiff := false, false
		needsCue := needsCueRead(row)
		if needsCue && primary.AnalysisPath != "" {
			deck, err := enginedb.ParseDeckCuesAndLoops(row.trackData, row.quickCues, row.loops, row.activeLoops)
			offset, ok := 0.0, false
			if err == nil {
				offset, ok = trackRekordboxOffset(resolver, primary.Path)
			}
			if ok {
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
						if !takeOver || anlzDiff || len(orig) == 0 {
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
		if change.cuesDiff || metaDiff {
			change.diff = describeTrackDiff(row, change, primary, orig, anlzDiff, oneDiff)
			changes = append(changes, change)
		}
	}
	return changes, matched, nil
}

func describeTrackDiff(row dbCandidate, change trackChange, primary rekordbox.Track, orig []rekordbox.Cue, anlzDiff, oneDiff bool) WriteBackTrackDiff {
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
	if merged == nil {
		return nil
	}
	return merged.Cues
}

// near: two cue times the same within the rounding of a sample position to milliseconds.
func near(a, b uint32) bool { return max(a, b)-min(a, b) <= 2 }

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
		if x.HotCue == 0 && x.ColorID != y.ColorID || x.Color != y.Color && x.ColorCode != y.ColorCode {
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
			code := rekordbox.NearestCueColorCode(rgb)
			var num, den uint16
			if st, ok := sameLoop(hc.Samples, oc, start, end); hasOrig && oc.Loop && ok {
				start, num, den = st, oc.LoopNumerator, oc.LoopDenominator
				if uncoloured(oc) && rgb == engineDefaultLoopColor {
					rgb, code = [4]byte{}, 0
				} else if oc.Color == rgb && oc.ColorCode != 0 {
					code = oc.ColorCode
				}
			}
			out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, EndMS: end, Loop: true, Active: lp.Active, Name: name, Color: rgb, ColorCode: code, LoopNumerator: num, LoopDenominator: den, Extended: true})
			continue
		}
		rgb := hc.Color
		code := rekordbox.NearestCueColorCode(rgb)
		if hasOrig && !oc.Loop {
			if atZero(hc.Samples, oc) || legacy && hc.Name == "" && oc.Name == "" && max(oc.TimeMS, start)-min(oc.TimeMS, start) <= 60 {
				start = oc.TimeMS
			}
			if uncoloured(oc) && near(oc.TimeMS, start) && rgb == engineDefaultHotCueColor {
				rgb, code = [4]byte{}, 0
			} else if oc.Color == rgb && oc.ColorCode != 0 {
				code = oc.ColorCode
			}
		}
		out = append(out, rekordbox.Cue{HotCue: pad, TimeMS: start, Name: hc.Name, Color: rgb, ColorCode: code, Extended: true})
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
