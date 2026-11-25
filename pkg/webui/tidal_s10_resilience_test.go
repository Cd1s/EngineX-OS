package webui

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// closedLocalURL reserves a localhost port and closes the listener so requests
// fail immediately with a pre-connect dial error (simulating a Wi-Fi drop).
func closedLocalURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// S10: When Engine sends a synthetic bearer and the gateway refreshes a real
// TIDAL login during a transient Wi-Fi / upstream outage, returning HTTP 401
// (subStatus 6001) drives Engine's TidalAPI::handleErrors straight into
// StreamingLogoutEvent -> StreamingInterfaceController::onLoggedOut ->
// StreamingDeckController::unloadFromProvider, ejecting all loaded TIDAL decks.
// Transient refresh failures must return 503 and preserve tidal-auth.json;
// genuine invalid_grant must still return 401 and clear tidal-auth.json.
func TestS10PrepareRealAuthTransientRefreshErrorReturns503Not401(t *testing.T) {
	t.Run("upstream_503", func(t *testing.T) {
		auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		}))
		defer auth.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(authFile, []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  auth.URL,
			TidalAuthURL: auth.URL,
		}, nil))
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer enginex-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (never 401 on transient refresh outage)", resp.StatusCode)
		}
		if _, err := os.Stat(authFile); err != nil {
			t.Fatalf("tidal-auth.json was removed on transient 503: %v", err)
		}
	})

	t.Run("dial_failure", func(t *testing.T) {
		dead := closedLocalURL(t)
		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(authFile, []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  dead,
			TidalAuthURL: dead,
		}, nil))
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer enginex-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 on dial failure", resp.StatusCode)
		}
		if _, err := os.Stat(authFile); err != nil {
			t.Fatalf("tidal-auth.json was removed on dial failure: %v", err)
		}
	})

	t.Run("invalid_grant_still_returns_401_and_clears", func(t *testing.T) {
		auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant"}`)
		}))
		defer auth.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(authFile, []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  auth.URL,
			TidalAuthURL: auth.URL,
		}, nil))
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer enginex-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 on invalid_grant", resp.StatusCode)
		}
		if _, err := os.Stat(authFile); !os.IsNotExist(err) {
			t.Fatalf("tidal-auth.json should be cleared on invalid_grant, stat err=%v", err)
		}
	})
}

// S10: When upstream TIDAL API returns a session-logout 401 (expired hourly
// access token) and the gateway's token refresh fails because Wi-Fi/MiFi drops,
// proxyToTidal must return 503 instead of leaking the raw 401 to Engine.
// Once Wi-Fi recovers, the next request refreshes and succeeds with 200.
func TestS10Proxy401DuringTransientRefreshFailureReturns503Not401(t *testing.T) {
	var authOnline atomic.Bool
	var authCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/oauth2/token" {
			authCalls.Add(1)
			if !authOnline.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":"service_unavailable"}`)
				return
			}
			io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"status":401,"subStatus":11003,"userMessage":"Token expired"}`)
			return
		}
		io.WriteString(w, `{"userId":42}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"accessToken":"expired-access","refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	// Phase 1: Wi-Fi / auth upstream is flapping -> must NOT pass 401 to Engine.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer expired-access")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("during outage: status = %d, want 503 (never leak 401 to Engine)", resp.StatusCode)
	}
	if _, err := os.Stat(authFile); err != nil {
		t.Fatalf("tidal-auth.json lost during outage: %v", err)
	}

	// Phase 2: Wi-Fi recovers -> next request rotates token and succeeds transparently.
	authOnline.Store(true)
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	req2.Header.Set("Authorization", "Bearer expired-access")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("after recovery: status = %d, want 200", resp2.StatusCode)
	}
}

// S10: A post-send connection drop (EOF) during /oauth2/token must NOT retry
// with the same rotating refresh token (which could burn the rotated pair with
// invalid_grant) and must return 503 to Engine while keeping tidal-auth.json.
func TestS10PostSendRefreshEOFNotRetriedAndReturns503(t *testing.T) {
	var authCalls atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCalls.Add(1)
		// Hijack and drop after reading the request body (simulates response lost mid-flight).
		io.Copy(io.Discard, r.Body)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer auth.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":401,"subStatus":11003,"userMessage":"Token expired"}`)
	}))
	defer api.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"accessToken":"expired-access","refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  api.URL,
		TidalAuthURL: auth.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer expired-access")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on post-send EOF", resp.StatusCode)
	}
	if got := authCalls.Load(); got != 1 {
		t.Fatalf("auth calls = %d, want 1 (must not retry post-send rotating refresh)", got)
	}
	if _, err := os.Stat(authFile); err != nil {
		t.Fatalf("tidal-auth.json was removed on EOF: %v", err)
	}
}

// S10: Track-specific 401 restrictions (subStatus outside Engine's session-logout
// set {6001, 6002, 11001, 11002, 11003, 11101}) must pass through untouched
// without triggering token refresh or rewriting to 503.
func TestS10TrackSpecific401PassesThroughWithoutRefresh(t *testing.T) {
	var authCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/oauth2/token" {
			authCalls.Add(1)
			io.WriteString(w, `{"access_token":"unexpected","refresh_token":"unexpected"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":401,"subStatus":4005,"userMessage":"Asset is not ready for playback"}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"accessToken":"valid-access","refreshToken":"real-refresh"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/tracks/123/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer valid-access")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"subStatus":4005`) {
		t.Fatalf("got HTTP %d %s, want untouched 401 subStatus 4005", resp.StatusCode, body)
	}
	if authCalls.Load() != 0 {
		t.Fatalf("track-specific 401 triggered %d unnecessary refreshes", authCalls.Load())
	}
}

// S10: After the gateway rotates T1 -> T2, Engine still carries T1 in memory.
// Outgoing requests must be upgraded to T2 so that when T2 later expires (hour 2
// of a live set), the gateway recognizes T2 as the failing token and rotates
// T2 -> T3 instead of skipping refresh and leaking a fatal 401 to Engine.
func TestS10StaleEngineBearerUpgradedAcrossMultipleRotations(t *testing.T) {
	var mu sync.Mutex
	validToken := "token-2"
	var refreshes int
	var seenBearers []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/oauth2/token" {
			mu.Lock()
			refreshes++
			validToken = "token-3"
			mu.Unlock()
			io.WriteString(w, `{"access_token":"token-3","refresh_token":"refresh-3"}`)
			return
		}
		mu.Lock()
		auth := r.Header.Get("Authorization")
		seenBearers = append(seenBearers, auth)
		want := "Bearer " + validToken
		mu.Unlock()
		if auth != want {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"status":401,"subStatus":11003,"userMessage":"Token expired"}`)
			return
		}
		io.WriteString(w, `{"userId":42}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	// Gateway already rotated to token-2; Engine still sends stale token-1.
	if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"accessToken":"token-2","refreshToken":"refresh-2","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	// Request 1: Engine sends stale token-1 -> gateway upgrades to token-2 with 0 401s.
	req1, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	req1.Header.Set("Authorization", "Bearer token-1")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("req1 status = %d, want 200", resp1.StatusCode)
	}

	// Expire token-2 on upstream (hour 2 of a live set).
	mu.Lock()
	validToken = "token-3"
	mu.Unlock()

	// Request 2: Engine STILL sends stale token-1 -> gateway must rotate token-2 -> token-3 and return 200.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	req2.Header.Set("Authorization", "Bearer token-1")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("req2 (hour-2 rotation) status = %d, want 200", resp2.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if len(seenBearers) != 3 || seenBearers[0] != "Bearer token-2" || seenBearers[1] != "Bearer token-2" || seenBearers[2] != "Bearer token-3" {
		t.Fatalf("upstream bearers = %v, want [Bearer token-2, Bearer token-2, Bearer token-3]", seenBearers)
	}
}

// S10: A non-GET/HEAD API call (e.g. POST playback/favorite) receiving a session
// 401 must refresh the token and return 503 rather than passing 401 to Engine.
func TestS10PostSession401RefreshesAndReturns503Not401(t *testing.T) {
	var refreshes atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/oauth2/token" {
			refreshes.Add(1)
			io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"status":401,"subStatus":11003,"userMessage":"Token expired"}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"accessToken":"old-access","refreshToken":"old-refresh","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/users/42/favorites/tracks", strings.NewReader("trackId=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer old-access")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST session 401 status = %d, want 503", resp.StatusCode)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
	saved, err := os.ReadFile(authFile)
	if err != nil || !strings.Contains(string(saved), "rotated-access") {
		t.Fatalf("rotated token was not persisted: %q err=%v", saved, err)
	}
}

// S10: Transient upstream 503 blips on idempotent GETs and on /oauth2/token
// refresh calls recover via bounded retry without failing or logging out.
func TestS10TransientGetAndRefresh503RecoverViaRetry(t *testing.T) {
	var apiAttempts, authAttempts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/oauth2/token" {
			if authAttempts.Add(1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":"temporarily_unavailable"}`)
				return
			}
			io.WriteString(w, `{"access_token":"recovered-access","refresh_token":"recovered-refresh"}`)
			return
		}
		if apiAttempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"status":503}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer recovered-access" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"status":401,"subStatus":11003}`)
			return
		}
		io.WriteString(w, `{"trackId":99}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil))
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/tracks/99/playbackinfopostpaywall", nil)
	req.Header.Set("Authorization", "Bearer enginex-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"trackId":99`) {
		t.Fatalf("got HTTP %d %s, want 200 trackId:99", resp.StatusCode, body)
	}
	if authAttempts.Load() != 3 {
		t.Fatalf("authAttempts = %d, want 3 (2 transient 503 retries + success)", authAttempts.Load())
	}
	if apiAttempts.Load() != 2 {
		t.Fatalf("apiAttempts = %d, want 2 (1 transient 503 retry + success)", apiAttempts.Load())
	}
}

// S10: When Engine directly calls POST /au/oauth2/token (grant_type=refresh_token)
// and gives up waiting while TIDAL is rotating the token pair over a slow MiFi link,
// proxyToTidal must still finish receiving and storing the rotated token pair.
func TestS10ProxiedTokenRefreshSurvivesCallerCancel(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"proxied-rotated-access","refresh_token":"proxied-rotated-refresh"}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"accessToken":"old-access","refreshToken":"old-refresh","clientId":"cid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		AccessToken:  "code",
		UIDir:        t.TempDir(),
		StateDir:     dir,
		TidalAPIURL:  up.URL,
		TidalAuthURL: up.URL,
	}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader("grant_type=refresh_token&refresh_token=old-refresh"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("proxied refresh never reached upstream")
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(authFile)
		if strings.Contains(string(b), "proxied-rotated-refresh") && strings.Contains(string(b), "proxied-rotated-access") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxied rotated tokens were lost on caller cancel: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
