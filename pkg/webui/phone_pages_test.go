package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standalone starts a deck with no relay and opens a QR login for provider.
func standalone(t *testing.T, cfg Config, provider string) (*Server, *httptest.Server, *url.URL) {
	t.Helper()
	cfg.AccessToken, cfg.UIDir = "code", t.TempDir()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	ui := New(cfg, nil)
	srv := httptest.NewServer(ui)
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/v1/sc5000/login?provider="+provider, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	target, err := url.Parse(ui.logins[provider].Target)
	if err != nil {
		t.Fatal(err)
	}
	return ui, srv, target
}

func getPage(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	return readPage(t, resp, err)
}

func postPage(t *testing.T, u string, form url.Values) string {
	t.Helper()
	resp, err := http.PostForm(u, form)
	return readPage(t, resp, err)
}

func readPage(t *testing.T, resp *http.Response, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestEveryPhoneTextHasThreeLanguages(t *testing.T) {
	for key, v := range phoneText {
		for i, s := range v {
			if strings.TrimSpace(s) == "" {
				t.Errorf("%s: %s missing", key, phoneLangs[i].id)
			}
		}
	}
}

// #79: scanning a streaming QR code (anyone in the room can) never hands out the
// console: no session cookie and no access code on the phone page.
func TestPhoneQRDoesNotGrantConsole(t *testing.T) {
	_, srv, target := standalone(t, Config{}, "deezer")
	resp, err := http.Get(srv.URL + target.Path + "?" + target.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("phone page: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "stream_sync" {
			t.Fatal("phone page set the console cookie")
		}
	}
	if strings.Contains(string(body), "<code>code</code>") || strings.Contains(string(body), `"code"`) {
		t.Fatal("phone page shows the console code")
	}
}

func TestConsoleStatusLoopbackOnly(t *testing.T) {
	ui := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	for _, tc := range []struct {
		addr    string
		status  int
		hasCode bool
	}{{"127.0.0.1:1234", 200, true}, {"192.0.2.1:1234", 403, false}} {
		r := httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
		r.RemoteAddr = tc.addr
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), `"accessCode":"code"`) != tc.hasCode || (tc.hasCode && !strings.Contains(w.Body.String(), `"consoleUrl":"http://`)) {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "/api/ui", nil)
	r.Header.Set("Authorization", "Bearer code")
	w := httptest.NewRecorder()
	ui.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "accessCode") {
		t.Fatal("console code leaked in /api/ui")
	}
}

// Phone pages are English unless the deck's language or the switch says otherwise,
// and every page renders in every language.
func TestPhonePagesDefaultToEnglishAndSwitchLanguage(t *testing.T) {
	for _, provider := range []string{"deezer", "soundcloud", "beatport", "spotify", "applemusic"} {
		_, srv, target := standalone(t, Config{SpotifyClientID: testSpotifyClientID}, provider)
		if target.Query().Get("lang") != "en" {
			t.Fatalf("%s QR code %s", provider, target)
		}
		base := srv.URL + target.Path + "?t=" + target.Query().Get("t")
		en := getPage(t, base)
		if !strings.Contains(en, "Connect ") || !strings.Contains(en, "简体中文</a>") || !strings.Contains(en, "<b>EN</b>") {
			t.Fatalf("%s english page: %s", provider, en)
		}
		if zh := getPage(t, base+"&lang=zh-Hans"); !strings.Contains(zh, "连接 ") || !strings.Contains(zh, "lang=zh-Hans") {
			t.Fatalf("%s chinese page: %s", provider, zh)
		}
		if tw := getPage(t, base+"&lang=zh-Hant"); !strings.Contains(tw, "連接 ") {
			t.Fatalf("%s traditional page: %s", provider, tw)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("lang=zh-Hant\n"), 0o644)
	ui := New(Config{AccessToken: "c", UIDir: dir, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	postPage(t, srv.URL+"/v1/sc5000/login?provider=deezer", nil)
	if !strings.Contains(ui.logins["deezer"].Target, "lang=zh-Hant") {
		t.Fatalf("QR code ignores the deck's language: %s", ui.logins["deezer"].Target)
	}
}

// A pasted Spotify address often lacks "http://" or its state, or comes with other
// copied text. Any of them still links the account.
func TestSpotifyPasteWithoutSchemeOrState(t *testing.T) {
	accounts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "CODE" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "AT", "refresh_token": "RT", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer accounts.Close()
	_, srv, target := standalone(t, Config{SpotifyAccountsURL: accounts.URL, SpotifyClientID: testSpotifyClientID}, "spotify")
	page := srv.URL + target.Path + "?" + target.RawQuery
	getPage(t, page)
	if code, state := spotifyPaste("Copied 127.0.0.1:8080/oauth/spotify/callback?code=CODE&state=S#x now"); code != "CODE" || state != "S" {
		t.Fatalf("parsed %q %q", code, state)
	}
	if body := postPage(t, page, url.Values{"callback": {"no code here"}}); !strings.Contains(body, "No code=") {
		t.Fatalf("bad paste: %s", body)
	}
	body := postPage(t, page, url.Values{"callback": {" 127.0.0.1:8080/oauth/spotify/callback?code=CODE\n"}})
	if !strings.Contains(body, "Connected") {
		t.Fatalf("paste refused: %s", body)
	}
}

// With no app and no relay, a SoundCloud username alone binds the profile.
func TestSoundCloudUsernameAloneOnAStandaloneDeck(t *testing.T) {
	const id = "0123456789abcdefABCDEF0123456789"
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, `<script src="`+api.URL+`/assets/app.js"></script>`)
		case r.URL.Path == "/assets/app.js":
			io.WriteString(w, `client_id:"`+id+`"`)
		case r.URL.Query().Get("client_id") != id:
			w.WriteHeader(401)
		case r.URL.Path == "/resolve" && r.URL.Query().Get("url") == "https://soundcloud.com/cave":
			io.WriteString(w, `{"kind":"user","id":7,"username":"cave"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	state := t.TempDir()
	ui, srv, target := standalone(t, Config{StateDir: state, SoundCloudURL: api.URL}, "soundcloud")
	page := srv.URL + target.Path + "?" + target.RawQuery
	if body := postPage(t, page, url.Values{"user": {"cave"}}); !strings.Contains(body, "Connected") {
		t.Fatalf("phone page: %s", body)
	}
	if p := ui.ownerSoundCloud(); p.UserID != 7 || p.ClientID != "" {
		t.Fatalf("owner %+v", p)
	}
	if s := getPage(t, srv.URL+"/v1/sc5000/login?provider=soundcloud"); !strings.Contains(s, `"status":"done"`) {
		t.Fatalf("deck status %s", s)
	}
}
