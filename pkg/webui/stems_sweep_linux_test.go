//go:build linux

package webui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A drive mounted on the download directory: the path is under /data and is
// no link, and only the device says that the files are not on the deck's own
// file system. Mounting needs root in a mount namespace of its own (the build
// host's test wrapper); elsewhere this case is skipped and the link case in
// stems_source_test.go still runs.
func TestStemsSweepLeavesAMountedDriveAlone(t *testing.T) {
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	stemsSweepLibrary(t, data)
	dl := filepath.Join(data, "enginex/Stream")
	os.MkdirAll(dl, 0o755)
	if err := syscall.Mount("tmpfs", dl, "tmpfs", 0, "size=1m"); err != nil {
		t.Skipf("cannot mount here: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(dl, syscall.MNT_DETACH) })
	if a, _ := deviceOf(data); onDevice(dl, a) {
		t.Fatal("the mount is on the data root's device: this proves nothing")
	}
	stemsSweepKeeps(t, dl)
}
