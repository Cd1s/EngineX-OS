package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestDetectMediaKind(t *testing.T) {
	mounts := filepath.Join(t.TempDir(), "mounts")
	content := strings.Join([]string{
		"/dev/mmcblk0p7 /data ext4 rw 0 0",
		"/dev/mmcblk1p1 /media/SD_CARD exfat rw 0 0",
		"/dev/sda1 /media/USB_DRIVE vfat rw 0 0",
	}, "\n") + "\n"
	if err := os.WriteFile(mounts, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectMediaKind("/media/SD_CARD", mounts); got != "sd" {
		t.Fatalf("mmcblk1p1 kind = %q, want sd", got)
	}
	if got := detectMediaKind("/media/USB_DRIVE", mounts); got != "usb" {
		t.Fatalf("sda1 kind = %q, want usb", got)
	}
}

func TestRBBridgeUSBLoadingStatusLifecycle(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "preparing")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", statusPath)

	// 1. Progress reporter writes throttled loading JSON and clears on success.
	root := t.TempDir()
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("RBBridge success: %v", err)
	}
	if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
		t.Fatalf("status file must be removed on success, got err=%v", err)
	}
	if _, err := os.Stat(statusPath + ".err"); !os.IsNotExist(err) {
		t.Fatalf("error file must not exist on success, got err=%v", err)
	}

	// 2. Reporter writes loading state with counts.
	rep := newUSBLoadingReporter(statusPath, "sd")
	rep.Report(1200, 21352, true)
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var st usbLoadingStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.State != "loading" || st.Kind != "sd" || st.Done != 1200 || st.Total != 21352 {
		t.Fatalf("unexpected loading status: %+v", st)
	}

	// 3. Failure removes loading file and records error text in .err file.
	badRoot := filepath.Join(t.TempDir(), "missing")
	if err := os.MkdirAll(filepath.Join(badRoot, "PIONEER", "rekordbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "PIONEER", "rekordbox", "export.pdb"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = RBBridge([]string{"--mount-path", badRoot}, &out)
	if err == nil {
		t.Fatal("expected RBBridge to fail on corrupt export.pdb")
	}
	if _, statErr := os.Stat(statusPath); !os.IsNotExist(statErr) {
		t.Fatalf("loading file must be removed on failure, got %v", statErr)
	}
	errRaw, readErr := os.ReadFile(statusPath + ".err")
	if readErr != nil {
		t.Fatalf("expected .err file on failure: %v", readErr)
	}
	var errSt usbLoadingStatus
	if err := json.Unmarshal(errRaw, &errSt); err != nil {
		t.Fatal(err)
	}
	if errSt.State != "error" || errSt.Error == "" {
		t.Fatalf("unexpected error status: %+v", errSt)
	}
}
