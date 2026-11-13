package webui

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review of 03cf399: the first-generation 5.3.0 root is mounted read-only (no etc.mount). The
// console wrote the key-only unit's port drop-in to /etc, ignored EROFS and never used /run,
// so a new port silently did not apply. All paths are this test's own; systemctl is a stub.
func TestSSHGen1PortGoesToRunOnAReadOnlyEtc(t *testing.T) {
	asDeck(t, "inmusic,jp07")
	u := &unitRunner{state: "active"}
	SetSystemRunner(u.run)
	defer SetSystemRunner(nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "etc"), nil, 0o644); err != nil { // a file: /etc cannot be written
		t.Fatal(err)
	}
	savedEtc, savedRun := gen1SSHDropInPath, gen1SSHRunDropInPath
	gen1SSHDropInPath = filepath.Join(dir, "etc", "systemd", "system", "sc5000-custom-sshd.service.d", "51-port.conf")
	gen1SSHRunDropInPath = filepath.Join(dir, "run", "systemd", "system", "sc5000-custom-sshd.service.d", "51-port.conf")
	defer func() { gen1SSHDropInPath, gen1SSHRunDropInPath = savedEtc, savedRun }()
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	if w := sshCall(s, http.MethodPost, `{"port":2222}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	b, err := os.ReadFile(gen1SSHRunDropInPath)
	if err != nil || !strings.Contains(string(b), "-f /usr/share/sc5000-custom/sshd_config -p 2222") {
		t.Fatalf("no /run port drop-in on a read-only /etc: %v %q", err, b)
	}
	if !u.ran("systemctl daemon-reload") || !u.ran("systemctl restart sc5000-custom-sshd") {
		t.Fatalf("the new port was not applied: %q", u.cmds)
	}
}

// Review of 03cf399: on the first generation, "off" only stopped the unit until the next boot,
// where the prepare script's target started it again on port 22; a port did not survive a
// reboot either. Off now also masks it for this boot, and RestoreSSH applies the saved
// settings after each boot.
func TestSSHGen1SettingsSurviveAReboot(t *testing.T) {
	asDeck(t, "inmusic,jp07")
	u := &unitRunner{state: "active"}
	SetSystemRunner(u.run)
	defer SetSystemRunner(nil)
	state := t.TempDir()
	s := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
	if w := sshCall(s, http.MethodPost, `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !u.ran("systemctl stop sc5000-custom-sshd") || !u.ran("systemctl mask --runtime sc5000-custom-sshd") {
		t.Fatalf("off: %q", u.cmds)
	}
	// The boot: /run is empty again and the unit was started on port 22.
	os.RemoveAll(filepath.Dir(gen1SSHRunDropInPath))
	u.cmds = nil
	New(Config{StateDir: state, UIDir: t.TempDir()}, nil).RestoreSSH(context.Background())
	if !u.ran("systemctl stop sc5000-custom-sshd") || !u.ran("systemctl mask --runtime sc5000-custom-sshd") {
		t.Fatalf("off was not restored after a boot: %q", u.cmds)
	}
	// On again, on port 2200: after a boot the port comes back.
	if w := sshCall(s, http.MethodPost, `{"enabled":true,"port":2200}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !u.ran("systemctl unmask --runtime sc5000-custom-sshd") {
		t.Fatalf("on did not lift the runtime mask: %q", u.cmds)
	}
	os.Remove(gen1SSHDropInPath)
	os.Remove(gen1SSHRunDropInPath)
	u.cmds = nil
	New(Config{StateDir: state, UIDir: t.TempDir()}, nil).RestoreSSH(context.Background())
	b, err := os.ReadFile(gen1SSHDropInPath)
	if err != nil {
		b, err = os.ReadFile(gen1SSHRunDropInPath)
	}
	if err != nil || !strings.Contains(string(b), "-p 2200") || !u.ran("systemctl restart sc5000-custom-sshd") {
		t.Fatalf("the port was not restored after a boot: %v %q %q", err, b, u.cmds)
	}
	// Without saved settings a boot changes nothing (#46, #116).
	u.cmds = nil
	New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil).RestoreSSH(context.Background())
	if len(u.cmds) != 0 {
		t.Fatalf("a deck without console settings: %q", u.cmds)
	}
}
