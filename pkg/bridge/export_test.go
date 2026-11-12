package bridge

import (
	"testing"
	"time"
)

// FinishPublishForTest exposes finishPublish to the external test package.
func FinishPublishForTest(dir string) error { return finishPublish(dir, stateName, "m.db") }

// WithReadExportHookForTest installs an export-read hook and custom mountReadBudget for a test.
func WithReadExportHookForTest(tb testing.TB, budget time.Duration, fn func()) {
	tb.Helper()
	prevHook := readExportHook
	prevBudget := mountReadBudget
	readExportHook = fn
	if budget > 0 {
		mountReadBudget = budget
	}
	tb.Cleanup(func() {
		readExportHook = prevHook
		mountReadBudget = prevBudget
	})
}
