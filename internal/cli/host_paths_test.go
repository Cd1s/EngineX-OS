package cli

import (
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
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

// Pro DJ Link's live-display MediaCache defaults to tmpfs (/tmp), never to /data or a user drive under /media.
func TestProlinkMediaCacheDefaultStaysInTmpfs(t *testing.T) {
	t.Setenv("ENGINEX_PROLINK_MEDIA_CACHE", "")
	got := prolink.SharedMediaCacheDir()
	if !strings.HasPrefix(got, "/tmp/") || strings.HasPrefix(got, "/data") || strings.HasPrefix(got, "/media") {
		t.Fatalf("SharedMediaCacheDir() = %q, must default under /tmp/", got)
	}
}
