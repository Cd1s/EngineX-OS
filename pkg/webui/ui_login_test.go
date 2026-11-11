package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func deezerProfiles(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": 7, "title": "Mine", "nb_tracks": 1}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPhoneLoginNeedsTokenAndLinksDeezer(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, DeezerURL: deezerProfiles(t).URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	flow := ui.logins["deezer"]
	if flow == nil || !regexp.MustCompile(`:8080/m/deezer\?t=`).MatchString(flow.Target) {
		t.Fatalf("flow %+v", flow)
	}
	qr, err := http.Get(srv.URL + "/v1/sc5000/login/qr?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	qr.Body.Close()
	if qr.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr type %s", qr.Header.Get("Content-Type"))
	}
	bad, err := http.PostForm(srv.URL+"/m/deezer?t=wrong", url.Values{"user": {"123"}})
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 403 {
		t.Fatalf("wrong token: %d", bad.StatusCode)
	}
	token := flow.token
	// #8: holding the QR token must not re-point the relay.
	ok, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"https://www.deezer.com/profile/123?x=1"}, "relayUrl": {"https://evil.example"}, "deemixPassword": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	data, err := os.ReadFile(filepath.Join(state, "deezer-owner.json"))
	if err != nil || !strings.Contains(string(data), "123") {
		t.Fatalf("not saved: %s %v", data, err)
	}
	if relay, password := ui.relaySettings(); relay != "" || password != "" {
		t.Fatalf("phone page changed the relay: %q %q", relay, password)
	}
	// The token works once, and the deck learns that the login finished.
	again, err := http.PostForm(srv.URL+"/m/deezer?t="+token, url.Values{"user": {"456"}})
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != 403 {
		t.Fatalf("reused token: %d", again.StatusCode)
	}
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=deezer")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("status %s", body)
	}
}

// testSpotifyClientID stands in for an owner's own Spotify app.
const testSpotifyClientID = "0123456789abcdef0123456789abcdef"

func TestSpotifyPhoneClientIDAndAccessToken(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me" || r.Header.Get("Authorization") != "Bearer valid-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"id":"listener"}`)
	}))
	defer api.Close()
	state := t.TempDir()
	ui := New(Config{AccessToken: "console", UIDir: t.TempDir(), StateDir: state, SpotifyBaseURL: api.URL}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	token := ui.logins["spotify"].token
	page, err := http.Get(srv.URL + "/m/spotify?t=" + token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), `name="clientId"`) || !strings.Contains(string(body), `name="accessToken"`) {
		t.Fatal("missing Spotify fields")
	}
	resp, err = http.PostForm(srv.URL+"/m/spotify?t="+token, url.Values{"clientId": {testSpotifyClientID}, "accessToken": {"bad-token"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(state, "spotify-token.json")); !os.IsNotExist(err) {
		t.Fatal("invalid token saved")
	}
	resp, err = http.PostForm(srv.URL+"/m/spotify?t="+token, url.Values{"accessToken": {"valid-token"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var saved providers.Token
	b, err := os.ReadFile(filepath.Join(state, "spotify-token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &saved) != nil || saved.AccessToken != "valid-token" || saved.ClientID != testSpotifyClientID {
		t.Fatal("validated token not saved")
	}
}

// #104: the deck's Spotify login shows its QR code with no Client ID set, using the built-in one.
func TestSpotifyLoginWorksWithoutAnOwnerClientID(t *testing.T) {
	ui := New(Config{AccessToken: "test-console-code", UIDir: t.TempDir(), StateDir: t.TempDir(), SpotifyAccountsURL: "https://offline.example.invalid"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "/m/spotify?t=") {
		t.Fatalf("login without an ID: %d %s", resp.StatusCode, body)
	}
	if ui.spotifyClientID() != DefaultSpotifyClientID {
		t.Fatalf("client id %q", ui.spotifyClientID())
	}
	request(t, srv.Client(), srv.URL, "POST", "/api/spotify", map[string]any{"clientId": "not-an-id"}, 400)
	var got struct {
		Set         bool
		ClientID    string
		RedirectURI string
	}
	json.Unmarshal(request(t, srv.Client(), srv.URL, "POST", "/api/spotify", map[string]any{"clientId": strings.ToUpper(testSpotifyClientID)}, 200), &got)
	if !got.Set || strings.Contains(got.ClientID, testSpotifyClientID) || got.RedirectURI != spotifyLoopbackRedirect {
		t.Fatalf("saved: %+v", got)
	}
	resp, err = http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	page, err := http.Get(srv.URL + "/m/spotify?t=" + ui.logins["spotify"].token)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), testSpotifyClientID) {
		t.Fatalf("page %s", body)
	}
}

// A token minted by another app (the old built-in ID) is not used.
func TestSpotifyTokenFromAnotherAppIsDropped(t *testing.T) {
	state := t.TempDir()
	tokenPath := filepath.Join(state, "spotify-token.json")
	os.WriteFile(tokenPath, []byte(`{"access_token":"AT","refresh_token":"RT","client_id":"ffffffffffffffffffffffffffffffff"}`), 0o600)
	os.WriteFile(filepath.Join(state, "spotify-client-id"), []byte(testSpotifyClientID+"\n"), 0o600)
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, SpotifyTokenPath: tokenPath}, nil)
	p, err := ui.makeProvider("spotify", false, "")
	if err != nil {
		t.Fatal(err)
	}
	sp := p.(*providers.Spotify)
	if sp.Token.AccessToken != "" || sp.ClientID != testSpotifyClientID {
		t.Fatalf("kept: %q %q", sp.Token.AccessToken, sp.ClientID)
	}
}

func TestSpotifyPhoneLoginSurvivesASlowTwoFactorDetour(t *testing.T) {
	accounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/api/token" || r.Form.Get("code") != "CODE" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "AT", "refresh_token": "RT", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer accounts.Close()
	ui := New(Config{AccessToken: "code", SpotifyClientID: testSpotifyClientID, UIDir: t.TempDir(), StateDir: t.TempDir(), SpotifyAccountsURL: accounts.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	get := func(path string) string {
		t.Helper()
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	token := ui.logins["spotify"].token
	state := regexp.MustCompile(`state=([A-Za-z0-9_-]+)`).FindStringSubmatch(get("/m/spotify?t=" + token))
	if state == nil {
		t.Fatal("no authorize link on the phone page")
	}
	ui.mu.Lock()
	ui.logins["spotify"].expires = ui.logins["spotify"].expires.Add(-25 * time.Minute)
	for k, v := range ui.pending {
		v.expires = v.expires.Add(-25 * time.Minute)
		ui.pending[k] = v
	}
	ui.mu.Unlock()
	if s := get("/v1/sc5000/login?provider=spotify"); strings.Contains(s, "expired") {
		t.Fatalf("the deck gave up on the QR code: %s", s)
	}
	get("/m/spotify?t=" + token) // the browser reloads the page on return
	callback := "http://127.0.0.1:8080/oauth/spotify/callback?code=CODE&state=" + state[1]
	done, err := http.PostForm(srv.URL+"/m/spotify?t="+token, url.Values{"callback": {callback}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(done.Body)
	done.Body.Close()
	if !strings.Contains(string(body), "Connected") {
		t.Fatalf("pasted callback refused: %s", body)
	}
	if s := get("/v1/sc5000/login?provider=spotify"); !strings.Contains(s, `"status":"done"`) {
		t.Fatalf("deck status %s", s)
	}
}

func TestSavedSpotifyClientBeatsTheDefault(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "spotify-client-id"), []byte("mine\n"), 0o600)
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state}, nil)
	if got := ui.spotifyClientID(); got != "mine" {
		t.Fatal(got)
	}
}

func TestPhonePostAcceptsNullOriginButConsoleRejectsForeignOrigin(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir(), DeezerURL: deezerProfiles(t).URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider=deezer", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	post := func(path, origin, form string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := post("/m/deezer?t="+ui.logins["deezer"].token, "null", "user=123"); code != 200 {
		t.Fatalf("null origin on phone page: %d", code)
	}
	if code := post("/api/sessions", "https://evil.example", "{}"); code != 403 {
		t.Fatalf("foreign origin on console: %d", code)
	}
}

func TestRelayLoginCollectsBothServices(t *testing.T) {
	const arl = "abcdef0123456789abcdef0123456789"
	var registered []map[string]string
	status := "waiting"
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/relay/session" {
			var v map[string]string
			json.NewDecoder(r.Body).Decode(&v)
			registered = append(registered, v)
			io.WriteString(w, `{}`)
			return
		}
		if r.URL.Path != "/api/relay/session" || r.URL.Query().Get("t") == "" {
			t.Errorf("unexpected %s", r.URL)
		}
		out := map[string]any{"status": status}
		if status == "done" {
			out["spotifyToken"] = map[string]any{"access_token": "AT", "refresh_token": "RT", "expires_in": 3600, "client_id": "cid"}
			out["deezer"] = map[string]any{"userId": "42", "arl": arl}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer relay.Close()
	state := t.TempDir()
	tokenPath := filepath.Join(state, "spotify-token.json")
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, SpotifyTokenPath: tokenPath, RelayURL: relay.URL, SpotifyClientID: testSpotifyClientID, SpotifyBaseURL: "https://offline.example.invalid"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	poll := func(provider string) string {
		r, err := http.Get(srv.URL + "/v1/sc5000/login?provider=" + provider)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v struct{ Status string }
		json.NewDecoder(r.Body).Decode(&v)
		return v.Status
	}
	for _, provider := range []string{"spotify", "deezer"} {
		status = "scanned"
		r, err := http.Post(srv.URL+"/v1/sc5000/login?provider="+provider, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var flow struct{ Target string }
		json.NewDecoder(r.Body).Decode(&flow)
		r.Body.Close()
		if !strings.HasPrefix(flow.Target, relay.URL+"/m/"+provider+"?t=") {
			t.Fatalf("target %s", flow.Target)
		}
		if got := poll(provider); got != "scanned" {
			t.Fatalf("%s: %s", provider, got)
		}
		status = "done"
		if got := poll(provider); got != "done" {
			t.Fatalf("%s: %s", provider, got)
		}
	}
	if len(registered) != 2 || registered[0]["spotifyClientId"] != testSpotifyClientID {
		t.Fatalf("registered %v", registered)
	}
	saved, err := os.ReadFile(tokenPath)
	if err != nil || !strings.Contains(string(saved), `"refresh_token":"RT"`) {
		t.Fatalf("token %s %v", saved, err)
	}
	owner := ui.savedDeezerOwner()
	if owner.UserID != "42" || owner.ARL != arl {
		t.Fatalf("owner %+v", owner)
	}
}

func TestRelayRequestsCarryThePassword(t *testing.T) {
	var got string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Deemix-Password") + "|" + r.Header.Get("Authorization")
		io.WriteString(w, `{}`)
	}))
	defer relay.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	if err := ui.updateRelayConfig(nil, ptr("pw")); err != nil {
		t.Fatal(err)
	}
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if got != "pw|Bearer pw" {
		t.Fatalf("%q", got)
	}
}

func TestRelayRefusesToFollowRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer relay.Close()
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	ui.updateRelayConfig(nil, ptr("pw"))
	ui.relayRegister(context.Background(), &loginFlow{Provider: "deezer", token: "t"})
	if leaked {
		t.Fatal("password followed a redirect")
	}
}

func TestRelayOutageStillShowsItsQRCode(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer relay.Close()
	ui := New(Config{AccessToken: "code", SpotifyClientID: testSpotifyClientID, SpotifyAccountsURL: "https://offline.example.invalid", UIDir: t.TempDir(), StateDir: t.TempDir(), RelayURL: relay.URL}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(ui.logins["spotify"].Target, relay.URL+"/m/spotify") {
		t.Fatalf("%d %+v", r.StatusCode, ui.logins["spotify"])
	}
}

func TestRelayPasswordIsNeverReadBack(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: state, RelayURL: "https://default.example"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/relay", "application/json", strings.NewReader(`{"deemixPassword":"hunter2"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(body), "hunter2") || !strings.Contains(string(body), `"deemixPasswordSet":true`) || !strings.Contains(string(body), "default.example") {
		t.Fatalf("status %s", body)
	}
	if relay, password := ui.relaySettings(); relay != "https://default.example" || password != "hunter2" {
		t.Fatal(relay, password)
	}
	info, err := os.Stat(filepath.Join(state, "deemix-config.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	r, err = http.Post(srv.URL+"/v1/sc5000/ui?deemixPassword=changed", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(body), "changed") {
		t.Fatalf("status %s", body)
	}
	if _, password := ui.relaySettings(); password != "changed" {
		t.Fatal(password)
	}
	// #9: a rejected request leaves the relay alone.
	r, err = http.Post(srv.URL+"/v1/sc5000/ui?lang=xx&relayUrl=https://other.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if relay, password := ui.relaySettings(); r.StatusCode != 400 || relay != "https://default.example" || password != "changed" {
		t.Fatalf("%d %q %q", r.StatusCode, relay, password)
	}
	// #8: the password stays with its host; the same host keeps it.
	ui.updateRelayConfig(ptr("https://DEFAULT.example/"), nil)
	if _, password := ui.relaySettings(); password != "changed" {
		t.Fatal("same host lost the password")
	}
	ui.updateRelayConfig(ptr("https://other.example"), nil)
	if _, password := ui.relaySettings(); password != "" {
		t.Fatal("password followed the relay to another host")
	}
	r, err = http.Post(srv.URL+"/v1/sc5000/relay", "application/json", strings.NewReader(`{"relayUrl":"http://public.example"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 400 {
		t.Fatalf("plain HTTP accepted: %d", r.StatusCode)
	}
}

func netEaseServer(t *testing.T, audioURL func() string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/weapi/song/enhance/player/url/v1":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": []any{map[string]any{"url": audioURL()}}})
		case "/weapi/cloudsearch/pc":
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "result": map[string]any{"songs": []any{map[string]any{"id": 5, "name": "Song", "dt": 203000, "ar": []any{map[string]any{"name": "Artist"}}}}}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchPrefersLocalDeemixThenFallsBackToNetease(t *testing.T) {
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	var neteaseCalls, bridgeCalls atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("login cookie sent to the audio host")
		}
		w.Write(flac.Bytes())
	}))
	defer cdn.Close()
	netease := netEaseServer(t, func() string { neteaseCalls.Add(1); return cdn.URL + "/song.flac" })
	dir := t.TempDir()
	bridgeUp := atomic.Bool{}
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		if !bridgeUp.Load() {
			http.Error(w, "no match", 500)
			return
		}
		os.WriteFile(filepath.Join(dir, "9.flac"), flac.Bytes(), 0o644)
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "j", State: "done", Progress: 1, Path: "9.flac"})
	}))
	defer bridge.Close()
	catalog := &providers.Deezer{BaseURL: fakeDeezer(t).URL}
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
	defer jobs.Close()
	New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) {
		return &providers.Netease{WebURL: netease.URL, Cookie: "MUSIC_U=secret", UserID: 1}, nil
	}}, jobs)
	ctx := context.Background()

	// With Deemix working, NetEase's download quota is never touched.
	bridgeUp.Store(true)
	deezer := providers.Track{ID: "9", Provider: "deezer", Title: "Rare", Artist: "Nobody", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", deezer, false); err != nil || filepath.Base(p) != "9.flac" {
		t.Fatalf("%q %v", p, err)
	}
	if neteaseCalls.Load() != 0 {
		t.Fatal("NetEase used while Deemix works")
	}

	// A NetEase-only track falls back to the login, by its own ID.
	bridgeUp.Store(false)
	own := providers.Track{ID: "77", Provider: "netease", Title: "Song", Artist: "Artist", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", own, false); err != nil || filepath.Base(p) != "netease-77.flac" {
		t.Fatalf("%q %v", p, err)
	}
	// Another service's track is never matched on NetEase.
	other := providers.Track{ID: "abc", Provider: "spotify", Title: "Song", Artist: "Artist", DurationMS: 200000}
	if p, err := jobs.Fetch(ctx, "s", other, false); err == nil {
		t.Fatalf("spotify track played from %q", p)
	}
	// A saved NetEase file is reused without asking anyone.
	jobs2 := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
	defer jobs2.Close()
	calls := bridgeCalls.Load()
	if p, err := jobs2.Fetch(ctx, "s", own, false); err != nil || filepath.Base(p) != "netease-77.flac" || bridgeCalls.Load() != calls {
		t.Fatalf("%q %v", p, err)
	}
}

func TestNeteaseModeChoosesTheDownloadSource(t *testing.T) {
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	var neteaseCalls, bridgeCalls atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(flac.Bytes()) }))
	defer cdn.Close()
	netease := netEaseServer(t, func() string { neteaseCalls.Add(1); return cdn.URL + "/song.flac" })
	dir := t.TempDir()
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		os.WriteFile(filepath.Join(dir, "9.flac"), flac.Bytes(), 0o644)
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "j", State: "done", Progress: 1, Path: "9.flac"})
	}))
	defer bridge.Close()
	catalog := &providers.Deezer{BaseURL: fakeDeezer(t).URL}
	loggedIn := true
	setup := func(mode string) *Jobs {
		uiDir := t.TempDir()
		if mode != "" {
			os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("netease_mode="+mode+"\n"), 0o644)
		}
		jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
		t.Cleanup(jobs.Close)
		New(Config{AccessToken: "c", UIDir: uiDir, StateDir: t.TempDir(), Factory: func(string, bool, string) (providers.Provider, error) {
			if !loggedIn {
				return &providers.Netease{WebURL: netease.URL}, nil
			}
			return &providers.Netease{WebURL: netease.URL, Cookie: "MUSIC_U=secret", UserID: 1}, nil
		}}, jobs)
		return jobs
	}
	ctx := context.Background()
	track := providers.Track{ID: "31", Provider: "netease", Title: "Found", Artist: "Artist", DurationMS: 200000}

	// Default: straight from NetEase, Deemix never asked.
	if p, err := setup("").Fetch(ctx, "s", track, false); err != nil || filepath.Base(p) != "netease-31.flac" || bridgeCalls.Load() != 0 || neteaseCalls.Load() != 1 {
		t.Fatalf("%q %v bridge=%d netease=%d", p, err, bridgeCalls.Load(), neteaseCalls.Load())
	}
	os.Remove(filepath.Join(dir, "netease-31.flac"))
	// Deemix first spares the NetEase quota.
	if p, err := setup("deemix").Fetch(ctx, "s", track, false); err != nil || filepath.Base(p) != "9.flac" || bridgeCalls.Load() != 1 || neteaseCalls.Load() != 1 {
		t.Fatalf("%q %v bridge=%d netease=%d", p, err, bridgeCalls.Load(), neteaseCalls.Load())
	}
	// Official mode without a NetEase login says what is missing.
	loggedIn = false
	if _, err := setup("official").Fetch(ctx, "s", providers.Track{ID: "32", Provider: "netease", Title: "Found", Artist: "Artist"}, false); err == nil || !strings.Contains(err.Error(), "log in to NetEase") || bridgeCalls.Load() != 1 {
		t.Fatalf("%v bridge=%d", err, bridgeCalls.Load())
	}
}

func TestGuestNeverUsesTheOwnersNeteaseLogin(t *testing.T) {
	state := t.TempDir()
	os.WriteFile(filepath.Join(state, "netease-owner.json"), []byte(`{"Cookie":"MUSIC_U=owner","UserID":1}`), 0o600)
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state}, nil)
	if ui.neteaseFor("nobody", false) == nil {
		t.Fatal("owner login not found")
	}
	if ui.neteaseFor("nobody", true) != nil {
		t.Fatal("guest got the owner's login")
	}
}

func ptr[T any](v T) *T { return &v }

func TestRelayBPMKeysSyncToTheDeck(t *testing.T) {
	var auth string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("X-Deemix-Password")
		if r.URL.RequestURI() != "/api/config?secrets=1" {
			t.Errorf("unexpected %s", r.URL)
		}
		io.WriteString(w, `{"ok":true,"config":{"soundcloudClientId":"from-relay","getSongBpmKey":""}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	bpm.SaveConfig(providers.BPMConfig{GetSongBPMKey: "local", SoundCloudClientID: "old"})
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL, BPM: bpm}, nil)

	// Nothing is sent or asked without the relay's password.
	if err := ui.pullBPMConfig(context.Background()); err != nil || auth != "" {
		t.Fatalf("%v %q", err, auth)
	}
	ui.updateRelayConfig(nil, ptr("pw"))
	if err := ui.pullBPMConfig(context.Background()); err != nil || auth != "pw" {
		t.Fatalf("%v %q", err, auth)
	}
	got := bpm.Config()
	if got.SoundCloudClientID != "from-relay" || got.GetSongBPMKey != "local" {
		t.Fatalf("%+v", got)
	}
}

func TestPreviewAnalyzerMeasuresAClipAndKeepsNothing(t *testing.T) {
	var clip bytes.Buffer
	if err := testaudio.FLAC(&clip, testaudio.Song(30)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/short" {
			var short bytes.Buffer
			testaudio.FLAC(&short, testaudio.Song(2))
			w.Write(short.Bytes())
			return
		}
		w.Write(clip.Bytes())
	}))
	defer srv.Close()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	analyze := PreviewAnalyzer(resourceguard.New(resourceguard.Config{Disabled: true}))
	if got := analyze(context.Background(), srv.URL+"/clip"); got.BPM <= 0 || got.Measured {
		t.Fatalf("%+v", got)
	}
	if got := analyze(context.Background(), srv.URL+"/short"); got.BPM != 0 {
		t.Fatalf("short clip: %+v", got)
	}
	if got := analyze(context.Background(), srv.URL+"/missing\x7f"); got.BPM != 0 {
		t.Fatalf("%+v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
}

func TestUnblockRadiosLeavesOtherSwitchesAlone(t *testing.T) {
	root := t.TempDir()
	for name, kind := range map[string]string{"rfkill0": "bluetooth", "rfkill1": "wlan", "rfkill2": "nfc"} {
		os.MkdirAll(filepath.Join(root, name), 0o755)
		os.WriteFile(filepath.Join(root, name, "type"), []byte(kind+"\n"), 0o644)
		os.WriteFile(filepath.Join(root, name, "soft"), []byte("1"), 0o644)
	}
	UnblockRadios(root)
	for name, want := range map[string]string{"rfkill0": "0", "rfkill1": "0", "rfkill2": "1"} {
		if got, _ := os.ReadFile(filepath.Join(root, name, "soft")); string(got) != want {
			t.Fatalf("%s: %s", name, got)
		}
	}
}

func TestLongTracksSkipAnalysis(t *testing.T) {
	if !tooLongToAnalyse(providers.Track{DurationMS: 2 * 60 * 60 * 1000}, "/x/a.mp3") {
		t.Fatal("two-hour set would be analysed")
	}
	if tooLongToAnalyse(providers.Track{DurationMS: 5 * 60 * 1000}, "/missing/a.flac") {
		t.Fatal("ordinary track skipped")
	}
}

func TestRelayCollectsSoundCloudAndDeckChecksIt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if id, secret, ok := r.BasicAuth(); !ok || id != "cid" || secret != "shh" {
				t.Error("token request without the app's credentials")
			}
			io.WriteString(w, `{"access_token":"app","expires_in":3600}`)
		case "/resolve":
			if r.Header.Get("Authorization") != "OAuth app" {
				t.Errorf("no token on %s", r.URL)
			}
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		default:
			io.WriteString(w, `{"collection":[]}`)
		}
	}))
	defer api.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"done","soundcloud":{"user":"cave","clientId":"cid","clientSecret":"shh"}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL, SoundCloudURL: api.URL, SoundCloudTokenURL: api.URL + "/token", BPM: bpm}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=soundcloud", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if !strings.HasPrefix(ui.logins["soundcloud"].Target, relay.URL+"/m/soundcloud?t=") {
		t.Fatalf("%+v", ui.logins["soundcloud"])
	}
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=soundcloud")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("%s", body)
	}
	if p := ui.ownerSoundCloud(); p == nil || p.UserID != 7 || p.ClientID != "cid" {
		t.Fatalf("%+v", p)
	}
	if got := bpm.Config().SoundCloudClientID; got != "cid" {
		t.Fatal(got)
	}
	info, err := os.Stat(filepath.Join(state, "soundcloud-owner.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

func TestStrayEthernetDefaultOnlyWhenWiFiHasARoute(t *testing.T) {
	wifi := "default via 192.168.1.6 dev wlan0 \n"
	if got, ok := strayEthernetDefault("0.0.0.0 dev eth0 scope link \n" + wifi + "169.254.0.0/16 dev eth0 scope link src 169.254.1.2\n"); !ok || got != "0.0.0.0" {
		t.Fatal(got, ok)
	}
	if got, ok := strayEthernetDefault("default dev eth0 scope link \n" + wifi); !ok || got != "default" {
		t.Fatal(got, ok)
	}
	if _, ok := strayEthernetDefault(wifi + "169.254.0.0/16 dev eth0 scope link\n"); ok {
		t.Fatal("removed nothing stray")
	}
	if _, ok := strayEthernetDefault("0.0.0.0 dev eth0 scope link \n"); ok {
		t.Fatal("would cut the only route")
	}
	if _, ok := strayEthernetDefault("default via 10.0.0.1 dev eth0 \n" + wifi); ok {
		t.Fatal("touched a real Ethernet gateway")
	}
}

func TestProviderTracksCarryCovers(t *testing.T) {
	deezer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":1,"title":"S","artist":{"name":"A"},"album":{"title":"L","cover_big":"https://cdn/big.jpg"}}]}`)
	}))
	defer deezer.Close()
	found, err := (&providers.Deezer{BaseURL: deezer.URL}).Search(context.Background(), "q")
	if err != nil || found[0].CoverURL != "https://cdn/big.jpg" {
		t.Fatalf("%+v %v", found, err)
	}
}

func TestSoundCloudAppCredentialsSearchButAreNotALogin(t *testing.T) {
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	bpm.SaveConfig(providers.BPMConfig{SoundCloudClientID: "cid", SoundCloudSecret: "shh"})
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, BPM: bpm}, nil)
	p := ui.ownerSoundCloud()
	if p == nil {
		t.Fatal("search and download need the app's credentials to count")
	}
	if ready(p) {
		t.Fatal("an app token must still offer the login QR code")
	}
	p.UserID = 7
	if !ready(p) {
		t.Fatal("a bound profile is a login")
	}
}

func TestRelayDeliversASoundCloudAccountLogin(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"done","soundcloud":{"userId":8,"username":"me","userToken":"u1","refreshToken":"r1","expiry":"2099-01-01T00:00:00Z"}}`)
	}))
	defer relay.Close()
	state := t.TempDir()
	bpm := &providers.BPMStore{Path: filepath.Join(state, "bpm.json"), ConfigPath: filepath.Join(state, "bpm-config.json")}
	bpm.SaveConfig(providers.BPMConfig{SoundCloudClientID: "cid", SoundCloudSecret: "shh"})
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: state, RelayURL: relay.URL, BPM: bpm}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=soundcloud", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	status, err := http.Get(srv.URL + "/v1/sc5000/login?provider=soundcloud")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(status.Body)
	status.Body.Close()
	if !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("%s", body)
	}
	p := ui.ownerSoundCloud()
	if p == nil || p.UserID != 8 || !ready(p) || !p.Auth.HasUser() {
		t.Fatalf("%+v", p)
	}
	owner, _ := os.ReadFile(filepath.Join(state, "soundcloud-owner.json"))
	if strings.Contains(string(owner), "u1") {
		t.Fatal("token copied into the owner file")
	}
}

// A restarted stream-sync finds another service's track by the file it was
// saved as: no catalog search, no download.
func TestRestartReusesADownloadWithoutAskingAnyone(t *testing.T) {
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(2)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var bridgeCalls, catalogCalls atomic.Int32
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgeCalls.Add(1)
		os.WriteFile(filepath.Join(dir, "9.flac"), flac.Bytes(), 0o644)
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "j", State: "done", Progress: 1, Path: "9.flac"})
	}))
	defer bridge.Close()
	catalog := countingCatalog{&providers.Deezer{BaseURL: fakeDeezer(t).URL}, &catalogCalls}
	start := func() *Jobs {
		j := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: catalog, Deemix: &deemixclient.Client{BaseURL: bridge.URL, DownloadDir: dir, AllowLossy: true}})
		t.Cleanup(j.Close)
		return j
	}
	ctx := context.Background()
	track := providers.Track{ID: "sp1", Provider: "netease", Title: "Found", Artist: "Artist", DurationMS: 200000}
	if p, err := start().Fetch(ctx, "s", track, false); err != nil || filepath.Base(p) != "9.flac" || bridgeCalls.Load() != 1 {
		t.Fatalf("%q %v bridge=%d", p, err, bridgeCalls.Load())
	}
	searched := catalogCalls.Load()
	again := start()
	if p := again.File(track); filepath.Base(p) != "9.flac" {
		t.Fatalf("File after restart: %q", p)
	}
	if p, err := again.Fetch(ctx, "s", track, false); err != nil || filepath.Base(p) != "9.flac" {
		t.Fatalf("%q %v", p, err)
	}
	if bridgeCalls.Load() != 1 || catalogCalls.Load() != searched {
		t.Fatalf("restart asked again: bridge=%d catalog=%d->%d", bridgeCalls.Load(), searched, catalogCalls.Load())
	}
	// The same Deezer recording, played directly, is not downloaded again either.
	if p, err := start().Fetch(ctx, "s", providers.Track{ID: "9", Provider: "deezer", Title: "Found", Artist: "Artist", DurationMS: 200000}, false); err != nil || filepath.Base(p) != "9.flac" || bridgeCalls.Load() != 1 {
		t.Fatalf("%q %v bridge=%d", p, err, bridgeCalls.Load())
	}
}

type countingCatalog struct {
	deemixclient.Catalog
	calls *atomic.Int32
}

func (c countingCatalog) Search(ctx context.Context, q string) ([]providers.Track, error) {
	c.calls.Add(1)
	return c.Catalog.Search(ctx, q)
}

func TestSpotifyConnectEndpoints(t *testing.T) {
	state := t.TempDir()
	ui := New(Config{AccessToken: "console", UIDir: t.TempDir(), StateDir: state, SpotifyBaseURL: "https://offline.example.invalid"}, nil)
	defer ui.Close()
	srv := httptest.NewServer(ui)
	defer srv.Close()

	// 1. GET /spotify-connect?action=getInfo
	resp, err := http.Get(srv.URL + "/spotify-connect?action=getInfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /spotify-connect status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		Status       int    `json:"status"`
		StatusString string `json:"statusString"`
		DeviceID     string `json:"deviceID"`
		RemoteName   string `json:"remoteName"`
		Version      string `json:"version"`
		AccountReq   string `json:"accountReq"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("unmarshal info: %v", err)
	}
	if info.Status != 101 || info.StatusString != "OK" || info.DeviceID != func() string {
		id, err := providers.LoadSpotifyDeviceID(state)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}() || info.RemoteName != "EngineX OS" {
		t.Fatalf("unexpected info response: %+v", info)
	}

	// 2. Start a spotify login flow to test link update
	r, err := http.Post(srv.URL+"/v1/sc5000/login?provider=spotify", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if ui.logins["spotify"] == nil || ui.logins["spotify"].Status != "waiting" {
		t.Fatalf("unexpected flow status: %+v", ui.logins["spotify"])
	}

	// 3. POST /spotify-connect
	postResp, err := http.PostForm(srv.URL+"/spotify-connect", url.Values{
		"action":      {"addUser"},
		"userName":    {"testuser"},
		"accessToken": {"test-connect-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != 200 {
		t.Fatalf("POST /spotify-connect status %d", postResp.StatusCode)
	}
	var res struct {
		Status       int    `json:"status"`
		StatusString string `json:"statusString"`
	}
	postBody, _ := io.ReadAll(postResp.Body)
	if err := json.Unmarshal(postBody, &res); err != nil || res.Status != 101 || res.StatusString != "OK" {
		t.Fatalf("unexpected post response: %s", postBody)
	}

	// Flow status must be "linked"
	if ui.logins["spotify"].Status != "linked" {
		t.Fatalf("expected flow status 'linked', got %q", ui.logins["spotify"].Status)
	}

	// Token must be saved to disk
	savedBytes, err := os.ReadFile(filepath.Join(state, "spotify-token.json"))
	if err != nil {
		t.Fatalf("spotify-token.json was not created: %v", err)
	}
	var savedTok providers.Token
	if err := json.Unmarshal(savedBytes, &savedTok); err != nil || savedTok.AccessToken != "test-connect-token" {
		t.Fatalf("token mismatch: %+v", savedTok)
	}
}
