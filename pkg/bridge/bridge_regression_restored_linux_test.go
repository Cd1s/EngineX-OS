//go:build linux

package bridge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func watchANLZOpens(t *testing.T, dir string, step func()) map[string]int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify: %v", err)
	}
	defer unix.Close(fd)
	if _, err = unix.InotifyAddWatch(fd, dir, unix.IN_OPEN|unix.IN_CLOSE); err != nil {
		t.Fatal(err)
	}
	step()
	opens := map[string]int{}
	buf := make([]byte, 1<<16)
	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) || n <= 0 {
			return opens
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
				t.Fatal("inotify overflow")
			}
			name := ""
			if ev.Len > 0 {
				raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(ev.Len)]
				name = strings.TrimRight(string(raw), "\x00")
			}
			if ev.Mask&unix.IN_OPEN != 0 && name != "" {
				opens[name]++
			}
			off += unix.SizeofInotifyEvent + int(ev.Len)
		}
	}
}

// Restored #253 (replaces pkg/bridge/issue253_mountreads_linux_test.go:
// TestIssue253MountReadsNoANLZFileForTheWaveformCopies):
// no ConvertAtMount window (first insert, unchanged reinsert, or publish) ever opens any file
// under PIONEER/USBANLZ/ on Linux.
// Guards pkg/bridge/mount.go and pkg/bridge/apply.go (mount windows never parse ANLZ files).
func TestRestored_Issue253_MountWindowOpensZeroANLZFiles(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 4)
	anlzDir := filepath.Join(mount, "PIONEER", "USBANLZ", "P001")

	if opens := watchANLZOpens(t, anlzDir, func() {
		mountNow(t, mount, MountOptions{})
	}); len(opens) != 0 {
		t.Fatalf("first ConvertAtMount opened ANLZ files: %v", opens)
	}

	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatalf("FillInBackground: %v", err)
	}

	if opens := watchANLZOpens(t, anlzDir, func() {
		mountNow(t, mount, MountOptions{Publish: true})
	}); len(opens) != 0 {
		t.Fatalf("publish ConvertAtMount opened ANLZ files: %v", opens)
	}

	if opens := watchANLZOpens(t, anlzDir, func() {
		res := mountNow(t, mount, MountOptions{})
		if !res.Bypassed {
			t.Fatalf("expected unchanged bypass: %+v", res)
		}
	}); len(opens) != 0 {
		t.Fatalf("unchanged ConvertAtMount opened ANLZ files: %v", opens)
	}
}

// Restored #271/#321 (6c) (replaces pkg/bridge/issue321_attach_linux_test.go:
// TestIssue321AStickThatHangsAtInsertHoldsOnlyItself):
// a stick whose writeback leftover cleanup blocks at insert must not stall CueWatcher from
// writing back cue edits on a second healthy stick.
// Guards pkg/bridge/cuewatch.go:101 (`go func() { CleanWriteBackLeftovers(mount) }()`).
func TestRestored_Issue321_HungStickAtInsertHoldsOnlyItself(t *testing.T) {
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	hung := nextStick(t, 2)
	healthy := nextStick(t, 2)
	for _, m := range []string{hung, healthy} {
		stageAll(t, m)
		mountNow(t, m, MountOptions{Publish: true})
	}
	// Plant a FIFO at writeBackMarkName on hung so os.ReadFile inside CleanWriteBackLeftovers blocks.
	fifo := filepath.Join(dbDir(hung), writeBackMarkName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	release := func() {
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	}
	defer release()

	oldPrep := usbPreparingFlagPath
	usbPreparingFlagPath = filepath.Join(t.TempDir(), "no-preparing")
	defer func() { usbPreparingFlagPath = oldPrep }()

	w := &CueWatcher{
		quiet:  150 * time.Millisecond,
		retry:  200 * time.Millisecond,
		poll:   50 * time.Millisecond,
		mounts: func() []string { return []string{hung, healthy} },
		pass:   WriteBackCues,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	defer func() {
		cancel()
		for {
			release()
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	// Wait briefly for rescan to enter the blocked FIFO goroutine on hung, then edit healthy.
	time.Sleep(120 * time.Millisecond)
	db, err := sql.Open("sqlite", filepath.Join(dbDir(healthy), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	pos := 1000.0
	if err := enginedb.SetQuickCue(context.Background(), db, 1, 0, "BesideHung", "#ff0000", &pos); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=COALESCE(lastEditTime,0)+100 WHERE id=1`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := rekordbox.ReadAnalysis(filepath.Join(healthy, "PIONEER", "USBANLZ", "P001", "ANLZ0000.EXT")); err == nil {
			for _, c := range a.Cues {
				if c.Name == "BesideHung" {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("healthy stick cue edit was not written back while hung stick blocked in leftover cleanup")
}
