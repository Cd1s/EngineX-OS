package webui

// Source-only background detail generation. Neither this worker nor the display
// receiver invokes Engine's analyzer or writes any Engine library.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"golang.org/x/sys/unix"
)

type shadowSourceEntry struct {
	asset          *shadowDisplayAsset
	path, key, req string
	queued         bool
	working        bool
	at             time.Time
	// The worker's place in the file, for the receiver's progress ring.
	read, size atomic.Int64
}

type shadowSourceCache struct {
	mu      sync.Mutex
	entries map[string]*shadowSourceEntry
	running bool
}

func sameSourcePath(a, b, dataRoot string) bool {
	if a == b {
		return true
	}
	oa, oka := inMusicTempOrig(a, dataRoot)
	ob, okb := inMusicTempOrig(b, dataRoot)
	return oka && okb && oa == ob
}

// sourceDisplayAsset is the display data of one of this player's tracks. With
// detail it queues the detail waveform; without (Engine mode's cover) it is
// read from the library as it is, and neither queued nor kept.
func (s *Server) sourceDisplayAsset(ctx context.Context, path, key string, detail bool) (*shadowDisplayAsset, error) {
	stableKey := stableTrackKey(key)
	c := &s.shadow.sourceDisplay
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*shadowSourceEntry{}
	}
	if e := c.entries[stableKey]; e != nil && sameSourcePath(e.path, path, s.shadow.dataRoot) {
		e.at = time.Now()
		if size := e.size.Load(); e.asset.DetailPending && size > 0 {
			a := *e.asset // the cached asset stays immutable
			a.Key = key
			a.DetailProgress = min(1, math.Round(float64(e.read.Load())/float64(size)*100)/100)
			c.mu.Unlock()
			return &a, nil
		}
		if e.req == "" || e.req == key || !detail || e.asset.DetailPending {
			a := e.asset
			if a.Key != "" && a.Key != key {
				copyAsset := *a
				copyAsset.Key = key
				a = &copyAsset
			}
			c.mu.Unlock()
			return a, nil
		}
		// New load session for a previously computed track: reuse the Waveform
		// envelope without re-decoding audio, refreshing lightweight m.db metadata.
		cachedWave := e.asset.Waveform
		cachedSource := e.asset.DetailSource
		c.mu.Unlock()
		fresh, err := libraryDisplay(ctx, path, s.shadow.dataRoot, s.shadow.mediaRoot)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil && c.entries[stableKey] == e {
			fresh.Key, fresh.Version = key, 1
			if cachedWave != nil {
				fresh.Waveform = cachedWave
				fresh.Detail = true
				fresh.DetailSource = cachedSource
			}
			fresh.DetailPending = false
			e.asset = fresh
			e.path = path
			e.req = key
			e.at = time.Now()
			return fresh, nil
		}
		if e := c.entries[stableKey]; e != nil {
			copyAsset := *e.asset
			copyAsset.Key = key
			return &copyAsset, nil
		}
	}
	if !detail {
		c.mu.Unlock()
		a, err := libraryDisplay(ctx, path, s.shadow.dataRoot, s.shadow.mediaRoot)
		if err == nil {
			a.Key, a.Version = key, 1
		}
		return a, err
	}
	if len(c.entries) >= 16 {
		oldest := ""
		for k, e := range c.entries {
			if !e.queued && !e.working && (oldest == "" || e.at.Before(c.entries[oldest].at)) {
				oldest = k
			}
		}
		if oldest == "" {
			c.mu.Unlock()
			return nil, fmt.Errorf("display source busy")
		}
		delete(c.entries, oldest)
	}
	c.mu.Unlock()
	a, err := libraryDisplay(ctx, path, s.shadow.dataRoot, s.shadow.mediaRoot)
	if err != nil {
		return nil, err
	}
	a.Key, a.Version = key, 1
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[stableKey]; e != nil && sameSourcePath(e.path, path, s.shadow.dataRoot) {
		e.at = time.Now()
		copyAsset := *e.asset
		copyAsset.Key = key
		return &copyAsset, nil
	}
	e := &shadowSourceEntry{asset: a, path: path, key: stableKey, req: key, at: time.Now()}
	if a.Waveform == nil {
		a.DetailPending = true
		e.queued = true
	}
	c.entries[stableKey] = e
	if e.queued && !c.running {
		c.running = true
		go s.shadowSourceWorker()
	}
	return a, nil
}

func (s *Server) shadowSourceLoaded(key string) bool {
	select {
	case <-s.closed:
		return false
	default:
	}
	stable := stableTrackKey(key)
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	for _, d := range s.shadow.own {
		if (d.Key == key || stableTrackKey(d.Key) == stable) && d.Path != "" {
			return true
		}
	}
	for _, p := range s.shadow.peerDecks { // a peer's track on our own drive
		if c := s.shadow.peerLocal[p.Net]; p.Key == "" && c.file != "" && (c.key == key || stableTrackKey(c.key) == stable) {
			return true
		}
	}
	return false
}

type shadowSourceTicker struct {
	s        *Server
	job      *shadowSourceEntry
	key      string
	window   time.Time
	deadline time.Time
}

// Progress is analyzer.ShadowProgress: the bytes of the file read so far.
func (t *shadowSourceTicker) Progress(read int64) { t.job.read.Store(read) }

func (t *shadowSourceTicker) Tick() error {
	if time.Now().After(t.deadline) {
		return context.DeadlineExceeded
	}
	if !t.s.shadowSourceLoaded(t.key) {
		return context.Canceled
	}
	// At most a 5 ms work burst followed by 15 ms yielded time: <=25% of one
	// core on average, with bounded checks in the streaming decoder.
	if time.Since(t.window) >= 5*time.Millisecond {
		timer := time.NewTimer(15 * time.Millisecond)
		select {
		case <-t.s.closed:
			timer.Stop()
			return context.Canceled
		case <-timer.C:
		}
		t.window = time.Now()
	}
	return nil
}

func (s *Server) shadowSourceWorker() {
	// Linux nice is per thread. Returning while locked terminates this worker
	// thread, so a niced thread can never return to the general Go scheduler.
	runtime.LockOSThread()
	_ = unix.Setpriority(unix.PRIO_PROCESS, unix.Gettid(), 19)
	var allowed unix.CPUSet
	if unix.SchedGetaffinity(unix.Gettid(), &allowed) == nil {
		for cpu := 1023; cpu >= 0; cpu-- {
			if allowed.IsSet(cpu) {
				var background unix.CPUSet
				background.Set(cpu)
				_ = unix.SchedSetaffinity(unix.Gettid(), &background)
				break
			}
		}
	}
	c := &s.shadow.sourceDisplay
	for {
		c.mu.Lock()
		var job *shadowSourceEntry
		for _, e := range c.entries {
			if e.queued {
				job = e
				break
			}
		}
		if job == nil {
			c.running = false
			c.mu.Unlock()
			return
		}
		job.queued = false
		job.working = true
		c.mu.Unlock()
		ticker := &shadowSourceTicker{s: s, job: job, key: job.key, window: time.Now(), deadline: time.Now().Add(10 * time.Minute)}
		if st, err := os.Stat(job.path); err == nil {
			job.size.Store(st.Size())
		}
		var envelope *analyzer.ShadowEnvelope
		err := ticker.Tick()
		if err == nil {
			// PCM full scale for every band. Stretched to trackData's own band peaks
			// (0.63/0.36/0.19 on a real track) the three bands stacked past the lane
			// edge on 60 % of the bins: a lane filled top to bottom, no transients.
			envelope, err = analyzer.ShadowWaveformFile(job.path, [3]float64{1, 1, 1}, ticker)
		}
		copyAsset := *job.asset
		copyAsset.DetailPending = false
		if err == nil && envelope != nil && envelope.SampleRate > 0 && len(envelope.Bands) > 0 {
			// Use source sample coordinates for both grid and waveform. MP3 gapless
			// padding/unsupported file changes must not produce a displaced image.
			duration := float64(len(envelope.Bands)/3) * envelope.SamplesPerBin / float64(envelope.SampleRate) * 1000
			if duration-copyAsset.DurationMS < 100 && copyAsset.DurationMS-duration < 100 {
				w := enginedb.DisplayWave{Bands: envelope.Bands, SamplesPerBin: envelope.SamplesPerBin * copyAsset.SampleRate / float64(envelope.SampleRate)}
				copyAsset.Waveform = &w
				copyAsset.Detail = true
				copyAsset.DetailSource = "source-envelope"
			}
		}
		c.mu.Lock()
		if c.entries[job.key] == job {
			job.working = false
			if errors.Is(err, context.Canceled) {
				delete(c.entries, job.key)
			} else {
				job.asset = &copyAsset
			}
		}
		c.mu.Unlock()
		s.shadow.dbg.event("source-envelope", map[string]any{"ok": copyAsset.Detail, "key": job.key})
	}
}
