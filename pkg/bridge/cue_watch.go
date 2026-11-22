package bridge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A deck's cue edits go back to the rekordbox stick by themselves (#321): a pass runs a few
// seconds after Engine's m.db on the stick goes quiet after a change, the same pass as the
// Eject one (temp file, fsync, rename, first backup). It lives in stream-sync, the long-running
// process that serves the write-back route: no process of its own, nothing at insert but a look
// at the mount table and two inotify watches, and the exportLibrary.db keys stay in its memory
// for the Eject's final pass. Engine's m.db is only read while quiet, never written (rule 6-4).

// CueQuiet is how long Engine's m.db stays unchanged before a pass reads it.
const CueQuiet = 5 * time.Second

// errNotQuiet drops a pass whose read of m.db overlapped a change by Engine.
var errNotQuiet = errors.New("writeback: m.db changed while it was read")

// cueEjectingDir holds the edisksd hook's marks of an Eject. The hook makes two at each Eject:
// enginex-ejecting-<hash> (pkg/prolink EjectingMark), kept until the next mount, and
// enginex-rb-ejected-<hash>, which it removes again when the unmount fails. While the second is
// there only the Eject's final pass writes the stick; after a failed Eject the watcher writes
// again (#321 review R2: it waited for the first, and wrote nothing more until the stick was
// mounted again).
var cueEjectingDir = "/run"

func cueEjecting(mount string) bool {
	h := fnv.New64a()
	h.Write([]byte(mount))
	_, err := os.Lstat(filepath.Join(cueEjectingDir, fmt.Sprintf("enginex-rb-ejected-%016x", h.Sum64())))
	return err == nil
}

// CueWatcher runs the passes of the rekordbox sticks mounted under MediaRoot.
type CueWatcher struct {
	MediaRoot string
	Quiet     time.Duration // after Engine's last write of m.db
	Retry     time.Duration // after a busy stick (an import, a conversion at mount)
	Logf      func(format string, args ...any)

	// mounts lists the writable volumes under MediaRoot; pass is WriteBackCues; grace lets
	// inotify report a write that ended the read before Settled looks.
	mounts func() []string
	pass   func(context.Context, string, WriteBackOptions) (WriteBackResult, error)
	grace  time.Duration

	mu     sync.Mutex
	sticks map[string]*cueStick
	wakeFn func()
}

type cueStick struct {
	mount   string
	session CueSession
	keys    *rekordbox.KeyCache
	events  atomic.Uint64 // Engine's writes of m.db seen
	due     time.Time     // when the next pass starts; zero for none
	running chan struct{} // closed when the pass in progress ends
	cancel  context.CancelFunc
}

// NewCueWatcher watches the volumes mounted under mediaRoot.
func NewCueWatcher(mediaRoot string) *CueWatcher {
	w := &CueWatcher{MediaRoot: mediaRoot, Quiet: CueQuiet, Retry: 15 * time.Second, Logf: log.Printf,
		pass: WriteBackCues, grace: 100 * time.Millisecond}
	w.mounts = func() []string { return mediaMounts(w.MediaRoot) }
	return w
}

func (w *CueWatcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

func (w *CueWatcher) wake() {
	w.mu.Lock()
	f := w.wakeFn
	w.mu.Unlock()
	if f != nil {
		f()
	}
}

// rekordboxStick reports a volume holding a rekordbox export (the bridge's state may come later,
// with the import's publication).
func rekordboxStick(mount string) bool {
	for _, d := range []string{"PIONEER/rekordbox", ".PIONEER/rekordbox"} {
		if st, err := os.Stat(filepath.Join(mount, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// attach starts watching mount; a stick already watched is left as it is.
func (w *CueWatcher) attach(mount string) *cueStick {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.sticks[mount]; st != nil {
		return st
	}
	if w.sticks == nil {
		w.sticks = map[string]*cueStick{}
	}
	st := &cueStick{mount: mount, keys: rekordbox.NewKeyCache()}
	w.sticks[mount] = st
	// What a cut pass left goes first, off the loop (#321 review R2: a stick that hangs at insert
	// held every stick's watcher); the stick's first pass waits for it as for a pass in progress.
	done := make(chan struct{})
	st.running = done
	go func() {
		if n := CleanWriteBackLeftovers(mount); n > 0 {
			w.logf("rb-bridge: removed %d temporary files a cut write-back left on %s", n, mount)
		}
		w.mu.Lock()
		close(done)
		if st.running == done {
			st.running = nil
		}
		w.mu.Unlock()
		w.wake()
	}()
	return st
}

// detach stops watching mount: a pass in progress is cancelled, the keys forgotten.
func (w *CueWatcher) detach(mount string) {
	w.mu.Lock()
	st := w.sticks[mount]
	delete(w.sticks, mount)
	w.mu.Unlock()
	if st == nil {
		return
	}
	if st.cancel != nil {
		st.cancel()
	}
	st.keys.Clear()
}

// changed notes a write of mount's m.db: the next pass starts Quiet after the last one.
func (w *CueWatcher) changed(mount string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st := w.sticks[mount]; st != nil {
		st.events.Add(1)
		st.due = now.Add(w.Quiet)
	}
}

// startDue starts the passes that are due and returns when the next one is (zero for none).
func (w *CueWatcher) startDue(ctx context.Context, now time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	var next time.Time
	for _, st := range w.sticks {
		if st.due.IsZero() || st.running != nil {
			continue
		}
		if st.due.After(now) {
			if next.IsZero() || st.due.Before(next) {
				next = st.due
			}
			continue
		}
		st.due = time.Time{}
		pctx, cancel := context.WithCancel(ctx)
		st.running, st.cancel = make(chan struct{}), cancel
		go w.run(pctx, st, st.running)
	}
	return next
}

// quietState is what the pass compares before and after its read: m.db's size and date, the
// journal's size (Engine truncates it after each commit) and the WAL's.
func quietState(mount string) string {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	var b strings.Builder
	for _, n := range []string{"m.db", "m.db-journal", "m.db-wal"} {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", n, st.Size(), st.ModTime().UnixNano())
		}
	}
	return b.String()
}

func journalBusy(mount string) bool {
	st, err := os.Stat(filepath.Join(mount, "Engine Library", "Database2", "m.db-journal"))
	return err == nil && st.Size() > 0
}

func (w *CueWatcher) run(ctx context.Context, st *cueStick, done chan struct{}) {
	retry := false
	defer func() {
		w.mu.Lock()
		close(done)
		if st.running == done {
			st.running, st.cancel = nil, nil
		}
		if retry && st.due.IsZero() {
			st.due = time.Now().Add(w.Retry)
		}
		w.mu.Unlock()
		w.wake()
	}()
	if cueEjecting(st.mount) {
		return // the Eject's final pass writes the stick
	}
	if journalBusy(st.mount) {
		retry = true // Engine is inside a transaction
		return
	}
	ev0, q0 := st.events.Load(), quietState(st.mount)
	_, err := w.pass(ctx, st.mount, WriteBackOptions{Session: &st.session, Keys: st.keys, Settled: func() error {
		if w.grace > 0 {
			time.Sleep(w.grace) // a write at the end of the read reaches the loop first
		}
		if st.events.Load() != ev0 || quietState(st.mount) != q0 || cueEjecting(st.mount) {
			return errNotQuiet
		}
		return nil
	}})
	switch {
	case err == nil, ctx.Err() != nil:
	case errors.Is(err, errNotQuiet):
		w.logf("rb-bridge: cue write-back on %s dropped: Engine wrote m.db while it was read", st.mount)
	case errors.Is(err, errVolumeBusy), errors.Is(err, errConverting):
		retry = true
	case errors.Is(err, os.ErrNotExist):
		// no library or no state yet: the import's publication remounts the stick
	case errors.Is(err, errUnownedClusters):
		// the stick needs a disk check: the pass's own line says so (#321 review R3)
	default:
		w.logf("rb-bridge: cue write-back on %s: %v", st.mount, err)
	}
}

// exclusive runs one pass on mount with the stick's session and keys, after the pass in
// progress (cancelled) has ended; a stick not watched gets a pass of its own.
func (w *CueWatcher) exclusive(ctx context.Context, mount string, opts WriteBackOptions) (WriteBackResult, error) {
	for {
		w.mu.Lock()
		st := w.sticks[mount]
		if st == nil {
			w.mu.Unlock()
			return w.pass(ctx, mount, opts)
		}
		if st.running == nil {
			done := make(chan struct{})
			st.running, st.due = done, time.Time{}
			w.mu.Unlock()
			opts.Session, opts.Keys = &st.session, st.keys
			res, err := w.pass(ctx, mount, opts)
			w.mu.Lock()
			close(done)
			st.running = nil
			w.mu.Unlock()
			w.wake()
			return res, err
		}
		cancel, done := st.cancel, st.running
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		select {
		case <-done:
		case <-ctx.Done():
			return WriteBackResult{}, ctx.Err()
		}
	}
}

// Final is the Eject's pass (the edisksd hook through rb-bridge --writeback-cues --on-eject):
// it stops a pass in progress and writes what is left with the keys the watcher kept.
func (w *CueWatcher) Final(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{OnEject: true})
}

// Pass is the manual write-back (the web route without final).
func (w *CueWatcher) Pass(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{})
}

// rescan attaches the rekordbox sticks newly mounted and detaches the ones gone.
func (w *CueWatcher) rescan() (added, removed []string) {
	now := map[string]bool{}
	for _, m := range w.mounts() {
		if rekordboxStick(m) {
			now[m] = true
		}
	}
	w.mu.Lock()
	var gone []string
	for m := range w.sticks {
		if !now[m] {
			gone = append(gone, m)
		}
	}
	w.mu.Unlock()
	for _, m := range gone {
		w.detach(m)
	}
	for m := range now {
		w.mu.Lock()
		known := w.sticks[m] != nil
		w.mu.Unlock()
		if !known {
			w.attach(m)
			added = append(added, m)
		}
	}
	return added, gone
}

// mediaMounts lists the writable volumes mounted right under root, from /proc/self/mountinfo.
func mediaMounts(root string) []string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return parseMediaMounts(string(b), root)
}

func parseMediaMounts(info, root string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(info, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		mp := unescapeMountinfo(f[4])
		if filepath.Dir(mp) != filepath.Clean(root) || seen[mp] {
			continue
		}
		rw := false
		for _, o := range strings.Split(f[5], ",") {
			rw = rw || o == "rw"
		}
		if rw {
			seen[mp] = true
			out = append(out, mp)
		}
	}
	return out
}

// unescapeMountinfo undoes the kernel's octal escapes (\040 for a space).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
