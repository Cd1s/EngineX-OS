package bridge

import (
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// The next library (design 1.1 rule 1). What does not fit one window is not written into the
// stick's m.db a window at a time: the job writes a second library file beside it, which Engine
// does not open, and a window renames it into place. The number of remounts and a window's own
// work then do not grow with the library.
//
//	Database2/.rb-next.db              the next library: every row of m.db, and the bridge's rows, lists
//	                                   and staged data on top (a first library: built from the exports)
//	Database2/.rb-bridge-snapshot.swap its snapshot, once it is ready; Header.Base names the m.db it replaces
//	<stage>/<uuid>/next/               on the deck while it is built: its snapshot (with the commits'
//	                                   own next-snapshot), "base" (the snapshot of m.db it was made
//	                                   from) and "mirror" (what it took of m.db, enginedb.Mirror)
//
// Both files are siblings of what they replace: a rename inside one FAT directory, as every other
// replace on the stick. m.db itself is only read, in short statements, never written.
const nextDB = ".rb-next.db"

// errNoNext: the next library cannot be made here; the job publishes in place, a window at a time.
var errNoNext = errors.New("no next library")

func nextState(uuid string) string {
	root, _ := rbstate.StageRoot()
	return filepath.Join(root, uuid, "next")
}

func readGob(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewDecoder(f).Decode(v)
}

func writeGob(file string, v any) error {
	tmp := file + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = gob.NewEncoder(f).Encode(v)
	if e := f.Sync(); err == nil {
		err = e
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// liveLibrary says what the stick's own library is: its UUID ("" without one) and whether it
// holds anything (a track or a list; Engine's empty skeleton does not).
func liveLibrary(ctx context.Context, file string) (uuid string, content bool, err error) {
	if st, e := os.Stat(file); e != nil || st.Size() == 0 {
		return "", false, nil
	}
	db, err := openRO(file, 5000)
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	err = db.QueryRowContext(ctx, `SELECT COALESCE(uuid,''),EXISTS(SELECT 1 FROM Track) OR EXISTS(SELECT 1 FROM Playlist) FROM Information`).Scan(&uuid, &content)
	if err == nil && !content {
		var seq sql.NullInt64
		if e := db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='Track'`).Scan(&seq); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return "", false, e
		}
		if enginedb.HasRGBOverviews(filepath.Dir(file), uuid) && (!seq.Valid || seq.Int64 == 0) {
			return "", false, nil
		}
	}
	if err == nil && !content {
		content = enginedb.HasRGBOverviews(filepath.Dir(file), uuid)
	}
	return strings.TrimSpace(uuid), content, err
}

// nextIdentity is the next library's UUID and commit ID, "" when there is no file that opens.
func nextIdentity(ctx context.Context, mount, file string) (uuid string, commit int64) {
	if st, err := os.Stat(file); err != nil || st.Size() == 0 {
		return "", 0
	}
	lib, err := enginedb.OpenLibraryFile(ctx, file, mount) // a transaction the job was stopped in is rolled back
	if err != nil {
		return "", 0
	}
	defer lib.Close()
	if commit, err = lib.CommitID(ctx); err != nil {
		return "", 0
	}
	return lib.UUID(), commit
}

// discardNext removes a next library that cannot be gone on with, and its state on the deck.
func discardNext(dir, uuid string) {
	for _, name := range []string{rbstate.SwapName, nextDB, nextDB + "-journal"} {
		if err := rbstate.RemoveFile(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("rb-bridge: %s not removed: %v", name, err)
		}
	}
	if uuid != "" {
		_ = os.RemoveAll(nextState(uuid))
	}
}

// takeLive puts right the next library's snapshot after rows of the stick's library were copied
// over its own: what the bridge had written on them there is gone, so their records are the base's
// again (the snapshot of the library they came from), and the work on them is done again.
func takeLive(s, base *rbstate.Snapshot, ch enginedb.MirrorChange) {
	if len(ch.Tracks) > 0 {
		taken := make(map[int64]bool, len(ch.Tracks))
		for _, id := range ch.Tracks {
			taken[id] = true
		}
		for key, t := range s.Tracks {
			if taken[t.EngineID] {
				delete(s.Tracks, key)
			}
		}
		for key, t := range base.Tracks {
			if taken[t.EngineID] {
				c := *t
				s.Tracks[key] = &c
			}
		}
	}
	if ch.Lists {
		s.Lists = make(map[string]*rbstate.List, len(base.Lists))
		for key, l := range base.Lists {
			c := *l
			s.Lists[key] = &c
		}
	}
}

// nextBuild is the job's work on the next library.
type nextBuild struct {
	ctx      context.Context
	mount    string
	dir      string                    // Database2
	view     *View                     // the exports, as a window reads them
	live     *rbstate.Snapshot         // the stick's snapshot, nil without one
	pace     func(time.Duration) error // after each statement and commit, with the time it took
	progress func(done, total int)
}

// build brings the next library up to the view and to the stick's library: its rows, lists and
// the staged data, as a window would write them, with no window's limits. drop: applied records
// leave the stage (it is full). ready: the swap snapshot is written, a remount can swap it in.
func (n *nextBuild) build(drop, ready bool) error {
	if _, err := rbstate.StageRoot(); err != nil {
		return fmt.Errorf("%w: %v", errNoNext, err)
	}
	if _, err := compat.SafeOutputDir(n.mount, databaseSubdir); err != nil {
		return err
	}
	live, next, swap := filepath.Join(n.dir, "m.db"), filepath.Join(n.dir, nextDB), filepath.Join(n.dir, rbstate.SwapName)
	if err := rbstate.RemoveFile(swap); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err // not ready while it is written
	}
	start := time.Now()
	var res MountResult
	var snap *rbstate.Snapshot
	var state string
	for pass := 0; ; pass++ {
		state = enginedb.LibraryState(live)
		liveUUID, content, err := liveLibrary(n.ctx, live)
		if err != nil {
			return err
		}
		w := &window{ctx: n.ctx, mount: n.mount, dir: n.dir, view: n.view, st: newSteps(MountOptions{Full: true}),
			o: MountOptions{Full: true, Progress: n.progress}, res: &res, next: true, keep: !drop, rest: n.pace}
		uuid, commit := nextIdentity(n.ctx, n.mount, next)
		if _, old := os.Stat(filepath.Join(n.dir, oldStateName)); content || old == nil {
			err = n.mirror(live, next, liveUUID, uuid, commit)
		} else {
			// The stick has no library to keep: the next one is built from the exports, under the
			// UUID of Engine's skeleton when there is one.
			if h, e := rbstate.ReadHeader(nextState(uuid)); uuid == "" || e != nil || h.Commit != commit || readGob(filepath.Join(nextState(uuid), "mirror"), new(enginedb.Mirror)) == nil {
				discardNext(n.dir, uuid)
				w.snap = rbstate.New(liveUUID)
			}
		}
		if err == nil {
			err = w.run()
		}
		if err != nil {
			return err
		}
		if snap = w.snap; enginedb.LibraryState(live) == state {
			break
		}
		if pass == 3 {
			return fmt.Errorf("the stick's library kept changing while the next one was made")
		}
	}
	log.Printf("rb-bridge: next library in %s: %d rows added, %d updated, %d removed, lists changed %v, %d staged tracks applied; pending: %d rows, %d tracks' data",
		time.Since(start).Round(time.Millisecond), res.Added, res.Updated, res.Removed, res.PlaylistsChanged, res.Applied, snap.Pending.Rows, snap.Pending.Perf)
	if !ready {
		return nil
	}
	snap.Base = state
	return snap.SaveAs(swap)
}

// mirror makes the next library from the stick's (every row of it, once) or takes the rows Engine
// wrote since the last time, and puts its snapshot right.
func (n *nextBuild) mirror(live, next, liveUUID, uuid string, commit int64) error {
	sdir := nextState(liveUUID)
	if c, e := rbstate.LoadNext(sdir); e == nil && c.Commit == commit {
		_ = rbstate.Promote(sdir) // a commit of the job was cut after it
	} else if e == nil {
		_ = rbstate.DropNext(sdir)
	}
	var m enginedb.Mirror
	if h, e := rbstate.ReadHeader(sdir); uuid != liveUUID || e != nil || h.Commit != commit || readGob(filepath.Join(sdir, "mirror"), &m) != nil || m.Tracks == nil {
		// No next library of this one that can be gone on with: it is made, when the stick has the room.
		discardNext(n.dir, uuid)
		discardNext(n.dir, liveUUID)
		m = enginedb.Mirror{}
		if err := nextRoom(n.dir, liveUUID); err != nil {
			return err
		}
	}
	fresh := m.Tracks == nil
	if err := os.MkdirAll(filepath.Join(sdir, "base"), 0o700); err != nil {
		return err
	}
	ch, err := enginedb.MirrorLibrary(n.ctx, live, next, &m, n.pace)
	if errors.Is(err, enginedb.ErrMirrorConflict) {
		log.Printf("rb-bridge: %v; the next library is made again", err)
		discardNext(n.dir, liveUUID)
		return n.mirror(live, next, liveUUID, "", 0)
	}
	if err != nil {
		return err
	}
	base := n.live
	if base == nil {
		// No snapshot on the stick: an earlier build's library (adopted from the copy, once) or Engine's own.
		if base, err = rbstate.Load(filepath.Join(sdir, "base")); err != nil {
			base = rbstate.New(liveUUID)
			if _, e := os.Stat(filepath.Join(n.dir, oldStateName)); e == nil {
				if base, err = adopt(n.ctx, next, n.dir, liveUUID); err != nil {
					return err
				}
				log.Printf("rb-bridge: library of an earlier build taken over: %d tracks, %d lists", len(base.Tracks), len(base.Lists))
			}
		}
	}
	s := base
	if !fresh {
		if s, err = rbstate.Load(sdir); err != nil {
			return err
		}
		takeLive(s, base, ch)
	}
	if fresh || len(ch.Tracks) > 0 || ch.Lists {
		// The snapshot before the mirror: a cut between the two has the same rows taken again.
		_, s.Commit = nextIdentity(n.ctx, n.mount, next)
		if err = s.Save(sdir); err != nil {
			return err
		}
		log.Printf("rb-bridge: next library took %d rows of the stick's library (%d read, lists %v)", len(ch.Tracks), ch.Rows, ch.Lists)
	}
	if err = base.Save(filepath.Join(sdir, "base")); err != nil {
		return err
	}
	return writeGob(filepath.Join(sdir, "mirror"), &m)
}

// stickFree is what the filesystem of dir has free; a variable for the tests.
var stickFree = func(dir string) (int64, bool) {
	var fs syscall.Statfs_t
	if syscall.Statfs(dir, &fs) != nil {
		return 0, false
	}
	return int64(fs.Bavail) * int64(fs.Bsize), true
}

// nextRoom is errNoNext when the stick lacks the room for a next library beside its library: a
// second copy, what the stage of the library holds as files, and 64 MiB.
func nextRoom(dir, uuid string) error {
	need := int64(64 << 20)
	root, _ := rbstate.StageRoot()
	entries, _ := os.ReadDir(filepath.Join(root, uuid))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			need += info.Size()
		}
	}
	st, err := os.Stat(filepath.Join(dir, "m.db"))
	if free, ok := stickFree(dir); err == nil && ok && free < st.Size()+need {
		return fmt.Errorf("%w: the stick has %d MiB free, the library is %d MiB", errNoNext, free>>20, st.Size()>>20)
	}
	return nil
}

// swapNext makes a next library that is ready the stick's library: m.db first, then its
// snapshot, each a rename inside Database2. Rows Engine wrote to m.db since the job last looked
// are taken first (carryOver). It runs at the start of every window; without a swap snapshot it
// costs one stat.
func swapNext(ctx context.Context, mount, dir string, st *steps) bool {
	live, next, swap := filepath.Join(dir, "m.db"), filepath.Join(dir, nextDB), filepath.Join(dir, rbstate.SwapName)
	h, err := rbstate.ReadHeaderFile(swap)
	if err != nil {
		return false
	}
	if h.Rules != rbstate.Rules {
		discardNext(dir, h.LibraryUUID) // of a build with other rules
		return false
	}
	finish := func() bool {
		if err := rbstate.RenameFile(swap, filepath.Join(dir, rbstate.SnapshotName)); err != nil {
			log.Printf("rb-bridge: snapshot of the next library not put in place: %v", err)
			return false
		}
		_ = rbstate.DropNext(dir) // of a transaction on the file that is gone
		dropOldState(dir)
		_ = os.RemoveAll(nextState(h.LibraryUUID))
		return true
	}
	if _, err = os.Stat(next); err != nil {
		// A window was cut between its two renames: m.db is the next library, or the files are strays.
		if _, commit := nextIdentity(ctx, mount, live); commit == h.Commit && h.Commit != 0 {
			log.Printf("rb-bridge: the last window was cut short while it swapped the library in; done now")
			return finish()
		}
		_ = rbstate.RemoveFile(swap)
		return false
	}
	for _, side := range []string{live + "-journal", live + "-wal", next + "-journal"} {
		if s, e := os.Stat(side); e == nil && s.Size() > 0 {
			return false // an unfinished transaction: the file is opened first (this window does, or the job)
		}
	}
	if enginedb.OpenElsewhere(live) != 0 {
		return false
	}
	if h.Base != enginedb.LibraryState(live) && !carryOver(ctx, dir, &h, st, nil) {
		log.Printf("rb-bridge: the stick's library changed after the next one was made; the job takes the changes first")
		return false
	}
	cut("before swap")
	if err = rbstate.RenameFile(next, live); err != nil {
		log.Printf("rb-bridge: next library not swapped in: %v", err)
		return false
	}
	cut("swapped")
	if !finish() {
		return false
	}
	st.done("swap", "next library swapped in: %d tracks, %d lists; pending: %d rows, %d tracks' data", h.Tracks, h.Lists, h.Pending.Rows, h.Pending.Perf)
	return true
}

var mirrorLibrary = enginedb.MirrorLibrary // a variable for the test of the look's deadline

// carryOver takes into the next library the rows Engine wrote to m.db after the job made it
// ready: the DJ's edits are not lost with the file. A row taken loses what the bridge wrote on it
// in the next library; its track is pending again and published by the job's next round.
// pace is nil in a window. With it the call is the remount helper's (TakeDeckRows): paced like the
// job, and Engine may write again before the remount, so what was taken is also noted in the next
// library's state on the deck, as after a pass of the job; the window or the job reads on from it.
func carryOver(ctx context.Context, dir string, h *rbstate.Header, st *steps, pace func(time.Duration) error) bool {
	live, next, swap := filepath.Join(dir, "m.db"), filepath.Join(dir, nextDB), filepath.Join(dir, rbstate.SwapName)
	sdir := nextState(h.LibraryUUID)
	var m enginedb.Mirror
	if readGob(filepath.Join(sdir, "mirror"), &m) != nil || m.Tracks == nil {
		// Made from the exports alone, or on another deck: nothing of m.db may be lost with it.
		_, content, err := liveLibrary(ctx, live)
		return err == nil && !content
	}
	// Every row of every table is read again to find the few Engine wrote (50,000 tracks in one
	// list: 100,002 rows, 15.7 s): only when that ends in time. Else the job takes them and asks again.
	save := work{commits: 1, tracks: h.Tracks}
	if !st.fits(work{scan: m.Rows, commits: 1, tracks: h.Tracks}) {
		return false
	}
	if !st.kill.IsZero() {
		// The estimate is the replica's for rows that lie together (0.15 ms a row). In a library an
		// older build made they lie between the waveforms, and the look took 4.4 s where 2.0 s were
		// estimated (KDATA, 13,169 rows). So the look also stops where the window's rule says it
		// must: what it took until then is taken again by the job, as after a cut.
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, st.kill.Add(-spare-save.cost()))
		defer cancel()
	}
	ch, err := mirrorLibrary(ctx, live, next, &m, pace)
	if err != nil {
		log.Printf("rb-bridge: rows of the stick's library not taken: %v", err)
		return false
	}
	if pace == nil {
		st.done("carry", "%d rows the deck wrote taken into the next library (%d read, lists %v)", len(ch.Tracks), ch.Rows, ch.Lists)
		if len(ch.Tracks) == 0 && !ch.Lists {
			return true
		}
	}
	s, err := rbstate.LoadFile(swap)
	if err != nil {
		return false
	}
	base, err := rbstate.Load(filepath.Join(sdir, "base"))
	if err != nil {
		return false
	}
	takeLive(s, base, ch)
	// What the window cannot count without the exports: the job's next round does.
	s.Pending.Rows, s.Pending.Lists, s.Base = s.Pending.Rows+len(ch.Tracks), s.Pending.Lists || ch.Lists, m.State
	if err = s.SaveAs(swap); err != nil {
		return false
	}
	*h = s.Header
	if pace != nil {
		// The snapshot before the mirror, as in the job's pass: a cut between the two has the same rows taken again.
		if err = s.Save(sdir); err == nil {
			err = writeGob(filepath.Join(sdir, "mirror"), &m)
		}
		if err != nil {
			log.Printf("rb-bridge: state of the next library not saved: %v", err) // the swap snapshot stands on its own
		}
		log.Printf("rb-bridge: before the remount, %d rows the deck wrote taken into the next library in %d ms (%d read, lists %v)",
			len(ch.Tracks), time.Since(st.start).Milliseconds(), ch.Rows, ch.Lists)
	}
	return true
}

// TakeDeckRows is the remount helper's step right before the remount that swaps a next library
// in: the rows Engine wrote to the stick's library since the job made the next one ready are
// taken now, as the swap's window would take them, with the job's pacing and no window's limit.
// The window then finds the library the next one was made from and only renames, whatever the
// library's size (design 1.11 item 6). It reports whether it looked: that takes time (0.15 ms a
// row on a deck), so the helper looks at the decks again after it. With no next library waiting,
// or the stick's library as the job left it, it is two short reads.
func TakeDeckRows(ctx context.Context, mount string) bool {
	dir := filepath.Join(mount, filepath.FromSlash(databaseSubdir))
	h, err := rbstate.ReadHeaderFile(filepath.Join(dir, rbstate.SwapName))
	if err != nil || h.Rules != rbstate.Rules || h.Base == enginedb.LibraryState(filepath.Join(dir, "m.db")) {
		return false
	}
	if _, err = os.Stat(filepath.Join(dir, nextDB)); err != nil {
		return false
	}
	pace := newPacer(ctx)
	carryOver(ctx, dir, &h, newSteps(MountOptions{Full: true}), func(took time.Duration) error { return pace.afterStat(pace.active(), took) })
	return true
}
