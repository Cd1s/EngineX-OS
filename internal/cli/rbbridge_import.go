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
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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

const mountHandoffName = ".rb-bridge-mount.lock"

func mountHandoffPath(mount string) string {
	return filepath.Join(mount, "Engine Library", "Database2", mountHandoffName)
}

// holdMountHandoff keeps the mount-path conversion ahead of its detached worker.
// The kernel releases the lock when edisksd kills the mount process, which is the
// hand-off needed when ConvertAtMount never gets to return Result.Background.
func holdMountHandoff(mount string) (func(), error) {
	dir := filepath.Dir(mountHandoffPath(mount))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(mountHandoffPath(mount), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func waitMountHandoff(ctx context.Context, mount string) error {
	f, err := os.OpenFile(mountHandoffPath(mount), os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

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
	if err := waitMountHandoff(ctx, mount); err != nil {
		return err
	}
	// A lazy unmount leaves the stick writable while it can be pulled: stop with it.
	ctx, cancel := context.WithCancel(ctx)
	var watchers sync.WaitGroup
	defer func() { cancel(); watchers.Wait() }()
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		_ = pollUntil(ctx, 0, func() bool { return !mounted(mount) })
		cancel()
	}()
	// A peer that loads a track from the stick while the import runs keeps it (see remount).
	_ = os.WriteFile(servedMark(mount), nil, 0o600) // begin observing this insert
	watchers.Add(1)
	go func() { defer watchers.Done(); watchServed(ctx, mount) }()
	rep := newDriveReporter(mount)
	r, started, err := convertLocked(ctx, mount, rep)
	if !started {
		return nil // ejected while another stick's import ran
	}
	if ctx.Err() != nil {
		rep.Done()
		log.Printf("rb-bridge: import of %s stopped; the next mount goes on from its last pass", mount)
		return nil
	}
	if err != nil {
		return err // rep.Fail kept the error for the browser; the next import's Start clears it
	}
	if r.Bypassed {
		rep.Done()
		return nil
	}
	defer rep.Done()
	// The remount helper refreshes publication status after this handoff.
	// Published by a remount (#301); the hook stops this job when the remount unmounts the stick.
	log.Printf("rb-bridge: import of %s complete; a remount publishes it", mount)
	rep.Publishing()
	if err = startRemount(mount); err != nil {
		log.Printf("rb-bridge: %v", err)
		rep.FailPublication(err)
	}
	<-ctx.Done()
	return nil
}

// convertLocked runs the import under the global lock and releases it as soon as the
// conversion ends: a finished stick waiting for its remount never holds another's import.
// started is false when it never got the lock (the stick went first).
func convertLocked(ctx context.Context, mount string, rep *usbLoadingReporter) (r enginedb.Result, started bool, err error) {
	lock, err := os.OpenFile(importLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return r, true, err
	}
	defer lock.Close()
	if pollUntil(ctx, 0, func() bool { return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil }) != nil {
		return r, false, nil
	}
	rep.state = "importing"
	rep.Start()
	convert, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); refresh(convert, rep) }()
	defer func() { stop(); <-done }()
	// Throttled: the job reports its count as each track goes (#328); refresh writes the latest.
	r, err = bridge.ImportInBackground(ctx, mount, func(done, total int) { rep.Report(done, total, false) })
	if err != nil && ctx.Err() == nil {
		rep.Fail(err)
	}
	return r, true, err
}

// refresh writes rep's state again every 2 s until ctx ends.
func refresh(ctx context.Context, rep *usbLoadingReporter) {
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
