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

// ErrLocked: another process holds the volume's lock.
var ErrLocked = errors.New("rbstate: the volume is locked by another bridge process")

// lockDir is where the locks live. Never on the stick: creating a file there, before a pass
// has decided whether the stick's FAT may be written, could take the very cluster a stale
// entry names (#321 review R4).
const lockDir = "/run"

// LockVolume takes the lock of the volume mounted at mount. Everything that writes the
// stick's library or its export files holds it: the mount path, cue write-back and
// NoteWriteBack; the background job holds it while it reads the snapshot. With wait 0 it
// fails at once with ErrLocked; otherwise it retries until wait has passed.
func LockVolume(mount string, wait time.Duration) (unlock func(), err error) {
	var st syscall.Stat_t
	if err = syscall.Stat(mount, &st); err != nil {
		return nil, err
	}
	dir := lockDir
	if testing.Testing() {
		dir = filepath.Dir(filepath.Clean(mount)) // a test locks beside its stick, not in the host's /run
	}
	name := fmt.Sprintf("enginex-rb-%x-%x.lock", uint64(st.Dev), uint64(st.Ino))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrLocked
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
