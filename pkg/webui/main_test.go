package webui

import (
	"fmt"
	"github.com/Cd1s/EngineX-OS/internal/migration"
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps every test of this package off the machine it runs on: the
// host paths the server writes point into a private directory, and the device
// tree is absent (a test that wants a deck points deviceCompatiblePath at a
// file of its own). System commands are refused by guardHostCommand unless a
// stand-in under the temporary directory is used. r3: a test that wrote the
// build host's sshd drop-in cost that host its SSH access.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "webui-host-")
	if err != nil {
		panic(err)
	}
	at := func(parts ...string) string { return filepath.Join(append([]string{dir}, parts...)...) }
	uiScaleEnvPath = at("data", migration.DataName, "ui-scale.env")
	sshPortDropInPath = at("etc", "systemd", "system", "sshd.service.d", "51-port.conf")
	sshRunDropInPath = at("run", "systemd", "system", "sshd.service.d", "51-port.conf")
	gen1SSHDropInPath = at("etc", "systemd", "system", "enginex-sshd.service.d", "51-port.conf")
	gen1SSHRunDropInPath = at("run", "systemd", "system", "enginex-sshd.service.d", "51-port.conf")
	sshPersistKeyDir = at("data", "enginex-absent")
	tlsPersistDir = at("data", "enginex-absent")
	deviceCompatiblePath = at("sys", "compatible-absent")
	fourDeckRolePath = at("run", "enginex-assignments", "role")
	samplerDataDir = at("data", "enginex", "samples")
	// The update keeps updateReserve free for Engine; the tests run on whatever disk the host has.
	freeBytes = func(string) (int64, bool) { return 1 << 40, true }
	role := fourDeckRolePath
	code := m.Run()
	if code == 0 && fourDeckRolePath != role {
		// A test that sets the path must put the old value back, not the deck's real path.
		fmt.Fprintln(os.Stderr, "a test left fourDeckRolePath at", fourDeckRolePath)
		code = 1
	}
	os.RemoveAll(dir)
	os.Exit(code)
}
