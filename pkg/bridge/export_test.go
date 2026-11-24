package bridge

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// CueWatcherForTest watches explicit mounts without consulting mountinfo.
func CueWatcherForTest(sticks []string, quiet, retry time.Duration) *CueWatcher {
	w := NewCueWatcher(filepath.Dir(sticks[0]))
	w.quiet, w.retry, w.poll = quiet, retry, 100*time.Millisecond
	w.mounts = func() []string { return sticks }
	return w
}

type CuePass = func(context.Context, string, WriteBackOptions) (WriteBackResult, error)

func WrapCuePassForTest(w *CueWatcher, wrap func(next CuePass) CuePass) { w.pass = wrap(w.pass) }

func SetCueRunDirForTest(dir string) func() {
	oldFlag, oldEject := usbPreparingFlagPath, cueEjectingDir
	usbPreparingFlagPath, cueEjectingDir = filepath.Join(dir, "enginex-usb-preparing"), dir
	return func() { usbPreparingFlagPath, cueEjectingDir = oldFlag, oldEject }
}

func SetWriteBackCutForTest(cut func(step string)) func() {
	old := writeBackCut
	writeBackCut = cut
	return func() { writeBackCut = old }
}

var ErrNotQuietForTest = errNotQuiet
var ErrUnownedClustersForTest = errUnownedClusters

func EjectingNameForTest(mount string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mount))
	return fmt.Sprintf("enginex-ejecting-%016x", h.Sum64())
}

func EjectedNameForTest(mount string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mount))
	return fmt.Sprintf("enginex-rb-ejected-%016x", h.Sum64())
}

// StampWriteBackForTest refreshes a test snapshot's export stamp after a fixture import.
func StampWriteBackForTest(mount string) error {
	resolver, err := compat.NewResolver(mount)
	if err != nil {
		return err
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	snap, err := rbstate.Load(dbDir)
	if err != nil {
		return err
	}
	sources, err := FindSources(resolver, mount)
	if err != nil {
		return err
	}
	snap.Header.Sources = sources
	snap.Header.Stamp = rbstate.Stamp(snap.Header.Layout, sources, stagedStamp(mount))
	return snap.Save(dbDir)
}
