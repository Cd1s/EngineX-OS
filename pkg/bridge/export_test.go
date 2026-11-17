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
