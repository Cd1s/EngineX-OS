package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
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
		if enginedb.HasRGBOverviews(filepath.Dir(file), uuid) || (seq.Valid && seq.Int64 > 0) {
			return "", false, nil
		}
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
	for _, name := range []string{rbstate.SwapName, rbstate.BrowseSwapName, nextDB, nextDB + "-journal", nextDB + "-wal", nextDB + "-shm"} {
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
		baseByID := make(map[int64]*rbstate.Track, len(base.Tracks))
		for _, t := range base.Tracks {
			if taken[t.EngineID] {
				baseByID[t.EngineID] = t
			}
		}
		seen := make(map[int64]bool, len(baseByID))
		for _, current := range s.Tracks {
			if old := baseByID[current.EngineID]; old != nil {
				seen[old.EngineID] = true
				current.EngineID = old.EngineID
				current.Row = old.Row
				current.Rating = old.Rating
				current.Perf = old.Perf
				current.Built = old.Built
				current.Written = bytes.Clone(old.Written)
				current.Synced = bytes.Clone(old.Synced)
				current.SyncedColorTag = bytes.Clone(old.SyncedColorTag)
				current.SyncedHotColors = bytes.Clone(old.SyncedHotColors)
				current.SyncedLoopColors = bytes.Clone(old.SyncedLoopColors)
				current.Edit = old.Edit
				current.Native = old.Native
			}
		}
		for key, old := range base.Tracks {
			if baseByID[old.EngineID] != nil && !seen[old.EngineID] {
				copy := *old
				s.Tracks[key] = &copy
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

// errDeckBusy: a deck is playing or loading; whole-DB writes (.rb-next.db copy/fsync)
// wait until no deck is active and abort immediately if playback starts mid-build.
var errDeckBusy = errors.New("deck active")

// nextBuild is the job's work on the next library.
type nextBuild struct {
	ctx      context.Context
	mount    string
	dir      string                    // Database2
	view     *View                     // the exports, as a window reads them
	live     *rbstate.Snapshot         // the stick's snapshot, nil without one
	pace     func(time.Duration) error // after each statement and commit, with the time it took
	busy     func() bool               // true while any deck is playing, cueing, scratching, or loading
	progress func(done, total int)
	onDemand bool
}

func (n *nextBuild) isBusy() bool {
	if n.busy != nil {
		return n.busy()
	}
	return IsDeckActive() || IsDeckLoading() || IsEngineCopying()
}

func (n *nextBuild) nextPace(took time.Duration) error {
	if err := n.ctx.Err(); err != nil {
		return err
	}
	if n.isBusy() {
		return errDeckBusy
	}
	if n.pace != nil {
		return n.pace(took)
	}
	return nil
}

// build brings the next library up to the view and to the stick's library: its rows, lists and
// the staged data, as a window would write them, with no window's limits. drop: applied records
// leave the stage (it is full). ready: the swap snapshot is written, a remount can swap it in.
func (n *nextBuild) build(drop, ready bool) error {
	if n.isBusy() {
		return errDeckBusy
	}
	if _, err := rbstate.StageRoot(); err != nil {
		return fmt.Errorf("%w: %v", errNoNext, err)
	}
	if _, err := compat.SafeOutputDir(n.mount, databaseSubdir); err != nil {
		return err
	}
	live, next, swap := filepath.Join(n.dir, "m.db"), filepath.Join(n.dir, nextDB), filepath.Join(n.dir, rbstate.SwapName)
	// The next library belongs to a window while its swap snapshot is there, and to the job
	// while it is not. The two changes of owner are made under the volume's lock: here the
	// job takes it back (a window that is swapping it in finishes first), and offer gives a
	// finished one to the windows. The build in between is the job's own and needs no lock.
	if err := n.locked(func() error {
		if err := rbstate.RemoveFile(swap); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err // not ready while it is written
		}
		return nil
	}); err != nil {
		return err
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
		w := &window{ctx: n.ctx, mount: n.mount, dir: n.dir, view: n.view, st: newSteps(MountOptions{Full: true, OnDemand: n.onDemand}),
			o: MountOptions{Full: true, OnDemand: n.onDemand, Progress: n.progress}, res: &res, next: true, keep: !drop, rest: n.nextPace}
		uuid, commit := nextIdentity(n.ctx, n.mount, next)
		if _, old := os.Stat(filepath.Join(n.dir, oldStateName)); content || old == nil {
			err = n.mirror(live, next, liveUUID, uuid, commit)
			if err == nil {
				w.st.done("mirror", "next library mirrored from live")
			}
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
			if !ready {
				break
			}
			offered, err := n.offer(snap, state, live, swap)
			if err != nil {
				return err
			}
			if offered {
				break
			}
		}
		if pass == 3 {
			return fmt.Errorf("the stick's library kept changing while the next one was made")
		}
	}
	log.Printf("rb-bridge: next library in %s: %d rows added, %d updated, %d removed, lists changed %v, %d staged tracks applied; pending: %d rows, %d tracks' data",
		time.Since(start).Round(time.Millisecond), res.Added, res.Updated, res.Removed, res.PlaylistsChanged, res.Applied, snap.Pending.Rows, snap.Pending.Perf)
	return nil
}

// locked runs step under the volume's lock. A ctx that holds it already (a test's) is reused.
func (n *nextBuild) locked(step func() error) error {
	_, release, err := rbstate.HoldVolume(n.ctx, n.mount, fillLockWait)
	if err != nil {
		return fmt.Errorf("volume lock: %w", err)
	}
	t := time.Now()
	defer func() {
		release()
		if nextLockHeld != nil {
			nextLockHeld(time.Since(t))
		}
	}()
	return step()
}

// nextLockHeld, in tests, is told how long each locked step of the build held the lock.
var nextLockHeld func(time.Duration)

// offer makes the finished next library ready for a window, under the volume's lock: its swap
// snapshot is written only if the stick's library is still the one the build read. It reports
// false when a window changed that library meanwhile; the build then goes round again.
func (n *nextBuild) offer(snap *rbstate.Snapshot, state, live, swap string) (offered bool, err error) {
	err = n.locked(func() error {
		if n.isBusy() {
			return errDeckBusy
		}
		if enginedb.LibraryState(live) != state {
			return nil
		}
		snap.Base = state
		if n.onDemand {
			snap.Mode = rbstate.ModeOnDemand
		} else {
			snap.Mode = rbstate.ModeFull
		}
		if n.onDemand {
			if err := rbstate.SaveBrowseIndexAsCtx(n.ctx, filepath.Join(n.dir, rbstate.BrowseSwapName), snap); err != nil {
				return err
			}
		}
		if err := snap.SaveAsCtx(n.ctx, swap); err != nil {
			return err
		}
		offered = true
		return nil
	})
	return offered, err
}

// mirror makes the next library from the stick's (every row of it, once) or takes the rows Engine
// wrote since the last time, and puts its snapshot right.
func (n *nextBuild) mirror(live, next, liveUUID, uuid string, commit int64) error {
	sdir := nextState(liveUUID)
	if c, e := rbstate.LoadNext(sdir); e == nil && c.Commit == commit {
		_ = rbstate.PromoteCtx(n.ctx, sdir) // a commit of the job was cut after it
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
	ch, err := enginedb.MirrorLibrary(n.ctx, live, next, &m, n.nextPace)
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
		if err = s.SaveCtx(n.ctx, sdir); err != nil {
			return err
		}
		log.Printf("rb-bridge: next library took %d rows of the stick's library (%d read, lists %v)", len(ch.Tracks), ch.Rows, ch.Lists)
	}
	if err = base.SaveCtx(n.ctx, filepath.Join(sdir, "base")); err != nil {
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

// liveIdentityRO reads m.db's commit ID and UUID read-only (safe even while Engine holds m.db open).
func liveIdentityRO(ctx context.Context, file string) (commit int64, uuid string, exists bool, err error) {
	st, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	if st.Size() == 0 {
		return 0, "", false, nil
	}
	db, err := openRO(file, 5000)
	if err != nil {
		return 0, "", true, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var id sql.NullInt64
	var u sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT lastRekordBoxLibraryImportReadCounter, COALESCE(uuid,'') FROM Information LIMIT 1`).Scan(&id, &u); err != nil {
		return 0, "", true, err
	}
	return id.Int64, u.String, true, nil
}

// LiveLibraryIdentity returns m.db's live UUID and commit ID read-only without mutating the file.
func LiveLibraryIdentity(ctx context.Context, dbDir string) (uuid string, commit int64, err error) {
	commit, uuid, exists, err := liveIdentityRO(ctx, filepath.Join(dbDir, "m.db"))
	if err != nil {
		return "", 0, err
	}
	if !exists {
		return "", 0, os.ErrNotExist
	}
	return uuid, commit, nil
}

func promoteBrowseSwap(dir string, h rbstate.Header) {
	bswap := filepath.Join(dir, rbstate.BrowseSwapName)
	bidx := filepath.Join(dir, rbstate.BrowseIndexName)
	if h.Mode == rbstate.ModeOnDemand {
		if rbstate.BrowseIndexMatchesFile(bswap, h) {
			if err := rbstate.RenameFile(bswap, bidx); err == nil {
				return
			}
		}
		_ = rbstate.RemoveFile(bswap)
		if snap, err := rbstate.Load(dir); err == nil {
			if serr := syncBrowseIndex(dir, snap); serr != nil {
				log.Printf("rb-bridge: browse index not repaired after swap: %v", serr)
			}
			return
		}
		_ = rbstate.RemoveFile(bidx)
		return
	}
	_ = rbstate.RemoveFile(bswap)
	if !rbstate.BrowseIndexMatches(dir, h) {
		_ = rbstate.RemoveFile(bidx)
	}
}

// reconcileOrphanSwap cleans up a leftover .rb-bridge-snapshot.swap when .rb-next.db is already
// gone (e.g. a mount window killed right between renaming .rb-next.db -> m.db and renaming
// .rb-bridge-snapshot.swap -> .rb-bridge-snapshot, or a corrupt/stale swap file). It never writes
// m.db and is safe to call from both the mount window and the background job under LockVolume.
func reconcileOrphanSwap(ctx context.Context, dir string) (bool, error) {
	live, next, swap := filepath.Join(dir, "m.db"), filepath.Join(dir, nextDB), filepath.Join(dir, rbstate.SwapName)
	if _, err := os.Stat(next); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err // active ready swap waits for a mount window, or filesystem stat failed
	}
	if _, err := os.Stat(swap); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	h, err := rbstate.ReadHeaderFile(swap)
	if err != nil {
		if errors.Is(err, rbstate.ErrNoSnapshot) {
			_ = rbstate.RemoveFile(swap)
			_ = rbstate.RemoveFile(filepath.Join(dir, rbstate.BrowseSwapName))
			return false, nil
		}
		return false, err
	}
	commit, uuid, exists, err := liveIdentityRO(ctx, live)
	if err != nil {
		return false, err // transient DB read failure: keep swap intact for the next check
	}
	if exists && h.Rules == rbstate.Rules && h.LibraryUUID != "" && uuid == h.LibraryUUID && h.Commit != 0 && commit == h.Commit {
		if err := rbstate.RenameFile(swap, filepath.Join(dir, rbstate.SnapshotName)); err != nil {
			log.Printf("rb-bridge: orphan swap snapshot not promoted: %v", err)
			return false, err
		}
		promoteBrowseSwap(dir, h)
		_ = rbstate.DropNext(dir)
		dropOldState(dir)
		_ = os.RemoveAll(nextState(h.LibraryUUID))
		log.Printf("rb-bridge: promoted leftover swap snapshot from an interrupted swap")
		return true, nil
	}
	_ = rbstate.RemoveFile(swap)
	_ = rbstate.RemoveFile(filepath.Join(dir, rbstate.BrowseSwapName))
	return false, nil
}

// swapNext makes a next library that is ready the stick's library: m.db first, then its
// snapshot, each a rename inside Database2. Rows Engine wrote to m.db since the job last looked
// are taken first (carryOver). It runs at the start of every window; without a swap snapshot it
// costs one stat.
func swapNext(ctx context.Context, mount, dir string, st *steps, onDemand bool, stamp string) (swapped, deadlineHit bool) {
	live, next, swap := filepath.Join(dir, "m.db"), filepath.Join(dir, nextDB), filepath.Join(dir, rbstate.SwapName)
	if _, nextStatErr := os.Stat(next); nextStatErr != nil {
		if errors.Is(nextStatErr, os.ErrNotExist) {
			promoted, _ := reconcileOrphanSwap(ctx, dir)
			return promoted, false
		}
		return false, false
	}
	h, err := rbstate.ReadHeaderFile(swap)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, rbstate.ErrNoSnapshot) {
			return false, false
		}
		if errors.Is(err, rbstate.ErrNoSnapshot) {
			discardNext(dir, "")
		}
		return false, false
	}
	wantMode := rbstate.ModeFull
	if onDemand {
		wantMode = rbstate.ModeOnDemand
	}
	if h.Rules != rbstate.Rules || h.Mode != wantMode || (onDemand && stamp != "" && h.Stamp != stamp) {
		log.Printf("rb-bridge: discarding stale next library (rules=%d/%d mode=%q/%q stampMatch=%v)",
			h.Rules, rbstate.Rules, h.Mode, wantMode, stamp == "" || h.Stamp == stamp)
		discardNext(dir, h.LibraryUUID)
		return false, false
	}
	finish := func() bool {
		if err := rbstate.RenameFile(swap, filepath.Join(dir, rbstate.SnapshotName)); err != nil {
			log.Printf("rb-bridge: snapshot of the next library not put in place: %v", err)
			return false
		}
		promoteBrowseSwap(dir, h)
		_ = rbstate.DropNext(dir) // of a transaction on the file that is gone
		dropOldState(dir)
		_ = os.RemoveAll(nextState(h.LibraryUUID))
		return true
	}
	unlockDir, lockErr := rbstate.LockDatabaseDir(dir, 0)
	if lockErr != nil {
		return false, false
	}
	defer unlockDir()
	if enginedb.OpenElsewhere(live) != 0 {
		return false, false
	}
	if err := checkSQLiteSidecars(live); err != nil {
		return false, false
	}
	if err := checkSQLiteSidecars(next); err != nil {
		return false, false
	}
	if h.Base != enginedb.LibraryState(live) {
		ok, hit, carryErr := carryOverChecked(ctx, dir, &h, st, nil)
		if carryErr != nil {
			if hit || errors.Is(carryErr, context.Canceled) || errors.Is(carryErr, context.DeadlineExceeded) {
				log.Printf("rb-bridge: the stick's library changed after the next one was made; the job takes the changes first")
			} else {
				log.Printf("rb-bridge: carry-over failed: %v", carryErr)
			}
			return false, hit
		}
		if !ok {
			log.Printf("rb-bridge: the stick's library changed after the next one was made; the job takes the changes first")
			return false, hit
		}
	}
	// The exclusive hold on the library that goes, from here to the snapshot's promotion: no
	// SQLite connection of any process reads or writes it meanwhile, or makes a sidecar of it.
	// Every check is repeated under it; a library that is not the one the next one was made
	// from (or carried over from, just above) stays, and the job takes its changes first.
	hold, err := enginedb.HoldLibrary(ctx, live)
	if err != nil {
		log.Printf("rb-bridge: next library not swapped in, the stick's library is not free: %v", err)
		return false, false
	}
	defer hold.Release()
	if !hold.Is(h.Base) {
		log.Printf("rb-bridge: the stick's library changed after the next one was made; the job takes the changes first")
		return false, false
	}
	if enginedb.OpenElsewhere(live) != 0 {
		return false, false
	}
	if err := cleanSQLiteSidecars(live); err != nil {
		log.Printf("rb-bridge: next library not swapped in: %v", err)
		return false, false
	}
	if err := cleanSQLiteSidecars(next); err != nil {
		log.Printf("rb-bridge: next library not swapped in: %v", err)
		return false, false
	}
	cut("before swap")
	if err = hold.Same(); err == nil {
		err = rbstate.RenameFile(next, live)
	}
	if err != nil {
		log.Printf("rb-bridge: next library not swapped in: %v", err)
		return false, false
	}
	cut("swapped")
	if !finish() {
		return false, false
	}
	st.done("swap", "next library swapped in: %d tracks, %d lists; pending: %d rows, %d tracks' data", h.Tracks, h.Lists, h.Pending.Rows, h.Pending.Perf)
	return true, false
}

var mirrorLibrary = enginedb.MirrorLibrary // a variable for the test of the look's deadline

// checkSQLiteSidecars verifies without modifying the filesystem that db has no uncheckpointed
// WAL or active rollback journal. An all-zero rollback journal is SQLite's committed PERSIST
// journal and is safe to replace.
func checkSQLiteSidecars(db string) error {
	_, err := sqliteSidecars(db)
	return err
}

// sidecar is a sidecar entry of a database as a check found it.
type sidecar struct {
	path string
	info os.FileInfo
}

// sqliteSidecars is checkSQLiteSidecars that also returns the sidecar entries that exist.
func sqliteSidecars(db string) (seen []sidecar, err error) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		path := db + suffix
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("sqlite sidecar is not a regular file: %s", path)
		}
		seen = append(seen, sidecar{path, st})
		if suffix == "-shm" {
			continue // the index of a WAL: it holds no work of its own
		}
		active := st.Size() > 0
		if suffix == "-journal" && active {
			f, e := os.Open(path)
			if e != nil {
				return nil, e
			}
			var header [28]byte
			_, e = io.ReadFull(f, header[:])
			_ = f.Close()
			if e == nil {
				active = false
				for _, b := range header {
					if b != 0 {
						active = true
						break
					}
				}
			}
		}
		if active {
			return nil, fmt.Errorf("sqlite sidecar has uncheckpointed work: %s", path)
		}
	}
	return seen, nil
}

// sidecarsChecked, in tests, runs between cleanSQLiteSidecars' check and its removals.
var sidecarsChecked func(db string)

// cleanSQLiteSidecars verifies no sidecar holds uncheckpointed work, then removes the stale
// sidecars it checked, and ends with none left. The caller holds the volume's lock, and for a
// live library the exclusive hold on it. Any change after the check is the work of whoever
// opened the database meanwhile and stops the caller's replacement: a sidecar that appeared,
// changed, became another file or went away (SQLite deletes a stale journal when it writes).
// Nothing but what was checked is removed. A -wal left beside a new file would be replayed
// into it, whatever database it was made for (#336 Go review R12, finding 1).
func cleanSQLiteSidecars(db string) error {
	seen, err := sqliteSidecars(db)
	if err != nil {
		return err
	}
	if sidecarsChecked != nil {
		sidecarsChecked(db)
	}
	for _, s := range seen {
		cur, err := os.Lstat(s.path)
		if err != nil {
			return fmt.Errorf("sqlite sidecar changed after it was checked: %s: %w", s.path, err)
		}
		if !os.SameFile(s.info, cur) || cur.Size() != s.info.Size() || !cur.ModTime().Equal(s.info.ModTime()) {
			return fmt.Errorf("sqlite sidecar changed after it was checked: %s", s.path)
		}
		if err := rbstate.RemoveFile(s.path); err != nil {
			return err
		}
	}
	left, err := sqliteSidecars(db)
	if err != nil {
		return err
	}
	if len(left) != 0 {
		return fmt.Errorf("sqlite sidecar appeared after the check: %s", left[0].path)
	}
	return nil
}

// carryOver takes into the next library the rows Engine wrote to m.db after the job made it
// ready: the DJ's edits are not lost with the file. A row taken loses what the bridge wrote on it
// in the next library; its track is pending again and published by the job's next round.
// pace is nil in a window. With it the call is the remount helper's (TakeDeckRows): paced like the
// job, and Engine may write again before the remount, so what was taken is also noted in the next
// library's state on the deck, as after a pass of the job; the window or the job reads on from it.
func carryOver(ctx context.Context, dir string, h *rbstate.Header, st *steps, pace func(time.Duration) error) bool {
	ok, _, _ := carryOverChecked(ctx, dir, h, st, pace)
	return ok
}

func carryOverChecked(ctx context.Context, dir string, h *rbstate.Header, st *steps, pace func(time.Duration) error) (ok, deadlineHit bool, err error) {
	if ctx.Err() != nil {
		return false, true, ctx.Err()
	}
	live, next, swap := filepath.Join(dir, "m.db"), filepath.Join(dir, nextDB), filepath.Join(dir, rbstate.SwapName)
	sdir := nextState(h.LibraryUUID)
	var m enginedb.Mirror
	if readGob(filepath.Join(sdir, "mirror"), &m) != nil || m.Tracks == nil {
		// Made from the exports alone, or on another deck: nothing of m.db may be lost with it.
		_, content, err := liveLibrary(ctx, live)
		return err == nil && !content, false, err
	}
	// Every row of every table is read again to find the few Engine wrote (KDATA scattered:
	// >= 0.33 ms/row; 20,000+ tracks exceed a window). Reserve post-mirror load/save/swap cost
	// on top of spare (3 s) so a window is not killed between the m.db and snapshot renames.
	postSwap := work{load: h.Tracks, commits: 2, tracks: h.Tracks}
	if !st.fits(work{scan: m.Rows, load: h.Tracks, commits: 2, tracks: h.Tracks}) {
		return false, true, nil
	}
	lookCtx := ctx
	if !st.kill.IsZero() {
		var cancel context.CancelFunc
		lookCtx, cancel = context.WithDeadline(ctx, st.kill.Add(-spare-postSwap.cost()))
		defer cancel()
	}
	ch, err := mirrorLibrary(lookCtx, live, next, &m, pace)
	if err != nil {
		log.Printf("rb-bridge: rows of the stick's library not taken: %v", err)
		return false, lookCtx.Err() != nil, err
	}
	if lookCtx.Err() != nil || (pace == nil && !st.fits(postSwap)) {
		return false, true, lookCtx.Err()
	}
	if pace == nil {
		st.done("carry", "%d rows the deck wrote taken into the next library (%d read, lists %v)", len(ch.Tracks), ch.Rows, ch.Lists)
		if len(ch.Tracks) == 0 && !ch.Lists {
			return true, false, nil
		}
	}
	s, err := rbstate.LoadFile(swap)
	if err != nil {
		return false, false, err
	}
	base, err := rbstate.Load(filepath.Join(sdir, "base"))
	if err != nil {
		return false, false, err
	}
	takeLive(s, base, ch)
	// What the window cannot count without the exports: the job's next round does.
	s.Pending.Rows, s.Pending.Lists, s.Base = s.Pending.Rows+len(ch.Tracks), s.Pending.Lists || ch.Lists, m.State
	if ctx.Err() != nil {
		return false, true, ctx.Err()
	}
	if s.Mode == rbstate.ModeOnDemand {
		if err = rbstate.SaveBrowseIndexAsCtx(ctx, filepath.Join(dir, rbstate.BrowseSwapName), s); err != nil {
			return false, false, err
		}
	}
	if err = s.SaveAsCtx(ctx, swap); err != nil {
		return false, false, err
	}
	*h = s.Header
	if pace != nil {
		// The snapshot before the mirror, as in the job's pass: a cut between the two has the same rows taken again.
		if err = s.SaveCtx(ctx, sdir); err == nil {
			err = writeGob(filepath.Join(sdir, "mirror"), &m)
		}
		if err != nil {
			log.Printf("rb-bridge: state of the next library not saved: %v", err) // the swap snapshot stands on its own
			return false, false, err
		}
		log.Printf("rb-bridge: before the remount, %d rows the deck wrote taken into the next library in %d ms (%d read, lists %v)",
			len(ch.Tracks), time.Since(st.start).Milliseconds(), ch.Rows, ch.Lists)
	}
	return true, false, nil
}

// TakeDeckRows is the remount helper's step right before the remount that swaps a next library
// in: the rows Engine wrote to the stick's library since the job made the next one ready are
// taken now, as the swap's window would take them, with the job's pacing and no window's limit.
// The window then finds the library the next one was made from and only renames, whatever the
// library's size (design 1.11 item 6). It reports whether it looked: that takes time (0.15 ms a
// row on a deck), so the helper looks at the decks again after it. With no next library waiting,
// or the stick's library as the job left it, it is two short reads.
func TakeDeckRows(ctx context.Context, mount string) bool {
	looked, _ := TakeDeckRowsChecked(ctx, mount)
	return looked
}

// TakeDeckRowsChecked is TakeDeckRows with an error return so callers can distinguish a safe
// no-op (no swap waiting or base already current, (false, nil)) from a lock, state, or
// carry-over failure ((false, err != nil)) and defer publication without losing deck edits.
func TakeDeckRowsChecked(ctx context.Context, mount string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	unlock, err := rbstate.LockVolume(mount, 0)
	if err != nil {
		return false, err
	}
	defer unlock()
	dir := filepath.Join(mount, filepath.FromSlash(databaseSubdir))
	swapPath := filepath.Join(dir, rbstate.SwapName)
	if _, statErr := os.Stat(swapPath); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	} else if statErr != nil {
		return false, statErr
	}
	h, err := rbstate.ReadHeaderFile(swapPath)
	if err != nil {
		return false, err
	}
	if h.Rules != rbstate.Rules {
		return false, fmt.Errorf("take deck rows: swap snapshot rules %d != %d", h.Rules, rbstate.Rules)
	}
	if h.Base == enginedb.LibraryState(filepath.Join(dir, "m.db")) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err = os.Stat(filepath.Join(dir, nextDB)); err != nil {
		return false, err
	}
	pace := newPacer(ctx)
	looked, _, err := carryOverChecked(ctx, dir, &h, newSteps(MountOptions{Full: true}), func(took time.Duration) error { return pace.afterStat(pace.active(), took) })
	if err != nil {
		log.Printf("rb-bridge: taking deck rows failed: %v", err)
		return false, err
	}
	if !looked {
		err = errors.New("take deck rows: live library changes could not be carried over")
		log.Printf("rb-bridge: %v", err)
		return false, err
	}
	return true, nil
}
