package bridge

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// MountOptions are one window's.
type MountOptions struct {
	// Deadline is when the hook kills the process; zero for a run by hand, which has no limit.
	Deadline time.Time
	// Publish: the window was asked for by the background job's remount, so nobody is waiting
	// for the stick and the whole window is used for staged data. At an insert the DJ waits:
	// optional work stops after insertBudget.
	Publish bool
	// Full: never take the stat-only way out, and use no budget (a run by hand).
	Full bool
	// SmartMatch adds the list "[Smart Match]" for the track with this rekordbox ID.
	SmartMatch uint32
	// Settings is the deck's settings file (the layout and the language), "" for the defaults.
	Settings string
	// StartBackground starts this mount's background job. It is called before any step that can
	// take long, so a window that is killed has still left the job running.
	StartBackground func() error
	// Progress reports tracks done out of the view's, for the deck's "loading" banner.
	Progress func(done, total int)
}

// MountResult is what a window did; the command line prints it.
type MountResult struct {
	Added, Updated, Removed int
	PlaylistsChanged        bool
	Bypassed                bool            // nothing to import, or nothing to do
	Background              bool            `json:",omitempty"` // the background job was started
	Applied                 int             `json:",omitempty"` // staged tracks written to the library
	Pending                 rbstate.Pending // what is left for the job and later windows
}

var (
	// commitReserve ends a window's work this long before the hook's kill: the commit under
	// way, the snapshot and the exit must fit in it.
	commitReserve = 2500 * time.Millisecond
	// insertBudget is how long after its start an insert's window stops doing optional work.
	insertBudget = 4 * time.Second
	// applyBatch is how many staged tracks one transaction writes at most: what a cut loses, and
	// about 7 MB for the commit to put on the stick within commitReserve.
	applyBatch = 512
	// applyReserve is the optional time a window must have left to start on one more staged track.
	applyReserve = 1500 * time.Millisecond
	// lockWait is how long a window waits for a write-back pass to finish.
	lockWait = 2 * time.Second
)

func init() {
	// Everything the bridge replaces or removes on a stick first checks that the file owns its
	// clusters in the FAT (#321 review R3).
	rbstate.UnsafeToFree = unsafeToFree
}

// steps times a window and writes one journal line a step, so the next insert on a deck shows
// where the time goes.
type steps struct {
	start, last, deadline, soft time.Time
}

func newSteps(o MountOptions) *steps {
	now := time.Now()
	s := &steps{start: now, last: now, deadline: o.Deadline}
	if !o.Publish && !o.Full {
		s.soft = now.Add(insertBudget)
	}
	return s
}

func (s *steps) done(name, format string, args ...any) {
	now := time.Now()
	log.Printf("rb-bridge: mount %s %d ms (%s)", name, now.Sub(s.last).Milliseconds(), fmt.Sprintf(format, args...))
	s.last = now
}

// hard is when the window's last commit must have started.
func (s *steps) hard() time.Time {
	if s.deadline.IsZero() {
		return time.Time{}
	}
	return s.deadline.Add(-commitReserve)
}

// more reports whether optional work may go on for at least d.
func (s *steps) more(d time.Duration) bool {
	end := time.Now().Add(d)
	if h := s.hard(); !h.IsZero() && end.After(h) {
		return false
	}
	return s.soft.IsZero() || !end.After(s.soft)
}

// must reports whether work the window owes (rows and lists) may go on for at least d.
func (s *steps) must(d time.Duration) bool {
	h := s.hard()
	return h.IsZero() || !time.Now().Add(d).After(h)
}

// writableLibrary reports that the stick's Engine Library can be written: the volume itself,
// or the library view the mount hook puts over a read-only volume (HFS+).
func writableLibrary(mount string) bool {
	const rdonly = 0x1 // ST_RDONLY
	var st syscall.Statfs_t
	if syscall.Statfs(mount, &st) != nil || st.Flags&rdonly == 0 {
		return true
	}
	return syscall.Statfs(filepath.Join(mount, "Engine Library"), &st) == nil && st.Flags&rdonly == 0
}

// libraryUUID reads Information.uuid of dir/m.db without writing anything; "" when there is no
// library or it is in the middle of a commit a cut left (a journal beside it).
func libraryUUID(dir string) string {
	p := filepath.Join(dir, "m.db")
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		return ""
	}
	if st, err := os.Stat(p + "-journal"); err == nil && st.Size() > 0 {
		return ""
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}).String())
	if err != nil {
		return ""
	}
	defer db.Close()
	var id string
	if db.QueryRow(`SELECT uuid FROM Information`).Scan(&id) != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

// ConvertAtMount is one window: it makes the stick's Engine library what its rekordbox
// libraries hold, as far as the window's time goes, before Engine sees the stick.
//
// An export that has not changed since a complete import costs a stat of each export file and
// one line of the snapshot. Anything else reads the exports once, compares them with the
// snapshot and writes the difference to m.db in place: rows and lists first, then the tracks
// the background job has staged, the ones the DJ asked for first. The job is started before
// the exports are read.
func ConvertAtMount(ctx context.Context, mount string, o MountOptions) (res MountResult, err error) {
	st := newSteps(o)
	r, err := compat.NewResolver(mount)
	if err != nil {
		return res, err
	}
	files, err := FindSources(r, mount)
	if err != nil {
		return res, err
	}
	staged := stagedStamp(mount)
	if (len(files) == 0 && staged == "") || !writableLibrary(mount) {
		return MountResult{Bypassed: true}, nil
	}
	_, hadLibrary := os.Lstat(filepath.Join(mount, "Engine Library"))
	dir, err := compat.SafeOutputDir(mount, databaseSubdir)
	if err != nil {
		return res, err
	}
	if errors.Is(hadLibrary, os.ErrNotExist) {
		// A first import that fails leaves no Engine Library behind (#53): Engine then shows the
		// stick's rekordbox library itself instead of an empty one.
		defer func() {
			if _, e := os.Stat(filepath.Join(dir, "m.db")); err != nil && e != nil {
				_ = rbstate.DropNext(dir)
				_ = os.Remove(dir)
				_ = os.Remove(filepath.Dir(dir))
			}
		}()
	}
	unlock, err := rbstate.LockVolume(mount, lockWait)
	if err != nil {
		return res, err
	}
	defer unlock()
	layout := ReadLayout(o.Settings)
	stamp := rbstate.Stamp(layout, files, staged)
	head, headErr := rbstate.ReadHeader(dir)
	_, nextErr := os.Stat(filepath.Join(dir, rbstate.SnapshotName+".next"))
	unchanged := headErr == nil && head.Stamp == stamp && head.Rules == rbstate.Rules && errors.Is(nextErr, os.ErrNotExist) &&
		!o.Full && o.SmartMatch == 0 && libraryUUID(dir) == head.LibraryUUID
	st.done("stat", "%d export files, snapshot %v, unchanged %v", len(files), headErr == nil, unchanged)
	if unchanged && head.Pending.None() {
		chores(ctx, mount, st)
		return MountResult{Bypassed: true}, nil
	}
	if o.StartBackground != nil {
		if e := o.StartBackground(); e != nil {
			log.Printf("rb-bridge: background job not started: %v", e)
		} else {
			res.Background = true
		}
	}
	rbstate.CleanTemps(dir)
	if n := CleanWriteBackLeftovers(mount); n > 0 {
		log.Printf("rb-bridge: %d files of a write-back pass that was cut short put right", n)
	}

	libs, failed, err := ReadSources(ctx, r, mount, files, nil)
	if err != nil && staged == "" {
		return res, err
	}
	for _, f := range failed {
		log.Printf("rb-bridge: %v: this library is left out of the import", f)
	}
	libs = append(libs, readStaged(mount)...)
	if len(libs) == 0 {
		if err != nil {
			return res, err
		}
		return MountResult{Bypassed: true}, nil
	}
	st.done("read", "%d libraries, %d unreadable", len(libs), len(failed))
	view, err := BuildView(libs, layout, readSetting(o.Settings, "lang"))
	if err != nil {
		return res, err
	}
	view.Stamp, view.Sources = stamp, files
	if o.SmartMatch != 0 {
		if err = addSmartMatch(view, o.SmartMatch); err != nil {
			return res, err
		}
	}
	st.done("view", "%d tracks, %d lists", len(view.Tracks), len(view.Lists))

	w := &window{ctx: ctx, mount: mount, dir: dir, r: r, view: view, st: st, o: o, res: &res,
		partial: len(failed) > 0, linkOnly: len(files) == 0}
	defer w.close()
	if err = w.open(); err != nil {
		return res, err
	}
	if w.lib == nil {
		err = w.build()
	} else {
		err = w.apply(Diff(view, w.snap))
	}
	if err != nil {
		return res, err
	}
	if err = w.applyStaged(); err != nil {
		return res, err
	}
	w.lib.Close()
	w.lib = nil
	chores(ctx, mount, st)
	res.Pending = w.snap.Pending
	log.Printf("rb-bridge: mount done in %d ms: %d added, %d updated, %d removed, lists changed %v, %d staged tracks applied; pending: %d rows, %d tracks' data, verify %v",
		time.Since(st.start).Milliseconds(), res.Added, res.Updated, res.Removed, res.PlaylistsChanged, res.Applied,
		res.Pending.Rows, res.Pending.Perf, res.Pending.Verify)
	return res, nil
}

// chores are the library upkeep every mount of a rekordbox stick does while it has time: the
// edits the Web Console queued while Engine had the library open, and the MP3 grids of
// streamed tracks an early build wrote.
func chores(ctx context.Context, mount string, st *steps) {
	if !st.more(500 * time.Millisecond) {
		return
	}
	if n, err := enginedb.ApplyPending(ctx, mount); err != nil {
		log.Printf("rb-bridge: queued library edits not applied: %v", err)
	} else if n > 0 {
		log.Printf("rb-bridge: applied %d queued library edits", n)
	}
	budget := time.Second
	if !st.soft.IsZero() {
		budget = min(budget, time.Until(st.soft))
	}
	if budget <= 0 {
		return
	}
	if r, err := enginedb.FixMP3Grids(ctx, mount, enginedb.MP3GridOptions{Budget: budget}); err != nil {
		log.Printf("rb-bridge: mp3 grid fix not finished: %v", err)
	} else if r.Corrected > 0 || r.Skipped > 0 {
		log.Printf("rb-bridge: mp3 grid fix: corrected=%d unchanged=%d skipped=%d interrupted=%v", r.Corrected, r.Unchanged, r.Skipped, r.Interrupted)
	}
}

// addSmartMatch adds the list "[Smart Match]" at the end of the root: the tracks that mix with
// the one whose rekordbox ID is ref. It refuses when no library has that track or when the
// export has a root list of that name.
func addSmartMatch(v *View, ref uint32) error {
	const title = "[Smart Match]"
	for _, l := range v.Lists {
		if l.Parent == "" && l.Name == title {
			return fmt.Errorf("%s already exists; source playlist is protected", title)
		}
	}
	current := -1
	for i := range v.Tracks {
		for _, r := range v.Tracks[i].Refs {
			if r.ID == ref && r.Kind != rbstate.LinkLibrary && current < 0 {
				current = i
			}
		}
	}
	if current < 0 {
		return fmt.Errorf("Smart Match reference track %d not found", ref)
	}
	// enginedb.SmartMatch works on IDs: give every track of the view one of its own.
	tracks := make([]rekordbox.Track, len(v.Tracks))
	for i := range v.Tracks {
		tracks[i] = v.Tracks[i].Track
		tracks[i].ID = uint32(i + 1)
	}
	var keys []string
	for _, id := range enginedb.SmartMatch(tracks, uint32(current+1)) {
		keys = append(keys, v.Tracks[id-1].Key)
	}
	after := ""
	for _, l := range v.Lists {
		if l.Parent == "" {
			after = l.Key
		}
	}
	h := sha1.New()
	l := ViewList{Key: "smart", Name: title, After: after, Tracks: keys}
	l.Hash = hashFields(h, l.Name, l.Parent, l.After, "list") + ":" + hashFields(h, l.Tracks...)
	v.Lists = append(v.Lists, l)
	return nil
}

// inApplyOrder orders staged keys: the tracks a deck loaded or the DJ looked at first, so that a
// window that runs out of time has done the ones that matter, then by row. A library's rows lie
// in m.db in the order of their IDs: in that order one page read serves many tracks, where one
// track after another by path cost a read each.
func (w *window) inApplyOrder(keys []string) []string {
	first := map[string]int{}
	items, _ := PendingPriority(w.mount)
	for i := len(items) - 1; i >= 0; i-- {
		p := items[i].Path
		if _, ok := first[rbstate.FoldPath(p)]; !ok {
			first[rbstate.FoldPath(p)] = i + 1
		}
	}
	id := func(key string) int64 {
		if t := w.snap.Tracks[key]; t != nil && t.EngineID != 0 {
			return t.EngineID
		}
		return math.MaxInt64 // no row yet: after the rows there are
	}
	sort.Slice(keys, func(a, b int) bool {
		pa, pb := first[keys[a]], first[keys[b]]
		if (pa != 0) != (pb != 0) {
			return pa != 0
		}
		if pa != pb {
			return pa < pb
		}
		if ia, ib := id(keys[a]), id(keys[b]); ia != ib {
			return ia < ib
		}
		return keys[a] < keys[b]
	})
	return keys
}
