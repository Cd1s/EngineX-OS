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
	oldFlag := setUSBPreparingFlagPath(filepath.Join(dir, "enginex-usb-preparing"))
	oldEject := cueEjectingDir
	cueEjectingDir = dir
	return func() {
		setUSBPreparingFlagPath(oldFlag)
		cueEjectingDir = oldEject
	}
}

// WaitWatchingForTest blocks until CueWatcher has attached mount, or times out.
func WaitWatchingForTest(w *CueWatcher, mount string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		_, ok := w.sticks[mount]
		w.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// WaitPassIdleForTest blocks until CueWatcher's stick pass for mount is no longer running.
func WaitPassIdleForTest(w *CueWatcher, mount string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		st := w.sticks[mount]
		idle := st != nil && st.running == nil
		w.mu.Unlock()
		if idle {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
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
