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

// Spotify cannot use catalog credentials until another service is logged in.
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
	j, dir, deezer := cacheTestJobs(t)
	j.deezerReady = func() bool { return true }
	track := deezer
	track.Provider, track.ID = "spotify", "sptrack1"
	_, dl, err := j.acquire(context.Background(), "s", track, false, nil)
	if err != nil || filepath.Base(dl.Path) != "spotify-sptrack1.flac" || !dl.Quality.Lossless {
		t.Fatalf("matched download: %+v, %v", dl, err)
	}
	if cached := j.p.Deemix.Cached("spotify-" + track.ID); cached == nil || cached.Path != dl.Path {
		t.Fatal("Spotify FLAC cache missing")
	}
	j.deezerReady = func() bool { return false }
	_, dl2, err := j.acquire(context.Background(), "s", track, false, nil)
	if err != nil || dl2.Path != dl.Path {
		t.Fatal("cached track requires a login")
	}
	// A restart must discover the prefix even without a source index.
	restarted := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: dir}})
	defer restarted.Close()
	if path := restarted.File(track); path != dl.Path {
		t.Fatalf("cold cache: %q", path)
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

// A stale shared-file index is refused; logged-in matching creates a Spotify name.
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
	j.deezerReady = func() bool { return true }
	_, dl, err = j.acquire(context.Background(), "s", sp, false, nil)
	if err != nil || filepath.Base(dl.Path) != "spotify-abc.flac" {
		t.Fatalf("matching: %+v %v", dl, err)
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

// A reachable catalog alone is not authorization to use Deezer for Spotify.
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

func TestSpotifyMatchesBeatportAndAppleMusic(t *testing.T) {
	for _, service := range []string{"beatport", "applemusic", "beatport-only", "applemusic-only"} {
		t.Run(service, func(t *testing.T) {
			exclusive := strings.HasSuffix(service, "-only")
			service = strings.TrimSuffix(service, "-only")
			dir := t.TempDir()
			var flac bytes.Buffer
			if err := testaudio.FLAC(&flac, testaudio.Song(4)); err != nil {
				t.Fatal(err)
			}
			var helperCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/catalog/search/":
					w.Write([]byte(`{"tracks":[{"id":12,"name":"Song","artists":[{"name":"A"}],"length_ms":200000,"isrc":"FIXTURE"}]}`))
				case "/v1/catalog/us/search":
					w.Write([]byte(`{"results":{"songs":{"data":[{"id":"34","type":"songs","attributes":{"name":"Song","artistName":"A","durationInMillis":200000,"isrc":"FIXTURE"}}]}}}`))
				case "/v1/download":
					helperCalls.Add(1)
					w.Write([]byte(`{"file":"/audio"}`))
				case "/audio":
					w.Write(flac.Bytes())
				default:
					t.Errorf("unexpected matching request: %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			j := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: dir}, Catalog: noCatalog{}})
			defer j.Close()
			if exclusive {
				j.spotifySource = func() string { return service }
			}
			if service == "beatport" {
				writeTestFLAC(t, filepath.Join(dir, "beatport-12.flac"))
				b := &providers.Beatport{BaseURL: srv.URL}
				if err := b.SetToken(providers.BeatportToken{Access: "fixture"}); err != nil {
					t.Fatal(err)
				}
				j.beatport = func() *providers.Beatport { return b }
			} else {
				am := &providers.AppleMusic{BaseURL: srv.URL, HelperURL: srv.URL}
				// The fixture developer token avoids scraping the live web player.
				owner := filepath.Join(dir, "owner.json")
				if err := providers.SaveJSON(owner, providers.AppleMusicOwner{DevToken: "fixture", Storefront: "us"}); err != nil {
					t.Fatal(err)
				}
				am.OwnerPath = owner
				if err := am.Load(); err != nil {
					t.Fatal(err)
				}
				j.applemusic = func() *providers.AppleMusic { return am }
			}
			tr := providers.Track{ID: "spotifytrack", Provider: "spotify", Title: "Song", Artist: "A", ISRC: "FIXTURE", DurationMS: 200000}
			got, dl, err := j.acquire(context.Background(), "s", tr, false, nil)
			if err != nil || got.ID != tr.ID || filepath.Base(dl.Path) != "spotify-spotifytrack.flac" || !dl.Quality.Lossless {
				t.Fatalf("%s match failed: %+v %v", service, dl, err)
			}
			if service == "applemusic" && helperCalls.Load() != 1 {
				t.Fatal("Apple Music helper was not used")
			}
		})
	}
}

func TestSpotifyLossyCacheReusedWithOtherServices(t *testing.T) {
	j, dir, deezer := cacheTestJobs(t)
	tr := deezer
	tr.Provider, tr.ID = "spotify", "upgrade"
	lossy := filepath.Join(dir, "spotify-upgrade.mp3")
	if err := os.WriteFile(lossy, testMP3(200), 0o644); err != nil {
		t.Fatal(err)
	}
	j.p.Deemix.AllowLossy = true
	// A warm lossy file remains playable regardless of current logins or preference.
	_, dl, err := j.acquire(context.Background(), "s", tr, false, nil)
	if err != nil || dl.Path != lossy {
		t.Fatalf("lossy fallback: %+v %v", dl, err)
	}
	j.deezerReady = func() bool { return true }
	_, dl, err = j.acquire(context.Background(), "s", tr, false, nil)
	if err != nil || dl.Path != lossy || dl.Quality.Lossless {
		t.Fatalf("warm lossy cache was replaced: %+v %v", dl, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "123.flac")); err != nil {
		t.Fatal("shared Deezer cache was consumed")
	}
}

func TestSpotifySourceIsolationAndAutoOrder(t *testing.T) {
	for _, source := range []string{"auto", "deezer", "netease", "beatport", "applemusic", "soundcloud"} {
		t.Run(source, func(t *testing.T) {
			var called []string
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
			defer api.Close()
			dir := t.TempDir()
			am := &providers.AppleMusic{BaseURL: api.URL, HelperURL: api.URL, OwnerPath: filepath.Join(dir, "apple.json")}
			if err := providers.SaveJSON(am.OwnerPath, providers.AppleMusicOwner{DevToken: "fixture", Storefront: "us"}); err != nil {
				t.Fatal(err)
			}
			if err := am.Load(); err != nil {
				t.Fatal(err)
			}
			b := &providers.Beatport{BaseURL: api.URL}
			if err := b.SetToken(providers.BeatportToken{Access: "fixture"}); err != nil {
				t.Fatal(err)
			}
			j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dir}})
			defer j.Close()
			j.spotifySource = func() string { return source }
			j.deezerReady = func() bool { called = append(called, "deezer"); return true }
			j.netease = func(string, bool) *providers.Netease {
				called = append(called, "netease")
				return &providers.Netease{BaseURL: api.URL}
			}
			j.beatport = func() *providers.Beatport { called = append(called, "beatport"); return b }
			j.applemusic = func() *providers.AppleMusic { called = append(called, "applemusic"); return am }
			j.soundcloud = func() *providers.SoundCloud {
				called = append(called, "soundcloud")
				return &providers.SoundCloud{BaseURL: api.URL, ClientID: "fixture", Secret: "fixture", Token: "fixture"}
			}
			_, dl, err := j.acquire(context.Background(), "s", providers.Track{Provider: "spotify", ID: "missing", Title: "Missing", Artist: "A", ISRC: "MISSING"}, false, nil)
			if err == nil || dl != nil || !strings.Contains(err.Error(), "登录") || !strings.Contains(err.Error(), "spotify:") {
				t.Fatalf("missing bilingual failure: %v, %+v", err, dl)
			}
			want := source
			if source == "auto" {
				want = "deezer,netease,beatport,applemusic,soundcloud"
			}
			if got := strings.Join(called, ","); got != want {
				t.Fatalf("source %s queried %s; want %s", source, got, want)
			}
		})
	}
}

func TestSpotifySourceLoggedOutAndCache(t *testing.T) {
	for _, source := range []string{"deezer", "netease", "beatport", "applemusic", "soundcloud"} {
		t.Run(source, func(t *testing.T) {
			j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
			defer j.Close()
			j.spotifySource = func() string { return source }
			track := providers.Track{Provider: "spotify", ID: "cached"}
			if _, _, err := j.acquire(context.Background(), "s", track, false, nil); err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), "未登录") || !strings.Contains(err.Error(), "not logged in") {
				t.Fatalf("logged-out source: %v", err)
			}
			path := filepath.Join(j.p.Deemix.DownloadDir, "spotify-cached.flac")
			writeTestFLAC(t, path)
			_, dl, err := j.acquire(context.Background(), "s", track, false, nil)
			if err != nil || dl == nil || dl.Path != path {
				t.Fatalf("cache requires login: %+v %v", dl, err)
			}
		})
	}
}

func TestSpotifySoundCloudSourceAndFallback(t *testing.T) {
	for _, source := range []string{"auto", "soundcloud"} {
		t.Run(source, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/tracks" {
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				w.Write([]byte(`{"collection":[{"id":5,"title":"Song","user":{"username":"A"},"duration":200000,"access":"playable"}]}`))
			}))
			defer srv.Close()
			dir := t.TempDir()
			writeTestFLAC(t, filepath.Join(dir, "soundcloud-5.flac"))
			j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: dir}})
			defer j.Close()
			sc := &providers.SoundCloud{BaseURL: srv.URL, ClientID: "fixture", Secret: "fixture", Token: "fixture"}
			j.soundcloud = func() *providers.SoundCloud { return sc }
			j.spotifySource = func() string { return source }
			tr := providers.Track{Provider: "spotify", ID: "scmatch", Title: "Song", Artist: "A", DurationMS: 200000}
			_, dl, err := j.acquire(context.Background(), "s", tr, false, nil)
			if err != nil || dl == nil || filepath.Base(dl.Path) != "spotify-scmatch.flac" || calls.Load() != 1 {
				t.Fatalf("SoundCloud match: %+v %v, calls=%d", dl, err, calls.Load())
			}
			j.soundcloud = nil
			_, again, err := j.acquire(context.Background(), "s", tr, false, nil)
			if err != nil || again == nil || again.Path != dl.Path || calls.Load() != 1 {
				t.Fatalf("SoundCloud cache missed: %+v %v", again, err)
			}
		})
	}
}

func TestSpotifySoundCloudPublicSearchNeedsLogin(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	j := NewJobs(Pipeline{Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer j.Close()
	j.spotifySource = func() string { return "soundcloud" }
	j.soundcloud = func() *providers.SoundCloud {
		return &providers.SoundCloud{BaseURL: srv.URL, WebURL: srv.URL, SiteURL: srv.URL}
	}
	_, _, err := j.acquire(context.Background(), "s", providers.Track{Provider: "spotify", ID: "loggedout"}, false, nil)
	if err == nil || calls.Load() != 0 || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("public search bypassed login: %v, calls=%d", err, calls.Load())
	}
}
