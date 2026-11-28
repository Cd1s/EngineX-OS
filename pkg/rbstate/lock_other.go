//go:build !linux

package rbstate

import (
	"path/filepath"
	"time"
)

// LockVolume is a no-op off Linux, where no deck runs.
func LockVolume(mount string, wait time.Duration) (unlock func(), err error) {
	return func() {}, nil
}

// LockDatabaseDir is a no-op off Linux, where no deck runs.
func LockDatabaseDir(dir string, wait time.Duration) (unlock func(), err error) {
	return func() {}, nil
}

func lockVolumeID(mount, id string, wait time.Duration) (unlock func(), err error) {
	return func() {}, nil
}

// volumeID names the volume by its path off Linux, where no deck runs.
func volumeID(mount string) (string, error) {
	abs, err := filepath.Abs(mount)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
