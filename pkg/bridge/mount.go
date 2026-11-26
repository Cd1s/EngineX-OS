package bridge

import (
	"cmp"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
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
	OnDemand   bool      // CDJ direct-read mode (arm32 hook present): hash-only AlbumArt, .rb-bridge-browse, no background perf pass
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
// real libraries of 2,416 and 5,962 tracks, the reads as of 7fa6a009; a OneLibrary key is taken as
// not cached. A row added costs what its index pages cost to read: 5 ms is the figure for many rows
// (330 into KDATA: 1.6 ms each); 30 rows into a cold library took 4 to 25 ms each, a read a page
// for up to the library's index pages. The loop that adds rows also stops by the clock (must).
type work struct {
	oneLibrary, deviceLibrary int64 // bytes of export files to read
	warm                      bool  // the OneLibrary key is in the cache (the job has just read the file)
	view                      int   // tracks of the exports to join into the view
	load                      int   // tracks of the snapshot to read and compare with the library
	build                     int   // rows of a first library, built on the deck and copied to the stick
	adds, removes, updates    int   // rows a diff writes in place
	lists, entries            int   // lists a diff writes or removes, and the entries of those it writes
	staged                    int   // staged tracks applied in place
	scan                      int   // rows of m.db compared with a next library's
	legacyState               int64 // bytes of a valid legacy .rb-bridge-state.db (FAT32 walk + page scatter)
	commits, tracks           int   // commits, each with a save of the snapshot of this many tracks
}

func (w work) cost() time.Duration {
	us := 0.0
	if w.oneLibrary > 0 && !w.warm {
		us += 5.8e6 // the key
	}
	us += 0.48 * float64(w.oneLibrary)    // 0.50 s a MiB
	us += 0.15 * float64(w.deviceLibrary) // 0.16 s a MiB
	us += 130*float64(w.view) + 100*float64(w.load) + 650*float64(w.build)
	us += 25000*float64(w.adds) + 30000*float64(w.removes) + 15000*float64(w.updates) + 5000*float64(w.lists) + 120*float64(w.entries)
	us += 33000*float64(w.staged) + 350*float64(w.scan)
	us += 0.0057 * float64(w.legacyState) // ~6 ms/MiB FAT32 cluster-chain traversal + page scatter on multi-GB legacy state DBs
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

const (
	legacySQLiteProbeMaxBytes = 16 << 20 // never run SQLite queries on >16 MiB legacy DBs during the mount window (FAT32 walk risk)
	legacyFallbackMaxRows     = 200_000  // cap non-SQLite size/250 fallback
)

// legacyAdoptEstimate estimates the row count and legacy-file FAT32 scan bytes for adopting an
// earlier build's .rb-bridge-state.db without letting BridgeAnalysis waveform blobs blow up the
// row estimate (a 2.71 GB legacy state DB with ~6k rows previously estimated 5,597,379 ms via
// size/250). For valid SQLite files <= 16 MiB, MAX(rowid) on BridgeState descends only the
// rightmost B-tree path (2-3 pages). Above 16 MiB, querying the state DB during the 17 s mount
// window risks a multi-second FAT32 cluster-chain walk, so rows come from m.db's Track MAX(id)
// when m.db <= 16 MiB (or exportWork's view estimate when m.db is also huge), while stateBytes
// prices the FAT32 traversal + scattered page cost (~6 ms/MiB). Non-SQLite files fall back
// conservatively to size/250 (capped).
func legacyAdoptEstimate(ctx context.Context, dir string, oldSt os.FileInfo) (rows int, stateBytes int64) {
	if oldSt == nil || oldSt.Size() <= 0 {
		return 0, 0
	}
	statePath := filepath.Join(dir, oldStateName)
	if !isSQLiteFile(statePath) {
		return int(min(oldSt.Size()/250, legacyFallbackMaxRows)), 0
	}
	stateBytes = oldSt.Size()
	if ctx.Err() != nil {
		return 0, stateBytes
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if oldSt.Size() <= legacySQLiteProbeMaxBytes {
		if n, ok := queryMaxInt(probeCtx, statePath, `SELECT COALESCE(MAX(rowid), 0) FROM BridgeState`); ok {
			return n, stateBytes
		}
		return 0, stateBytes
	}
	mdbPath := filepath.Join(dir, "m.db")
	if mSt, err := os.Stat(mdbPath); err == nil && mSt.Size() > 0 && mSt.Size() <= legacySQLiteProbeMaxBytes {
		if n, ok := queryMaxInt(probeCtx, mdbPath, `SELECT COALESCE(MAX(id), 0) FROM Track`); ok {
			return n, stateBytes
		}
	}
	return 0, stateBytes
}

func isSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [16]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false
	}
	return string(hdr[:]) == "SQLite format 3\x00"
}

func queryMaxInt(ctx context.Context, dbPath, q string) (int, bool) {
	db, err := openRO(dbPath, 100)
	if err != nil {
		return 0, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var n int64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, false
	}
	if n < 0 {
		return 0, true
	}
	return int(min(n, legacyFallbackMaxRows)), true
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
	parentCtx := ctx
	if !o.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, o.Deadline.Add(-spare))
		defer cancel()
	}
	startBG := func() {
		if res.Background || o.StartBackground == nil {
			return
		}
		if e := o.StartBackground(); e != nil {
			log.Printf("rb-bridge: background job not started: %v", e)
		} else {
			res.Background = true
		}
	}
	deadlineReached := func() bool {
		return ctx.Err() != nil && parentCtx.Err() == nil
	}
	stopAtDeadline := func() (MountResult, error) {
		startBG()
		log.Printf("rb-bridge: mount window stopped at deadline margin (%d ms before kill); remaining work left to the job", time.Until(o.Deadline).Milliseconds())
		return res, nil
	}
	r, err := compat.NewResolver(mount)
	if err != nil {
		return res, err
	}
	files, err := FindSources(r, mount)
	if err != nil {
		return res, err
	}
	if len(files) == 0 || !writableLibrary(mount) {
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
	swapped, swapDeadline := swapNext(ctx, mount, dir, st)
	if swapped && !o.OnDemand {
		// The job drops the records the next library kept in the stage for the deck's loads, and
		// goes on with what is pending.
		startBG()
	}
	if !swapped && (swapDeadline || deadlineReached()) {
		return stopAtDeadline()
	}
	layout := ReadLayout(o.Settings)
	stamp := rbstate.Stamp(layout, files, "")
	head, headErr := rbstate.ReadHeader(dir)
	if swapped && o.OnDemand && (headErr != nil || !head.Pending.None()) {
		startBG()
	}
	_, nextErr := os.Stat(filepath.Join(dir, rbstate.SnapshotName+".next"))
	unchanged := headErr == nil && head.Stamp == stamp && head.Rules == rbstate.Rules && errors.Is(nextErr, os.ErrNotExist) &&
		!o.Full && o.SmartMatch == 0 && libraryUUID(dir) == head.LibraryUUID
	st.done("stat", "%d export files, snapshot %v, unchanged %v", len(files), headErr == nil, unchanged)
	if unchanged && head.Pending.None() {
		chores(ctx, mount, st, !o.Publish)
		return MountResult{Bypassed: true, Background: res.Background}, nil
	}
	if unchanged && swapped && !o.Full {
		// The window that swaps a library in does nothing else, whatever the library's size.
		res.Pending = head.Pending
		return res, nil
	}
	if !o.OnDemand {
		startBG()
	}
	rbstate.CleanTemps(dir)
	if n := CleanWriteBackLeftoversCtx(ctx, mount); n > 0 {
		log.Printf("rb-bridge: %d files of a write-back pass that was cut short put right", n)
	}
	if deadlineReached() {
		return stopAtDeadline()
	}
	ahead := exportWork(files, head.Tracks)
	if lib, e := os.Stat(filepath.Join(dir, "m.db")); e != nil || lib.Size() == 0 {
		ahead.build = ahead.view // a first library
	} else if headErr != nil {
		if oldSt, oe := os.Stat(filepath.Join(dir, oldStateName)); oe == nil && oldSt.Size() > 0 {
			// Legacy take-over (adopt.go) reads BridgeState/BridgeWritten/Track and reconciles
			// every row. Estimate rows independently of BridgeAnalysis blob size, while still
			// pricing FAT32 cluster-chain walk + scatter for multi-GB legacy state files.
			stateRows, stateBytes := legacyAdoptEstimate(ctx, dir, oldSt)
			adoptRows := max(ahead.view, stateRows)
			ahead.load += adoptRows
			ahead.scan += adoptRows
			ahead.legacyState += stateBytes
			ahead.tracks = max(ahead.tracks, adoptRows)
		}
	}
	if !st.fits(ahead) {
		// Engine gets the stick now, with the library it has; the job reads the exports and
		// makes the next library, and its remount swaps it in.
		startBG()
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
	if errors.Is(err, errWindowDeadline) || deadlineReached() {
		return stopAtDeadline()
	}
	if err != nil {
		return res, err
	}
	for _, f := range failed {
		log.Printf("rb-bridge: %v: this library is left out of the import", f)
	}
	if len(libs) == 0 {
		return MountResult{Bypassed: true}, nil
	}
	// A OneLibrary key not in the cache costs seconds on a deck: the line says which it was.
	key := "cached"
	if rekordbox.KeyDerivations() > derived {
		key = "derived"
	}
	st.done("read", "%d libraries, %d unreadable, key %s", len(libs), len(failed), key)
	view, err := BuildViewCtx(ctx, libs, layout, readSetting(o.Settings, "lang"))
	if err != nil {
		if errors.Is(err, errWindowDeadline) || deadlineReached() {
			return stopAtDeadline()
		}
		return res, err
	}
	view.Stamp, view.Sources = stamp, files
	if o.SmartMatch != 0 {
		if err = addSmartMatchCtx(ctx, st, view, o.SmartMatch); err != nil {
			if errors.Is(err, errWindowDeadline) || deadlineReached() {
				return stopAtDeadline()
			}
			return res, err
		}
	}
	st.done("view", "%d tracks, %d lists", len(view.Tracks), len(view.Lists))

	w := &window{ctx: ctx, mount: mount, dir: dir, view: view, st: st, o: o, res: &res, load: load,
		partial: len(failed) > 0}
	if err = w.run(); err != nil {
		if errors.Is(err, errWindowDeadline) || deadlineReached() {
			if w.snap != nil {
				res.Pending = w.snap.Pending
			}
			return stopAtDeadline()
		}
		return res, err
	}
	chores(ctx, mount, st, false)
	res.Pending = w.snap.Pending
	if o.OnDemand && (w.left || !res.Pending.None()) {
		startBG()
	}
	log.Printf("rb-bridge: mount done in %d ms: %d added, %d updated, %d removed, lists changed %v, %d staged tracks applied; pending: %d rows, %d tracks' data, verify %v",
		time.Since(st.start).Milliseconds(), res.Added, res.Updated, res.Removed, res.PlaylistsChanged, res.Applied,
		res.Pending.Rows, res.Pending.Perf, res.Pending.Verify)
	return res, nil
}

var fixMP3Grids = enginedb.FixMP3Grids // a variable for the test of its budget

// chores are the upkeep every mount does while it has time: the library edits the Web Console
// queued while Engine had the library open, and the MP3 grids an early build wrote. The grid fix
// reads every row of a library that has no stamp of it: on a cold stick that grows with the
// library (1.6 s for 20,000 tracks, 6.5 s when the rows lie scattered) and its own budget does not
// stop a statement. So it is stopped: after half a second in the insert of an unchanged stick,
// where the DJ waits and nothing else is done, after a second in any other window. A fix cut
// short goes on from its cursor at the next mount.
func chores(ctx context.Context, mount string, st *steps, insert bool) {
	if !st.more(500 * time.Millisecond) {
		return
	}
	budget := time.Second
	if insert {
		budget = 500 * time.Millisecond
	}
	if !st.optional.IsZero() {
		budget = min(budget, time.Until(st.optional))
	}
	if budget <= 0 {
		return
	}
	pctx, pcancel := context.WithTimeout(ctx, budget)
	if n, err := enginedb.ApplyPending(pctx, mount); err != nil && pctx.Err() == nil {
		log.Printf("rb-bridge: queued library edits not applied: %v", err)
	} else if n > 0 {
		log.Printf("rb-bridge: applied %d queued library edits", n)
	}
	pcancel()
	if !st.optional.IsZero() {
		budget = min(budget, time.Until(st.optional))
	}
	if budget <= 0 || ctx.Err() != nil {
		return
	}
	stop, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if r, err := fixMP3Grids(stop, mount, enginedb.MP3GridOptions{Budget: budget}); err != nil && stop.Err() == nil {
		log.Printf("rb-bridge: mp3 grid fix not finished: %v", err)
	} else if r.Corrected > 0 || r.Skipped > 0 {
		log.Printf("rb-bridge: mp3 grid fix: corrected=%d unchanged=%d skipped=%d interrupted=%v", r.Corrected, r.Unchanged, r.Skipped, r.Interrupted)
	}
}

// addSmartMatch adds the list "[Smart Match]" at the end of the root: the tracks that mix with the
// one whose rekordbox ID is ref. It refuses when the export has a root list of that name or no
// library has the track.
func addSmartMatch(v *View, ref uint32) error {
	return addSmartMatchCtx(context.Background(), nil, v, ref)
}

func addSmartMatchCtx(ctx context.Context, st *steps, v *View, ref uint32) error {
	const title = "[Smart Match]"
	checkStop := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if st != nil && !st.must(0) {
			return errWindowDeadline
		}
		return nil
	}
	if err := checkStop(); err != nil {
		return err
	}
	after, current := "", -1
	for i, l := range v.Lists {
		if i&255 == 0 {
			if err := checkStop(); err != nil {
				return err
			}
		}
		if l.Parent == "" && l.Name == title {
			return fmt.Errorf("%s already exists; source playlist is protected", title)
		} else if l.Parent == "" {
			after = l.Key
		}
	}
	tracks := make([]rekordbox.Track, len(v.Tracks)) // enginedb.SmartMatch works on IDs: index + 1
	for i := range v.Tracks {
		if i&511 == 0 {
			if err := checkStop(); err != nil {
				return err
			}
		}
		tracks[i] = v.Tracks[i].Track
		tracks[i].ID = uint32(i + 1)
		if current < 0 && slices.ContainsFunc(v.Tracks[i].Refs, func(r rbstate.SourceRef) bool { return r.ID == ref }) {
			current = i
		}
	}
	if current < 0 {
		return fmt.Errorf("Smart Match reference track %d not found", ref)
	}
	matchCtx := ctx
	if st != nil && !st.owed.IsZero() {
		var cancel context.CancelFunc
		matchCtx, cancel = context.WithDeadline(ctx, st.owed)
		defer cancel()
	}
	matched, err := enginedb.SmartMatchCtx(matchCtx, tracks, uint32(current+1))
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return errWindowDeadline
		}
		return err
	}
	var keys []string
	for i, id := range matched {
		if i&511 == 0 {
			if err := checkStop(); err != nil {
				return err
			}
		}
		keys = append(keys, v.Tracks[id-1].Key)
	}
	if err := checkStop(); err != nil {
		return err
	}
	h := sha1.New()
	l := ViewList{Key: "smart", Name: title, After: after, Tracks: keys}
	l.Hash = hashFields(h, l.Name, l.Parent, l.After, "list") + ":" + hashFields(h, l.Tracks...)
	v.Lists = append(v.Lists, l)
	return nil
}

// inApplyOrder sorts staged tracks by row ID: rows lie in m.db in that order, so one page read
// serves many tracks. A track without a row yet comes last. (The tracks a deck asked for have no
// place of their own here: the job stages them first and acknowledges the request, so by the
// window of its remount the queue names none of them; and what one window cannot apply goes into
// a next library whole.)
func (w *window) inApplyOrder(keys []string) []string {
	id := func(key string) int64 {
		if t := w.snap.Tracks[key]; t != nil && t.EngineID != 0 {
			return t.EngineID
		}
		return math.MaxInt64
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(id(a), id(b)), strings.Compare(a, b)) })
	return keys
}
