package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

// The package's tests run as root on shared build hosts: the markers and the import lock must point
// into the private directory of TestMain, not into the host's /run (86 stray markers, 2026-11-22).
func TestMarkersStayOffTheHost(t *testing.T) {
	for name, p := range map[string]string{"ejectMarkDir": ejectMarkDir, "importLock": importLock} {
		if p == "/run" || strings.HasPrefix(p, "/run/") {
			t.Errorf("%s = %s: this package's tests would write the host's /run", name, p)
		}
	}
}

// The drive status file of this package's tests is the private one of TestMain, not the deck's own path
// (a trace of the tests found /run/enginex-usb-preparing and its lock written on a build host, 2026-11-22).
func TestDriveStatusStaysOffTheHost(t *testing.T) {
	if p := usbLoadingStatusPath(); p == defaultUSBPreparingPath || strings.HasPrefix(p, "/run/") {
		t.Fatalf("usbLoadingStatusPath() = %s: this package's tests would write the host's /run", p)
	}
}

// The mount filter takes its root from a variable, so a test keeps its drive in its own directory
// instead of creating one under the host's /media.
func TestLocalExportsFollowMediaRoot(t *testing.T) {
	base := t.TempDir()
	drive := filepath.Join(base, "DRIVE")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	testfixture.Export(t, drive)
	line := fmt.Sprintf("/dev/test %s ext4 rw 0 0\n", drive)
	if got := localExportsFrom(strings.NewReader(line)); len(got) != 0 {
		t.Fatalf("a drive outside %s was selected: %v", mediaRoot, got)
	}
	old := mediaRoot
	mediaRoot = base + string(os.PathSeparator)
	defer func() { mediaRoot = old }()
	if got := localExportsFrom(strings.NewReader(line)); len(got) != 1 || got[0] != drive {
		t.Fatalf("the drive below mediaRoot was not selected: %v", got)
	}
}
