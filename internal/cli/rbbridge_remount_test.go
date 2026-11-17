package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	f := &fakeStick{mount: "/media/REKORDBOX", mountedNow: true, unmountOnRemove: true, proc: filepath.Join(dir, "proc")}
	sys := filepath.Join(dir, "sys")
	f.uevent = filepath.Join(sys, "sdz1", "uevent")
	for _, d := range []string{filepath.Dir(f.uevent), filepath.Join(f.proc, "77", "fd"), filepath.Join(f.proc, "77", "fdinfo")} {
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
	oldWait, oldRemove, oldClose, oldRemount, oldReady := remountWait, removeLimit, closeLimit, remountLimit, readyEvery
	t.Cleanup(func() {
		mountinfoPath, sysClassBlock, procDir, ejectMarkDir, mounted = oldInfo, oldSys, oldProc, oldMark, oldMounted
		remountWait, removeLimit, closeLimit, remountLimit, readyEvery = oldWait, oldRemove, oldClose, oldRemount, oldReady
	})
	mountinfoPath, sysClassBlock, procDir, ejectMarkDir = info, sys, f.proc, filepath.Join(dir, "run")
	remountWait, removeLimit, closeLimit, remountLimit, readyEvery = 20*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond, 20*time.Millisecond
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

// A remove that unmounts nothing leaves the old library; the banner asks for a re-insert until
// the stick is mounted anew.
func TestIssue301RemountFailsToTheBanner(t *testing.T) {
	f := newFakeStick(t)
	f.engine("5", "", "")
	f.unmountOnRemove = false
	status := filepath.Join(t.TempDir(), "preparing")
	t.Setenv("ENGINEX_USB_PREPARING_PATH", status)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if err := runRemount(ctx, f.mount); err == nil || !strings.Contains(err.Error(), "insert it again") {
		t.Fatalf("err %v", err)
	}
	var st usbLoadingStatus
	if b, err := os.ReadFile(status); err != nil || json.Unmarshal(b, &st) != nil || st.State != "ready" {
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
