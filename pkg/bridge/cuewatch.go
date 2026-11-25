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
	"time"

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

// CueWatcher writes a deck's cue edits back to the rekordbox sticks under a media root by
// itself, a quiet period after Engine's last write to a stick's m.db (#321).
type CueWatcher struct {
	quiet, retry time.Duration
	// poll is how often the mounts and each m.db are looked at: ten times a second cost about 2 %
	// of a deck's core with nothing to do; a second delays a pass by at most that.
	poll     time.Duration
	ejectDir string
	mounts   func() []string
	pass     func(context.Context, string, WriteBackOptions) (WriteBackResult, error)
	mu       sync.Mutex
	sticks   map[string]*cueStick
}

type cueStick struct {
	session CueSession
	events  atomic.Uint64 // changes of m.db seen: a pass that saw one during its read is dropped
	state   string        // quietState at the last poll
	due     time.Time     // when the next pass starts; zero: none is due
	running chan struct{} // closed when the pass in progress ends
	cancel  context.CancelFunc
}

func NewCueWatcher(mediaRoot string) *CueWatcher {
	return &CueWatcher{quiet: CueQuiet, retry: 15 * time.Second, poll: time.Second, ejectDir: cueEjectingDir,
		pass: WriteBackCues, mounts: func() []string { return mediaMounts(mediaRoot) }}
}

func rekordboxStick(mount string) bool {
	for _, d := range []string{"PIONEER/rekordbox", ".PIONEER/rekordbox"} {
		if st, err := os.Stat(filepath.Join(mount, d)); err == nil && st.IsDir() {
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
// one) and forgets the others.
func (w *CueWatcher) rescan() {
	now := map[string]bool{}
	for _, mount := range w.mounts() {
		if rekordboxStick(mount) {
			now[mount] = true
		}
	}
	w.forget(now)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sticks == nil {
		w.sticks = map[string]*cueStick{}
	}
	for mount := range now {
		if w.sticks[mount] == nil {
			w.sticks[mount] = &cueStick{session: CueSession{keys: newKeyCache()}, state: quietState(mount)}
			keys := w.sticks[mount].session.keys
			go func(keys *rekordbox.KeyCache) {
				if n := CleanWriteBackLeftoversWithKeys(mount, keys); n > 0 {
					log.Printf("rb-bridge: removed %d temporary files a cut write-back left on %s", n, mount)
				}
			}(keys)
		}
	}
}

// forget stops watching the sticks keep does not name: their pass is cancelled, their keys go.
func (w *CueWatcher) forget(keep map[string]bool) {
	type forgotten struct {
		stick *cueStick
		done  <-chan struct{}
	}
	var gone []forgotten
	w.mu.Lock()
	for mount, st := range w.sticks {
		if !keep[mount] {
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
		item.stick.session.keys.Clear()
	}
}

// tick notes each m.db change, and starts the passes that are due.
func (w *CueWatcher) tick(ctx context.Context, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for mount, st := range w.sticks {
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
}

func (w *CueWatcher) run(ctx context.Context, mount string, st *cueStick, done chan struct{}) {
	retry := w.cueEjecting(mount) || journalBusy(mount)
	if !retry {
		ev0, q0 := st.events.Load(), quietState(mount)
		_, err := w.pass(ctx, mount, WriteBackOptions{Session: &st.session, Settled: func() error {
			time.Sleep(100 * time.Millisecond)
			if w.cueEjecting(mount) {
				return errVolumeBusy
			}
			if st.events.Load() != ev0 || quietState(mount) != q0 {
				return errNotQuiet
			}
			return nil
		}})
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
	if retry && st.due.IsZero() {
		st.due = time.Now().Add(w.retry)
	}
}

// exclusive runs a pass of its own on mount, with the stick's session and keys, once the
// watcher's pass there (cancelled) has ended.
func (w *CueWatcher) exclusive(ctx context.Context, mount string, opts WriteBackOptions) (WriteBackResult, error) {
	for {
		w.mu.Lock()
		st := w.sticks[mount]
		if st == nil {
			w.mu.Unlock()
			return w.pass(ctx, mount, opts)
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
			st.running = nil
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

func (w *CueWatcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	defer w.forget(nil)
	w.rescan()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			w.rescan()
			w.tick(ctx, now)
		}
	}
}

// mediaMounts lists the read-write mounts right under root.
func mediaMounts(root string) []string {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || !slices.Contains(strings.Split(f[5], ","), "rw") {
			continue
		}
		if mount := unescapeMountinfo(f[4]); filepath.Dir(mount) == filepath.Clean(root) && !slices.Contains(out, mount) {
			out = append(out, mount)
		}
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
