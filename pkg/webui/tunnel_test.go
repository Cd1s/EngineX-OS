package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func doTunnelReq(t *testing.T, s *Server, method, body string) (int, TunnelStatus, string) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, "/api/tunnel", bytes.NewReader([]byte(body)))
	} else {
		req = httptest.NewRequest(method, "/api/tunnel", nil)
	}
	req.Header.Set("Authorization", "Bearer "+s.AccessToken())
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var st TunnelStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	return w.Code, st, w.Body.String()
}

func TestTunnelQuickAndNamedModes(t *testing.T) {
	stateDir := t.TempDir()
	uiDir := t.TempDir()

	// Fake cloudflared script that prints a Quick Tunnel URL for --url mode,
	// or a Registered tunnel connection message for run --token mode.
	fakeScript := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "run" ]; then
    echo "2026-11-08T00:00:00Z INF Registered tunnel connection connIndex=0"
    exec sleep 30
  fi
done
echo "2026-11-08T00:00:00Z INF +--------------------------------------------------------------------------------------------+"
echo "2026-11-08T00:00:00Z INF |  https://enginex-deck-9876.trycloudflare.com                                               |"
echo "2026-11-08T00:00:00Z INF +--------------------------------------------------------------------------------------------+"
exec sleep 30
`
	var downloads atomic.Int32
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cloudflared-linux-arm64" {
			downloads.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fakeScript))
			return
		}
		http.NotFound(w, r)
	}))
	defer dlSrv.Close()

	oldBase := cloudflaredDownloadBaseURL
	oldArch := cloudflaredGOARCH
	oldPaths := cloudflaredSearchPaths
	oldLook := cloudflaredLookPath
	oldInstall := cloudflaredInstallPath

	installTarget := filepath.Join(stateDir, "bin", "cloudflared")
	cloudflaredDownloadBaseURL = dlSrv.URL
	cloudflaredGOARCH = "arm64"
	cloudflaredSearchPaths = nil
	cloudflaredLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	cloudflaredInstallPath = installTarget

	defer func() {
		cloudflaredDownloadBaseURL = oldBase
		cloudflaredGOARCH = oldArch
		cloudflaredSearchPaths = oldPaths
		cloudflaredLookPath = oldLook
		cloudflaredInstallPath = oldInstall
	}()

	// Use the real constructor: startup resume must finish before the first
	// enable request and before the package-level mocks are restored.
	s := New(Config{StateDir: stateDir, UIDir: uiDir, AccessToken: "ABCD-2345"}, nil)
	defer func() { s.TunnelManager().Stop(); tunnelManagers.Delete(s) }()

	// 1. Initial GET /api/tunnel is stopped.
	code, st, raw := doTunnelReq(t, s, http.MethodGet, "")
	if code != http.StatusOK || st.Enabled || st.Status != "stopped" || st.AccessCode != "ABCD-2345" {
		t.Fatalf("initial status: %d %+v (%s)", code, st, raw)
	}
	if got := s.PublicTunnelURL(); got != "" {
		t.Fatalf("expected empty PublicTunnelURL when stopped, got %q", got)
	}

	// 2. Enable Quick Tunnel (auto-downloads cloudflared and extracts trycloudflare.com URL).
	code, st, raw = doTunnelReq(t, s, http.MethodPost, `{"enabled":true}`)
	if code != http.StatusOK || !st.Enabled || st.Status != "running" {
		t.Fatalf("start quick tunnel: %d %+v (%s)", code, st, raw)
	}
	if st.PublicURL != "https://enginex-deck-9876.trycloudflare.com" {
		t.Fatalf("unexpected publicUrl: %q", st.PublicURL)
	}
	if got := s.PublicTunnelURL(); got != "https://enginex-deck-9876.trycloudflare.com" {
		t.Fatalf("PublicTunnelURL() = %q", got)
	}
	if n := downloads.Load(); n != 1 {
		t.Fatalf("expected 1 on-demand binary download, got %d", n)
	}
	if info, err := os.Stat(installTarget); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary not executable: %v %v", info, err)
	}

	// Verify persisted config.
	cfgBytes, err := os.ReadFile(filepath.Join(stateDir, "tunnel-config.json"))
	if err != nil || !strings.Contains(string(cfgBytes), `"enabled": true`) && !strings.Contains(string(cfgBytes), `"enabled":true`) {
		t.Fatalf("tunnel-config.json not persisted: %v %s", err, cfgBytes)
	}

	// 3. Switch to Named Tunnel with token.
	code, st, raw = doTunnelReq(t, s, http.MethodPost, `{"enabled":true,"token":"eyJhIjoiVEVTVF9UT0tFTiJ9"}`)
	if code != http.StatusOK || !st.Enabled || st.Status != "running" || !st.HasToken {
		t.Fatalf("named tunnel: %d %+v (%s)", code, st, raw)
	}

	// 4. Reject bad token.
	if code, _, _ := doTunnelReq(t, s, http.MethodPost, `{"enabled":true,"token":"--bad-flag"}`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for flag-like token, got %d", code)
	}

	// 5. Stop tunnel.
	code, st, raw = doTunnelReq(t, s, http.MethodPost, `{"enabled":false}`)
	if code != http.StatusOK || st.Enabled || st.Status != "stopped" || st.PublicURL != "" {
		t.Fatalf("stop tunnel: %d %+v (%s)", code, st, raw)
	}
	if got := s.PublicTunnelURL(); got != "" {
		t.Fatalf("expected empty PublicTunnelURL after stop, got %q", got)
	}
}
