package bridge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const CueQuiet = 5 * time.Second

var errNotQuiet = errors.New("writeback: m.db changed while it was read")
var cueEjectingDir = "/run"

// cueEjecting: the Eject's own pass is due on mount (the edisksd hook's flag).
func (w *CueWatcher) cueEjecting(mount string) bool {
	dir := w.ejectDir
	if dir == "" {
		dir = cueEjectingDir
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(mount))
	_, err := os.Lstat(filepath.Join(dir, fmt.Sprintf("enginex-rb-ejected-%016x", h.Sum64())))
	return err == nil
}

const cueWatcherDefaultFallback = 15 * time.Second

type mountEntry struct {
	path string
	id   string
}

// CueWatcher writes a deck's cue edits back to the rekordbox sticks under a media root by
// itself, a quiet period after Engine's last write to a stick's m.db (#321).
type CueWatcher struct {
	quiet, retry time.Duration
	// poll is how often active sticks' m.db are looked at: stopped when 0 rekordbox sticks
	// are mounted on Linux where /proc/self/mountinfo notifies mount/unmount via EPOLLPRI.
	poll           time.Duration
	rescanFallback time.Duration
	mediaRoot      string
	ejectDir       string
	mounts         func() []string
	mountsInfo     func() []mountEntry
	pass           func(context.Context, string, WriteBackOptions) (WriteBackResult, error)
	useMountWatch  bool
	mountEvents    <-chan struct{}
	wakeOnce       sync.Once
	wakeCh         chan struct{}
	mu             sync.Mutex
	sticks         map[string]*cueStick
	otherMounts    int
}

type cueStick struct {
	identity  string
	viewUUID  string
	session   CueSession
	events    atomic.Uint64 // changes of m.db seen: a pass that saw one during its read is dropped
	state     string        // quietState at the last poll
	due       time.Time     // when the next pass starts; zero: none is due
	running   chan struct{} // closed when the pass in progress ends
	cancel    context.CancelFunc
	clearOnce sync.Once
}

func (st *cueStick) clearKeys() {
	if st == nil {
		return
	}
	st.clearOnce.Do(func() {
		st.session.keys.Clear()
	})
}

func NewCueWatcher(mediaRoot string) *CueWatcher {
	return &CueWatcher{
		quiet:          CueQuiet,
		retry:          15 * time.Second,
		poll:           time.Second,
		rescanFallback: cueWatcherDefaultFallback,
		mediaRoot:      mediaRoot,
		ejectDir:       cueEjectingDir,
		useMountWatch:  true,
		pass:           WriteBackCues,
		mounts:         func() []string { return mediaMounts(mediaRoot) },
	}
}

func (w *CueWatcher) notifyCh() chan struct{} {
	w.wakeOnce.Do(func() {
		w.wakeCh = make(chan struct{}, 1)
	})
	return w.wakeCh
}

// Notify wakes the watcher to rescan mounted volumes for newly eligible exports or replacements.
func (w *CueWatcher) Notify() {
	select {
	case w.notifyCh() <- struct{}{}:
	default:
	}
}

func dirStatIdent(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	var dev, ino uint64
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys != nil {
		dev = uint64(sys.Dev)
		ino = uint64(sys.Ino)
	}
	return fmt.Sprintf("dev:%x:%x", dev, ino)
}

func viewUUID(mount string) string {
	h, err := rbstate.ReadHeader(filepath.Join(mount, "Engine Library", "Database2"))
	if err != nil {
		return ""
	}
	return h.LibraryUUID
}

func (w *CueWatcher) listMounts() []mountEntry {
	if w.mountsInfo != nil {
		return w.mountsInfo()
	}
	if w.mounts != nil {
		raw := w.mounts()
		out := make([]mountEntry, 0, len(raw))
		for _, m := range raw {
			out = append(out, mountEntry{path: m, id: dirStatIdent(m)})
		}
		return out
	}
	if w.mediaRoot != "" {
		return mediaMountEntries(w.mediaRoot)
	}
	return nil
}

func (w *CueWatcher) lookupStickIdentity(mount string) (identity string, view string, found bool) {
	for _, ent := range w.listMounts() {
		if ent.path == mount {
			view = viewUUID(mount)
			return ent.id + "\x00" + view, view, true
		}
	}
	view = viewUUID(mount)
	return dirStatIdent(mount) + "\x00" + view, view, false
}

const cueWatcherSettleBatch = 256

func rekordboxStick(mount string) bool {
	r, err := compat.NewResolver(mount)
	if err != nil {
		return false
	}
	files, err := FindSources(r, mount)
	return err == nil && len(files) > 0
}

func needsInitialSettle(mount string) bool {
	snap, err := rbstate.Load(filepath.Join(mount, "Engine Library", "Database2"))
	if err != nil || !snap.Header.Pending.None() {
		return false
	}
	for _, tr := range snap.Tracks {
		if !tr.Native && tr.Synced == nil {
			return true
		}
	}
	return false
}

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

// rescan watches the rekordbox sticks mounted now (cleaning what a cut pass left on each new
// one) and forgets any gone or mount/view-replaced stick.
func (w *CueWatcher) rescan() {
	now := map[string]string{}
	views := map[string]string{}
	other := 0
	for _, ent := range w.listMounts() {
		if rekordboxStick(ent.path) {
			vu := viewUUID(ent.path)
			now[ent.path] = ent.id + "\x00" + vu
			views[ent.path] = vu
		} else {
			other++
		}
	}
	w.forget(now)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.otherMounts = other
	if w.sticks == nil {
		w.sticks = map[string]*cueStick{}
	}
	for mount, id := range now {
		if w.sticks[mount] == nil {
			var due time.Time
			if needsInitialSettle(mount) {
				due = time.Now().Add(w.quiet)
			}
			w.sticks[mount] = &cueStick{
				identity: id,
				viewUUID: views[mount],
				session:  CueSession{keys: newKeyCache()},
				state:    quietState(mount),
				due:      due,
			}
			keys := w.sticks[mount].session.keys
			go func(mount string, keys *rekordbox.KeyCache) {
				if n := CleanWriteBackLeftoversWithKeys(mount, keys); n > 0 {
					log.Printf("rb-bridge: removed %d temporary files a cut write-back left on %s", n, mount)
				}
			}(mount, keys)
		}
	}
}

// forget stops watching the sticks keep does not match by mount path and mount/view identity.
func (w *CueWatcher) forget(keep map[string]string) {
	type forgotten struct {
		stick *cueStick
		done  <-chan struct{}
	}
	var gone []forgotten
	w.mu.Lock()
	for mount, st := range w.sticks {
		wantID, ok := keep[mount]
		if !ok || st.identity != wantID {
			if st.cancel != nil {
				st.cancel()
			}
			delete(w.sticks, mount)
			gone = append(gone, forgotten{stick: st, done: st.running})
		}
	}
	w.mu.Unlock()
	for _, item := range gone {
		if item.done != nil {
			<-item.done
		}
		item.stick.clearKeys()
	}
}

// tick notes each m.db change, and starts the passes that are due.
func (w *CueWatcher) tick(ctx context.Context, now time.Time) {
	w.mu.Lock()
	needRescan := false
	for mount, st := range w.sticks {
		if viewUUID(mount) != st.viewUUID {
			needRescan = true
			continue
		}
		if state := quietState(mount); state != st.state {
			st.state = state
			st.events.Add(1)
			st.due = now.Add(w.quiet)
		}
		if st.running == nil && !st.due.IsZero() && !st.due.After(now) {
			st.due = time.Time{}
			pctx, cancel := context.WithCancel(ctx)
			st.running, st.cancel = make(chan struct{}), cancel
			go w.run(pctx, mount, st, st.running)
		}
	}
	w.mu.Unlock()
	if needRescan {
		w.Notify()
	}
}

func (w *CueWatcher) run(ctx context.Context, mount string, st *cueStick, done chan struct{}) {
	retry := w.cueEjecting(mount) || journalBusy(mount)
	var moreSettle bool
	if !retry {
		ev0, q0 := st.events.Load(), quietState(mount)
		res, err := w.pass(ctx, mount, WriteBackOptions{Session: &st.session, SettleBatch: cueWatcherSettleBatch, Settled: func() error {
			time.Sleep(100 * time.Millisecond)
			if w.cueEjecting(mount) {
				return errVolumeBusy
			}
			if st.events.Load() != ev0 || quietState(mount) != q0 {
				return errNotQuiet
			}
			return nil
		}})
		moreSettle = err == nil && res.PendingSettle > 0
		switch {
		case err == nil, ctx.Err() != nil, errors.Is(err, errUnownedClusters):
		case errors.Is(err, errNotQuiet):
			retry = true
			log.Printf("rb-bridge: cue write-back on %s dropped: Engine wrote m.db while it was read", mount)
		case errors.Is(err, errVolumeBusy), errors.Is(err, errConverting):
			retry = true
		default:
			log.Printf("rb-bridge: cue write-back on %s: %v", mount, err)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	close(done)
	if st.running == done {
		st.running, st.cancel = nil, nil
	}
	if moreSettle && st.due.IsZero() {
		st.due = time.Now().Add(w.quiet)
	} else if retry && st.due.IsZero() {
		st.due = time.Now().Add(w.retry)
	}
}

// exclusive runs a pass of its own on mount, with the stick's session and keys, once any
// active watcher pass (cancelled) or replaced stick identity has ended.
func (w *CueWatcher) exclusive(ctx context.Context, mount string, opts WriteBackOptions) (WriteBackResult, error) {
	for {
		curID, curView, knownMount := w.lookupStickIdentity(mount)
		w.mu.Lock()
		st := w.sticks[mount]
		if st != nil && ((knownMount && st.identity != curID) || curView != st.viewUUID) {
			if st.cancel != nil {
				st.cancel()
			}
			delete(w.sticks, mount)
			done := st.running
			w.mu.Unlock()
			if done != nil {
				<-done
			}
			st.clearKeys()
			w.Notify()
			continue
		}
		if st == nil {
			w.mu.Unlock()
			res, err := w.pass(ctx, mount, opts)
			if !opts.OnEject && rekordboxStick(mount) {
				w.Notify()
			}
			return res, err
		}
		done := st.running
		if done == nil {
			done = make(chan struct{})
			st.running, st.due = done, time.Time{}
			w.mu.Unlock()
			opts.Session = &st.session
			res, err := w.pass(ctx, mount, opts)
			w.mu.Lock()
			close(done)
			if st.running == done {
				st.running = nil
			}
			w.mu.Unlock()
			return res, err
		}
		if st.cancel != nil {
			st.cancel()
		}
		w.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return WriteBackResult{}, ctx.Err()
		}
	}
}

// Final is the Eject's pass.
func (w *CueWatcher) Final(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{OnEject: true})
}

func (w *CueWatcher) Pass(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{})
}

func (w *CueWatcher) hasSticks() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sticks) > 0
}

func (w *CueWatcher) hasOtherMounts() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.otherMounts > 0
}

func (w *CueWatcher) Run(ctx context.Context) error {
	wakeCh := w.notifyCh()
	evCh := w.mountEvents
	stopWatch := func() {}
	if evCh == nil && w.useMountWatch {
		evCh, stopWatch = watchMountChanges(ctx)
	}
	defer stopWatch()
	defer w.forget(nil)

	fbInterval := w.rescanFallback
	if fbInterval <= 0 {
		fbInterval = cueWatcherDefaultFallback
	}

	var ticker, fbTicker *time.Ticker
	var tickC, fbC <-chan time.Time
	syncTicker := func() {
		needTick := evCh == nil || w.hasSticks()
		if needTick && ticker == nil {
			ticker = time.NewTicker(w.poll)
			tickC = ticker.C
		} else if !needTick && ticker != nil {
			ticker.Stop()
			ticker = nil
			tickC = nil
		}
		needFallback := evCh != nil && w.hasOtherMounts()
		if needFallback && fbTicker == nil {
			fbTicker = time.NewTicker(fbInterval)
			fbC = fbTicker.C
		} else if !needFallback && fbTicker != nil {
			fbTicker.Stop()
			fbTicker = nil
			fbC = nil
		}
	}
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		if fbTicker != nil {
			fbTicker.Stop()
		}
	}()

	w.rescan()
	if w.hasSticks() {
		w.tick(ctx, time.Now())
	}
	syncTicker()

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-evCh:
			if !ok {
				evCh = nil
				syncTicker()
				continue
			}
			w.rescan()
			if w.hasSticks() {
				w.tick(ctx, time.Now())
			}
			syncTicker()
		case <-wakeCh:
			w.rescan()
			if w.hasSticks() {
				w.tick(ctx, time.Now())
			}
			syncTicker()
		case <-fbC:
			w.rescan()
			if w.hasSticks() {
				w.tick(ctx, time.Now())
			}
			syncTicker()
		case now := <-tickC:
			if evCh == nil {
				w.rescan()
			}
			w.tick(ctx, now)
			syncTicker()
		}
	}
}

// mediaMountEntries lists active read-write mounts right under root with kernel/stat identity.
func mediaMountEntries(root string) []mountEntry {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	cleanRoot := filepath.Clean(root)
	var order []string
	byPath := map[string]mountEntry{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !slices.Contains(strings.Split(f[5], ","), "rw") {
			continue
		}
		mount := unescapeMountinfo(f[4])
		if filepath.Dir(mount) != cleanRoot {
			continue
		}
		if _, seen := byPath[mount]; !seen {
			order = append(order, mount)
		}
		byPath[mount] = mountEntry{
			path: mount,
			id:   fmt.Sprintf("mid:%s:%s:%s:%s", f[0], f[2], f[3], dirStatIdent(mount)),
		}
	}
	out := make([]mountEntry, 0, len(order))
	for _, m := range order {
		out = append(out, byPath[m])
	}
	return out
}

// mediaMounts lists the read-write mounts right under root.
func mediaMounts(root string) []string {
	entries := mediaMountEntries(root)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.path)
	}
	return out
}

func unescapeMountinfo(s string) string {
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
