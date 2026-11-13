package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type tsFake struct {
	mu      sync.Mutex
	cmds    []string
	active  bool
	state   string
	authURL string
	keyFile string
}

func (f *tsFake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, line)
	switch {
	case name == "systemctl" && len(args) > 0 && args[0] == "is-active":
		if f.active {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errors.New("exit 3")
	case name == "systemd-run":
		f.active = true
	case name == "systemctl" && len(args) > 0 && args[0] == "stop":
		f.active = false
	case strings.HasSuffix(name, "/tailscale") && len(args) > 1 && args[1] == "status":
		if !f.active {
			return nil, errors.New("no daemon")
		}
		return []byte(`{"Version":"1.104.5","BackendState":"` + f.state + `","AuthURL":"` + f.authURL +
			`","Self":{"HostName":"deck","TailscaleIPs":["100.64.0.7"]}}`), nil
	case strings.HasSuffix(name, "/tailscale") && len(args) > 1 && args[1] == "up":
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--auth-key=file:"); ok {
				b, _ := os.ReadFile(p)
				f.keyFile = string(b) // the key must be readable while "up" runs
				f.state = "Running"
			}
		}
	}
	return []byte("ok\n"), nil
}

func (f *tsFake) has(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func (f *tsFake) snapshot() ([]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...), f.keyFile
}

func tsPost(t *testing.T, s *Server, body string) (int, TailscaleStatusView, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleTailscale(w, httptest.NewRequest(http.MethodPost, "/api/tailscale", bytes.NewReader([]byte(body))))
	var v TailscaleStatusView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w.Body.String()
}

func tsRelease(t *testing.T, payload []byte, sum string) {
	t.Helper()
	if sum == "" {
		h := sha256.Sum256(payload)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			w.Write([]byte(sum + "  tailscaled-arm64\n"))
		case "/tailscaled-arm64":
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	old, oldArch := tailscaleReleaseURL, tailscaleGOARCH
	tailscaleReleaseURL, tailscaleGOARCH = srv.URL, "arm64"
	t.Cleanup(func() { srv.Close(); tailscaleReleaseURL, tailscaleGOARCH = old, oldArch })
}

func TestTailscaleRejectsBadInput(t *testing.T) {
	SetSystemRunner((&tsFake{}).run)
	defer SetSystemRunner(nil)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	for _, body := range []string{
		`{"loginServer":"ftp://x"}`, `{"loginServer":"https://u:p@h"}`, `{"loginServer":"https://a b"}`,
		`{"hostname":"bad name"}`, `{"hostname":"-x"}`, `{"authKey":"short"}`, `{"authKey":"abc def ghi jkl"}`,
		`{"action":"rm -rf"}`, `{"nope":1}`,
	} {
		if code, _, out := tsPost(t, s, body); code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", body, code, out)
		}
	}
	// Enabling before the binary is installed must not start anything.
	if code, _, _ := tsPost(t, s, `{"enabled":true}`); code != http.StatusConflict {
		t.Fatalf("want 409 when not installed, got %d", code)
	}
}

func TestTailscaleInstallVerifiesChecksum(t *testing.T) {
	SetSystemRunner((&tsFake{}).run)
	defer SetSystemRunner(nil)
	payload := []byte("#!/bin/sh\nexit 0\n")

	tsRelease(t, payload, strings.Repeat("0", 64))
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	if code, _, _ := tsPost(t, s, `{"action":"install"}`); code != http.StatusBadGateway || s.tailscaleInstalled() {
		t.Fatalf("a wrong checksum must be rejected and leave nothing installed (code %d)", code)
	}

	tsRelease(t, payload, "")
	code, v, out := tsPost(t, s, `{"action":"install"}`)
	if code != http.StatusOK || !v.Installed || !v.Supported {
		t.Fatalf("install failed: %d %s", code, out)
	}
	if link, err := os.Readlink(s.tailscaleCLI()); err != nil || link != "tailscaled" {
		t.Fatalf("tailscale must link to tailscaled: %q %v", link, err)
	}
}

func TestTailscaleLoginServeAndStop(t *testing.T) {
	f := &tsFake{state: "NeedsLogin", authURL: "https://hs.example/register/abc"}
	SetSystemRunner(f.run)
	defer SetSystemRunner(nil)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	bin := s.tailscaleBin()
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil || os.WriteFile(bin, []byte("x"), 0o755) != nil {
		t.Fatal("setup")
	}

	// Login URL flow: no auth key, the pending URL comes back for the browser.
	code, v, out := tsPost(t, s, `{"enabled":true,"loginServer":"https://hs.example/","hostname":"my-deck","action":"login","forward":{"ssh":true,"webui":false}}`)
	if code != http.StatusOK || !v.Running || v.State != "NeedsLogin" || v.AuthURL != "https://hs.example/register/abc" {
		t.Fatalf("login URL flow: %d %s", code, out)
	}
	if !f.has("systemd-run --unit=enginex-tailscaled") || !f.has("--tun=userspace-networking") || !f.has("--login-server=https://hs.example ") && !f.has("--login-server=https://hs.example") {
		cmds, _ := f.snapshot()
		t.Fatalf("daemon/up flags wrong: %v", cmds)
	}
	if f.has("serve --bg") {
		t.Fatal("must not forward before the node is logged in")
	}

	// Auth key flow: the key goes through a file, not argv, and is never persisted.
	code, v, out = tsPost(t, s, `{"action":"login","authKey":"tskey-auth-SECRET1234","forward":{"ssh":true,"webui":true}}`)
	if code != http.StatusOK || v.State != "Running" || len(v.IPs) != 1 {
		t.Fatalf("auth key flow: %d %s", code, out)
	}
	cmds, keyFile := f.snapshot()
	if keyFile != "tskey-auth-SECRET1234" {
		t.Fatalf("auth key file content differs from test fixture")
	}
	for _, c := range cmds {
		if strings.Contains(c, "SECRET1234") {
			t.Fatalf("auth key leaked into a command line: %s", c)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(s.tailscaleRoot(), "config.json")); strings.Contains(string(b), "SECRET") {
		t.Fatal("auth key was persisted")
	}
	if left, _ := filepath.Glob(filepath.Join(s.tailscaleRoot(), ".authkey-*")); len(left) != 0 {
		t.Fatalf("temporary key file left behind: %v", left)
	}
	if !f.has("serve reset") || !f.has("serve --bg --tcp=22 tcp://127.0.0.1:22") || !f.has("serve --bg --tcp=8080 tcp://"+ForwardAddr) {
		cmds, _ := f.snapshot()
		t.Fatalf("forwards wrong: %v", cmds)
	}

	// Off stops the daemon but keeps the login state and settings.
	code, v, _ = tsPost(t, s, `{"enabled":false}`)
	if code != http.StatusOK || v.Running || v.Enabled || !f.has("systemctl stop enginex-tailscaled") {
		t.Fatalf("disable: %d %+v", code, v)
	}
	if v.LoginServer != "https://hs.example" || !v.Forward.SSH {
		t.Fatalf("settings lost: %+v", v)
	}
}
