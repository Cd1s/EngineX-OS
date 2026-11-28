//go:build !linux

package rbstate

import "os"

// ReplacedHeldElsewhere is always false off Linux, where no deck runs.
func ReplacedHeldElsewhere(old *os.File) bool { return false }

// RetireReplaced does nothing off Linux, where no deck runs.
func RetireReplaced(old *os.File) (emptied bool, err error) { return false, nil }
