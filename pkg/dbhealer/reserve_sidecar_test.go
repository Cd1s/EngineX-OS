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
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Regression test for finding 2 of the Go review R11 of #336: the sidecar cleanup of a reserve
// rebuild and a library put in place after the cleanup validated.

const (
	walWriterChildEnv  = "ENGINEX_RESERVE_WAL_WRITER_CHILD"
	walWriterCommitted = "ENGINEX_WAL_WRITER_COMMITTED"
)

// R11 finding 2. The rebuild of a rollback-journal library validates: no sidecar exists. Then
// another library is put at m.db and its writer, a separate process, commits in WAL mode and
// holds its next transaction. The cleanup must leave that writer's -wal and -shm alone: they
// were not there when it validated. Unlinked, the commit is lost once the writer dies.
func TestReserveCleanupLeavesSidecarsMadeAfterValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	if db := os.Getenv(walWriterChildEnv); db != "" {
		walWriterChild(t, db)
		return
	}
	mount := filepath.Join(t.TempDir(), "stick")
	live := reserve12Volume(t, mount, reserve12Library(t))
	// The library put in place meanwhile: WAL mode, checkpointed and closed, so it has no sidecar yet.
	incoming := filepath.Join(filepath.Dir(live), "incoming")
	db, err := sql.Open("sqlite", incoming)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, "PRAGMA journal_mode=WAL")
	mustExec(t, db, "CREATE TABLE Information(uuid TEXT); INSERT INTO Information VALUES('before-commit')")
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, serr := os.Lstat(incoming + suffix); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("fixture: %s%s exists (err=%v)", incoming, suffix, serr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	writer := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReserveCleanupLeavesSidecarsMadeAfterValidation$", "-test.v")
	writer.Env = append(os.Environ(), walWriterChildEnv+"="+live)
	writer.WaitDelay = 5 * time.Second
	stdout, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	writerOut := &lockedBuffer{}
	writer.Stderr = writerOut
	writerDone := make(chan error, 1)
	started := false
	defer func() {
		if started {
			_ = writer.Process.Kill()
			<-writerDone
		}
	}()

	var hookErr, lockErr error
	var writerWAL, writerSHM os.FileInfo
	hookRan := false
	reserveBeforeSidecarUnlink = func(p string) {
		if p != live || hookRan {
			return
		}
		hookRan = true
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, serr := os.Lstat(live + suffix); !errors.Is(serr, os.ErrNotExist) {
				hookErr = fmt.Errorf("fixture: %s%s exists at validation (err=%v)", live, suffix, serr)
				return
			}
		}
		// The cleanup runs under the volume lock: no window or job section publishes here.
		if unlock, lerr := rbstate.LockVolume(mount, 0); !errors.Is(lerr, rbstate.ErrLocked) {
			if lerr == nil {
				unlock()
			}
			lockErr = fmt.Errorf("volume lock during the sidecar cleanup: err=%v, want it held by the rebuild (rbstate.ErrLocked)", lerr)
		}
		// What follows is a publisher outside every lock, the schedule of the review.
		if hookErr = os.Rename(incoming, live); hookErr != nil {
			return
		}
		if hookErr = writer.Start(); hookErr != nil {
			return
		}
		started = true
		committed := make(chan bool, 1)
		go func() {
			found := false
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				fmt.Fprintln(writerOut, sc.Text())
				if !found && strings.Contains(sc.Text(), walWriterCommitted) {
					found = true
					committed <- true
				}
			}
			if !found {
				committed <- false
			}
			writerDone <- writer.Wait()
		}()
		select {
		case ok := <-committed:
			if !ok {
				hookErr = errors.New("the writer ended before its commit")
				return
			}
		case <-ctx.Done():
			hookErr = errors.New("the writer did not commit")
			return
		}
		if writerWAL, hookErr = os.Lstat(live + "-wal"); hookErr != nil {
			return
		}
		if writerWAL.Size() == 0 {
			hookErr = errors.New("fixture: the writer's commit left an empty -wal")
			return
		}
		writerSHM, hookErr = os.Lstat(live + "-shm")
	}
	defer func() { reserveBeforeSidecarUnlink = nil }()

	fixed, err := NormalizeMountReserve(mount)
	if !hookRan {
		t.Fatalf("the cleanup was never reached: fixed=%v err=%v", fixed, err)
	}
	if hookErr != nil {
		t.Fatalf("schedule: %v\n%s", hookErr, writerOut.String())
	}
	if lockErr != nil {
		t.Error(lockErr)
	}
	if err == nil || len(fixed) != 0 {
		t.Errorf("rebuild of a library replaced during its cleanup: fixed=%v err=%v, want nothing fixed and an error", fixed, err)
	}
	for suffix, made := range map[string]os.FileInfo{"-wal": writerWAL, "-shm": writerSHM} {
		if cur, serr := os.Lstat(live + suffix); serr != nil || !os.SameFile(made, cur) {
			t.Errorf("the cleanup removed the writer's m.db%s, which was absent at validation: %v", suffix, serr)
		}
	}
	// The writer dies with its connection open: recovery is left to the next connection.
	if err = writer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerDone:
		started = false
	case <-ctx.Done():
		t.Fatal("the writer did not end after its kill")
	}
	check, err := sql.Open("sqlite", fileURI(live))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var uuid string
	if err = check.QueryRow("SELECT uuid FROM Information").Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if uuid != "after-commit" {
		t.Errorf("a new connection reads uuid=%q, want the writer's committed %q: the cleanup deleted a durable commit", uuid, "after-commit")
	}
}

// walWriterChild commits once in WAL mode, durably, and holds its next transaction until killed.
func walWriterChild(t *testing.T, path string) {
	db, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA synchronous=FULL", "UPDATE Information SET uuid='after-commit'", "BEGIN IMMEDIATE"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("wal writer: %s: %v", q, err)
		}
	}
	fmt.Println(walWriterCommitted)
	time.Sleep(90 * time.Second) // its own deadline: the parent kills it long before
	t.Fatal("wal writer: never stopped")
}
