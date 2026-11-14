package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type partial248Provider struct {
	provider      string
	cause         error
	recovered     atomic.Bool
	lists, tracks atomic.Int32
}

func (p *partial248Provider) Playlists(context.Context) ([]providers.Playlist, error) {
	p.lists.Add(1)
	rows := []providers.Playlist{{ID: "one", Title: "one"}, {ID: "two", Title: "two"}, {ID: "three", Title: "three"}}
	if !p.recovered.Load() {
		return rows[:2], p.cause
	}
	return rows, nil
}

func (p *partial248Provider) Tracks(context.Context, string) ([]providers.Track, error) {
	p.tracks.Add(1)
	rows := []providers.Track{{ID: "one", Title: "one", Provider: p.provider}, {ID: "two", Title: "two", Provider: p.provider}, {ID: "three", Title: "three", Provider: p.provider}}
	if !p.recovered.Load() {
		return rows[:2], p.cause
	}
	return rows, nil
}

func TestGatewayPartial248ShowsRowsAndOneUnavailableNotice(t *testing.T) {
	for _, scope := range []string{"spotify", "netease", "soundcloud"} {
		for _, cause := range []error{context.DeadlineExceeded, providers.StatusError(401), providers.StatusError(403)} {
			for _, kind := range []string{"playlists", "favoritePlaylists", "tracks", "items", "likes"} {
				t.Run(scope+"/"+cause.Error()+"/"+kind, func(t *testing.T) {
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope="+scope+"\nsources="+scope+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
					defer s.Close()
					p := &partial248Provider{provider: scope, cause: cause}
					s.sessions["owner"] = &Session{ID: "owner", Provider: scope, Ready: true, p: p, expires: time.Now().Add(time.Hour)}
					clock := cacheClock(s.tidal.browse)
					id := s.tidal.addPlaylist(playlistRef{"owner", scope, providers.Playlist{ID: "one", Title: "one"}, false})
					path := "/v1/playlists/" + id + "/" + kind
					if kind == "playlists" {
						path = "/v1/users/1/playlists"
					}
					if kind == "favoritePlaylists" {
						path = "/v1/users/1/favorites/playlists"
					}
					if kind == "likes" {
						path = "/v1/users/1/favorites/tracks"
					}
					get := func() []map[string]any {
						t.Helper()
						w := httptest.NewRecorder()
						r := httptest.NewRequest("GET", path, nil)
						r.RemoteAddr = "127.0.0.1:1234"
						s.tidal.ServeHTTP(w, r)
						var page struct{ Items []map[string]any }
						if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
							t.Fatalf("listing: %d %s", w.Code, w.Body.String())
						}
						return page.Items
					}
					for range 2 {
						rows := get()
						if len(rows) != 3 || rows[0]["title"] != "one" || rows[1]["title"] != "two" || !strings.Contains(fmt.Sprint(rows[2]["title"]), "List incomplete") {
							t.Fatalf("prefix/notice missing or reordered: %+v", rows)
						}
						if kind != "playlists" && kind != "favoritePlaylists" && (rows[2]["allowStreaming"] != false || rows[2]["streamReady"] != false) {
							t.Fatal("notice can be loaded as audio")
						}
						if kind == "playlists" || kind == "favoritePlaylists" {
							w := httptest.NewRecorder()
							r := httptest.NewRequest("GET", "/v1/playlists/"+rows[2]["uuid"].(string), nil)
							r.RemoteAddr = "127.0.0.1:1234"
							s.tidal.ServeHTTP(w, r)
							var detail struct{ Title string }
							if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Title != rows[2]["title"] {
								t.Fatal("opening a partial-list notice replaced it with a login hint")
							}
						}
						if kind == "tracks" || kind == "items" || kind == "likes" {
							id := fmt.Sprintf("%.0f", rows[2]["id"])
							for _, endpoint := range []string{"/v1/tracks/" + id + "/playbackinfopostpaywall", "/v1/stream/" + id + ".mp3"} {
								w := httptest.NewRecorder()
								r := httptest.NewRequest("GET", endpoint, nil)
								r.RemoteAddr = "127.0.0.1:1234"
								s.tidal.ServeHTTP(w, r)
								if w.Code != 404 {
									t.Fatalf("notice reached playback: %s: %d", endpoint, w.Code)
								}
							}
						}
						var status providers.StatusError
						if errors.As(cause, &status) && !strings.Contains(fmt.Sprint(rows[2]), fmt.Sprint(int(status))) {
							t.Fatal("auth status/re-login hint lost")
						}
					}
					wantCalls := int32(1)
					if scope == "netease" && kind == "likes" {
						wantCalls = 2
					}
					if p.lists.Load()+p.tracks.Load() != wantCalls {
						t.Fatal("implicit retry or provider substitution")
					}
					files, _ := os.ReadDir(s.tidal.browse.dir)
					if len(files) != 0 {
						t.Fatal("partial result cached as complete")
					}
					p.recovered.Store(true)
					clock.Add(int64(browseRetry + time.Second))
					rows := get()
					if len(rows) != 3 || rows[2]["title"] != "three" {
						t.Fatalf("retry kept partial result/notice: %+v", rows)
					}
					if s.scope() != scope || s.sessions["owner"].p != p {
						t.Fatal("carrier/source session changed")
					}
				})
			}
		}
	}
}

func TestBrowsePartial248KeepsPrefixAndErrorUntilRetry(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, providers.StatusError(401), providers.StatusError(403)} {
		t.Run(cause.Error(), func(t *testing.T) {
			c := newBrowseCache(t.TempDir())
			clock := cacheClock(c)
			key := browseKey("spotify", "owner", "tracks", "playlist")
			var calls atomic.Int32
			load := func(context.Context) (browseData, error) {
				calls.Add(1)
				return browseData{
					Lists:  []providers.Playlist{{ID: "one"}, {ID: "two"}},
					Tracks: []browseTrack{{Track: providers.Track{ID: "one"}}, {Track: providers.Track{ID: "two"}}},
				}, cause
			}
			for range 2 { // Native pagination shares the same partial snapshot.
				data, err := c.get(context.Background(), key, load)
				if !errors.Is(err, cause) || len(data.Lists) != 2 || len(data.Tracks) != 2 || data.Tracks[1].ID != "two" {
					t.Fatalf("prefix/error lost: %+v, %v", data, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("implicit retries: %d", calls.Load())
			}
			if _, err := os.Stat(filepath.Join(c.dir, key+".json")); !os.IsNotExist(err) {
				t.Fatalf("partial list persisted as complete: %v", err)
			}
			clock.Add(int64(browseRetry + time.Second))
			nextFailure := errors.New("retry temporarily offline")
			partial, retryErr := c.get(context.Background(), key, func(context.Context) (browseData, error) {
				calls.Add(1)
				return browseData{}, nextFailure
			})
			if len(partial.Lists) != 2 || len(partial.Tracks) != 2 || !errors.Is(retryErr, nextFailure) {
				t.Fatalf("failed retry discarded the existing prefix: %+v, %v", partial, retryErr)
			}
			var auth providers.StatusError
			if errors.As(cause, &auth) && !errors.Is(retryErr, auth) {
				t.Fatal("transport failure during retry hid the pending re-login action")
			}
			clock.Add(int64(browseRetry + time.Second))
			data, err := c.get(context.Background(), key, func(context.Context) (browseData, error) {
				calls.Add(1)
				return browseFixture("complete"), nil
			})
			if err != nil || len(data.Lists) != 1 || data.Lists[0].Title != "complete" || calls.Load() != 3 {
				t.Fatalf("successful retry did not replace partial: %+v, %v, calls=%d", data, err, calls.Load())
			}
		})
	}
}

func TestGatewayPartial248OmniKeepsOneNoticeAndAuthError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope=soundcloud\nomni_source=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir(), MediaRoot: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
	defer s.Close()
	for provider, cause := range map[string]error{"soundcloud": context.DeadlineExceeded, "spotify": providers.StatusError(403)} {
		s.sessions[provider] = &Session{ID: provider, Provider: provider, Ready: true, p: &partial248Provider{provider: provider, cause: cause}, expires: time.Now().Add(time.Hour)}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/users/1/playlists", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	s.tidal.ServeHTTP(w, r)
	var page struct{ Items []map[string]any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("listing: %d %s", w.Code, w.Body.String())
	}
	if len(page.Items) != 5 || page.Items[2]["title"] != "[Spotify] one" || !strings.Contains(fmt.Sprint(page.Items[4]), "403") || !strings.Contains(fmt.Sprint(page.Items[4]["description"]), "Spotify") {
		t.Fatalf("partial aggregate/auth notice lost: %+v", page.Items)
	}
}

func TestGatewayPartial248AlbumAndPublicChartRoutes(t *testing.T) {
	for _, path := range []string{"/v1/albums/7/tracks", "/v1/albums/7/items", "/v1/users/1/playlists"} {
		t.Run(path, func(t *testing.T) {
			scope, session, operation, id := "netease", "owner", "tracks", "album:7"
			data := browseData{Tracks: []browseTrack{{Track: providers.Track{ID: "one", Title: "one", Provider: scope}}, {Track: providers.Track{ID: "two", Title: "two", Provider: scope}}}}
			if strings.Contains(path, "/users/") {
				scope, session, operation, id = "deezer", "public", "charts", ""
				data = browseData{Lists: []providers.Playlist{{ID: "one", Title: "one"}, {ID: "two", Title: "two"}}}
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("scope="+scope+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{UIDir: dir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) { return nil, errors.New("unconfigured") }}, nil)
			defer s.Close()
			if scope == "netease" {
				s.sessions[session] = &Session{ID: session, Provider: scope, Ready: true, p: &providers.Netease{Cookie: "fixture", UserID: 1}, expires: time.Now().Add(time.Hour)}
			}
			_, _ = s.tidal.browse.get(context.Background(), browseKey(scope, session, operation, id), func(context.Context) (browseData, error) { return data, context.DeadlineExceeded })
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", path, nil)
			r.RemoteAddr = "127.0.0.1:1234"
			s.tidal.ServeHTTP(w, r)
			var page struct{ Items []map[string]any }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 3 || page.Items[0]["title"] != "one" || page.Items[1]["title"] != "two" || !strings.Contains(fmt.Sprint(page.Items[2]["title"]), "List incomplete") {
				t.Fatalf("partial typed route: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
