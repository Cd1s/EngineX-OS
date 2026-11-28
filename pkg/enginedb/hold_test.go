package enginedb

import (
	"context"
	"database/sql"
	"encoding/json"
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

// The two halves of the rule every writer of a volume's library follows (#336 Go review R13):
// the volume's lock for an edit in place, and the exclusive hold for a replacement.

const (
	holdChildEnv   = "ENGINEX_HOLD_CHILD"
	holdChildOpEnv = "ENGINEX_HOLD_CHILD_OP"
)

// TestHoldChild is another process with a plain SQLite connection: Engine, or any tool.
func TestHoldChild(t *testing.T) {
	path := os.Getenv(holdChildEnv)
	if path == "" {
		t.Skip("a child of the hold tests")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(200)")
	if err != nil {
		fmt.Println("HOLD-CHILD ERR", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	switch os.Getenv(holdChildOpEnv) {
	case "read":
		var n int
		err = db.QueryRow("SELECT count(*) FROM Track").Scan(&n)
	case "write":
		_, err = db.Exec("INSERT INTO Track(title) VALUES('child')")
	case "stay": // has the library open, idle, until killed
		var n int
		if err = db.QueryRow("SELECT count(*) FROM Track").Scan(&n); err == nil {
			fmt.Println("HOLD-CHILD OK")
			time.Sleep(90 * time.Second)
			return
		}
	}
	if err != nil {
		fmt.Println("HOLD-CHILD ERR", err)
		return
	}
	fmt.Println("HOLD-CHILD OK")
}

func holdChild(t *testing.T, path, op string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHoldChild$", "-test.v")
	cmd.Env = append(os.Environ(), holdChildEnv+"="+path, holdChildOpEnv+"="+op)
	out, _ := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "HOLD-CHILD ") {
			return line
		}
	}
	t.Fatalf("the child said nothing:\n%s", out)
	return ""
}

// holdLibrary makes a small library in the given journal mode, closed, without a sidecar.
func holdLibrary(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"PRAGMA journal_mode=" + mode,
		"CREATE TABLE Track(id INTEGER PRIMARY KEY, title TEXT)", "CREATE TABLE Playlist(id INTEGER PRIMARY KEY, title TEXT)"} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	return path
}

func TestHoldLibraryShutsOutEveryOtherConnection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the decks' locking is Linux's")
	}
	for _, mode := range []string{"DELETE", "WAL"} {
		t.Run(mode, func(t *testing.T) {
			path := holdLibrary(t, mode)
			state := LibraryState(path)
			h, err := HoldLibrary(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Release()
			if !h.Is(state) {
				t.Errorf("the held library is not the one LibraryState named (%q)", state)
			}
			if empty, err := h.Empty(context.Background()); err != nil || !empty {
				t.Errorf("Empty of a library without rows: %v %v", empty, err)
			}
			for _, op := range []string{"read", "write"} {
				if got := holdChild(t, path, op); !strings.Contains(got, "SQLITE_BUSY") {
					t.Errorf("another process, %s during the hold: %s, want SQLITE_BUSY", op, got)
				}
			}
			for _, suffix := range []string{"-shm", "-journal"} {
				if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the hold made m.db%s: %v", suffix, err)
				}
			}
			if err := h.Same(); err != nil {
				t.Errorf("Same: %v", err)
			}
			h.Release()
			h.Release() // harmless
			// With the hold gone the other process writes, and that is another library state.
			if got := holdChild(t, path, "write"); got != "HOLD-CHILD OK" {
				t.Fatalf("another process, write after the hold: %s", got)
			}
			h2, err := HoldLibrary(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer h2.Release()
			if h2.Is(state) {
				t.Error("a library written since still counts as the one LibraryState named")
			}
			if empty, err := h2.Empty(context.Background()); err != nil || empty {
				t.Errorf("Empty of a library with a track: %v %v", empty, err)
			}
			// Another file at the path is not the held one.
			other := holdLibrary(t, "DELETE")
			if err = os.Rename(other, path); err != nil {
				t.Fatal(err)
			}
			if err = h2.Same(); !errors.Is(err, ErrLibraryChanged) {
				t.Errorf("Same after another file took the path: %v, want ErrLibraryChanged", err)
			}
			if h2.Is(LibraryState(path)) {
				t.Error("Is reports the hold on a file that is not the held one")
			}
		})
	}
}

func TestHoldLibraryIsRefusedWhileAnotherProcessHasTheLibrary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the decks' locking is Linux's")
	}
	// A WAL library that any other process has open, even idle, cannot be held at all.
	path := holdLibrary(t, "WAL")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stay := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHoldChild$", "-test.v")
	stay.Env = append(os.Environ(), holdChildEnv+"="+path, holdChildOpEnv+"=stay")
	out, err := stay.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = stay.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stay.Process.Kill(); _ = stay.Wait() }()
	buf := make([]byte, 4096)
	seen := ""
	for !strings.Contains(seen, "HOLD-CHILD ") {
		n, rerr := out.Read(buf)
		seen += string(buf[:n])
		if rerr != nil {
			t.Fatalf("the other process ended early: %v\n%s", rerr, seen)
		}
	}
	if !strings.Contains(seen, "HOLD-CHILD OK") {
		t.Fatalf("the other process could not open the library:\n%s", seen)
	}
	if h, err := HoldLibrary(context.Background(), path); !errors.Is(err, ErrLibraryInUse) {
		h.Release()
		t.Fatalf("hold on a WAL library another process has open: %v, want ErrLibraryInUse", err)
	}
	// A library in a transaction of another connection cannot be held either.
	busy := holdLibrary(t, "DELETE")
	db, err := sql.Open("sqlite", "file:"+busy)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if h, err := HoldLibrary(context.Background(), busy); !errors.Is(err, ErrLibraryInUse) {
		h.Release()
		t.Fatalf("hold on a library with an open write transaction: %v, want ErrLibraryInUse", err)
	}
	// No file, nothing to hold.
	if h, err := HoldLibrary(context.Background(), filepath.Join(t.TempDir(), "m.db")); !errors.Is(err, os.ErrNotExist) {
		h.Release()
		t.Fatalf("hold on a missing library: %v, want os.ErrNotExist", err)
	}
}

// volumeWithLibrary is a volume whose library has one track.
func volumeWithLibrary(t *testing.T) (root, dbPath string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "stick")
	dir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dir, "m.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY, genre TEXT, comment TEXT, label TEXT); INSERT INTO Track VALUES(7,'Old','Old comment','Old label')`); err != nil {
		t.Fatal(err)
	}
	return root, dbPath
}

func TestOpenLibraryRWRefusesAContextWithoutTheVolumeLock(t *testing.T) {
	root, _ := volumeWithLibrary(t)
	if db, err := OpenLibraryRW(context.Background(), root, ""); err == nil {
		db.Close()
		t.Fatal("the library was opened for an edit without its volume's lock")
	}
	ctx, release, err := HoldForEdit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenLibraryRW(ctx, root, "_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatalf("with the lock: %v", err)
	}
	if _, err = db.Exec(`UPDATE Track SET comment='edited' WHERE id=7`); err != nil {
		t.Errorf("an edit under the lock: %v", err)
	}
	db.Close()
	release()
	if db, err := OpenLibraryRW(ctx, root, ""); err == nil {
		db.Close()
		t.Fatal("the library was opened for an edit with a lock that was given back")
	}
}

// An edit in place waits for the volume's lock for a bounded time, then is refused with the
// message the Web Console shows, and has written nothing: not the library, not the queue.
func TestEditsInPlaceAreRefusedAsBusyWhileTheVolumeIsLocked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	root, dbPath := volumeWithLibrary(t)
	queue := pendingPath(root)
	edit := func(p *Pending) {
		p.TrackMetadata = append(p.TrackMetadata, PendingTrackMetadata{TrackID: 7, Genre: "House", Comment: "queued", Label: "EngineX"})
	}
	prev := EditLockWait
	EditLockWait = 150 * time.Millisecond
	defer func() { EditLockWait = prev }()
	unlock, err := rbstate.LockVolume(root, 0) // a bridge window, a reserve rebuild, a write-back pass
	if err != nil {
		t.Fatal(err)
	}
	isBusy := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrLibraryBusy) || !errors.Is(err, rbstate.ErrLocked) || err.Error() != "the library is busy, try again in a moment" {
			t.Errorf("%s under a held volume lock: %v, want ErrLibraryBusy with its plain message", what, err)
		}
	}
	t0 := time.Now()
	isBusy("QueuePending", QueuePending(context.Background(), root, edit))
	if waited := time.Since(t0); waited < EditLockWait || waited > EditLockWait+2*time.Second {
		t.Errorf("QueuePending waited %s for the lock, want about %s", waited, EditLockWait)
	}
	if _, err := os.Lstat(queue); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused edit left a queue file: %v", err)
	}
	// A queue that is there stays as it is, and the library too.
	b, _ := json.Marshal(Pending{TrackMetadata: []PendingTrackMetadata{{TrackID: 7, Comment: "queued earlier"}}})
	if err = os.WriteFile(queue, b, 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ApplyPending(context.Background(), root)
	isBusy("ApplyPending", err)
	_, err = FixMP3Grids(context.Background(), root, MP3GridOptions{})
	isBusy("FixMP3Grids", err)
	if after, rerr := os.ReadFile(queue); rerr != nil || string(after) != string(b) || n != 0 {
		t.Errorf("a refused ApplyPending changed the queue (applied=%d): %v", n, rerr)
	}
	comment := func() (c string) {
		db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err = db.QueryRow(`SELECT comment FROM Track WHERE id=7`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if got := comment(); got != "Old comment" {
		t.Errorf("the library was written under a held volume lock: comment=%q", got)
	}
	// The holder itself is not shut out: its context carries the lock down.
	unlock()
	ctx, release, err := rbstate.HoldVolume(context.Background(), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err = ApplyPending(ctx, root); err != nil || n != 1 {
		t.Errorf("ApplyPending by the holder of the lock: applied=%d err=%v", n, err)
	}
	if err = QueuePending(ctx, root, edit); err != nil {
		t.Errorf("QueuePending by the holder of the lock: %v", err)
	}
	release()
	if got := comment(); got != "queued earlier" {
		t.Errorf("the holder's ApplyPending did not write: comment=%q", got)
	}
	// With the lock free an edit goes through at once.
	if n, err = ApplyPending(context.Background(), root); err != nil || n != 1 || comment() != "queued" {
		t.Errorf("ApplyPending with the lock free: applied=%d err=%v comment=%q", n, err, comment())
	}
}
