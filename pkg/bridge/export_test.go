package bridge

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// FinishPublishForTest exposes finishPublish to the external test package.
func FinishPublishForTest(dir string) error { return finishPublish(dir, stateName, "m.db") }

// ExportReadsForTest reports how often the drive's export was read.
func ExportReadsForTest() int64 { return exportReads.Load() }

// SetLargeExportForTest makes every export one too large to import or update while the mount
// waits: first imports and updates go on in the background (#301).
func SetLargeExportForTest() func() {
	oldAttempt, oldPublish := attemptExportBytes, maxPublishExportBytes
	attemptExportBytes, maxPublishExportBytes = -1, -1
	return func() { attemptExportBytes, maxPublishExportBytes = oldAttempt, oldPublish }
}

// SetUpdateExportForTest makes every export one too large to update while the mount waits; a
// first import is still published at the mount from the export when that fits (#253).
func SetUpdateExportForTest() func() {
	old := attemptExportBytes
	attemptExportBytes = -1
	return func() { attemptExportBytes = old }
}

// SetInPlaceUpdateForTest counts each track an update reads as costing perTrack bytes: an update
// of few tracks is left to the mounts once copying the library costs more (#253). A bridge state
// from 1 MiB counts as large enough to compact.
func SetInPlaceUpdateForTest(perTrack int64) func() {
	old, oldCompact := perTrackReadBytes, compactStateBytes
	perTrackReadBytes, compactStateBytes = perTrack, 1<<20
	return func() { perTrackReadBytes, compactStateBytes = old, oldCompact }
}

// SlowExportReadForTest makes each export read take d and shortens the mount
// read budget to budget, until the returned function restores both.
func SlowExportReadForTest(d, budget time.Duration) func() {
	oldAfter, oldBudget := afterExportRead, mountReadBudget
	afterExportRead, mountReadBudget = func() { time.Sleep(d) }, budget
	return func() { afterExportRead, mountReadBudget = oldAfter, oldBudget }
}

// HoldExportReadForTest keeps an export read from ending: reading is closed once
// rb-bridge is in it, and the read goes on after release. restore puts the hook back
// once nothing reads any more.
func HoldExportReadForTest() (reading <-chan struct{}, release, restore func()) {
	old := afterExportRead
	in, out := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	afterExportRead = func() { entered.Do(func() { close(in) }); <-out }
	return in, func() { released.Do(func() { close(out) }) }, func() { afterExportRead = old }
}

// ImportLikeEarlierBuildsForTest leaves on a volume what its first mount left before #301: the
// whole export imported at once into the live library, from the export alone past 500 tracks,
// else reading files until opts.Deadline, with .rb-bridge-pending until every track has its
// files read. Tests of how such a library is finished in place start from it.
func ImportLikeEarlierBuildsForTest(ctx context.Context, mount string, opts enginedb.Options) (enginedb.Result, error) {
	r, err := compat.NewResolver(mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	stamp, err := exportStamp(r, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	if err = ReconcileContext(ctx, r, l); err != nil {
		return enginedb.Result{}, err
	}
	mergeByPath(l)
	opts.StagedOnly, opts.Fast = stagedOnly, len(l.Tracks) > 500
	res, err := enginedb.SyncResolved(ctx, r, l, opts)
	if err != nil {
		return res, err
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if opts.Fast || res.Deferred > 0 {
		return res, writeMark(dir, pendingName, stamp)
	}
	return res, writeMark(dir, stampName, stamp)
}

// SetImportWindowForTest shortens a background pass until the returned function restores it.
func SetImportWindowForTest(d time.Duration) func() {
	old := importWindow
	importWindow = d
	return func() { importWindow = old }
}

// SetMountWindowForTest shortens the time a mount gives rb-bridge before the hook's kill
// until the returned function restores it.
func SetMountWindowForTest(d time.Duration) func() {
	old := maxMountHookBudget
	maxMountHookBudget = d
	return func() { maxMountHookBudget = old }
}

// QuickCostForTest is the quick update's SC5000 estimate (#253) and the mount's time it must fit.
func QuickCostForTest(known, fresh, entries int) (cost, mount time.Duration) {
	return quickCost(known, fresh, entries), maxMountHookBudget
}

// SetCutForTest calls f at each point between two writes an interruption can separate (#253).
func SetCutForTest(f func(name string)) func() {
	old := cutPoint
	cutPoint = f
	return func() { cutPoint = old }
}

// StagedWholeForTest reports whether tmp, a staged copy of dst, would be put back (#321 review R4).
func StagedWholeForTest(tmp, dst string) bool { return stagedWhole(tmp, dst) }

// InChildForTest runs the calling test in a process of its own and reports true when it did (the
// caller returns); in the child it returns false and the body runs as it always did.
//
// Why it exists. `go test -race ./pkg/bridge/...` took 18 minutes because the package's tests ran
// one after another in one process. Many of them cannot share a process: they set a package
// variable through the For-test hooks above (the large-export threshold, the read and mount
// windows, the write-back cut), or they read what is process-wide (the bytes the process has
// read, the key derivations it has made, the log's writer, its heap). Run in the same process,
// two of them see each other's hooks. In a process each, the body, its hooks and its assertions
// are unchanged and nothing else can see them, so these tests run side by side, each parent
// being a parallel test that waits for its child. Tests that set and read nothing of the kind
// call t.Parallel() directly.
//
// BRIDGE_TEST_INPROC=1 runs every test in this process, one at a time, as before this helper
// (to profile a test, to debug it, or to collect coverage: the parent's -coverprofile does not see
// what a child executes). The child's output is shown only when it fails; a skip is a skip.
func InChildForTest(t *testing.T) bool {
	t.Helper()
	const env = "BRIDGE_TEST_CHILD"
	if os.Getenv(env) == t.Name() || os.Getenv("BRIDGE_TEST_INPROC") != "" {
		return false
	}
	t.Parallel()
	args := []string{"-test.run=^" + regexp.QuoteMeta(t.Name()) + "$", "-test.count=1", "-test.v"}
	if d, ok := t.Deadline(); ok {
		args = append(args, "-test.timeout="+time.Until(d).Round(time.Second).String())
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), env+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in its own process: %v\n%s", t.Name(), err, out)
	}
	if bytes.Contains(out, []byte("--- SKIP: "+t.Name()+" ")) {
		t.Skipf("skipped in its own process:\n%s", out)
	}
	return true
}
