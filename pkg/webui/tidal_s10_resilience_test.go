package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

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
// Transient refresh failures must return 503, emit the keeping-session log, and
// preserve tidal-auth.json without replaying a rotating grant on HTTP 503;
// genuine invalid_grant must still return 401 and clear tidal-auth.json.
func TestS10PrepareRealAuthTransientRefreshErrorReturns503Not401(t *testing.T) {
	t.Run("upstream_503", func(t *testing.T) {
		var authCalls atomic.Int32
		auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		}))
		defer auth.Close()

		var logs lockedLogBuffer
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(authFile, []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  auth.URL,
			TidalAuthURL: auth.URL,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
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
		if got := authCalls.Load(); got != 1 {
			t.Fatalf("auth calls = %d, want 1 (rotating grant must not retry HTTP 503)", got)
		}
		if _, err := os.Stat(authFile); err != nil {
			t.Fatalf("tidal-auth.json was removed on transient 503: %v", err)
		}
		if !strings.Contains(logs.String(), "stream-sync: TIDAL refresh temporarily unavailable, keeping session") {
			t.Fatalf("expected keeping-session log on prepareRealAuth 503, got: %q", logs.String())
		}
	})

	t.Run("dial_failure", func(t *testing.T) {
		dead := closedLocalURL(t)
		var logs lockedLogBuffer
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		if err := os.WriteFile(authFile, []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  dead,
			TidalAuthURL: dead,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
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
		if !strings.Contains(logs.String(), "stream-sync: TIDAL refresh temporarily unavailable, keeping session") {
			t.Fatalf("expected keeping-session log on dial failure 503, got: %q", logs.String())
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
		s := New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  auth.URL,
			TidalAuthURL: auth.URL,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
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
// access token) and the gateway's token refresh fails:
//  1. A pre-connect Wi-Fi drop (dial/DNS failure) never sends the rotating grant
//     upstream, keeps tidal-auth.json, returns 503, and transparently rotates and
//     succeeds with 200 once Wi-Fi recovers.
//  2. An ambiguous upstream HTTP 503 after reaching /oauth2/token makes a single
//     submission, keeps tidal-auth.json byte-for-byte, suppresses resubmission on
//     subsequent requests (503 + keeping-session log), and recovers once a fresh
//     grant pair arrives via re-pairing.
func TestS10Proxy401DuringTransientRefreshFailureReturns503Not401(t *testing.T) {
	t.Run("preconnect_wifi_drop_recovers_transparently", func(t *testing.T) {
		var wifiOnline atomic.Bool
		var authCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/oauth2/token" {
				authCalls.Add(1)
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

		httpClient := &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") && !wifiOnline.Load() {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network is unreachable")}
				}
				return http.DefaultTransport.RoundTrip(r)
			}),
		}

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		initial := `{"accessToken":"expired-access","refreshToken":"real-refresh","clientId":"cid"}`
		if err := os.WriteFile(authFile, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  up.URL,
			TidalAuthURL: up.URL,
			HTTP:         httpClient,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		// Phase 1: pre-connect Wi-Fi outage -> 0 upstream token submissions, 503, store preserved.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer expired-access")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("during outage: status = %d, want 503", resp.StatusCode)
		}
		if got := authCalls.Load(); got != 0 {
			t.Fatalf("during pre-connect outage: authCalls = %d, want 0", got)
		}
		if got, err := os.ReadFile(authFile); err != nil || string(got) != initial {
			t.Fatalf("tidal-auth.json modified during pre-connect outage: %q err=%v", got, err)
		}

		// Phase 2: Wi-Fi recovers -> unspent grant rotates cleanly and succeeds with 200.
		wifiOnline.Store(true)
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
		if got := authCalls.Load(); got != 1 {
			t.Fatalf("after recovery: authCalls = %d, want 1", got)
		}
	})

	t.Run("ambiguous_http_503_suppresses_resubmission_until_fresh_pair", func(t *testing.T) {
		var authCalls atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/oauth2/token" {
				_ = r.ParseForm()
				if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
					io.WriteString(w, `{"access_token":"repaired-access","refresh_token":"repaired-refresh"}`)
					return
				}
				if authCalls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					io.WriteString(w, `{"error":"service_unavailable"}`)
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer repaired-access" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"status":401,"subStatus":11003,"userMessage":"Token expired"}`)
				return
			}
			io.WriteString(w, `{"userId":42}`)
		}))
		defer up.Close()

		var logs lockedLogBuffer
		prev := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(prev)

		dir := t.TempDir()
		authFile := filepath.Join(dir, "tidal-auth.json")
		initial := `{"accessToken":"expired-access","refreshToken":"real-refresh","clientId":"cid"}`
		if err := os.WriteFile(authFile, []byte(initial), 0600); err != nil {
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

		// Phase 1: upstream HTTP 503 on rotating refresh -> 1 submission, 503, store preserved byte-for-byte.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req.Header.Set("Authorization", "Bearer expired-access")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || authCalls.Load() != 1 {
			t.Fatalf("phase 1: status=%d authCalls=%d, want 503/1", resp.StatusCode, authCalls.Load())
		}
		if got, err := os.ReadFile(authFile); err != nil || string(got) != initial {
			t.Fatalf("phase 1: tidal-auth.json changed: %q err=%v", got, err)
		}

		// Phase 2: follow-up ordinary API request must NOT resubmit the spent grant -> still 1 submission, 503, store preserved.
		req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req2.Header.Set("Authorization", "Bearer expired-access")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusServiceUnavailable || authCalls.Load() != 1 {
			t.Fatalf("phase 2: status=%d authCalls=%d, want 503/1", resp2.StatusCode, authCalls.Load())
		}
		if got, err := os.ReadFile(authFile); err != nil || string(got) != initial {
			t.Fatalf("phase 2: tidal-auth.json changed: %q err=%v", got, err)
		}
		if !strings.Contains(logs.String(), "TIDAL refresh temporarily unavailable, keeping session") {
			t.Fatalf("phase 2: missing keeping-session log: %q", logs.String())
		}

		// Phase 3: explicit re-pairing stores a fresh grant pair and recovers transparently to 200.
		pairReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader("grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=dev-1"))
		pairReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		pairResp, err := http.DefaultClient.Do(pairReq)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, pairResp.Body)
		pairResp.Body.Close()
		if pairResp.StatusCode != http.StatusOK {
			t.Fatalf("re-pair status=%d, want 200", pairResp.StatusCode)
		}

		req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
		req3.Header.Set("Authorization", "Bearer expired-access")
		resp3, err := http.DefaultClient.Do(req3)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp3.Body)
		resp3.Body.Close()
		if resp3.StatusCode != http.StatusOK {
			t.Fatalf("after re-pair: status = %d, want 200", resp3.StatusCode)
		}
	})
}

// S10: A post-send connection drop (EOF) during /oauth2/token must NOT retry
// with the same rotating refresh token on either the internal or proxied path
// and must return 503 to Engine while keeping tidal-auth.json.
func TestS10PostSendRefreshEOFNotRetriedAndReturns503(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"internal_refresh_eof", http.MethodGet, "/v1/sessions", ""},
		{"proxied_refresh_eof", http.MethodPost, "/au/oauth2/token", "grant_type=refresh_token&refresh_token=real-refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			s := New(Config{
				AccessToken:  "code",
				UIDir:        t.TempDir(),
				StateDir:     dir,
				TidalAPIURL:  api.URL,
				TidalAuthURL: auth.URL,
			}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			uiPost(t, srv.URL, "scope=tidal")

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, body)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		})
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
// 401 must wait for refreshRealAuth and return 503 rather than passing 401 to Engine.
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

// S10: Distinguish safe pre-connect retries on rotating refresh_token grants
// (and idempotent GET 503 retries) from non-retried HTTP 429/502/503/504
// ambiguity after a rotating grant has already been transmitted.
func TestS10SafePreConnectRefreshAndGet503RecoverViaRetry(t *testing.T) {
	t.Run("preconnect_dial_retry_recovers_internal_and_get_503_recovers", func(t *testing.T) {
		var dialAttempts, authUpstream, apiAttempts atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/oauth2/token" {
				authUpstream.Add(1)
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

		httpClient := &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") && dialAttempts.Add(1) <= 2 {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
				}
				return http.DefaultTransport.RoundTrip(r)
			}),
		}

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tidal-auth.json"), []byte(`{"refreshToken":"real-refresh","clientId":"cid"}`), 0600); err != nil {
			t.Fatal(err)
		}
		s := New(Config{
			AccessToken:  "code",
			UIDir:        t.TempDir(),
			StateDir:     dir,
			TidalAPIURL:  up.URL,
			TidalAuthURL: up.URL,
			HTTP:         httpClient,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
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
		if dialAttempts.Load() != 3 || authUpstream.Load() != 1 {
			t.Fatalf("dialAttempts=%d authUpstream=%d, want 3/1 (2 pre-connect retries + 1 actual submission)", dialAttempts.Load(), authUpstream.Load())
		}
		if apiAttempts.Load() != 2 {
			t.Fatalf("apiAttempts = %d, want 2 (1 idempotent GET 503 retry + success)", apiAttempts.Load())
		}
	})

	t.Run("preconnect_dial_retry_recovers_proxied_refresh", func(t *testing.T) {
		var dialAttempts, authUpstream atomic.Int32
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authUpstream.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"proxied-new-access","refresh_token":"proxied-new-refresh"}`)
		}))
		defer up.Close()

		httpClient := &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") && dialAttempts.Add(1) <= 2 {
					return nil, &net.DNSError{Err: "no such host", Name: "auth.tidal.com", IsTemporary: true}
				}
				return http.DefaultTransport.RoundTrip(r)
			}),
		}

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
			HTTP:         httpClient,
		}, nil)
		defer s.Close()
		srv := httptest.NewServer(s)
		defer srv.Close()
		uiPost(t, srv.URL, "scope=tidal")

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader("grant_type=refresh_token&refresh_token=old-refresh"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if dialAttempts.Load() != 3 || authUpstream.Load() != 1 {
			t.Fatalf("dialAttempts=%d authUpstream=%d, want 3/1", dialAttempts.Load(), authUpstream.Load())
		}
		saved, err := os.ReadFile(authFile)
		if err != nil || !strings.Contains(string(saved), "proxied-new-refresh") {
			t.Fatalf("rotated token not persisted: %q err=%v", saved, err)
		}
	})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		for _, path := range []string{"/v1/sessions", "/au/oauth2/token"} {
			name := fmt.Sprintf("no_retry_%d_%s", status, strings.TrimPrefix(strings.ReplaceAll(path, "/", "_"), "_"))
			t.Run(name, func(t *testing.T) {
				var authCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
						if authCalls.Add(1) == 1 {
							// One-use grant consumed upstream, but intermediary replies ambiguously.
							w.WriteHeader(status)
							io.WriteString(w, "ambiguous upstream response")
							return
						}
						// A second submission with the spent grant would trigger invalid_grant.
						w.WriteHeader(http.StatusBadRequest)
						io.WriteString(w, `{"error":"invalid_grant"}`)
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"status":401,"subStatus":6001}`)
				}))
				defer up.Close()

				dir := t.TempDir()
				authFile := filepath.Join(dir, "tidal-auth.json")
				if err := os.WriteFile(authFile, []byte(`{"accessToken":"old-access","refreshToken":"old-refresh"}`), 0600); err != nil {
					t.Fatal(err)
				}
				s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
				defer s.Close()
				srv := httptest.NewServer(s)
				defer srv.Close()
				uiPost(t, srv.URL, "scope=tidal")

				method := http.MethodGet
				var body io.Reader
				if path == "/au/oauth2/token" {
					method = http.MethodPost
					body = strings.NewReader("grant_type=refresh_token&refresh_token=old-refresh")
				}
				req, _ := http.NewRequest(method, srv.URL+path, body)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Authorization", "Bearer old-access")
				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503", resp.StatusCode)
				}
				if got := authCalls.Load(); got != 1 {
					t.Fatalf("authCalls = %d, want 1 (never retry rotating grant on HTTP %d)", got, status)
				}
				if _, err := os.Stat(authFile); err != nil {
					t.Fatalf("tidal-auth.json was deleted on HTTP %d: %v", status, err)
				}
			})
		}
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

// S10 Round 2 permanent boundary suite covering all QA boundary cases:
// rotating-grant ambiguity (internal & proxied), strict invalid_grant-only
// 401/store-clear classification on both token paths, replay session-401 -> 503
// guard vs track-4005 passthrough, confirmed revocation, and persistent ordinary
// API 502 forwarding after bounded GET retry.
func TestS10Boundary(t *testing.T) {
	const session401 = `{"status":401,"subStatus":6001}`
	const pair = `{"access_token":"test-new-access","refresh_token":"test-new-refresh"}`
	const revoked = `{"error":"invalid_grant"}`
	const temporary = `{"error":"temporarily_unavailable"}`
	const api = "/v1/sessions"
	const grant = "/au/oauth2/token"
	type reply struct {
		status int
		body   string
	}
	for _, c := range []struct {
		name, path                             string
		firstAuth, nextAuth, firstAPI, nextAPI reply
		wantStatus, wantAuth, wantAPI          int
		wantStore                              bool
		wantBodySubstr                         string
		wantStoreSubstr                        string
		wantLogSubstr                          string
	}{
		{"unconfirmed_client_error", api, reply{401, `{"error":"invalid_client"}`}, reply{}, reply{401, session401}, reply{}, 503, 1, 1, true, "", "test-refresh", "invalid_client"},
		{"malformed_400", api, reply{400, "upstream response unavailable"}, reply{}, reply{401, session401}, reply{}, 503, 1, 1, true, "", "test-refresh", "keeping session"},
		{"implicit_refresh_ambiguous_502", api, reply{502, "upstream response lost"}, reply{400, revoked}, reply{401, session401}, reply{}, 503, 1, 1, true, "", "test-refresh", "keeping session"},
		{"proxy_refresh_ambiguous_502", grant, reply{502, "upstream response lost"}, reply{400, revoked}, reply{}, reply{}, 503, 1, 0, true, "", "test-refresh", "keeping session"},
		{"proxied_oauth_401_transient", grant, reply{401, temporary}, reply{401, temporary}, reply{}, reply{}, 503, 1, 0, true, "", "test-refresh", "keeping session"},
		{"proxied_oauth_400_invalid_client", grant, reply{400, `{"error":"invalid_client"}`}, reply{}, reply{}, reply{}, 503, 1, 0, true, "", "test-refresh", "invalid_client"},
		{"proxied_oauth_400_malformed", grant, reply{400, "upstream response unavailable"}, reply{}, reply{}, reply{}, 503, 1, 0, true, "", "test-refresh", "keeping session"},
		{"fresh_pair_second_session_401", api, reply{200, pair}, reply{}, reply{401, session401}, reply{401, session401}, 503, 1, 2, true, "", "test-new-refresh", "keeping session"},
		{"fresh_pair_track_401", api, reply{200, pair}, reply{}, reply{401, session401}, reply{401, `{"status":401,"subStatus":4005}`}, 401, 1, 2, true, `"subStatus":4005`, "test-new-refresh", ""},
		{"confirmed_revocation", api, reply{400, revoked}, reply{}, reply{401, session401}, reply{}, 401, 1, 1, false, "", "", ""},
		{"normal_api_502", api, reply{}, reply{}, reply{502, "bad gateway"}, reply{502, "bad gateway"}, 502, 0, 2, true, "bad gateway", "test-refresh", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs lockedLogBuffer
			log.SetOutput(&logs)
			defer log.SetOutput(os.Stderr)

			var authN, apiN atomic.Int32
			writeReply := func(w http.ResponseWriter, r reply) {
				if r.status == 0 {
					r.status = 500
				}
				w.WriteHeader(r.status)
				_, _ = io.WriteString(w, r.body)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
					_ = r.ParseForm()
					if r.Form.Get("refresh_token") != "test-refresh" {
						t.Error("unexpected fixture grant")
					}
					if authN.Add(1) == 1 {
						writeReply(w, c.firstAuth)
					} else {
						writeReply(w, c.nextAuth)
					}
					return
				}
				if apiN.Add(1) == 1 {
					writeReply(w, c.firstAPI)
				} else {
					writeReply(w, c.nextAPI)
				}
			}))
			defer up.Close()
			dir := t.TempDir()
			path := filepath.Join(dir, "tidal-auth.json")
			if err := os.WriteFile(path, []byte(`{"accessToken":"test-access","refreshToken":"test-refresh"}`), 0600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAuthURL: up.URL, TidalAPIURL: up.URL}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			uiPost(t, srv.URL, "scope=tidal")
			method := http.MethodGet
			var body io.Reader
			if c.path == grant {
				method = http.MethodPost
				body = strings.NewReader("grant_type=refresh_token&refresh_token=test-refresh")
			}
			req, err := http.NewRequest(method, srv.URL+c.path, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer test-access")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			respBody, _ := io.ReadAll(res.Body)
			res.Body.Close()
			storedBytes, err := os.ReadFile(path)
			store := err == nil
			if res.StatusCode != c.wantStatus || int(authN.Load()) != c.wantAuth || int(apiN.Load()) != c.wantAPI || store != c.wantStore {
				t.Errorf("status/auth/api/store = %d/%d/%d/%t, want %d/%d/%d/%t", res.StatusCode, authN.Load(), apiN.Load(), store, c.wantStatus, c.wantAuth, c.wantAPI, c.wantStore)
			}
			if c.wantBodySubstr != "" && !strings.Contains(string(respBody), c.wantBodySubstr) {
				t.Errorf("body = %q, want substring %q", respBody, c.wantBodySubstr)
			}
			if c.wantStoreSubstr != "" && !strings.Contains(string(storedBytes), c.wantStoreSubstr) {
				t.Errorf("stored = %q, want substring %q", storedBytes, c.wantStoreSubstr)
			}
			logOut := logs.String()
			if c.wantLogSubstr != "" && !strings.Contains(logOut, c.wantLogSubstr) {
				t.Errorf("logs = %q, want substring %q", logOut, c.wantLogSubstr)
			}
			if strings.Contains(logOut, "test-refresh") || strings.Contains(logOut, "test-access") {
				t.Errorf("logs leaked credential material: %q", logOut)
			}
		})
	}
}

// S10: Device-code pairing/polling responses (authorization_pending, slow_down)
// must pass through unchanged with their original OAuth status and body.
func TestS10DeviceGrantPollingPreserved(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"authorization_pending"}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"slow_down"}`)
		default:
			io.WriteString(w, `{"access_token":"paired-access","refresh_token":"paired-refresh"}`)
		}
	}))
	defer up.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "tidal-auth.json")
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAuthURL: up.URL, TidalAPIURL: up.URL}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")

	poll := func(wantStatus int, wantSubstr string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader("grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=dev-123"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != wantStatus || !strings.Contains(string(b), wantSubstr) {
			t.Fatalf("got HTTP %d %s, want %d containing %q", resp.StatusCode, b, wantStatus, wantSubstr)
		}
	}

	poll(http.StatusBadRequest, `"authorization_pending"`)
	poll(http.StatusBadRequest, `"slow_down"`)
	poll(http.StatusOK, `"paired-access"`)

	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls.Load())
	}
	saved, err := os.ReadFile(authFile)
	if err != nil || !strings.Contains(string(saved), "paired-refresh") {
		t.Fatalf("paired tokens not persisted: %q err=%v", saved, err)
	}
}
