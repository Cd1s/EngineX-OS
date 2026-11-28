package bridge

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// The four interleavings of the Go review R12 of #336, each as a regression test. They state what
// must hold whatever the fix: a write another program was told is committed is in the library
// afterwards, and a publication that reports success published what it says.

const (
	r13WriterEnv     = "ENGINEX_R13_WRITER"
	r13WriterModeEnv = "ENGINEX_R13_WRITER_MODE"
	r13WindowEnv     = "ENGINEX_R13_WINDOW"
	r13DJTrack       = `INSERT INTO Track(path,filename,title) VALUES('../Music/dj-live.mp3','dj-live.mp3','DJ Live Track')`
)

// TestR13WriterChild is a plain SQLite writer, a program that does not know the volume's lock
// (Engine, or any tool): it adds a track, durably, and holds its next transaction until killed.
func TestR13WriterChild(t *testing.T) {
	path := os.Getenv(r13WriterEnv)
	if path == "" {
		t.Skip("a child of the R13 tests")
	}
	fail := func(step string, err error) {
		fmt.Printf("R13-WRITER REFUSED %s: %v\n", step, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(300)")
	if err != nil {
		fail("open", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if os.Getenv(r13WriterModeEnv) == "wal" {
		var mode string
		if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			fail("journal_mode=WAL", err)
			return
		}
	}
	for _, q := range []string{"PRAGMA synchronous=FULL", r13DJTrack, "BEGIN IMMEDIATE"} {
		if _, err = db.Exec(q); err != nil {
			fail(q, err)
			return
		}
	}
	fmt.Println("R13-WRITER COMMITTED")
	time.Sleep(90 * time.Second) // its own deadline: the test kills it long before
}

// r13Writer runs the writer on db and reports whether it was told its row is committed. stop
// kills it with its connection open, as a pulled plug would.
func r13Writer(t *testing.T, db, mode string) (committed bool, stop func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestR13WriterChild$", "-test.v")
	cmd.Env = append(os.Environ(), r13WriterEnv+"="+db, r13WriterModeEnv+"="+mode)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop = func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cancel()
	}
	said := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "R13-WRITER ") {
				said <- sc.Text()
				return
			}
		}
		said <- "R13-WRITER ended without a word"
	}()
	select {
	case line := <-said:
		t.Logf("writer: %s", line)
		return line == "R13-WRITER COMMITTED", stop
	case <-ctx.Done():
		stop()
		t.Fatal("the writer said nothing")
		return false, stop
	}
}

func r13Integrity(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&s); err != nil {
		return "integrity_check failed: " + err.Error()
	}
	return s
}

func r13Count(t *testing.T, path, q string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow(q).Scan(&n); err != nil {
		t.Errorf("%s: %v", q, err)
		return -1
	}
	return n
}

// r13Skeleton puts Engine's empty library on the stick: no track, no list.
func r13Skeleton(t *testing.T, mount string) (live string) {
	t.Helper()
	path, _, db := testlibrary.Create(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	live = filepath.Join(dbDir(mount), "m.db")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, live); err != nil {
		t.Fatal(err)
	}
	return live
}

// Finding 1, swap of the next library. At the sidecar check another program opens the library,
// switches it to WAL and commits. Unstopped, the swap renames the next library in under that
// program's -wal, and SQLite replays the old file's pages into the new one.
func TestR13SwapUnderAWriterAtTheSidecarCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		journal bool
	}{{"no sidecar at the check", false}, {"a stale journal that the writer takes away", true}} {
		t.Run(tc.name, func(t *testing.T) {
			mount := nextStick(t, 4)
			dir := dbDir(mount)
			live := filepath.Join(dir, "m.db")
			makeNextOf(t, mount, func(v *View) {
				v.Tracks[0].Track.Title = "bridge-next-title"
				v.Tracks[0].Hash = "bridge-next-hash"
			})
			if tc.journal { // SQLite's committed PERSIST journal: all zeros, safe to replace
				if err := os.WriteFile(live+"-journal", make([]byte, 512), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var committed bool
			var stop func()
			defer func() { sidecarsChecked = nil }()
			sidecarsChecked = func(db string) {
				if db == live && stop == nil {
					committed, stop = r13Writer(t, live, "wal")
				}
			}
			unlock, err := rbstate.LockVolume(mount, 0)
			if err != nil {
				t.Fatal(err)
			}
			swapped, _ := swapNext(context.Background(), mount, dir, newSteps(MountOptions{}), false, "")
			unlock()
			if stop == nil {
				t.Fatal("the sidecar check was never reached")
			}
			stop()
			if got := r13Integrity(t, live); got != "ok" {
				t.Fatalf("the library is corrupt after the swap (swapped=%v, writer committed=%v): %s", swapped, committed, got)
			}
			dj := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='DJ Live Track'`)
			next := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='bridge-next-title'`)
			if committed && dj != 1 {
				t.Errorf("the writer's committed track is gone (swapped=%v)", swapped)
			}
			if swapped && next != 1 {
				t.Errorf("the swap reported success but the library is not the next one (next title rows=%d)", next)
			}
			if !swapped && !committed {
				t.Error("neither the swap nor the writer went through")
			}
		})
	}
}

// Finding 1, first library. The same writer at the sidecar check of a first import over
// Engine's empty library. Unstopped, the import reports its tracks added and they are gone.
func TestR13FirstImportUnderAWriterAtTheSidecarCheck(t *testing.T) {
	const tracks = 4
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, tracks)
	live := r13Skeleton(t, mount)
	var committed bool
	var stop func()
	defer func() { sidecarsChecked = nil }()
	sidecarsChecked = func(db string) {
		if db == live && stop == nil {
			committed, stop = r13Writer(t, live, "wal")
		}
	}
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	if stop == nil {
		t.Fatalf("the sidecar check was never reached: %+v %v", res, err)
	}
	stop()
	if got := r13Integrity(t, live); got != "ok" {
		t.Fatalf("the library is corrupt after the first import (added=%d err=%v, writer committed=%v): %s", res.Added, err, committed, got)
	}
	dj := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='DJ Live Track'`)
	imported := r13Count(t, live, `SELECT count(*) FROM Track WHERE title IS NOT 'DJ Live Track'`)
	if committed && dj != 1 {
		t.Errorf("the writer's committed track is gone (added=%d err=%v)", res.Added, err)
	}
	if err == nil && imported != res.Added {
		t.Errorf("the import reported %d tracks added, the library has %d of them", res.Added, imported)
	}
	if err != nil && !committed {
		t.Errorf("neither the import nor the writer went through: %v", err)
	}
}

// Finding 2. The background job starts its build over while a window is swapping the next
// library in. Unstopped, it removes the swap snapshot and empties the very file the window
// renames over the stick's library: the DJ's own tracks are gone and the window reports success.
func TestR13BackgroundBuildWaitsForAWindowsSwap(t *testing.T) {
	mount := nextStick(t, 4)
	dir := dbDir(mount)
	live := filepath.Join(dir, "m.db")
	engineWrites(t, mount, `INSERT INTO Track(path,filename,title) VALUES('../Music/dj-native.mp3','dj-native.mp3','DJ Native Track')`)
	makeNext(t, mount) // the next library, ready: it has the DJ's track too
	r, err := newResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := rbstate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := readJobView(context.Background(), r, mount, snap.Layout, "")
	if err != nil {
		t.Fatal(err)
	}
	// This deck's job has no record of that next library: the stick comes from another deck, or
	// the stage on /data was cleaned. Its build then starts the next library from nothing.
	if err = os.RemoveAll(nextState(snap.LibraryUUID)); err != nil {
		t.Fatal(err)
	}
	inBuild, goOn, built := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	job := &nextBuild{ctx: context.Background(), mount: mount, dir: dir, view: v.view, live: snap,
		pace: func(time.Duration) error {
			if _, e := os.Stat(filepath.Join(dir, nextDB)); e == nil { // its new, still empty next library is on the stick
				once.Do(func() { close(inBuild); <-goOn })
			}
			return nil
		}}
	prevCut := cut
	defer func() { cut = prevCut }()
	started, waited := false, false
	cut = func(step string) {
		if step != "before swap" || started {
			return
		}
		started = true
		go func() { built <- job.build(false, true) }()
		select {
		case <-inBuild: // the build is at work on the next library while the window swaps it
		case err := <-built:
			built <- err
		case <-time.After(1500 * time.Millisecond):
			waited = true // it waits for the window's lock
		}
	}
	// The window of the job's remount: it holds the volume's lock and swaps the next library in.
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	if !started {
		t.Fatalf("the window never reached its swap: %+v %v", res, err)
	}
	// What the window left, before the job goes on.
	check := func(when string) {
		t.Helper()
		if got := r13Integrity(t, live); got != "ok" {
			t.Errorf("%s: the library is not sound (the build waited for the window=%v): %s", when, waited, got)
			return
		}
		if dj := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='DJ Native Track'`); dj != 1 {
			t.Errorf("%s: the DJ's own track is gone: %d rows (window: %+v err=%v; the build waited for the window=%v)", when, dj, res, err, waited)
		}
		if n := r13Count(t, live, `SELECT count(*) FROM Track`); n != 5 {
			t.Errorf("%s: the library has %d tracks, want the stick's 4 and the DJ's 1", when, n)
		}
	}
	check("when the window returned")
	if err != nil {
		t.Errorf("the window: %v", err)
	}
	close(goOn)
	select {
	case err := <-built:
		if err != nil {
			t.Errorf("the job's build: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the job's build did not end")
	}
	check("when the job's build had ended")
	if _, err := rbstate.ReadHeader(dir); err != nil {
		t.Errorf("the stick's library has no snapshot: %v", err)
	}
}

// Finding 3, queued edits. engine-guardian (or a window's chore) applies the Web Console's queued
// edits while a window is swapping. Unstopped, the edit goes into the file that is about to be
// replaced, the queue is deleted, and the edit is gone for good.
func TestR13PendingEditsSurviveAWindowsSwap(t *testing.T) {
	mount := nextStick(t, 4)
	dir := dbDir(mount)
	live := filepath.Join(dir, "m.db")
	makeNext(t, mount)
	id, err := strconv.ParseInt(query(t, mount, `SELECT id FROM Track ORDER BY id LIMIT 1`)[0][0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(dir, enginedb.PendingFile)
	b, err := json.Marshal(enginedb.Pending{TrackMetadata: []enginedb.PendingTrackMetadata{{TrackID: id, Comment: "web-console-queued-edit"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(queue, b, 0o644); err != nil {
		t.Fatal(err)
	}
	prevCut := cut
	defer func() { cut = prevCut }()
	ran, applied := false, 0
	var applyErr error
	cut = func(step string) {
		if step == "before swap" && !ran {
			ran = true
			applied, applyErr = enginedb.ApplyPending(context.Background(), mount)
		}
	}
	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	swapped, _ := swapNext(context.Background(), mount, dir, newSteps(MountOptions{}), false, "")
	unlock()
	if !ran {
		t.Fatal("the swap never reached its rename")
	}
	// What was not applied then is applied by the next run, as the guardian does at the next mount.
	later, laterErr := enginedb.ApplyPending(context.Background(), mount)
	_, queueErr := os.Stat(queue)
	if got := r13Count(t, live, `SELECT count(*) FROM Track WHERE comment='web-console-queued-edit'`); got != 1 {
		t.Errorf("the queued edit is lost (swapped=%v). During the swap: applied=%d err=%v. After it: applied=%d err=%v. Queue file: %v",
			swapped, applied, applyErr, later, laterErr, queueErr)
	}
	if !swapped {
		t.Error("the window did not swap the ready next library in")
	}
}

// Finding 3, first import. A program adds a track to Engine's empty library while the first
// library is copied to the stick, which takes seconds on a real one. Unstopped, the rename
// replaces the file with the track in it and the window reports its import.
func TestR13FirstImportKeepsWhatWasWrittenDuringItsCopy(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 4)
	live := r13Skeleton(t, mount)
	ran := false
	restore := rbstate.SetCutHooks(func(step string) {
		if step == "temp "+live && !ran { // the copy is whole, its rename is next
			ran = true
			engineWrites(t, mount, r13DJTrack)
		}
	}, nil)
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	restore()
	if !ran {
		t.Fatalf("the first import never reached its rename: %+v %v", res, err)
	}
	if got := r13Integrity(t, live); got != "ok" {
		t.Fatalf("the library is corrupt: %s", got)
	}
	if dj := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='DJ Live Track'`); dj != 1 {
		t.Errorf("the track written during the copy is gone; the window reported added=%d err=%v", res.Added, err)
	}
	if err == nil {
		t.Errorf("the window reported an import (added=%d) over a library that had changed", res.Added)
	}
	// The next insert takes the stick as it is now: the DJ's track stays, the export's are added.
	if _, err = ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatalf("the next window: %v", err)
	}
	if dj := r13Count(t, live, `SELECT count(*) FROM Track WHERE title='DJ Live Track'`); dj != 1 {
		t.Errorf("the next window lost the DJ's track")
	}
	if n := r13Count(t, live, `SELECT count(*) FROM Track`); n != 5 {
		t.Errorf("after the next window the library has %d tracks, want the export's 4 and the DJ's 1", n)
	}
}

// TestR13WindowChild is a mount window in another process.
func TestR13WindowChild(t *testing.T) {
	mount := os.Getenv(r13WindowEnv)
	if mount == "" {
		t.Skip("a child of the R13 tests")
	}
	fmt.Println("R13-WINDOW START")
	_, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	fmt.Printf("R13-WINDOW END locked=%v err=%v\n", errors.Is(err, rbstate.ErrLocked), err)
}

type r13Buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *r13Buffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *r13Buffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Finding 4. Two windows start on a stick without a library (Gen-2's start-up run beside the
// hook's). The second one does not get the volume's lock. Unstopped, its cleanup of "a first
// import that failed" removes the next snapshot of the first window, whose import then has no
// snapshot: the next insert takes every track for Engine's own and doubles the lists.
func TestR13ARefusedWindowLeavesTheFirstImportAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 4)
	dir := dbDir(mount)
	live := filepath.Join(dir, "m.db")

	// The second window starts while the volume is taken, and gets as far as the lock.
	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	second := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestR13WindowChild$", "-test.v")
	second.Env = append(os.Environ(), r13WindowEnv+"="+mount)
	second.WaitDelay = 5 * time.Second
	out := &r13Buffer{}
	second.Stdout, second.Stderr = out, out
	if err = second.Start(); err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Wait() }()
	defer func() {
		_ = syscall.Kill(second.Process.Pid, syscall.SIGCONT)
		_ = second.Process.Kill()
	}()
	wait := func(what string, limit time.Duration, ok func() bool) bool {
		for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if ok() {
				return true
			}
			select {
			case err := <-secondDone:
				t.Fatalf("the second window ended while the test waited for %s: %v\n%s", what, err, out.String())
			default:
			}
		}
		return false
	}
	if !wait("its start", time.Minute, func() bool { return strings.Contains(out.String(), "R13-WINDOW START") }) {
		t.Fatalf("the second window did not start\n%s", out.String())
	}
	// It has looked at the stick when it has made the library's folder (the order before the fix),
	// or it stands at the lock without having looked (the order after it).
	wait("its look at the stick", 1500*time.Millisecond, func() bool { _, e := os.Stat(dir); return e == nil })
	time.Sleep(100 * time.Millisecond)
	if err = syscall.Kill(second.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	unlock()
	unlocked = true

	// The first window: stopped when its library is copied and its next snapshot saved, before its rename.
	atRename, goOn := make(chan struct{}), make(chan struct{})
	var once, release sync.Once
	letGo := func() { release.Do(func() { close(goOn) }) }
	defer letGo()
	restore := rbstate.SetCutHooks(func(step string) {
		if step == "temp "+live {
			once.Do(func() { close(atRename); <-goOn })
		}
	}, nil)
	defer restore()
	type result struct {
		res MountResult
		err error
	}
	first := make(chan result, 1)
	go func() {
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
		first <- result{res, err}
	}()
	select {
	case <-atRename:
	case r := <-first:
		t.Fatalf("the first window ended before its rename: %+v %v", r.res, r.err)
	case <-ctx.Done():
		t.Fatal("the first window did not reach its rename")
	}
	if err = syscall.Kill(second.Process.Pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondDone:
	case <-ctx.Done():
		t.Fatalf("the second window did not end\n%s", out.String())
	}
	if !strings.Contains(out.String(), "R13-WINDOW END locked=true") {
		t.Fatalf("the second window was not refused at the lock:\n%s", out.String())
	}
	letGo()
	var r result
	select {
	case r = <-first:
	case <-ctx.Done():
		t.Fatal("the first window did not end")
	}
	restore()
	if r.err != nil {
		t.Errorf("the first window failed after the second one was refused: %v", r.err)
	}
	if _, err = rbstate.ReadHeader(dir); err != nil {
		t.Errorf("the first import has no snapshot: %v", err)
	}
	tracks := r13Count(t, live, `SELECT count(*) FROM Track`)
	lists := r13Count(t, live, `SELECT count(*) FROM Playlist`)
	// The next insert finds its own import: nothing is added, no list is doubled.
	if _, err = ConvertAtMount(context.Background(), mount, MountOptions{}); err != nil {
		t.Fatalf("the next window: %v", err)
	}
	tracks2, lists2 := r13Count(t, live, `SELECT count(*) FROM Track`), r13Count(t, live, `SELECT count(*) FROM Playlist`)
	if tracks != 4 || tracks2 != 4 || lists2 != lists {
		t.Errorf("after the first import: %d tracks, %d lists; after the next insert: %d tracks, %d lists; want 4 tracks and the same lists both times",
			tracks, lists, tracks2, lists2)
	}
	if snap, lerr := rbstate.Load(dir); lerr != nil || len(snap.Tracks) != 4 {
		t.Errorf("the bridge does not own its 4 tracks after the next insert: %v", lerr)
	}
}
