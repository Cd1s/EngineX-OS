package bridge

import "time"

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
