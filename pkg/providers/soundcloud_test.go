package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An expired account login is renewed with the app's credentials in the form,
// which is the only way SoundCloud accepts a refresh_token grant.
func TestSoundCloudRenewsExpiredLoginWithFormCredentials(t *testing.T) {
	refreshes := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if _, _, ok := r.BasicAuth(); ok {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_request"}`)
				return
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r1" || r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "shh" {
				t.Errorf("token form %v", r.Form)
				w.WriteHeader(400)
				return
			}
			refreshes++
			io.WriteString(w, `{"access_token":"u2","refresh_token":"r2","expires_in":3600}`)
		case "/me/playlists":
			if r.Header.Get("Authorization") != "OAuth u2" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"collection":[{"id":5,"title":"Mine","track_count":2}]}`)
		case "/me/likes/playlists":
			if r.Header.Get("Authorization") != "OAuth u2" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"collection":[{"created_at":"2026/01/01","playlist":{"id":6,"title":"Saved","track_count":9}},{"id":7,"title":"Flat","track_count":1}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	path := filepath.Join(t.TempDir(), "soundcloud-token.json")
	SaveJSON(path, soundCloudToken{ClientID: "cid", UserAccess: "u1", UserRefresh: "r1", UserExpiry: time.Now().Add(-time.Hour)})
	auth := &SoundCloudAuth{TokenURL: api.URL + "/token", Path: path}
	// A stale pasted token must not shadow the account login, which renews itself.
	sc := &SoundCloud{BaseURL: api.URL, ClientID: "cid", Secret: "shh", Token: "stale", Auth: auth}
	lists, err := sc.Playlists(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, p := range lists {
		got = append(got, p.ID+":"+p.Title)
	}
	want := "likes:Liked Tracks & Sets（我点赞的音乐）,5:Mine,6:【收藏】Saved,7:【收藏】Flat"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	if refreshes != 1 {
		t.Fatalf("refreshed %d times", refreshes)
	}
	var saved soundCloudToken
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &saved)
	if saved.UserAccess != "u2" || saved.UserRefresh != "r2" || !saved.UserExpiry.After(time.Now()) {
		t.Fatalf("saved %+v", saved)
	}
}

// The app's own token still goes through a Basic header.
func TestSoundCloudAppTokenUsesBasicAuth(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if id, secret, ok := r.BasicAuth(); !ok || id != "cid" || secret != "shh" || r.Form.Get("client_secret") != "" {
			t.Errorf("app token request %v", r.Form)
		}
		io.WriteString(w, `{"access_token":"app","expires_in":3600}`)
	}))
	defer api.Close()
	auth := &SoundCloudAuth{TokenURL: api.URL, Path: filepath.Join(t.TempDir(), "t.json")}
	if tok, err := auth.Token(context.Background(), "cid", "shh"); err != nil || tok != "app" {
		t.Fatal(tok, err)
	}
}

// Liked tracks arrive wrapped as {"track": {...}}; playlist tracks arrive flat.
func TestSoundCloudLikedTracksUnwrap(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me/likes/tracks":
			io.WriteString(w, `{"collection":[
				{"created_at":"2026/01/01","track":{"id":11,"title":"Wrapped","duration":1000,"access":"playable","user":{"username":"dj"}}},
				{"id":12,"title":"Flat","access":"playable","user":{"username":"dj"}},
				{"created_at":"2026/01/01","track":{"id":13,"title":"Blocked","access":"blocked"}}
			]}`)
		case "/playlists/5/tracks":
			io.WriteString(w, `{"collection":[{"id":21,"title":"In list","user":{"username":"x"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	sc := &SoundCloud{BaseURL: api.URL, ClientID: "cid", Secret: "shh", Token: "t"}
	likes, err := sc.Tracks(context.Background(), soundCloudLikesID)
	if err != nil || len(likes) != 2 || likes[0].ID != "11" || likes[0].Title != "Wrapped" || likes[0].Artist != "dj" || likes[1].ID != "12" {
		t.Fatalf("%+v %v", likes, err)
	}
	list, err := sc.Tracks(context.Background(), "5")
	if err != nil || len(list) != 1 || list[0].ID != "21" {
		t.Fatalf("%+v %v", list, err)
	}
}

// Without an app of its own the deck reads a public profile through the web API,
// with the client ID from soundcloud.com's scripts, looked up again once refused.
func TestSoundCloudWithoutCredentialsUsesTheWebAPI(t *testing.T) {
	const id = "0123456789abcdefABCDEF0123456789"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			io.WriteString(w, `<script crossorigin src="`+srv.URL+`/assets/a.js"></script><script crossorigin src="`+srv.URL+`/assets/b.js"></script>`)
			return
		case "/assets/a.js":
			io.WriteString(w, `var x=1`)
			return
		case "/assets/b.js":
			io.WriteString(w, `{client_id:"`+id+`",env:"production"}`)
			return
		case "/cdn/aac.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:10,\nseg1.m4s\n")
			return
		}
		if r.URL.Query().Get("client_id") != id || r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/resolve":
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		case "/users/7/track_likes":
			io.WriteString(w, `{"collection":[{"track":{"id":11,"title":"Full","policy":"ALLOW","user":{"username":"dj"}}},{"track":{"id":12,"title":"Go+","policy":"SNIP"}}]}`)
		case "/users/7/playlists":
			io.WriteString(w, `{"collection":[{"id":5,"title":"Mine","track_count":2}]}`)
		case "/users/7/playlist_likes":
			io.WriteString(w, `{"collection":[{"playlist":{"id":6,"title":"Saved","track_count":9}}]}`)
		case "/playlists/5":
			io.WriteString(w, `{"tracks":[{"id":21,"title":"First","policy":"ALLOW"},{"id":22,"policy":"ALLOW"}]}`)
		case "/tracks":
			if r.URL.Query().Get("ids") != "22" {
				t.Errorf("ids %q", r.URL.Query().Get("ids"))
			}
			io.WriteString(w, `[{"id":22,"title":"Second","policy":"ALLOW"}]`)
		case "/tracks/11":
			io.WriteString(w, `{"policy":"ALLOW","track_authorization":"ta","media":{"transcodings":[
				{"url":"`+srv.URL+`/media/preview","snipped":true,"format":{"protocol":"progressive","mime_type":"audio/mpeg"}},
				{"url":"`+srv.URL+`/media/aac","format":{"protocol":"hls","mime_type":"audio/mp4; codecs=\"mp4a.40.2\""}}]}}`)
		case "/media/aac":
			if r.URL.Query().Get("track_authorization") != "ta" {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{"url":"`+srv.URL+`/cdn/aac.m3u8"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Cleanup(func() { delete(scWebIDs, srv.URL) })
	scWebIDs[srv.URL] = "stale0000000000000000000000000000"
	ctx := context.Background()
	sc := &SoundCloud{WebURL: srv.URL, SiteURL: srv.URL}
	if err := sc.ResolveUser(ctx, "cave"); err != nil || sc.UserID != 7 {
		t.Fatalf("resolve %v %d", err, sc.UserID)
	}
	lists, _ := sc.Playlists(ctx)
	if len(lists) != 3 || lists[1].Title != "Mine" || lists[2].Title != "【收藏】Saved" {
		t.Fatalf("playlists %+v", lists)
	}
	likes, err := sc.Tracks(ctx, soundCloudLikesID)
	if err != nil || len(likes) != 1 || likes[0].ID != "11" {
		t.Fatalf("likes %+v %v", likes, err)
	}
	list, err := sc.Tracks(ctx, "5")
	if err != nil || len(list) != 2 || list[1].Title != "Second" {
		t.Fatalf("playlist %+v %v", list, err)
	}
	// The preview is skipped; the AAC stream's playlist is read from the CDN.
	m, err := sc.Media(ctx, "11", false)
	if err != nil || m.Ext != "mp4" || strings.Join(m.URLs, " ") != srv.URL+"/cdn/init.mp4 "+srv.URL+"/cdn/seg1.m4s" {
		t.Fatalf("media %+v %v", m, err)
	}
}

// The best stream wins over the 128 kbps MP3 that is listed first: hq (256 kbps) AAC,
// then 160 kbps AAC; lossless also takes the original where downloads are allowed.
func TestSoundCloudWebMediaPrefersHighestQuality(t *testing.T) {
	const id = "0123456789abcdefABCDEF0123456789"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXTINF:10,\n"+strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/cdn/"), ".m3u8")+".m4s\n")
			return
		}
		switch r.URL.Path {
		case "/tracks/31", "/tracks/32":
			dl := r.URL.Path == "/tracks/31"
			io.WriteString(w, `{"policy":"ALLOW","downloadable":`+map[bool]string{true: "true", false: "false"}[dl]+`,"has_downloads_left":true,"media":{"transcodings":[
				{"url":"`+srv.URL+`/media/mp3","format":{"protocol":"progressive","mime_type":"audio/mpeg"}},
				{"url":"`+srv.URL+`/media/sq","quality":"sq","preset":"aac_160k","format":{"protocol":"hls","mime_type":"audio/mp4"}},
				{"url":"`+srv.URL+`/media/hq","quality":"hq","preset":"aac_256k","format":{"protocol":"hls","mime_type":"audio/mp4"}}]}}`)
		case "/tracks/31/download":
			io.WriteString(w, `{"redirectUri":"`+srv.URL+`/orig.wav"}`)
		case "/media/mp3":
			io.WriteString(w, `{"url":"`+srv.URL+`/mp3"}`)
		case "/media/sq":
			io.WriteString(w, `{"url":"`+srv.URL+`/cdn/sq.m3u8"}`)
		case "/media/hq":
			io.WriteString(w, `{"url":"`+srv.URL+`/cdn/hq.m3u8"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Cleanup(func() { delete(scWebIDs, srv.URL) })
	scWebIDs[srv.URL] = id
	sc := &SoundCloud{WebURL: srv.URL, SiteURL: srv.URL}
	ctx := context.Background()
	m, err := sc.Media(ctx, "31", true)
	if err != nil || m.Ext != "mp4" || strings.Join(m.URLs, " ") != srv.URL+"/cdn/hq.m4s" || m.Original != srv.URL+"/orig.wav" {
		t.Fatalf("lossless %+v %v", m, err)
	}
	// Without lossless, or without downloads allowed, there is no original.
	if m, err = sc.Media(ctx, "31", false); err != nil || m.Original != "" || m.Ext != "mp4" {
		t.Fatalf("lossy %+v %v", m, err)
	}
	if m, err = sc.Media(ctx, "32", true); err != nil || m.Original != "" || m.Ext != "mp4" {
		t.Fatalf("no downloads %+v %v", m, err)
	}
}

// A login whose single-use refresh token is refused for good is dropped: the app
// token (or a public profile) serves the lists instead of failing with HTTP 400.
func TestSoundCloudDeadLoginFallsBackToAppAndPublicLikes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				w.WriteHeader(400)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			io.WriteString(w, `{"access_token":"app1","expires_in":3600}`)
		case "/users/9/playlists", "/users/9/likes/playlists":
			io.WriteString(w, `{"collection":[]}`)
		default: // /me/... and /users/9/likes/tracks refuse; the public web API answers
			w.WriteHeader(403)
		}
	}))
	defer api.Close()
	var web *httptest.Server
	web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/": // the site page names its script, which carries the client id
			io.WriteString(w, `<script src="`+web.URL+`/app.js"></script>`)
		case "/app.js":
			io.WriteString(w, `client_id:"0123456789abcdef0123456789abcdef"`)
		case "/users/9/track_likes":
			if r.URL.Query().Get("client_id") != "0123456789abcdef0123456789abcdef" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"collection":[{"track":{"id":31,"title":"Public like","access":"playable","user":{"username":"dj"}}}]}`)
		}
	}))
	defer web.Close()
	path := filepath.Join(t.TempDir(), "soundcloud-token.json")
	SaveJSON(path, soundCloudToken{ClientID: "cid", UserAccess: "u1", UserRefresh: "r1", UserExpiry: time.Now().Add(-time.Hour)})
	auth := &SoundCloudAuth{TokenURL: api.URL + "/token", Path: path}
	sc := &SoundCloud{BaseURL: api.URL, WebURL: web.URL, SiteURL: web.URL, ClientID: "cid", Secret: "shh", Auth: auth, UserID: 9}
	likes, err := sc.Tracks(context.Background(), soundCloudLikesID)
	if err != nil || len(likes) != 1 || likes[0].ID != "31" {
		t.Fatalf("likes %+v %v", likes, err)
	}
	if auth.HasUser() {
		t.Fatal("the dead login was kept")
	}
	var saved soundCloudToken
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &saved)
	if saved.UserAccess != "" || saved.UserRefresh != "" {
		t.Fatalf("saved %+v", saved)
	}
}
