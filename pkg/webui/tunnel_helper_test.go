package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const fakeTunnelLine = "echo 'INF |  https://enginex-deck-9876.trycloudflare.com  |'\n"

// fakeTunnelHelper serves script as the helper's download and makes stateDir/bin its only place.
// Call it before the server exists: its clean-up restores the package settings after the server's.
func fakeTunnelHelper(t *testing.T, stateDir, script string) *atomic.Int32 {
	t.Helper()
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write([]byte(script))
	}))
	oldBase, oldArch, oldPaths, oldLook, oldInstall := cloudflaredDownloadBaseURL, cloudflaredGOARCH, cloudflaredSearchPaths, cloudflaredLookPath, cloudflaredInstallPath
	cloudflaredDownloadBaseURL = srv.URL
	cloudflaredGOARCH = "arm64"
	cloudflaredSearchPaths = nil
	cloudflaredLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	cloudflaredInstallPath = filepath.Join(stateDir, "bin", "cloudflared")
	t.Cleanup(func() {
		srv.Close()
		cloudflaredDownloadBaseURL, cloudflaredGOARCH, cloudflaredSearchPaths, cloudflaredLookPath, cloudflaredInstallPath = oldBase, oldArch, oldPaths, oldLook, oldInstall
	})
	return &downloads
}

// tunnelServer is a server whose tunnel is stopped before the package settings are restored.
func tunnelServer(t *testing.T, stateDir string) (*Server, *TunnelManager) {
	t.Helper()
	s := New(Config{StateDir: stateDir, UIDir: t.TempDir(), AccessToken: "ABCD-2345"}, nil)
	m := s.TunnelManager()
	t.Cleanup(func() { m.Stop(); tunnelManagers.Delete(s) })
	return s, m
}

// settledTunnel waits until the tunnel has left the given states (or five seconds have passed).
func settledTunnel(m *TunnelManager, busy ...string) TunnelStatus {
	var st TunnelStatus
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		st = m.Status("")
		if !strings.Contains(" "+strings.Join(busy, " ")+" ", " "+st.Status+" ") {
			break
		}
	}
	return st
}

// A helper that printed its address and then ended by itself, even with exit status 0, has no
// address any more: the console and the QR code must not go on showing it.
func TestTunnelHelperThatEndsByItselfIsNotShownAsRunning(t *testing.T) {
	stateDir := t.TempDir()
	fakeTunnelHelper(t, stateDir, "#!/bin/sh\n"+fakeTunnelLine+"exit 0\n")
	s, m := tunnelServer(t, stateDir)
	if err := m.Configure(true, nil); err != nil {
		t.Fatal(err)
	}
	st := settledTunnel(m, "installing", "starting", "running")
	if st.Status != "error" || st.PublicURL != "" || s.PublicTunnelURL() != "" {
		t.Fatalf("the helper ended by itself: status %q, address %q; want an error and no address", st.Status, st.PublicURL)
	}
	if st.Error == "" || strings.Contains(st.Error, "trycloudflare") {
		t.Fatalf("the helper ended by itself with status 0: the console shows %q as the reason, want a text that says it ended (not its own address line)", st.Error)
	}
}

// What a power cut right after an earlier download can leave: the helper's name, executable, no
// content. It must be downloaded again, not run.
func TestTunnelHelperFileThatIsEmptyIsDownloadedAgain(t *testing.T) {
	stateDir := t.TempDir()
	downloads := fakeTunnelHelper(t, stateDir, "#!/bin/sh\n"+fakeTunnelLine+"exec sleep 30\n")
	if err := os.MkdirAll(filepath.Join(stateDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "bin", "cloudflared"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	_, m := tunnelServer(t, stateDir)
	if err := m.Configure(true, nil); err != nil {
		t.Fatal(err)
	}
	st := settledTunnel(m, "installing", "starting")
	if st.Status != "running" || downloads.Load() != 1 {
		t.Fatalf("an empty helper file: status %q (%s), %d downloads; want one download and a running tunnel", st.Status, st.Error, downloads.Load())
	}
}

// Closing the server stops the helper it started and forgets its manager.
func TestCloseStopsTheTunnelHelper(t *testing.T) {
	stateDir := t.TempDir()
	pidFile := filepath.Join(stateDir, "helper.pid")
	fakeTunnelHelper(t, stateDir, "#!/bin/sh\necho $$ > '"+pidFile+"'\n"+fakeTunnelLine+"exec sleep 30\n")
	s, m := tunnelServer(t, stateDir)
	if err := m.Configure(true, nil); err != nil {
		t.Fatal(err)
	}
	if st := settledTunnel(m, "installing", "starting"); st.Status != "running" {
		t.Fatalf("the tunnel did not start: %q (%s)", st.Status, st.Error)
	}
	b, err := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("no helper process id: %q %v", b, err)
	}

	s.Close()

	if _, ok := tunnelManagers.Load(s); ok {
		t.Error("Close left the server in the tunnel registry")
	}
	if st := m.Status(""); st.Status != "stopped" || st.PublicURL != "" {
		t.Errorf("after Close the tunnel is %q with address %q, want stopped and none", st.Status, st.PublicURL)
	}
	gone := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			gone = true
			break
		}
	}
	if !gone {
		t.Errorf("the helper process %d still runs after Close", pid)
	}
}
