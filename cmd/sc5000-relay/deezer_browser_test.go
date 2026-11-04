package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type browserRPCRequest struct {
	ID        int            `json:"id"`
	Method    string         `json:"method"`
	SessionID string         `json:"sessionId"`
	Params    map[string]any `json:"params"`
}

type fakeDeezerChrome struct {
	mu         sync.Mutex
	calls      []browserRPCRequest
	cookies    []map[string]any
	failMethod string
	disposed   chan struct{}
}

func browserFixture(t *testing.T) (*server, *fakeDeezerChrome) {
	t.Helper()
	f := &fakeDeezerChrome{disposed: make(chan struct{}, 10)}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 390, 680)), nil); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	chrome := httptest.NewServer(mux)
	t.Cleanup(chrome.Close)
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"webSocketDebuggerUrl": "ws" + strings.TrimPrefix(chrome.URL, "http") + "/cdp"})
	})
	mux.HandleFunc("/cdp", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var req browserRPCRequest
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, req)
			cookies, fail := f.cookies, f.failMethod == req.Method
			f.mu.Unlock()
			if fail {
				conn.WriteJSON(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "do not expose CDP data"}})
				continue
			}
			result := map[string]any{}
			switch req.Method {
			case "Target.createBrowserContext":
				result["browserContextId"] = "isolated-context"
			case "Target.createTarget":
				result["targetId"] = "isolated-target"
			case "Target.attachToTarget":
				result["sessionId"] = "isolated-session"
			case "Page.captureScreenshot":
				result["data"] = base64.StdEncoding.EncodeToString(jpegData.Bytes())
			case "Network.getCookies":
				result["cookies"] = cookies
			case "Target.disposeBrowserContext":
				f.disposed <- struct{}{}
			}
			// An unrelated event must never be confused with a command reply.
			if err := conn.WriteJSON(map[string]any{"method": "Page.frameNavigated", "params": map[string]any{}}); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{"id": req.ID, "result": result}); err != nil {
				return
			}
		}
	})
	s := newTestServer(t)
	s.browser = newDeezerBrowser(chrome.URL)
	t.Cleanup(s.browser.close)
	return s, f
}

func (f *fakeDeezerChrome) snapshot() []browserRPCRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]browserRPCRequest(nil), f.calls...)
}
func (f *fakeDeezerChrome) setCookies(c ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cookies = c
}
func (f *fakeDeezerChrome) count(method string) int {
	n := 0
	for _, c := range f.snapshot() {
		if c.Method == method {
			n++
		}
	}
	return n
}

const browserTestToken = "browsertoken1234567890"
const browserOtherToken = "othertoken12345678901"

func openBrowserQR(t *testing.T, s *server, token string, trusted bool) {
	t.Helper()
	if !s.sessions.register(token, "deezer", "", trusted) {
		t.Fatal("register failed")
	}
	w := do(s.routes(), "GET", "/m/deezer?t="+token, "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "启动云端登录窗口") || !strings.Contains(w.Body.String(), "方式二") {
		t.Fatal("missing mobile login choices")
	}
}
func browserAct(s *server, token, body string) *httptest.ResponseRecorder {
	return do(s.routes(), "POST", "/m/deezer/act?t="+token, body, map[string]string{"Content-Type": "application/json"})
}

func TestDeezerBrowserIsolationAndInput(t *testing.T) {
	s, f := browserFixture(t)
	openBrowserQR(t, s, browserTestToken, false)
	openBrowserQR(t, s, browserOtherToken, false)
	frame := do(s.routes(), "GET", "/m/deezer/frame?t="+browserTestToken, "", nil)
	if frame.Code != 200 || frame.Header().Get("Content-Type") != "image/jpeg" || frame.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("frame: %d %s", frame.Code, frame.Header())
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame.Body.Bytes()))
	if err != nil || decoded.Bounds().Dx() != 390 || decoded.Bounds().Dy() != 680 {
		t.Fatal("invalid screenshot")
	}
	for _, method := range []string{"GET", "POST"} {
		path, body := "/m/deezer/frame", ""
		if method == "POST" {
			path, body = "/m/deezer/act", `{"type":"start"}`
		}
		w := do(s.routes(), method, path+"?t="+browserOtherToken, body, map[string]string{"Content-Type": "application/json"})
		if w.Code != 409 {
			t.Fatalf("another token got %d", w.Code)
		}
	}
	if f.count("Target.createBrowserContext") != 1 {
		t.Fatal("more than one context")
	}
	for _, body := range []string{`{"type":"click","x":20,"y":30}`, `{"type":"wheel","dy":420}`, `{"type":"text","text":"private password"}`, `{"type":"key","key":"Enter"}`, `{"type":"key","key":"Tab"}`, `{"type":"key","key":"Backspace"}`} {
		if w := browserAct(s, browserTestToken, body); w.Code != 200 || strings.Contains(w.Body.String(), "private password") {
			t.Fatalf("input failed %d", w.Code)
		}
	}
	calls := f.snapshot()
	var mouse []string
	var keys []string
	for _, c := range calls {
		switch c.Method {
		case "Target.createBrowserContext":
			if c.Params["disposeOnDetach"] != true {
				t.Error("missing crash cleanup")
			}
		case "Target.createTarget":
			if c.Params["browserContextId"] != "isolated-context" {
				t.Error("not isolated")
			}
		case "Emulation.setDeviceMetricsOverride":
			if c.Params["width"] != float64(390) || c.Params["height"] != float64(680) || c.Params["mobile"] != true {
				t.Error("wrong viewport")
			}
		case "Page.navigate":
			if c.Params["url"] != deezerLoginURL {
				t.Error("wrong login page")
			}
		case "Input.dispatchMouseEvent":
			mouse = append(mouse, c.Params["type"].(string))
		case "Input.dispatchKeyEvent":
			keys = append(keys, c.Params["key"].(string)+":"+c.Params["type"].(string))
		case "Input.insertText":
			if c.Params["text"] != "private password" {
				t.Error("text not forwarded")
			}
		case "Network.getCookies":
			if fmt.Sprint(c.Params["urls"]) != "[https://www.deezer.com https://deezer.com]" {
				t.Error("cookie URL scope")
			}
		}
		if strings.HasPrefix(c.Method, "Input.") || strings.HasPrefix(c.Method, "Network.") || strings.HasPrefix(c.Method, "Page.") {
			if c.SessionID != "isolated-session" {
				t.Error("wrong CDP session")
			}
		}
	}
	if strings.Join(mouse, ",") != "mousePressed,mouseReleased,mouseWheel" || strings.Join(keys, ",") != "Enter:keyDown,Enter:keyUp,Tab:keyDown,Tab:keyUp,Backspace:keyDown,Backspace:keyUp" {
		t.Fatalf("incorrect events: %v %v", mouse, keys)
	}
	if f.count("Network.getCookies") != 7 {
		t.Fatal("not checking cookies on each request")
	}
	if w := browserAct(s, browserTestToken, `{"type":"stop"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if f.count("Target.disposeBrowserContext") != 1 {
		t.Fatal("not disposed on stop")
	}
	if w := browserAct(s, browserOtherToken, `{"type":"start"}`); w.Code != 200 {
		t.Fatal("slot not released")
	}
}

type browserRoundTripper func(*http.Request) (*http.Response, error)

func (f browserRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeezerBrowserVerifiedCompletion(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, byAct := range []bool{false, true} {
			t.Run(fmt.Sprintf("trusted=%v/action=%v", trusted, byAct), func(t *testing.T) {
				s, f := browserFixture(t)
				openBrowserQR(t, s, browserTestToken, trusted)
				if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 200 {
					t.Fatal(w.Code)
				}
				arl := strings.Repeat("a", 192)
				f.setCookies(map[string]any{"name": "arl", "domain": ".deezer.com", "value": arl})
				s.deezer.hc = &http.Client{Transport: browserRoundTripper(func(r *http.Request) (*http.Response, error) {
					cookie, err := r.Cookie("arl")
					if err != nil || cookie.Value != arl || r.URL.Query().Get("method") != "deezer.getUserData" {
						t.Error("missing identity check")
					}
					if f.count("Target.disposeBrowserContext") != 0 {
						t.Error("disposed before identity validation")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":{"USER":{"USER_ID":42,"BLOG_NAME":"mobile"}}}`)), Header: http.Header{}}, nil
				})}
				var w *httptest.ResponseRecorder
				if byAct {
					w = browserAct(s, browserTestToken, `{"type":"key","key":"Enter"}`)
				} else {
					w = do(s.routes(), "GET", "/m/deezer/frame?t="+browserTestToken, "", nil)
				}
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) || strings.Contains(w.Body.String(), arl) {
					t.Fatalf("completion: %d %s", w.Code, w.Body)
				}
				if f.count("Target.disposeBrowserContext") != 1 {
					t.Fatal("context not disposed")
				}
				sess, _ := s.sessions.get(browserTestToken)
				if sess.Status != "done" || sess.Deezer.UserID != "42" || sess.Deezer.ARL != arl {
					t.Fatal("not delivered to deck")
				}
				cfg := s.cfg.get()
				if (cfg.DeezerARL == arl) != trusted || (trusted && cfg.DeezerUserID != "42") {
					t.Fatal("incorrect global config trust boundary")
				}
				restored := newSessionStore(s.sessions.path)
				if got, _ := restored.get(browserTestToken); got.Status != "done" {
					t.Fatal("login not persisted")
				}
				page := do(s.routes(), "GET", "/m/deezer?t="+browserTestToken, "", nil)
				if !strings.Contains(page.Body.String(), "碟机已自动登录 Deezer 与 Deemix") || strings.Contains(page.Body.String(), "启动云端登录窗口") {
					t.Fatal("wrong completion page")
				}
				do(s.routes(), "GET", "/m/deezer/frame?t="+browserTestToken, "", nil)
				if f.count("Target.createBrowserContext") != 1 {
					t.Fatal("completed login reopened")
				}
			})
		}
	}
}

func TestDeezerBrowserRejectsInvalidCookies(t *testing.T) {
	for _, tc := range []struct {
		name, value, domain, id string
		checks                  int
	}{
		{"bad format", "bad", ".deezer.com", "42", 0},
		{"foreign domain", strings.Repeat("a", 192), "deezer.com.evil.test", "42", 0},
		{"shortlink domain", strings.Repeat("a", 192), "deezer.page.link", "42", 0},
		{"guest", strings.Repeat("a", 192), ".deezer.com", "0", 1},
		{"empty identity", strings.Repeat("a", 192), ".deezer.com", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := browserFixture(t)
			openBrowserQR(t, s, browserTestToken, true)
			f.setCookies(map[string]any{"name": "arl", "value": tc.value, "domain": tc.domain})
			checks := 0
			s.deezer.hc = &http.Client{Transport: browserRoundTripper(func(r *http.Request) (*http.Response, error) {
				checks++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":{"USER":{"USER_ID":"` + tc.id + `"}}}`))}, nil
			})}
			w := browserAct(s, browserTestToken, `{"type":"start"}`)
			if w.Code != 200 || checks != tc.checks {
				t.Fatalf("status=%d identity checks=%d", w.Code, checks)
			}
			if sess, _ := s.sessions.get(browserTestToken); sess.Status == "done" || sess.Deezer != nil {
				t.Fatal("invalid identity completed")
			}
			if s.cfg.get().DeezerARL != "" {
				t.Fatal("invalid cookie persisted")
			}
		})
	}
}

func TestDeezerBrowserIdleAndExpiredTokens(t *testing.T) {
	s, f := browserFixture(t)
	s.browser.idle = 150 * time.Millisecond
	openBrowserQR(t, s, browserTestToken, false)
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.browser.mu.Lock()
	expires := s.browser.active.expires
	s.browser.mu.Unlock()
	if w := do(s.routes(), "GET", "/m/deezer/frame?poll=1&t="+browserTestToken, "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.browser.mu.Lock()
	after := s.browser.active.expires
	s.browser.mu.Unlock()
	if !expires.Equal(after) {
		t.Fatal("poll prolonged idle deadline")
	}
	select {
	case <-f.disposed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle context leaked")
	}
	if w := do(s.routes(), "GET", "/m/deezer/frame?poll=1&t="+browserTestToken, "", nil); w.Code != 410 {
		t.Fatal("poll recreated expired browser")
	}
	if f.count("Target.createBrowserContext") != 1 {
		t.Fatal("unexpected recreation")
	}
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	s.sessions.update(browserTestToken, func(x *session) { x.Expires = time.Now().Add(-time.Second) })
	if w := do(s.routes(), "GET", "/m/deezer/frame?t="+browserTestToken, "", nil); w.Code != 400 {
		t.Fatal("expired token accepted")
	}
	select {
	case <-f.disposed:
	case <-time.After(2 * time.Second):
		t.Fatal("expired context leaked")
	}
}

func TestDeezerBrowserSetupFailureAndShutdown(t *testing.T) {
	s, f := browserFixture(t)
	openBrowserQR(t, s, browserTestToken, false)
	f.failMethod = "Emulation.setDeviceMetricsOverride"
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 502 || strings.Contains(w.Body.String(), "do not expose") {
		t.Fatal("wrong setup error")
	}
	if f.count("Target.disposeBrowserContext") != 1 {
		t.Fatal("partial startup leaked context")
	}
	f.mu.Lock()
	f.failMethod = ""
	f.mu.Unlock()
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 200 {
		t.Fatal("failed setup retained slot")
	}
	s.browser.close()
	if f.count("Target.disposeBrowserContext") != 2 {
		t.Fatal("shutdown leaked context")
	}
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 503 {
		t.Fatal("shutdown created context")
	}
}

func TestDeezerBrowserValidationAndConcurrentStarts(t *testing.T) {
	s, f := browserFixture(t)
	for _, body := range []string{`{"type":"click","x":390}`, `{"type":"wheel","dy":99999}`, `{"type":"key","key":"Escape"}`, `{"type":"text","text":""}`, `{"type":"start"}{}`} {
		openBrowserQR(t, s, browserTestToken, false)
		if w := browserAct(s, browserTestToken, body); w.Code != 400 {
			t.Fatalf("accepted invalid input: %s", body)
		}
	}
	if w := browserAct(s, "unregisteredtoken12345", `{"type":"start"}`); w.Code != 400 {
		t.Fatal("unknown token accepted")
	}
	if w := do(s.routes(), "POST", "/m/deezer/act?t="+browserTestToken, `{"type":"start"}`, map[string]string{"Content-Type": "text/plain"}); w.Code != 415 {
		t.Fatal("simple cross-site POST accepted")
	}
	s.sessions.register(browserOtherToken, "spotify", "", false)
	if w := browserAct(s, browserOtherToken, `{"type":"start"}`); w.Code != 400 {
		t.Fatal("wrong provider accepted")
	}
	if f.count("Target.createBrowserContext") != 0 {
		t.Fatal("invalid input launched browser")
	}
	openBrowserQR(t, s, browserOtherToken, false)
	results := make(chan int, 2)
	for _, token := range []string{browserTestToken, browserOtherToken} {
		go func(tok string) { results <- browserAct(s, tok, `{"type":"start"}`).Code }(token)
	}
	a, b := <-results, <-results
	if !((a == 200 && b == 409) || (a == 409 && b == 200)) || f.count("Target.createBrowserContext") != 1 {
		t.Fatalf("concurrent starts: %d %d", a, b)
	}
}

func TestCompleteDeezerConfigFailureAndExpiry(t *testing.T) {
	s := newTestServer(t)
	s.sessions.register(browserTestToken, "deezer", "", true)
	err := s.sessions.completeDeezer(browserTestToken, "42", strings.Repeat("a", 192), func(bool) error { return fmt.Errorf("disk unavailable") })
	if err == nil {
		t.Fatal("config failure ignored")
	}
	if sess, _ := s.sessions.get(browserTestToken); sess.Status == "done" {
		t.Fatal("partial completion published")
	}
	s.sessions.update(browserTestToken, func(x *session) { x.Expires = time.Now().Add(-time.Second) })
	called := false
	err = s.sessions.completeDeezer(browserTestToken, "42", strings.Repeat("a", 192), func(bool) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("expired token changed global config")
	}
}

func TestSpotifyRegisteredDefaultHTTPSAndScopes(t *testing.T) {
	for _, cache := range []string{"", `null`, `{"` + defaultSpotifyClientID + `":false}`, `{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":true}`} {
		t.Run(cache, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "learned.json")
			if cache != "" {
				if err := os.WriteFile(path, []byte(cache), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := newSpotifyAuth(path, "https://relay.example.com/oauth/spotify/callback")
			a.hc = &http.Client{Transport: browserRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Error("known callback performed probe")
				return nil, fmt.Errorf("no network")
			})}
			if !a.useHTTPS(context.Background(), defaultSpotifyClientID) {
				t.Fatal("registered callback not enabled")
			}
			s := newTestServer(t)
			s.spotify = a
			w := do(s.routes(), "GET", "/m/spotify?t=spotifyregistered00001", "", nil)
			loc, err := url.Parse(w.Header().Get("Location"))
			if err != nil || w.Code != 302 {
				t.Fatalf("expected Spotify redirect, got %d", w.Code)
			}
			if loc.Query().Get("redirect_uri") != a.httpsRedirect || loc.Query().Get("scope") != "user-library-read playlist-read-private playlist-read-collaborative" {
				t.Fatal("incorrect callback/scopes")
			}
		})
	}
}

// Simulate a phone that stops reading a screenshot after response headers.
// Context disposal must remain possible while that HTTP write is blocked.
type blockedBrowserWriter struct {
	header  http.Header
	started chan struct{}
	proceed chan struct{}
}

func (w *blockedBrowserWriter) Header() http.Header { return w.header }
func (w *blockedBrowserWriter) WriteHeader(int)     {}
func (w *blockedBrowserWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.proceed
	return len(p), nil
}

func TestDeezerBrowserSlowPhoneDoesNotBlockDisposal(t *testing.T) {
	s, f := browserFixture(t)
	openBrowserQR(t, s, browserTestToken, false)
	if w := browserAct(s, browserTestToken, `{"type":"start"}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	writer := &blockedBrowserWriter{header: http.Header{}, started: make(chan struct{}), proceed: make(chan struct{})}
	finished := make(chan struct{})
	defer func() {
		close(writer.proceed)
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("screenshot handler did not finish")
		}
	}()
	go func() {
		s.routes().ServeHTTP(writer, httptest.NewRequest("GET", "/m/deezer/frame?t="+browserTestToken, nil))
		close(finished)
	}()
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("screenshot did not start")
	}
	result := make(chan int, 1)
	go func() { result <- browserAct(s, browserTestToken, `{"type":"stop"}`).Code }()
	select {
	case code := <-result:
		if code != 200 {
			t.Fatal(code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow phone held browser lock")
	}
	if f.count("Target.disposeBrowserContext") != 1 {
		t.Fatal("context not disposed during slow write")
	}
}

func TestDeezerBrowserStatelessQRBothPrefixes(t *testing.T) {
	for _, prefix := range []string{"", "/sc5000"} {
		t.Run(prefix, func(t *testing.T) {
			s, f := browserFixture(t)
			token := "testtoken1234567890"
			h := s.routes()
			if s.sessions.exists(token) {
				t.Fatal("expected stateless token")
			}
			if w := do(h, "GET", prefix+"/m/deezer?t="+token, "", nil); w.Code != 200 {
				t.Fatal("stateless phone page failed")
			}
			if w := do(h, "GET", prefix+"/m/deezer/frame?t="+token, "", nil); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
				t.Fatal("stateless screenshot failed")
			}
			if f.count("Target.createBrowserContext") != 1 {
				t.Fatal("no isolated browser")
			}
			if sess, _ := s.sessions.get(token); sess.Trusted {
				t.Fatal("phone fallback became trusted")
			}
		})
	}
}
