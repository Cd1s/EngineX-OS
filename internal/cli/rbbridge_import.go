package cli

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// What a mount window leaves to do goes on after the mount in a job of its own (#301): `rb-bridge
// --import-path`, started by the --mount-path run through systemd, at idle CPU and I/O priority
// so a set playing from the same stick keeps its audio. The job builds the tracks' performance
// data into the stage on /data (bridge v2, FillInBackground) and asks for a remount when a window
// has something to apply. The edisksd hook stops the job under the same unit name before it
// unmounts the volume.

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

// importSettings is the deck's settings file (--ui-settings): the job builds the lists a window
// would, under the folder names of the deck's language.
var importSettings string

// importLock lets one job run at a time, whatever the number of sticks.
var importLock = "/run/enginex-rb-import.lock"

// runImportJob is --import-path: wait for the visible mount, fill the stage, then ask for the
// remount that publishes it and show that until the volume is ejected.
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
	if errors.Is(err, rbstate.ErrNoSnapshot) {
		// No snapshot: no window converted this stick (it has no export, or the window was
		// stopped before it saved one). The next mount starts again.
		log.Printf("rb-bridge: nothing to fill on %s: %v", mount, err)
		rep.Done()
		return nil
	}
	if err != nil && !(errors.Is(err, bridge.ErrFilesUnreadable) && r.Publish) {
		return err // rep.Fail kept the error for the browser; the next import's Start clears it
	}
	if !r.Publish {
		rep.Done()
		return nil
	}
	defer rep.Done()
	// The remount helper refreshes publication status after this handoff.
	// Published by a remount (#301); the hook stops this job when the remount unmounts the stick.
	log.Printf("rb-bridge: the stage of %s holds what a window applies; a remount publishes it", mount)
	rep.Publishing()
	if err = startRemount(mount); err != nil {
		log.Printf("rb-bridge: %v", err)
		rep.FailPublication(err)
	}
	<-ctx.Done()
	return nil
}

// convertLocked runs the fill under the global lock and releases it as soon as the fill ends:
// a finished stick waiting for its remount never holds another's fill. started is false when it
// never got the lock (the stick went first).
func convertLocked(ctx context.Context, mount string, rep *usbLoadingReporter) (r bridge.FillResult, started bool, err error) {
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
	pokeStreamSyncUI()
	convert, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); refresh(convert, rep) }()
	defer func() { stop(); <-done }()
	// Throttled: the job reports its count as each track goes (#328); refresh writes the latest.
	r, err = bridge.FillInBackground(ctx, mount, bridge.FillOptions{Settings: importSettings, Progress: func(done, total int) { rep.Report(done, total, false) }})
	if err != nil && ctx.Err() == nil && !errors.Is(err, rbstate.ErrNoSnapshot) {
		rep.Fail(err)
	}
	return r, true, err
}

// pokeStreamSyncUI nudges stream-sync to refresh rb-deck-activity.json right when
// --import-path creates /run/enginex-usb-preparing, eliminating the 3 s UI poll blind spot
// if a deck on a 2-deck player was already playing before the stick was inserted.
var pokeStreamSyncUI = func() {
	if testing.Testing() {
		return
	}
	c := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := c.Get("http://127.0.0.1:8080/v1/enginex/ui")
	if err == nil {
		resp.Body.Close()
	}
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
