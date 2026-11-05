package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"ES256","kid":"TESTKEY0000"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"iss":"TEAM","iat":1,"exp":%d,"root_https_origin":["apple.com"]}`, exp.Unix()))) + "." +
		enc([]byte("signature-bytes-that-are-long-enough"))
}

func song(id, name, isrc string) map[string]any {
	return map[string]any{"id": id, "type": "songs", "attributes": map[string]any{
		"name": name, "artistName": "Artist " + id, "albumName": "Album", "isrc": isrc, "durationInMillis": 200000,
		"artwork": map[string]any{"url": "https://is1.mzstatic.com/image/{w}x{h}bb.jpg"}}}
}

// appleMusicServer fakes the web player and the API; scrapes counts page fetches.
func appleMusicServer(t *testing.T, scrapes *atomic.Int32, revoked *atomic.Bool) *httptest.Server {
	token := fakeJWT(time.Now().Add(30 * 24 * time.Hour))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := func(v any) { json.NewEncoder(w).Encode(v) }
		switch {
		case r.URL.Path == "/us/browse":
			scrapes.Add(1)
			fmt.Fprint(w, `<html><script type="module" crossorigin src="/assets/index~a1b2c3.js"></script></html>`)
			return
		case r.URL.Path == "/assets/index~a1b2c3.js":
			fmt.Fprintf(w, `var x="noise";const t="%s";export{t}`, token)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Origin") != "https://music.apple.com" || revoked.Swap(false) {
			w.WriteHeader(401)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/me/") && r.Header.Get("Media-User-Token") != "user-token" {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/v1/catalog/cn/playlists/pl.abc":
			send(map[string]any{"data": []any{map[string]any{"id": "pl.abc", "type": "playlists", "attributes": map[string]any{"name": "Club Set"},
				"relationships": map[string]any{"tracks": map[string]any{"data": []any{song("1", "One", "ISRC1")}}}}}})
		case "/v1/catalog/cn/playlists/pl.abc/tracks":
			if r.URL.Query().Get("offset") == "" {
				send(map[string]any{"data": []any{song("1", "One", "ISRC1"), map[string]any{"id": "v", "type": "music-videos", "attributes": map[string]any{"name": "Video"}}},
					"next": "/v1/catalog/cn/playlists/pl.abc/tracks?offset=2"})
			} else {
				send(map[string]any{"data": []any{song("2", "Two", "ISRC2")}})
			}
		case "/v1/catalog/us/search":
			if r.URL.Query().Get("types") != "songs" || r.URL.Query().Get("term") != "daft punk" {
				w.WriteHeader(400)
				return
			}
			send(map[string]any{"results": map[string]any{"songs": map[string]any{"data": []any{song("9", "One More Time", "GBDUW0000059")}}}})
		case "/v1/me/library/playlists":
			send(map[string]any{"data": []any{map[string]any{"id": "p.lib1", "type": "library-playlists", "attributes": map[string]any{"name": "My Favourites"}}}})
		case "/v1/me/library/playlists/p.lib1/tracks":
			if r.URL.Query().Get("include") != "catalog" {
				w.WriteHeader(400)
				return
			}
			send(map[string]any{"data": []any{map[string]any{"id": "i.lib", "type": "library-songs",
				"attributes":    map[string]any{"name": "Mine", "artistName": "Me", "durationInMillis": 1000},
				"relationships": map[string]any{"catalog": map[string]any{"data": []any{song("5", "Mine", "ISRC5")}}}}}})
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestParseAppleMusicLinks(t *testing.T) {
	got := ParseAppleMusicLinks("look https://music.apple.com/cn/playlist/%E6%B4%BE%E5%AF%B9/pl.u-AkAm8ZKCzBN9?l=en and " +
		"https://music.apple.com/us/album/discovery/697194953 https://music.apple.com/cn/playlist/x/pl.u-AkAm8ZKCzBN9 " +
		"https://example.com/us/playlist/x/pl.1 https://music.apple.com/jp/artist/x/123")
	want := []AppleMusicLink{{"cn", "playlist", "pl.u-AkAm8ZKCzBN9"}, {"us", "album", "697194953"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v", got)
	}
}

func TestAppleMusicLinksLibraryAndSearch(t *testing.T) {
	var scrapes atomic.Int32
	var revoked atomic.Bool
	srv := appleMusicServer(t, &scrapes, &revoked)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "applemusic-owner.json")
	a := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL, OwnerPath: path}
	if a.Ready() {
		t.Fatal("ready without a login")
	}
	if err := a.SetOwner(t.Context(), AppleMusicOwner{Links: []string{"https://music.apple.com/cn/playlist/x/pl.missing"}}); err == nil {
		t.Fatal("accepted a link that does not open")
	}
	if err := a.SetOwner(t.Context(), AppleMusicOwner{UserToken: "wrong"}); err == nil {
		t.Fatal("accepted a refused media-user-token")
	}
	err := a.SetOwner(t.Context(), AppleMusicOwner{Links: []string{"https://music.apple.com/cn/playlist/club-set/pl.abc"}, UserToken: "user-token", Storefront: "US"})
	if err != nil || !a.Ready() {
		t.Fatal(err)
	}
	// A fresh provider reads the saved login back.
	b := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL, OwnerPath: path}
	if err := b.Load(); err != nil || !b.Ready() {
		t.Fatal("saved login not loaded", err)
	}
	lists, err := b.Playlists(t.Context())
	if err != nil || len(lists) != 2 || lists[0] != (Playlist{"pl:cn:pl.abc", "Club Set", 1}) || lists[1].ID != "lib:p.lib1" {
		t.Fatalf("playlists %+v %v", lists, err)
	}
	tracks, err := b.Tracks(t.Context(), "pl:cn:pl.abc")
	if err != nil || len(tracks) != 2 || tracks[1].ISRC != "ISRC2" || tracks[0].CoverURL != "https://is1.mzstatic.com/image/600x600bb.jpg" || tracks[0].Provider != "applemusic" {
		t.Fatalf("tracks %+v %v", tracks, err)
	}
	lib, err := b.Tracks(t.Context(), "lib:p.lib1")
	if err != nil || len(lib) != 1 || lib[0].ISRC != "ISRC5" || lib[0].CoverURL == "" {
		t.Fatalf("library tracks %+v %v", lib, err)
	}
	found, err := b.Search(t.Context(), "daft punk")
	if err != nil || len(found) != 1 || found[0].ISRC != "GBDUW0000059" {
		t.Fatalf("search %+v %v", found, err)
	}
	// The scraped token is cached, and fetched again once after a 401.
	before := scrapes.Load()
	revoked.Store(true)
	if _, err := b.Search(t.Context(), "daft punk"); err != nil || scrapes.Load() != before+1 {
		t.Fatalf("401 refresh: %v, scrapes %d -> %d", err, before, scrapes.Load())
	}
	if _, err := b.Tracks(t.Context(), "pl:cn/../x:1"); err == nil {
		t.Fatal("accepted an invalid playlist id")
	}
}

func TestAppleMusicDeveloperTokenOverride(t *testing.T) {
	var scrapes atomic.Int32
	var revoked atomic.Bool
	srv := appleMusicServer(t, &scrapes, &revoked)
	defer srv.Close()
	a := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL}
	a.owner = AppleMusicOwner{DevToken: "own-token", Storefront: "us"}
	if _, err := a.Search(t.Context(), "daft punk"); err == nil || scrapes.Load() != 0 {
		t.Fatalf("override must be used as is: %v, scrapes %d", err, scrapes.Load())
	}
}
