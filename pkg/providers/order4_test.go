package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAppleMusicScraperHandlesRotatedBundleFilenames(t *testing.T) {
	token := fakeJWT(time.Now().Add(30 * 24 * time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/us/browse":
			// Apple rotated away from /assets/index*.js to chunked entry/vendor scripts.
			io.WriteString(w, `<!doctype html><html><head>`+
				`<script src="https://evil.example/steal.js"></script>`+
				`<script type="module" crossorigin src="/assets/runtime-77a91c.js"></script>`+
				`<script type="module" crossorigin src="/assets/musickit-web-9b8f12.js"></script>`+
				`</head></html>`)
		case "/assets/runtime-77a91c.js":
			io.WriteString(w, `export const chunk="runtime";`)
		case "/assets/musickit-web-9b8f12.js":
			fmt.Fprintf(w, `const cfg={developerToken:%q};export default cfg;`, token)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	am := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL}
	got, err := am.DeveloperToken(context.Background())
	if err != nil || got != token {
		t.Fatalf("DeveloperToken on rotated bundle filename = %q, err = %v; want %q", got, err, token)
	}
}

func TestExpiredOrRevokedLoginsSurfaceStatusError401(t *testing.T) {
	ctx := context.Background()

	// 1. NetEase code 301 ("login required / expired") must unwrap to StatusError(401).
	neSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 301, "msg": "need login"})
	}))
	defer neSrv.Close()
	ne := &Netease{BaseURL: neSrv.URL, Cookie: "MUSIC_U=expired", UserID: 42}
	_, err := ne.Playlists(ctx)
	var st StatusError
	if !errors.As(err, &st) || st != 401 {
		t.Fatalf("NetEase code 301 error = %v, want StatusError(401)", err)
	}

	// 2. Beatport revoked refresh_token (OAuth 400 invalid_grant) must unwrap to StatusError(401).
	bpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/o/token/" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bpSrv.Close()
	bp := &Beatport{
		BaseURL: bpSrv.URL,
		Path:    filepath.Join(t.TempDir(), "bp.json"),
	}
	if err := bp.SetToken(BeatportToken{Access: "expired-a", Refresh: "revoked-r", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, err = bp.Playlists(ctx)
	st = 0
	if !errors.As(err, &st) || st != 401 {
		t.Fatalf("Beatport revoked refresh error = %v, want StatusError(401)", err)
	}

	// 3. Spotify expired token (with revoked refresh_token returning 400, or without refresh_token) must unwrap to StatusError(401).
	spSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer spSrv.Close()
	sp := &Spotify{
		BaseURL:     spSrv.URL,
		AccountsURL: spSrv.URL,
		ClientID:    "0123456789abcdef0123456789abcdef",
		TokenPath:   filepath.Join(t.TempDir(), "sp.json"),
		CredsPath:   filepath.Join(t.TempDir(), "missing-creds.json"),
		Token:       Token{AccessToken: "exp", RefreshToken: "revoked", Expiry: time.Now().Add(-time.Hour)},
	}
	_, err = sp.Playlists(ctx)
	st = 0
	if !errors.As(err, &st) || st != 401 {
		t.Fatalf("Spotify revoked refresh error = %v, want StatusError(401)", err)
	}

	// 4. SoundCloud expired user login (without public UserID fallback) must unwrap to StatusError(401).
	scSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer scSrv.Close()
	scPath := filepath.Join(t.TempDir(), "sc.json")
	if err := SaveJSON(scPath, soundCloudToken{ClientID: "cid", UserAccess: "exp-u", UserRefresh: "revoked-r", UserExpiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	sc := &SoundCloud{
		BaseURL:  scSrv.URL,
		ClientID: "cid",
		Secret:   "sec",
		Auth:     &SoundCloudAuth{TokenURL: scSrv.URL + "/token", Path: scPath},
	}
	_, err = sc.Playlists(ctx)
	st = 0
	if !errors.As(err, &st) || st != 401 {
		t.Fatalf("SoundCloud revoked refresh error = %v, want StatusError(401)", err)
	}
}

func TestProvidersSurfacePage2AndFallbackMaskedAuthErrors(t *testing.T) {
	ctx := context.Background()

	// 1. Spotify.Playlists must not mask a 401/403 API error just because LinksPath has a public link.
	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{"name":"Public Mix","trackList":[{"uri":"spotify:track:abc","title":"T","subtitle":"A","duration":1000}]}}}}}}</script></html>`)
	}))
	defer embedSrv.Close()
	spAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer spAPI.Close()
	sp := &Spotify{
		BaseURL:   spAPI.URL,
		EmbedURL:  embedSrv.URL,
		LinksPath: filepath.Join(t.TempDir(), "links.json"),
		CredsPath: filepath.Join(t.TempDir(), "no-creds.json"),
		Token:     Token{AccessToken: "forbidden-tok", Expiry: time.Now().Add(time.Hour)},
	}
	if _, err := sp.SetLinks(ctx, "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M"); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Playlists(ctx); !errors.Is(err, StatusError(403)) {
		t.Fatalf("Spotify.Playlists masked 403 behind linkedPlaylists: err=%v", err)
	}

	// 2. Beatport.Tracks must not drop page 2+ errors, and Beatport.Playlists must not mask /my/playlists 403 behind public charts.
	bpAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/my/playlists/":
			w.WriteHeader(http.StatusForbidden)
		case "/catalog/charts/":
			io.WriteString(w, `{"next":null,"results":[{"id":7,"name":"Top 10","track_count":10}]}`)
		case "/my/playlists/11/tracks/":
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `{"next":"p2","count":150,"results":[{"id":1,"track":{"id":501,"name":"One"}}]}`)
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer bpAPI.Close()
	bp := &Beatport{BaseURL: bpAPI.URL, loaded: true, tok: BeatportToken{Access: "tok", Expiry: time.Now().Add(time.Hour)}}
	if _, err := bp.Playlists(ctx); !errors.Is(err, StatusError(403)) {
		t.Fatalf("Beatport.Playlists masked /my/playlists 403 behind charts: err=%v", err)
	}
	if _, err := bp.Tracks(ctx, "11"); !errors.Is(err, StatusError(429)) {
		t.Fatalf("Beatport.Tracks dropped page 2 HTTP 429: err=%v", err)
	}

	// 3. SoundCloud.Tracks must not drop page 2+ errors when page 1 returned tracks.
	var scSrv *httptest.Server
	scSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"collection":[{"id":1,"title":"Page1Track","access":"playable"}],"next_href":"`+scSrv.URL+`/playlists/55/tracks?page=2"}`)
	}))
	defer scSrv.Close()
	sc := &SoundCloud{BaseURL: scSrv.URL, ClientID: "cid", Secret: "sec", Token: "tok"}
	if _, err := sc.Tracks(ctx, "55"); !errors.Is(err, StatusError(502)) {
		t.Fatalf("SoundCloud.Tracks dropped page 2 HTTP 502: err=%v", err)
	}
}
