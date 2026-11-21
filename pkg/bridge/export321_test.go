package bridge

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"time"
)

// CueWatcherForTest watches sticks as if they were the volumes mounted (no mount table), with
// the quiet time and the retry after a busy stick given.
func CueWatcherForTest(sticks []string, quiet, retry time.Duration) *CueWatcher {
	w := NewCueWatcher(filepath.Dir(sticks[0]))
	w.Quiet, w.Retry = quiet, retry
	w.mounts = func() []string { return sticks }
	return w
}

// CuePass is the watcher's pass.
type CuePass = func(context.Context, string, WriteBackOptions) (WriteBackResult, error)

// WrapCuePassForTest puts wrap around the watcher's pass.
func WrapCuePassForTest(w *CueWatcher, wrap func(next CuePass) CuePass) { w.pass = wrap(w.pass) }

// SetCueRunDirForTest moves the conversion flag and the Eject marks from /run to dir.
func SetCueRunDirForTest(dir string) func() {
	oldFlag, oldEject := usbPreparingFlagPath, cueEjectingDir
	usbPreparingFlagPath, cueEjectingDir = filepath.Join(dir, "enginex-usb-preparing"), dir
	return func() { usbPreparingFlagPath, cueEjectingDir = oldFlag, oldEject }
}

// ErrNotQuietForTest is the error of a pass whose read overlapped a write by Engine.
var ErrNotQuietForTest = errNotQuiet

// EjectingNameForTest is the name of the edisksd hook's Eject mark of mount.
func EjectingNameForTest(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return fmt.Sprintf("enginex-ejecting-%016x", h.Sum64())
}
