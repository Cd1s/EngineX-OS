package webui

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

var streamOverviewDir = "/run/enginex-stream-overview"

const (
	streamOverviewMaxEntries = 64
	streamOverviewBudget     = 90 * time.Second
)

// DropStaleStreamOverviews clears sidecars left by an earlier build of
// stream-sync (/run survives daemon restarts across binary swaps).
func DropStaleStreamOverviews(build string) {
	stamp := filepath.Join(streamOverviewDir, "build")
	if old, err := os.ReadFile(stamp); err == nil && build != "" && string(old) == build {
		return
	}
	entries, _ := os.ReadDir(streamOverviewDir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(streamOverviewDir, e.Name()))
	}
	if build != "" && os.MkdirAll(streamOverviewDir, 0o755) == nil {
		_ = os.WriteFile(stamp, []byte(build), 0o644)
	}
}

type streamOverviewJob struct {
	id       int64
	provider string
	path     string
}

type streamOverviewQueue struct {
	guard   *resourceguard.Guard
	dir     string
	mu      sync.Mutex
	closed  bool
	running bool
	active  string
	cancel  context.CancelFunc
	pending *streamOverviewJob
	wg      *sync.WaitGroup
}

func newStreamOverviewQueue(guard *resourceguard.Guard, dir string, wg *sync.WaitGroup) *streamOverviewQueue {
	return &streamOverviewQueue{guard: guard, dir: dir, wg: wg}
}

func (q *streamOverviewQueue) enqueue(job streamOverviewJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || job.path == "" {
		return
	}
	// Deduplicate playbackInfo + stream back-to-back calls for the same audio file.
	if q.active == job.path || (q.pending != nil && q.pending.path == job.path) {
		return
	}
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.pending = &job
	if q.running {
		return
	}
	q.running = true
	q.wg.Add(1)
	go q.drain()
}

func (q *streamOverviewQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.pending = nil
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
	q.mu.Unlock()
}

func (q *streamOverviewQueue) drain() {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if q.closed || q.pending == nil {
			q.active = ""
			q.cancel = nil
			q.running = false
			q.mu.Unlock()
			return
		}
		job := *q.pending
		q.pending = nil
		ctx, cancel := context.WithTimeout(context.Background(), streamOverviewBudget)
		q.active = job.path
		q.cancel = cancel
		q.mu.Unlock()

		q.runOne(ctx, job)
		cancel()

		q.mu.Lock()
		if q.pending == nil {
			q.active = ""
			q.cancel = nil
		}
		q.mu.Unlock()
	}
}

func (q *streamOverviewQueue) runOne(ctx context.Context, job streamOverviewJob) {
	started := time.Now()
	fp, err := analyzer.OverviewFileFingerprint(job.path)
	if err != nil {
		log.Printf("stream-sync: stream-overview track=%d provider=%s result=failed error=%v", job.id, job.provider, err)
		return
	}
	dest := filepath.Join(q.dir, fp+".ov")
	if _, err := os.Stat(dest); err == nil {
		log.Printf("stream-sync: stream-overview track=%d provider=%s result=skip reason=cached", job.id, job.provider)
		return
	}
	var ov *analyzer.OverviewPeaks
	if q.guard != nil {
		err = q.guard.Run(ctx, func(t *resourceguard.Throttle) error {
			var cerr error
			ov, cerr = analyzer.ComputeOverviewPeaks(ctx, job.path, t)
			return cerr
		})
	} else {
		ov, err = analyzer.ComputeOverviewPeaks(ctx, job.path, nil)
	}
	elapsed := time.Since(started).Round(time.Millisecond)
	if errors.Is(err, analyzer.ErrUnsupportedOverviewFormat) {
		log.Printf("stream-sync: stream-overview track=%d provider=%s result=unsupported elapsed=%s", job.id, job.provider, elapsed)
		return
	}
	if err != nil {
		log.Printf("stream-sync: stream-overview track=%d provider=%s result=failed elapsed=%s error=%v", job.id, job.provider, elapsed, err)
		return
	}
	if _, err := analyzer.WriteOverviewSidecar(q.dir, fp, ov, streamOverviewMaxEntries); err != nil {
		log.Printf("stream-sync: stream-overview track=%d provider=%s result=failed elapsed=%s error=%v", job.id, job.provider, elapsed, err)
		return
	}
	log.Printf("stream-sync: stream-overview track=%d provider=%s format=%s elapsed=%s trailer=%v result=ready",
		job.id, job.provider, ov.Format, elapsed, ov.Trailer)
}

func (s *Server) enqueueStreamOverview(id int64, provider, audioPath string) {
	if s == nil || audioPath == "" || provider == rekordboxScope || provider == localProvider {
		return
	}
	s.streamOvMu.Lock()
	select {
	case <-s.closed:
		s.streamOvMu.Unlock()
		return
	default:
	}
	if s.streamOv == nil {
		var guard *resourceguard.Guard
		if s.jobs != nil {
			guard = s.jobs.p.Guard
		}
		s.streamOv = newStreamOverviewQueue(guard, streamOverviewDir, &s.streamOvWG)
	}
	q := s.streamOv
	s.streamOvMu.Unlock()
	q.enqueue(streamOverviewJob{id: id, provider: provider, path: audioPath})
}

func (s *Server) stopStreamOverview() {
	s.streamOvMu.Lock()
	q := s.streamOv
	s.streamOvMu.Unlock()
	if q != nil {
		q.close()
	}
	s.streamOvWG.Wait()
}
