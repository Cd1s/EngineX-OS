package dbhealer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// A mount's heal writes the library in place and may replace it: it runs under the volume's
// lock like every writer of a volume's library (#336 Go review R13). Held by another program,
// the heal waits a bounded time, then touches nothing and says so.
func TestHealMountRunsUnderTheVolumeLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the volume lock is Linux only")
	}
	mount := filepath.Join(t.TempDir(), "stick")
	live := reserve12Volume(t, mount, reserve12Library(t))
	before, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	prev := HealLockWait
	HealLockWait = 150 * time.Millisecond
	defer func() { HealLockWait = prev }()

	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, err = HealMountDataOnly(mount, 0)
	waited := time.Since(t0)
	unlock()
	if !errors.Is(err, rbstate.ErrLocked) {
		t.Fatalf("heal under a held volume lock: %v, want rbstate.ErrLocked", err)
	}
	if waited < HealLockWait || waited > HealLockWait+3*time.Second {
		t.Errorf("the heal waited %s for the lock, want about %s", waited, HealLockWait)
	}
	if after, rerr := os.ReadFile(live); rerr != nil || !bytes.Equal(before, after) {
		t.Errorf("a refused heal changed m.db (read error %v)", rerr)
	}
	if _, serr := os.Stat(live + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a refused heal left a .pre-reserve: %v", serr)
	}

	// The heal holds the lock while it works: from its reserve rebuild's last validation to the
	// rename, a second holder is refused. Then it gives the lock back.
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
	reserveBeforeCopy, reserveAfterRename = probe("rebuild"), probe("renamed")
	defer func() { reserveBeforeCopy, reserveAfterRename = nil, nil }()
	r, err := HealMountDataOnly(mount, 0)
	if err != nil || len(r.ReserveFixed) != 1 {
		t.Fatalf("heal with the lock free: fixed=%v err=%v", r.ReserveFixed, err)
	}
	for _, at := range []string{"rebuild", "renamed"} {
		if lerr, ok := held[at]; !ok || !errors.Is(lerr, rbstate.ErrLocked) {
			t.Errorf("volume lock at %q: reached=%v err=%v, want it held by the heal", at, ok, lerr)
		}
	}
	if u, lerr := rbstate.LockVolume(mount, 0); lerr != nil {
		t.Errorf("the heal kept the volume's lock: %v", lerr)
	} else {
		u()
	}
}
