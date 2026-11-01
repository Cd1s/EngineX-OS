package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNeteaseQRAndCompletePlaylist(t *testing.T) {
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("cookie metadata must use POST")
		}
		r.ParseForm()
		if r.URL.RawQuery != "" {
			t.Error("credentials leaked into URL")
		}
		var response any
		switch r.URL.Path {
		case "/login/qr/key":
			response = map[string]any{"code": 200, "data": map[string]string{"unikey": "qr-key"}}
		case "/login/qr/create":
			if r.Form.Get("key") != "qr-key" {
				t.Error("key missing")
			}
			response = map[string]any{"code": 200, "data": map[string]string{"qrurl": "https://music.163.com/login?key=qr-key"}}
		case "/login/qr/check":
			polls++
			response = map[string]any{"code": 803, "cookie": "private-cookie"}
		case "/login/status":
			if r.Form.Get("cookie") != "private-cookie" {
				t.Error("cookie missing")
			}
			response = map[string]any{"data": map[string]any{"code": 200, "profile": map[string]int{"userId": 7}}}
		case "/user/playlist":
			if r.Form.Get("uid") != "7" {
				t.Error("wrong account")
			}
			response = map[string]any{"code": 200, "playlist": []any{map[string]any{"id": 11, "name": "Guest songs", "trackCount": 1}}, "more": false}
		case "/playlist/detail":
			response = map[string]any{"code": 200, "playlist": map[string]any{"tracks": []any{}, "trackIds": []any{map[string]int{"id": 99}}}}
		case "/song/detail":
			if r.Form.Get("ids") != "99" {
				t.Error("incomplete tracks not expanded")
			}
			response = map[string]any{"code": 200, "songs": []any{map[string]any{"id": 99, "name": "Title", "dt": 200000, "ar": []any{map[string]string{"name": "Artist"}}, "al": map[string]string{"name": "Album"}}}}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "owner.json")
	owner := &Netease{BaseURL: srv.URL, CookiePath: path}
	qr, err := owner.BeginQR(ctx)
	if err != nil || qr.Key != "qr-key" || !strings.Contains(qr.URL, "music.163.com") {
		t.Fatalf("QR %+v %v", qr, err)
	}
	if code, err := owner.PollQR(ctx, qr.Key); err != nil || code != 803 {
		t.Fatalf("login %d %v", code, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("credential mode %v %v", st, err)
	}
	playlists, err := owner.Playlists(ctx)
	if err != nil || len(playlists) != 1 || playlists[0].ID != "11" {
		t.Fatalf("playlists %v %v", playlists, err)
	}
	tracks, err := owner.Tracks(ctx, "11")
	if err != nil || len(tracks) != 1 || tracks[0].DurationMS != 200000 {
		t.Fatalf("tracks %v %v", tracks, err)
	}
	guest := &Netease{BaseURL: srv.URL}
	if guest.Cookie != "" || guest.UserID != 0 {
		t.Fatal("guest shares credentials")
	}
	if _, err = guest.PollQR(ctx, qr.Key); err != nil {
		t.Fatal(err)
	}
	if guest.CookiePath != "" || polls != 2 {
		t.Fatal("guest persisted")
	}
}
func TestSpotifyTokenRefreshAndPKCE(t *testing.T) {
	var refresh, exchange, playlistPages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token":
			r.ParseForm()
			if r.Form.Get("client_id") != "public-client" || r.Form.Get("client_secret") != "" {
				t.Error("wrong PKCE client")
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				refresh++
				if r.Form.Get("refresh_token") != "refresh-private" {
					t.Error("refresh token")
				}
			} else {
				exchange++
				if len(r.Form.Get("code_verifier")) < 43 || r.Form.Get("redirect_uri") != "https://device/callback" {
					t.Error("PKCE verifier/redirect")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-private", "expires_in": 3600, "token_type": "Bearer"})
		case "/me/playlists":
			if r.Header.Get("Authorization") != "Bearer fresh-private" {
				t.Error("expired bearer token")
			}
			playlistPages++
			if r.URL.Query().Get("offset") == "0" {
				ioJSON(w, `{"items":[{"id":"p1","name":"First","tracks":{"total":1}}],"next":"https://untrusted.invalid/token-steal"}`)
			} else {
				ioJSON(w, `{"items":[{"id":"p2","name":"Second"}],"next":null}`)
			}
		case "/playlists/p1/tracks":
			ioJSON(w, `{"items":[{"track":null},{"is_local":true,"track":{"id":"local"}},{"track":{"id":"s1","name":"Song","type":"track","duration_ms":180001,"artists":[{"name":"Artist"}],"album":{"name":"Album"},"external_ids":{"isrc":"US1234567890"}}}],"next":null}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "token.json")
	SaveJSON(path, Token{AccessToken: "expired-private", RefreshToken: "refresh-private", Expiry: time.Now().Add(-time.Hour), ClientID: "public-client"})
	sp := &Spotify{TokenPath: path, BaseURL: srv.URL, AccountsURL: srv.URL}
	if err := sp.LoadToken(); err != nil {
		t.Fatal(err)
	}
	lists, err := sp.Playlists(context.Background())
	if err != nil || len(lists) != 2 || refresh != 1 || playlistPages != 2 {
		t.Fatalf("pagination/refresh %v %v", lists, err)
	}
	tracks, err := sp.Tracks(context.Background(), "p1")
	if err != nil || len(tracks) != 1 || tracks[0].ISRC != "US1234567890" || tracks[0].DurationMS != 180001 {
		t.Fatalf("metadata %v %v", tracks, err)
	}
	auth, verifier := sp.BeginPKCE("state-bound", "https://device/callback")
	if !strings.Contains(auth, "code_challenge_method=S256") || !strings.Contains(auth, "state=state-bound") {
		t.Fatal(auth)
	}
	if err = sp.CompletePKCE(context.Background(), "code", verifier, "https://device/callback"); err != nil {
		t.Fatal(err)
	}
	if exchange != 1 || sp.Token.RefreshToken != "refresh-private" {
		t.Fatal("refresh credential lost")
	}
}
func ioJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(s))
}
func TestDeezerPublicPlaylistAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/7/playlists":
			ioJSON(w, `{"data":[{"id":8,"title":"Public","nb_tracks":1}]}`)
		case "/playlist/8/tracks", "/search":
			ioJSON(w, `{"data":[{"id":9,"title":"Song","duration":200,"artist":{"name":"Artist"}}]}`)
		case "/track/isrc:US123":
			ioJSON(w, `{"id":9,"isrc":"US123","duration":200,"title":"Song"}`)
		default:
			ioJSON(w, `{"error":{"message":"private token must not appear"}}`)
		}
	}))
	defer srv.Close()
	d := &Deezer{BaseURL: srv.URL, UserID: "7"}
	if p, e := d.Playlists(context.Background()); e != nil || len(p) != 1 || p[0].ID != "8" {
		t.Fatalf("playlists %v %v", p, e)
	}
	if tracks, e := d.Tracks(context.Background(), "8"); e != nil || len(tracks) != 1 || tracks[0].DurationMS != 200000 {
		t.Fatalf("tracks %v %v", tracks, e)
	}
	if tr, e := d.ByISRC(context.Background(), "US123"); e != nil || tr.ID != "9" {
		t.Fatalf("isrc %v %v", tr, e)
	}
	if _, e := d.ByISRC(context.Background(), "bad"); e == nil || strings.Contains(e.Error(), "private token") {
		t.Fatal("API error leaked body")
	}
}
