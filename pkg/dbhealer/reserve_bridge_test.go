package dbhealer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// Regression tests for finding 1 of the Go review R11 of #336: a reserve rebuild and a bridge
// window that publish the same volume's library. Each wait watches its worker's result and a
// deadline, and the child has its own deadline.

const (
	bridgeImportChildEnv = "ENGINEX_RESERVE_BRIDGE_IMPORT_CHILD"
	bridgeImportReadyEnv = "ENGINEX_RESERVE_BRIDGE_IMPORT_READY"
	bridgeImportGoEnv    = "ENGINEX_RESERVE_BRIDGE_IMPORT_GO"
)

// lockedBuffer collects a child's output; the test may read it while the child still writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// reserve12Volume is a volume whose library is reserve12Library's: mount/Engine Library/Database2/m.db.
func reserve12Volume(t *testing.T, mount string, seed string) (live string) {
	t.Helper()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	live = filepath.Join(dir, "m.db")
	if err = os.WriteFile(live, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return live
}

// reserve12Skeleton is Engine's empty library with a page reserve of 12: no track, no list.
func reserve12Skeleton(t *testing.T) string {
	t.Helper()
	path := reserve12Seed(t, "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('skeleton-library',3,0,2)")
	return path
}

func countTracks(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil {
		t.Fatalf("count tracks of %s: %v", path, err)
	}
	return n
}

// R11 finding 1. A mount window makes a stick's first library over Engine's empty reserve-12
// skeleton. With its last checks done and its library written beside m.db, a reserve rebuild
// of the same library starts in another process. Unexcluded, the rebuild validates the
// skeleton, waits at its rename, and after the window has renamed and promoted its snapshot
// puts the rebuilt skeleton over the import: both report success and the tracks are gone.
func TestReserveNormalizeExcludesBridgeFirstImport(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	if mount := os.Getenv(bridgeImportChildEnv); mount != "" {
		bridgeFirstImportChild(t, mount)
		return
	}
	const tracks = 4
	base := t.TempDir()
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, tracks)
	live := reserve12Volume(t, mount, reserve12Skeleton(t))
	ready, goFile := filepath.Join(base, "window-at-rename"), filepath.Join(base, "window-go")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReserveNormalizeExcludesBridgeFirstImport$", "-test.v")
	cmd.Env = append(os.Environ(), bridgeImportChildEnv+"="+mount, bridgeImportReadyEnv+"="+ready, bridgeImportGoEnv+"="+goFile,
		rbstate.StageEnv+"="+filepath.Join(base, "stage"))
	cmd.WaitDelay = 5 * time.Second
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	windowDone := make(chan error, 1)
	go func() { windowDone <- cmd.Wait() }()
	var goOnce sync.Once
	letWindowGo := func() {
		goOnce.Do(func() {
			if err := os.WriteFile(goFile, nil, 0o600); err != nil {
				t.Error(err)
			}
		})
	}
	defer letWindowGo() // a failed test never leaves the window waiting

	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-windowDone:
			t.Fatalf("the window ended before its rename: %v\n%s", err, out.String())
		case <-ctx.Done():
			t.Fatalf("the window did not reach its rename\n%s", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}

	type healed struct {
		fixed []string
		err   error
	}
	atRename, renameGo := make(chan struct{}), make(chan struct{})
	var renameOnce sync.Once
	letRenameGo := func() { renameOnce.Do(func() { close(renameGo) }) }
	defer letRenameGo()
	origRename := reserveRename
	defer func() { reserveRename = origRename }()
	reserveRename = func(oldpath, newpath string) error {
		if newpath == live {
			close(atRename)
			<-renameGo
		}
		return origRename(oldpath, newpath)
	}
	healDone := make(chan healed, 1)
	go func() {
		fixed, err := NormalizeMountReserve(mount)
		healDone <- healed{fixed, err}
	}()
	var heal healed
	returned := false
	select {
	case heal = <-healDone: // excluded, or refused for another reason
		returned = true
	case <-atRename: // not excluded: it validated the skeleton and its rename is next
	case err := <-windowDone:
		t.Fatalf("the window ended while it was held at its rename: %v\n%s", err, out.String())
	case <-ctx.Done():
		t.Fatal("the reserve rebuild neither returned nor reached its rename")
	}

	letWindowGo()
	select {
	case err := <-windowDone:
		if err != nil {
			t.Fatalf("the window failed: %v\n%s", err, out.String())
		}
	case <-ctx.Done():
		t.Fatalf("the window did not finish\n%s", out.String())
	}
	if want := fmt.Sprintf("window child: Added=%d", tracks); !strings.Contains(out.String(), want) {
		t.Fatalf("the window did not report %q:\n%s", want, out.String())
	}
	if n := countTracks(t, live); n != tracks {
		t.Fatalf("after the window: %d tracks in m.db, want %d", n, tracks)
	}

	letRenameGo()
	if !returned {
		select {
		case heal = <-healDone:
		case <-ctx.Done():
			t.Fatal("the reserve rebuild did not return after its rename")
		}
	}
	reserveRename = origRename

	snap, err := rbstate.Load(filepath.Dir(live))
	if err != nil {
		t.Fatalf("the window's snapshot: %v", err)
	}
	if n := countTracks(t, live); n != tracks || len(snap.Tracks) != tracks {
		t.Errorf("the reserve rebuild published over the window's first import: m.db has %d tracks, the promoted snapshot %d, want %d in both (rebuild: fixed=%v err=%v)",
			n, len(snap.Tracks), tracks, heal.fixed, heal.err)
	}
	if !errors.Is(heal.err, rbstate.ErrLocked) || len(heal.fixed) != 0 {
		t.Errorf("reserve rebuild during the window: fixed=%v err=%v, want nothing fixed and rbstate.ErrLocked", heal.fixed, heal.err)
	}
	if _, err = os.Stat(live + ".pre-reserve"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the reserve rebuild left a .pre-reserve during the window: err=%v", err)
	}
	// With the window over the rebuild runs; the library the bridge built needs none.
	if fixed, err := NormalizeMountReserve(mount); err != nil || len(fixed) != 0 {
		t.Errorf("reserve rebuild after the window: fixed=%v err=%v, want none and no error", fixed, err)
	}
	if n := countTracks(t, live); n != tracks {
		t.Errorf("after the later rebuild: %d tracks in m.db, want %d", n, tracks)
	}
}

// bridgeFirstImportChild is the mount window of the test above. It stops where its library is
// written beside m.db and every check is done, with only the rename left, until told to go on.
func bridgeFirstImportChild(t *testing.T, mount string) {
	live := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	ready, goFile := os.Getenv(bridgeImportReadyEnv), os.Getenv(bridgeImportGoEnv)
	restore := rbstate.SetCutHooks(func(step string) {
		if step != "temp "+live {
			return
		}
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			t.Error(err)
			return
		}
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(goFile); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Error("window child: never told to go on")
				return
			}
		}
	}, nil)
	defer restore()
	res, err := bridge.ConvertAtMount(context.Background(), mount, bridge.MountOptions{})
	if err != nil {
		t.Fatalf("window child: %v", err)
	}
	fmt.Printf("window child: Added=%d\n", res.Added)
}

// The volume lock in both directions, in one process: a rebuild refuses while a window or job
// section holds the lock and leaves the library alone, and holds it itself from before its
// last validation to its rename.
func TestReserveNormalizeHoldsVolumeLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	mount := filepath.Join(t.TempDir(), "stick")
	live := reserve12Volume(t, mount, reserve12Library(t))
	before, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := NormalizeMountReserve(mount)
	unlock()
	if !errors.Is(err, rbstate.ErrLocked) || len(fixed) != 0 {
		t.Fatalf("rebuild under a held volume lock: fixed=%v err=%v, want nothing fixed and rbstate.ErrLocked", fixed, err)
	}
	if after, rerr := os.ReadFile(live); rerr != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused rebuild changed m.db (read error %v)", rerr)
	}
	for _, name := range []string{".pre-reserve", ".reserve-rebuild", ".pre-reserve.tmp"} {
		if _, serr := os.Stat(live + name); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("a refused rebuild left m.db%s: err=%v", name, serr)
		}
	}

	held := map[string]error{}
	probe := func(at string) func(string) {
		return func(p string) {
			if p != live {
				return
			}
			u, lerr := rbstate.LockVolume(mount, 0)
			if lerr == nil {
				u()
			}
			held[at] = lerr
		}
	}
	reserveBeforeCopy, reserveBeforeRename, reserveAfterRename = probe("validated"), probe("before rename"), probe("renamed")
	defer func() { reserveBeforeCopy, reserveBeforeRename, reserveAfterRename = nil, nil, nil }()
	if fixed, err = NormalizeMountReserve(mount); err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
		t.Fatalf("rebuild with the volume free: fixed=%v err=%v, want [m.db]", fixed, err)
	}
	for _, at := range []string{"validated", "before rename", "renamed"} {
		if lerr, ok := held[at]; !ok || !errors.Is(lerr, rbstate.ErrLocked) {
			t.Errorf("volume lock at %q: reached=%v err=%v, want it held by the rebuild (rbstate.ErrLocked)", at, ok, lerr)
		}
	}
	if u, lerr := rbstate.LockVolume(mount, 0); lerr != nil {
		t.Errorf("volume lock after the rebuild: %v, want it released", lerr)
	} else {
		u()
	}
}
