//go:build !linux

package rbstate

import "time"

// LockVolume is a no-op off Linux, where no deck runs.
func LockVolume(mount string, wait time.Duration) (unlock func(), err error) {
	return func() {}, nil
}
