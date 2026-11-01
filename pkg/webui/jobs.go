package webui

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/analyzer"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

type Importer interface {
	Import(context.Context, analyzer.Import) (int64, error)
}
type Job struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Track     providers.Track `json:"track"`
	Playlist  string          `json:"playlist"`
	State     string          `json:"state"`
	Progress  float64         `json:"progress"`
	Quality   string          `json:"quality,omitempty"`
	Error     string          `json:"error,omitempty"`
	TrackID   int64           `json:"trackId,omitempty"`
}
type Pipeline struct {
	Guard   *resourceguard.Guard
	Catalog deemixclient.Catalog
	Deemix  *deemixclient.Client
	Library Importer
	Timeout time.Duration
}
type Jobs struct {
	p       Pipeline
	ctx     context.Context
	cancel  context.CancelFunc
	queue   chan string
	done    chan struct{}
	mu      sync.Mutex
	items   map[string]Job
	order   []string
	results map[string]*analyzer.Result
}

func NewJobs(p Pipeline) *Jobs {
	if p.Guard == nil {
		p.Guard = resourceguard.New(resourceguard.DefaultConfig())
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Jobs{p: p, ctx: ctx, cancel: cancel, queue: make(chan string, 16), done: make(chan struct{}), items: make(map[string]Job), results: make(map[string]*analyzer.Result)}
	go j.worker()
	return j
}
func (j *Jobs) Close() { j.cancel(); <-j.done }
func (j *Jobs) Submit(session string, t providers.Track, guest bool) (Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.ctx.Err() != nil {
		return Job{}, errors.New("worker stopped")
	}
	// Bound retained history and reject duplicate active downloads.
	for _, v := range j.items {
		if v.SessionID == session && v.Track.ID == t.ID && (v.State == "queued" || v.State == "matching" || v.State == "downloading" || v.State == "analyzing" || v.State == "importing") {
			return v, nil
		}
	}
	if len(j.order) >= 100 {
		old := j.order[0]
		s := j.items[old].State
		if s != "done" && s != "error" {
			return Job{}, errors.New("job history full")
		}
		delete(j.items, old)
		delete(j.results, old)
		j.order = j.order[1:]
	}
	playlist := providers.OwnerPlaylist
	if guest {
		playlist = providers.GuestPlaylist
	}
	job := Job{ID: providers.RandomID(), SessionID: session, Track: t, Playlist: playlist, State: "queued"}
	j.items[job.ID] = job
	j.order = append(j.order, job.ID)
	select {
	case j.queue <- job.ID:
		return job, nil
	default:
		delete(j.items, job.ID)
		j.order = j.order[:len(j.order)-1]
		return Job{}, errors.New("download queue full")
	}
}
func (j *Jobs) List() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.order))
	for i := len(j.order) - 1; i >= 0; i-- {
		out = append(out, j.items[j.order[i]])
	}
	return out
}
func (j *Jobs) Result(id string) *analyzer.Result {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.results[id]
}
func (j *Jobs) update(id string, fn func(*Job)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.items[id]
	fn(&v)
	j.items[id] = v
}
func (j *Jobs) worker() {
	defer close(j.done)
	for {
		select {
		case <-j.ctx.Done():
			return
		case id := <-j.queue:
			j.mu.Lock()
			v := j.items[id]
			j.mu.Unlock()
			timeout := j.p.Timeout
			if timeout <= 0 {
				timeout = 30 * time.Minute
			}
			ctx, cancel := context.WithTimeout(j.ctx, timeout)
			err := j.p.Guard.Run(ctx, func(t *resourceguard.Throttle) error { return j.run(ctx, v, t) })
			cancel()
			if err != nil {
				j.update(id, func(v *Job) { v.State = "error"; v.Error = err.Error() })
			}
		}
	}
}
func (j *Jobs) run(ctx context.Context, v Job, t *resourceguard.Throttle) error {
	if j.p.Catalog == nil || j.p.Deemix == nil || j.p.Library == nil {
		return errors.New("pipeline is not configured")
	}
	if err := t.Tick(); err != nil {
		return err
	}
	j.update(v.ID, func(v *Job) { v.State = "matching"; v.Progress = .02 })
	matched, err := deemixclient.Resolve(ctx, j.p.Catalog, v.Track)
	if err != nil {
		return err
	}
	j.update(v.ID, func(v *Job) { v.State = "downloading"; v.Progress = .05 })
	dl, err := j.p.Deemix.Download(ctx, matched, func(q deemixclient.QueueStatus) {
		j.update(v.ID, func(v *Job) { v.Progress = .05 + .55*max(0, min(1, q.Progress)) })
	})
	if err != nil {
		return err
	}
	j.update(v.ID, func(v *Job) { v.State = "analyzing"; v.Quality = dl.Quality.Label; v.Progress = .65 })
	before, err := os.Stat(dl.Path)
	if err != nil {
		return err
	}
	audio, res, err := analyzer.AnalyzeFile(dl.Path, analyzer.Options{Ticker: t})
	if err != nil {
		return err
	}
	if audio.Format != dl.Quality.Format || audio.SampleRate != dl.Quality.SampleRate || (dl.Quality.Lossless && audio.BitsPerSample != dl.Quality.Bits) {
		return errors.New("download format changed after quality inspection")
	}
	if matched.DurationMS > 0 && math.Abs(res.Duration*1000-float64(matched.DurationMS)) > 2000 {
		return errors.New("download duration differs from matched recording by more than 2 seconds")
	}
	if err = t.Tick(); err != nil {
		return err
	}
	j.update(v.ID, func(v *Job) { v.State = "importing"; v.Progress = .95 })
	after, err := os.Stat(dl.Path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("download changed during analysis; retry after the downloader finishes")
	}
	id, err := j.p.Library.Import(ctx, analyzer.Import{Path: dl.Path, Title: v.Track.Title, Artist: v.Track.Artist, Album: v.Track.Album, Comment: dl.Quality.Comment(), Format: audio.Format, Playlist: v.Playlist, Result: res})
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.results[v.ID] = res
	j.mu.Unlock()
	j.update(v.ID, func(v *Job) { v.State = "done"; v.Progress = 1; v.TrackID = id })
	return nil
}
