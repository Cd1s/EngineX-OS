//go:build unix

package rekordbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func platformAvailableBytes(dir string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	if int64(fs.Bsize) <= 0 {
		return 0, fmt.Errorf("sqlcipher: invalid filesystem block size %d", fs.Bsize)
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}

func platformProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func platformLockTempFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func platformUnlockTempFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func platformRemoveIfUnlocked(path string) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return
	}
	if st1, e1 := f.Stat(); e1 == nil {
		if st2, e2 := os.Lstat(path); e2 == nil && os.SameFile(st1, st2) {
			_ = os.Remove(path)
		}
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
