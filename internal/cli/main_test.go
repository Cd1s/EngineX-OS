package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
