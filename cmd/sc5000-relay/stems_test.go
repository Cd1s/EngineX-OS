package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/stems"
)

const (
	testStemsToken = "internal-token-abcdefghijklmnopqrstuvwxyz"
	testGreeting   = "session=FFFFFFFFFFFFFFFF\n\n"
)

// TestStemsWorker is re-executed as the renderer worker subprocess.
func TestStemsWorker(t *testing.T) {
	if os.Args[len(os.Args)-1] != "relay-stems-worker" {
		return
	}
	os.Stdout.Write([]byte(testGreeting))
	p := make([]byte, 4)
	if _, err := io.ReadFull(os.Stdin, p); err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(bytes.ToUpper(p))
	os.Exit(0)
}

// stemsFixture is deck client -> relay -> sc5000-stemsd -> worker.
type stemsFixture struct {
	relay   *httptest.Server
	backend *stems.Backend
	srv     *server
}

func newStemsFixture(t *testing.T, worker []string) *stemsFixture {
	t.Helper()
	b := &stems.Backend{Secret: testStemsToken, Worker: worker, Slot: make(chan struct{}, 1)}
	backend := httptest.NewServer(b)
	t.Cleanup(backend.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testStemsToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	target, err := parseStemsTarget(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.stems = newStemsProxy(target, tokenFile)
	relay := httptest.NewServer(s.routes())
	t.Cleanup(relay.Close)
	return &stemsFixture{relay: relay, backend: b, srv: s}
}

func echoWorker() []string {
	return []string{os.Args[0], "-test.run=^TestStemsWorker$", "--", "relay-stems-worker"}
}

func waitSlotFree(t *testing.T, b *stems.Backend) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for len(b.Slot) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(b.Slot) != 0 {
		t.Fatal("renderer slot still held")
	}
}

// The deck's real client must get the greeting while its upload is still open,
// then the worker's answer and the ok trailer, through both URL prefixes.
func TestStemsRelayFullDuplex(t *testing.T) {
	f := newStemsFixture(t, echoWorker())
	for i, base := range []string{f.relay.URL, f.relay.URL + "/sc5000/"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		in, send := io.Pipe()
		out, receive := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := stems.Proxy(ctx, base, defaultPassword, in, receive)
			receive.CloseWithError(err)
			done <- err
		}()
		greeting := make([]byte, len(testGreeting))
		if _, err := io.ReadFull(out, greeting); err != nil {
			cancel()
			t.Fatalf("%d: greeting: %v", i, err)
		}
		if string(greeting) != testGreeting {
			t.Fatalf("%d: greeting %q", i, greeting)
		}
		// Upload stays open: the worker exiting must still end the session.
		send.Write([]byte("abcd"))
		tail, err := io.ReadAll(out)
		if err != nil || string(tail) != "ABCD" {
			t.Fatalf("%d: tail %q %v", i, tail, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%d: proxy: %v", i, err)
		}
		send.Close()
		cancel()
		waitSlotFree(t, f.backend)
	}
}

func TestStemsRelayAuthentication(t *testing.T) {
	f := newStemsFixture(t, echoWorker())
	post := func(path string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", f.relay.URL+path, strings.NewReader(""))
		req.Header.Set("X-Stems-Protocol", stemsProtocol)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	for _, c := range []struct {
		name, path string
		hdr        map[string]string
	}{
		{"none", "/v1/engine", nil},
		{"wrong bearer", "/v1/engine", map[string]string{"Authorization": "Bearer nope"}},
		// The deck's password is not the daemon token and vice versa.
		{"internal token", "/v1/engine", map[string]string{"Authorization": "Bearer " + testStemsToken}},
		// Passwords in URLs end up in proxy logs; only headers count here.
		{"query key", "/v1/engine?key=" + url.QueryEscape(defaultPassword), nil},
	} {
		if resp := post(c.path, c.hdr); resp.StatusCode != 401 {
			t.Errorf("%s: %d, want 401", c.name, resp.StatusCode)
		}
	}
	// A busy renderer is reported as such, which also proves both header
	// forms passed authentication and reached the backend.
	f.backend.Slot <- struct{}{}
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer " + defaultPassword},
		{"X-Deemix-Password": defaultPassword},
	} {
		if resp := post("/v1/engine", hdr); resp.StatusCode != 503 {
			t.Errorf("%v: %d, want 503 busy", hdr, resp.StatusCode)
		}
	}
	<-f.backend.Slot
	// Missing protocol header never takes a renderer slot.
	req, _ := http.NewRequest("POST", f.relay.URL+"/v1/engine", nil)
	req.Header.Set("X-Deemix-Password", defaultPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("no protocol: %d, want 400", resp.StatusCode)
	}
	// Repeated failures lock the client out, even with the right password.
	for i := 0; i < failMax; i++ {
		post("/v1/engine", map[string]string{"Authorization": "Bearer wrong"})
	}
	if resp := post("/v1/engine", map[string]string{"Authorization": "Bearer " + defaultPassword}); resp.StatusCode != 429 {
		t.Errorf("after failures: %d, want 429", resp.StatusCode)
	}
}

func TestStemsRelayHealth(t *testing.T) {
	f := newStemsFixture(t, echoWorker())
	get := func(hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", f.relay.URL+"/healthz/stems", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get(nil); code != 401 {
		t.Errorf("unauthenticated health: %d", code)
	}
	code, body := get(map[string]string{"X-Deemix-Password": defaultPassword})
	if code != 200 || !strings.Contains(body, `"protocol":"engine-5.0.4"`) {
		t.Errorf("health: %d %s", code, body)
	}
	// Wrong internal token: the daemon refuses and the relay says so.
	os.WriteFile(f.srv.stems.tokenFile, []byte("some-other-token-0123456789abcdef"), 0o600)
	code, body = get(map[string]string{"X-Deemix-Password": defaultPassword})
	if code != 502 || !strings.Contains(body, "401") {
		t.Errorf("rejected token health: %d %s", code, body)
	}
	if strings.Contains(body, testStemsToken) {
		t.Error("health response leaked the internal token")
	}
}

func TestStemsRelayBackendRejectsToken(t *testing.T) {
	f := newStemsFixture(t, echoWorker())
	os.WriteFile(f.srv.stems.tokenFile, []byte("some-other-token-0123456789abcdef"), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := stems.Proxy(ctx, f.relay.URL, defaultPassword, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want backend status 502, got %v", err)
	}
}

func TestStemsRelayWorkerFailurePropagates(t *testing.T) {
	f := newStemsFixture(t, []string{"/bin/sh", "-c", "printf broken; exit 1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := stems.Proxy(ctx, f.relay.URL, defaultPassword, strings.NewReader(""), &out); err == nil {
		t.Fatal("worker failure was reported as success through the relay")
	}
	waitSlotFree(t, f.backend)
}

// A deck that disappears mid-session must free the renderer, even when the
// worker would otherwise run on regardless of its input.
func TestStemsRelayDisconnectKillsWorker(t *testing.T) {
	f := newStemsFixture(t, []string{"/bin/sh", "-c", "printf ready; exec sleep 60"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, send := io.Pipe()
	defer send.Close()
	out, receive := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := stems.Proxy(ctx, f.relay.URL, defaultPassword, in, receive)
		receive.CloseWithError(err)
		done <- err
	}()
	greeting := make([]byte, 5)
	if _, err := io.ReadFull(out, greeting); err != nil || string(greeting) != "ready" {
		t.Fatalf("greeting %q %v", greeting, err)
	}
	cancel()
	out.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not cancel")
	}
	waitSlotFree(t, f.backend)
}

func TestStemsRelayBackendDown(t *testing.T) {
	s := newTestServer(t)
	target, _ := parseStemsTarget("127.0.0.1:1")
	s.stems = newStemsProxy(target, "")
	h := s.routes()
	w := do(h, "GET", "/healthz/stems", "", map[string]string{"X-Deemix-Password": defaultPassword})
	if w.Code != 502 {
		t.Errorf("health with backend down: %d %s", w.Code, w.Body)
	}
	// Full duplex needs a real connection, not a ResponseRecorder.
	relay := httptest.NewServer(h)
	defer relay.Close()
	req, _ := http.NewRequest("POST", relay.URL+"/v1/engine", strings.NewReader(""))
	req.Header.Set("X-Deemix-Password", defaultPassword)
	req.Header.Set("X-Stems-Protocol", stemsProtocol)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("engine with backend down: %d", resp.StatusCode)
	}
	// Without a backend configured at all the route still answers cleanly.
	s.stems = nil
	if w := do(h, "GET", "/healthz/stems", "", map[string]string{"X-Deemix-Password": defaultPassword}); w.Code != 503 {
		t.Errorf("unconfigured: %d", w.Code)
	}
}

// The deck's credentials stay at the relay; the daemon sees only its token,
// or none when the token file is absent.
func TestStemsRelayForwardsOnlyInternalToken(t *testing.T) {
	seen := make(chan http.Header, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()
	s := newTestServer(t)
	target, _ := parseStemsTarget(backend.URL)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("  "+testStemsToken+"\n"), 0o600)
	s.stems = newStemsProxy(target, tokenFile)
	h := s.routes()
	if w := do(h, "GET", "/healthz/stems", "", map[string]string{"X-Deemix-Password": defaultPassword}); w.Code != 200 {
		t.Fatalf("health: %d %s", w.Code, w.Body)
	}
	hdr := <-seen
	if got := hdr.Get("Authorization"); got != "Bearer "+testStemsToken {
		t.Errorf("forwarded Authorization %q", got)
	}
	if hdr.Get("X-Deemix-Password") != "" {
		t.Error("console password forwarded to the daemon")
	}
	s.stems.tokenFile = filepath.Join(dir, "missing")
	if w := do(h, "GET", "/healthz/stems", "", map[string]string{"Authorization": "Bearer " + defaultPassword}); w.Code != 200 {
		t.Fatalf("health without token: %d %s", w.Code, w.Body)
	}
	if hdr := <-seen; hdr.Get("Authorization") != "" {
		t.Errorf("missing token file still sent %q", hdr.Get("Authorization"))
	}
}

func TestParseStemsTarget(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:6596":                  "http://127.0.0.1:6596",
		"127.0.0.1:6596/v1/engine":        "http://127.0.0.1:6596",
		"http://192.168.6.104:6596/":      "http://192.168.6.104:6596",
		"https://stems.example/base/":     "https://stems.example/base",
		"http://[::1]:6596/v1/engine?x=1": "http://[::1]:6596",
	} {
		u, err := parseStemsTarget(in)
		if err != nil || u.String() != want {
			t.Errorf("%q: %v %v, want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://h:1", "http://user:pw@h:1", "http://"} {
		if _, err := parseStemsTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDefaultStemsTokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if got := defaultStemsTokenFile(); got != stemsTokenFile {
		t.Errorf("no credential: %s", got)
	}
	os.WriteFile(filepath.Join(dir, stemsCredential), []byte("x"), 0o600)
	if got := defaultStemsTokenFile(); got != filepath.Join(dir, stemsCredential) {
		t.Errorf("with credential: %s", got)
	}
}
