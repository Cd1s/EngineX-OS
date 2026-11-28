package rbstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// LockVolume takes the lock of the volume mounted at mount, held by everything that writes the
// stick's library or export files. With wait 0 it fails at once with ErrLocked. The lock lives
// in /run, never on the stick: a file created there before a pass has checked the FAT could
// take the cluster a stale entry names (#321 review R4).
func LockVolume(mount string, wait time.Duration) (unlock func(), err error) {
	var st syscall.Stat_t
	if err = syscall.Stat(mount, &st); err != nil {
		return nil, err
	}
	dir := "/run"
	if testing.Testing() {
		dir = filepath.Dir(filepath.Clean(mount)) // a test locks beside its stick, not in the host's /run
	}
	name := fmt.Sprintf("enginex-rb-%x-%x.lock", uint64(st.Dev), uint64(st.Ino))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return flockFile(f, wait)
}

// LockDatabaseDir takes an exclusive flock on Database2 directory dir itself (creating zero
// files on the volume) so bridge swapNext and dbhealer reserve rebuilds coordinate before
// replacing m.db.
func LockDatabaseDir(dir string, wait time.Duration) (unlock func(), err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	return flockFile(f, wait)
}

func flockFile(f *os.File, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = f.Close() }, nil // the close releases the lock
		} else if errors.Is(err, syscall.EWOULDBLOCK) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		} else {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrLocked
			}
			return nil, err
		}
	}
}
