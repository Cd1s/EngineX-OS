package webui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Permanent Round 3 regression suite incorporating all Round 2 review boundary
// cases plus cross-route/restart spent-grant suppression, fresh-grant recovery
// & revocation, and unreadable-store vs confirmed-partial-credential separation.
func TestReviewS10Boundary(t *testing.T) {
	const session = `{"status":401,"subStatus":6001}`
	const track = `{"status":401,"subStatus":4005}`
	const pair = `{"access_token":"review-new-access","refresh_token":"review-new-refresh"}`
	const revoked = `{"error":"invalid_grant"}`
	const keepMessage = "TIDAL refresh temporarily unavailable, keeping session"
	const marker = "review-diagnostic-canary"
	type scenario struct {
		name, route, method           string
		firstStatus                   int
		firstBody                     string
		replayStatus                  int
		replayBody                    string
		missing, lost, again          bool
		wantStatus, wantAuth, wantAPI int
		wantStore, keepLog            bool
	}
	var cases []scenario
	for _, route := range []string{"api", "grant"} {
		for _, status := range []int{429, 502, 503, 504} {
			cases = append(cases, scenario{name: fmt.Sprintf("%s_http_%d", route, status), route: route,
				firstStatus: status, wantStatus: 503, wantAuth: 1, wantStore: true, keepLog: true})
		}
		for _, entry := range []struct {
			name string
			code int
			body string
		}{
			{"invalid_client", 401, `{"error":"invalid_client","error_description":"review-diagnostic-canary"}`},
			{"malformed", 400, "review-diagnostic-canary"},
			{"temporary", 401, `{"error":"temporarily_unavailable"}`},
			{"unknown", 400, `{"error":"review-diagnostic-canary"}`},
		} {
			cases = append(cases, scenario{name: route + "_" + entry.name, route: route, firstStatus: entry.code, firstBody: entry.body, wantStatus: 503, wantAuth: 1, wantStore: true, keepLog: true})
		}
		for _, code := range []int{400, 401} {
			cases = append(cases, scenario{name: fmt.Sprintf("%s_revoked_%d", route, code), route: route, firstStatus: code, firstBody: revoked, wantStatus: 401, wantAuth: 1})
		}
		cases = append(cases,
			scenario{name: route + "_missing", route: route, missing: true, wantStatus: 401},
			scenario{name: route + "_postsendeof", route: route, lost: true, wantStatus: 503, wantAuth: 1, wantStore: true, keepLog: true})
		for _, lost := range []bool{false, true} {
			cases = append(cases, scenario{name: fmt.Sprintf("%s_next_request_after_loss_%t", route, lost),
				route: route, firstStatus: 502, lost: lost, again: true, wantStatus: 503, wantAuth: 1, wantStore: true, keepLog: true})
		}
	}
	// GET carries an observable JSON subStatus body so track 401/4005 passes through
	// while retaining the refreshed pair. Per RFC 9110 §9.3.2, HTTP HEAD carries no
	// response body so track 4005 is unobservable and conservatively returns 503
	// with the keeping-session log while retaining the refreshed pair.
	for _, method := range []string{"GET", "HEAD"} {
		trackStatus, trackKeepLog := 401, false
		if method == "HEAD" {
			trackStatus, trackKeepLog = 503, true
		}
		cases = append(cases,
			scenario{name: method + "_second_session", route: "api", method: method, firstStatus: 200, firstBody: pair, replayStatus: 401, replayBody: session, wantStatus: 503, wantAuth: 1, wantAPI: 2, wantStore: true, keepLog: true},
			scenario{name: method + "_second_track", route: "api", method: method, firstStatus: 200, firstBody: pair, replayStatus: 401, replayBody: track, wantStatus: trackStatus, wantAuth: 1, wantAPI: 2, wantStore: true, keepLog: trackKeepLog},
			scenario{name: method + "_ordinary_502", route: "ordinary", method: method, wantStatus: 502, wantAPI: 2, wantStore: true})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var nAuth, nAPI atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
					if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "review-refresh" {
						t.Error("unexpected fixture grant")
					}
					n := nAuth.Add(1)
					if n > 1 {
						w.WriteHeader(400)
						io.WriteString(w, revoked)
						return
					}
					if c.lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					if c.firstStatus == 0 {
						t.Error("unexpected token submission")
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(c.firstStatus)
					io.WriteString(w, c.firstBody)
					return
				}
				n := nAPI.Add(1)
				if c.route == "ordinary" {
					w.WriteHeader(502)
					return
				}
				if n > 1 && c.replayStatus != 0 {
					w.WriteHeader(c.replayStatus)
					io.WriteString(w, c.replayBody)
					return
				}
				w.WriteHeader(401)
				io.WriteString(w, session)
			}))
			defer up.Close()
			dir := t.TempDir()
			file := filepath.Join(dir, "tidal-auth.json")
			initial := `{"accessToken":"review-access","refreshToken":"review-refresh"}`
			if c.missing {
				initial = `{"accessToken":"review-access"}`
			}
			if err := os.WriteFile(file, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			g := New(Config{AccessToken: "code", StateDir: dir, UIDir: t.TempDir(), TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
			defer g.Close()
			srv := httptest.NewServer(g)
			defer srv.Close()
			uiPost(t, srv.URL, "scope=tidal")
			var logs lockedLogBuffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			call := func() int {
				method, path := c.method, "/v1/sessions"
				var body io.Reader
				if method == "" {
					method = "GET"
				}
				if c.route == "grant" {
					method, path = "POST", "/au/oauth2/token"
					v := url.Values{"grant_type": {"refresh_token"}}
					if !c.missing {
						v.Set("refresh_token", "review-refresh")
					}
					body = strings.NewReader(v.Encode())
				}
				req, err := http.NewRequest(method, srv.URL+path, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer review-access")
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				res, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				return res.StatusCode
			}
			status := call()
			wantAPI := c.wantAPI
			if c.route == "api" && wantAPI == 0 {
				wantAPI = 1
			}
			if c.again {
				firstFile, err := os.ReadFile(file)
				if status != 503 || nAuth.Load() != 1 || err != nil || string(firstFile) != initial {
					t.Errorf("first ambiguous outcome changed session: status=%d grants=%d err=%v kept=%t", status, nAuth.Load(), err, string(firstFile) == initial)
				}
				status = call()
				if c.route == "api" {
					wantAPI++
				}
			}
			data, err := os.ReadFile(file)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			stored := err == nil
			diag := logs.String()
			logged := strings.Contains(diag, keepMessage)
			if strings.Contains(diag, marker) || strings.Contains(diag, "review-access") || strings.Contains(diag, "review-refresh") {
				t.Error("fixture credential or raw diagnostic reached the log")
			}
			if stored && c.wantStore && c.firstStatus != 200 && string(data) != initial {
				t.Error("credentials changed in transient case")
			}
			if c.firstStatus == 200 && !strings.Contains(string(data), "review-new-refresh") {
				t.Error("rotated pair missing")
			}
			if status != c.wantStatus || int(nAuth.Load()) != c.wantAuth || int(nAPI.Load()) != wantAPI || stored != c.wantStore || logged != c.keepLog {
				t.Errorf("got status=%d grants=%d api=%d stored=%t keeping-log=%t; want %d/%d/%d/%t/%t",
					status, nAuth.Load(), nAPI.Load(), stored, logged,
					c.wantStatus, c.wantAuth, wantAPI, c.wantStore, c.keepLog)
			}
		})
	}
}

// TestReviewS10CrossRequestUncertainGrantLifecycle proves:
//  1. Ambiguous refresh outcomes (post-send EOF and HTTP 429/502/503/504) persist
//     per grant generation across ordinary API requests, direct /oauth2/token
//     refreshes, cross-route transitions, AND gateway restarts while preserving
//     tidal-auth.json byte-for-byte.
//  2. Recovery via explicit device_code re-pairing or external fresh-pair arrival
//     restores normal operation.
//  3. Genuine revocation (400/401 invalid_grant) of the fresh unspent grant still
//     clears credentials and returns 401.
func TestReviewS10CrossRequestUncertainGrantLifecycle(t *testing.T) {
	const keepMessage = "TIDAL refresh temporarily unavailable, keeping session"
	const initial = `{"accessToken":"gen1-access","refreshToken":"gen1-refresh"}`

	for _, tc := range []struct {
		name       string
		firstRoute string
		nextRoute  string
		status     int
		lost       bool
		recovery   string // "device_code" or "external"
		revokeCode int
	}{
		{"api_eof_then_grant_device_recover_revoke_400", "api", "grant", 0, true, "device_code", 400},
		{"grant_eof_then_api_external_recover_revoke_401", "grant", "api", 0, true, "external", 401},
		{"api_429_then_api_device_recover_revoke_401", "api", "api", 429, false, "device_code", 401},
		{"grant_503_then_api_external_recover_revoke_400", "grant", "api", 503, false, "external", 400},
		{"api_504_then_grant_device_recover_revoke_400", "api", "grant", 504, false, "device_code", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gen1Submits, gen2Submits atomic.Int32
			var revokeGen2 atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
					_ = r.ParseForm()
					if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"access_token":"gen2-access","refresh_token":"gen2-refresh"}`)
						return
					}
					switch r.Form.Get("refresh_token") {
					case "gen1-refresh":
						n := gen1Submits.Add(1)
						if n > 1 {
							w.WriteHeader(400)
							io.WriteString(w, `{"error":"invalid_grant"}`)
							return
						}
						if tc.lost {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						}
						w.WriteHeader(tc.status)
						io.WriteString(w, "ambiguous upstream reply")
						return
					case "gen2-refresh":
						gen2Submits.Add(1)
						if revokeGen2.Load() {
							w.WriteHeader(tc.revokeCode)
							io.WriteString(w, `{"error":"invalid_grant"}`)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"access_token":"gen3-access","refresh_token":"gen3-refresh"}`)
						return
					default:
						t.Errorf("unexpected refresh_token %q", r.Form.Get("refresh_token"))
						w.WriteHeader(500)
						return
					}
				}
				if r.Header.Get("Authorization") == "Bearer gen2-access" && !revokeGen2.Load() {
					io.WriteString(w, `{"userId":42}`)
					return
				}
				if r.Header.Get("Authorization") == "Bearer gen3-access" {
					io.WriteString(w, `{"userId":42}`)
					return
				}
				w.WriteHeader(401)
				io.WriteString(w, `{"status":401,"subStatus":6001}`)
			}))
			defer up.Close()

			dir := t.TempDir()
			uiDir := t.TempDir()
			file := filepath.Join(dir, "tidal-auth.json")
			if err := os.WriteFile(file, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}

			newGatewayServer := func() (*Server, *httptest.Server) {
				s := New(Config{AccessToken: "code", StateDir: dir, UIDir: uiDir, TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
				srv := httptest.NewServer(s)
				uiPost(t, srv.URL, "scope=tidal")
				return s, srv
			}

			send := func(srv *httptest.Server, route, bearer, refresh string) int {
				var req *http.Request
				if route == "grant" {
					v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
					req, _ = http.NewRequest(http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader(v.Encode()))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				} else {
					req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
				}
				req.Header.Set("Authorization", "Bearer "+bearer)
				res, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				return res.StatusCode
			}

			var logs lockedLogBuffer
			prev := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(prev)

			s1, srv1 := newGatewayServer()
			// 1. First request encounters ambiguous refresh on gen1.
			if code := send(srv1, tc.firstRoute, "gen1-access", "gen1-refresh"); code != 503 {
				t.Fatalf("first ambiguous status = %d, want 503", code)
			}
			// 2. Follow-up request (cross-route or same-route) suppresses resubmission.
			if code := send(srv1, tc.nextRoute, "gen1-access", "gen1-refresh"); code != 503 {
				t.Fatalf("second request status = %d, want 503", code)
			}
			srv1.Close()
			s1.Close()

			// 3. Restarted gateway on the same StateDir still suppresses resubmission of gen1.
			s2, srv2 := newGatewayServer()
			defer s2.Close()
			defer srv2.Close()
			if code := send(srv2, "api", "gen1-access", "gen1-refresh"); code != 503 {
				t.Fatalf("post-restart API status = %d, want 503", code)
			}
			if code := send(srv2, "grant", "gen1-access", "gen1-refresh"); code != 503 {
				t.Fatalf("post-restart grant status = %d, want 503", code)
			}
			if got := gen1Submits.Load(); got != 1 {
				t.Fatalf("gen1 upstream submissions = %d, want 1", got)
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != initial {
				t.Fatalf("tidal-auth.json changed during gen1 suppression: %q err=%v", data, err)
			}
			if !strings.Contains(logs.String(), keepMessage) {
				t.Fatalf("missing keeping-session log: %q", logs.String())
			}
			if strings.Contains(logs.String(), "gen1-refresh") || strings.Contains(logs.String(), "gen1-access") {
				t.Fatalf("credential leaked into logs: %q", logs.String())
			}

			// 4. Recovery: fresh grant pair (gen2) arrives via device_code re-pairing or external atomic replace.
			if tc.recovery == "device_code" {
				req, _ := http.NewRequest(http.MethodPost, srv2.URL+"/au/oauth2/token", strings.NewReader("grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=dev-1"))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				res, err := srv2.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					t.Fatalf("device_code re-pair status = %d, want 200", res.StatusCode)
				}
			} else {
				tmp := filepath.Join(dir, ".fresh-gen2")
				if err := os.WriteFile(tmp, []byte(`{"accessToken":"gen2-access","refreshToken":"gen2-refresh"}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(tmp, file); err != nil {
					t.Fatal(err)
				}
			}

			// Fresh gen2 serves API cleanly.
			if code := send(srv2, "api", "gen1-access", "gen1-refresh"); code != 200 {
				t.Fatalf("post-recovery API status = %d, want 200", code)
			}

			// 5. Genuine revocation of the fresh unspent gen2 clears tidal-auth.json and returns 401.
			revokeGen2.Store(true)
			revokeRoute := tc.nextRoute
			if code := send(srv2, revokeRoute, "gen2-access", "gen2-refresh"); code != 401 {
				t.Fatalf("fresh grant revocation (%s HTTP %d) status = %d, want 401", revokeRoute, tc.revokeCode, code)
			}
			if got := gen2Submits.Load(); got != 1 {
				t.Fatalf("gen2 submissions = %d, want 1", got)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("tidal-auth.json should be removed on fresh grant revocation, err=%v", err)
			}
		})
	}
}

// TestReviewS10UnreadableStoreVsConfirmedPartial ensures both internal and
// proxied token paths clear confirmed partial logins (accessToken without
// refreshToken) with 401, while preserving unreadable/corrupted stores with 503
// and the keeping-session log.
func TestReviewS10UnreadableStoreVsConfirmedPartial(t *testing.T) {
	const keepMessage = "TIDAL refresh temporarily unavailable, keeping session"
	for _, storeKind := range []string{"corrupt_json", "unreadable_dir"} {
		for _, route := range []string{"api_real_bearer", "api_synthetic_bearer", "grant"} {
			t.Run(storeKind+"_"+route, func(t *testing.T) {
				var authCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
						authCalls.Add(1)
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(401)
					io.WriteString(w, `{"status":401,"subStatus":6001}`)
				}))
				defer up.Close()

				dir := t.TempDir()
				file := filepath.Join(dir, "tidal-auth.json")
				const corruptBytes = `{"accessToken":"unreadable-canary"`
				if storeKind == "corrupt_json" {
					if err := os.WriteFile(file, []byte(corruptBytes), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(file, 0700); err != nil {
						t.Fatal(err)
					}
				}

				s := New(Config{AccessToken: "code", StateDir: dir, UIDir: t.TempDir(), TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
				defer s.Close()
				srv := httptest.NewServer(s)
				defer srv.Close()
				uiPost(t, srv.URL, "scope=tidal")

				var logs lockedLogBuffer
				prev := log.Writer()
				log.SetOutput(&logs)
				defer log.SetOutput(prev)

				var req *http.Request
				switch route {
				case "grant":
					req, _ = http.NewRequest(http.MethodPost, srv.URL+"/au/oauth2/token", strings.NewReader("grant_type=refresh_token"))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.Header.Set("Authorization", "Bearer review-access")
				case "api_synthetic_bearer":
					req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
					req.Header.Set("Authorization", "Bearer enginex-token")
				default:
					req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
					req.Header.Set("Authorization", "Bearer review-access")
				}
				res, err := srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()

				if res.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503 for unreadable store", res.StatusCode)
				}
				if authCalls.Load() != 0 {
					t.Fatalf("authCalls = %d, want 0", authCalls.Load())
				}
				if !strings.Contains(logs.String(), keepMessage) {
					t.Fatalf("expected keeping-session log, got %q", logs.String())
				}
				if storeKind == "corrupt_json" {
					got, err := os.ReadFile(file)
					if err != nil || string(got) != corruptBytes {
						t.Fatalf("corrupt store was modified or deleted: %q err=%v", got, err)
					}
				} else {
					st, err := os.Stat(file)
					if err != nil || !st.IsDir() {
						t.Fatalf("unreadable store path was removed or altered: st=%v err=%v", st, err)
					}
				}
			})
		}
	}
}

func TestReviewS10POSTWaitsForRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var grants, posts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
			if grants.Add(1) == 1 {
				close(started)
			}
			<-release
			io.WriteString(w, `{"access_token":"review-new-access","refresh_token":"review-new-refresh"}`)
			return
		}
		posts.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, `{"status":401,"subStatus":6001}`)
	}))
	defer up.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"review-access","refreshToken":"review-refresh"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, TidalAPIURL: up.URL, TidalAuthURL: up.URL}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer unblock()
	uiPost(t, srv.URL, "scope=tidal")
	req, err := http.NewRequest("POST", srv.URL+"/v1/users/42/favorites/tracks", strings.NewReader("trackId=1"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer review-access")
	result := make(chan int, 1)
	go func() {
		res, err := srv.Client().Do(req)
		if err != nil {
			result <- 0
			return
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		result <- res.StatusCode
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	select {
	case code := <-result:
		t.Fatalf("POST returned %d while issuer was blocked", code)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case code := <-result:
		data, err := os.ReadFile(path)
		if code != 503 || posts.Load() != 1 || grants.Load() != 1 || err != nil || !strings.Contains(string(data), "review-new-refresh") {
			t.Fatal("unexpected POST result, grant count or stored pair")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("POST stayed blocked after issuer returned")
	}
}

func TestReviewS10OrdinaryAPITransport502(t *testing.T) {
	var attempts atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("fixture transport is unavailable")
	})}
	dir := t.TempDir()
	path := filepath.Join(dir, "tidal-auth.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"review-access","refreshToken":"review-refresh"}`), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{AccessToken: "code", UIDir: t.TempDir(), StateDir: dir, HTTP: client, TidalAPIURL: "http://fixture.invalid"}, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "scope=tidal")
	var logs lockedLogBuffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)
	r, err := http.NewRequest("GET", srv.URL+"/v1/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer review-access")
	res, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 502 || attempts.Load() != 2 || !strings.Contains(logs.String(), "upstream request failed") || strings.Contains(logs.String(), "keeping session") {
		t.Fatal("unexpected ordinary API transport response or log")
	}
}
