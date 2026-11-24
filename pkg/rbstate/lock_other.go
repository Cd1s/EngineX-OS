//go:build !linux

package rbstate

import (
	"errors"
	"time"
)

// ErrLocked: another process holds the volume's lock.
var ErrLocked = errors.New("rbstate: the volume is locked by another bridge process")

// LockVolume is a no-op off Linux, where no deck runs.
func LockVolume(mount string, wait time.Duration) (unlock func(), err error) {
	return func() {}, nil
}
