package bridge

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestCueWatcherForgetWaitsForCancelledPass(t *testing.T) {
	done := make(chan struct{})
	cancelled := make(chan struct{})
	w := &CueWatcher{sticks: map[string]*cueStick{
		"/mount": {running: done, cancel: func() { close(cancelled) }},
	}}
	returned := make(chan struct{})
	go func() {
		w.forget(nil)
		close(returned)
	}()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forget did not cancel the running pass")
	}
	select {
	case <-returned:
		t.Fatal("forget cleared the session before the cancelled pass ended")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("forget did not finish after the running pass ended")
	}
}

func TestCueSessionSkipsSnapshotParseAndTrackScanOnUntouchedLoads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedRealRekordboxOneLibraryStick(t, root)
	dir := filepath.Join(root, "Engine Library", "Database2")
	bakDir := filepath.Join(root, ".enginex-backup", "2026-11-26")
	if err := os.MkdirAll(bakDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sess := &CueSession{keys: newKeyCache()}
	if _, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: sess}); err != nil {
		t.Fatalf("warm #1 WriteBackCues: %v", err)
	}
	// Pass #2 warms CueSession's snapshot + backup cache on the settled snapshot.
	if _, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: sess}); err != nil {
		t.Fatalf("warm #2 WriteBackCues: %v", err)
	}

	var snapLoads, filledReads, backupWalks, readLibsCalls atomic.Int32
	prevSnap := writeBackSnapshotLoadHook
	prevFill := writeBackFilledReadHook
	prevBak := writeBackBackupWalkHook
	prevLibs := writeBackReadLibsHook
	writeBackSnapshotLoadHook = func() { snapLoads.Add(1) }
	writeBackFilledReadHook = func() { filledReads.Add(1) }
	writeBackBackupWalkHook = func() { backupWalks.Add(1) }
	writeBackReadLibsHook = func() { readLibsCalls.Add(1) }
	defer func() {
		writeBackSnapshotLoadHook = prevSnap
		writeBackFilledReadHook = prevFill
		writeBackBackupWalkHook = prevBak
		writeBackReadLibsHook = prevLibs
	}()

	// Touch m.db mtime (like an untouched track load) so a watcher pass runs without changing lastEditTime.
	mdb := filepath.Join(dir, "m.db")
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(mdb, now, now); err != nil {
		t.Fatal(err)
	}
	res, err := WriteBackCues(ctx, root, WriteBackOptions{SkipPreparingCheck: true, Session: sess})
	if err != nil {
		t.Fatalf("untouched pass with CueSession: %v", err)
	}
	if res.TracksRead != 0 || res.TracksChanged != 0 {
		t.Fatalf("untouched pass read/changed tracks: %+v", res)
	}
	if snapLoads.Load() != 0 || filledReads.Load() != 0 || backupWalks.Load() != 0 || readLibsCalls.Load() != 0 {
		t.Fatalf("session re-did idle work: snapLoads=%d filledReads=%d backupWalks=%d readLibs=%d",
			snapLoads.Load(), filledReads.Load(), backupWalks.Load(), readLibsCalls.Load())
	}
}

func TestCueWatcherEventDrivenZeroIdlePolls(t *testing.T) {
	base := t.TempDir()
	stockMount := filepath.Join(base, "ENGINESTOCK")
	makeStockEngineStick(t, stockMount)
	rbMount := filepath.Join(base, "RBSTICK")
	testfixture.LargeExport(t, rbMount, 1)

	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	evCh := make(chan struct{}, 4)
	var mountList atomic.Value
	mountList.Store([]string{stockMount}) // start with 0 rekordbox sticks

	var mountsCalls atomic.Int32
	w := &CueWatcher{
		quiet:         25 * time.Millisecond,
		retry:         50 * time.Millisecond,
		poll:          20 * time.Millisecond,
		ejectDir:      cueEjectingDir,
		useMountWatch: true,
		mountEvents:   evCh,
		mounts: func() []string {
			mountsCalls.Add(1)
			return mountList.Load().([]string)
		},
		pass: WriteBackCues,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()

	// Wait through 12x the legacy poll interval with 0 rekordbox sticks -> must stay at 1 initial scan!
	time.Sleep(240 * time.Millisecond)
	if got := mountsCalls.Load(); got != 1 {
		cancel()
		<-done
		t.Fatalf("mounts() called %d times during idle with 0 rekordbox sticks, want 1", got)
	}

	// Simulate mounting a rekordbox stick via mount event -> attaches immediately, ticker starts.
	mountList.Store([]string{stockMount, rbMount})
	evCh <- struct{}{}
	if !WaitWatchingForTest(w, rbMount, time.Second) {
		cancel()
		<-done
		t.Fatal("CueWatcher did not attach rbMount on mount event")
	}

	// Simulate unmounting rbMount -> detaches, returns to 0 idle polls!
	mountList.Store([]string{stockMount})
	evCh <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && w.hasSticks() {
		time.Sleep(5 * time.Millisecond)
	}
	if w.hasSticks() {
		cancel()
		<-done
		t.Fatal("CueWatcher did not forget rbMount after unmount event")
	}
	afterUnmount := mountsCalls.Load()
	time.Sleep(240 * time.Millisecond)
	if got := mountsCalls.Load(); got != afterUnmount {
		cancel()
		<-done
		t.Fatalf("mounts() polled %d extra times after unmount returned to 0 rekordbox sticks", got-afterUnmount)
	}
	cancel()
	<-done
}
