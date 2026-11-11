package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestSpotifyPairingGateBusyAndCooldown(t *testing.T) {
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer ui.Close()
	session, err := ui.ownerSession("spotify")
	if err != nil {
		t.Fatal(err)
	}
	sp := session.p.(*providers.Spotify)
	if err := sp.SetToken(providers.Token{AccessToken: "fixture-owner"}); err != nil {
		t.Fatal(err)
	}
	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		ui.spotifyConnectPost(w, httptest.NewRequest("POST", "/spotify-connect", strings.NewReader("action=addUser&userName=fixture&blob=invalid&clientKey=invalid")))
		return w
	}
	if w := post(); w.Code != 403 || !strings.Contains(w.Body.String(), "ERROR-SPOTIFY-ERROR") {
		t.Fatal("LAN requester could overwrite logged-in owner")
	}
	if ui.addUserRetryAt.Load() != 0 {
		t.Fatal("403 gate rejection armed pairing cooldown")
	}
	ui.mu.Lock()
	ui.logins["spotify"] = &loginFlow{Status: "waiting", expires: time.Now().Add(time.Minute), session: session.ID}
	ui.mu.Unlock()
	if w := post(); w.Code != 200 || !strings.Contains(w.Body.String(), "402") {
		t.Fatal("explicit owner login did not permit pairing validation")
	}
	if w := post(); w.Code != 403 {
		t.Fatal("failed pairing has no cooldown")
	}
	ui.addUserRetryAt.Store(0)
	ui.addUserBusy.Store(true)
	start := time.Now()
	if w := post(); w.Code != 403 || time.Since(start) > 100*time.Millisecond {
		t.Fatal("second addUser queued behind active authentication")
	}
	ui.addUserBusy.Store(false)
	ui.mu.Lock()
	ui.logins["spotify"].Status = "done"
	ui.mu.Unlock()
	if w := post(); w.Code != 403 {
		t.Fatal("completed QR flow left account pairing unlocked")
	}
}

func TestSpotifyPairingGatePreservesOwnerDeletedOn403(t *testing.T) {
	state := t.TempDir()
	if err := providers.SaveJSON(filepath.Join(state, "spotify-token.json"), providers.Token{AccessToken: "stored-owner", ClientID: providers.SpotifyBuiltinClientID}); err != nil {
		t.Fatal(err)
	}
	ui := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
	defer ui.Close()
	ui.mu.Lock()
	ui.ownerDeleted["spotify"] = true
	ui.mu.Unlock()
	for _, body := range []string{
		"action=addUser&userName=fixture&blob=invalid&clientKey=invalid",
		"action=addUser&userName=fixture&accessToken=unsolicited-token",
	} {
		w := httptest.NewRecorder()
		ui.spotifyConnectPost(w, httptest.NewRequest("POST", "/spotify-connect", strings.NewReader(body)))
		if w.Code != 403 {
			t.Fatalf("expected 403 for %q, got %d", body, w.Code)
		}
		ui.mu.Lock()
		deleted := ui.ownerDeleted["spotify"]
		sessions := len(ui.sessions)
		ui.mu.Unlock()
		if !deleted || sessions != 0 || ui.addUserRetryAt.Load() != 0 {
			t.Fatalf("rejected 403 mutated ownerDeleted=%v sessions=%d retryAt=%d", deleted, sessions, ui.addUserRetryAt.Load())
		}
	}
}

type blockedSpotifyTeardown struct {
	fixtureProvider
	entered, release chan struct{}
	once             sync.Once
}

func (p *blockedSpotifyTeardown) Close()        { p.once.Do(func() { close(p.entered) }); <-p.release }
func (p *blockedSpotifyTeardown) Logout() error { p.Close(); return nil }
func TestSpotifyTeardownNeverHoldsServerMutex(t *testing.T) {
	for _, mode := range []string{"delete", "prune", "close"} {
		t.Run(mode, func(t *testing.T) {
			ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
			fake := &blockedSpotifyTeardown{entered: make(chan struct{}), release: make(chan struct{})}
			ui.sessions["fixture"] = &Session{ID: "fixture", Provider: "spotify", Guest: mode == "prune", expires: time.Now().Add(-time.Minute), p: fake}
			var release sync.Once
			unblock := func() { release.Do(func() { close(fake.release) }) }
			defer unblock()
			defer func() { unblock(); ui.Close() }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				switch mode {
				case "delete":
					req := httptest.NewRequest("DELETE", "/api/sessions/fixture", nil)
					req.SetPathValue("id", "fixture")
					ui.deleteSession(httptest.NewRecorder(), req)
				case "prune":
					ui.prune()
				case "close":
					ui.Close()
				}
			}()
			select {
			case <-fake.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("teardown did not begin")
			}
			free := make(chan struct{})
			go func() { ui.mu.Lock(); ui.mu.Unlock(); close(free) }()
			select {
			case <-free:
			case <-time.After(200 * time.Millisecond):
				unblock()
				<-done
				t.Fatal("network teardown held the global server mutex")
			}
			unblock()
			<-done
		})
	}
}

func TestSpotifyDeviceTerminalErrorAndConcurrentLANLink(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var tokenCalls int
	hc := &http.Client{Transport: spotifyRoundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"device_code":"fixture-device-code","user_code":"ABCD","verification_uri_complete":"https://spotify.com/pair?code=ABCD","expires_in":600,"interval":5}`
		status := 200
		if r.URL.Path == "/api/token" {
			tokenCalls++
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			status = 400
			body = `{"error":"invalid_client"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ui := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), HTTP: hc}, nil)
	defer ui.Close()
	ui.loginStart(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sc5000/login?provider=spotify", nil))
	flow := ui.logins["spotify"]
	if flow == nil || flow.spotifyDevice == nil {
		t.Fatal("no device flow")
	}
	// Exercise the actual five-second endpoint interval rather than bypassing the provider's clock.
	timer := time.NewTimer(5100 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	done := make(chan struct{})
	go func() {
		defer close(done)
		ui.loginStatus(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/sc5000/login/status?provider=spotify", nil))
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("device token was not polled")
	}
	ui.mu.Lock()
	flow.Status = "linked"
	ui.mu.Unlock()
	close(release)
	<-done
	ui.mu.Lock()
	linked := flow.Status
	ui.mu.Unlock()
	if linked != "linked" && linked != "done" {
		t.Fatal("in-flight device poll overwrote concurrent LAN link")
	}
	ui.mu.Lock()
	flow.Status = "waiting"
	ui.mu.Unlock()
	w := httptest.NewRecorder()
	ui.loginStatus(w, httptest.NewRequest("GET", "/v1/sc5000/login/status?provider=spotify", nil))
	if !strings.Contains(w.Body.String(), `"status":"error"`) || !strings.Contains(w.Body.String(), `"error":"spotify:`) || tokenCalls != 1 {
		t.Fatal("terminal device error was swallowed or permanent failure retried")
	}
}
