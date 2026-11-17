package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestAcquireAppleMusicHelper(t *testing.T) {
	var gotAuth, gotURL string
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(4)); err != nil {
		t.Fatal(err)
	}
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/download":
			gotAuth = r.Header.Get("Authorization")
			var in struct{ URL string }
			json.NewDecoder(r.Body).Decode(&in)
			gotURL = in.URL
			json.NewEncoder(w).Encode(map[string]any{
				"file":    "/v1/files/x.flac",
				"quality": map[string]any{"sampleRate": 44100, "channels": 2, "bits": 16, "lossless": true},
			})
		case "/v1/files/x.flac":
			w.Write(flac.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer helper.Close()

	am := &providers.AppleMusic{HelperURL: helper.URL, HelperToken: "tok"}
	j := NewJobs(Pipeline{Catalog: noCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}})
	defer j.Close()
	j.applemusic = func() *providers.AppleMusic { return am }

	_, dl, err := j.acquire(context.Background(), "", providers.Track{ID: "1559523359", Provider: "applemusic"}, false, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if dl == nil || dl.Path == "" {
		t.Fatal("no download")
	}
	defer os.Remove(dl.Path)
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotURL != "https://music.apple.com/us/song/1559523359" {
		t.Fatalf("helper URL = %q", gotURL)
	}
	if dl.Quality.SampleRate != testaudio.Rate || !dl.Quality.Lossless {
		t.Fatalf("quality = %+v", dl.Quality)
	}
}

func TestAppleMusicHelperAPIPersistenceAndAccess(t *testing.T) {
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		w.Write([]byte(`{"state":"ready"}`))
	}))
	defer helper.Close()
	state := t.TempDir()
	ui := New(Config{StateDir: state, UIDir: t.TempDir(), AccessToken: "console-fixture"}, nil)
	defer ui.Close()
	request := func(method, path, body, auth string, local bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.1:1234"
		if local {
			r.RemoteAddr = "127.0.0.1:1234"
		}
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/applemusic", "/api/applemusic/helper-qr", "/api/applemusic/helper-login"} {
		if got := request("GET", path, "", "", false); got.Code != 401 {
			t.Fatalf("unprotected %s: %d", path, got.Code)
		}
	}
	if got := request("GET", "/v1/sc5000/applemusic", "", "", false); got.Code != 403 {
		t.Fatal("LAN reached loopback API")
	}
	payload, _ := json.Marshal(map[string]string{"helperUrl": helper.URL, "helperToken": "private-helper-fixture"})
	saved := request("POST", "/api/applemusic", string(payload), "console-fixture", false)
	if saved.Code != 200 || strings.Contains(saved.Body.String(), "private-helper-fixture") {
		t.Fatal("save failed or token exposed")
	}
	info, err := os.Stat(filepath.Join(state, "applemusic-helper.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential permissions")
	}
	restarted := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
	defer restarted.Close()
	if cfg := restarted.appleMusic.HelperConfig(); cfg.URL != helper.URL || cfg.Token != "private-helper-fixture" {
		t.Fatal("helper settings not restored")
	}
	qr := request("GET", "/api/applemusic/helper-qr", "", "console-fixture", false)
	if qr.Code != 200 || qr.Header().Get("Content-Type") != "image/png" {
		t.Fatal("console QR")
	}
	if got := request("POST", "/v1/sc5000/applemusic/login", `{"username":"fixture","password":"fixture"}`, "", true); got.Code != 200 {
		t.Fatal("loopback login route")
	}
	if got := request("POST", "/api/applemusic/2fa", `{"code":"123456"}`, "console-fixture", false); got.Code != 200 {
		t.Fatal("console 2FA route")
	}
	for _, bad := range []string{"file:///tmp/x", "http://user:pass@example.invalid", "http://example.invalid/?token=x", "javascript:alert(1)"} {
		b, _ := json.Marshal(map[string]string{"helperUrl": bad})
		if got := request("POST", "/api/applemusic", string(b), "console-fixture", false); got.Code != 400 {
			t.Fatal("invalid URL accepted")
		}
	}
	if got := request("GET", "/m/applemusic/helper-qr?t=invalid", "", "", false); got.Code != 403 {
		t.Fatal("phone QR accepted missing flow")
	}
	start := request("POST", "/v1/sc5000/login?provider=applemusic", "", "", true)
	if start.Code != 200 {
		t.Fatal("start phone login")
	}
	flow := ui.loginFlow("applemusic")
	action := "/m/applemusic?t=" + url.QueryEscape(flow.token)
	if got := request("GET", "/m/applemusic/helper-qr?t="+url.QueryEscape(flow.token), "", "", false); got.Code != 200 {
		t.Fatal("phone QR rejected valid flow")
	}
	before := ui.appleMusic.HelperConfig()
	r := httptest.NewRequest("POST", action, strings.NewReader("helperAction=login&username=fixture&password=fixture&helperUrl=http://attacker.invalid&helperToken=changed"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("phone finish: %d", w.Code)
	}
	if ui.appleMusic.HelperConfig() != before {
		t.Fatal("phone token changed helper configuration")
	}
	clear, _ := json.Marshal(map[string]any{"helperUrl": helper.URL, "clearToken": true})
	if got := request("POST", "/api/applemusic", string(clear), "console-fixture", false); got.Code != 200 || ui.appleMusic.HelperConfig().Token != "" {
		t.Fatal("clear helper token")
	}
	cleared := New(Config{StateDir: state, UIDir: t.TempDir(), AppleMusicHelperToken: "environment-default"}, nil)
	defer cleared.Close()
	if cleared.appleMusic.HelperConfig().Token != "" {
		t.Fatal("cleared token restored from environment")
	}

}

func TestAppleMusicHelperPhoneStates(t *testing.T) {
	for lang := range phoneLangs {
		for _, state := range []struct {
			status     providers.AppleMusicHelperStatus
			key, field string
		}{
			{providers.AppleMusicHelperStatus{}, "amHelperSetup", ""},
			{providers.AppleMusicHelperStatus{Configured: true}, "amHelperOffline", ""},
			{providers.AppleMusicHelperStatus{Configured: true, Online: true, SignedIn: true}, "amHelperReady", ""},
			{providers.AppleMusicHelperStatus{Configured: true, Online: true}, "amHelperSignIn", `name="username"`},
			{providers.AppleMusicHelperStatus{Configured: true, Online: true, LoginState: "2fa"}, "amHelperCode", `name="code"`},
			{providers.AppleMusicHelperStatus{Configured: true, Online: true, LoginState: "logging-in"}, "amHelperBusy", `name="username"`},
		} {
			page := appleMusicHelperPhonePage(lang, "/m/applemusic?t=fixture", state.status, "http://helper.invalid/?token=fixture")
			if !strings.Contains(page, tr(lang, state.key)) || !strings.Contains(page, state.field) {
				t.Fatal("helper phone state")
			}
			if state.status.Configured && strings.Contains(page, `name="helperUrl"`) {
				t.Fatal("configured helper phone page should not expose helperUrl")
			}
			if !state.status.Configured && !strings.Contains(page, `name="helperUrl"`) {
				t.Fatal("unconfigured helper phone page should expose setup form")
			}
			if !strings.Contains(page, "/m/applemusic/helper-qr?t=fixture") {
				t.Fatal("phone QR flow lost")
			}
		}
	}
}

func TestAppleMusicHelperLoginURL(t *testing.T) {
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer ui.Close()
	for _, base := range []string{"http://127.0.0.1:12343", "http://localhost:12343", "http://[::1]:12343"} {
		ui.appleMusic.SetHelper(base, "", "fixture")
		u, err := url.Parse(ui.helperLoginPageURL())
		if err != nil || u.Hostname() != lanHost() || u.Port() != "12342" || u.Query().Get("token") != "fixture" {
			t.Fatal("LAN helper login URL")
		}
	}
	ui.appleMusic.SetHelper("http://helper.invalid", "https://login.example.invalid/", "")
	if ui.helperLoginPageURL() != "https://login.example.invalid/" {
		t.Fatal("explicit login URL ignored")
	}
}

func TestAppleMusicHelperDefaults(t *testing.T) {
	state := t.TempDir()
	if err := providers.SaveJSON(filepath.Join(state, "applemusic-helper.json"), providers.AppleMusicHelperConfig{}); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{StateDir: state, UIDir: t.TempDir(), AppleMusicHelperURL: "http://helper.invalid", AppleMusicHelperToken: "environment-default"}, nil)
	defer ui.Close()
	if cfg := ui.appleMusic.HelperConfig(); cfg.URL != "http://helper.invalid" || cfg.Token != "environment-default" {
		t.Fatal("missing helper fields did not use environment defaults")
	}
}

func TestAppleMusicHelperPhoneLoginCompletesAndPolls(t *testing.T) {
	var mu sync.Mutex
	loginState := "sign-in"
	helper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(200)
		case "/v1/login/status":
			json.NewEncoder(w).Encode(map[string]any{"state": loginState, "signedIn": loginState == "ready"})
		case "/v1/login":
			loginState = "2fa"
			json.NewEncoder(w).Encode(map[string]any{"state": loginState, "signedIn": false})
		case "/v1/2fa":
			loginState = "logging-in"
			json.NewEncoder(w).Encode(map[string]any{"state": loginState, "signedIn": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer helper.Close()

	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), AppleMusicHelperURL: helper.URL}, nil)
	defer ui.Close()

	startReq := httptest.NewRequest("POST", "/v1/sc5000/login?provider=applemusic", nil)
	startReq.RemoteAddr = "127.0.0.1:41001"
	startRec := httptest.NewRecorder()
	ui.ServeHTTP(startRec, startReq)
	if startRec.Code != 200 {
		t.Fatalf("start login: %d", startRec.Code)
	}
	flow := ui.loginFlow("applemusic")
	action := "/m/applemusic?t=" + url.QueryEscape(flow.token)

	postForm := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", action, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		ui.ServeHTTP(rec, r)
		return rec
	}

	// Step 1: Apple ID + password moves helper to 2FA and redirects back to phone page.
	if rec := postForm("helperAction=login&username=user@example.com&password=secret"); rec.Code != 303 {
		t.Fatalf("login step: %d", rec.Code)
	}

	// Step 2: 2FA returns logging-in (>2.5s async), so phone page shows auto-refresh.
	if rec := postForm("helperAction=2fa&code=123456"); rec.Code != 303 {
		t.Fatalf("2fa step: %d", rec.Code)
	}
	getRec := httptest.NewRecorder()
	ui.ServeHTTP(getRec, httptest.NewRequest("GET", action, nil))
	if getRec.Code != 200 || !strings.Contains(getRec.Body.String(), "location.reload()") {
		t.Fatalf("expected auto-refresh while logging-in: code=%d", getRec.Code)
	}

	// Step 3: Helper finishes async sign-in; deck poll transitions flow to done.
	mu.Lock()
	loginState = "ready"
	mu.Unlock()
	pollReq := httptest.NewRequest("GET", "/v1/sc5000/login?provider=applemusic", nil)
	pollReq.RemoteAddr = "127.0.0.1:41001"
	pollRec := httptest.NewRecorder()
	ui.ServeHTTP(pollRec, pollReq)
	if pollRec.Code != 200 || !strings.Contains(pollRec.Body.String(), `"status":"done"`) {
		t.Fatalf("expected done after async helper sign-in: code=%d body=%s", pollRec.Code, pollRec.Body.String())
	}
	if !ui.appleMusic.Ready() {
		t.Fatal("expected AppleMusic.Ready() to be true after helper sign-in")
	}
}

func TestAppleMusicHelperPhoneSetupWhenUnconfigured(t *testing.T) {
	relayReceived := make(chan map[string]any, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config" && r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			relayReceived <- body
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer relay.Close()

	state := t.TempDir()
	ui := New(Config{
		AccessToken: "console-fixture",
		UIDir:       t.TempDir(),
		StateDir:    state,
		RelayURL:    relay.URL,
	}, nil)
	defer ui.Close()
	ui.updateRelayConfig(&relay.URL, ptr("relay-pw"))

	startReq := httptest.NewRequest("POST", "/v1/sc5000/login?provider=applemusic", nil)
	startReq.RemoteAddr = "127.0.0.1:41002"
	startRec := httptest.NewRecorder()
	ui.ServeHTTP(startRec, startReq)
	if startRec.Code != 200 {
		t.Fatalf("start phone login: %d", startRec.Code)
	}
	flow := ui.loginFlow("applemusic")
	action := "/m/applemusic?t=" + url.QueryEscape(flow.token)

	// Unconfigured phone page renders the setup form.
	getRec := httptest.NewRecorder()
	ui.ServeHTTP(getRec, httptest.NewRequest("GET", action, nil))
	if getRec.Code != 200 || !strings.Contains(getRec.Body.String(), `name="helperAction" value="setup"`) {
		t.Fatalf("expected setup form on unconfigured phone page: code=%d", getRec.Code)
	}

	// Submitting setup configures helper and pushes to relay.
	postReq := httptest.NewRequest("POST", action, strings.NewReader("helperAction=setup&helperUrl=http://192.168.6.104:12341&helperToken=tok-1"))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	ui.ServeHTTP(postRec, postReq)
	if postRec.Code != 303 {
		t.Fatalf("setup post: %d %s", postRec.Code, postRec.Body.String())
	}
	cfg := ui.appleMusic.HelperConfig()
	if cfg.URL != "http://192.168.6.104:12341" || cfg.Token != "tok-1" {
		t.Fatalf("unexpected helper config after phone setup: %+v", cfg)
	}
	select {
	case pushed := <-relayReceived:
		if pushed["appleMusicHelperUrl"] != "http://192.168.6.104:12341" || pushed["appleMusicHelperToken"] != "tok-1" {
			t.Fatalf("unexpected relay push payload: %+v", pushed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relay push")
	}

	// Once configured, phone setup cannot overwrite existing helperUrl.
	overwriteReq := httptest.NewRequest("POST", action, strings.NewReader("helperAction=setup&helperUrl=http://attacker.invalid:12341&helperToken=evil"))
	overwriteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	overwriteRec := httptest.NewRecorder()
	ui.ServeHTTP(overwriteRec, overwriteReq)
	if overwriteRec.Code != 200 || ui.appleMusic.HelperConfig().URL != "http://192.168.6.104:12341" {
		t.Fatalf("expected overwrite rejection, got code=%d cfg=%+v", overwriteRec.Code, ui.appleMusic.HelperConfig())
	}
}

func TestAppleMusicHelperAutoRestoreFavoritesAlbumsAndSearch(t *testing.T) {
	var mu sync.Mutex
	account := 1
	const devTok = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJURUFNIiwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJlLWJ5dGVzLWxvbmctZW5vdWdo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acc := account
		mu.Unlock()
		wantUser := "user-1"
		wantPlaylist := "Favourite Songs"
		if acc == 2 {
			wantUser = "user-2"
			wantPlaylist = "Second Account"
		}
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(200)
		case "/v1/login/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "ready", "signedIn": true})
		case "/v1/login":
			json.NewEncoder(w).Encode(map[string]any{"state": "ready", "signedIn": true})
		case "/v1/login/credentials":
			if r.Header.Get("Authorization") != "Bearer helper-tok" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"signedIn":       true,
				"mediaUserToken": wantUser,
				"developerToken": devTok,
				"storefront":     "th",
			})
		case "/us/browse":
			w.WriteHeader(http.StatusBadGateway)
		case "/v1/me/library/playlists":
			if r.Header.Get("Media-User-Token") != wantUser || r.Header.Get("Authorization") != "Bearer "+devTok {
				w.WriteHeader(403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "p.fav", "type": "library-playlists", "attributes": map[string]any{"name": wantPlaylist}}},
			})
		case "/v1/me/library/songs":
			if r.Header.Get("Media-User-Token") != wantUser {
				w.WriteHeader(403)
				return
			}
			if r.URL.Query().Get("limit") == "1" {
				json.NewEncoder(w).Encode(map[string]any{
					"data": []any{map[string]any{"id": "i.s1", "type": "library-songs", "attributes": map[string]any{"name": "Sandstorm", "artistName": "Darude"}}},
					"meta": map[string]any{"total": 1},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "i.s1", "type": "library-songs", "attributes": map[string]any{"name": "Sandstorm", "artistName": "Darude", "durationInMillis": 225000, "playParams": map[string]any{"catalogId": "338349243"}}}},
			})
		case "/v1/me/library/albums":
			if r.Header.Get("Media-User-Token") != wantUser {
				w.WriteHeader(403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "l.alb1", "type": "library-albums", "attributes": map[string]any{"name": "Before the Storm", "artistName": "Darude", "trackCount": 1}}},
			})
		case "/v1/me/library/albums/l.alb1/tracks":
			json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "i.s1", "type": "library-songs", "attributes": map[string]any{"name": "Sandstorm", "artistName": "Darude", "durationInMillis": 225000}}},
			})
		case "/v1/catalog/th/search":
			if r.URL.Query().Get("types") == "playlists" {
				json.NewEncoder(w).Encode(map[string]any{
					"results": map[string]any{"playlists": map[string]any{"data": []any{map[string]any{"id": "pl.srch1", "type": "playlists", "attributes": map[string]any{"name": "Sandstorm Mix", "trackCount": 10}}}}},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"results": map[string]any{"songs": map[string]any{"data": []any{map[string]any{"id": "338349243", "type": "songs", "attributes": map[string]any{"name": "Sandstorm", "artistName": "Darude", "durationInMillis": 225000}}}}},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	state := t.TempDir()
	uiDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("scope=applemusic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{
		AccessToken:           "console-fixture",
		StateDir:              state,
		UIDir:                 uiDir,
		AppleMusicURL:         srv.URL,
		AppleMusicWebURL:      srv.URL,
		AppleMusicHelperURL:   srv.URL,
		AppleMusicHelperToken: "helper-tok",
	}, nil)
	defer ui.Close()
	gw := httptest.NewServer(ui)
	defer gw.Close()

	// 1. Auto-restore from helper without any pre-existing applemusic-owner.json.
	var status struct {
		LoggedIn []string `json:"loggedIn"`
	}
	tidalGet(t, gw.URL, "/v1/enginex/ui", 200, &status)
	if !strings.Contains(strings.Join(status.LoggedIn, ","), "applemusic") {
		t.Fatalf("expected applemusic in loggedIn, got %+v", status.LoggedIn)
	}
	if _, err := os.Stat(filepath.Join(state, "applemusic-owner.json")); err != nil {
		t.Fatalf("expected persisted applemusic-owner.json: %v", err)
	}

	// 2. Playlists, favorite tracks, favorite albums, album tracks, and search playlists.
	var lists tidalList
	tidalGet(t, gw.URL, "/v1/users/1/playlists", 200, &lists)
	if len(lists.Items) != 3 {
		t.Fatalf("expected 3 playlists (liked, Favourite Songs, album), got %d: %s", len(lists.Items), lists.Items)
	}

	var favTracks struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/favorites/tracks", 200, &favTracks)
	if len(favTracks.Items) != 1 || favTracks.Items[0].Title != "Sandstorm" {
		t.Fatalf("favorite tracks: %+v", favTracks)
	}

	var favAlbums struct {
		Items []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/users/1/favorites/albums", 200, &favAlbums)
	if len(favAlbums.Items) != 1 || favAlbums.Items[0].Title != "Before the Storm" || favAlbums.Items[0].ID <= 0 || favAlbums.Items[0].ID > 1<<31-1 {
		t.Fatalf("favorite albums: %+v", favAlbums)
	}

	var albTracks struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	tidalGet(t, gw.URL, "/v1/albums/"+fmt.Sprint(favAlbums.Items[0].ID)+"/tracks", 200, &albTracks)
	if len(albTracks.Items) != 1 || albTracks.Items[0].Title != "Sandstorm" {
		t.Fatalf("album tracks: %+v", albTracks)
	}

	var searchRes struct {
		Tracks    tidalList `json:"tracks"`
		Playlists tidalList `json:"playlists"`
	}
	tidalGet(t, gw.URL, "/v1/search?query=sandstorm", 200, &searchRes)
	if searchRes.Tracks.TotalNumberOfItems != 1 || searchRes.Playlists.TotalNumberOfItems != 1 {
		t.Fatalf("search results: tracks=%+v playlists=%+v", searchRes.Tracks, searchRes.Playlists)
	}

	// 3. Console session reports ready=true and sign-out clears memory & owner file.
	sessReq, _ := http.NewRequest("POST", gw.URL+"/api/sessions", strings.NewReader(`{"provider":"applemusic"}`))
	sessReq.Header.Set("Authorization", "Bearer console-fixture")
	sessReq.Header.Set("Content-Type", "application/json")
	sessResp, err := http.DefaultClient.Do(sessReq)
	if err != nil {
		t.Fatal(err)
	}
	var sess Session
	json.NewDecoder(sessResp.Body).Decode(&sess)
	sessResp.Body.Close()
	if !sess.Ready {
		t.Fatalf("expected applemusic console session ready=true, got %+v", sess)
	}

	delReq, _ := http.NewRequest("DELETE", gw.URL+"/api/sessions/"+sess.ID, nil)
	delReq.Header.Set("Authorization", "Bearer console-fixture")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != 200 {
		t.Fatalf("delete session: %v %v", err, delResp)
	}
	delResp.Body.Close()
	if ui.appleMusic.Ready() || ui.appleMusic.HelperConfig().URL != srv.URL {
		t.Fatalf("logout should clear ready but keep helper: ready=%v cfg=%+v", ui.appleMusic.Ready(), ui.appleMusic.HelperConfig())
	}

	// 4. Signing into a second Apple Music account via helper clears ownerDeleted and drops browse cache.
	mu.Lock()
	account = 2
	mu.Unlock()
	loginReq, _ := http.NewRequest("POST", gw.URL+"/api/applemusic/login", strings.NewReader(`{"username":"dj@example.com","password":"pw"}`))
	loginReq.Header.Set("Authorization", "Bearer console-fixture")
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil || loginResp.StatusCode != 200 {
		t.Fatalf("helper login: %v %v", err, loginResp)
	}
	loginResp.Body.Close()

	var lists2 tidalList
	tidalGet(t, gw.URL, "/v1/users/1/playlists", 200, &lists2)
	if !strings.Contains(string(lists2.Items[1]), "Second Account") {
		t.Fatalf("expected invalidated browse cache with Second Account, got %s", lists2.Items)
	}
}
