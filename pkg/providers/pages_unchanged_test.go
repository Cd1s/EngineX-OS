package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Cursor APIs cannot safely predict a later page. Record their unchanged cold
// cost too; a partial prefix would give different global sort results.
func TestColdCursorLatency300ms(t *testing.T) {
	for _, name := range []string{"applemusic-lists", "applemusic-tracks", "soundcloud-likes", "soundcloud-lists", "netease-lists"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				time.Sleep(300 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				r.ParseForm()
				page, _ := strconv.Atoi(r.Form.Get("offset"))
				page /= 100
				if strings.HasPrefix(name, "soundcloud") {
					page, _ = strconv.Atoi(r.Form.Get("page"))
				}
				if name == "netease-lists" && r.URL.Path != "/user/playlist" {
					fmt.Fprint(w, `{"code":200}`)
					return
				}
				if name == "soundcloud-lists" && strings.Contains(r.URL.Path, "likes") {
					fmt.Fprint(w, `{"collection":[]}`)
					return
				}
				rows := []map[string]any{}
				for i := page*100 + 1; i <= (page+1)*100; i++ {
					row := map[string]any{"id": i, "title": fmt.Sprint(i), "name": fmt.Sprint(i)}
					if strings.HasPrefix(name, "applemusic") {
						row = song(fmt.Sprint(i), fmt.Sprint(i), "")
					}
					rows = append(rows, row)
				}
				next := ""
				if page < 4 {
					next = r.URL.Path + "?offset=" + strconv.Itoa((page+1)*100)
					if strings.HasPrefix(name, "soundcloud") {
						next = base + r.URL.Path + "?page=" + strconv.Itoa(page+1)
					}
				}
				switch {
				case strings.HasPrefix(name, "applemusic"):
					json.NewEncoder(w).Encode(map[string]any{"data": rows, "next": next})
				case strings.HasPrefix(name, "soundcloud"):
					json.NewEncoder(w).Encode(map[string]any{"collection": rows, "next_href": next})
				default:
					json.NewEncoder(w).Encode(map[string]any{"code": 200, "playlist": rows, "more": page < 4})
				}
			}))
			defer srv.Close()
			base = srv.URL
			for run := 0; run < 2; run++ {
				calls.Store(0)
				var p Provider
				id := "1"
				switch {
				case strings.HasPrefix(name, "applemusic"):
					p = &AppleMusic{BaseURL: base, owner: AppleMusicOwner{DevToken: "fixture", UserToken: "fixture"}}
					id = "pl:us:1"
				case strings.HasPrefix(name, "soundcloud"):
					p = &SoundCloud{BaseURL: base, ClientID: "fixture", Secret: "fixture", Token: "fixture"}
					id = "likes"
				default:
					p = &Netease{BaseURL: base, UserID: 1}
				}
				start := time.Now()
				var err error
				count := 0
				if strings.HasSuffix(name, "-lists") {
					var rows []Playlist
					rows, err = p.Playlists(context.Background())
					count = len(rows)
				} else {
					var rows []Track
					rows, err = p.Tracks(context.Background(), id)
					count = len(rows)
				}
				if err != nil || count < 500 {
					t.Fatalf("rows=%d err=%v", count, err)
				}
				t.Logf("unchanged run=%d cold=%s requests=%d rows=%d", run, time.Since(start).Round(time.Millisecond), calls.Load(), count)
			}
		})
	}
}

func TestColdSelfHostedLatency300ms(t *testing.T) {
	for _, protocol := range []string{"subsonic", "webdav", "plex"} {
		t.Run(protocol, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				time.Sleep(300 * time.Millisecond)
				switch protocol {
				case "webdav":
					w.WriteHeader(207)
					fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"></d:multistatus>`)
				case "subsonic":
					fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlists":{"playlist":[]},"playlist":{"entry":[]}}}`)
				case "plex":
					fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
				}
			}))
			defer srv.Close()
			p := NewSelfHostedProvider(NASConfig{Protocol: protocol, URL: srv.URL}, nil)
			for run := 0; run < 2; run++ {
				start := time.Now()
				if _, err := p.Playlists(context.Background()); err != nil {
					t.Fatal(err)
				}
				lists := time.Since(start)
				start = time.Now()
				if _, err := p.Tracks(context.Background(), "1"); err != nil {
					t.Fatal(err)
				}
				t.Logf("unchanged run=%d lists=%s tracks=%s requests=1 each", run, lists.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
			}
			if calls.Load() != 4 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}
