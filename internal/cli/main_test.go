package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// systemdStartImport and systemdStartRemount are the real starters, for the tests of their
// command lines.
var systemdStartImport, systemdStartRemount func(string) error

// TestMain keeps this package's tests off the machine they run on: the served and ejected markers
// and the import lock go into a private directory instead of the host's /run. A test that wants its
// own directory still sets ejectMarkDir itself and puts the old value back.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cli-host-")
	if err != nil {
		panic(err)
	}
	ejectMarkDir = dir
	importLock = filepath.Join(dir, "enginex-rb-import.lock")
	// No test starts a unit on the machine it runs on: a test binary started as the job would run
	// this package's tests again, as root and outside any sandbox. A test of the unit's command
	// line puts its own systemd-run in PATH and sets these back.
	systemdStartImport, systemdStartRemount = startImport, startRemount
	startImport = func(string) error { return nil }
	startRemount = func(string) error { return nil }
	// The drive status file as well: a test that does not set this variable itself would use the deck's own path.
	os.Setenv("ENGINEX_USB_PREPARING_PATH", filepath.Join(dir, "enginex-usb-preparing"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
