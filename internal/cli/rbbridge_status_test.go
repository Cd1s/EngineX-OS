package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/v2/bridgetest"
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

	// 1. A mount that leaves work to the background job leaves the status to it; a mount of a
	// complete library clears it.
	root := t.TempDir()
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("RBBridge success: %v", err)
	}
	if _, err := os.Stat(statusPath); err != nil {
		t.Fatalf("the job reports from here on: the status file must stay, got err=%v", err)
	}
	bridgetest.Import(t, root)
	out.Reset()
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatalf("RBBridge of a complete library: %v", err)
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

// #328 stage 1B, cause A: the hook's clear_preparing() removes the status file at the end of the
// mount, while the background import the mount started goes on; its refresh rewrote the file
// only while the file was its own, so the banner was gone until the job's next count, minutes
// later. The refresh brings it back, by the #301 rules: not over another drive's status nor over
// its error, and not after the job is done.
func TestIssue328StatusComesBackAfterTheHookClearsIt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "preparing")
	read := func() (usbLoadingStatus, bool) {
		var st usbLoadingStatus
		b, err := os.ReadFile(p)
		return st, err == nil && json.Unmarshal(b, &st) == nil
	}
	job := newUSBLoadingReporter(p, "usb")
	job.state = "importing"
	job.Start()
	job.Report(120, 2400, true)
	if err := os.Remove(p); err != nil { // the hook's clear_preparing()
		t.Fatal(err)
	}
	job.Refresh()
	if st, ok := read(); !ok || st.State != "importing" || st.Done != 120 || st.Total != 2400 || st.Owner != job.owner {
		t.Fatalf("the job's status after the hook cleared it and a refresh: %+v %v", st, ok)
	}
	// A count the throttle held back is the one the refresh writes.
	job.Report(121, 2400, false)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	job.Refresh()
	if st, ok := read(); !ok || st.Done != 121 {
		t.Fatalf("refresh after a throttled count: %+v %v", st, ok)
	}
	// Another drive's status stays.
	other := newUSBLoadingReporter(p, "sd")
	other.state = "importing"
	other.Start()
	other.Report(4, 10, true)
	job.Refresh()
	if st, ok := read(); !ok || st.Owner != other.owner || st.Done != 4 {
		t.Fatalf("the job's refresh took another drive's status: %+v %v", st, ok)
	}
	// Another drive's error stays the latest word.
	other.Fail(errors.New("other failed"))
	job.Refresh()
	if _, ok := read(); ok {
		t.Fatalf("the job's refresh wrote over another drive's error")
	}
	other.Done()
	// Its own error cleared, the job's status comes back; once the job is done, it does not.
	job.Refresh()
	if st, ok := read(); !ok || st.Owner != job.owner {
		t.Fatalf("the job's status did not come back: %+v %v", st, ok)
	}
	job.Done()
	job.Refresh()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("a done job's refresh brought its status back: %v", err)
	}
}
