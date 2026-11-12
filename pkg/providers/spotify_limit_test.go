package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// spotifyTestClock gives a login its own clock and sleep, before its first
// request: a wait moves the clock instead of the test. It returns the waits
// made so far.
func spotifyTestClock(p *Spotify) (advance func(time.Duration), slept func() []time.Duration) {
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	var waits []time.Duration
	p.limit.clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p.limit.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		now = now.Add(d)
		return nil
	}
	return func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() },
		func() []time.Duration { mu.Lock(); defer mu.Unlock(); return append([]time.Duration(nil), waits...) }
}

// spotifyListsSettled waits for the liked-songs counts that Playlists started
// and, when the lists failed, did not wait for: their requests must not land
// after the test has read the request count or moved the clock.
func spotifyListsSettled(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for until := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		if !bytes.Contains(buf[:n], []byte("apiPlaylists")) {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("a liked-songs count is still running:\n%s", buf[:n])
		}
	}
}

// spotifyTestAPI is a Web API with 120 playlists, 130 liked songs and 250
// tracks in every playlist. refuse may answer a request itself (a 429).
func spotifyTestAPI(t *testing.T, refuse func(n int, w http.ResponseWriter, r *http.Request) bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			page(120, func(i int) string {
				return fmt.Sprintf(`{"id":"pl%d","name":"List %d","items":{"total":250}}`, i, i)
			})
		case r.URL.Path == "/me/tracks":
			page(130, track)
		case strings.HasPrefix(r.URL.Path, "/playlists/") && strings.HasSuffix(r.URL.Path, "/items"):
			page(250, track)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// #227 (b): a burst that ran into the rolling limit is waited out, as long as
// Retry-After says, and the listing succeeds.
func TestSpotifyShortRateLimitIsWaitedOut(t *testing.T) {
	var refused atomic.Int32
	srv, calls := spotifyTestAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) bool {
		// The second page of each listing is refused once.
		if r.URL.Query().Get("offset") == "0" || r.URL.Query().Get("limit") == "1" || refused.Add(1) > 1 {
			return false
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		return true
	})
	p := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "own-app"}}
	_, slept := spotifyTestClock(p)
	lists, err := p.Playlists(context.Background())
	if err != nil || len(lists) != 121 || lists[0].ID != SpotifyLikedID || lists[0].Count != 130 {
		t.Fatalf("playlists after a 2 s limit: %d lists, %v", len(lists), err)
	}
	waits := slept()
	if len(waits) == 0 || len(waits) > 2 {
		t.Fatalf("waits %v, want the one limit sat out (the parallel page may share it)", waits)
	}
	for _, d := range waits {
		if d <= 0 || d > 2*time.Second {
			t.Fatalf("waited %v, Retry-After said 2 s", d)
		}
	}
	// 3 pages of playlists, the liked count, and the one refused request again.
	if n := calls.Load(); n != 5 {
		t.Fatalf("%d requests for 120 playlists and one 429, want 5", n)
	}
	refused.Store(0)
	tracks, err := p.Tracks(context.Background(), "pl7")
	if err != nil || len(tracks) != 250 || tracks[249].ID != "t249" {
		t.Fatalf("tracks after a limit in the middle of the pages: %d, %v", len(tracks), err)
	}
	if n := calls.Load(); n != 5+4 {
		t.Fatalf("%d requests for a 250-track playlist and one 429, want 4", n-5)
	}
}

// #227 (a): a QR login is made with the built-in desktop client, which the Web
// API limits on its first request and keeps limited. The deck must browse over
// the protocol session, and must not keep asking the Web API while it does.
func TestSpotifyQRLoginBrowsesNativelyWhenTheWebAPILimitsIt(t *testing.T) {
	for _, retryAfter := range []string{"1", "3600", ""} {
		t.Run("Retry-After "+retryAfter, func(t *testing.T) {
			srv, calls := spotifyTestAPI(t, func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.WriteHeader(429)
				return true
			})
			p := spotifyStoredTest(t)
			advance, slept := spotifyTestClock(p)
			lib := &spotifyLibraryFixture{fakeSpotifyNative: &fakeSpotifyNative{done: make(chan struct{})}}
			p.native = lib
			p.BaseURL = srv.URL
			p.Token = Token{AccessToken: "device-grant", RefreshToken: "r", ClientID: SpotifyBuiltinClientID, Expiry: time.Now().Add(time.Hour)}
			open := func() {
				t.Helper()
				// What the deck asks for: the source's lists (twice: lists and
				// favourite lists), the liked songs, then one playlist.
				defer spotifyListsSettled(t)
				for i := 0; i < 2; i++ {
					if lists, err := p.Playlists(context.Background()); err != nil || len(lists) != 2 {
						t.Fatalf("playlists: %v %v", lists, err)
					}
				}
				for _, id := range []string{SpotifyLikedID, "private"} {
					if tracks, err := p.Tracks(context.Background(), id); err != nil || len(tracks) != 1 || tracks[0].Title != "Native" {
						t.Fatalf("tracks of %s: %v %v", id, tracks, err)
					}
				}
			}
			open()
			// The bound: the lists page and the liked count that run together.
			if n := calls.Load(); n > 2 {
				t.Fatalf("%d Web API requests for login + open source + open one playlist, want at most 2", n)
			}
			first := calls.Load()
			for i := 0; i < 3; i++ { // the DJ opens the source again and again
				open()
			}
			if n := calls.Load(); n != first {
				t.Fatalf("%d more Web API requests while it is limited", n-first)
			}
			if w := slept(); len(w) != 0 {
				t.Fatalf("the deck waited %v although the protocol session can serve the lists", w)
			}
			// It is asked again later, but ever less often.
			advance(spotifyBlockCap + time.Second)
			open()
			again := calls.Load()
			if again == first || again > first+2 {
				t.Fatalf("%d requests after the pause, want 1 or 2", again-first)
			}
			advance(spotifyBlockFloor + time.Second)
			open()
			if n := calls.Load(); n != again {
				t.Fatalf("asked again %v after the second refusal", spotifyBlockFloor)
			}
			if lib.playlistCalls == 0 || lib.trackCalls == 0 {
				t.Fatal("the protocol session was not used")
			}
		})
	}
}

// A login without a protocol session (an own app's token) cannot fall back: a
// limit that lasts is reported with its wait, and not asked about again until
// it is over.
func TestSpotifyLastingRateLimitStopsAsking(t *testing.T) {
	limited := atomic.Bool{}
	limited.Store(true)
	srv, calls := spotifyTestAPI(t, func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		if !limited.Load() {
			return false
		}
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		return true
	})
	p := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "own-app"}}
	advance, slept := spotifyTestClock(p)
	for i := 0; i < 5; i++ {
		_, err := p.Playlists(context.Background())
		var status StatusError
		var wait *RateLimited
		if !errors.As(err, &status) || status != 429 || !errors.As(err, &wait) || wait.RetryAfter <= time.Minute || wait.RetryAfter > spotifyBlockCap {
			t.Fatalf("call %d: %v (%+v), want a 429 with a wait of at most %v", i, err, wait, spotifyBlockCap)
		}
		if _, err := p.Tracks(context.Background(), "pl1"); !errors.As(err, &wait) {
			t.Fatalf("tracks during the limit: %v", err)
		}
	}
	spotifyListsSettled(t)
	if n := calls.Load(); n > 2 {
		t.Fatalf("%d requests during an hour-long limit, want the first two only", n)
	}
	if w := slept(); len(w) != 0 {
		t.Fatalf("sat out %v of an hour-long limit inside a request", w)
	}
	limited.Store(false)
	advance(spotifyBlockCap + time.Second)
	if lists, err := p.Playlists(context.Background()); err != nil || len(lists) != 121 {
		t.Fatalf("after the limit: %d lists, %v", len(lists), err)
	}
}

// Short waits that never end are not sat out for ever.
func TestSpotifyRepeatedShortLimitGivesUp(t *testing.T) {
	srv, calls := spotifyTestAPI(t, func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		return true
	})
	p := &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "own-app"}}
	_, slept := spotifyTestClock(p)
	if _, err := p.Tracks(context.Background(), "pl1"); !errors.Is(err, StatusError(429)) {
		t.Fatalf("tracks: %v", err)
	}
	if n, w := calls.Load(), slept(); n != spotifyRetries+1 || len(w) != spotifyRetries {
		t.Fatalf("%d requests, waits %v: want %d tries", n, w, spotifyRetries+1)
	}
	if _, err := p.Tracks(context.Background(), "pl1"); err == nil || calls.Load() != spotifyRetries+1 {
		t.Fatalf("asked again right after giving up: %v, %d requests", err, calls.Load())
	}
}

// The 429 error keeps reading and matching as the plain status it replaced.
func TestRateLimitedMatchesStatusError(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"": 0, "7": 7 * time.Second, " 30 ": 30 * time.Second, "-5": 0, "soon": 0, "999999999": 24 * time.Hour,
		"Fri, 13 Nov 2026 10:00:20 GMT": 20 * time.Second, "Fri, 13 Nov 2026 09:00:00 GMT": 0,
	} {
		if got := retryAfter(in, now); got != want {
			t.Errorf("Retry-After %q: %v, want %v", in, got, want)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	err := JSON(context.Background(), nil, "GET", srv.URL, nil, nil, &struct{}{})
	var status StatusError
	var limited *RateLimited
	if !errors.As(err, &status) || status != 429 || !errors.Is(err, StatusError(429)) || errors.Is(err, StatusError(404)) ||
		err.Error() != "provider: HTTP 429" || !errors.As(err, &limited) || limited.RetryAfter != 12*time.Second {
		t.Fatalf("429 reads as %v (%T, status %d)", err, err, status)
	}
	if wrapped := fmt.Errorf("lists: %w", err); !errors.As(wrapped, &status) || status != 429 {
		t.Fatal("a wrapped 429 lost its status")
	}
}
