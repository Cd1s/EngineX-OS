package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise public callers: the collectors already preserve the prefix. Known
// totals may fetch one companion page, but its rows must never cross a failure.
func TestPartial248Listings(t *testing.T) {
	for _, name := range []string{"deezer-lists", "deezer-tracks", "spotify-lists", "spotify-tracks", "spotify-likes", "netease-lists", "netease-albums", "netease-radios", "netease-programs", "netease-tracks", "soundcloud-lists", "soundcloud-saved", "soundcloud-tracks", "soundcloud-likes", "soundcloud-web", "beatport-lists", "beatport-tracks"} {
		for _, failure := range []error{context.DeadlineExceeded, context.Canceled, StatusError(401), StatusError(403), StatusError(404), StatusError(429)} {
			t.Run(name+"/"+failure.Error(), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				const base = "https://fixture.invalid"
				var mu sync.Mutex
				var firstOnce sync.Once
				firstFetched := make(chan struct{})
				calls := map[int]int{}
				response := func(r *http.Request, status int, body any) (*http.Response, error) {
					data, _ := json.Marshal(body)
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
				}
				client := &http.Client{Transport: spotifyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if name == "soundcloud-web" && (r.URL.Path == "/" || r.URL.Path == "/fixture.js") {
						body := `<script src="https://fixture.invalid/fixture.js"></script>`
						if r.URL.Path == "/fixture.js" {
							body = `client_id:"0123456789abcdef0123456789abcdef"`
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					}
					r.ParseForm()
					q := r.Form
					if r.URL.Path == "/me/tracks" && q.Get("limit") == "1" {
						return response(r, 404, nil) // independent optional count
					}
					page := 0
					switch {
					case strings.HasPrefix(name, "deezer"):
						v, _ := strconv.Atoi(q.Get("index"))
						page = v / 100
					case strings.HasPrefix(name, "spotify"):
						v, _ := strconv.Atoi(q.Get("offset"))
						size, _ := strconv.Atoi(q.Get("limit"))
						page = v / size
					case strings.HasPrefix(name, "beatport"):
						v, _ := strconv.Atoi(q.Get("page"))
						page = v - 1
					case name == "netease-tracks":
						if r.URL.Path == "/playlist/detail" {
							var ids []map[string]int
							for i := 1; i <= 801; i++ {
								ids = append(ids, map[string]int{"id": i})
							}
							return response(r, 200, map[string]any{"code": 200, "playlist": map[string]any{"trackIds": ids}})
						}
						v, _ := strconv.Atoi(strings.Split(q.Get("ids"), ",")[0])
						page = (v - 1) / 200
					case strings.HasPrefix(name, "netease"):
						paths := map[string]string{"netease-lists": "/user/playlist", "netease-albums": "/album/sublist", "netease-radios": "/dj/sublist", "netease-programs": "/dj/program"}
						if r.URL.Path != paths[name] {
							return response(r, 200, map[string]any{"code": 200})
						}
						v, _ := strconv.Atoi(q.Get("offset"))
						page = v / 100
					case name == "soundcloud-web":
						if r.URL.Path == "/playlists/1" {
							var refs []map[string]any
							for i := 1; i <= 201; i++ {
								row := map[string]any{"id": i}
								if i == 1 || i == 201 {
									row["title"] = "expanded"
								}
								refs = append(refs, row)
							}
							return response(r, 200, map[string]any{"tracks": refs})
						}
						v, _ := strconv.Atoi(strings.Split(q.Get("ids"), ",")[0])
						page = (v - 2) / 50
					default:
						if name == "soundcloud-saved" && r.URL.Path == "/me/playlists" {
							return response(r, 200, map[string]any{"collection": []any{map[string]any{"id": 9}}})
						}
						page, _ = strconv.Atoi(q.Get("page"))
					}
					mu.Lock()
					calls[page]++
					mu.Unlock()
					if page == 1 {
						if failure == context.Canceled {
							<-firstFetched // the first detail batch must precede cancellation
							cancel()
							return nil, failure
						}
						if status, ok := failure.(StatusError); ok {
							return response(r, int(status), nil)
						}
						return nil, failure
					}
					rows := []any{map[string]any{"id": 1, "name": "first", "title": "first"}, map[string]any{"id": 2, "name": "second", "title": "second"}}
					if page > 1 {
						rows = []any{map[string]any{"id": 999, "name": "later", "title": "later"}}
					}
					var body any
					switch {
					case strings.HasPrefix(name, "deezer"):
						body = map[string]any{"data": rows, "next": "more", "total": 500}
					case strings.HasPrefix(name, "spotify"):
						if name == "spotify-lists" {
							rows = []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}}
						} else {
							rows = []any{map[string]any{"item": map[string]any{"id": "1"}}, map[string]any{"item": map[string]any{"id": "2"}}}
						}
						if page > 1 {
							rows = []any{}
						}
						body = map[string]any{"items": rows, "next": "more", "total": 500}
					case strings.HasPrefix(name, "beatport"):
						body = map[string]any{"results": rows, "next": "more", "count": 500}
					case name == "netease-tracks":
						body = map[string]any{"code": 200, "songs": rows}
					case name == "netease-programs":
						body = map[string]any{"code": 200, "programs": []any{map[string]any{"mainSong": rows[0]}, map[string]any{"mainSong": rows[1]}}, "more": true}
					case strings.HasPrefix(name, "netease"):
						body = map[string]any{"code": 200, "playlist": rows, "data": rows, "djRadios": rows, "more": true, "hasMore": true}
					case name == "soundcloud-web":
						body = []any{map[string]any{"id": 2, "title": "second"}, map[string]any{"id": 51, "title": "last available"}}
					default:
						body = map[string]any{"collection": rows, "next_href": base + r.URL.Path + "?page=" + strconv.Itoa(page+1)}
					}
					if page == 0 {
						firstOnce.Do(func() { close(firstFetched) })
					}
					return response(r, 200, body)
				})}
				var p Provider
				id := "1"
				want := []string{"1", "2"}
				switch {
				case strings.HasPrefix(name, "deezer"):
					p = &Deezer{BaseURL: base, HTTP: client, UserID: "1"}
				case strings.HasPrefix(name, "spotify"):
					s := spotifyStoredTest(t)
					s.BaseURL, s.HTTP, s.Token = base, client, Token{AccessToken: "fixture", Expiry: time.Now().Add(time.Hour)}
					s.native = &spotifyLibraryFixture{fakeSpotifyNative: &fakeSpotifyNative{done: make(chan struct{})}}
					p = s
					if name == "spotify-likes" {
						id = SpotifyLikedID
					}
				case strings.HasPrefix(name, "netease"):
					p = &Netease{BaseURL: base, HTTP: client, UserID: 1}
					if name == "netease-programs" {
						id = "dj:1"
					}
					if name == "netease-albums" {
						want = []string{"album:1", "album:2"}
					}
					if name == "netease-radios" {
						want = []string{"dj:1", "dj:2"}
					}
				case strings.HasPrefix(name, "beatport"):
					p = &Beatport{BaseURL: base, HTTP: client, loaded: true, tok: BeatportToken{Access: "fixture", Expiry: time.Now().Add(time.Hour)}}
				default:
					s := &SoundCloud{BaseURL: base, HTTP: client, ClientID: "fixture", Secret: "fixture", Token: "fixture", UserID: 42}
					if name == "soundcloud-web" {
						s.ClientID, s.WebURL, s.SiteURL = "", base, base
						scWebMu.Lock()
						scWebIDs[base] = "fixture"
						scWebMu.Unlock()
						t.Cleanup(func() { scWebMu.Lock(); delete(scWebIDs, base); scWebMu.Unlock() })
						want = []string{"1", "2", "51"}
					}
					if name == "soundcloud-likes" {
						id = soundCloudLikesID
					}
					if name == "soundcloud-lists" {
						want = []string{soundCloudLikesID, "1", "2"}
					}
					if name == "soundcloud-saved" {
						want = []string{soundCloudLikesID, "9", "1", "2"}
					}
					p = s
				}
				var got []string
				var err error
				if strings.HasSuffix(name, "-lists") || name == "netease-albums" || name == "netease-radios" || name == "soundcloud-saved" {
					var lists []Playlist
					lists, err = p.Playlists(ctx)
					for _, v := range lists {
						got = append(got, v.ID)
					}
				} else {
					var tracks []Track
					tracks, err = p.Tracks(ctx, id)
					for _, v := range tracks {
						got = append(got, v.ID)
					}
				}
				if !errors.Is(err, failure) || !reflect.DeepEqual(got, want) {
					t.Errorf("prefix=%v error=%v; want %v and %v", got, err, want, failure)
				}
				mu.Lock()
				defer mu.Unlock()
				for page, count := range calls {
					wantCalls := 1
					if name == "soundcloud-web" && page == 1 && (failure == StatusError(401) || failure == StatusError(403)) {
						wantCalls = 2
					} // existing client-ID renewal
					if page > 2 || count != wantCalls {
						t.Errorf("page %d requested %d times: %v", page, count, calls)
					}
				}
				if calls[0] != 1 || calls[1] == 0 {
					t.Errorf("missing initial/failed page: %v", calls)
				}
			})
		}
	}
}

func TestPartial248ChartsAfterPlaylists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/my/playlists/" {
			io.WriteString(w, `{"results":[{"id":1,"name":"Mine"}]}`)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	b := &Beatport{BaseURL: srv.URL, loaded: true, tok: BeatportToken{Access: "fixture"}}
	lists, err := b.Playlists(context.Background())
	if len(lists) != 1 || lists[0].ID != "1" || !errors.Is(err, StatusError(503)) {
		t.Fatalf("prefix=%v err=%v; want own playlist and charts error", lists, err)
	}
}
