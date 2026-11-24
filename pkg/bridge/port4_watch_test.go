package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func p4StartWatcher(t *testing.T, mount string, quiet time.Duration) (*CueWatcher, *atomic.Int32, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	w := CueWatcherForTest([]string{mount}, quiet, 500*time.Millisecond)
	var passes atomic.Int32
	WrapCuePassForTest(w, func(next CuePass) CuePass {
		return func(ctx context.Context, m string, o WriteBackOptions) (WriteBackResult, error) {
			passes.Add(1)
			return next(ctx, m, o)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	time.Sleep(120 * time.Millisecond)
	return w, &passes, cancel, done
}

func p4WaitCue(t *testing.T, mount, name string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if p4AnalysisHas(t, mount, name) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cue %q was not written within %s", name, within)
}

func TestPort4WatcherWaitsForQuietAndDoesNotRepoll(t *testing.T) {
	mount := p4ImportedStick(t)
	_, passes, cancel, done := p4StartWatcher(t, mount, 300*time.Millisecond)
	defer func() { cancel(); <-done }()
	p4DeckEdit(t, mount, "quiet cue", "", "", "")
	p4WaitCue(t, mount, "quiet cue", 3*time.Second)
	if got := passes.Load(); got != 1 {
		t.Fatalf("passes for one edit burst=%d, want 1", got)
	}
	time.Sleep(700 * time.Millisecond)
	if got := passes.Load(); got != 1 {
		t.Fatalf("watcher repolled unchanged m.db: %d passes", got)
	}
}

func TestPort4WatcherDefersConversionAndEjectToTheirPasses(t *testing.T) {
	mount := p4ImportedStick(t)
	run := t.TempDir()
	t.Cleanup(SetCueRunDirForTest(run))
	preparing := filepath.Join(run, "enginex-usb-preparing")
	if err := os.WriteFile(preparing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := CueWatcherForTest([]string{mount}, 250*time.Millisecond, 250*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(120 * time.Millisecond)
	p4DeckEdit(t, mount, "after conversion", "", "", "")
	time.Sleep(700 * time.Millisecond)
	if p4AnalysisHas(t, mount, "after conversion") {
		t.Fatal("watcher wrote while conversion was flagged")
	}
	if err := os.Remove(preparing); err != nil {
		t.Fatal(err)
	}
	p4WaitCue(t, mount, "after conversion", 3*time.Second)
	for _, name := range []string{EjectingNameForTest(mount), EjectedNameForTest(mount)} {
		if err := os.WriteFile(filepath.Join(run, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p4DeckEdit(t, mount, "eject cue", "", "", "")
	time.Sleep(700 * time.Millisecond)
	if p4AnalysisHas(t, mount, "eject cue") {
		t.Fatal("watcher wrote while eject was flagged")
	}
	res, err := w.Final(context.Background(), mount)
	if err != nil || res.TracksChanged != 1 || !p4AnalysisHas(t, mount, "eject cue") {
		t.Fatalf("final pass: %+v %v", res, err)
	}
}

func TestPort4WatcherDropsOverlappingReadAndWritesLatestEdit(t *testing.T) {
	mount := p4ImportedStick(t)
	t.Cleanup(SetCueRunDirForTest(t.TempDir()))
	w := CueWatcherForTest([]string{mount}, 250*time.Millisecond, 250*time.Millisecond)
	var mu sync.Mutex
	var errs []error
	var once sync.Once
	WrapCuePassForTest(w, func(next CuePass) CuePass {
		return func(ctx context.Context, m string, o WriteBackOptions) (WriteBackResult, error) {
			settled := o.Settled
			o.Settled = func() error {
				once.Do(func() { p4DeckEdit(t, mount, "during read", "", "", "") })
				return settled()
			}
			res, err := next(ctx, m, o)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
			return res, err
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(120 * time.Millisecond)
	p4DeckEdit(t, mount, "before read", "", "", "")
	p4WaitCue(t, mount, "during read", 4*time.Second)
	// The cue is on the stick at its rename; the pass returns (and is recorded) after it notes the
	// snapshot, which under -race and load can take longer than a poll.
	var gotErrs []error
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		gotErrs = append([]error(nil), errs...)
		mu.Unlock()
		if len(gotErrs) >= 2 && gotErrs[len(gotErrs)-1] == nil || time.Now().After(deadline) {
			break
		}
	}
	if len(gotErrs) < 2 || !errors.Is(gotErrs[0], ErrNotQuietForTest) || gotErrs[len(gotErrs)-1] != nil {
		t.Fatalf("passes=%v, want dropped first pass and successful retry", gotErrs)
	}
	if p4AnalysisHas(t, mount, "before read") {
		t.Fatal("stale cue from the overlapping read was written")
	}
}

func TestPort4WatcherCleansCutLeftoversOnAttach(t *testing.T) {
	mount := p4ImportedStick(t)
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	anlzDir := filepath.Join(mount, "PIONEER", "USBANLZ", "P001")
	if err := os.WriteFile(filepath.Join(anlzDir, ".enginex-wb-1.tmp"), []byte("cut"), 0o600); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(dbDir, writeBackMarkName)
	if err := os.WriteFile(mark, []byte("PIONEER/USBANLZ/P001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _, cancel, done := p4StartWatcher(t, mount, time.Second)
	_ = w
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(mark); os.IsNotExist(err) {
			if _, err := os.Stat(filepath.Join(anlzDir, ".enginex-wb-1.tmp")); os.IsNotExist(err) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watcher left cut-write leftovers on attach")
}
