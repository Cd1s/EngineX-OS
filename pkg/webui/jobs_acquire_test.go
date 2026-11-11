package webui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// A Spotify, Beatport-less or unknown track never borrows another service's audio.
func TestAcquireDoesNotCrossServices(t *testing.T) {
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer j.Close()
	for provider, want := range map[string]string{"spotify": "spotify:", "applemusic": "applemusic:"} {
		_, _, err := j.acquire(context.Background(), "s", providers.Track{ID: "1", Provider: provider, Title: "T", Artist: "A", ISRC: "X"}, false, nil)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("%s: %v", provider, err)
		}
	}
}

func TestFromSpotifyDownloadsAndCaches(t *testing.T) {
	dlDir := t.TempDir()
	spAudio := []byte("OggS-downloaded-spotify-stream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write(spAudio)
	}))
	defer srv.Close()

	sp := &providers.Spotify{
		BaseURL: srv.URL,
		Token:   providers.Token{AccessToken: "test-token"},
	}

	deemix := &deemixclient.Client{DownloadDir: dlDir}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: deemix})
	defer j.Close()
	j.spotify = func() *providers.Spotify { return sp }

	ctx := context.Background()
	track := providers.Track{ID: "sptrack1", Provider: "spotify"}

	// 1. First fetch: should call DownloadAudio and return Ogg Vorbis
	dl, err := j.fromSpotify(ctx, "session1", track, false)
	if err != nil {
		t.Fatalf("fromSpotify download: %v", err)
	}
	if dl.Quality.Format != "ogg" {
		t.Fatalf("expected format ogg, got %q", dl.Quality.Format)
	}
	content, err := os.ReadFile(dl.Path)
	if err != nil || string(content) != string(spAudio) {
		t.Fatalf("unexpected downloaded content: %s %v", content, err)
	}

	// 2. Cached verification: Cached should find .ogg
	cached := deemix.Cached("spotify-" + track.ID)
	if cached == nil || cached.Path != dl.Path || cached.Quality.Lossless {
		t.Fatalf("deemix.Cached failed on ogg: %+v", cached)
	}

	// 3. Second fetch: shut down server, should hit cache without error
	srv.Close()
	dl2, err := j.fromSpotify(ctx, "session1", track, false)
	if err != nil || dl2.Path != dl.Path || dl2.Quality.Format != "ogg" || dl2.Quality.SampleRate != 44100 {
		t.Fatalf("fromSpotify cache hit failed: %v, %+v", err, dl2)
	}
}

// isrcCatalog resolves every ISRC to one Deezer track.
type isrcCatalog struct{ track providers.Track }

func (c isrcCatalog) ByISRC(context.Context, string) (providers.Track, error) { return c.track, nil }
func (isrcCatalog) Search(context.Context, string) ([]providers.Track, error) { return nil, nil }

func writeTestFLAC(t *testing.T, path string) {
	t.Helper()
	var b bytes.Buffer
	if err := testaudio.FLAC(&b, testaudio.Song(4)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cacheTestJobs(t *testing.T) (*Jobs, string, providers.Track) {
	dir := t.TempDir()
	writeTestFLAC(t, filepath.Join(dir, "123.flac"))
	deezer := providers.Track{ID: "123", Provider: "deezer", ISRC: "USX000000001", Title: "Song", Artist: "A", DurationMS: 200000}
	j := NewJobs(Pipeline{Catalog: isrcCatalog{deezer}, Deemix: &deemixclient.Client{DownloadDir: dir}})
	t.Cleanup(j.Close)
	return j, dir, deezer
}

// NetEase in Deemix mode and Deezer share one download; NetEase official mode does not use it.
func TestNeteaseDeemixSharesTheDeezerFile(t *testing.T) {
	j, dir, deezer := cacheTestJobs(t)
	nt := providers.Track{ID: "9", Provider: "netease", ISRC: deezer.ISRC, Title: "Song", Artist: "A", DurationMS: 200000}
	path, err := j.Fetch(context.Background(), "s", nt, false)
	if err != nil || path != filepath.Join(dir, "123.flac") {
		t.Fatalf("%q %v", path, err)
	}
	if got := j.File(deezer); got != path {
		t.Fatalf("Deezer does not find the NetEase download: %q", got)
	}
	j.neteaseDirect = func() bool { return true }
	if got := j.File(nt); got != "" {
		t.Fatalf("official mode reused a Deemix file: %q", got)
	}
}

// Spotify never takes a Deezer file: a saved index entry for it is ignored, and the
// failed native downloads never use Deemix, even with an exact ISRC match.
func TestSpotifyKeepsItsOwnFiles(t *testing.T) {
	j, dir, deezer := cacheTestJobs(t)
	sp := providers.Track{ID: "abc", Provider: "spotify", ISRC: deezer.ISRC, Title: "Song", Artist: "A", DurationMS: 200000}
	os.WriteFile(filepath.Join(dir, sourcesFile), []byte(`{"spotify:abc":"123.flac"}`), 0o644)
	if got := j.File(sp); got != "" {
		t.Fatalf("Spotify reused the Deezer file: %q", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer srv.Close()
	spotify := &providers.Spotify{BaseURL: srv.URL, Token: providers.Token{AccessToken: "t"}}
	j.spotify = func() *providers.Spotify { return spotify }
	_, dl, err := j.acquire(context.Background(), "s", sp, false, nil)
	if err == nil || dl != nil {
		t.Fatalf("%+v %v", dl, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "123.flac")); err != nil {
		t.Fatal("the Deezer file is gone")
	}
}

func TestIsSoundCloudSet(t *testing.T) {
	for _, c := range []struct {
		t    providers.Track
		want bool
	}{
		{providers.Track{DurationMS: 4 * 60 * 1000, Title: "Club banger"}, false},
		{providers.Track{DurationMS: 10 * 60 * 1000}, false},
		{providers.Track{DurationMS: 10*60*1000 + 1}, true},
		{providers.Track{Title: "Boiler Room Berlin"}, true},
		{providers.Track{Title: "Weekly Podcast 12"}, true},
		{providers.Track{Title: "Plain title"}, false},
		{providers.Track{DurationMS: 5 * 60 * 1000, Title: "DJ Set"}, false},
	} {
		if got := isSoundCloudSet(c.t); got != c.want {
			t.Errorf("%+v: got %v", c.t, got)
		}
	}
}

// A SoundCloud single without Go+ comes from another logged-in service, as soundcloud-<id>.
func TestSoundCloudSingleUsesLoggedInDeezer(t *testing.T) {
	j, dir, deezer := cacheTestJobs(t)
	j.deezerReady = func() bool { return true }
	sc := providers.Track{ID: "77", Provider: "soundcloud", ISRC: deezer.ISRC, Title: "Song", Artist: "A", DurationMS: 200000}
	_, dl, err := j.acquire(context.Background(), "s", sc, false, nil)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-77.flac" {
		t.Fatalf("%+v %v", dl, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "123.flac")); err != nil {
		t.Fatal("the Deezer file is gone")
	}
}

func TestSoundCloudGoPlusRouting(t *testing.T) {
	stream := testMP3(200)
	subscription := ""
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			if subscription == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(`{"id":1,"consumer_subscription":{"product":{"id":"` + subscription + `"}}}`))
		case "/tracks/5":
			if subscription == "" {
				w.Write([]byte(`{"id":5,"access":"preview"}`))
				return
			}
			w.Write([]byte(`{"id":5,"access":"playable"}`))
		case "/tracks/5/streams":
			w.Write([]byte(`{"http_mp3_128_url":"` + srv.URL + `/cdn/stream.mp3"}`))
		case "/cdn/stream.mp3":
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(stream))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
	defer srv.Close()
	d := &deemixclient.Client{DownloadDir: t.TempDir(), AllowLossy: true, PreferMP3: func() bool { return true }}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: d})
	defer j.Close()
	// Deezer is logged in but has nothing: a Go+ owner never asks it.
	j.deezerReady = func() bool { return true }
	newSC := func() *providers.SoundCloud {
		return &providers.SoundCloud{BaseURL: srv.URL, ClientID: "cid", Secret: "shh", Token: "t"}
	}
	j.soundcloud = newSC
	track := providers.Track{ID: "5", Provider: "soundcloud", Title: "Song", Artist: "A", DurationMS: 200000}

	// No Go+, nothing else to play it from: the preview is refused with a login hint.
	if _, err := j.acquireSoundCloud(context.Background(), "s", track, false, nil); err == nil || !strings.Contains(err.Error(), "Go+") {
		t.Fatalf("want the Go+ login hint, got %v", err)
	}
	// Go+: SoundCloud itself plays it.
	subscription = "high_tier"
	dl, err := j.acquireSoundCloud(context.Background(), "s", track, false, nil)
	if err != nil || filepath.Base(dl.Path) != "soundcloud-5.mp3" {
		t.Fatalf("%+v %v", dl, err)
	}
}

// #210: even a resolvable ISRC and reachable Deemix must never supply Spotify.
func TestSpotifyDownloadFailureNeverCallsDeemix(t *testing.T) {
	var bridgeCalls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bridge.Close()
	var catalogCalls atomic.Int32
	catalog := forbiddenSpotifyCatalog{calls: &catalogCalls}
	audio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer audio.Close()
	j := NewJobs(Pipeline{Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: t.TempDir()}})
	defer j.Close()
	sp := &providers.Spotify{BaseURL: audio.URL, Token: providers.Token{AccessToken: "fixture"}}
	j.spotify = func() *providers.Spotify { return sp }
	track := providers.Track{Provider: "spotify", ID: "fixture", ISRC: "FIXTURE", Title: "Track", Artist: "Artist"}
	got, dl, err := j.acquire(context.Background(), "s", track, false, nil)
	if err == nil || dl != nil || got.ID != track.ID || bridgeCalls.Load() != 0 || catalogCalls.Load() != 0 {
		t.Fatalf("Spotify failure crossed services: err=%v, bridge=%d, catalog=%d", err, bridgeCalls.Load(), catalogCalls.Load())
	}
}

type forbiddenSpotifyCatalog struct{ calls *atomic.Int32 }

func (c forbiddenSpotifyCatalog) ByISRC(context.Context, string) (providers.Track, error) {
	c.calls.Add(1)
	return providers.Track{ID: "match", Provider: "deezer"}, nil
}
func (c forbiddenSpotifyCatalog) Search(context.Context, string) ([]providers.Track, error) {
	c.calls.Add(1)
	return []providers.Track{{ID: "match", Provider: "deezer"}}, nil
}
