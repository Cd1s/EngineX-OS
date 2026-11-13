package webui

import (
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
	uiScaleEnvPath = at("data", "sc5000-custom", "ui-scale.env")
	sshPortDropInPath = at("etc", "systemd", "system", "sshd.service.d", "51-port.conf")
	sshRunDropInPath = at("run", "systemd", "system", "sshd.service.d", "51-port.conf")
	gen1SSHDropInPath = at("etc", "systemd", "system", "enginex-sshd.service.d", "51-port.conf")
	gen1SSHRunDropInPath = at("run", "systemd", "system", "enginex-sshd.service.d", "51-port.conf")
	sshPersistKeyDir = at("data", "enginex-absent")
	deviceCompatiblePath = at("sys", "compatible-absent")
	updateFileURLPath = at("tmp", "update-file-url")
	engineQuitReasonPath = at("tmp", "engine-quit-reason")
	fourDeckRolePath = at("run", "enginex-assignments", "role")
	engineScriptPath = at("usr", "Engine", "Scripts", "engine")
	planckRemoteScreenPath = at("usr", "bin", "planck-remote-screen")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
