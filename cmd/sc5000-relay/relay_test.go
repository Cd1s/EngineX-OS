package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// defaultPassword is the console password the test servers are set up with.
const defaultPassword = "test-relay-password"

// testSpotifyClientID stands in for an operator's explicitly configured app.
const testSpotifyClientID = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	cfg, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.setPassword(defaultPassword); err != nil {
		t.Fatal(err)
	}
	if err := cfg.update(func(c *config) { c.SpotifyClientID = testSpotifyClientID }); err != nil {
		t.Fatal(err)
	}
	target, _ := parseStemsTarget("127.0.0.1:1")
	s := &server{
		public:      "https://relay.example.com",
		cfg:         cfg,
		fails:       newLimiter(failWindow, failMax),
		newSessions: newLimiter(time.Minute, 30),
		sessions:    newSessionStore(filepath.Join(dir, "sessions.json")),
		decks:       newDeckStore(filepath.Join(dir, "decks.json")),
		deezer:      newDeezerClient(),
		stems:       newStemsProxy(target, ""),
	}
	s.spotify = newSpotifyAuth(filepath.Join(dir, "learned.json"), s.public+"/oauth/spotify/callback")
	s.spotify.probes[testSpotifyClientID] = probeResult{at: time.Now()} // no network probe
	return s
}

func do(h http.Handler, method, target string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCredentialOnlyEndpoints(t *testing.T) {
	h := newTestServer(t).routes()
	for _, prefix := range []string{"", "/sc5000"} {
		for _, p := range []string{"/api/audio", "/api/download", "/api/download-stream", "/api/queue", "/api/download/job"} {
			for _, method := range []string{"GET", "POST", "PUT"} {
				w := do(h, method, prefix+p, "", nil)
				if w.Code != 410 || !strings.Contains(w.Body.String(), audioGoneMessage) {
					t.Errorf("%s %s: %d %s", method, prefix+p, w.Code, w.Body)
				}
			}
		}
	}
}

func TestConfigSecrets(t *testing.T) {
	s := newTestServer(t)
	arl := strings.Repeat("a", 192)
	if err := s.cfg.update(func(c *config) { c.DeezerARL = arl }); err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	for _, hdr := range []map[string]string{nil, {"X-Deemix-Password": "wrong"}} {
		w := do(h, "GET", "/api/config?secrets=1", "", hdr)
		if w.Code != 401 || strings.Contains(w.Body.String(), arl) {
			t.Fatal("unprotected secrets")
		}
	}
	for _, hdr := range []map[string]string{{"X-Deemix-Password": defaultPassword}, {"Authorization": "Bearer " + defaultPassword}} {
		for _, query := range []string{"", "?secrets=0", "?secrets=1"} {
			w := do(h, "GET", "/api/config"+query, "", hdr)
			var v struct{ Config map[string]any }
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			got, exists := v.Config["deezerArl"]
			if w.Code != 200 || exists != (query == "?secrets=1") || (exists && got != arl) {
				t.Fatal("wrong secret opt-in response")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("secrets must not be cached")
			}
		}
	}
}

func TestLoginCookieAndPasswordChange(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	if w := do(h, "GET", "/", "", nil); !strings.Contains(w.Body.String(), `name="password"`) {
		t.Fatal("login page expected")
	}
	w := do(h, "POST", "/login", "password="+defaultPassword, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != 303 {
		t.Fatalf("login: %d", w.Code)
	}
	cookie := w.Result().Cookies()[0]
	hdr := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
	if w := do(h, "GET", "/api/config", "", hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"passwordIsDefault":false`) {
		t.Fatalf("cookie auth: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/sc5000/", "", hdr); !strings.Contains(w.Body.String(), "EngineX OS 控制台") {
		t.Fatal("panel expected under /sc5000/")
	}
	hdr["Content-Type"] = "application/json"
	if w := do(h, "POST", "/api/config", `{"currentPassword":"nope","newPassword":"another-pass-1"}`, hdr); w.Code != 403 {
		t.Fatalf("wrong current password: %d", w.Code)
	}
	if w := do(h, "POST", "/api/config", `{"currentPassword":"`+defaultPassword+`","newPassword":"another-pass-1"}`, hdr); w.Code != 200 {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	delete(hdr, "Content-Type")
	if w := do(h, "GET", "/api/config", "", hdr); w.Code != 401 {
		t.Fatalf("old cookie must be invalid after password change: %d", w.Code)
	}
	if w := do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": defaultPassword}); w.Code != 401 {
		t.Fatal("old password still accepted")
	}
	if w := do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": "another-pass-1"}); w.Code != 200 {
		t.Fatal("new password rejected")
	}
}

func TestFailLimiter(t *testing.T) {
	h := newTestServer(t).routes()
	for i := 0; i < failMax; i++ {
		do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": "bad"})
	}
	if w := do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": defaultPassword}); w.Code != 429 {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestSessionFlowBothPrefixes(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	tok := "abcdefghijklmnop1234"
	w := do(h, "POST", "/sc5000/api/relay/session", `{"token":"`+tok+`","provider":"spotify","spotifyClientId":""}`, nil)
	var cr struct {
		OK  bool
		URL string
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cr)
	if !cr.OK || cr.URL != "https://relay.example.com/m/spotify?t="+tok {
		t.Fatalf("create: %s", w.Body)
	}
	poll := func(p string) pollResponse {
		var pr pollResponse
		_ = json.Unmarshal(do(h, "GET", p+"/api/relay/session?t="+tok, "", nil).Body.Bytes(), &pr)
		return pr
	}
	if pr := poll(""); pr.Status != "waiting" || pr.Provider != "spotify" {
		t.Fatalf("poll: %+v", pr)
	}
	w = do(h, "GET", "/m/spotify?manual=1&t="+tok, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "client_id="+testSpotifyClientID) ||
		!strings.Contains(w.Body.String(), "127.0.0.1%3a8080") && !strings.Contains(w.Body.String(), "127.0.0.1%3A8080") {
		t.Fatalf("spotify page: %d", w.Code)
	}
	if pr := poll("/sc5000"); pr.Status != "scanned" {
		t.Fatalf("after scan: %+v", pr)
	}
	// Stateless fallback: unregistered token opened by phone.
	if w := do(h, "GET", "/sc5000/m/spotify?manual=1&t=testtoken1234567890", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), testSpotifyClientID) {
		t.Fatalf("fallback page: %d", w.Code)
	}
	if w := do(h, "GET", "/m/deezer?t=short", "", nil); w.Code != 400 {
		t.Fatalf("short token: %d", w.Code)
	}
	// Done sessions survive repeated polls.
	s.sessions.update(tok, func(x *session) { x.Status = "done"; x.SpotifyToken = &spotifyToken{AccessToken: "a"} })
	for i := 0; i < 2; i++ {
		if pr := poll(""); pr.Status != "done" || pr.SpotifyToken == nil {
			t.Fatalf("poll %d: %+v", i, pr)
		}
	}
	s.sessions.update(tok, func(x *session) { x.Expires = time.Now().Add(-time.Second) })
	if pr := poll(""); pr.Status != "expired" || pr.SpotifyToken != nil {
		t.Fatalf("expired: %+v", pr)
	}
}

func TestParsePastedCode(t *testing.T) {
	cases := []struct{ in, code, redirect string }{
		{"http://127.0.0.1:8080/oauth/spotify/callback?code=AQB123&state=x", "AQB123", spotifyLoopbackURI},
		{"  AQBxyz  ", "AQBxyz", spotifyLoopbackURI},
		{"打开了 http://127.0.0.1:8080/oauth/spotify/callback?code=AQC9&state=t 页面", "AQC9", spotifyLoopbackURI},
		{"https://relay.example.com/oauth/spotify/callback?code=Z&state=t", "Z", "https://relay.example.com/oauth/spotify/callback"},
	}
	for _, c := range cases {
		code, red, err := parsePastedCode(c.in)
		if err != nil || code != c.code || red != c.redirect {
			t.Errorf("%q: %q %q %v", c.in, code, red, err)
		}
	}
	if _, _, err := parsePastedCode("http://127.0.0.1:8080/oauth/spotify/callback?error=access_denied"); err == nil {
		t.Error("error param must fail")
	}
}

func TestProfileExtraction(t *testing.T) {
	hop := "https://link.deezer.com/?awf=https%3A%2F%2Fwww.deezer.com%2Fprofile%2F6850245583%3Fhost%3D6850245583&dest=x"
	found := ""
	for _, v := range decodedForms(hop) {
		if m := profileRe[0].FindStringSubmatch(v); m != nil {
			found = m[1]
		}
	}
	if found != "6850245583" {
		t.Fatalf("got %q", found)
	}
	if !isDeezerHost("link.deezer.com") || isDeezerHost("deezer.com.evil.org") || isDeezerHost("evildeezer.com") {
		t.Fatal("host check")
	}
}

func TestDeezerPostNeedsInput(t *testing.T) {
	h := newTestServer(t).routes()
	w := do(h, "POST", "/m/deezer?t=abcdefghijklmnop1234", url.Values{"user": {""}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "请填写") {
		t.Fatalf("%d", w.Code)
	}
}

func TestReviewFixes(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	// Only password-bearing deck calls make a session trusted (global sync).
	do(h, "POST", "/api/relay/session", `{"token":"trustedtoken00000001","provider":"deezer"}`, map[string]string{"X-Deemix-Password": defaultPassword})
	if x, _ := s.sessions.open("trustedtoken00000001", "deezer", ""); !x.Trusted {
		t.Fatal("registered with password must be trusted")
	}
	do(h, "POST", "/api/relay/session", `{"token":"anontoken00000000001","provider":"deezer"}`, nil)
	if x, _ := s.sessions.open("anontoken00000000001", "deezer", ""); x.Trusted {
		t.Fatal("anonymous session must not be trusted")
	}
	if x, _ := s.sessions.open("phoneonly00000000001", "deezer", ""); x.Trusted {
		t.Fatal("phone-created session must not be trusted")
	}
	// Expired QR codes stay dead.
	s.sessions.update("anontoken00000000001", func(x *session) { x.Expires = time.Now().Add(-time.Second) })
	if w := do(h, "GET", "/m/deezer?t=anontoken00000000001", "", nil); w.Code != 400 {
		t.Fatalf("expired token revived: %d", w.Code)
	}
	// The console ignores ?key= (no limiter bypass).
	if w := do(h, "GET", "/?key="+defaultPassword, "", nil); strings.Contains(w.Body.String(), "控制台") {
		t.Fatal("panel must require the cookie")
	}
	// Anonymous session creation is rate limited per client.
	code := 0
	for i := 0; i < 40; i++ {
		code = do(h, "GET", "/api/relay/session?t=floodtoken"+strings.Repeat("0", 6)+string(rune('a'+i%26))+string(rune('a'+i/26)), "", nil).Code
	}
	if code != 429 {
		t.Fatalf("expected 429 after flood, got %d", code)
	}
}

func TestBPMKeysOnlyLeaveWithSecrets(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	hdr := map[string]string{"X-Deemix-Password": defaultPassword, "Content-Type": "application/json"}
	if w := do(h, "POST", "/api/config", `{"soundcloudClientId":"cid-value","getSongBpmKey":"gsb-value"}`, hdr); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	plain := do(h, "GET", "/api/config", "", hdr).Body.String()
	if strings.Contains(plain, "cid-value") || strings.Contains(plain, "gsb-value") || !strings.Contains(plain, `"getSongBpmKeySet":true`) {
		t.Fatalf("plain view: %s", plain)
	}
	secret := do(h, "GET", "/api/config?secrets=1", "", hdr).Body.String()
	if !strings.Contains(secret, "cid-value") || !strings.Contains(secret, "gsb-value") {
		t.Fatalf("secrets view: %s", secret)
	}
	if w := do(h, "GET", "/api/config?secrets=1", "", map[string]string{"X-Deemix-Password": "wrong"}); w.Code == 200 {
		t.Fatal("secrets served without the password")
	}
}

func TestSoundCloudPageNeedsACredentialAndPassesItOn(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()
	tok := "soundcloudtoken00001"
	if w := do(h, "GET", "/m/soundcloud?t="+tok, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "连接 SoundCloud") {
		t.Fatalf("without an app there is nothing to redirect to: %d %s", w.Code, w.Body)
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if w := do(h, "POST", "/m/soundcloud?t="+tok, "user=cave", form); w.Code == 200 {
		t.Fatalf("accepted without a credential: %s", w.Body)
	}
	if w := do(h, "POST", "/m/soundcloud?t="+tok, "user=cave&clientId=cid&clientSecret=shh", form); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	poll := do(h, "GET", "/api/relay/session?t="+tok, "", nil).Body.String()
	if !strings.Contains(poll, `"status":"done"`) || !strings.Contains(poll, `"clientId":"cid"`) || !strings.Contains(poll, `"user":"cave"`) {
		t.Fatal(poll)
	}
}

func TestSoundCloudAccountLoginByPastedAddress(t *testing.T) {
	var verifier, redirect string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			id, secret, ok := r.BasicAuth()
			if !ok || id != "cid" || secret != "shh" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "abc" {
				t.Errorf("token request %v", r.Form)
			}
			verifier, redirect = r.Form.Get("code_verifier"), r.Form.Get("redirect_uri")
			w.Write([]byte(`{"access_token":"u1","refresh_token":"r1","expires_in":3600}`))
		case "/me":
			if r.Header.Get("Authorization") != "OAuth u1" {
				t.Error("no token on /me")
			}
			w.Write([]byte(`{"id":8,"username":"me"}`))
		}
	}))
	defer api.Close()
	soundCloudTokenURL, soundCloudAPIURL = api.URL+"/token", api.URL
	s := newTestServer(t)
	s.cfg.update(func(c *config) { c.SoundCloudClientID, c.SoundCloudSecret = "cid", "shh" })
	h := s.routes()
	tok := "soundcloudlogin00001"
	direct := do(h, "GET", "/m/soundcloud?t="+tok, "", nil)
	if direct.Code != http.StatusFound || !strings.HasPrefix(direct.Header().Get("Location"), soundCloudAuthorizeURL+"?") || !strings.Contains(direct.Header().Get("Location"), "oauth%2Fsoundcloud%2Fcallback") {
		t.Fatalf("%d %s", direct.Code, direct.Header().Get("Location"))
	}
	page := do(h, "GET", "/m/soundcloud?manual=1&t="+tok, "", nil).Body.String()
	if !strings.Contains(page, "code_challenge_method=S256") || !strings.Contains(page, "client_id=cid") || strings.Contains(page, "shh") || !strings.Contains(page, "oauth%2Fsoundcloud%2Fcallback") {
		t.Fatal(page)
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	foreign := url.QueryEscape("http://127.0.0.1:8765/callback?code=abc&state=someoneelse00000001")
	if w := do(h, "POST", "/m/soundcloud?t="+tok, "callback="+foreign, form); w.Code == 200 {
		t.Fatal("address from another login accepted")
	}
	pasted := url.QueryEscape("http://127.0.0.1:8765/callback?code=abc&state=" + tok)
	if w := do(h, "POST", "/m/soundcloud?t="+tok, "callback="+pasted, form); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if verifier == "" || redirect != soundCloudLoopback {
		t.Fatalf("verifier %q redirect %q", verifier, redirect)
	}
	if w := do(h, "GET", "/m/soundcloud?t="+tok, "", nil); w.Code != 200 {
		t.Fatalf("a finished QR code should say so, not log in again: %d", w.Code)
	}
	poll := do(h, "GET", "/api/relay/session?t="+tok, "", nil).Body.String()
	if !strings.Contains(poll, `"userToken":"u1"`) || !strings.Contains(poll, `"refreshToken":"r1"`) || !strings.Contains(poll, `"userId":8`) {
		t.Fatal(poll)
	}
}

func TestNoFixedDefaultPassword(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "initial-password"))
	if info, _ := os.Stat(filepath.Join(dir, "initial-password")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(err)
	}
	if cfg.checkPassword(legacyDefaultPassword) || !cfg.checkPassword(strings.TrimSpace(string(b))) || cfg.get().PasswordIsDefault {
		t.Fatal("first start must use the random initial password")
	}
	// A legacy config still on the old default may log in but not read secrets or use Stems.
	s := newTestServer(t)
	if err := s.cfg.setPassword(legacyDefaultPassword); err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	hdr := map[string]string{"X-Deemix-Password": legacyDefaultPassword}
	if w := do(h, "GET", "/api/config", "", hdr); w.Code != 200 {
		t.Fatalf("legacy login: %d", w.Code)
	}
	if w := do(h, "GET", "/api/config?secrets=1", "", hdr); w.Code != 403 {
		t.Fatalf("legacy secrets: %d", w.Code)
	}
	if w := do(h, "GET", "/api/config", "", nil); w.Code != 401 {
		t.Fatal("no password")
	}
	if w := do(h, "GET", "/api/config?key="+legacyDefaultPassword, "", nil); w.Code != 401 {
		t.Fatalf("password from the query string accepted: %d", w.Code)
	}
}

func TestLimiterKeysIPv6ByPrefixAndNeverWipes(t *testing.T) {
	l := newLimiter(time.Minute, 2)
	l.fail("2001:db8:1:2::1")
	l.fail("2001:db8:1:2::ffff")
	if !l.blocked("2001:db8:1:2:abcd::9") {
		t.Fatal("same /64 must share one key")
	}
	for i := 0; i < maxLimiterKeys+5; i++ {
		l.fail("10." + strconv.Itoa(i/65536) + "." + strconv.Itoa(i/256%256) + "." + strconv.Itoa(i%256))
	}
	if len(l.m) > maxLimiterKeys {
		t.Fatalf("%d keys", len(l.m))
	}
	l.fail("2001:db8:1:2::1")
	if !l.blocked("2001:db8:1:2::1") {
		t.Fatal("a flood released a blocked client")
	}
}

func TestAnonymousPollDoesNotCreateSessions(t *testing.T) {
	s := newTestServer(t)
	if pr := s.sessions.poll("unknowntoken00000001", "", false); pr.Status != "expired" || s.sessions.exists("unknowntoken00000001") {
		t.Fatalf("%+v", pr)
	}
	if pr := s.sessions.poll("unknowntoken00000002", "", true); pr.Status != "waiting" {
		t.Fatalf("deck poll after restart: %+v", pr)
	}
}

func TestSecondCompletionIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	s.sessions.register("completetoken0000001", "soundcloud", "", true)
	synced := 0
	sync := func(bool) error { synced++; return nil }
	if err := s.sessions.complete("completetoken0000001", "soundcloud", sync, func(x *session) { x.SoundCloud = &soundCloudInfo{User: "a"} }); err != nil {
		t.Fatal(err)
	}
	err := s.sessions.complete("completetoken0000001", "soundcloud", sync, func(x *session) { x.SoundCloud = &soundCloudInfo{User: "b"} })
	if doneOK(err) != nil || synced != 1 {
		t.Fatalf("%v synced=%d", err, synced)
	}
	if x, _ := s.sessions.get("completetoken0000001"); x.SoundCloud.User != "a" {
		t.Fatal("a done login was replaced")
	}
	if err := s.sessions.complete("completetoken0000001", "deezer", sync, func(*session) {}); err != errSessionGone {
		t.Fatalf("wrong provider: %v", err)
	}
}

func TestStaleDefaultPasswordFlagIsCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, c := range []struct {
		pw   string
		want bool
	}{{"someone-changed-it", false}, {legacyDefaultPassword, true}} {
		h, _ := bcrypt.GenerateFromPassword([]byte(c.pw), bcrypt.MinCost)
		b, _ := json.Marshal(map[string]any{"passwordHash": string(h), "passwordIsDefault": true})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.get().PasswordIsDefault; got != c.want {
			t.Fatalf("password %q: PasswordIsDefault = %v, want %v", c.pw, got, c.want)
		}
	}
}

// The relay swaps the password for a token, and only the token reaches the deck.
func TestBeatportPageLogsInAndPassesOnlyTheToken(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/o/token/":
			r.ParseForm()
			if r.Form.Get("grant_type") != "password" || r.Form.Get("username") != "dj" || r.Form.Get("password") != "secret-pw" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"access_token":"bp-access","refresh_token":"bp-refresh","expires_in":36000}`)
		case "/my/playlists/", "/catalog/charts/":
			if r.Header.Get("Authorization") != "Bearer bp-access" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"results":[]}`)
		default:
			w.WriteHeader(404) // no website session: the password grant is the fallback
		}
	}))
	defer api.Close()
	s := newTestServer(t)
	s.beatportURL = api.URL
	h := s.routes()
	tok := "beatporttoken000001"
	if w := do(h, "GET", "/m/beatport?t="+tok, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "连接 Beatport") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if w := do(h, "POST", "/m/beatport?t="+tok, "user=dj&password=wrong", form); w.Code == 200 || !strings.Contains(w.Body.String(), "Beatport 登录失败") {
		t.Fatalf("wrong password accepted: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/m/beatport?t="+tok, "user=dj&password=secret-pw", form); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	poll := do(h, "GET", "/api/relay/session?t="+tok, "", nil).Body.String()
	if !strings.Contains(poll, `"status":"done"`) || !strings.Contains(poll, `"access_token":"bp-access"`) || strings.Contains(poll, "secret-pw") {
		t.Fatal(poll)
	}
}

func TestMultiDeckHubRegistrationAndAccessCodeLogin(t *testing.T) {
	stemsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true,"protocol":"engine-5.0.4","separator":"spectral-hpss-approximation","backend":"hpss","device":"Claw-ai","sessions":1,"busy":false}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer stemsSrv.Close()

	s := newTestServer(t)
	target, err := parseStemsTarget(stemsSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.stems = newStemsProxy(target, "")
	h := s.routes()

	// Deck 1 registers via GET /api/config headers.
	w := do(h, "GET", "/api/config?secrets=1", "", map[string]string{
		"X-Deemix-Password":       defaultPassword,
		"X-EngineX-Deck-IP":       "192.168.1.224",
		"X-EngineX-Deck-Role":     "13",
		"X-EngineX-Deck-Version":  "5.3.0.4",
		"X-EngineX-Deck-Code":     "ABCD-1234",
		"X-EngineX-Deck-LoggedIn": "deezer,soundcloud",
	})
	if w.Code != 200 {
		t.Fatalf("deck 1 config: %d %s", w.Code, w.Body)
	}

	// Deck 2 registers via POST /api/relay/deck.
	w = do(h, "POST", "/api/relay/deck", `{"lanIp":"192.168.1.238","role":"24","version":"5.3.0.4","accessCode":"K7M9-P4X2","loggedIn":["deezer","spotify"]}`, map[string]string{
		"X-Deemix-Password": defaultPassword,
		"Content-Type":      "application/json",
	})
	if w.Code != 200 {
		t.Fatalf("deck 2 post: %d %s", w.Code, w.Body)
	}

	// Verify GET /api/config returns both decks and stems status without leaking accessCode.
	w = do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": defaultPassword})
	if w.Code != 200 {
		t.Fatalf("config get: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, "ABCD-1234") || strings.Contains(body, "K7M9-P4X2") {
		t.Fatalf("accessCode leaked in /api/config: %s", body)
	}
	var resp struct {
		Decks []deckView  `json:"decks"`
		Stems stemsStatus `json:"stems"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Decks) != 2 || !resp.Decks[0].Online || resp.Decks[0].LANIP != "192.168.1.224" || resp.Decks[0].Role != "13" || resp.Decks[1].LANIP != "192.168.1.238" || resp.Decks[1].Role != "24" {
		t.Fatalf("unexpected decks: %+v", resp.Decks)
	}
	if !resp.Stems.Online || resp.Stems.Device != "Claw-ai" || resp.Stems.Backend != "hpss" || resp.Stems.Busy {
		t.Fatalf("unexpected stems status: %+v", resp.Stems)
	}

	// Reload deckStore from disk to verify persistence in decks.json.
	reloaded := newDeckStore(s.decks.path)
	if !reloaded.checkAccessCode("ABCD-1234") || !reloaded.checkAccessCode("abcd1234") || !reloaded.checkAccessCode("k7m9-p4x2") {
		t.Fatal("reloaded deckStore did not match saved access codes")
	}

	// Login at /login using Deck 1's AccessCode (exact and case/hyphen-insensitive).
	for _, code := range []string{"ABCD-1234", "abcd1234", "k7m9-p4x2"} {
		lw := do(h, "POST", "/login", "password="+url.QueryEscape(code), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if lw.Code != 303 || len(lw.Result().Cookies()) == 0 {
			t.Fatalf("login with deck access code %q failed: %d", code, lw.Code)
		}
		cookie := lw.Result().Cookies()[0]
		pw := do(h, "GET", "/", "", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
		panel := pw.Body.String()
		if pw.Code != 200 || !strings.Contains(panel, "已连接碟机集群 (Multi-Deck Hub)") || !strings.Contains(panel, "http://192.168.1.224:8080") || !strings.Contains(panel, "Claw-ai") {
			t.Fatalf("panel missing multi-deck hub or stems info: %d %s", pw.Code, panel)
		}
	}
}

func TestRelayQuickTunnelAndDynamicPublicURL(t *testing.T) {
	stateDir := t.TempDir()
	fakeScript := `#!/bin/sh
echo "2026-11-08T00:00:00Z INF Requesting new quick Tunnel on trycloudflare.com..."
echo "2026-11-08T00:00:00Z INF |  https://enginex-relay-4321.trycloudflare.com  |"
exec sleep 30
`
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudflared-linux-arm64" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fakeScript))
			return
		}
		http.NotFound(w, r)
	}))
	defer dlSrv.Close()

	oldBase := relayCloudflaredDownloadBaseURL
	oldArch := relayCloudflaredGOARCH
	oldPaths := relayCloudflaredSearchPaths
	oldLook := relayCloudflaredLookPath

	relayCloudflaredDownloadBaseURL = dlSrv.URL
	relayCloudflaredGOARCH = "arm64"
	relayCloudflaredSearchPaths = nil
	relayCloudflaredLookPath = func(string) (string, error) { return "", os.ErrNotExist }

	defer func() {
		relayCloudflaredDownloadBaseURL = oldBase
		relayCloudflaredGOARCH = oldArch
		relayCloudflaredSearchPaths = oldPaths
		relayCloudflaredLookPath = oldLook
	}()

	s := newTestServer(t)
	s.setPublicURL("")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	gotURL, err := startQuickTunnel(ctx, "0.0.0.0:6595", stateDir, s.setPublicURL)
	if err != nil {
		t.Fatalf("startQuickTunnel error: %v", err)
	}
	wantURL := "https://enginex-relay-4321.trycloudflare.com"
	if gotURL != wantURL {
		t.Fatalf("gotURL = %q, want %q", gotURL, wantURL)
	}
	if s.publicURL() != wantURL {
		t.Fatalf("s.publicURL() = %q, want %q", s.publicURL(), wantURL)
	}
	if s.spotifyHTTPSRedirect() != wantURL+"/oauth/spotify/callback" {
		t.Fatalf("spotifyHTTPSRedirect() = %q", s.spotifyHTTPSRedirect())
	}

	h := s.routes()
	w := do(h, "GET", "/api/config", "", map[string]string{"X-Deemix-Password": defaultPassword})
	if w.Code != 200 || !strings.Contains(w.Body.String(), wantURL) {
		t.Fatalf("GET /api/config missing tunnel URL: %d %s", w.Code, w.Body)
	}
}

func TestSpotifyBuiltinRelayFallbackRequestsStreaming(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	clientID := cfg.get().SpotifyClientID
	if clientID != "65b708073fc0480ea92a077233ca87bd" {
		t.Fatal("relay has no desktop fallback")
	}
	a := newSpotifyAuth(filepath.Join(t.TempDir(), "learned.json"), "https://relay.example.invalid/oauth/spotify/callback")
	u, err := url.Parse(a.authorizeURL(clientID, spotifyLoopbackURI, "state", "verifier"))
	if err != nil {
		t.Fatal(err)
	}
	scope := " " + u.Query().Get("scope") + " "
	if !strings.Contains(scope, " streaming ") || !strings.Contains(scope, " user-read-private ") || u.Query().Get("redirect_uri") != spotifyDesktopURI || a.useHTTPS(context.Background(), clientID) {
		t.Fatal("desktop fallback used wrong scopes or redirect")
	}
	if spotifyClientRedirect(testSpotifyClientID, spotifyLoopbackURI) != spotifyLoopbackURI {
		t.Fatal("custom app redirect changed")
	}
}

func TestPasswordChangeRequiresRelayPassword(t *testing.T) {
	s := newTestServer(t)
	h := s.routes()

	if err := s.cfg.setPassword("relay-owner-pass-1234"); err != nil {
		t.Fatal(err)
	}
	s.decks.upsert(deckInfo{ID: "192.168.1.224", LANIP: "192.168.1.224", AccessCode: "ABCD-1234"})

	lw := do(h, "POST", "/login", "password=ABCD-1234", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if lw.Code != 303 || len(lw.Result().Cookies()) == 0 {
		t.Fatalf("deck code login status = %d", lw.Code)
	}
	cookie := lw.Result().Cookies()[0]

	w := do(h, "POST", "/api/config",
		"currentPassword=ABCD-1234&newPassword=changed-pass-5678",
		map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Cookie":       cookie.Name + "=" + cookie.Value,
		})
	if w.Code != 403 {
		t.Fatalf("expected 403 when currentPassword is deck code, got %d: %s", w.Code, w.Body.String())
	}
	if !s.cfg.checkPassword("relay-owner-pass-1234") {
		t.Fatal("relay password changed without current relay password")
	}

	wOK := do(h, "POST", "/api/config",
		"currentPassword=relay-owner-pass-1234&newPassword=changed-pass-5678",
		map[string]string{
			"Content-Type":      "application/x-www-form-urlencoded",
			"X-Deemix-Password": "relay-owner-pass-1234",
		})
	if wOK.Code != 200 || !s.cfg.checkPassword("changed-pass-5678") {
		t.Fatalf("valid relay password change failed: %d %s", wOK.Code, wOK.Body.String())
	}
}
