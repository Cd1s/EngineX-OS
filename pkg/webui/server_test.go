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
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

type fixtureProvider struct{}

func (fixtureProvider) Playlists(context.Context) ([]providers.Playlist, error) {
	return []providers.Playlist{{ID: "p1", Title: "Friend", Count: 1}}, nil
}
func (fixtureProvider) Tracks(_ context.Context, playlist string) ([]providers.Track, error) {
	if playlist == "p2" {
		return []providers.Track{{ID: "s2", Title: "Other song", Provider: "spotify"}}, nil
	}
	return []providers.Track{{ID: "s1", Title: "Song", Artist: "Artist", ISRC: "US123", DurationMS: 8000, Provider: "deezer"}}, nil
}

type fixtureCatalog struct{}

func (fixtureCatalog) ByISRC(context.Context, string) (providers.Track, error) {
	return providers.Track{ID: "42", Title: "Song", Artist: "Artist", ISRC: "US123", DurationMS: 8000, Provider: "deezer"}, nil
}
func (fixtureCatalog) Search(context.Context, string) ([]providers.Track, error) { return nil, nil }
func request(t *testing.T, c *http.Client, base, method, path string, body any, status int) []byte {
	t.Helper()
	var b io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		b = bytes.NewReader(data)
	}
	r, err := http.NewRequest(method, base+path, b)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test-console-code")
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	return data
}
func TestGuestDownloadAnalyzeImportEndToEnd(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	lib, err := analyzer.OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	file := filepath.Join(root, "guest.flac")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = testaudio.FLAC(f, testaudio.Song(8)); err != nil {
		t.Fatal(err)
	}
	queue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := "queued"
		if r.Method == "GET" {
			state = "completed"
		}
		json.NewEncoder(w).Encode(deemixclient.QueueStatus{ID: "q1", State: state, Progress: 1, Path: file})
	}))
	defer queue.Close()
	guard := resourceguard.New(resourceguard.Config{Disabled: true})
	jobs := NewJobs(Pipeline{Guard: guard, Catalog: fixtureCatalog{}, Deemix: &deemixclient.Client{BaseURL: queue.URL, DownloadDir: root, PollInterval: time.Millisecond}, Library: lib})
	defer jobs.Close()
	ui := New(Config{AccessToken: "test-console-code", SpotifyClientID: testSpotifyClientID, Factory: func(string, bool, string) (providers.Provider, error) { return fixtureProvider{}, nil }}, jobs)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	c := srv.Client()
	data := request(t, c, srv.URL, "POST", "/api/sessions", map[string]any{"provider": "spotify", "guest": true}, 200)
	var session Session
	json.Unmarshal(data, &session)
	request(t, c, srv.URL, "GET", "/api/sessions/"+session.ID+"/playlists", nil, 200)
	request(t, c, srv.URL, "GET", "/api/sessions/"+session.ID+"/tracks?playlist=p1", nil, 200)
	request(t, c, srv.URL, "GET", "/api/sessions/"+session.ID+"/tracks?playlist=p2", nil, 200)
	request(t, c, srv.URL, "POST", "/api/jobs", map[string]string{"sessionId": session.ID, "trackId": "forged-ID"}, 400)
	data = request(t, c, srv.URL, "POST", "/api/jobs", map[string]string{"sessionId": session.ID, "trackId": "s1"}, 200)
	var job Job
	json.Unmarshal(data, &job)
	deadline := time.Now().Add(5 * time.Second)
	for {
		list := jobs.List()
		if len(list) == 1 && (list[0].State == "done" || list[0].State == "error") {
			job = list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.State != "done" || job.Quality != "[FLAC]" || job.TrackID == 0 {
		t.Fatalf("job=%+v", job)
	}
	var title, comment, fileType string
	var cues []byte
	if err = db.QueryRow("SELECT p.title,t.comment,t.fileType,pd.quickCues FROM Track t JOIN PlaylistEntity pe ON pe.trackId=t.id JOIN Playlist p ON p.id=pe.listId JOIN PerformanceData pd ON pd.trackId=t.id").Scan(&title, &comment, &fileType, &cues); err != nil {
		t.Fatal(err)
	}
	if title != providers.GuestPlaylist || !strings.Contains(comment, "[FLAC]") || fileType != "flac" || len(cues) > 0 {
		t.Fatalf("incorrect import %s %s %s", title, comment, fileType)
	}
	data = request(t, c, srv.URL, "GET", "/api/jobs/"+job.ID+"/waveform", nil, 200)
	if !bytes.Contains(data, []byte(`"overview"`)) {
		t.Fatal("waveform unavailable")
	}
	request(t, c, srv.URL, "DELETE", "/api/sessions/"+session.ID, nil, 200)
	request(t, c, srv.URL, "GET", "/api/sessions/"+session.ID+"/playlists", nil, 404)
	request(t, c, srv.URL, "POST", "/api/jobs", map[string]string{"sessionId": session.ID, "trackId": "s1"}, 400)
}
func TestAuthorizationCSRFAndPKCEState(t *testing.T) {
	var exchanges int
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		r.ParseForm()
		if len(r.Form.Get("code_verifier")) < 43 {
			t.Error("missing verifier")
		}
		json.NewEncoder(w).Encode(providers.Token{AccessToken: "private-token", ExpiresIn: 3600})
	}))
	defer tokenServer.Close()
	ui := New(Config{AccessToken: "test-console-code", PublicURL: "https://device", SpotifyRedirectURL: "https://device/oauth/spotify/callback", SpotifyClientID: "public", SpotifyAccountsURL: tokenServer.URL, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("unauthenticated API")
	}
	r, _ := http.NewRequest("POST", srv.URL+"/api/sessions", strings.NewReader(`{"provider":"spotify","guest":true}`))
	r.Header.Set("Authorization", "Bearer test-console-code")
	r.Header.Set("Origin", "https://evil.example")
	resp, err = srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	data := request(t, srv.Client(), srv.URL, "POST", "/api/sessions", map[string]any{"provider": "spotify", "guest": true}, 200)
	var session Session
	json.Unmarshal(data, &session)
	data = request(t, srv.Client(), srv.URL, "POST", "/api/sessions/"+session.ID+"/login", map[string]string{}, 200)
	var auth map[string]string
	json.Unmarshal(data, &auth)
	u, _ := url.Parse(auth["url"])
	state := u.Query().Get("state")
	if state == "" {
		t.Fatal("missing state")
	}
	qr := request(t, srv.Client(), srv.URL, "GET", "/api/sessions/"+session.ID+"/qr", nil, 200)
	if !bytes.HasPrefix(qr, []byte("\x89PNG")) {
		t.Fatal("not QR PNG")
	}
	request(t, srv.Client(), srv.URL, "GET", "/oauth/spotify/callback?state=wrong&code=code", nil, 400)
	request(t, srv.Client(), srv.URL, "GET", "/oauth/spotify/callback?state="+state+"&code=code", nil, 200)
	request(t, srv.Client(), srv.URL, "GET", "/oauth/spotify/callback?state="+state+"&code=replay", nil, 400)
	if exchanges != 1 {
		t.Fatalf("exchanges=%d", exchanges)
	}
	data = request(t, srv.Client(), srv.URL, "GET", "/api/sessions", nil, 200)
	if bytes.Contains(data, []byte("private-token")) {
		t.Fatal("credential exposed")
	}
	ui.mu.Lock()
	p := ui.sessions[session.ID].p.(*providers.Spotify)
	if p.TokenPath != "" {
		t.Error("guest token persisted")
	}
	ui.mu.Unlock()
}

func TestExpiredOwnerCanExplicitlyRestartLogin(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.json")
	if err := providers.SaveJSON(tokenPath, providers.Token{AccessToken: "expired", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{AccessToken: "test-console-code", SpotifyClientID: "public", SpotifyTokenPath: tokenPath, PublicURL: "https://device"}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	data := request(t, srv.Client(), srv.URL, "POST", "/api/sessions", map[string]any{"provider": "spotify", "guest": false}, 200)
	var session Session
	json.Unmarshal(data, &session)
	request(t, srv.Client(), srv.URL, "GET", "/api/sessions/"+session.ID+"/playlists", nil, 502)
	data = request(t, srv.Client(), srv.URL, "POST", "/api/sessions/"+session.ID+"/login", nil, 200)
	if !bytes.Contains(data, []byte("code_challenge")) {
		t.Fatal("cannot restart expired login")
	}
	html := request(t, srv.Client(), srv.URL, "GET", "/", nil, 200)
	if !bytes.Contains(html, []byte(`id="relogin"`)) {
		t.Fatal("missing UI reauthentication action")
	}
}

// #46: on a club's Wi-Fi, nobody else on the LAN may restart, update or reconfigure
// the deck. Deck-internal routes answer only loopback; console APIs need the code.
func TestLANCannotDriveTheDeck(t *testing.T) {
	uiDir := t.TempDir()
	ui := New(Config{AccessToken: "test-console-code", UIDir: uiDir, StateDir: t.TempDir()}, nil)
	for _, c := range []struct {
		method, target string
		want           int
	}{
		{"POST", "/v1/sc5000/ui?restart=1", 403},
		{"POST", "/v1/sc5000/ui?waveform=rgb&lang=en", 403},
		{"GET", "/v1/sc5000/update?apply=1", 403},
		{"POST", "/v1/sc5000/login?provider=spotify", 403},
		{"POST", "/v1/sc5000/relay", 403},
		{"GET", "/v1/sc5000/stems/source", 403},
		{"GET", "/v1/tracks/1", 403},
		{"GET", "/v1/users/1/playlists", 403},
		{"GET", "/v1/search?query=x", 403},
		{"GET", "/v1/stream/1", 403},
		{"GET", "/v1/ping", 403},
		{"POST", "/v1//sc5000/ui?restart=1", 403},
		{"POST", "/api/ui", 401},
		{"GET", "/api/update?apply=1", 401},
	} {
		for _, addr := range []string{"192.168.1.23:50000", "[fe80::1]:50000"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(c.method, c.target, strings.NewReader(`{"waveform":"rgb"}`))
			r.RemoteAddr = addr
			ui.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s %s from %s: %d, want %d", c.method, c.target, addr, w.Code, c.want)
			}
		}
	}
	// "/./v1/..." is not a gateway path: the console mux only redirects it.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/./v1/sc5000/ui?restart=1", nil)
	r.RemoteAddr = "192.168.1.23:50000"
	ui.ServeHTTP(w, r)
	if w.Code < 300 || w.Code >= 500 {
		t.Errorf("dot path from LAN: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(uiDir, "settings.conf")); !os.IsNotExist(err) {
		t.Fatalf("a LAN request changed the deck settings: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:41000", "[::1]:41000"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/ping", nil)
		r.RemoteAddr = addr
		ui.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Errorf("loopback ping from %s: %d", addr, w.Code)
		}
	}
}

func TestGenerateAccessCode(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := GenerateAccessCode()
		if len(c) != 9 || c[4] != '-' {
			t.Fatalf("format: %q", c)
		}
		for i, r := range c {
			if i != 4 && !strings.ContainsRune(accessCodeAlphabet, r) {
				t.Fatalf("ambiguous or invalid character in %q", c)
			}
		}
		if seen[c] {
			t.Fatalf("repeated code %q", c)
		}
		seen[c] = true
	}
}

func TestLoadAccessTokenUpgradesLegacyCode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access-token")
	for _, old := range []string{"0123456789abcdef0123456789abcdef", providers.RandomID()} {
		if err := os.WriteFile(path, []byte(old+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, created, err := LoadAccessToken(dir); err != nil || !created || len(code) != 9 || code[4] != '-' {
			t.Fatal(old, code, created, err)
		}
	}
	code, _, _ := LoadAccessToken(dir)
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != code {
		t.Fatalf("not written back: %q", b)
	}
	if again, created, _ := LoadAccessToken(dir); again != code || created {
		t.Fatal("upgraded code is not kept", again, created)
	}
}

func TestLoginAccessCodeInputAndLockout(t *testing.T) {
	ui := New(Config{AccessToken: "K7M9-P4X2", StateDir: t.TempDir()}, nil)
	login := func(code, ip string) int {
		r := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"token": {code}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		return w.Code
	}
	for _, in := range []string{"K7M9-P4X2", "k7m9p4x2", "k7m9 p4x2"} {
		if code := login(in, "192.0.2.1"); code != http.StatusSeeOther {
			t.Fatalf("%q: %d", in, code)
		}
	}
	for i := 1; i < authMaxFails; i++ {
		if code := login("WRONG-CODE", "192.0.2.2"); code != 401 {
			t.Fatalf("fail %d: %d", i, code)
		}
	}
	if code := login("WRONG-CODE", "192.0.2.2"); code != http.StatusTooManyRequests {
		t.Fatalf("5th fail: %d", code)
	}
	if code := login("K7M9-P4X2", "192.0.2.2"); code != http.StatusTooManyRequests {
		t.Fatalf("locked IP accepted the right code: %d", code)
	}
	// Bearer guesses at the API count too; other clients are unaffected.
	api := func(code, ip string) int {
		r := httptest.NewRequest("GET", "/api/sessions", nil)
		r.Header.Set("Authorization", "Bearer "+code)
		r.RemoteAddr = ip + ":1"
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, r)
		return w.Code
	}
	if code := api("K7M9-P4X2", "192.0.2.2"); code != http.StatusTooManyRequests {
		t.Fatalf("locked API: %d", code)
	}
	for range authMaxFails {
		api("nope", "2001:db8::1")
	}
	if code := api("k7m9p4x2", "2001:db8::2"); code != http.StatusTooManyRequests {
		t.Fatalf("same /64 not locked: %d", code)
	}
	if code := login("K7M9-P4X2", "192.0.2.3"); code != http.StatusSeeOther {
		t.Fatalf("other client: %d", code)
	}
}
