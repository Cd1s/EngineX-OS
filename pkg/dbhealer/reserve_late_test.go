package dbhealer

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	lateOpenerEnv   = "ENGINEX_RESERVE_LATE_OPENER"
	lateOpenerGoEnv = "ENGINEX_RESERVE_LATE_OPENER_GO"
)

// TestReserveLateOpenerChild opens the library now, whatever its first read says, and writes when told to.
func TestReserveLateOpenerChild(t *testing.T) {
	path := os.Getenv(lateOpenerEnv)
	if path == "" {
		t.Skip("a child of the reserve tests")
	}
	db, err := sql.Open("sqlite", fileURI(path)+"?_pragma=busy_timeout(300)")
	if err != nil {
		fmt.Println("LATE REFUSED open:", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int
	err = db.QueryRow("SELECT count(*) FROM Track").Scan(&n)
	fmt.Printf("LATE OPENED tracks=%d err=%v\n", n, err)
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if _, e := os.Stat(os.Getenv(lateOpenerGoEnv)); e == nil {
			break
		}
	}
	if _, err = db.Exec("UPDATE Information SET uuid='late-writer'"); err != nil {
		fmt.Println("LATE REFUSED write:", err)
	} else {
		fmt.Println("LATE COMMITTED")
	}
	time.Sleep(90 * time.Second)
}

// The window the reserve rebuild's hold leaves (#336 Go review R13): a program opens the original
// after the rebuild's last checks and before its rename, and goes on afterwards. On a WAL
// original its write made a -wal beside the rebuilt library, and SQLite replayed the original's
// pages into it. The original is emptied for such a program before the hold ends; when the
// backup is a hard link to the original it is made a file of its own first, and stays whole.
func TestReserveRebuildRetiresTheOriginalForALateOpener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	for _, backup := range []string{"hard-linked backup", "copied backup"} {
		t.Run(backup, func(t *testing.T) {
			mount := filepath.Join(t.TempDir(), "stick")
			live := reserve12Volume(t, mount, reserve12Library(t))
			db, err := sql.Open("sqlite", fileURI(live))
			if err != nil {
				t.Fatal(err)
			}
			var mode string
			if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
				t.Fatalf("fixture: journal mode %q %v", mode, err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if r, ok, rerr := Reserve(live); rerr != nil || !ok || r != 12 {
				t.Fatalf("fixture: reserve %d %v %v", r, ok, rerr)
			}
			if backup == "copied backup" {
				origLink := reserveLink
				reserveLink = func(string, string) error { return syscall.EXDEV }
				defer func() { reserveLink = origLink }()
			}
			goFile := filepath.Join(t.TempDir(), "go")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			late := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReserveLateOpenerChild$", "-test.v")
			late.Env = append(os.Environ(), lateOpenerEnv+"="+live, lateOpenerGoEnv+"="+goFile)
			late.WaitDelay = 5 * time.Second
			out, err := late.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 8)
			started := false
			defer func() {
				if started {
					_ = late.Process.Kill()
					_ = late.Wait()
				}
			}()
			next := func(what string) string {
				select {
				case line, ok := <-lines:
					if !ok {
						return "LATE (ended before " + what + ")"
					}
					return line
				case <-ctx.Done():
					return "LATE (said nothing about " + what + ")"
				}
			}
			opened := ""
			reserveBeforeRename = func(p string) {
				if p != live || started {
					return
				}
				if err := late.Start(); err != nil {
					opened = "LATE (not started: " + err.Error() + ")"
					return
				}
				started = true
				go func() {
					sc := bufio.NewScanner(out)
					for sc.Scan() {
						if strings.HasPrefix(sc.Text(), "LATE ") {
							lines <- sc.Text()
						}
					}
					close(lines)
				}()
				opened = next("its open")
			}
			defer func() { reserveBeforeRename = nil }()

			fixed, err := NormalizeMountReserve(mount)
			if !strings.HasPrefix(opened, "LATE OPENED") {
				t.Fatalf("the late opener did not open the original before the rename: %q (rebuild: fixed=%v err=%v)", opened, fixed, err)
			}
			if err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
				t.Fatalf("rebuild: fixed=%v err=%v, want [m.db]", fixed, err)
			}
			if err = os.WriteFile(goFile, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			wrote := next("its write")
			t.Logf("late opener: %s; then %s", opened, wrote)
			left := ""
			for _, suffix := range []string{"-wal", "-shm"} {
				if st, err := os.Lstat(live + suffix); err == nil && (suffix != "-wal" || st.Size() > 0) {
					left += " m.db" + suffix
				}
			}
			_ = late.Process.Kill()
			_ = late.Wait()
			started = false

			look := func(path string) (integrity, uuid string, tracks int) {
				t.Helper()
				c, err := sql.Open("sqlite", fileURI(path)+"?mode=ro")
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err = c.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
					integrity = "integrity_check failed: " + err.Error()
				}
				_ = c.QueryRow("SELECT uuid FROM Information").Scan(&uuid)
				_ = c.QueryRow("SELECT count(*) FROM Track").Scan(&tracks)
				return
			}
			integrity, uuid, tracks := look(live)
			if r, ok, rerr := Reserve(live); rerr != nil || !ok || r != 0 {
				t.Errorf("the rebuilt library's reserve is %d (%v %v), want 0", r, ok, rerr)
			}
			if integrity != "ok" || uuid != "test-library" || tracks != 5 {
				t.Errorf("the rebuilt library after the late opener went on: integrity=%q uuid=%q tracks=%d, want ok, test-library, 5 (late opener: %s; its sidecars:%s)",
					integrity, uuid, tracks, wrote, left)
			}
			if wrote == "LATE COMMITTED" {
				t.Errorf("the late opener's write into the replaced original was reported committed")
			}
			if left != "" {
				t.Errorf("the late opener left sidecars of the original beside the rebuilt library:%s", left)
			}
			// The backup is the original, whole, whichever way it was made.
			integrity, uuid, tracks = look(live + ".pre-reserve")
			if r, ok, rerr := Reserve(live + ".pre-reserve"); rerr != nil || !ok || r != 12 || integrity != "ok" || uuid != "test-library" || tracks != 5 {
				t.Errorf("the backup: reserve=%d (%v %v) integrity=%q uuid=%q tracks=%d, want the original (12, ok, test-library, 5)", r, ok, rerr, integrity, uuid, tracks)
			}
			if _, err := os.Lstat(live + ".pre-reserve.tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a .pre-reserve.tmp was left: %v", err)
			}
		})
	}
}
