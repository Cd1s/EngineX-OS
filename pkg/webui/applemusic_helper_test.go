package webui

import (
	"bytes"
	"context"
	"encoding/json"
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
	if got := request("GET", "/v1/enginex/applemusic", "", "", false); got.Code != 403 {
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
	if got := request("POST", "/v1/enginex/applemusic/login", `{"username":"fixture","password":"fixture"}`, "", true); got.Code != 200 {
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
	start := request("POST", "/v1/enginex/login?provider=applemusic", "", "", true)
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
			if !strings.Contains(page, tr(lang, state.key)) || !strings.Contains(page, state.field) || strings.Contains(page, `name="helperUrl"`) {
				t.Fatal("helper phone state")
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
