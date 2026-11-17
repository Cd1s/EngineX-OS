package cli

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// A first import that does not fit in the mount's time goes on after the mount in a job of its
// own (#301): `rb-bridge --import-path`, started by the --mount-path run through systemd, at
// idle CPU and I/O priority so a set playing from the same stick keeps its audio. The edisksd
// hook stops the job under the same unit name before it unmounts the volume.

// importUnit names the job of the volume mounted at mount: FNV-1a 64 of the path, as the hook
// computes it (edisksd_hook.c import_unit).
func importUnit(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return fmt.Sprintf("enginex-rb-import-%016x.service", h.Sum64())
}

// startImport starts the background job; a job of this volume that is still loaded keeps going.
var startImport = func(mount string) error {
	exe := os.Args[0]
	if !filepath.IsAbs(exe) {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	args := []string{"--unit=" + importUnit(mount), "--collect", "--no-block", "--quiet",
		"--slice=enginex-background.slice", "--nice=19",
		"--property=CPUSchedulingPolicy=idle", "--property=IOSchedulingClass=idle", "--property=TimeoutStopSec=5",
		"--setenv=GOMAXPROCS=1", "--setenv=GOMEMLIMIT=128MiB", exe}
	if filepath.Base(exe) != "rb-bridge" {
		args = append(args, "rb-bridge") // the multi-call suite
	}
	out, err := exec.Command("systemd-run", append(args, "--import-path", mount)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "already") { // loaded from an earlier mount of this volume
			return nil
		}
		return fmt.Errorf("background import not started: %v: %s", err, msg)
	}
	return nil
}

// importLock lets one job run at a time, whatever the number of sticks.
var importLock = "/run/enginex-rb-import.lock"

// runImportJob is --import-path: wait for the visible mount, finish the import, then show
// that the library is ready until the volume is ejected.
func runImportJob(ctx context.Context, mount string) error {
	if !filepath.IsAbs(mount) {
		return fmt.Errorf("--import-path must be absolute")
	}
	// Started from the hook's private namespace, before the visible mount.
	if err := pollUntil(ctx, 2*time.Minute, func() bool { return mounted(mount) }); err != nil {
		return fmt.Errorf("%s not mounted: %w", mount, err)
	}
	// A lazy unmount leaves the stick writable while it can be pulled: stop with it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = pollUntil(ctx, 0, func() bool { return !mounted(mount) })
		cancel()
	}()
	lock, err := os.OpenFile(importLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = pollUntil(ctx, 0, func() bool { return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil }); err != nil {
		return nil // ejected while another stick's import ran
	}
	rep := newUSBLoadingReporter(usbLoadingStatusPath(), detectMediaKind(mount, ""))
	rep.state = "importing"
	rep.Start()
	defer rep.Done()
	// The browser shows the state for 30 s after each write.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rep.Refresh()
			}
		}
	}()
	r, err := bridge.ImportInBackground(ctx, mount, func(done, total int) { rep.Report(done, total, true) })
	if ctx.Err() != nil {
		log.Printf("rb-bridge: import of %s stopped; the next mount goes on from its last pass", mount)
		return nil
	}
	if err != nil {
		rep.Fail(err)
		return err
	}
	if r.Bypassed {
		return nil
	}
	// Published by a remount (#301); the hook stops this job when the remount unmounts the stick.
	log.Printf("rb-bridge: import of %s complete; a remount publishes it", mount)
	rep.Publishing()
	if err = startRemount(mount); err != nil {
		log.Printf("rb-bridge: %v", err)
		rep.Ready()
	}
	<-ctx.Done()
	return nil
}

// mounted reports a mount point: its own filesystem, not its parent's. A variable for tests.
var mounted = func(p string) bool {
	var st, parent syscall.Stat_t
	return syscall.Lstat(p, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR &&
		syscall.Stat(filepath.Dir(p), &parent) == nil && st.Dev != parent.Dev
}

// pollUntil polls ok every 250 ms until it holds, ctx ends or, with limit > 0, limit passes.
func pollUntil(ctx context.Context, limit time.Duration, ok func() bool) error {
	var expired <-chan time.Time
	if limit > 0 {
		t := time.NewTimer(limit)
		defer t.Stop()
		expired = t.C
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for !ok() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-expired:
			return errors.New("timed out")
		case <-tick.C:
		}
	}
	return nil
}
