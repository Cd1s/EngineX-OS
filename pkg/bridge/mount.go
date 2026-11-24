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
	commitReserve = 2500 * time.Millisecond // the last commit starts this long before the kill (design section 3)
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
}

func newSteps(o MountOptions) *steps {
	now := time.Now()
	s := &steps{start: now, last: now}
	if !o.Deadline.IsZero() {
		s.owed = o.Deadline.Add(-commitReserve)
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

	// What the window reads of the stick next (the snapshot, megabytes, and the library's Track and
	// Playlist pages) is asked for now, so that the wait for it overlaps the exports' decryption
	// instead of following it. Nothing depends on these reads: the window does its own.
	if headErr == nil {
		go func() {
			_, _ = os.ReadFile(filepath.Join(dir, rbstate.SnapshotName))
			if db, err := openRO(filepath.Join(dir, "m.db"), 0); err == nil {
				for _, q := range []string{`SELECT id,path,COALESCE(lastEditTime,0) FROM Track WHERE path IS NOT NULL`, `SELECT id FROM Playlist`} {
					if rows, err := db.QueryContext(ctx, q); err == nil {
						for rows.Next() {
						}
						rows.Close()
					}
				}
				db.Close()
			}
		}()
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

	w := &window{ctx: ctx, mount: mount, dir: dir, view: view, st: st, o: o, res: &res,
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
