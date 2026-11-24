package bridge_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321: stream-sync's watcher runs the write-back a quiet time after Engine's last write of m.db.

func issue321Watch(t *testing.T, stick string, quiet, retry time.Duration) (*bridge.CueWatcher, *atomic.Int32, func()) {
	t.Helper()
	t.Cleanup(bridge.SetCueRunDirForTest(t.TempDir()))
	w := bridge.CueWatcherForTest([]string{stick}, quiet, retry)
	var passes atomic.Int32
	bridge.WrapCuePassForTest(w, func(next bridge.CuePass) bridge.CuePass {
		return func(ctx context.Context, m string, o bridge.WriteBackOptions) (bridge.WriteBackResult, error) {
			passes.Add(1)
			o.DateDir = "2026-11-22"
			return next(ctx, m, o)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // attached
	return w, &passes, func() { cancel(); <-done }
}

func issue321WaitANLZ(t *testing.T, stick, name string, within time.Duration) time.Duration {
	t.Helper()
	t0 := time.Now()
	for time.Since(t0) < within {
		if a, err := rekordbox.ReadAnalysis(filepath.Join(stick, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT")); err == nil {
			for _, c := range a.Cues {
				if c.Name == name {
					return time.Since(t0)
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%q not on the stick after %s", name, within)
	return 0
}

func TestIssue321WatcherWritesAQuietTimeAfterEnginesLastWrite(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick, db := issue321Stick(t, 8)
	_, passes, stop := issue321Watch(t, stick, 600*time.Millisecond, time.Second)
	defer stop()
	issue321Edit(t, db, 4, "Auto 321", 1000)
	// Engine writes again before the quiet time is over: the pass waits for the last write.
	time.Sleep(300 * time.Millisecond)
	if _, err := db.Exec(`UPDATE Track SET rating=60 WHERE pdbImportKey=6`); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	took := issue321WaitANLZ(t, stick, "Auto 321", 5*time.Second)
	if took < 500*time.Millisecond {
		t.Fatalf("written %s after Engine's last write, before the quiet time", took)
	}
	if n := passes.Load(); n != 1 {
		t.Fatalf("%d passes for one burst of writes, want 1", n)
	}
	_ = t0
	// Nothing changes: no pass.
	time.Sleep(1200 * time.Millisecond)
	if n := passes.Load(); n != 1 {
		t.Fatalf("%d passes without a write of m.db", n)
	}
}

// Engine writes m.db while the pass reads it: nothing the read found is kept or written, and the
// pass that follows the write writes what Engine wrote last.
func TestIssue321WatcherDropsAReadThatOverlapsAWrite(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick, db := issue321Stick(t, 8)
	t.Cleanup(bridge.SetCueRunDirForTest(t.TempDir()))
	w := bridge.CueWatcherForTest([]string{stick}, 400*time.Millisecond, time.Second)
	var mu sync.Mutex
	var errs []error
	var once sync.Once
	bridge.WrapCuePassForTest(w, func(next bridge.CuePass) bridge.CuePass {
		return func(ctx context.Context, m string, o bridge.WriteBackOptions) (bridge.WriteBackResult, error) {
			settled := o.Settled
			o.Settled = func() error {
				once.Do(func() { issue321Edit(t, db, 4, "During the read", 1500) })
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
	time.Sleep(100 * time.Millisecond)
	issue321Edit(t, db, 4, "Before the read", 1000)
	issue321WaitANLZ(t, stick, "During the read", 5*time.Second)
	for t0 := time.Now(); time.Since(t0) < 3*time.Second; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n >= 2 {
			break // the pass that wrote it has returned
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if issue321ANLZHas(t, stick, "Before the read") || len(errs) < 2 || !errors.Is(errs[0], bridge.ErrNotQuietForTest) || errs[len(errs)-1] != nil {
		t.Fatalf("passes: %v (want the first dropped, the last written)", errs)
	}
}

// No pass while the hook converts a stick at mount (/run/enginex-usb-preparing) or while an
// Eject is under way (its mark): the conversion's end is retried, the Eject's final pass writes.
func TestIssue321WatcherWaitsForAConversionAndLeavesAnEjectToItsPass(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick, db := issue321Stick(t, 8)
	run := t.TempDir()
	t.Cleanup(bridge.SetCueRunDirForTest(run))
	flag := filepath.Join(run, "enginex-usb-preparing")
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := bridge.CueWatcherForTest([]string{stick}, 300*time.Millisecond, 500*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(100 * time.Millisecond)
	issue321Edit(t, db, 4, "After the conversion", 1000)
	time.Sleep(1500 * time.Millisecond)
	if issue321ANLZHas(t, stick, "After the conversion") {
		t.Fatal("written while the stick was being converted")
	}
	os.Remove(flag)
	issue321WaitANLZ(t, stick, "After the conversion", 3*time.Second)

	// The hook's marks of an Eject, both made at its start.
	for _, mark := range []string{bridge.EjectingNameForTest(stick), bridge.EjectedNameForTest(stick)} {
		if err := os.WriteFile(filepath.Join(run, mark), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for s := time.Now().Unix(); time.Now().Unix() == s; {
		time.Sleep(20 * time.Millisecond)
	}
	issue321Edit(t, db, 4, "At the Eject", 2000)
	time.Sleep(1200 * time.Millisecond)
	if issue321ANLZHas(t, stick, "At the Eject") {
		t.Fatal("the watcher wrote while the stick was being ejected")
	}
	res, err := w.Final(context.Background(), stick)
	if err != nil || res.TracksChanged != 1 || !issue321ANLZHas(t, stick, "At the Eject") {
		t.Fatalf("final pass: %+v %v", res, err)
	}
}

// The Eject's final pass uses the keys the watcher derived for the stick: no derivation inside
// the hook's 12 s; and it stops a pass in progress first.
func TestIssue321FinalPassReusesTheWatchersKeys(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for s := time.Now().Unix(); time.Now().Unix() == s; {
		time.Sleep(20 * time.Millisecond)
	}
	w, _, stop := issue321Watch(t, stick, 300*time.Millisecond, time.Second)
	defer stop()
	issue321Edit(t, db, 1, "Watched", 1000)
	issue321WaitANLZ(t, stick, "Watched", 5*time.Second)
	for s := time.Now().Unix(); time.Now().Unix() == s; {
		time.Sleep(20 * time.Millisecond)
	}
	issue321Edit(t, db, 2, "Ejected", 1500)
	d0 := rekordbox.KeyDerivations()
	res, err := w.Final(ctx, stick) // inside the quiet time: no pass has started yet
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated || !issue321ANLZHas(t, stick, "Ejected") {
		t.Fatalf("final pass: %+v %v", res, err)
	}
	if n := rekordbox.KeyDerivations() - d0; n != 0 {
		t.Fatalf("the final pass derived the keys %d times", n)
	}
}

func TestIssue321AnalysisOnlyChangeSkipsExportDecrypt(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(context.Background(), stick); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE Track SET bpmAnalyzed=bpmAnalyzed+1 WHERE pdbImportKey=1`); err != nil {
		t.Fatal(err)
	}
	keys := rekordbox.NewKeyCache()
	defer keys.Clear()
	d0 := rekordbox.KeyDerivations()
	res, err := bridge.WriteBackCues(context.Background(), stick, bridge.WriteBackOptions{Keys: keys, SkipPreparingCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.TracksChanged != 0 || rekordbox.KeyDerivations()-d0 != 0 {
		t.Fatalf("analysis-only change caused export work: %+v derivations=%d", res, rekordbox.KeyDerivations()-d0)
	}
}

// What a cut write-back leaves goes at the next mount: the watcher's attach.
func TestIssue321LeftoversGoAtTheNextMount(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	stick, _ := issue321Stick(t, 4)
	dir := filepath.Join(stick, "Engine Library", "Database2")
	anlz := filepath.Join(stick, "PIONEER", "USBANLZ", "P001")
	for _, f := range []string{filepath.Join(anlz, ".enginex-wb-1.tmp"), filepath.Join(stick, "PIONEER", "rekordbox", ".enginex-onelib-2.tmp")} {
		if err := os.WriteFile(f, []byte("cut"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-writeback"), []byte("PIONEER/USBANLZ/P001\nPIONEER/rekordbox\n../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(stick), "outside") // a mark naming it is not followed
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(outside, ".enginex-wb-3.tmp")
	if err := os.WriteFile(keep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, stop := issue321Watch(t, stick, time.Second, time.Second)
	stop()
	left, _ := filepath.Glob(filepath.Join(stick, "PIONEER", "*", "*.tmp"))
	more, _ := filepath.Glob(filepath.Join(anlz, ".*.tmp"))
	if len(left)+len(more) != 0 {
		t.Fatalf("left after the mount: %v %v", left, more)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rb-bridge-writeback")); !os.IsNotExist(err) {
		t.Fatalf("mark kept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a file outside the stick was removed: %v", err)
	}
}
