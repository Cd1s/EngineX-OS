package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func browseFixture(title string) browseData {
	return browseData{Lists: []providers.Playlist{{ID: "1", Title: title, Count: 1}}}
}
func cacheClock(c *browseCache) *atomic.Int64 {
	now := &atomic.Int64{}
	now.Store(time.Now().UnixNano())
	c.now = func() time.Time { return time.Unix(0, now.Load()) }
	return now
}
func waitBrowse(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cache operation blocked")
	}
}
func currentBrowseFlight(c *browseCache, key string) *browseFlight {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flights[key]
}

func TestBrowseHitExpiryAndConcurrentRefresh(t *testing.T) {
	c := newBrowseCache("")
	clock := cacheClock(c)
	key := browseKey("netease", "owner-session", "lists", "")
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	load := func(ctx context.Context) (browseData, error) {
		if calls.Add(1) == 1 {
			return browseFixture("old"), nil
		}
		close(started)
		select {
		case <-release:
			return browseFixture("new"), nil
		case <-ctx.Done():
			return browseData{}, ctx.Err()
		}
	}
	first, err := c.get(context.Background(), key, load)
	if err != nil {
		t.Fatal(err)
	}
	first.Lists[0].Title = "mutated"
	hit, err := c.get(context.Background(), key, load)
	if err != nil || hit.Lists[0].Title != "old" || calls.Load() != 1 {
		t.Fatalf("hit = %+v, calls %d, err %v", hit, calls.Load(), err)
	}
	clock.Add(int64(browseTTL + time.Second))
	// A blocked upstream proves expired data does not wait for refresh.
	stale, err := c.get(context.Background(), key, load)
	if err != nil || stale.Lists[0].Title != "old" {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
	waitBrowse(t, started)
	for range 20 {
		v, err := c.get(context.Background(), key, load)
		if err != nil || v.Lists[0].Title != "old" {
			t.Fatal("lost stale value")
		}
	}
	f := currentBrowseFlight(c, key)
	close(release)
	waitBrowse(t, f.done)
	fresh, err := c.get(context.Background(), key, load)
	if err != nil || fresh.Lists[0].Title != "new" || calls.Load() != 2 {
		t.Fatalf("refresh %+v, calls %d, %v", fresh, calls.Load(), err)
	}
}

func TestBrowseColdRequestsCoalesceAndCancelIndependently(t *testing.T) {
	c := newBrowseCache("")
	key := browseKey("spotify", "guest-a", "tracks", "1")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context) (browseData, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return browseFixture("ok"), nil
		case <-ctx.Done():
			return browseData{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan struct{})
	go func() {
		defer close(cancelled)
		if _, err := c.get(ctx, key, load); !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	waitBrowse(t, started)
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.get(context.Background(), key, load)
			if err != nil || v.Lists[0].Title != "ok" {
				t.Errorf("shared fetch %+v, %v", v, err)
			}
		}()
	}
	cancel()
	waitBrowse(t, cancelled)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d upstream calls", calls.Load())
	}
}

func TestBrowseFailuresBackoffAndStaleLimit(t *testing.T) {
	c := newBrowseCache("")
	clock := cacheClock(c)
	key := browseKey("deezer", "owner", "lists", "")
	var calls atomic.Int32
	load := func(context.Context) (browseData, error) { calls.Add(1); return browseData{}, errors.New("offline") }
	c.insert(key, &browseEntry{data: []byte(`{"lists":[{"id":"1","title":"old"}]}`), at: c.now(), used: c.now()})
	clock.Add(int64(browseTTL + time.Second))
	_, _ = c.get(context.Background(), key, load)
	f := currentBrowseFlight(c, key)
	if f != nil {
		waitBrowse(t, f.done)
	}
	for range 5 {
		if v, err := c.get(context.Background(), key, load); err != nil || v.Lists[0].Title != "old" {
			t.Fatal("stale success discarded")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("failure storm")
	}
	clock.Add(int64(browseStale))
	if _, err := c.get(context.Background(), key, load); err == nil {
		t.Fatal("served data older than stale limit")
	}
	if _, err := c.get(context.Background(), key, load); err == nil || calls.Load() != 2 {
		t.Fatal("cold error backoff missing")
	}
}

func TestBrowseDiskIsolationSecretsAndBounds(t *testing.T) {
	state := t.TempDir()
	c := newBrowseCache(state)
	key := browseKey("soundcloud", "guest-a", "tracks", "same-id")
	if key == browseKey("deezer", "guest-a", "tracks", "same-id") || key == browseKey("soundcloud", "guest-b", "tracks", "same-id") {
		t.Fatal("namespace collision")
	}
	load := func(context.Context) (browseData, error) {
		return browseData{Tracks: []browseTrack{{Track: providers.Track{ID: "1", Title: "song", CoverURL: "https://cdn.invalid/art?token=secret"}, Thumb: "https://cdn.invalid/thumb?key=secret", PreviewURL: "https://cdn.invalid/preview?secret"}}}, nil
	}
	v, err := c.get(context.Background(), key, load)
	if err != nil || v.Tracks[0].Thumb == "" {
		t.Fatal("memory lost artwork")
	}
	path := filepath.Join(c.dir, key+".json")
	b, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(b), "secret") || strings.Contains(string(b), "https://") {
		t.Fatalf("unsafe disk payload or missing file: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatal("metadata permissions")
	}
	// Disk rows can be returned while the missing artwork is restored in background.
	restored := newBrowseCache(state)
	started, release := make(chan struct{}), make(chan struct{})
	disk, err := restored.get(context.Background(), key, func(ctx context.Context) (browseData, error) {
		close(started)
		select {
		case <-release:
			return load(ctx)
		case <-ctx.Done():
			return browseData{}, ctx.Err()
		}
	})
	if err != nil || disk.Tracks[0].Title != "song" {
		t.Fatalf("disk restore %+v, %v", disk, err)
	}
	waitBrowse(t, started)
	f := currentBrowseFlight(restored, key)
	close(release)
	waitBrowse(t, f.done)
	for i := range browseEntries + 4 {
		k := browseKey("deezer", "public", "lists", string(rune(i)))
		c.insert(k, &browseEntry{data: []byte(`{}`), used: time.Now()})
		c.writeDisk(k, []byte(`{}`), time.Now())
	}
	files, _ := os.ReadDir(c.dir)
	if len(c.entries) > browseEntries || len(files) > browseEntries || c.bytes > browseMemoryBytes {
		t.Fatal("cache bound exceeded")
	}

	blob := make([]byte, browseEntryBytes)
	for i := range 6 {
		c.insert(browseKey("large", "", "", string(rune(i))), &browseEntry{data: blob, used: time.Now()})
	}
	if c.bytes > browseMemoryBytes {
		t.Fatal("memory byte budget exceeded")
	}
	sparse, err := os.Create(filepath.Join(c.dir, "oversized.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(browseDiskBytes + 1); err != nil {
		t.Fatal(err)
	}
	sparse.Close()
	c.pruneDisk()
	files, _ = os.ReadDir(c.dir)
	var diskBytes int64
	for _, f := range files {
		st, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		diskBytes += st.Size()
	}
	if diskBytes > browseDiskBytes {
		t.Fatal("disk byte budget exceeded")
	}
	// Oversized and malformed disk entries are ignored, without failing browsing.
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.readDisk(key, time.Now()) != nil {
		t.Fatal("corrupt entry accepted")
	}
}

func BenchmarkBrowseWarm(b *testing.B) {
	c := newBrowseCache("")
	key := browseKey("netease", "owner", "tracks", "100")
	var calls atomic.Int64
	load := func(context.Context) (browseData, error) { calls.Add(1); return browseFixture("cached"), nil }
	_, _ = c.get(context.Background(), key, load)
	b.ResetTimer()
	for range b.N {
		_, _ = c.get(context.Background(), key, load)
	}
	b.StopTimer()
	b.ReportMetric(float64(calls.Load()-1), "upstream_calls")
}

type browseTestProvider struct{ lists, tracks atomic.Int32 }

func (p *browseTestProvider) Playlists(context.Context) ([]providers.Playlist, error) {
	p.lists.Add(1)
	return []providers.Playlist{{ID: "playlist", Title: "a", Count: 2}}, nil
}
func (p *browseTestProvider) Tracks(context.Context, string) ([]providers.Track, error) {
	p.tracks.Add(1)
	return []providers.Track{{ID: "one", Title: "one", Provider: "spotify"}, {ID: "two", Title: "two", Provider: "spotify"}}, nil
}
func TestBrowseGatewayPaginationUsesSameCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope=spotify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
	defer s.Close()
	p := &browseTestProvider{}
	s.sessions["guest-a"] = &Session{ID: "guest-a", Provider: "spotify", Guest: true, Ready: true, p: p, expires: time.Now().Add(time.Hour)}
	request := func(path string) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		s.tidal.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	var lists struct{ Items []struct{ UUID string } }
	if err := json.Unmarshal(request("/v1/users/1/playlists?limit=1"), &lists); err != nil || len(lists.Items) != 1 {
		t.Fatalf("lists %+v %v", lists, err)
	}
	request("/v1/users/1/favorites/playlists?limit=1")
	id := lists.Items[0].UUID
	request("/v1/playlists/" + id + "/tracks?offset=0&limit=1")
	second := request("/v1/playlists/" + id + "/items?offset=1&limit=1")
	if !strings.Contains(string(second), `"title":"two"`) {
		t.Fatalf("wrong second page: %s", second)
	}
	if p.lists.Load() != 1 || p.tracks.Load() != 1 {
		t.Fatalf("upstream lists=%d tracks=%d", p.lists.Load(), p.tracks.Load())
	}
}
func BenchmarkBrowseWarmTracks100(b *testing.B) {
	c := newBrowseCache("")
	key := browseKey("netease", "owner", "tracks", "100")
	var calls atomic.Int64
	load := func(context.Context) (browseData, error) {
		calls.Add(1)
		tracks := make([]browseTrack, 100)
		for i := range tracks {
			tracks[i] = browseTrack{Track: providers.Track{ID: "id", Title: "track", Artist: "artist", Provider: "netease"}}
		}
		return browseData{Tracks: tracks}, nil
	}
	_, _ = c.get(context.Background(), key, load)
	b.ResetTimer()
	for range b.N {
		_, _ = c.get(context.Background(), key, load)
	}
	b.StopTimer()
	b.ReportMetric(float64(calls.Load()-1), "upstream_calls")
}

type parallelBrowseSearch struct {
	browseTestProvider
	tracksStarted, listsStarted chan struct{}
}

func (p *parallelBrowseSearch) Search(ctx context.Context, _ string) ([]providers.Track, error) {
	close(p.tracksStarted)
	select {
	case <-p.listsStarted:
		return []providers.Track{{ID: "one", Title: "parallel", Provider: "spotify"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (p *parallelBrowseSearch) SearchPlaylists(ctx context.Context, _ string) ([]providers.Playlist, error) {
	close(p.listsStarted)
	select {
	case <-p.tracksStarted:
		return []providers.Playlist{{ID: "list", Title: "parallel"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestBrowseSearchStartsBothKindsTogether(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope=spotify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
	defer s.Close()
	p := &parallelBrowseSearch{tracksStarted: make(chan struct{}), listsStarted: make(chan struct{})}
	s.sessions["search"] = &Session{ID: "search", Provider: "spotify", Ready: true, p: p}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	s.tidal.ServeHTTP(w, httptest.NewRequest("GET", "/v1/search?query=parallel", nil).WithContext(ctx))
	var result struct {
		Tracks, Playlists struct{ TotalNumberOfItems int }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Tracks.TotalNumberOfItems != 1 || result.Playlists.TotalNumberOfItems != 1 {
		t.Fatalf("sequential search or missing result: %s (%v)", w.Body.String(), err)
	}
}
