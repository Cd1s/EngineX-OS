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
	sc := &SoundCloud{BaseURL: api.URL, Token: "t"}
	likes, err := sc.Tracks(context.Background(), soundCloudLikesID)
	if err != nil || len(likes) != 2 || likes[0].ID != "11" || likes[0].Title != "Wrapped" || likes[0].Artist != "dj" || likes[1].ID != "12" {
		t.Fatalf("%+v %v", likes, err)
	}
	list, err := sc.Tracks(context.Background(), "5")
	if err != nil || len(list) != 1 || list[0].ID != "21" {
		t.Fatalf("%+v %v", list, err)
	}
}
