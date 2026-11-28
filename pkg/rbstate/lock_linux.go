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
	id, err := volumeID(mount)
	if err != nil {
		return nil, err
	}
	return lockVolumeID(mount, id, wait)
}

func lockVolumeID(mount, id string, wait time.Duration) (unlock func(), err error) {
	dir := "/run"
	if testing.Testing() {
		dir = filepath.Dir(filepath.Clean(mount)) // a test locks beside its stick, not in the host's /run
	}
	f, err := os.OpenFile(filepath.Join(dir, "enginex-rb-"+id+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return flockFile(f, wait)
}

// volumeID names the volume mounted at mount, the same for every way into it (measured on
// Linux 6.12, #336 Go review R13):
//   - the device and inode of its root: a FAT, exFAT, ext4 or HFS+ volume has the same pair in the
//     hook's private pre-mount, in the visible mount, in a bind of its root, and an lklfuse
//     mount keeps its pair when it is moved from its stage to the target;
//   - but of "Engine Library" when that is on another file system than the root. That is the
//     read-only HFS+ view: its library is a folder of /data bound into each view, and a stick
//     without a library of its own gets a new tmpfs root for every view.
//
// "Engine Library" on the root's own file system is never the name: FAT and exFAT give a folder
// another inode number at every mount.
func volumeID(mount string) (string, error) {
	var root, lib syscall.Stat_t
	if err := syscall.Stat(mount, &root); err != nil {
		return "", err
	}
	dev, ino := uint64(root.Dev), uint64(root.Ino)
	if err := syscall.Lstat(filepath.Join(mount, "Engine Library"), &lib); err == nil && lib.Mode&syscall.S_IFMT == syscall.S_IFDIR && uint64(lib.Dev) != dev {
		dev, ino = uint64(lib.Dev), uint64(lib.Ino)
	}
	return fmt.Sprintf("%x-%x", dev, ino), nil
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
