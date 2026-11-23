//go:build linux

package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// #321 review R2 (P3): the watcher removes what a cut pass left when a stick is mounted. That ran
// inside its loop, so a stick whose files hang at insert (here a leftover that blocks its open, as
// a stalled stick blocks I/O) held the watcher of every other stick too. It runs beside the loop
// now, and only that stick's own passes wait for it.
func TestIssue321AStickThatHangsAtInsertHoldsOnlyItself(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	hung, _ := issue321Stick(t, 4)
	stick, db := issue321Stick(t, 4)
	fifo := filepath.Join(hung, "Engine Library", "Database2", ".enginex-wb-9.tmp")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	release := func() { // the stalled open returns
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	}
	t.Cleanup(bridge.SetCueRunDirForTest(t.TempDir()))
	w := bridge.CueWatcherForTest([]string{hung, stick}, 300*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() {
		for {
			release()
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
				cancel()
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	issue321Edit(t, db, 2, "Beside a hung stick", 1000)
	issue321WaitANLZ(t, stick, "Beside a hung stick", 4*time.Second)
}
