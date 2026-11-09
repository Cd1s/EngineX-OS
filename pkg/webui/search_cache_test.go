package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type searchFixture struct {
	browseTestProvider
	block   <-chan struct{}
	started chan struct{}
	calls   atomic.Int32
	title   string
}

func (p *searchFixture) Search(context.Context, string) ([]providers.Track, error) {
	p.calls.Add(1)
	if p.started != nil {
		close(p.started)
	}
	if p.block != nil {
		<-p.block
	} // Deliberately ignores cancellation.
	return []providers.Track{{ID: p.title, Title: p.title, Provider: "spotify", ThumbURL: "thumb", Preview: "preview", Artists: []string{"original"}}}, nil
}
func (p *searchFixture) SearchPlaylists(context.Context, string) ([]providers.Playlist, error) {
	if p.block != nil {
		<-p.block
	}
	return []providers.Playlist{{ID: p.title, Title: p.title}}, nil
}

func searchServer(t *testing.T, scope string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope="+scope+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
	t.Cleanup(s.Close)
	return s
}

func searchResponse(s *Server, ctx context.Context) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/search?query=parallel", nil).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:12345"
	s.tidal.ServeHTTP(w, r)
	return w
}

func TestSearchHungSourceKeepsOtherResultsAndOrder(t *testing.T) {
	s := searchServer(t, "spotify")
	release := make(chan struct{})
	defer close(release)
	slow := &searchFixture{block: release, started: make(chan struct{}), title: "late"}
	first := &searchFixture{title: "first"}
	second := &searchFixture{title: "second"}
	for id, p := range map[string]*searchFixture{"a": slow, "b": first, "c": second} {
		s.sessions[id] = &Session{ID: id, Provider: "spotify", Ready: true, p: p}
	}
	started := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- searchResponse(s, context.Background()) }()
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(searchTimeout + time.Second):
		t.Fatal("hung source exceeded independent search deadline")
	}
	if elapsed := time.Since(started); elapsed < searchTimeout || elapsed > searchTimeout+time.Second {
		t.Fatalf("unexpected deadline: %s", elapsed)
	}
	t.Logf("hung source response: %s", time.Since(started))
	var result struct {
		Tracks, Playlists struct {
			Items []struct {
				Title string
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var tracks, lists []string
	for _, v := range result.Tracks.Items {
		tracks = append(tracks, v.Title)
	}
	for _, v := range result.Playlists.Items {
		lists = append(lists, v.Title)
	}
	if w.Code != 200 || strings.Join(tracks, ",") != "first,second" || strings.Join(lists, ",") != "first,second" {
		t.Fatalf("lost or reordered fast results: %s", w.Body.String())
	}
	started = time.Now()
	w = searchResponse(s, context.Background())
	t.Logf("warm search response: %s", time.Since(started))
	if time.Since(started) > time.Second || first.calls.Load() != 1 || second.calls.Load() != 1 || slow.calls.Load() != 1 {
		t.Fatalf("repeat search missed cache/backoff: %s", w.Body.String())
	}
}

func TestSearchCacheNamespaceExpiryAndPrivateCopies(t *testing.T) {
	s := searchServer(t, "spotify")
	clock := cacheClock(s.tidal.searches.browseCache)
	p := &searchFixture{title: "one"}
	get := func(provider, session, query string) []providers.Track {
		t.Helper()
		tracks, err := s.tidal.searchTracks(context.Background(), provider, session, p, query)
		if err != nil || len(tracks) != 1 {
			t.Fatalf("%v %v", tracks, err)
		}
		return tracks
	}
	tracks := get("spotify", "owner", "query")
	tracks[0].Title, tracks[0].Artists[0] = "mutated", "mutated"
	tracks = get("spotify", "owner", "query")
	if p.calls.Load() != 1 || tracks[0].Title != "one" || tracks[0].Artists[0] != "original" || tracks[0].ThumbURL != "thumb" || tracks[0].Preview != "preview" {
		t.Fatalf("cache changed metadata: %+v calls=%d", tracks, p.calls.Load())
	}
	get("spotify", "guest", "query")
	get("soundcloud", "owner", "query")
	get("spotify", "owner", "different")
	if p.calls.Load() != 4 {
		t.Fatal("search cache crossed namespaces")
	}
	clock.Add(int64(searchTTL + time.Millisecond))
	get("spotify", "owner", "query")
	if p.calls.Load() != 5 {
		t.Fatal("expired search was served stale")
	}
	if s.tidal.searches.dir != "" {
		t.Fatal("volatile search was persisted")
	}
}

func TestSearchSkipsUnloggedServicesAndUnrelatedRestores(t *testing.T) {
	for _, scope := range loginServices {
		t.Run(scope, func(t *testing.T) {
			s := searchServer(t, scope)
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer api.Close()
			s.tidal.catalog.BaseURL = api.URL
			s.cfg.SoundCloudURL = api.URL
			s.cfg.Factory = func(name string, _ bool, _ string) (providers.Provider, error) {
				if name != scope {
					t.Errorf("restored unrelated platform: %s", name)
				}
				switch name {
				case "netease":
					return &providers.Netease{BaseURL: api.URL}, nil
				case "spotify":
					return &providers.Spotify{BaseURL: api.URL}, nil
				case "deezer":
					return &providers.Deezer{BaseURL: api.URL}, nil
				case "soundcloud":
					return &providers.SoundCloud{BaseURL: api.URL}, nil
				case "beatport":
					return &providers.Beatport{BaseURL: api.URL}, nil
				case "applemusic":
					return &providers.AppleMusic{BaseURL: api.URL}, nil
				}
				return nil, errors.New("unsupported")
			}
			w := searchResponse(s, context.Background())
			if w.Code != 200 || requests.Load() != 0 {
				t.Fatalf("unlogged search issued %d requests: %s", requests.Load(), w.Body.String())
			}
		})
	}
}

func TestSearchCallHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := searchCall(ctx, "fixture", "tracks", func(context.Context) (browseData, error) {
			close(started)
			<-release
			return browseData{}, nil
		})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation blocked")
	}
}

func TestSearchWorkerLimitSurvivesTimeoutAndReleases(t *testing.T) {
	c := newSearchCache()
	clock := cacheClock(c.browseCache)
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	defer closeRelease()
	var calls atomic.Int32
	blocked := func(context.Context) (browseData, error) {
		calls.Add(1)
		<-release
		return browseData{}, nil
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := c.call(ctx, "spotify", "stuck", "tracks", blocked)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected timeout: %v", err)
		}
	}
	clock.Add(int64(searchTimeout))
	key := browseKey("spotify", "stuck", "search-tracks", "last-keystroke")
	if _, err := c.get(context.Background(), key, func(ctx context.Context) (browseData, error) {
		return c.call(ctx, "spotify", "stuck", "tracks", blocked)
	}); !errors.Is(err, errBrowseBusy) || calls.Load() != 2 {
		t.Fatal("timed-out workers released slots before exiting")
	}
	quick := func(context.Context) (browseData, error) { return browseFixture("healthy"), nil }
	if data, err := c.call(context.Background(), "spotify", "healthy", "tracks", quick); err != nil || len(data.Lists) != 1 {
		t.Fatalf("stuck session consumed healthy session's slots: %+v %v", data, err)
	}
	closeRelease()
	// Wait for actual worker completion, then prove that capacity is reusable.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		c.workerMu.Lock()
		active := c.active
		c.workerMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed workers did not release slots")
		}
	}
	if _, err := c.get(context.Background(), key, func(ctx context.Context) (browseData, error) { return c.call(ctx, "spotify", "stuck", "tracks", quick) }); err != nil {
		t.Fatal(err)
	}
}

func TestSearchWorkerLimitAllowsHealthyTyping(t *testing.T) {
	c := newSearchCache()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	defer closeRelease()
	done := make(chan error, 4)
	for i := range 4 {
		go func() {
			_, err := c.get(context.Background(), browseKey("spotify", "owner", "search-tracks", strconv.Itoa(i)), func(ctx context.Context) (browseData, error) {
				return c.call(ctx, "spotify", "owner", "tracks", func(context.Context) (browseData, error) {
					started <- struct{}{}
					<-release
					return browseFixture("healthy"), nil
				})
			})
			done <- err
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("normal concurrent keystrokes rejected as hung workers")
		}
	}
	closeRelease()
	for range 4 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("healthy query did not finish")
		}
	}
}
