package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// The measurements review R13 asks for, as a test that keeps them true: the job holds the
// volume's lock only for its two short steps, each far below the two seconds a window waits
// for it. ENGINEX_R13_MEASURE_TRACKS sets the size of the library.
func TestR13BackgroundBuildHoldsTheLockOnlyBriefly(t *testing.T) {
	tracks := 300
	if v, err := strconv.Atoi(os.Getenv("ENGINEX_R13_MEASURE_TRACKS")); err == nil && v > 0 {
		tracks = v
	}
	mount := nextStick(t, tracks)
	dir := dbDir(mount)
	live := filepath.Join(dir, "m.db")
	var held []time.Duration
	nextLockHeld = func(d time.Duration) { held = append(held, d) }
	defer func() { nextLockHeld = nil }()
	t0 := time.Now()
	makeNext(t, mount)
	build := time.Since(t0)
	nextLockHeld = nil
	if len(held) != 2 {
		t.Fatalf("the build took the volume's lock %d times (%v), want its two steps", len(held), held)
	}
	for i, d := range held {
		if d > lockWait/2 {
			t.Errorf("locked step %d of the build held the lock for %s; a window waits %s for it", i+1, d, lockWait)
		}
	}
	swap, err := os.Stat(filepath.Join(dir, rbstate.SwapName))
	if err != nil {
		t.Fatal(err)
	}
	// The window that swaps it in: from its sidecar check, right after its last open-file scan,
	// to its rename; and the whole time it holds the old library.
	var checked, renamed time.Time
	defer func() { sidecarsChecked = nil }()
	sidecarsChecked = func(db string) {
		if db == live && checked.IsZero() {
			checked = time.Now()
		}
	}
	restore := rbstate.SetCutHooks(func(step string) {
		if step == "replacing "+live && renamed.IsZero() {
			renamed = time.Now()
		}
	}, nil)
	t1 := time.Now()
	res, err := ConvertAtMount(context.Background(), mount, MountOptions{})
	window := time.Since(t1)
	restore()
	if err != nil || checked.IsZero() || renamed.IsZero() {
		t.Fatalf("the window did not swap: %+v %v", res, err)
	}
	t.Logf("R13-MEASURE tracks=%d build=%s job-step-1(remove swap)=%s job-step-2(offer)=%s swap-snapshot=%dB scan-to-rename=%s window=%s",
		tracks, build.Round(time.Millisecond), held[0].Round(time.Microsecond), held[1].Round(time.Microsecond), swap.Size(),
		renamed.Sub(checked).Round(time.Microsecond), window.Round(time.Millisecond))
}
