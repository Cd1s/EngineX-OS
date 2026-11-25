package bridge

import (
	"cmp"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// MountOptions are one window's.
type MountOptions struct {
	Deadline   time.Time // the hook's kill; zero for a run by hand, which has no limit
	Publish    bool      // the job's remount asked for the window: nobody waits, staged data may use all of it
	Full       bool      // a run by hand: never stat-only, no budget
	SmartMatch uint32    // adds the list "[Smart Match]" for the track with this rekordbox ID
	Settings   string    // the deck's settings file (layout, language), "" for the defaults
	// StartBackground starts the mount's job. It is called before any step that can be long, so a
	// window the hook kills has still left the job running.
	StartBackground func() error
	Progress        func(done, total int) // for the deck's "loading" banner
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
	spare         = 3 * time.Second         // every window of the hook ends this long before the kill (design section 3)
	commitReserve = 3750 * time.Millisecond // the last commit starts this long before the kill, and ends spare before it
	insertBudget  = 4 * time.Second         // an insert's optional work stops this long after its start
	applyBatch    = 512                     // staged tracks a transaction: about 7 MB to commit within commitReserve
	applyReserve  = 1500 * time.Millisecond // optional time a window needs left to start one more staged track
	lockWait      = 2 * time.Second         // for a write-back pass to finish
)

// Every file the bridge replaces or removes on a stick must own its clusters in the FAT (#321 review R3).
func init() { rbstate.UnsafeToFree = unsafeToFree }

// steps times a window: one journal line a step, and its two limits. Work the window owes (rows,
// lists, the first batch of updates) runs until owed, optional work until optional.
type steps struct {
	start, last, owed, optional time.Time
	kill                        time.Time // zero: no hook, no limit, and nothing is left to a job
}

func newSteps(o MountOptions) *steps {
	now := time.Now()
	s := &steps{start: now, last: now}
	if !o.Deadline.IsZero() {
		s.owed = o.Deadline.Add(-commitReserve)
		if !o.Full {
			s.kill = o.Deadline
		}
	}
	s.optional = s.owed
	if soft := now.Add(insertBudget); !o.Publish && !o.Full && (s.owed.IsZero() || soft.Before(s.owed)) {
		s.optional = soft
	}
	return s
}

func (s *steps) done(name, format string, args ...any) {
	now := time.Now()
	log.Printf("rb-bridge: mount %s %d ms (%s)", name, now.Sub(s.last).Milliseconds(), fmt.Sprintf(format, args...))
	s.last = now
}

func before(limit time.Time, d time.Duration) bool {
	return limit.IsZero() || !time.Now().Add(d).After(limit)
}

func (s *steps) must(d time.Duration) bool { return before(s.owed, d) }
func (s *steps) more(d time.Duration) bool { return before(s.optional, d) }

// work is what a window has still to do, and cost what that takes on a deck. Before every step
// that grows with the library a window of the hook asks fits: work that would not end spare before
// the kill is left to the job, which has no limit (a next library, next.go). The figures are the
// deck-speed replica's (armhf at 18 % of one core, 12 ms a request to the stick, 2026-11-25) for two
// real libraries of 2,416 and 5,962 tracks; a OneLibrary key is taken as not cached.
type work struct {
	oneLibrary, deviceLibrary int64 // bytes of export files to read
	warm                      bool  // the OneLibrary key is in the cache (the job has just read the file)
	view                      int   // tracks of the exports to join into the view
	load                      int   // tracks of the snapshot to read and compare with the library
	build                     int   // rows of a first library, built on the deck and copied to the stick
	adds, removes, updates    int   // rows a diff writes in place
	lists                     int   // lists a diff writes or removes
	staged                    int   // staged tracks applied in place
	scan                      int   // rows of m.db compared with a next library's
	commits, tracks           int   // commits, each with a save of the snapshot of this many tracks
}

func (w work) cost() time.Duration {
	us := 0.0
	if w.oneLibrary > 0 && !w.warm {
		us += 5.5e6 // the key
	}
	us += 0.81 * float64(w.oneLibrary)    // 0.85 s a MiB
	us += 0.21 * float64(w.deviceLibrary) // 0.22 s a MiB
	us += 130*float64(w.view) + 100*float64(w.load) + 650*float64(w.build)
	us += 5000*float64(w.adds) + 30000*float64(w.removes) + 2000*float64(w.updates) + 5000*float64(w.lists)
	us += 33000*float64(w.staged) + 150*float64(w.scan)
	us += float64(w.commits) * (250000 + 65*float64(w.tracks))
	return time.Duration(us) * time.Microsecond
}

// fits reports that the work ends spare before the kill. A run by hand has no kill.
func (s *steps) fits(w work) bool {
	return s.kill.IsZero() || !time.Now().Add(w.cost()).After(s.kill.Add(-spare))
}

// exportWork is the reading of these export files, the view of their tracks and the reading of a
// snapshot of known tracks: what a window does before it knows the diff. The tracks are taken from
// the files' sizes (480 bytes a track in a OneLibrary, 730 in a Device Library).
func exportWork(files []rbstate.FileSig, known int) work {
	w := work{load: known, commits: 1}
	for _, f := range files {
		if strings.EqualFold(filepath.Base(f.Rel), filepath.Base(devicePDB)) {
			w.deviceLibrary += f.Size
			w.view = max(w.view, int(f.Size/730))
		} else {
			w.oneLibrary += f.Size
			w.view = max(w.view, int(f.Size/480))
		}
	}
	w.tracks = max(w.view, known)
	return w
}

// writableLibrary reports that the stick's Engine Library can be written: the volume itself, or
// the library view the mount hook puts over a read-only volume (HFS+).
func writableLibrary(mount string) bool {
	const rdonly = 0x1 // ST_RDONLY
	var st syscall.Statfs_t
	if syscall.Statfs(mount, &st) != nil || st.Flags&rdonly == 0 {
		return true
	}
	return syscall.Statfs(filepath.Join(mount, "Engine Library"), &st) == nil && st.Flags&rdonly == 0
}

// libraryUUID reads Information.uuid of dir/m.db; "" when there is no library or a cut left a
// journal beside it.
func libraryUUID(dir string) string {
	p := filepath.Join(dir, "m.db")
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		return ""
	}
	if st, err := os.Stat(p + "-journal"); err == nil && st.Size() > 0 {
		return ""
	}
	db, err := openRO(p, 2000)
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

// ConvertAtMount is one window: it makes the stick's Engine library what its rekordbox libraries
// hold, as far as the window's time goes, before Engine sees the stick. An export unchanged since
// a complete import costs a stat of each export file and one line of the snapshot.
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
		// A first import that fails leaves no Engine Library: Engine then shows the export itself (#53).
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
	swapped := swapNext(ctx, mount, dir, st)
	if swapped && o.StartBackground != nil {
		// The job drops the records the next library kept in the stage for the deck's loads, and
		// goes on with what is pending.
		if e := o.StartBackground(); e != nil {
			log.Printf("rb-bridge: background job not started: %v", e)
		} else {
			res.Background = true
		}
	}
	layout := ReadLayout(o.Settings)
	stamp := rbstate.Stamp(layout, files, staged)
	head, headErr := rbstate.ReadHeader(dir)
	_, nextErr := os.Stat(filepath.Join(dir, rbstate.SnapshotName+".next"))
	unchanged := headErr == nil && head.Stamp == stamp && head.Rules == rbstate.Rules && errors.Is(nextErr, os.ErrNotExist) &&
		!o.Full && o.SmartMatch == 0 && libraryUUID(dir) == head.LibraryUUID
	st.done("stat", "%d export files, snapshot %v, unchanged %v", len(files), headErr == nil, unchanged)
	if unchanged && head.Pending.None() {
		chores(ctx, mount, st)
		return MountResult{Bypassed: true, Background: res.Background}, nil
	}
	if unchanged && swapped && !o.Full {
		// The window that swaps a library in does nothing else, whatever the library's size.
		res.Pending = head.Pending
		return res, nil
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
	ahead := exportWork(files, head.Tracks)
	if lib, e := os.Stat(filepath.Join(dir, "m.db")); e != nil || lib.Size() == 0 {
		ahead.build = ahead.view // a first library
	}
	if !st.fits(ahead) {
		// Engine gets the stick now, with the library it has; the job reads the exports and
		// makes the next library, and its remount swaps it in.
		log.Printf("rb-bridge: mount leaves the exports to the job: reading them would take about %d ms of the %d ms left", ahead.cost().Milliseconds(), time.Until(st.kill).Milliseconds())
		res.Pending = head.Pending
		return res, nil
	}

	// The snapshot is megabytes on the stick: its read overlaps the exports' read and decryption
	// (I/O wait beside CPU work). Not when a cut window left a next snapshot: open settles it first.
	load := sync.OnceValues(func() (*rbstate.Snapshot, error) { return rbstate.Load(dir) })
	if headErr == nil && errors.Is(nextErr, os.ErrNotExist) {
		go load()
	}

	derived := rekordbox.KeyDerivations()
	libs, failed, err := ReadSources(ctx, r, mount, files, nil)
	if err != nil && staged == "" {
		return res, err
	}
	for _, f := range failed {
		log.Printf("rb-bridge: %v: this library is left out of the import", f)
	}
	if libs = append(libs, readStaged(mount)...); len(libs) == 0 {
		if err != nil {
			return res, err
		}
		return MountResult{Bypassed: true}, nil
	}
	// A OneLibrary key not in the cache costs seconds on a deck: the line says which it was.
	key := "cached"
	if rekordbox.KeyDerivations() > derived {
		key = "derived"
	}
	st.done("read", "%d libraries, %d unreadable, key %s", len(libs), len(failed), key)
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

	w := &window{ctx: ctx, mount: mount, dir: dir, view: view, st: st, o: o, res: &res, load: load,
		partial: len(failed) > 0, linkOnly: len(files) == 0}
	if err = w.run(); err != nil {
		return res, err
	}
	chores(ctx, mount, st)
	res.Pending = w.snap.Pending
	log.Printf("rb-bridge: mount done in %d ms: %d added, %d updated, %d removed, lists changed %v, %d staged tracks applied; pending: %d rows, %d tracks' data, verify %v",
		time.Since(st.start).Milliseconds(), res.Added, res.Updated, res.Removed, res.PlaylistsChanged, res.Applied,
		res.Pending.Rows, res.Pending.Perf, res.Pending.Verify)
	return res, nil
}

// chores are the upkeep every mount does while it has time: the library edits the Web Console
// queued while Engine had the library open, and the MP3 grids an early build wrote.
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
	if !st.optional.IsZero() {
		budget = min(budget, time.Until(st.optional))
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

// addSmartMatch adds the list "[Smart Match]" at the end of the root: the tracks that mix with the
// one whose rekordbox ID is ref. It refuses when the export has a root list of that name or no
// library has the track.
func addSmartMatch(v *View, ref uint32) error {
	const title = "[Smart Match]"
	after, current := "", -1
	for _, l := range v.Lists {
		if l.Parent == "" && l.Name == title {
			return fmt.Errorf("%s already exists; source playlist is protected", title)
		} else if l.Parent == "" {
			after = l.Key
		}
	}
	tracks := make([]rekordbox.Track, len(v.Tracks)) // enginedb.SmartMatch works on IDs: index + 1
	for i := range v.Tracks {
		tracks[i] = v.Tracks[i].Track
		tracks[i].ID = uint32(i + 1)
		if current < 0 && slices.ContainsFunc(v.Tracks[i].Refs, func(r rbstate.SourceRef) bool { return r.ID == ref && r.Kind != rbstate.LinkLibrary }) {
			current = i
		}
	}
	if current < 0 {
		return fmt.Errorf("Smart Match reference track %d not found", ref)
	}
	var keys []string
	for _, id := range enginedb.SmartMatch(tracks, uint32(current+1)) {
		keys = append(keys, v.Tracks[id-1].Key)
	}
	h := sha1.New()
	l := ViewList{Key: "smart", Name: title, After: after, Tracks: keys}
	l.Hash = hashFields(h, l.Name, l.Parent, l.After, "list") + ":" + hashFields(h, l.Tracks...)
	v.Lists = append(v.Lists, l)
	return nil
}

// inApplyOrder puts first the tracks a deck loaded or the DJ looked at, then the rest by row ID:
// rows lie in m.db in ID order, so one page read serves many tracks.
func (w *window) inApplyOrder(keys []string) []string {
	rank := map[string]int{}
	items, _ := PendingPriority(w.mount)
	for i := len(items) - 1; i >= 0; i-- {
		if k := rbstate.FoldPath(items[i].Path); rank[k] == 0 {
			rank[k] = i + 1
		}
	}
	order := func(key string) (int, int64) {
		r, id := rank[key], int64(math.MaxInt64) // no row yet: after the rows there are
		if r == 0 {
			r = math.MaxInt
		}
		if t := w.snap.Tracks[key]; t != nil && t.EngineID != 0 {
			id = t.EngineID
		}
		return r, id
	}
	slices.SortFunc(keys, func(a, b string) int {
		ra, ia := order(a)
		rb, ib := order(b)
		return cmp.Or(cmp.Compare(ra, rb), cmp.Compare(ia, ib), strings.Compare(a, b))
	})
	return keys
}
