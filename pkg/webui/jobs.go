package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// sources maps trackKey to the file name it was saved as, kept on disk so a
	// restart finds a Spotify/TIDAL track's Deezer file without asking Deezer.
	sources map[string]string

	// Playlists each streamed track was queued for, so a guest playing a
	// track the owner already downloaded still gets it in the guest playlist.
	queued map[string]bool

	// netease returns the NetEase login a session may fetch audio with, or nil.
	netease func(session string, guest bool) *providers.Netease
	// neteaseDirect reports whether NetEase tracks skip Deemix; nil tries Deemix first.
	neteaseDirect func() bool
	// deezerReady reports whether a Deezer ARL is saved; nil skips the hint.
	deezerReady func() bool
	soundcloud  func() *providers.SoundCloud
	beatport    func() *providers.Beatport
	spotify     func() *providers.Spotify
	selfHosted  func() *providers.SelfHostedProvider
	bpm         *providers.BPMStore
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

// File returns a finished download of t that is still on disk, including one
// saved before a restart.
func (j *Jobs) File(t providers.Track) string {
	key := trackKey(t)
	j.mu.Lock()
	path := ""
	if f, ok := j.files[key]; ok {
		path = f.dl.Path
	} else if name := j.loadSources()[key]; name != "" && j.p.Deemix != nil {
		j.mu.Unlock()
		if !j.reusable(t, name) {
			return ""
		}
		dl := j.p.Deemix.Cached(strings.TrimSuffix(name, filepath.Ext(name)))
		if dl == nil {
			return ""
		}
		// Recently used: Trim evicts by modification time.
		now := time.Now()
		os.Chtimes(dl.Path, now, now)
		j.putFile(key, fetched{t, dl})
		return dl.Path
	} else if j.p.Deemix != nil {
		// A finished provider file may predate the source index (or have been
		// downloaded via Deemix directly). Discover it before knownCodec/Fetch
		// would enter acquisition and synchronously fetch/embed artwork again.
		c := j.p.Deemix
		j.mu.Unlock()
		name := ""
		switch t.Provider {
		case "deezer":
			// Match Download's positive numeric-ID rule without interpreting
			// another provider's numeric ID as a Deezer ID.
			if id, err := strconv.ParseUint(t.ID, 10, 64); err == nil && id > 0 {
				name = t.ID
			}
		case "spotify", "beatport", "netease":
			name = t.Provider + "-" + t.ID
		}
		if name == "" || t.ID == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
			return ""
		}
		if dl := c.Cached(name); dl != nil {
			// Beatport may upgrade an earlier AAC stream to the subscription's
			// lossless file. Leave that decision to fromBeatport, as before.
			if t.Provider == "beatport" && c.Lossless() && !dl.Quality.Lossless {
				return ""
			}
			now := time.Now()
			os.Chtimes(dl.Path, now, now)
			j.putFile(key, fetched{t, dl})
			return dl.Path
		}
		return ""
	}
	j.mu.Unlock()
	if st, err := os.Stat(path); path == "" || err != nil || !st.Mode().IsRegular() || !j.reusable(t, path) {
		return ""
	}
	return path
}

// reusable tells whether an earlier file may stand in for t. Every service keeps
// its own files, except that Deezer and NetEase in Deemix mode share the Deezer
// download (<Deezer ID>.flac). Spotify, Beatport, SoundCloud and NetEase official
// mode only take their own prefix, so a file saved for another service is ignored
// and the track is fetched again from its own service.
func (j *Jobs) reusable(t providers.Track, path string) bool {
	base := filepath.Base(path)
	switch t.Provider {
	case "spotify":
		return strings.HasPrefix(base, "spotify-") && strings.EqualFold(filepath.Ext(base), ".ogg")
	case "beatport", "soundcloud":
		return strings.HasPrefix(base, t.Provider+"-")
	case "netease":
		j.mu.Lock()
		direct := j.neteaseDirect
		j.mu.Unlock()
		return direct == nil || !direct() || strings.HasPrefix(base, "netease-")
	}
	return true
}

// dropUnusable forgets an in-memory download that t may no longer reuse.
func (j *Jobs) dropUnusable(t providers.Track) {
	key := trackKey(t)
	j.mu.Lock()
	f, ok := j.files[key]
	j.mu.Unlock()
	if ok && !j.reusable(t, f.dl.Path) {
		j.mu.Lock()
		if j.files[key].dl == f.dl {
			delete(j.files, key)
		}
		j.mu.Unlock()
	}
}

const sourcesFile = ".sources.json"

// loadSources reads the saved index once; j.mu must be held.
func (j *Jobs) loadSources() map[string]string {
	if j.sources == nil {
		j.sources = map[string]string{}
		if j.p.Deemix != nil {
			if b, err := os.ReadFile(filepath.Join(j.p.Deemix.DownloadDir, sourcesFile)); err == nil {
				json.Unmarshal(b, &j.sources)
			}
		}
	}
	return j.sources
}

// saveSource records key's file and drops entries whose file was evicted; j.mu
// must be held. A lost index only costs one catalog lookup per track.
func (j *Jobs) saveSource(key, path string) {
	dir := j.p.Deemix.DownloadDir
	sources := j.loadSources()
	if sources[key] == filepath.Base(path) {
		return
	}
	// Only files directly in the download directory (paths may be symlink-resolved).
	if a, err := os.Stat(path); err != nil {
		return
	} else if b, err := os.Stat(filepath.Join(dir, filepath.Base(path))); err != nil || !os.SameFile(a, b) {
		return
	}
	sources[key] = filepath.Base(path)
	for k, name := range sources {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			delete(sources, k)
		}
	}
	b, _ := json.Marshal(sources)
	tmp := filepath.Join(dir, sourcesFile+".tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, filepath.Join(dir, sourcesFile))
	}
}

// Fetch downloads t for immediate playback and queues CPU3 analysis and import
// of the same file. Playback never waits for analysis. Concurrent requests for
// one track share a download; it continues if the waiting caller gives up.
func (j *Jobs) Fetch(wait context.Context, session string, t providers.Track, guest bool) (string, error) {
	if j.p.Catalog == nil || j.p.Deemix == nil {
		return "", errors.New("pipeline is not configured")
	}
	key := trackKey(t)
	j.File(t) // loads a download saved before a restart
	j.dropUnusable(t)
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
	// NetEase in Deemix mode downloaded the Deezer file: Deezer finds it as well.
	if t.Provider == "netease" && matched.Provider == "deezer" {
		j.putFile(trackKey(matched), f)
	}
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
	if j.p.Deemix != nil {
		j.saveSource(key, f.dl.Path)
	}
}

// acquire tries the deck's own Deemix service first, which costs no NetEase
// download quota, then the session's NetEase login. A NetEase track goes
// straight to NetEase unless the owner chose Deemix first. All audio lands on
// the deck.
func (j *Jobs) acquire(ctx context.Context, session string, t providers.Track, guest bool, progress func(deemixclient.QueueStatus)) (providers.Track, *deemixclient.Download, error) {
	// Each service plays from itself: no track is matched on another service
	// without the user asking for it. Only NetEase has a Deemix mode (below).
	switch t.Provider {
	case "soundcloud":
		dl, err := j.acquireSoundCloud(ctx, session, t, guest, progress)
		return t, dl, err
	case "beatport":
		dl, err := j.fromBeatport(ctx, t)
		return t, dl, err
	case "spotify":
		dl, err := j.fromSpotify(ctx, session, t, guest)
		return t, dl, err
	case "deezer":
		matched, err := deemixclient.Resolve(ctx, j.p.Catalog, t)
		if err != nil {
			return providers.Track{}, nil, err
		}
		dl, err := j.p.Deemix.Download(ctx, matched, progress)
		if err != nil {
			j.mu.Lock()
			ready := j.deezerReady
			j.mu.Unlock()
			if ready != nil && !ready() {
				return providers.Track{}, nil, errors.New("deezer: please scan QR code on the deck and enter your Deezer ARL first")
			}
			return providers.Track{}, nil, err
		}
		j.p.Deemix.Trim(dl.Path)
		return matched, dl, nil
	case "netease":
		return j.fromNeteaseMode(ctx, session, t, guest, progress)
	case "webdav", "subsonic", "plex":
		dl, err := j.fromSelfHosted(ctx, t)
		return t, dl, err
	}
	return providers.Track{}, nil, fmt.Errorf("%s: no audio source for this service", t.Provider)
}

// fromNeteaseMode: official mode goes straight to NetEase; deemix mode tries a
// Deezer match first and falls back to NetEase.
func (j *Jobs) fromNeteaseMode(ctx context.Context, session string, t providers.Track, guest bool, progress func(deemixclient.QueueStatus)) (providers.Track, *deemixclient.Download, error) {
	j.mu.Lock()
	lookup, direct := j.netease, j.neteaseDirect
	j.mu.Unlock()
	var failures []error
	if dl := j.p.Deemix.Cached("netease-" + t.ID); dl != nil {
		return t, dl, nil
	}
	// Official mode never touches Deemix or its shared <Deezer ID> files.
	if direct == nil || !direct() {
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

// fromSpotify fetches track t via the owner's logged-in Spotify provider.
func (j *Jobs) fromSpotify(ctx context.Context, session string, t providers.Track, guest bool) (*deemixclient.Download, error) {
	if j.p.Deemix != nil {
		if cached := j.p.Deemix.Cached("spotify-" + t.ID); cached != nil && strings.EqualFold(filepath.Ext(cached.Path), ".ogg") {
			if q, err := deemixclient.Inspect(cached.Path); err == nil {
				cached.Quality = q
			} else {
				cached.Quality = deemixclient.Quality{Label: "[320k]", Format: "ogg", SampleRate: 44100, Lossless: false}
			}
			return cached, nil
		}
	}
	j.mu.Lock()
	lookup := j.spotify
	j.mu.Unlock()
	var sp *providers.Spotify
	if lookup != nil {
		sp = lookup()
	}
	if sp == nil {
		return nil, errors.New("spotify: please log in via Spotify Connect or QR code on the deck first")
	}
	if j.p.Deemix == nil {
		return nil, errors.New("deemix: pipeline download directory is not configured")
	}
	path, err := sp.DownloadAudio(ctx, t.ID, j.p.Deemix.DownloadDir)
	if err != nil {
		return nil, err
	}
	j.p.Deemix.Trim(path)
	q, err := deemixclient.Inspect(path)
	if err != nil {
		q = deemixclient.Quality{Label: "[320k]", Format: "ogg", SampleRate: 44100, Lossless: false}
	}
	return &deemixclient.Download{Path: path, Quality: q}, nil
}

// isSoundCloudSet tells a DJ set, podcast or radio show (played from SoundCloud,
// which is the only place it exists) from a single, which other services carry.
func isSoundCloudSet(t providers.Track) bool {
	if t.DurationMS > 10*60*1000 {
		return true
	}
	if t.DurationMS == 0 {
		title := strings.ToLower(t.Title)
		for _, w := range []string{"dj set", "live set", "boiler room", "podcast", "radio show", "continuous mix"} {
			if strings.Contains(title, w) {
				return true
			}
		}
	}
	return false
}

const soundCloudLoginHint = "soundcloud: Go+ track requires SoundCloud Go+ login or another logged-in streaming service (scan QR code to log in) / 此为 SoundCloud Go+ 专享曲目，请扫码登录 SoundCloud Go+ 或其他流媒体账号"

// acquireSoundCloud plays a SoundCloud single from the best source the owner has:
// SoundCloud itself with Go+, else the same recording on a logged-in service (its
// lossless or 320k beats SoundCloud's stream, and a Go+ track is only a 30 s
// preview without Go+), else SoundCloud. A set always comes from SoundCloud.
func (j *Jobs) acquireSoundCloud(ctx context.Context, session string, t providers.Track, guest bool, progress func(deemixclient.QueueStatus)) (*deemixclient.Download, error) {
	if cached := j.p.Deemix.Cached("soundcloud-" + t.ID); cached != nil && (cached.Quality.Lossless || !j.p.Deemix.Lossless()) {
		return cached, nil
	}
	if isSoundCloudSet(t) {
		return j.fromSoundCloud(ctx, t)
	}
	j.mu.Lock()
	lookup := j.soundcloud
	j.mu.Unlock()
	var sc *providers.SoundCloud
	if lookup != nil {
		sc = lookup()
	}
	if sc != nil && sc.HasGoPlus(ctx) {
		dl, err := j.fromSoundCloud(ctx, t)
		if err == nil {
			return dl, nil
		}
		if other, oerr := j.fromLoggedInServicesForSoundCloud(ctx, session, t, guest, progress); oerr == nil {
			return other, nil
		}
		return nil, err
	}
	if dl, err := j.fromLoggedInServicesForSoundCloud(ctx, session, t, guest, progress); err == nil {
		return dl, nil
	}
	dl, err := j.fromSoundCloud(ctx, t)
	if errors.Is(err, providers.ErrNoFullStream) {
		return nil, errors.New(soundCloudLoginHint)
	}
	return dl, err
}

var errNoOtherService = errors.New("soundcloud: no other logged-in service has this track")

// fromLoggedInServicesForSoundCloud looks for t on Deezer, then NetEase, then
// Beatport, whichever the owner is logged in to, and saves it as soundcloud-<id>.
func (j *Jobs) fromLoggedInServicesForSoundCloud(ctx context.Context, session string, t providers.Track, guest bool, progress func(deemixclient.QueueStatus)) (*deemixclient.Download, error) {
	name := "soundcloud-" + t.ID
	j.mu.Lock()
	deezerReady, lookup, beatport := j.deezerReady, j.netease, j.beatport
	j.mu.Unlock()
	if deezerReady != nil && deezerReady() && j.p.Catalog != nil {
		if matched, err := deemixclient.Resolve(ctx, j.p.Catalog, t); err == nil {
			if dl, err := j.p.Deemix.DownloadAs(ctx, matched, name, progress); err == nil {
				return dl, nil
			}
		}
	}
	if lookup != nil {
		if n := lookup(session, guest); n != nil {
			if _, dl, err := j.fromNetease(ctx, n, t); err == nil {
				if dl, err = j.p.Deemix.LinkAs(dl, name); err == nil {
					return dl, nil
				}
			}
		}
	}
	if beatport != nil {
		if b := beatport(); b != nil {
			if found, err := b.Search(ctx, soundCloudSearch(t)); err == nil {
				if matched, err := deemixclient.MatchNearby(t, found, neteaseMatchToleranceMS); err == nil {
					if dl, err := j.fromBeatport(ctx, matched); err == nil {
						if dl, err = j.p.Deemix.LinkAs(dl, name); err == nil {
							return dl, nil
						}
					}
				}
			}
		}
	}
	return nil, errNoOtherService
}

// soundCloudSearch is "artist title" as the other services' search wants it.
func soundCloudSearch(t providers.Track) string {
	primary := t.Artist
	if len(t.Artists) > 0 {
		primary = t.Artists[0]
	}
	return strings.TrimSpace(strings.Split(primary, ",")[0] + " " + deemixclient.SearchTitle(t.Title))
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

// fromBeatport takes the best file the subscription allows: FLAC on Professional
// when the deck asks for lossless, else AAC. Essential has no file, only a stream.
func (j *Jobs) fromBeatport(ctx context.Context, t providers.Track) (*deemixclient.Download, error) {
	name := "beatport-" + t.ID
	lossless := j.p.Deemix.Lossless()
	cached := j.p.Deemix.Cached(name)
	if cached != nil && (cached.Quality.Lossless || !lossless) {
		return cached, nil
	}
	j.mu.Lock()
	lookup := j.beatport
	j.mu.Unlock()
	var b *providers.Beatport
	if lookup != nil {
		b = lookup()
	}
	if b == nil {
		if cached != nil {
			return cached, nil
		}
		return nil, errors.New("beatport: not connected")
	}
	location, ext, err := b.Download(ctx, t.ID, lossless)
	// AAC again would be no better than the file already on disk.
	if err == nil && (cached == nil || ext == "flac") {
		var dl *deemixclient.Download
		if ext == "flac" {
			dl, err = j.p.Deemix.SaveSplit(ctx, name, location)
		} else {
			dl, err = j.p.Deemix.SaveParts(ctx, name, "mp4", []string{location})
		}
		if err == nil {
			return dl, nil
		}
		log.Printf("stream-sync: Beatport file of track %s unusable: %v", t.ID, err)
	}
	if cached != nil {
		return cached, nil
	}
	return j.saveBeatportStream(ctx, b, name, t.ID)
}

var chmodJobFile = os.Chmod

func chmodStreamPerm(path string, mode os.FileMode) error {
	err := chmodJobFile(path, mode)
	if err == nil || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) {
		return nil
	}
	return err
}

// saveBeatportStream stores the track's stream as name.mp4.
func (j *Jobs) saveBeatportStream(ctx context.Context, b *providers.Beatport, name, id string) (*deemixclient.Download, error) {
	dir := j.p.Deemix.DownloadDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	final := filepath.Join(dir, name+".mp4")
	if err = b.SaveStream(ctx, id, f); err == nil {
		err = f.Close()
	}
	if err == nil {
		err = chmodStreamPerm(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), final)
	}
	if err != nil {
		return nil, err
	}
	for _, ext := range []string{".flac", ".mp3"} {
		_ = os.Remove(filepath.Join(dir, name+ext))
	}
	j.p.Deemix.Trim(final)
	return &deemixclient.Download{Path: final, Quality: deemixclient.Quality{Label: "[AAC]", Format: "mp4"}}, nil
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
	address, err := n.AudioURL(ctx, t.ID, j.p.Deemix.Lossless(), j.p.Deemix.LossyAllowed())
	if err != nil {
		return providers.Track{}, nil, err
	}
	dl, err := j.p.Deemix.Save(ctx, name, address)
	if err != nil {
		return providers.Track{}, nil, err
	}
	return t, dl, nil
}

func (j *Jobs) fromSelfHosted(ctx context.Context, t providers.Track) (*deemixclient.Download, error) {
	if j.p.Deemix == nil {
		return nil, errors.New("nas: download cache unavailable")
	}
	j.mu.Lock()
	lookup := j.selfHosted
	j.mu.Unlock()
	var sh *providers.SelfHostedProvider
	if lookup != nil {
		sh = lookup()
	}
	if sh == nil {
		return nil, fmt.Errorf("%s: NAS server is not enabled", t.Provider)
	}
	p, err := sh.DownloadTrack(ctx, t.ID, j.p.Deemix.DownloadDir)
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(filepath.Ext(p))
	if ext == ".m4a" || ext == ".aac" {
		mp4Path := strings.TrimSuffix(p, filepath.Ext(p)) + ".mp4"
		if os.Rename(p, mp4Path) == nil {
			p = mp4Path
			ext = ".mp4"
		}
	}
	q, ierr := deemixclient.Inspect(p)
	if ierr != nil {
		fmtName := strings.TrimPrefix(ext, ".")
		if fmtName == "" {
			fmtName = "mp3"
		}
		q = deemixclient.Quality{
			Label:    "[" + strings.ToUpper(fmtName) + "]",
			Format:   fmtName,
			Lossless: fmtName == "flac" || fmtName == "wav" || fmtName == "aif" || fmtName == "aiff",
		}
	}
	j.p.Deemix.Trim(p)
	return &deemixclient.Download{Path: p, Quality: q}, nil
}

// queueImport analyses and imports a streamed file once per target playlist.
// A full queue only skips library import; the file is still playable.
func (j *Jobs) queueImport(session string, t providers.Track, guest bool, f fetched) {
	// No library (no front SD Engine Library): nothing to import into, so do not
	// queue a job that can only end in "pipeline is not configured".
	// A DJ set is played, not analysed: decoding hours of audio would exhaust memory.
	if j.p.Library == nil || tooLongToAnalyse(t, f.dl.Path) {
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
	playlist := providers.OwnerPlaylist
	if guest {
		playlist = providers.GuestPlaylist
	}
	return j.submitTo(session, t, playlist, matched, dl)
}

// SubmitFile analyses an audio file already on the library's volume and
// imports it into playlist (#66). ok is false when there is no library, the
// file is on another drive, or it is not FLAC/MP3.
// shortcut: only FLAC/MP3 pass deemixclient.Inspect; WAV/AIFF/M4A are left to Engine's analysis on load.
func (j *Jobs) SubmitFile(path, playlist string) (job Job, ok bool, err error) {
	lib, has := j.p.Library.(interface{ Root() string })
	if !has {
		return Job{}, false, nil
	}
	if real, e := filepath.EvalSymlinks(path); e == nil {
		path = real
	}
	if rel, e := filepath.Rel(lib.Root(), path); e != nil || !filepath.IsLocal(rel) {
		return Job{}, false, nil
	}
	q, err := deemixclient.Inspect(path)
	if err != nil {
		return Job{}, false, nil
	}
	t := providers.Track{ID: "drop:" + path, Title: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), Provider: localProvider}
	job, err = j.submitTo("wifi-drop", t, playlist, providers.Track{}, &deemixclient.Download{Path: path, Quality: q})
	return job, err == nil, err
}

func (j *Jobs) submitTo(session string, t providers.Track, playlist string, matched providers.Track, dl *deemixclient.Download) (Job, error) {
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
