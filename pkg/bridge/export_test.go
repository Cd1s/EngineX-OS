package bridge

import (
	"sync"
	"time"
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
