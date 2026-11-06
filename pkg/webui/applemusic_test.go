package webui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAppleMusic is the web player (token source) and the catalog API.
func fakeAppleMusic(t *testing.T) *httptest.Server {
	const token = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJURUFNIiwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJlLWJ5dGVzLWxvbmctZW5vdWdo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/us/browse":
			io.WriteString(w, `<script type="module" src="/assets/index-Ab12.js"></script>`)
			return
		case "/assets/index-Ab12.js":
			fmt.Fprintf(w, `const k="%s";`, token)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		song := `{"id":"1440","type":"songs","attributes":{"name":"One More Time","artistName":"Daft Punk","albumName":"Discovery","isrc":"GBDUW0000059","durationInMillis":320000,"artwork":{"url":"https://img/{w}x{h}bb.jpg"}}}`
		switch r.URL.Path {
		case "/v1/catalog/cn/playlists/pl.club":
			io.WriteString(w, `{"data":[{"id":"pl.club","type":"playlists","attributes":{"name":"Club Set"},"relationships":{"tracks":{"data":[`+song+`]}}}]}`)
		case "/v1/catalog/cn/playlists/pl.club/tracks":
			io.WriteString(w, `{"data":[`+song+`]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The deck's QR code opens the phone page, a pasted share link logs in with no
// account, and the playlist and its tracks appear in the deck's browser.
func TestAppleMusicPhoneLinkLoginAndBrowse(t *testing.T) {
	am := fakeAppleMusic(t)
	state := t.TempDir()
	// A configured relay must not take the Apple Music login: it has no such page.
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, AppleMusicURL: am.URL, AppleMusicWebURL: am.URL, RelayURL: "http://relay.invalid"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=applemusic", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	target, _ := url.Parse(ui.logins["applemusic"].Target)
	if target.Path != "/m/applemusic" || ui.logins["applemusic"].viaRelay {
		t.Fatalf("QR code %+v", ui.logins["applemusic"])
	}
	page := srv.URL + target.Path + "?" + target.RawQuery
	if p := getPage(t, page); !strings.Contains(p, "Apple Music") || !strings.Contains(p, `name="links"`) || !strings.Contains(p, "musickit.js") { // the web player's token is enough for the button
		t.Fatalf("phone page: %s", p)
	}
	// A link that is not Apple Music is refused and nothing is saved.
	if body := postForm(t, page, url.Values{"links": {"https://example.com/x"}}); !strings.Contains(body, "did not accept") {
		t.Fatalf("bad link: %s", body)
	}
	if _, err := os.Stat(filepath.Join(state, "applemusic-owner.json")); !os.IsNotExist(err) {
		t.Fatal("saved a refused login")
	}
	if body := postForm(t, page, url.Values{"links": {"Copy Link: https://music.apple.com/cn/playlist/%E6%B4%BE%E5%AF%B9/pl.club?l=en"}}); !strings.Contains(body, "Connected") {
		t.Fatalf("phone page: %s", body)
	}
	if !strings.Contains(string(tidalGet(t, srv.URL, "/v1/sc5000/login?provider=applemusic", 200, nil)), `"status":"done"`) {
		t.Fatal("deck login not done")
	}
	info, err := os.Stat(filepath.Join(state, "applemusic-owner.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
	var saved struct {
		Links      []string
		Storefront string
	}
	b, _ := os.ReadFile(filepath.Join(state, "applemusic-owner.json"))
	json.Unmarshal(b, &saved)
	if len(saved.Links) != 1 || saved.Links[0] != "https://music.apple.com/cn/playlist/pl.club" || saved.Storefront != "cn" {
		t.Fatalf("saved %s", b)
	}

	if resp, err = http.Post(srv.URL+"/v1/sc5000/ui?scope=applemusic", "", nil); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var lists tidalList
	tidalGet(t, srv.URL, "/v1/users/1/playlists", 200, &lists)
	var club string
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		if p.Title == "Club Set" {
			club = p.UUID
		}
	}
	if club == "" {
		t.Fatalf("playlists %s", lists.Items)
	}
	var items struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	tidalGet(t, srv.URL, "/v1/playlists/"+club+"/items", 200, &items)
	if len(items.Items) != 1 || items.Items[0].Title != "One More Time" {
		t.Fatalf("items %+v", items)
	}
}

// With the owner's own developer token the page offers MusicKit's sign-in, and
// the token is written as a JSON string that cannot end the script.
func TestAppleMusicPageOffersMusicKitWithOwnToken(t *testing.T) {
	p := appleMusicPhonePage(0, "/m/applemusic?t=x", `ab</script><b>`)
	if !strings.Contains(p, "musickit.js") || strings.Contains(p, "</script><b>") || !strings.Contains(p, `</script>`) {
		t.Fatalf("page: %s", p)
	}
}

// The sign-in button falls back to the bookmarklet when authorize() throws, and
// stays hidden when no developer token could be found.
func TestAppleMusicPageFallsBackToBookmarklet(t *testing.T) {
	p := appleMusicPhonePage(1, "/m/applemusic?t=x", "tok")
	if !strings.Contains(p, `id="amb" hidden`) || !strings.Contains(p, "javascript:void(") || !strings.Contains(p, `getElementById("amb").hidden=false`) || !strings.Contains(p, `build:"5.3.0.2"`) {
		t.Fatalf("page: %s", p)
	}
	if strings.Contains(appleMusicPhonePage(0, "/m/applemusic?t=x", ""), "musickit.js") {
		t.Fatal("button without a developer token")
	}
}

func postForm(t *testing.T, page string, v url.Values) string {
	t.Helper()
	resp, err := http.PostForm(page, v)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
