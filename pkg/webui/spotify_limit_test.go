package webui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// spotifyDeck is a deck showing the Spotify source, logged in against a Web API
// with 120 playlists, 130 liked songs and 250 tracks in every playlist.
func spotifyDeck(t *testing.T, refuse func(n int, w http.ResponseWriter, r *http.Request) bool) (get func(path string) string, calls *atomic.Int32) {
	t.Helper()
	calls = &atomic.Int32{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if refuse != nil && refuse(n, w, r) {
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		page := func(total int, item func(i int) string) {
			var items []string
			for i := offset; i < min(offset+limit, total); i++ {
				items = append(items, item(i))
			}
			next := "null"
			if offset+limit < total {
				next = `"more"`
			}
			fmt.Fprintf(w, `{"items":[%s],"next":%s,"total":%d}`, strings.Join(items, ","), next, total)
		}
		track := func(i int) string {
			return fmt.Sprintf(`{"item":{"id":"t%d","name":"Track %d","type":"track","duration_ms":200000,"artists":[{"name":"A"}]}}`, i, i)
		}
		switch {
		case r.URL.Path == "/me/playlists":
			page(120, func(i int) string { return fmt.Sprintf(`{"id":"pl%d","name":"List %d","items":{"total":250}}`, i, i) })
		case r.URL.Path == "/me/tracks":
			page(130, track)
		case strings.HasPrefix(r.URL.Path, "/playlists/") && strings.HasSuffix(r.URL.Path, "/items"):
			page(250, track)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(api.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope=spotify\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
	t.Cleanup(s.Close)
	sp := &providers.Spotify{BaseURL: api.URL, Token: providers.Token{AccessToken: "own-app", Expiry: time.Now().Add(time.Hour)}}
	s.sessions["owner"] = &Session{ID: "owner", Provider: "spotify", Ready: true, p: sp, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
	return func(path string) string {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		s.tidal.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}, calls
}

// spotifyBrowse is what the deck asks the gateway for when the DJ opens the
// source and then one playlist; it returns that playlist's first page.
func spotifyBrowse(t *testing.T, get func(string) string) string {
	t.Helper()
	lists := get("/v1/users/1/playlists?limit=500&offset=0")
	for _, bad := range []string{"too many requests", ": busy", "(429)", "failed to load"} {
		if strings.Contains(lists, bad) {
			t.Fatalf("the playlist menu shows %q: %.300s", bad, lists)
		}
	}
	if n := strings.Count(lists, `"uuid"`); n != 121 { // 120 playlists and the liked songs
		t.Fatalf("%d playlists listed, want 121", n)
	}
	get("/v1/users/1/favorites/playlists?limit=500&offset=0")
	if liked := get("/v1/users/1/favorites/tracks?limit=500&offset=0"); strings.Count(liked, `"type":"track"`) != 130 {
		t.Fatalf("liked songs: %d rows", strings.Count(liked, `"type":"track"`))
	}
	_, rest, _ := strings.Cut(lists, `"title":"List 7"`) // keys are sorted: the uuid follows its title
	_, id, _ := strings.Cut(rest, `"uuid":"`)
	if id = id[:min(len(id), 36)]; len(id) != 36 {
		t.Fatalf("no playlist id in %.200s", lists)
	}
	get("/v1/playlists/" + id)
	first := get("/v1/playlists/" + id + "/items?limit=100&offset=0")
	get("/v1/playlists/" + id + "/items?limit=100&offset=100")
	get("/v1/playlists/" + id + "/tracks?limit=100&offset=200")
	return first
}

// #227 (a): opening the source and one playlist costs one request per page
// Spotify makes us read, and nothing more on the pages the deck asks for next.
func TestSpotifyBrowseRequestBound(t *testing.T) {
	get, calls := spotifyDeck(t, nil)
	if first := spotifyBrowse(t, get); strings.Count(first, `"type":"track"`) != 100 {
		t.Fatalf("first page of the playlist: %d rows", strings.Count(first, `"type":"track"`))
	}
	// 120 playlists: 3 pages; the liked count: 1; 130 liked songs: 3 pages; 250 tracks: 3 pages.
	const bound = 3 + 1 + 3 + 3
	if n := calls.Load(); n > bound {
		t.Fatalf("%d Web API requests to open the source and one playlist, want at most %d", n, bound)
	}
	spotifyBrowse(t, get) // the DJ goes back and opens it again
	if n := calls.Load(); n > bound {
		t.Fatalf("%d Web API requests after opening it twice, want still %d", n, bound)
	}
}

// #227 (b), as the deck sees it: Spotify limits the burst (here every fourth
// request, for a second). The menu and the playlist still load, with no error row.
func TestSpotifyBrowseSurvivesARateLimit(t *testing.T) {
	var refused atomic.Int32
	get, calls := spotifyDeck(t, func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n%4 != 2 || refused.Add(1) > 2 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		return true
	})
	start := time.Now()
	if first := spotifyBrowse(t, get); strings.Count(first, `"type":"track"`) != 100 {
		t.Fatalf("first page of the playlist: %d rows", strings.Count(first, `"type":"track"`))
	}
	if refused.Load() < 2 {
		t.Fatalf("the fixture refused %d requests, want 2", refused.Load())
	}
	// The 10 requests of TestSpotifyBrowseRequestBound, and each refused one again.
	if n := calls.Load(); n > 10+2 {
		t.Fatalf("%d Web API requests with two 429s, want at most 12", n)
	}
	if d := time.Since(start); d < time.Second || d > 8*time.Second {
		t.Fatalf("took %v: Retry-After 1 not honoured, or waited far longer", d)
	}
}

// A limit that lasts is said in words, with the wait, not as a raw status.
func TestRateLimitRowIsHumanReadable(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&providers.RateLimited{RetryAfter: 9*time.Minute + 10*time.Second}, "Spotify: busy, try again in 10 min / 繁忙，请 10 分钟后再试"},
		{&providers.RateLimited{RetryAfter: 20 * time.Second}, "Spotify: busy, try again in a minute / 繁忙，请一分钟后再试"},
		{fmt.Errorf("lists: %w", providers.StatusError(429)), "Spotify: busy, try again in a minute / 繁忙，请一分钟后再试"},
		{providers.StatusError(403), "Spotify: account not allowed (403) / 账号未获授权"},
	} {
		got := sourceFailedTitle("spotify", c.err)
		if got != c.want || strings.Contains(strings.ToLower(got), "too many requests") {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}
