//go:build !unix

package rekordbox

import (
	"math"
	"os"
)

func platformAvailableBytes(string) (uint64, error) {
	return math.MaxUint64, nil
}

func platformProcessAlive(int) bool {
	return true
}

func platformLockTempFile(*os.File) error {
	return nil
}

func platformUnlockTempFile(*os.File) {}

func platformRemoveIfUnlocked(path string) {
	_ = os.Remove(path)
}
