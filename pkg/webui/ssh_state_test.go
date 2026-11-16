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
// sc5000-custom-sshd.service (Conflicts=sshd.service, PasswordAuthentication
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
	if !u.ran("systemctl restart sc5000-custom-sshd") || u.ran("systemctl restart sshd") || u.ran("systemctl unmask sshd") {
		t.Fatalf("wrong unit: %q", u.cmds)
	}
	b, err := os.ReadFile(gen1SSHDropInPath)
	if err != nil || !strings.Contains(string(b), "-f /usr/share/sc5000-custom/sshd_config -p 2222") {
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
	if !u.ran("systemctl start sc5000-custom-sshd") {
		t.Fatalf("key upload did not start SSH: %q", u.cmds)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "authorized_keys")); err != nil {
		t.Fatal(err)
	}
}

// #299: on second-generation decks /etc is mounted read-only (5.3.0 removed etc.mount)
// and stock sshd enforces PasswordAuthentication no (10-az0x.conf). The console
// previously treated these boards as taking passwords and invoked chpasswd on /etc/shadow,
// failing with HTTP 500. It must refuse password submissions with HTTP 400 across all
// second-generation board prefixes and never run chpasswd.
func TestSSHGen2RefusesPasswordOnReadOnlyEtc(t *testing.T) {
	boards := []string{
		"inmusic,jp21",  // SCLIVE4
		"inmusic,jp20",  // SCLIVE2
		"inmusic,jc11s", // PRIME4PLUS
		"inmusic,jp11s", // PRIMEGOPLUS
		"inmusic,nh08s", // MIXSTREAMPROPLUS
		"inmusic,nh10",  // MIXSTREAMPROGO
		"inmusic,jp24",  // PRIME4G2
		"inmusic,rmz2",  // SYSTEMONE
	}
	for _, b := range boards {
		t.Run(b, func(t *testing.T) {
			asDeck(t, b)
			u := &unitRunner{state: "active"}
			SetSystemRunner(u.run)
			defer SetSystemRunner(nil)

			s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)

			// GET reports sshd unit and hasPassword false
			if w := sshCall(s, http.MethodGet, ""); !strings.Contains(w.Body.String(), `"hasPassword":false`) {
				t.Fatalf("%s: expected hasPassword:false, got: %s", b, w.Body.String())
			}

			// POST with password must fail with 400 "takes public keys only"
			if w := sshCall(s, http.MethodPost, `{"password":"secret"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "takes public keys only") {
				t.Fatalf("%s: expected 400 takes public keys only, got %d: %s", b, w.Code, w.Body.String())
			}

			// Ensure chpasswd / sh was NEVER called
			for _, cmd := range u.cmds {
				if strings.Contains(cmd, "chpasswd") {
					t.Fatalf("%s: chpasswd must not be called on read-only /etc: %s", b, cmd)
				}
			}

			// Key upload succeeds and manages sshd
			u.cmds = nil
			if w := sshCall(s, http.MethodPost, `{"authorizedKey":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEngineXTestKey test"}`); w.Code != http.StatusOK {
				t.Fatalf("%s: key upload failed: %d %s", b, w.Code, w.Body.String())
			}
			if !u.ran("systemctl restart sshd") && !u.ran("systemctl start sshd") {
				t.Fatalf("%s: key upload did not restart/start sshd: %q", b, u.cmds)
			}
		})
	}
}
