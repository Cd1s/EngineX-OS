package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// unitRunner answers systemctl is-active with state (exit status 3 when it is
// not "active", as systemctl does) and records every command.
type unitRunner struct {
	mu    sync.Mutex
	state string
	cmds  []string
}

func (u *unitRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cmds = append(u.cmds, strings.Join(append([]string{name}, args...), " "))
	if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
		if u.state != "active" {
			return []byte(u.state + "\n"), errors.New("exit status 3")
		}
		return []byte("active\n"), nil
	}
	return nil, nil
}

func (u *unitRunner) ran(cmd string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.cmds {
		if c == cmd {
			return true
		}
	}
	return false
}

func sshCall(s *Server, method, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleSSH(w, httptest.NewRequest(method, "/api/ssh", strings.NewReader(body)))
	return w
}

// BH-GC2-9: a stopped SSH (systemctl is-active: "inactive", exit 3) was shown as
// running, because any error fell back to "enabled".
func TestSSHStoppedIsNotReportedRunning(t *testing.T) {
	asDeck(t, "inmusic,jp21")
	u := &unitRunner{state: "inactive"}
	SetSystemRunner(u.run)
	defer SetSystemRunner(nil)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	if w := sshCall(s, http.MethodGet, ""); !strings.Contains(w.Body.String(), `"running":false`) {
		t.Fatalf("stopped SSH reported as: %s", w.Body.String())
	}
	u.state = "active"
	if w := sshCall(s, http.MethodGet, ""); !strings.Contains(w.Body.String(), `"running":true`) {
		t.Fatalf("running SSH reported as: %s", w.Body.String())
	}
}

// BH-GC2-9: first-generation EngineX images run the key-only
// enginex-sshd.service (Conflicts=sshd.service, PasswordAuthentication
// no). The console restarted the stock sshd, which stopped that unit, wrote
// the port for the wrong unit, and "set" a password that could never log in.
// A key upload alone never started SSH.
func TestSSHGen1ManagesEngineXsKeyOnlyUnit(t *testing.T) {
	asDeck(t, "inmusic,jp07")
	u := &unitRunner{state: "inactive"}
	SetSystemRunner(u.run)
	defer SetSystemRunner(nil)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	if w := sshCall(s, http.MethodPost, `{"enabled":true,"port":2222}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !u.ran("systemctl restart enginex-sshd") || u.ran("systemctl restart sshd") || u.ran("systemctl unmask sshd") {
		t.Fatalf("wrong unit: %q", u.cmds)
	}
	b, err := os.ReadFile(gen1SSHDropInPath)
	if err != nil || !strings.Contains(string(b), "-f /usr/share/enginex/sshd_config -p 2222") {
		t.Fatalf("gen-1 port drop-in: %v %q", err, b)
	}
	if _, err := os.Stat(sshPortDropInPath); !os.IsNotExist(err) {
		t.Fatal("a port drop-in for the stock sshd on a first-generation deck")
	}
	if w := sshCall(s, http.MethodPost, `{"password":"secret"}`); w.Code != http.StatusBadRequest || u.ran("sh -c printf 'root:%s\\n' \"$1\" | chpasswd sh secret") {
		t.Fatalf("password on a key-only deck: %d %s", w.Code, w.Body.String())
	}
	u.cmds = nil
	if w := sshCall(s, http.MethodPost, `{"authorizedKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEngineXTestKey test"}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !u.ran("systemctl start enginex-sshd") {
		t.Fatalf("key upload did not start SSH: %q", u.cmds)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "authorized_keys")); err != nil {
		t.Fatal(err)
	}
}
