package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// The hook stops the job under this name before an eject (edisksd_hook.c import_unit,
// test_eject_stops_the_background_import pins the same value).
func TestImportUnitMatchesTheHook(t *testing.T) {
	if got := importUnit("/media/REKORDBOX"); got != "enginex-rb-import-2531a9d305fa8897.service" {
		t.Fatalf("unit %q", got)
	}
}

// #301: what a mount window leaves to do goes on in a job, at idle priority. The first insert's
// window writes rows and lists; cues, grids and waveforms are the job's.
func TestMountPathStartsTheBackgroundImport(t *testing.T) {
	status := filepath.Join(t.TempDir(), "preparing")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	root := t.TempDir()
	testfixture.Export(t, root)
	var started []string
	old := startImport
	startImport = func(mount string) error { started = append(started, mount); return nil }
	defer func() { startImport = old }()
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatal(err)
	}
	var r bridge.MountResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.Background {
		t.Fatalf("result %q %v", &out, err)
	}
	if len(started) != 1 || started[0] != root {
		t.Fatalf("jobs started: %v", started)
	}
	// The job reports from here on; the mount's own status stays until then.
	if _, err := os.Stat(status); err != nil {
		t.Fatalf("loading status removed: %v", err)
	}
}

func TestStartImportRunsAnIdleSystemdUnit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+log+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := systemdStartImport("/media/REKORDBOX"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, want := range []string{"--unit=enginex-rb-import-2531a9d305fa8897.service", "--collect", "--no-block",
		"--slice=enginex-background.slice", "--nice=19", "--property=CPUSchedulingPolicy=idle", "--property=IOSchedulingClass=idle"} {
		if !strings.Contains("\n"+string(b), "\n"+want+"\n") {
			t.Fatalf("missing %s in %q", want, args)
		}
	}
	// The test binary stands for the suite: its applet goes before the flags.
	if n := len(args); n < 4 || args[n-3] != "rb-bridge" || args[n-2] != "--import-path" || args[n-1] != "/media/REKORDBOX" {
		t.Fatalf("command %q", args)
	}
}

func TestImportJobWaitsForTheVisibleMount(t *testing.T) {
	if !mounted("/proc") || mounted(t.TempDir()) {
		t.Fatal("mount point detection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := runImportJob(ctx, t.TempDir()); err == nil {
		t.Fatal("a job ran on a directory that is not mounted")
	}
}

func TestImportStatusWaitingStaysUntilDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preparing")
	rep := newUSBLoadingReporter(path, "usb")
	rep.state = "importing"
	rep.Report(10, 600, true)
	read := func() usbLoadingStatus {
		t.Helper()
		var st usbLoadingStatus
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &st) != nil {
			t.Fatalf("status %q %v", b, err)
		}
		return st
	}
	if st := read(); st.State != "importing" || st.Done != 10 || st.Total != 600 {
		t.Fatalf("importing: %+v", st)
	}
	rep.Waiting()
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	rep.Refresh()
	if st, err := os.Stat(path); err != nil || time.Since(st.ModTime()) > 10*time.Second || read().State != "waiting" {
		t.Fatalf("refresh: %v %+v", err, read())
	}
	rep.Done()
	rep.Refresh()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("status after done: %v", err)
	}
}

// A stick without an export and without a snapshot has nothing to fill: the job ends quietly,
// with no error for the browser and no remount. A stick with an export that no window converted
// (a window left a big export to the job, or was stopped before it saved a snapshot) gets its
// library from the job: the next library, and one remount that swaps it in.
func TestImportJobWithoutASnapshot(t *testing.T) {
	oldMounted, oldLock, oldRemount := mounted, importLock, startRemount
	mounted = func(string) bool { return true }
	importLock = filepath.Join(t.TempDir(), "lock")
	remounts := 0
	startRemount = func(string) error { remounts++; return nil }
	defer func() { mounted, importLock, startRemount = oldMounted, oldLock, oldRemount }()
	t.Setenv(rbstate.StageEnv, t.TempDir())
	t.Setenv("ENGINEX_RB_PRIORITY_DIR", t.TempDir())
	status := filepath.Join(t.TempDir(), "status")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	empty := t.TempDir()
	if err := runImportJob(context.Background(), empty); err != nil || remounts != 0 {
		t.Fatalf("job on a stick without an export: %v, %d remounts", err, remounts)
	}
	if _, err := os.Stat(status + ".err"); !os.IsNotExist(err) {
		t.Fatalf("an error was left for the browser: %v", err)
	}
	root := t.TempDir()
	testfixture.Export(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	startRemount = func(string) error { remounts++; cancel(); return nil } // the hook stops the job at the remount
	if err := runImportJob(ctx, root); err != nil || remounts != 1 {
		t.Fatalf("job on a stick no window converted: %v, %d remounts", err, remounts)
	}
	db := filepath.Join(root, "Engine Library", "Database2")
	if _, err := os.Stat(filepath.Join(db, "m.db")); !os.IsNotExist(err) {
		t.Fatalf("the job wrote the stick's library itself: %v", err)
	}
	res, err := bridge.ConvertAtMount(context.Background(), root, bridge.MountOptions{Publish: true})
	if err != nil || res.Pending.Rows != 0 || res.Pending.Lists {
		t.Fatalf("the window of the remount: %+v, %v", res, err)
	}
	if h, err := rbstate.ReadHeader(db); err != nil || h.Tracks != 5 {
		t.Fatalf("after the swap: %+v, %v", h, err)
	}
	if _, err := os.Stat(status + ".err"); !os.IsNotExist(err) {
		t.Fatalf("an error was left for the browser: %v", err)
	}
}
