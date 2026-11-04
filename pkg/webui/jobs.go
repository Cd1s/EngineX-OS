package webui

import (
	"context"
	"errors"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

const (
	maxAnalysedMS    = 15 * 60 * 1000
	maxAnalysedBytes = 50 << 20
	maxJobFiles      = 500
)

func tooLongToAnalyse(t providers.Track, path string) bool {
	if t.DurationMS > maxAnalysedMS || strings.HasSuffix(path, ".mp4") {
		return true
	}
	st, err := os.Stat(path)
	return err == nil && st.Size() > maxAnalysedBytes && strings.HasSuffix(path, ".mp3")
}

// Stores' editions of one recording differ by their fades.
const maxDurationGapMS = 15000

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

	// Set when the TIDAL gateway already downloaded the file for playback.
	matched providers.Track
	dl      *deemixclient.Download
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
	files   map[string]fetched
	fetches map[string]chan struct{}

	// Playlists each streamed track was queued for, so a guest playing a
	// track the owner already downloaded still gets it in the guest playlist.
	queued map[string]bool

	// netease returns the NetEase login a session may fetch audio with, or nil.
	netease func(session string, guest bool) *providers.Netease
	// neteaseDirect reports whether NetEase tracks skip Deemix; nil tries Deemix first.
	neteaseDirect func() bool
	soundcloud    func() *providers.SoundCloud
	bpm           *providers.BPMStore
}
type fetched struct {
	matched providers.Track
	dl      *deemixclient.Download
}

func NewJobs(p Pipeline) *Jobs {
	if p.Guard == nil {
		p.Guard = resourceguard.New(resourceguard.DefaultConfig())
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Jobs{p: p, ctx: ctx, cancel: cancel, queue: make(chan string, 16), done: make(chan struct{}), items: make(map[string]Job), results: make(map[string]*analyzer.Result), files: make(map[string]fetched), fetches: make(map[string]chan struct{}), queued: make(map[string]bool)}
	go j.worker()
	return j
}
func (j *Jobs) Close() { j.cancel(); <-j.done }
func (j *Jobs) timeout() time.Duration {
	if j.p.Timeout > 0 {
		return j.p.Timeout
	}
	return 30 * time.Minute
}
func trackKey(t providers.Track) string { return t.Provider + ":" + t.ID }

// File returns a finished download of t that is still on disk.
func (j *Jobs) File(t providers.Track) string {
	j.mu.Lock()
	path := ""
	if f, ok := j.files[trackKey(t)]; ok {
		path = f.dl.Path
	}
	j.mu.Unlock()
	if st, err := os.Stat(path); path == "" || err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return path
}

// Fetch downloads t for immediate playback and queues CPU3 analysis and import
// of the same file. Playback never waits for analysis. Concurrent requests for
// one track share a download; it continues if the waiting caller gives up.
func (j *Jobs) Fetch(wait context.Context, session string, t providers.Track, guest bool) (string, error) {
	if j.p.Catalog == nil || j.p.Deemix == nil {
		return "", errors.New("pipeline is not configured")
	}
	key := trackKey(t)
	for {
		// Check the cache and claim the download under one lock so a fetch
		// finishing in between cannot trigger a second download.
		j.mu.Lock()
		f, done := j.files[key]
		busy, ok := j.fetches[key]
		if !ok && !done {
			j.fetches[key] = make(chan struct{})
		}
		j.mu.Unlock()
		if done {
			if st, err := os.Stat(f.dl.Path); err == nil && st.Mode().IsRegular() {
				j.queueImport(session, t, guest, f)
				return f.dl.Path, nil
			}
			j.mu.Lock()
			if j.files[key].dl == f.dl {
				delete(j.files, key)
			}
			j.mu.Unlock()
			continue
		}
		if !ok {
			break
		}
		select {
		case <-busy:
		case <-wait.Done():
			return "", wait.Err()
		}
	}
	defer func() {
		j.mu.Lock()
		close(j.fetches[key])
		delete(j.fetches, key)
		j.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(j.ctx, j.timeout())
	defer cancel()
	matched, dl, err := j.acquire(ctx, session, t, guest, nil)
	if err != nil {
		return "", err
	}
	j.addCover(ctx, t, dl)
	f := fetched{matched, dl}
	j.putFile(key, f)
	j.queueImport(session, t, guest, f)
	return dl.Path, nil
}

func (j *Jobs) putFile(key string, f fetched) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.files[key]; !ok && len(j.files) >= maxJobFiles {
		for k := range j.files {
			if j.fetches[k] == nil {
				delete(j.files, k)
				break
			}
		}
	}
	j.files[key] = f
}

// acquire tries the deck's own Deemix service first, which costs no NetEase
// download quota, then the session's NetEase login. A NetEase track goes
// straight to NetEase unless the owner chose Deemix first. All audio lands on
// the deck.
func (j *Jobs) acquire(ctx context.Context, session string, t providers.Track, guest bool, progress func(deemixclient.QueueStatus)) (providers.Track, *deemixclient.Download, error) {
	if t.Provider == "soundcloud" {
		dl, err := j.fromSoundCloud(ctx, t)
		return t, dl, err
	}
	j.mu.Lock()
	lookup, direct := j.netease, j.neteaseDirect
	j.mu.Unlock()
	var failures []error
	if t.Provider == "netease" {
		if dl := j.p.Deemix.Cached("netease-" + t.ID); dl != nil {
			return t, dl, nil
		}
	}
	if t.Provider != "netease" || direct == nil || !direct() {
		matched, err := deemixclient.Resolve(ctx, j.p.Catalog, t)
		if err == nil {
			var dl *deemixclient.Download
			if dl, err = j.p.Deemix.Download(ctx, matched, progress); err == nil {
				j.p.Deemix.Trim(dl.Path)
				return matched, dl, nil
			}
		}
		failures = append(failures, err)
	}
	var n *providers.Netease
	if lookup != nil {
		n = lookup(session, guest)
	}
	if n == nil {
		if len(failures) == 0 {
			return providers.Track{}, nil, errors.New("netease: log in to NetEase Cloud Music on the deck first")
		}
		return providers.Track{}, nil, errors.Join(failures...)
	}
	matched, dl, err := j.fromNetease(ctx, n, t)
	if err == nil {
		return matched, dl, nil
	}
	return providers.Track{}, nil, errors.Join(append(failures, err)...)
}

// addCover embeds the artwork, which Engine reads from the file for its screens. A
// missing cover is not worth failing a track over.
func (j *Jobs) addCover(ctx context.Context, t providers.Track, dl *deemixclient.Download) {
	if t.CoverURL == "" {
		return
	}
	if err := j.p.Deemix.EmbedCover(ctx, dl.Path, t.CoverURL); err != nil {
		log.Printf("stream-sync: no cover for track %s: %v", t.ID, err)
	}
}

// fromSoundCloud follows the deck's Streaming Quality. 320kbps takes the stream
// (SoundCloud's own MP3/AAC, the smallest file) straight away. Lossless first
// tries the uploader's original, then falls back to that stream.
func (j *Jobs) fromSoundCloud(ctx context.Context, t providers.Track) (*deemixclient.Download, error) {
	name := "soundcloud-" + t.ID
	lossless := j.p.Deemix.Lossless()
	cached := j.p.Deemix.Cached(name)
	if cached != nil && (cached.Quality.Lossless || !lossless) {
		return cached, nil
	}
	j.mu.Lock()
	lookup := j.soundcloud
	j.mu.Unlock()
	var s *providers.SoundCloud
	if lookup != nil {
		s = lookup()
	}
	if s == nil {
		if cached != nil {
			return cached, nil
		}
		return nil, errors.New("soundcloud: not connected")
	}
	media, err := s.Media(ctx, t.ID, lossless)
	if err != nil {
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	if media.Original != "" {
		dl, err := j.p.Deemix.SaveSplit(ctx, name, media.Original)
		if err == nil {
			return dl, nil
		}
		log.Printf("stream-sync: SoundCloud original of track %s unusable, using the stream: %v", t.ID, err)
		if cached != nil {
			return cached, nil
		}
		if len(media.URLs) == 0 {
			return nil, err
		}
	}
	if cached != nil {
		// Lossless was asked for, but there is no original: the stream on disk is it.
		return cached, nil
	}
	if media.Ext == "mp3" && len(media.URLs) == 1 {
		return j.p.Deemix.SaveSplit(ctx, name, media.URLs[0])
	}
	return j.p.Deemix.SaveParts(ctx, name, media.Ext, media.URLs)
}

// A NetEase upload of another service's track may differ by fades of a few seconds.
const neteaseMatchToleranceMS = 5000

func (j *Jobs) fromNetease(ctx context.Context, n *providers.Netease, t providers.Track) (providers.Track, *deemixclient.Download, error) {
	if t.Provider != "netease" {
		primary := t.Artist
		if len(t.Artists) > 0 {
			primary = t.Artists[0]
		}
		found, err := n.Search(ctx, strings.TrimSpace(strings.Split(primary, ",")[0]+" "+deemixclient.SearchTitle(t.Title)))
		if err != nil {
			return providers.Track{}, nil, err
		}
		if t, err = deemixclient.MatchNearby(t, found, neteaseMatchToleranceMS); err != nil {
			return providers.Track{}, nil, err
		}
	}
	name := "netease-" + t.ID
	if dl := j.p.Deemix.Cached(name); dl != nil {
		return t, dl, nil
	}
	address, err := n.AudioURL(ctx, t.ID, j.p.Deemix.Lossless())
	if err != nil {
		return providers.Track{}, nil, err
	}
	dl, err := j.p.Deemix.Save(ctx, name, address)
	if err != nil {
		return providers.Track{}, nil, err
	}
	return t, dl, nil
}

// queueImport analyses and imports a streamed file once per target playlist.
// A full queue only skips library import; the file is still playable.
func (j *Jobs) queueImport(session string, t providers.Track, guest bool, f fetched) {
	// A DJ set is played, not analysed: decoding hours of audio would exhaust memory.
	if tooLongToAnalyse(t, f.dl.Path) {
		return
	}
	key := providers.OwnerPlaylist + "\x00" + trackKey(t)
	if guest {
		key = providers.GuestPlaylist + "\x00" + trackKey(t)
	}
	j.mu.Lock()
	seen := j.queued[key]
	if !seen && len(j.queued) >= maxJobFiles {
		for k := range j.queued {
			delete(j.queued, k)
			break
		}
	}
	j.queued[key] = true
	j.mu.Unlock()
	if !seen {
		if _, err := j.submit(session, t, guest, f.matched, f.dl); err != nil {
			j.mu.Lock()
			delete(j.queued, key)
			j.mu.Unlock()
		}
	}
}

// Lossy reports whether Deemix may deliver MP3 instead of FLAC.
func (j *Jobs) Lossy() bool { return j.p.Deemix != nil && j.p.Deemix.AllowLossy }
func (j *Jobs) Submit(session string, t providers.Track, guest bool) (Job, error) {
	return j.submit(session, t, guest, providers.Track{}, nil)
}
func (j *Jobs) submit(session string, t providers.Track, guest bool, matched providers.Track, dl *deemixclient.Download) (Job, error) {
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
	job := Job{ID: providers.RandomID(), SessionID: session, Track: t, Playlist: playlist, State: "queued", matched: matched, dl: dl}
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
			ctx, cancel := context.WithTimeout(j.ctx, j.timeout())
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
	matched, dl := v.matched, v.dl
	if dl == nil {
		var err error
		j.update(v.ID, func(v *Job) { v.State = "downloading"; v.Progress = .05 })
		matched, dl, err = j.acquire(ctx, v.SessionID, v.Track, v.Playlist == providers.GuestPlaylist, func(q deemixclient.QueueStatus) {
			j.update(v.ID, func(v *Job) { v.Progress = .05 + .55*max(0, min(1, q.Progress)) })
		})
		if err != nil {
			return err
		}
		j.addCover(ctx, v.Track, dl)
		j.putFile(trackKey(v.Track), fetched{matched, dl})
	}
	if tooLongToAnalyse(v.Track, dl.Path) {
		j.update(v.ID, func(v *Job) { v.State = "done"; v.Quality = dl.Quality.Label; v.Progress = 1 })
		return nil
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
	if matched.DurationMS > 0 && math.Abs(res.Duration*1000-float64(matched.DurationMS)) > maxDurationGapMS {
		return errors.New("download duration differs from matched recording by more than 15 seconds")
	}
	// What the deck measured replaces the preview shown in lists.
	if j.bpm != nil {
		j.bpm.Put(v.Track, providers.BPMInfo{BPM: res.BPM, Key: res.Key.Camelot, Measured: true})
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
