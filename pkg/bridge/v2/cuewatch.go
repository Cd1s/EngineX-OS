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

const CueQuiet = 5 * time.Second

var errNotQuiet = errors.New("writeback: m.db changed while it was read")
var cueEjectingDir = "/run"

func cueEjecting(mount string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mount))
	_, err := os.Lstat(filepath.Join(cueEjectingDir, fmt.Sprintf("enginex-rb-ejected-%016x", h.Sum64())))
	return err == nil
}

type CueWatcher struct {
	MediaRoot string
	Quiet     time.Duration
	Retry     time.Duration
	Logf      func(string, ...any)
	mounts    func() []string
	pass      func(context.Context, string, WriteBackOptions) (WriteBackResult, error)
	grace     time.Duration
	mu        sync.Mutex
	sticks    map[string]*cueStick
}

type cueStick struct {
	mount   string
	session CueSession
	keys    *rekordbox.KeyCache
	events  atomic.Uint64
	state   string
	due     time.Time
	running chan struct{}
	cancel  context.CancelFunc
}

func NewCueWatcher(mediaRoot string) *CueWatcher {
	w := &CueWatcher{MediaRoot: mediaRoot, Quiet: CueQuiet, Retry: 15 * time.Second, Logf: log.Printf, pass: WriteBackCues, grace: 100 * time.Millisecond}
	w.mounts = func() []string { return mediaMounts(mediaRoot) }
	return w
}

func (w *CueWatcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

func rekordboxStick(mount string) bool {
	for _, d := range []string{"PIONEER/rekordbox", ".PIONEER/rekordbox"} {
		if st, err := os.Stat(filepath.Join(mount, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func (w *CueWatcher) attach(mount string) *cueStick {
	w.mu.Lock()
	if st := w.sticks[mount]; st != nil {
		w.mu.Unlock()
		return st
	}
	if w.sticks == nil {
		w.sticks = map[string]*cueStick{}
	}
	st := &cueStick{mount: mount, keys: newKeyCache(), state: quietState(mount)}
	w.sticks[mount] = st
	w.mu.Unlock()
	go func() {
		if n := CleanWriteBackLeftovers(mount); n > 0 {
			w.logf("rb-bridge: removed %d temporary files a cut write-back left on %s", n, mount)
		}
	}()
	return st
}

func (w *CueWatcher) detach(mount string) {
	w.mu.Lock()
	st := w.sticks[mount]
	delete(w.sticks, mount)
	cancel := func() {}
	if st != nil && st.cancel != nil {
		cancel = st.cancel
	}
	w.mu.Unlock()
	if st != nil {
		cancel()
		st.keys.Clear()
	}
}

func (w *CueWatcher) changed(mount string, now time.Time) {
	w.mu.Lock()
	if st := w.sticks[mount]; st != nil {
		st.events.Add(1)
		st.due = now.Add(w.Quiet)
	}
	w.mu.Unlock()
}

func (w *CueWatcher) startDue(ctx context.Context, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, st := range w.sticks {
		if st.running != nil || st.due.IsZero() || st.due.After(now) {
			continue
		}
		st.due = time.Time{}
		pctx, cancel := context.WithCancel(ctx)
		st.running, st.cancel = make(chan struct{}), cancel
		go w.run(pctx, st, st.running)
	}
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
	}()
	if cueEjecting(st.mount) || journalBusy(st.mount) {
		retry = true
		return
	}
	ev0, q0 := st.events.Load(), quietState(st.mount)
	_, err := w.pass(ctx, st.mount, WriteBackOptions{Session: &st.session, Keys: st.keys, Settled: func() error {
		if w.grace > 0 {
			time.Sleep(w.grace)
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
	case errors.Is(err, errUnownedClusters):
	default:
		w.logf("rb-bridge: cue write-back on %s: %v", st.mount, err)
	}
}

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

func (w *CueWatcher) Final(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{OnEject: true})
}

func (w *CueWatcher) Pass(ctx context.Context, mount string) (WriteBackResult, error) {
	return w.exclusive(ctx, mount, WriteBackOptions{})
}

func (w *CueWatcher) rescan() {
	now := map[string]bool{}
	for _, mount := range w.mounts() {
		if rekordboxStick(mount) {
			now[mount] = true
		}
	}
	w.mu.Lock()
	var gone []string
	for mount := range w.sticks {
		if !now[mount] {
			gone = append(gone, mount)
		}
	}
	w.mu.Unlock()
	for _, mount := range gone {
		w.detach(mount)
	}
	for mount := range now {
		w.attach(mount)
	}
}

func (w *CueWatcher) Run(ctx context.Context) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	w.rescan()
	for {
		select {
		case <-ctx.Done():
			w.mu.Lock()
			var mounts []string
			for mount := range w.sticks {
				mounts = append(mounts, mount)
			}
			w.mu.Unlock()
			for _, mount := range mounts {
				w.detach(mount)
			}
			return nil
		case now := <-tick.C:
			w.rescan()
			w.mu.Lock()
			for mount, st := range w.sticks {
				state := quietState(mount)
				if state != st.state {
					st.state = state
					st.events.Add(1)
					st.due = now.Add(w.Quiet)
				}
			}
			w.mu.Unlock()
			w.startDue(ctx, now)
		}
	}
}

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
		mount := unescapeMountinfo(f[4])
		if filepath.Dir(mount) != filepath.Clean(root) || seen[mount] {
			continue
		}
		for _, option := range strings.Split(f[5], ",") {
			if option == "rw" {
				seen[mount] = true
				out = append(out, mount)
				break
			}
		}
	}
	return out
}

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
