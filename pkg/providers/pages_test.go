package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The one-slot control runs the same complete request plan serially, as the
// original providers did. Both runs use fresh provider instances and an actual
// HTTP server with 300 ms per response, without the gateway's warm cache.
func TestColdListingLatency300ms(t *testing.T) {
	for _, name := range []string{"deezer-lists", "deezer-tracks", "spotify-lists", "spotify-tracks", "spotify-likes", "beatport-lists", "beatport-tracks", "netease-tracks", "soundcloud-web-tracks"} {
		t.Run(name, func(t *testing.T) {
			var active, peak, calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				n := active.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				defer active.Add(-1)
				select {
				case <-time.After(300 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if name == "netease-tracks" {
					if r.URL.Path == "/playlist/detail" {
						ids := []map[string]int{}
						for i := 1; i <= 800; i++ {
							ids = append(ids, map[string]int{"id": i})
						}
						json.NewEncoder(w).Encode(map[string]any{"code": 200, "playlist": map[string]any{"trackIds": ids}})
					} else {
						r.ParseForm()
						songs := []map[string]any{}
						for _, id := range strings.Split(r.Form.Get("ids"), ",") {
							i, _ := strconv.Atoi(id)
							songs = append(songs, map[string]any{"id": i, "name": id})
						}
						json.NewEncoder(w).Encode(map[string]any{"code": 200, "songs": songs})
					}
					return
				}
				if name == "soundcloud-web-tracks" {
					tracks := []map[string]any{}
					if r.URL.Path == "/playlists/1" {
						for i := 1; i <= 201; i++ {
							row := map[string]any{"id": i}
							if i == 1 {
								row["title"] = "1"
							}
							tracks = append(tracks, row)
						}
						json.NewEncoder(w).Encode(map[string]any{"tracks": tracks})
					} else {
						for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
							i, _ := strconv.Atoi(id)
							tracks = append(tracks, map[string]any{"id": i, "title": id})
						}
						json.NewEncoder(w).Encode(tracks)
					}
					return
				}
				q := r.URL.Query()
				size := 100
				page := 0
				if strings.HasSuffix(name, "-lists") && name != "deezer-lists" || name == "spotify-likes" {
					size = 50
				}
				switch {
				case strings.HasPrefix(name, "deezer"):
					v, _ := strconv.Atoi(q.Get("index"))
					page = v / size
				case strings.HasPrefix(name, "spotify"):
					v, _ := strconv.Atoi(q.Get("offset"))
					page = v / size
				case strings.HasPrefix(name, "beatport"):
					v, _ := strconv.Atoi(q.Get("page"))
					page = v - 1
				}
				if name == "spotify-lists" && r.URL.Path == "/me/tracks" {
					json.NewEncoder(w).Encode(map[string]int{"total": 1})
					return
				}
				if name == "beatport-lists" && r.URL.Path == "/catalog/charts/" {
					json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": 999, "name": "chart"}}})
					return
				}
				rows := []map[string]any{}
				for i := page*size + 1; i <= (page+1)*size; i++ {
					row := map[string]any{"id": i, "title": fmt.Sprint(i), "name": fmt.Sprint(i)}
					if strings.HasPrefix(name, "spotify") {
						row["id"] = fmt.Sprint(i)
						if !strings.HasSuffix(name, "-lists") {
							row = map[string]any{"track": row}
						}
					}
					rows = append(rows, row)
				}
				next := ""
				if page < 4 {
					next = "next"
				}
				var body any
				switch {
				case strings.HasPrefix(name, "deezer"):
					body = map[string]any{"data": rows, "next": next, "total": 5 * size}
				case strings.HasPrefix(name, "spotify"):
					body = map[string]any{"items": rows, "next": next, "total": 5 * size}
				case strings.HasPrefix(name, "beatport"):
					body = map[string]any{"results": rows, "next": next, "count": 5 * size}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()
			if name == "soundcloud-web-tracks" {
				scWebMu.Lock()
				scWebIDs[srv.URL] = "fixture00000000000000000000000000"
				scWebMu.Unlock()
				defer func() { scWebMu.Lock(); delete(scWebIDs, srv.URL); scWebMu.Unlock() }()
			}
			slots := &deezerPages
			switch {
			case strings.HasPrefix(name, "spotify"):
				slots = &spotifyPages
			case strings.HasPrefix(name, "beatport"):
				slots = &beatportPageSlots
			case strings.HasPrefix(name, "netease"):
				slots = &neteasePages
			case strings.HasPrefix(name, "soundcloud"):
				slots = &soundcloudPages
			}
			original := *slots
			defer func() { *slots = original }()
			var control []string
			var before time.Duration
			var beforeCalls int32
			for _, concurrency := range []int{1, 2} {
				*slots = make(chan struct{}, concurrency)
				calls.Store(0)
				peak.Store(0)
				var p Provider
				switch {
				case strings.HasPrefix(name, "deezer"):
					p = &Deezer{BaseURL: srv.URL, UserID: "1"}
				case strings.HasPrefix(name, "spotify"):
					p = &Spotify{BaseURL: srv.URL, Token: Token{AccessToken: "fixture"}}
				case strings.HasPrefix(name, "beatport"):
					p = &Beatport{BaseURL: srv.URL, loaded: true, tok: BeatportToken{Access: "fixture"}}
				case strings.HasPrefix(name, "netease"):
					p = &Netease{BaseURL: srv.URL}
				case strings.HasPrefix(name, "soundcloud"):
					p = &SoundCloud{WebURL: srv.URL, SiteURL: srv.URL, Token: "fixture"}
				}
				start := time.Now()
				ids := []string{}
				if strings.HasSuffix(name, "-lists") {
					lists, err := p.Playlists(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					for _, l := range lists {
						ids = append(ids, l.ID)
					}
				} else {
					id := "1"
					if name == "spotify-likes" {
						id = SpotifyLikedID
					}
					tracks, err := p.Tracks(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					for _, tr := range tracks {
						ids = append(ids, tr.ID)
					}
				}
				elapsed := time.Since(start)
				if concurrency == 1 {
					control = ids
					before = elapsed
					beforeCalls = calls.Load()
					continue
				}
				if len(ids) < 200 || !reflect.DeepEqual(ids, control) {
					t.Fatalf("incomplete or reordered result (%d/%d)", len(ids), len(control))
				}
				if calls.Load() != beforeCalls || peak.Load() > 2 {
					t.Fatalf("requests=%d/%d peak=%d", calls.Load(), beforeCalls, peak.Load())
				}
				if elapsed >= before*85/100 {
					t.Fatalf("no cold latency improvement: %v -> %v", before, elapsed)
				}
				t.Logf("300ms/request: serial=%s parallel=%s requests=%d peak=%d rows=%d", before.Round(time.Millisecond), elapsed.Round(time.Millisecond), calls.Load(), peak.Load(), len(ids))
			}
		})
	}
}

func TestPagesTotalsCursorsErrorsAndCancellation(t *testing.T) {
	for _, total := range []int{0, 2, 5, 1000000000} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			var calls atomic.Int32
			got, err := collectPages(context.Background(), make(chan struct{}, 2), 1, 8, func(i int) (listPage[int], error) {
				calls.Add(1)
				if i == 1 {
					time.Sleep(10 * time.Millisecond)
				}
				return listPage[int]{items: []int{i}, more: i < 4, total: total}, nil
			})
			if err != nil || !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4}) || calls.Load() > 6 {
				t.Fatalf("%v %v calls=%d", got, err, calls.Load())
			}
		})
	}
	for _, partial := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		slots := make(chan struct{}, 2)
		var mu sync.Mutex
		seen := map[int]bool{}
		got, err := collectPages(ctx, slots, 1, 10, func(i int) (listPage[int], error) {
			mu.Lock()
			seen[i] = true
			mu.Unlock()
			if i == 1 {
				if partial {
					return listPage[int]{}, StatusError(429)
				}
				cancel()
				return listPage[int]{}, ctx.Err()
			}
			return listPage[int]{items: []int{i}, more: true, total: 10}, nil
		})
		cancel()
		if err == nil || !reflect.DeepEqual(got, []int{0}) || len(slots) != 0 {
			t.Fatalf("%v %v", got, err)
		}
		if partial && !errors.Is(err, StatusError(429)) {
			t.Fatal(err)
		}
		for i := range seen {
			if i > 2 {
				t.Fatalf("request beyond failed wave: %d", i)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := collectPages(ctx, make(chan struct{}, 2), 1, 10, func(int) (listPage[int], error) { t.Error("fetch after cancellation"); return listPage[int]{}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPageBudgetSharedAcrossColdLists(t *testing.T) {
	slots := make(chan struct{}, 2)
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := collectBatches(context.Background(), slots, 4, func(i int) ([]int, error) {
				n := active.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(5 * time.Millisecond)
				active.Add(-1)
				return []int{i}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 2 || len(slots) != 0 {
		t.Fatalf("peak=%d held=%d", peak.Load(), len(slots))
	}
	got, err := collectBatches(context.Background(), slots, 5, func(i int) ([]int, error) {
		if i == 1 {
			return nil, StatusError(429)
		}
		return []int{i}, nil
	})
	if !reflect.DeepEqual(got, []int{0}) || !errors.Is(err, StatusError(429)) {
		t.Fatalf("%v %v", got, err)
	}
}
