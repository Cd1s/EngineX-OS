package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"golang.org/x/sys/unix"
)

// fakeStick is the world --remount sees: a stick mounted from /dev/sdz1, whose sysfs uevent
// file mounts and unmounts it as edisksd does on a remove and an add, and an Engine in a fake
// /proc. events lists what was written to uevent, with whether Engine was busy then.
type fakeStick struct {
	mount, uevent, proc string
	mu                  sync.Mutex
	events              []string
	mountedNow          bool
	unmountOnRemove     bool
	busy                func() string // what Engine does at a write: "", "copying" or "holding"
}

func newFakeStick(t *testing.T) *fakeStick {
	t.Helper()
	dir := t.TempDir()
	f := &fakeStick{mount: filepath.Join(dir, "media", "REKORDBOX"), mountedNow: true, unmountOnRemove: true, proc: filepath.Join(dir, "proc")}
	sys := filepath.Join(dir, "sys")
	f.uevent = filepath.Join(sys, "sdz1", "uevent")
	for _, d := range []string{f.mount, filepath.Dir(f.uevent), filepath.Join(f.proc, "77", "fd"), filepath.Join(f.proc, "77", "fdinfo")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.proc, "77", "comm"), []byte("Engine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stick's device number is that of a real file, so that Engine can "hold" it.
	held := filepath.Join(dir, "m.db")
	if err := os.WriteFile(held, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(held, &st); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(dir, "mountinfo")
	line := fmt.Sprintf("40 25 %d:%d / %s rw,relatime shared:1 - vfat /dev/sdz1 rw,utf8\n", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), f.mount)
	if err := os.WriteFile(info, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.uevent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	oldInfo, oldSys, oldProc, oldMark, oldMounted := mountinfoPath, sysClassBlock, procDir, ejectMarkDir, mounted
	oldWait, oldRemove, oldClose, oldRemount, oldQuiet := remountWait, removeLimit, closeLimit, remountLimit, serveQuiet
	oldDeck, oldCopies, oldOwn, oldPeers, oldPeerDecks := deckWait, engineCopies, ownDecks, enginePeers, peerDecks
	t.Cleanup(func() {
		mountinfoPath, sysClassBlock, procDir, ejectMarkDir, mounted = oldInfo, oldSys, oldProc, oldMark, oldMounted
		remountWait, removeLimit, closeLimit, remountLimit, serveQuiet = oldWait, oldRemove, oldClose, oldRemount, oldQuiet
		deckWait, engineCopies, ownDecks, enginePeers, peerDecks = oldDeck, oldCopies, oldOwn, oldPeers, oldPeerDecks
	})
	// stream-sync: this player's decks hold what Engine has open of the stick; no Engine Link peer.
	ownDecks = func() ([]string, bool) {
		var nets []string
		fds, _ := os.ReadDir(filepath.Join(f.proc, "77", "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(f.proc, "77", "fd", fd.Name())); err == nil && strings.HasPrefix(t, f.mount+"/") {
				nets = append(nets, "net://{1}/REKORDBOX"+strings.TrimPrefix(t, f.mount))
			}
		}
		return nets, true
	}
	enginePeers = func() []net.IP { return nil }
	peerDecks = func(net.IP) ([]string, bool) { return nil, false }
	mountinfoPath, sysClassBlock, procDir, ejectMarkDir = info, sys, f.proc, filepath.Join(dir, "run")
	remountWait, removeLimit, closeLimit, remountLimit, serveQuiet = 20*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond, 100*time.Millisecond
	deckWait, engineCopies = 20*time.Millisecond, filepath.Join(dir, "inMusicTempFiles")+"/"
	if err := os.MkdirAll(engineCopies, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ejectMarkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.busy = func() string { return "" }
	mounted = func(p string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := os.ReadFile(f.uevent)
		if w := string(b); w != "" {
			os.WriteFile(f.uevent, nil, 0o644)
			f.events = append(f.events, w+":"+f.busy())
			switch {
			case w == "remove" && f.unmountOnRemove:
				f.mountedNow = false
			case w == "add":
				f.mountedNow = true
			}
		}
		return p == f.mount && f.mountedNow
	}
	f.engine("5", held, "0100002")
	return f
}

// engine gives Engine an fd on target with the fdinfo flags; target "" closes it.
func (f *fakeStick) engine(fd, target, flags string) {
	link := filepath.Join(f.proc, "77", "fd", fd)
	os.Remove(link)
	if target == "" {
		return
	}
	os.Symlink(target, link)
	os.WriteFile(filepath.Join(f.proc, "77", "fdinfo", fd), []byte("pos:\t0\nflags:\t"+flags+"\nmnt_id:\t40\n"), 0o644)
}

func (f *fakeStick) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// Design 1.11 item 6: right before the remount the helper has the rows the deck wrote taken into
// a next library that waits, so that the remount's window only renames. Looking for them takes
// time, in which a deck can load a track of the stick: the decks are looked at again before the
// remove, and the rows once more when the decks are clear again.
func TestRemountTakesTheDecksRowsBeforeTheRemove(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	track := filepath.Join(f.mount, "Contents", "a.mp3")
	if err := os.MkdirAll(filepath.Dir(track), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(track, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := takeDeckRows
	t.Cleanup(func() { takeDeckRows = old })
	var calls [][]string // the uevent writes seen at each call
	var whileHeld []string
	unloaded := make(chan struct{})
	takeDeckRows = func(_ context.Context, mount string) (bool, error) {
		if mount != f.mount {
			t.Errorf("rows taken on %s", mount)
		}
		calls = append(calls, f.seen())
		if len(calls) > 1 {
			return false, nil // the deck wrote nothing since
		}
		f.engine("9", track, "0100000") // a deck loads a track while the rows are looked for
		go func() {
			defer close(unloaded)
			time.Sleep(300 * time.Millisecond)
			whileHeld = f.seen()
			f.engine("9", "", "")
		}()
		return true, nil
	}
	if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	<-unloaded
	if len(calls) != 2 || len(calls[0]) != 0 || len(calls[1]) != 0 {
		t.Fatalf("the rows are taken before the remove, and again after the decks were busy: uevent writes at each call %v", calls)
	}
	if len(whileHeld) != 0 {
		t.Fatalf("uevent writes %v while a deck held the track it loaded during the pass", whileHeld)
	}
	if got := strings.Join(f.seen(), " "); got != "remove: add:" {
		t.Fatalf("uevent writes %q, want one remove and one add", got)
	}
	// A library the deck goes on writing does not hold the remount back for ever.
	calls = nil
	takeDeckRows = func(context.Context, string) (bool, error) { calls = append(calls, nil); return true, nil }
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen(), " "); len(calls) != deckRowPasses || got != "remove: add:" {
		t.Fatalf("%d passes (want %d), uevent writes %q", len(calls), deckRowPasses, got)
	}
}

// An Eject while the helper looks for the deck's rows stops the pass at once (it holds files of
// the stick open and writes the next library), and the stick is not mounted again.
func TestRemountStopsTakingRowsAtAnEject(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	old := takeDeckRows
	t.Cleanup(func() { takeDeckRows = old })
	var stopped time.Duration
	takeDeckRows = func(ctx context.Context, mount string) (bool, error) {
		start := time.Now()
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.WriteFile(ejectMark(mount), nil, 0o600) // the hook's mark of an Eject
		}()
		select {
		case <-ctx.Done():
			stopped = time.Since(start)
		case <-time.After(5 * time.Second):
		}
		return false, ctx.Err()
	}
	if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
		t.Fatal(err)
	}
	if stopped == 0 || stopped > 2*time.Second {
		t.Fatalf("the pass was not stopped by the eject (stopped after %v)", stopped)
	}
	if got := f.seen(); len(got) != 0 {
		t.Fatalf("uevent writes %v for an ejected stick", got)
	}
}

// #301: a finished background import is published by a synthetic remove and add of the stick's
// partition, once Engine copies no track it loads, and only after Engine has closed the stick's
// files: the publish renames the library under them.
func TestIssue301RemountRemovesAndAddsThePartition(t *testing.T) {
	f := newFakeStick(t)
	copying := filepath.Join(engineCopies, "c647_t.mp3")
	f.engine("9", copying, "0100001") // a load's copy under way: O_WRONLY
	_, devno, err := mountDevice(f.mount)
	if err != nil {
		t.Fatal(err)
	}
	f.busy = func() string {
		switch {
		case engineCopying():
			return "copying"
		case engineHolds(devno):
			return "holding"
		}
		return ""
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.engine("9", copying, "0100000") // the copy is done; Engine reads it back
		for len(f.seen()) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		f.engine("5", "", "") // InterfacesRemoved: Engine closes the stick's library
	}()
	if err := runRemount(context.Background(), f.mount); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen(), " "); got != "remove:holding add:" {
		t.Fatalf("uevent writes %q: the remove must wait for the copy, the add for Engine to close the library", got)
	}
}

// A stick the DJ ejected, or that is no longer mounted, is not touched.
func TestIssue301RemountLeavesAnEjectedStick(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	if err := os.WriteFile(ejectMark(f.mount), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRemount(context.Background(), f.mount); err != nil || len(f.seen()) != 0 {
		t.Fatalf("ejected: %v %v", err, f.seen())
	}
	if got := filepath.Base(ejectMark("/media/REKORDBOX")); got != "enginex-rb-ejected-2531a9d305fa8897" {
		t.Fatal(got) // edisksd_hook.c test_only_an_eject_marks_the_stick pins the same
	}
	os.Remove(ejectMark(f.mount))
	f.mountedNow = false
	if err := runRemount(context.Background(), f.mount); err != nil || len(f.seen()) != 0 {
		t.Fatalf("not mounted: %v %v", err, f.seen())
	}
}

// A remove that unmounts nothing leaves the old library and reports the actual failure.
func TestIssue301RemountFailureKeepsAnError(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	f.unmountOnRemove = false
	status := filepath.Join(t.TempDir(), "preparing")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runRemount(ctx, f.mount); err == nil || !strings.Contains(err.Error(), "not unmounted") {
		t.Fatalf("err %v", err)
	}
	var st usbLoadingStatus
	if b, err := os.ReadFile(status + ".err"); err != nil || json.Unmarshal(b, &st) != nil || st.State != "error" {
		t.Fatalf("status %q %v", b, err)
	}
	if got := strings.Join(f.seen(), " "); got != "remove:" {
		t.Fatalf("uevent writes %q", got)
	}
}

// A read-only HFS+ stick shows through the hook's view: its device is that of the view's entries.
func TestIssue301RemountFindsTheDeviceUnderAView(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "40 25 8:17 / /media/MAC ro - hfsplus /dev/sdz1 ro\n" +
		"41 40 0:55 / /media/MAC rw - tmpfs enginex-view rw\n" +
		"42 41 8:17 /Music /media/MAC/Music ro - hfsplus /dev/sdz1 ro\n"
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	old := mountinfoPath
	t.Cleanup(func() { mountinfoPath = old })
	mountinfoPath = info
	dev, devno, err := mountDevice("/media/MAC")
	if err != nil || dev != "/dev/sdz1" || devno != unix.Mkdev(8, 17) {
		t.Fatalf("%s %d %v", dev, devno, err)
	}
	if got := mountID("/media/MAC"); got != "41" {
		t.Fatal(got)
	}
	if _, _, err = mountDevice("/media/OTHER"); err == nil {
		t.Fatal("a volume that is not mounted")
	}
}

// #301: a remount waits while any deck holds a track from the stick, loaded, playing or not:
// Engine could not save that track's play count or edits after it ("pool is disposed"). Engine
// plays a loaded track from its copy "<id>_<file name>" in /data/inMusicTempFiles, which it keeps
// open until the deck unloads it (sandbox, stock Engine 5.3.0), so a copy whose name the stick's
// library has counts, whatever size the export recorded; so does an audio file Engine plays from
// the stick itself. The remount keeps waiting status and goes ahead once no deck holds one.
// Engine's copies of its own sampler
// sounds (always open), the stick's m.db and its OneLibrary export do not count.
func TestIssue301RemountWaitsWhileADeckHoldsATrackFromTheStick(t *testing.T) {
	for _, held := range []string{"copy", "file"} {
		t.Run(held, func(t *testing.T) {
			f := newFakeStick(t)
			f.engine("5", "", "")
			dir := filepath.Join(f.mount, "Engine Library", "Database2")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY,filename TEXT,fileBytes INTEGER);
INSERT INTO Track(filename,fileBytes) VALUES('Track00003.mp3',1234),('Track00004.mp3',NULL)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			files := map[string]string{
				filepath.Join(engineCopies, "c647-aa42-9cf4-7699_Track00003.mp3"):  "abcd", // this stick's track
				filepath.Join(engineCopies, "d001-0000-0000-0001_Other_Track.mp3"): "abcd", // another drive's, or Engine's sampler
				filepath.Join(f.mount, "Contents", "Track00004.mp3"):               "abcdef",
				filepath.Join(f.mount, "PIONEER", "rekordbox", "exportLibrary.db"): "x",
			}
			for p, body := range files {
				if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f.engine("6", filepath.Join(engineCopies, "d001-0000-0000-0001_Other_Track.mp3"), "0100000")
			f.engine("7", filepath.Join(f.mount, "PIONEER", "rekordbox", "exportLibrary.db"), "0100000")
			hold := filepath.Join(engineCopies, "c647-aa42-9cf4-7699_Track00003.mp3")
			if held == "file" { // a track Engine could not copy, played from the stick
				hold = filepath.Join(f.mount, "Contents", "Track00004.mp3")
			}
			f.engine("9", hold, "0100000")
			status := filepath.Join(t.TempDir(), "preparing")
			t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
			var before []string
			var state usbLoadingStatus
			unloaded := make(chan struct{})
			go func() {
				defer close(unloaded)
				time.Sleep(300 * time.Millisecond)
				before = f.seen()
				if b, err := os.ReadFile(status); err == nil {
					json.Unmarshal(b, &state)
				}
				f.engine("9", "", "") // the deck unloads it
			}()
			if err = runRemount(context.Background(), f.mount); err != nil {
				t.Fatal(err)
			}
			<-unloaded
			if len(before) != 0 {
				t.Fatalf("uevent writes %v while a deck held a track from the stick", before)
			}
			if state.State != "waiting" {
				t.Fatalf("status while a deck held a track: %q", state.State)
			}
			if got := strings.Join(f.seen(), " "); got != "remove: add:" {
				t.Fatalf("uevent writes %q once no deck held one", got)
			}
		})
	}
}

// The hook stops the remount helper under this name before an eager eject
// (edisksd_hook.c remount_unit, test_eject_stops_the_remount_helper pins the same value).
func TestRemountUnitMatchesTheHook(t *testing.T) {
	if got := remountUnit("/media/REKORDBOX"); got != "enginex-rb-remount-2531a9d305fa8897.service" {
		t.Fatalf("unit %q", got)
	}
}

func TestStartRemountRunsAnIdleSystemdUnit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+log+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := systemdStartRemount("/media/REKORDBOX"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, want := range []string{
		"--unit=enginex-rb-remount-2531a9d305fa8897.service", "--collect", "--no-block", "--quiet",
		"--slice=enginex-background.slice", "--nice=19",
		"--property=CPUSchedulingPolicy=idle", "--property=IOSchedulingClass=idle",
		"--property=TimeoutStopSec=5",
		"--setenv=GOMAXPROCS=1", "--setenv=GOMEMLIMIT=128MiB",
	} {
		if !strings.Contains("\n"+string(b), "\n"+want+"\n") {
			t.Fatalf("missing %s in %q", want, args)
		}
	}
	if n := len(args); n < 4 || args[n-3] != "rb-bridge" || args[n-2] != "--remount" || args[n-1] != "/media/REKORDBOX" {
		t.Fatalf("command %q", args)
	}
}

// BLOCK 1: a failed deck-row pass must never proceed to remove/add remounting (which would drop
// deck edits in swapNext when m.db was not carried over). Transient failures defer and retry;
// persistent failures abort remount without unmounting the stick.
func TestR4_Block1_RemountDefersAndAbortsOnFailedDeckRowPass(t *testing.T) {
	t.Run("transient_error_defers_then_publishes", func(t *testing.T) {
		f := newFakeStick(t)
		f.engine("5", "", "")
		old := takeDeckRows
		t.Cleanup(func() { takeDeckRows = old })
		var calls atomic.Int32
		var afterFirst []string
		takeDeckRows = func(context.Context, string) (bool, error) {
			n := calls.Add(1)
			if n == 1 {
				return false, errors.New("lock held")
			}
			if n == 2 {
				afterFirst = f.seen()
			}
			return false, nil
		}
		if err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb")); err != nil {
			t.Fatalf("remount failed after transient deck-row error: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("expected 2 takeDeckRows attempts, got %d", calls.Load())
		}
		if len(afterFirst) != 0 {
			t.Fatalf("uevent writes %v after failed takeDeckRows before retry succeeded", afterFirst)
		}
		if got := strings.Join(f.seen(), " "); got != "remove: add:" {
			t.Fatalf("uevent writes %q, want remove: add:", got)
		}
	})

	t.Run("persistent_error_aborts_without_remount", func(t *testing.T) {
		f := newFakeStick(t)
		f.engine("5", "", "")
		old := takeDeckRows
		t.Cleanup(func() { takeDeckRows = old })
		var calls atomic.Int32
		takeDeckRows = func(context.Context, string) (bool, error) {
			calls.Add(1)
			return false, errors.New("carryOver failed")
		}
		err := remount(context.Background(), f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb"))
		if err == nil || !strings.Contains(err.Error(), "take deck rows") {
			t.Fatalf("expected error from remount when takeDeckRows repeatedly fails, got %v", err)
		}
		if got := calls.Load(); got != int32(deckRowFailures) {
			t.Fatalf("expected %d failed attempts before aborting, got %d", deckRowFailures, got)
		}
		if seen := f.seen(); len(seen) != 0 {
			t.Fatalf("remount wrote uevents %v despite failed deck-row passes", seen)
		}
	})

	t.Run("real_lock_held_defers_remount", func(t *testing.T) {
		t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
		f := newFakeStick(t)
		f.engine("5", "", "")
		testfixture.LargeExport(t, f.mount, 4)
		ctx := context.Background()
		if _, err := bridge.ConvertAtMount(ctx, f.mount, bridge.MountOptions{}); err != nil {
			t.Fatalf("ConvertAtMount: %v", err)
		}
		res, err := bridge.FillInBackground(ctx, f.mount, bridge.FillOptions{})
		if err != nil || !res.Publish {
			t.Fatalf("FillInBackground: res=%+v err=%v", res, err)
		}
		db, err := sql.Open("sqlite", filepath.Join(f.mount, "Engine Library", "Database2", "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE Track SET rating=80 WHERE id=1`); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		_ = db.Close()

		unlock, err := rbstate.LockVolume(f.mount, 0)
		if err != nil {
			t.Fatalf("LockVolume: %v", err)
		}
		locked := true
		defer func() {
			if locked {
				unlock()
			}
		}()

		oldTake := takeDeckRows
		var firstCallOnce sync.Once
		firstTook := make(chan struct{})
		takeDeckRows = func(c context.Context, m string) (bool, error) {
			looked, err := oldTake(c, m)
			firstCallOnce.Do(func() { close(firstTook) })
			return looked, err
		}
		t.Cleanup(func() { takeDeckRows = oldTake })

		deckWait = 60 * time.Millisecond
		done := make(chan error, 1)
		go func() {
			done <- remount(ctx, f.mount, newUSBLoadingReporter(filepath.Join(t.TempDir(), "status"), "usb"))
		}()

		<-firstTook
		time.Sleep(100 * time.Millisecond)
		if seen := f.seen(); len(seen) != 0 {
			t.Fatalf("remount proceeded (%v) while real volume lock prevented TakeDeckRows", seen)
		}
		locked = false
		unlock()

		if err := <-done; err != nil {
			t.Fatalf("remount after lock release: %v", err)
		}
		if got := strings.Join(f.seen(), " "); got != "remove: add:" {
			t.Fatalf("uevent writes %q, want remove: add:", got)
		}
	})
}
