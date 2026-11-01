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

	"github.com/Cd1s/sc5000-custom-os/internal/testaudio"
	"github.com/Cd1s/sc5000-custom-os/internal/testlibrary"
	"github.com/Cd1s/sc5000-custom-os/pkg/analyzer"
	"github.com/Cd1s/sc5000-custom-os/pkg/deemixclient"
	"github.com/Cd1s/sc5000-custom-os/pkg/providers"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

type fixtureProvider struct{}

func (fixtureProvider) Playlists(context.Context) ([]providers.Playlist, error) {
	return []providers.Playlist{{ID: "p1", Title: "Friend", Count: 1}}, nil
}
func (fixtureProvider) Tracks(_ context.Context, playlist string) ([]providers.Track, error) {
	if playlist == "p2" {
		return []providers.Track{{ID: "s2", Title: "Other song", Provider: "spotify"}}, nil
	}
	return []providers.Track{{ID: "s1", Title: "Song", Artist: "Artist", ISRC: "US123", DurationMS: 8000, Provider: "spotify"}}, nil
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
	ui := New(Config{AccessToken: "test-console-code", Factory: func(string, bool, string) (providers.Provider, error) { return fixtureProvider{}, nil }}, jobs)
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
