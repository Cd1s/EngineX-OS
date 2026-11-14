package webui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standIn puts an executable `name` that records its arguments into dir and
// dir on PATH; it returns the record file.
func standIn(t *testing.T, dir, name string) string {
	t.Helper()
	log := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\necho active\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// r3: a test reached the build host's systemd. In a test binary the real
// runner refuses every command that is not a stand-in under the temporary
// directory, so a test that forgets its mock fails instead of changing the host.
func TestSystemCommandsNeverReachTheHostFromTests(t *testing.T) {
	SetSystemRunner(nil)
	host := t.TempDir() // a "host" binary: outside the temporary directory below
	log := standIn(t, host, "systemctl")
	t.Setenv("TMPDIR", t.TempDir())
	if _, err := getSystemRunner()(context.Background(), "systemctl", "restart", "sshd"); !errors.Is(err, errHostCommand) {
		t.Fatalf("host systemctl not refused: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("host systemctl ran")
	}
	// The first-generation update runs az01-update on the staged image (#239).
	if err := guardHostCommand(az01UpdateBin); !errors.Is(err, errHostCommand) {
		t.Fatalf("host az01-update not refused: %v", err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	if err := s.restartEngine(); !errors.Is(err, errHostCommand) {
		t.Fatalf("host engine restart not refused: %v", err)
	}
	// A stand-in under the temporary directory runs (the tests' fakes).
	t.Setenv("TMPDIR", filepath.Dir(host))
	if out, err := getSystemRunner()(context.Background(), "systemctl", "is-active", "sshd"); err != nil || !strings.Contains(string(out), "active") {
		t.Fatalf("stand-in refused: %v %q", err, out)
	}
}

// r3: the SSH settings wrote a port drop-in for the host's sshd and restarted
// it. Off a deck (no Denon board in the device tree) nothing of the host's
// SSH is touched: no drop-in, no command; the settings are only saved.
func TestSSHSettingsNeverTouchTheHostOffADeck(t *testing.T) {
	SetSystemRunner(nil)
	host := t.TempDir()
	log := standIn(t, host, "systemctl")
	standIn(t, host, "sh")
	t.Setenv("TMPDIR", t.TempDir())
	s := New(Config{AccessToken: "t", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/ssh", bytes.NewBufferString(`{"enabled":true,"port":2222,"authorizedKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEngineXTestKeyOnly test"}`))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	s.handleSSHPost(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{sshPortDropInPath, sshRunDropInPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("drop-in written off a deck: %s", p)
		}
	}
	if b, err := os.ReadFile(log); err == nil {
		t.Fatalf("host systemctl ran: %s", b)
	}
	if !strings.Contains(w.Body.String(), `"port":2222`) {
		t.Fatalf("settings not kept: %s", w.Body.String())
	}
}

// asDeck makes the device tree name board for this test (onDeck is true).
func asDeck(t *testing.T, board string) {
	t.Helper()
	old := deviceCompatiblePath
	p := filepath.Join(t.TempDir(), "compatible")
	if err := os.WriteFile(p, []byte(board+"\x00rockchip,rk3288\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	deviceCompatiblePath = p
	t.Cleanup(func() { deviceCompatiblePath = old })
}
