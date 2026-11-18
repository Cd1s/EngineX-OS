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
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// The hook stops the job under this name before an eject (edisksd_hook.c import_unit,
// test_eject_stops_the_background_import pins the same value).
func TestImportUnitMatchesTheHook(t *testing.T) {
	if got := importUnit("/media/REKORDBOX"); got != "enginex-rb-import-2531a9d305fa8897.service" {
		t.Fatalf("unit %q", got)
	}
}

// #301: a first import the mount's time cannot hold goes on in a job, at idle priority.
func TestMountPathStartsTheBackgroundImport(t *testing.T) {
	status := filepath.Join(t.TempDir(), "preparing")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	root := t.TempDir()
	testfixture.Export(t, root)
	// An export too large to read while the mount waits (64 MB, sparse): the stage waits for its job.
	if err := os.Truncate(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"), 64<<20); err != nil {
		t.Fatal(err)
	}
	if r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	var started []string
	old := startImport
	startImport = func(mount string) error { started = append(started, mount); return nil }
	defer func() { startImport = old }()
	var out bytes.Buffer
	if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
		t.Fatal(err)
	}
	var r enginedb.Result
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
	if err := startImport("/media/REKORDBOX"); err != nil {
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
