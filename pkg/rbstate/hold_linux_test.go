package rbstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The volume's lock as a context carries it (#336 Go review R13): who holds it hands it down,
// a writer without it takes it, and nobody waits for themselves.
func TestHoldVolumeIsReusedThroughTheContextAndExcludesEverybodyElse(t *testing.T) {
	base := t.TempDir()
	mount, other := filepath.Join(base, "stick"), filepath.Join(base, "other")
	for _, d := range []string{mount, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if VolumeHeld(context.Background(), mount) {
		t.Fatal("a plain context holds the volume")
	}
	ctx, release, err := HoldVolume(context.Background(), mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !VolumeHeld(ctx, mount) || VolumeHeld(ctx, other) || VolumeHeld(context.Background(), mount) {
		t.Fatalf("held: its own=%v another volume=%v a plain context=%v, want true false false",
			VolumeHeld(ctx, mount), VolumeHeld(ctx, other), VolumeHeld(context.Background(), mount))
	}
	// Handed down: no wait, and the inner release gives nothing back.
	derived, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	inner, innerRelease, err := HoldVolume(derived, mount, 0)
	if err != nil || !VolumeHeld(inner, mount) {
		t.Fatalf("a context that holds the volume was not reused: %v", err)
	}
	innerRelease()
	if !VolumeHeld(ctx, mount) {
		t.Fatal("the inner release gave the outer hold back")
	}
	// Everybody else is shut out: a context without the hold, and a plain LockVolume.
	if _, _, err = HoldVolume(context.Background(), mount, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second holder: %v, want ErrLocked", err)
	}
	if _, err = LockVolume(mount, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("LockVolume under a hold: %v, want ErrLocked", err)
	}
	// A second volume is its own lock, and one context carries both.
	both, releaseOther, err := HoldVolume(ctx, other, 0)
	if err != nil || !VolumeHeld(both, other) || !VolumeHeld(both, mount) {
		t.Fatalf("a second volume in the same context: %v", err)
	}
	releaseOther()
	if VolumeHeld(both, other) || !VolumeHeld(both, mount) {
		t.Fatal("releasing the second volume changed the wrong hold")
	}
	release()
	release() // harmless
	if VolumeHeld(ctx, mount) || VolumeHeld(inner, mount) {
		t.Fatal("a released hold still counts as held")
	}
	// A context whose hold was released takes the lock anew instead of trusting it.
	again, releaseAgain, err := HoldVolume(ctx, mount, 0)
	if err != nil || !VolumeHeld(again, mount) {
		t.Fatalf("taking the lock again after a release: %v", err)
	}
	if _, err = LockVolume(mount, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("the lock was not really taken again: %v", err)
	}
	releaseAgain()
	if unlock, err := LockVolume(mount, 0); err != nil {
		t.Fatalf("the lock is not free at the end: %v", err)
	} else {
		unlock()
	}
}

const viewChildEnv = "ENGINEX_RBSTATE_VIEW_CHILD"

// Every way into one volume has the same lock. The read-only HFS+ view is the hard one: a stick
// without a library of its own gets a new tmpfs root for every view, and what the views share is
// the library folder bound in from /data. Real mounts, in a private mount namespace.
func TestVolumeLockIsTheSameThroughEveryViewOfAVolume(t *testing.T) {
	if base := os.Getenv(viewChildEnv); base != "" {
		viewChild(t, base)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root for mounts in a private namespace")
	}
	base := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVolumeLockIsTheSameThroughEveryViewOfAVolume$", "-test.v")
	cmd.Env = append(os.Environ(), viewChildEnv+"="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "VIEW-CHILD SKIP") {
		t.Skipf("mounts are not allowed here:\n%s", out)
	}
	if err != nil || !strings.Contains(string(out), "--- PASS: TestVolumeLockIsTheSameThroughEveryViewOfAVolume") {
		t.Fatalf("the views test failed: %v\n%s", err, out)
	}
}

func viewChild(t *testing.T, base string) {
	mountOrSkip := func(src, dst, fstype string, flags uintptr, data string) {
		if err := syscall.Mount(src, dst, fstype, flags, data); err != nil {
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				fmt.Println("VIEW-CHILD SKIP", err)
				t.Skip(err)
			}
			t.Fatalf("mount %s on %s: %v", src, dst, err)
		}
	}
	mountOrSkip("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, "")
	dir := func(parts ...string) string {
		p := filepath.Join(append([]string{base}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	id := func(mount string) string {
		v, err := volumeID(mount)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// The library of a read-only HFS+ stick lives on the deck's own disk.
	library := dir("data", "library-of-the-stick")
	// Two views of that stick, one after the other, as the hook makes them for its private
	// pre-mount and for the visible mount: each a new tmpfs with the library bound in.
	view1, view2 := dir("views", "one"), dir("views", "two")
	for _, v := range []string{view1, view2} {
		mountOrSkip("enginex-hfs-view", v, "tmpfs", 0, "size=1m,mode=0755")
		if err := os.Mkdir(filepath.Join(v, "Engine Library"), 0o755); err != nil {
			t.Fatal(err)
		}
		mountOrSkip(library, filepath.Join(v, "Engine Library"), "", syscall.MS_BIND, "")
	}
	var r1, r2 syscall.Stat_t
	if err := syscall.Stat(view1, &r1); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(view2, &r2); err != nil {
		t.Fatal(err)
	}
	if r1.Dev == r2.Dev && r1.Ino == r2.Ino {
		t.Fatal("fixture: the two views have the same root; the test would prove nothing")
	}
	if id(view1) != id(view2) {
		t.Fatalf("two views of one library have different locks: %s and %s", id(view1), id(view2))
	}
	unlock, err := LockVolume(view1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = LockVolume(view2, 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("the library is locked through one view and free through the other: %v", err)
	}
	if _, _, herr := HoldVolume(context.Background(), view1, 0); !errors.Is(herr, ErrLocked) {
		t.Fatalf("HoldVolume through the locked view: %v", herr)
	}
	unlock()
	// A view of another stick is another lock.
	otherLibrary, view3 := dir("data", "library-of-another-stick"), dir("views", "three")
	mountOrSkip("enginex-hfs-view", view3, "tmpfs", 0, "size=1m,mode=0755")
	if err = os.Mkdir(filepath.Join(view3, "Engine Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(otherLibrary, filepath.Join(view3, "Engine Library"), "", syscall.MS_BIND, "")
	if id(view3) == id(view1) {
		t.Fatal("two sticks' views share one lock")
	}
	// An ordinary volume is named by its root, with or without a library, and through a bind
	// of its root; its library folder is on the root's own file system.
	stick, again := dir("media", "stick"), dir("media", "again")
	mountOrSkip("stick", stick, "tmpfs", 0, "size=1m,mode=0755")
	before := id(stick)
	if err = os.MkdirAll(filepath.Join(stick, "Engine Library", "Database2"), 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(stick, again, "", syscall.MS_BIND, "")
	if id(stick) != before || id(again) != before {
		t.Fatalf("an ordinary volume changed its lock: %s without a library, %s with one, %s through a bind", before, id(stick), id(again))
	}
}
