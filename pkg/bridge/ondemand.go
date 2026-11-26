package bridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

var (
	// ErrOnDemandNotFound indicates the track or snapshot is absent or belongs to Engine's own library.
	ErrOnDemandNotFound = errors.New("bridge: on-demand track not found in snapshot")
	// ErrOnDemandUUIDMismatch indicates the request's library UUID differs from the stick's snapshot.
	ErrOnDemandUUIDMismatch = errors.New("bridge: on-demand snapshot uuid mismatch")
	// ErrOnDemandDegraded indicates the track's audio or ANLZ files are missing, corrupt, or incomplete.
	ErrOnDemandDegraded = errors.New("bridge: on-demand track degraded or incomplete")
)

// engineKeyToCamelot is the inverse of enginedb.EngineKey (0..23 -> Camelot).
var engineKeyToCamelot = [24]string{
	"8B", "8A", "9B", "9A", "10B", "10A", "11B", "11A",
	"12B", "12A", "1B", "1A", "2B", "2A", "3B", "3A",
	"4B", "4A", "5B", "5A", "6B", "6A", "7B", "7A",
}

// OnDemandRequest identifies a single stick track that Engine is about to load onto a deck.
type OnDemandRequest struct {
	Mount      string  // mounted volume root (e.g. "/media/NEXTLIB")
	UUID       string  // Information.uuid from m.db (checked against snapshot when non-empty)
	EnginePath string  // Track.path as stored in m.db (e.g. "../Contents/Artist/Track.mp3")
	EngineID   int64   // Track.id in m.db (verified against snapshot when > 0)
	EngineKey  int     // Track.key (0..23) in m.db, or -1 if NULL/unknown
	EngineBPM  float64 // Track.bpm (or bpmAnalyzed) in m.db, 0 if unknown
	EngineLen  int     // Track.length (seconds) in m.db, 0 if unknown
}

// OnDemandTiming breaks down single-track direct-read latency in milliseconds.
type OnDemandTiming struct {
	SnapshotMS    float64 `json:"snapshotMs"`
	StatMS        float64 `json:"statMs"`
	ANLZReadMS    float64 `json:"anlzReadMs"`
	AudioHeaderMS float64 `json:"audioHeaderMs"`
	ExportMS      float64 `json:"exportMs"`
	EncodeMS      float64 `json:"encodeMs"`
	TotalMS       float64 `json:"totalMs"`
	ANLZBytes     int64   `json:"anlzBytes"`
	UsedExport    bool    `json:"usedExport"`
}

// OnDemandResult is the in-memory RBXA1 payload and digest metadata returned to the UI hook.
type OnDemandResult struct {
	RBXA1         []byte         // rbstate.EncodePerf(perf)
	Key           string         // folded track key (rbstate.Track.Key)
	Written       []byte         // 16-byte enginedb.WrittenDigest(beats, cues, loops)
	WrittenNoMain []byte         // 16-byte writtenDigestIgnoringMainCueTail(beats, cues, loops)
	RowHash       string         // rbstate.Track.Row
	ActiveLoops   uint32         // activeOnLoadLoops bitmask
	Rules         int            // rbstate.Rules (5)
	Timing        OnDemandTiming // per-stage latency breakdown
}

type cachedOnDemandSnapshot struct {
	size  int64
	mtime time.Time
	snap  *rbstate.Snapshot
	res   *compat.Resolver
}

type cachedOnDemandExport struct {
	sigs   []rbstate.FileSig
	tracks map[string]rekordbox.Track // folded key -> merged export track
}

type onDemandCall struct {
	done chan struct{}
	res  *OnDemandResult
	err  error
}

// OnDemandBuilder reads a single track's rekordbox ANLZ files and audio header directly from the
// stick and translates them in memory into Engine's RBXA1 format with zero staging writes.
type OnDemandBuilder struct {
	snapMu   sync.Mutex
	snaps    map[string]*cachedOnDemandSnapshot
	exportMu sync.Mutex
	exports  map[string]*cachedOnDemandExport
	flightMu sync.Mutex
	inflight map[string]*onDemandCall
}

// NewOnDemandBuilder returns an OnDemandBuilder with bounded per-volume snapshot and fallback caches.
func NewOnDemandBuilder() *OnDemandBuilder {
	return &OnDemandBuilder{
		snaps:    map[string]*cachedOnDemandSnapshot{},
		exports:  map[string]*cachedOnDemandExport{},
		inflight: map[string]*onDemandCall{},
	}
}

// NoteLoading signals any active background rb-bridge pass to yield the stick during an on-demand
// track load, without clearing an existing playing state and without writing when no pass is active.
func (q *PriorityQueue) NoteLoading(now time.Time) {
	if q == nil {
		return
	}
	if _, err := os.Lstat(usbPreparingFlagPath); err != nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	prevLoading := q.deckLoading
	q.deckLoading = true
	q.hasActivity = true
	if !prevLoading || now.Sub(q.lastActWrite) >= time.Second {
		act := DeckActivity{Active: q.deckActive, Loading: true, IdleSamples: q.idleSamples, UpdatedAtMS: now.UnixMilli()}
		if writePrivateJSON(deckActivityPath(), act) == nil {
			q.lastActWrite = now
		}
	}
}

func cleanEnginePath(p string) (string, error) {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "../")
	return compat.CleanRelative(p)
}

// Build reads one track's rekordbox ANLZ + audio header straight from req.Mount and returns its
// in-memory RBXA1 performance payload. Concurrent requests for the same track are coalesced.
func (b *OnDemandBuilder) Build(ctx context.Context, req OnDemandRequest) (*OnDemandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mount := filepath.Clean(strings.TrimSpace(req.Mount))
	if mount == "" || mount == "." || !filepath.IsAbs(mount) {
		return nil, ErrOnDemandNotFound
	}
	cleanRel, err := cleanEnginePath(req.EnginePath)
	if err != nil {
		return nil, ErrOnDemandNotFound
	}
	req.Mount = mount
	req.EnginePath = cleanRel
	sfKey := mount + "\x00" + req.UUID + "\x00" + rbstate.FoldPath(cleanRel)

	b.flightMu.Lock()
	if call, ok := b.inflight[sfKey]; ok {
		b.flightMu.Unlock()
		select {
		case <-call.done:
			return call.res, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &onDemandCall{done: make(chan struct{})}
	b.inflight[sfKey] = call
	b.flightMu.Unlock()

	go func() {
		// Detach leader from a single caller's cancellation so a second coalesced caller still completes.
		buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		call.res, call.err = b.buildOnce(buildCtx, req)
		b.flightMu.Lock()
		delete(b.inflight, sfKey)
		b.flightMu.Unlock()
		close(call.done)
	}()

	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *OnDemandBuilder) loadSnapshot(mount string) (*rbstate.Snapshot, *compat.Resolver, error) {
	dbDir := filepath.Join(mount, databaseSubdir)
	st, err := os.Stat(filepath.Join(dbDir, rbstate.SnapshotName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrOnDemandNotFound
		}
		return nil, nil, err
	}
	b.snapMu.Lock()
	if c := b.snaps[dbDir]; c != nil && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		b.snapMu.Unlock()
		return c.snap, c.res, nil
	}
	b.snapMu.Unlock()

	snap, err := rbstate.Load(dbDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrOnDemandNotFound
		}
		return nil, nil, err
	}
	res, err := compat.NewResolver(mount)
	if err != nil {
		return nil, nil, err
	}
	b.snapMu.Lock()
	if len(b.snaps) >= 4 {
		clear(b.snaps)
	}
	b.snaps[dbDir] = &cachedOnDemandSnapshot{size: st.Size(), mtime: st.ModTime(), snap: snap, res: res}
	b.snapMu.Unlock()
	return snap, res, nil
}

func fileSigsEqual(a, b []rbstate.FileSig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lookupExportTrack lazily parses the stick's export DB only on rare fallback paths (snapshots
// predating Track.AnalysisPath or AAC implicit SBR streams advertising core rate <= 24 kHz).
func (b *OnDemandBuilder) lookupExportTrack(ctx context.Context, mount string, res *compat.Resolver, key string) (rekordbox.Track, bool, error) {
	sigs, err := FindSources(res, mount)
	if err != nil || len(sigs) == 0 {
		return rekordbox.Track{}, false, err
	}
	b.exportMu.Lock()
	if c := b.exports[mount]; c != nil && fileSigsEqual(c.sigs, sigs) {
		tr, ok := c.tracks[key]
		b.exportMu.Unlock()
		return tr, ok, nil
	}
	b.exportMu.Unlock()

	libs, _, err := ReadSources(ctx, res, mount, sigs, nil)
	if err != nil || len(libs) == 0 {
		return rekordbox.Track{}, false, err
	}
	view, err := BuildViewCtx(ctx, libs, "", "")
	if err != nil {
		return rekordbox.Track{}, false, err
	}
	tracks := make(map[string]rekordbox.Track, len(view.Tracks))
	for i := range view.Tracks {
		vt := &view.Tracks[i]
		tracks[vt.Key] = vt.Track
	}
	b.exportMu.Lock()
	if len(b.exports) >= 2 {
		clear(b.exports)
	}
	b.exports[mount] = &cachedOnDemandExport{sigs: sigs, tracks: tracks}
	b.exportMu.Unlock()
	tr, ok := tracks[key]
	return tr, ok, nil
}

func openOnDemandFile(full string) (*os.File, error) {
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("not a regular file: %s", full)
	}
	return f, nil
}

// resolveVolumeFile checks the exact relative path under mount first (zero ReadDir calls on the
// common case) and falls back to compat.Resolver for case or Unicode NFC/NFD differences.
func resolveVolumeFile(ctx context.Context, mount string, res *compat.Resolver, rel string) (string, error) {
	clean, err := compat.CleanRelative(rel)
	if err != nil {
		return "", err
	}
	direct := filepath.Join(mount, filepath.FromSlash(clean))
	if st, err := os.Lstat(direct); err == nil {
		if st.Mode().IsRegular() {
			return direct, nil
		}
		return "", fmt.Errorf("not a regular file: %s", clean)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	resolved, err := res.ResolveContext(ctx, clean)
	if err != nil {
		return "", err
	}
	full := res.FullPath(resolved)
	st, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", resolved)
	}
	return full, nil
}

// resolveANLZOpen opens an ANLZ file by trying the exact path directly before falling back to
// compat.Resolver (avoiding extra lstat + ReadDir syscalls on slow USB sticks).
func resolveANLZOpen(ctx context.Context, mount string, res *compat.Resolver, rel string) (*os.File, error) {
	clean, err := compat.CleanRelative(rel)
	if err != nil {
		return nil, err
	}
	direct := filepath.Join(mount, filepath.FromSlash(clean))
	f, err := openOnDemandFile(direct)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	resolved, err := res.ResolveContext(ctx, clean)
	if err != nil {
		return nil, err
	}
	return openOnDemandFile(res.FullPath(resolved))
}

func anlzCandidates(tr *rbstate.Track, fallbackAnalysisPath string) (datRel, extRel, ex2Rel string) {
	if len(tr.Built.ANLZ) > 0 {
		for _, f := range tr.Built.ANLZ {
			switch strings.ToUpper(path.Ext(f.Rel)) {
			case ".DAT":
				datRel = f.Rel
			case ".EXT":
				extRel = f.Rel
			case ".2EX":
				ex2Rel = f.Rel
			}
		}
		if datRel != "" || extRel != "" || ex2Rel != "" {
			return datRel, extRel, ex2Rel
		}
	}
	ap := tr.AnalysisPath
	if ap == "" {
		ap = fallbackAnalysisPath
	}
	if ap == "" {
		return "", "", ""
	}
	base := strings.TrimSuffix(ap, path.Ext(ap))
	return base + ".DAT", base + ".EXT", base + ".2EX"
}

func hasPWV7Waveform(a *rekordbox.Analysis) bool {
	if a == nil {
		return false
	}
	for i := range a.Waveforms {
		if a.Waveforms[i].Tag == "PWV7" && len(a.Waveforms[i].Data) > 0 {
			return true
		}
	}
	return false
}

func (b *OnDemandBuilder) buildOnce(ctx context.Context, req OnDemandRequest) (*OnDemandResult, error) {
	tStart := time.Now()
	var timing OnDemandTiming

	t0 := time.Now()
	snap, res, err := b.loadSnapshot(req.Mount)
	timing.SnapshotMS = float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return nil, err
	}
	if req.UUID != "" && snap.LibraryUUID != req.UUID {
		return nil, ErrOnDemandUUIDMismatch
	}
	tr, ok := snap.TrackAt(req.EnginePath)
	if !ok || tr == nil || tr.Native {
		return nil, ErrOnDemandNotFound
	}
	if req.EngineID > 0 && tr.EngineID > 0 && tr.EngineID != req.EngineID {
		return nil, ErrOnDemandNotFound
	}

	var expTrack rekordbox.Track
	var hasExpTrack bool
	ensureExport := func() {
		if hasExpTrack {
			return
		}
		te := time.Now()
		et, found, _ := b.lookupExportTrack(ctx, req.Mount, res, tr.Key)
		timing.ExportMS += float64(time.Since(te).Microseconds()) / 1000.0
		timing.UsedExport = true
		if found {
			expTrack, hasExpTrack = et, true
		}
	}

	// Resolve audio path on the stick (direct stat first).
	tStat := time.Now()
	audioRel := tr.Rel
	if audioRel == "" {
		audioRel = req.EnginePath
	}
	audioFull, err := resolveVolumeFile(ctx, req.Mount, res, audioRel)
	timing.StatMS = float64(time.Since(tStat).Microseconds()) / 1000.0
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: audio missing: %v", ErrOnDemandDegraded, err)
		}
		return nil, err
	}

	// Determine ANLZ sibling paths; fall back to export DB only when snapshot lacks AnalysisPath.
	datRel, extRel, ex2Rel := anlzCandidates(tr, "")
	if datRel == "" && extRel == "" && ex2Rel == "" {
		ensureExport()
		if hasExpTrack && expTrack.AnalysisPath != "" {
			datRel, extRel, ex2Rel = anlzCandidates(tr, expTrack.AnalysisPath)
		}
	}
	if datRel == "" && extRel == "" && ex2Rel == "" {
		return nil, fmt.Errorf("%w: no analysis path for %s", ErrOnDemandDegraded, tr.Key)
	}

	// Read ANLZ files unpaced: .2EX first (PWV7 only, skipping PWV6); if PWV7 is present, skip
	// all waveform bodies in .DAT and .EXT (reading only PQTZ beatgrid and PCOB/PCO2 cues/loops).
	tANLZ := time.Now()
	var a2ex, aext, adat *rekordbox.Analysis
	readCount := 0
	readOne := func(rel string, skip func(string) bool) (*rekordbox.Analysis, error) {
		if rel == "" {
			return nil, nil
		}
		f, err := resolveANLZOpen(ctx, req.Mount, res, rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		defer f.Close()
		a, n, err := rekordbox.ReadAnalysisFile(f, skip)
		timing.ANLZBytes += n
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: anlz %s: %v", ErrOnDemandDegraded, rel, err)
		}
		readCount++
		return a, nil
	}

	if a2ex, err = readOne(ex2Rel, func(tag string) bool { return tag != "PWV7" }); err != nil {
		return nil, err
	}
	var otherSkip func(string) bool
	if hasPWV7Waveform(a2ex) {
		otherSkip = func(string) bool { return true }
	}
	if adat, err = readOne(datRel, otherSkip); err != nil {
		return nil, err
	}
	if aext, err = readOne(extRel, otherSkip); err != nil {
		return nil, err
	}
	timing.ANLZReadMS = float64(time.Since(tANLZ).Microseconds()) / 1000.0
	if readCount == 0 {
		return nil, fmt.Errorf("%w: no ANLZ files found for %s", ErrOnDemandDegraded, tr.Key)
	}

	// Merge in the exact same order as enginedb.BuildTrack: .DAT -> .EXT -> .2EX.
	merged := &rekordbox.Analysis{}
	for _, part := range []*rekordbox.Analysis{adat, aext, a2ex} {
		if part != nil {
			merged.Merge(part)
		}
	}

	// Read audio header for rule 4 start offset, sample rate, sample count, and duration.
	tAud := time.Now()
	audio, err := compat.AudioHeader(audioFull)
	timing.AudioHeaderMS = float64(time.Since(tAud).Microseconds()) / 1000.0
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: audio header: %v", ErrOnDemandDegraded, err)
	}

	rt := rekordbox.Track{
		ID:       uint32(tr.EngineID),
		Path:     "/" + audioRel,
		Duration: float64(req.EngineLen),
		BPM:      req.EngineBPM,
	}
	if req.EngineKey >= 0 && req.EngineKey < len(engineKeyToCamelot) {
		rt.Key = engineKeyToCamelot[req.EngineKey]
	}
	if hasExpTrack {
		if rt.Key == "" {
			rt.Key = expTrack.Key
		}
		if rt.BPM == 0 {
			rt.BPM = expTrack.BPM
		}
		if expTrack.SampleRate > 0 {
			rt.SampleRate = expTrack.SampleRate
		}
		if expTrack.Duration > 0 {
			rt.Duration = expTrack.Duration
		}
		if len(expTrack.Cues) > 0 {
			merged.Merge(&rekordbox.Analysis{Cues: expTrack.Cues})
		}
	}

	// Implicit AAC SBR: core rate <= 24 kHz in header may double in export metadata (trackbuild.go:239).
	if audio.Format == "aac" && audio.SampleRate > 0 && audio.SampleRate <= 24000 && !hasExpTrack {
		ensureExport()
		if hasExpTrack {
			if expTrack.SampleRate > 0 {
				rt.SampleRate = expTrack.SampleRate
			}
			if expTrack.Duration > 0 {
				rt.Duration = expTrack.Duration
			}
			if rt.Key == "" {
				rt.Key = expTrack.Key
			}
			if len(expTrack.Cues) > 0 {
				merged.Merge(&rekordbox.Analysis{Cues: expTrack.Cues})
			}
		}
	}

	if audio.Format == "aac" && rt.SampleRate > 0 && rt.SampleRate != audio.SampleRate {
		// Implicit SBR: keep export sample rate/duration; priming tag is in decoded samples.
		rt.StartOffsetSamples = audio.RekordboxOffset()
	} else if audio.SampleRate > 0 {
		rt.SampleRate = audio.SampleRate
		if audio.Samples > 0 {
			rt.SampleCount = audio.Samples
		}
		if audio.Duration > 0 {
			rt.Duration = audio.Duration
		}
		rt.StartOffsetSamples = audio.RekordboxOffset()
	}

	// Preserve OneLibrary-only cues recorded in snapshot FastCues/FastLoops without touching export DB.
	if !hasExpTrack && len(tr.FastCues) > 0 {
		mergeFastCues(merged, tr, rt.SampleRate, rt.SampleCount, rt.Duration)
	}

	// CF049: clamp misparsed 500 BPM tracks and grids identically to enginedb.BuildTrack.
	if rt.BPM >= 499.5 {
		rt.BPM = 0
	}
	if len(merged.Beats) > 0 && merged.Beats[0].BPM >= 499.5 {
		merged.Beats = nil
	}
	if rt.BPM == 0 && len(merged.Beats) > 0 {
		rt.BPM = merged.Beats[0].BPM
	}

	tEnc := time.Now()
	perf, err := enginedb.EncodePerformance(rt, merged)
	if err != nil {
		return nil, fmt.Errorf("%w: encode performance: %v", ErrOnDemandDegraded, err)
	}
	if !perf.Complete || len(perf.TrackData) == 0 || len(perf.Beats) == 0 {
		return nil, fmt.Errorf("%w: incomplete performance data for %s", ErrOnDemandDegraded, tr.Key)
	}
	written := enginedb.WrittenDigest(perf.Beats, perf.QuickCues, perf.Loops)
	writtenNoMain := writtenDigestIgnoringMainCueTail(perf.Beats, perf.QuickCues, perf.Loops)
	rbxa1 := rbstate.EncodePerf(rbstate.Perf{
		TrackData:   perf.TrackData,
		Overview:    perf.Overview,
		Beats:       perf.Beats,
		QuickCues:   perf.QuickCues,
		Loops:       perf.Loops,
		ActiveLoops: perf.ActiveLoops,
		Complete:    perf.Complete,
	})
	timing.EncodeMS = float64(time.Since(tEnc).Microseconds()) / 1000.0
	timing.TotalMS = float64(time.Since(tStart).Microseconds()) / 1000.0

	return &OnDemandResult{
		RBXA1:         rbxa1,
		Key:           tr.Key,
		Written:       written,
		WrittenNoMain: writtenNoMain,
		RowHash:       tr.Row,
		ActiveLoops:   perf.ActiveLoops,
		Rules:         rbstate.Rules,
		Timing:        timing,
	}, nil
}

// mergeFastCues recovers OneLibrary-only cues from the snapshot's FastCues/FastLoops blobs when
// exportLibrary.db held cues not present in ANLZ files (zero stick I/O).
func mergeFastCues(a *rekordbox.Analysis, tr *rbstate.Track, sr int, sampleCount uint64, duration float64) {
	if sr <= 0 {
		sr = 44100
	}
	if sampleCount == 0 {
		if duration > 0 {
			sampleCount = uint64(math.Round(duration * float64(sr)))
		} else {
			sampleCount = uint64(sr) * 3600
		}
	}
	// Build a minimal valid trackData header for ParseDeckCuesAndLoops.
	raw := make([]byte, 68)
	binary.BigEndian.PutUint64(raw[0:], math.Float64bits(float64(sr)))
	binary.BigEndian.PutUint64(raw[8:], sampleCount)
	td, err := enginedb.QCompress(raw)
	if err != nil {
		return
	}
	deck, err := enginedb.ParseDeckCuesAndLoops(td, tr.FastCues, tr.FastLoops, int64(tr.FastActive))
	if err != nil {
		return
	}
	hasHot := [9]bool{}
	hasMem := false
	for _, c := range a.Cues {
		if c.HotCue >= 1 && c.HotCue <= 8 {
			hasHot[c.HotCue] = true
		} else if c.HotCue == 0 {
			hasMem = true
		}
	}
	var extra []rekordbox.Cue
	for slot := 0; slot < 8; slot++ {
		if hasHot[slot+1] {
			continue
		}
		if hc := deck.HotCues[slot]; hc != nil {
			cue := rekordbox.Cue{
				HotCue:   uint32(slot + 1),
				TimeMS:   uint32(math.Round(hc.Samples * 1000 / float64(sr))),
				Name:     hc.Name,
				Color:    [4]byte{hc.Color[1], hc.Color[2], hc.Color[3], hc.Color[0]},
				Extended: true,
			}
			if lp := deck.Loops[slot]; lp != nil && math.Abs(lp.StartSamples-hc.Samples) <= float64(sr)/200 {
				cue.Loop = true
				cue.EndMS = uint32(math.Round(lp.EndSamples * 1000 / float64(sr)))
				cue.Active = lp.Active
			}
			extra = append(extra, cue)
		}
	}
	if !hasMem && deck.MainCue >= 0 {
		extra = append(extra, rekordbox.Cue{
			HotCue:   0,
			TimeMS:   uint32(math.Round(deck.MainCue * 1000 / float64(sr))),
			Extended: true,
		})
	}
	if len(extra) > 0 {
		a.Merge(&rekordbox.Analysis{Cues: extra})
	}
}
