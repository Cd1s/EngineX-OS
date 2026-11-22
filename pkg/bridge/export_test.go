package bridge

import (
	"context"
	"path/filepath"
	"sync"
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
