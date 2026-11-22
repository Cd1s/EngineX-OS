package bridge

// An update of a converted library that does not fit in the mount's time goes through the same
// stage as a first import (#301). The mount only marks the stage; the background job copies the
// live library into it, runs the import passes on that copy, and a later mount publishes it:
// MergeLive first carries over what Engine wrote meanwhile. Before, such an update ran in place
// in the mount's time, and a re-export of a large stick was killed there on every insert.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const (
	kindName       = "KIND"        // "update" in the stage of an update
	baseCountsName = "BASE.counts" // enginedb.BaseCounts of the snapshot, as JSON
	baseTimesName  = "BASE.times"  // enginedb.BaseTimes of the snapshot, as JSON
	resumeName     = "RESUME"      // the stamp, once a pass of the update was committed
)

func isUpdateStage(stage string) bool { return readMark(stage, kindName) == "update" }

// cutPoint, when set, is called between two writes of an import, an update or a publish that a
// power cut or the hook's kill can separate (#253 review): a test copies the volume there to see
// what the next mount makes of it. Nil outside tests.
var cutPoint func(name string)

func cut(name string) {
	if cutPoint != nil {
		cutPoint(name)
	}
}

// startUpdate marks the stage of an update of stamp's export; the job copies the library.
func startUpdate(stage, stamp string) error {
	if err := dropStage(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	if err := writeMark(stage, kindName, "update"); err != nil {
		return err
	}
	cut("update stage marked")
	if err := writeMark(stage, pendingName, stamp); err != nil {
		return err
	}
	if err := syncFile(stage); err != nil {
		return err
	}
	cut("update stage pending")
	return nil
}

// What a quick update costs on an SC5000, the slowest deck, on top of the export read: a track the
// library holds is looked up and left as it is, a new one is inserted with Engine's indexes and
// triggers in place (unlike fastPublish's bulk load), and each playlist entry as in fastPublish.
// Measured native armhf on the aarch64 build host, times 3: a re-export of 20 004 tracks with
// 60 000 playlist entries took 6.8 s there, 240 µs a known track once the entries are counted.
// So a 6000-track re-export fits the mount (about 8 s on the deck) and a 20 000-track one does
// not (about 22 s): its list is built first by the job (quickName).
var (
	quickPerKnown = 750 * time.Microsecond
	quickPerNew   = 2 * time.Millisecond
)

// quickUpdate brings a re-export's playlists and new tracks into the live library while the mount
// waits, from the export alone, as fastPublish does for a first import (#253). Tracks the library
// holds keep their rows unread; new tracks come with the export's own cues; tracks the export
// dropped go. The background update reads the files afterwards. Before, a large export's update
// only marked its stage here, and the playlists appeared once a remount published the update:
// 11.5 minutes on the maintainer's 6000-track stick, and not at all while a deck held one of its
// tracks. limit is the hook's kill; nothing is written when the update would not end before it.
func quickUpdate(ctx context.Context, mount, dir string, r *compat.Resolver, limit time.Time, flip bool) (enginedb.Result, error) {
	inner, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	start := time.Now()
	l, stagedOnly, err := readLibrary(inner, mount)
	if err != nil {
		return enginedb.Result{}, quickLate(ctx, err)
	}
	read := time.Since(start)
	fresh, err := quickPaths(dir, l, flip)
	if err != nil {
		return enginedb.Result{}, err
	}
	entries := 0
	for _, p := range l.Playlists {
		entries += len(p.Tracks)
	}
	cost := quickCost(len(l.Tracks)-fresh, fresh, entries)
	if left := time.Until(limit); cost > left {
		return enginedb.Result{}, fmt.Errorf("%w: %d tracks (%d new) and %d playlist entries need about %s, %s left", errQuickTooLong, len(l.Tracks), fresh, entries, cost.Round(time.Second), left.Round(time.Second))
	}
	res, err := enginedb.SyncResolved(inner, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, StagedOnly: stagedOnly, MatchByPath: true, PathFirst: flip})
	if err != nil {
		return res, quickLate(ctx, err)
	}
	log.Printf("rb-bridge: quick update of %d tracks (%d new), %d playlist entries in %s (export read %s, estimate %s)", len(l.Tracks), fresh, entries, time.Since(start).Round(time.Millisecond), read.Round(time.Millisecond), cost.Round(time.Millisecond))
	return res, nil
}

// quickCost is what a quick update of known and fresh tracks and entries playlist entries costs on
// an SC5000, after the export is read.
func quickCost(known, fresh, entries int) time.Duration {
	return time.Duration(known)*quickPerKnown + time.Duration(fresh)*quickPerNew + time.Duration(entries)*publishPerEntry + publishMargin
}

var errQuickTooLong = errors.New("the re-export's list does not fit the mount")

// quickLate is a quick update's error, errQuickTooLong when it ran out of the mount's time (its own
// deadline, not the caller's): the job then builds the list first (quickName).
func quickLate(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%w: %v", errQuickTooLong, err)
	}
	return err
}

// quickPaths prepares the export l for an import of its list alone into the library in dir and
// counts its new tracks. The export's spelling of a path is trusted for a new track; one the
// library holds keeps its row's path, which an import resolved on the volume, even when the export
// moved its file: a quick update moves no row. A row it moved would be imported as new from the
// export alone and then no longer count as the import's, and the update would replace what a deck
// changed on it; the update moves it with its files read.
func quickPaths(dir string, l *rekordbox.Library, flip bool) (fresh int, err error) {
	known, err := ownedSpellings(dir)
	if err != nil {
		return 0, err
	}
	rows, err := ownedPaths(dir)
	if err != nil {
		return 0, err
	}
	rowsByPath := make(map[string]uint32, len(rows))
	for id, p := range rows {
		rowsByPath[foldPath(filepath.ToSlash(filepath.Clean("/"+p)))] = id
	}
	for i := range l.Tracks {
		if flip {
			break
		}
		if p, ok := rows[l.Tracks[i].ID]; ok {
			storedPath := foldPath(filepath.ToSlash(filepath.Clean("/" + p)))
			exportPath := foldPath(filepath.ToSlash(filepath.Clean(l.Tracks[i].Path)))
			if storedPath == exportPath || rowsByPath[exportPath] == l.Tracks[i].ID {
				l.Tracks[i].Path = "/" + p
			} else if _, occupied := rowsByPath[exportPath]; !occupied {
				l.Tracks[i].Path = "/" + p
			}
		}
	}
	if fresh, err = keepSpellings(l, known); err != nil {
		return 0, err
	}
	mergeByPath(l)
	return fresh, nil
}

// quickName marks the stage of an update whose list did not fit the mount (a 20 000-track
// re-export on an SC5000): the job builds the list alone first, and the remount that publishes
// it starts the update that reads the files. Its playlists then wait for one remount, not for
// every file of the stick.
const quickName = "QUICK"

// quickInStage is quickUpdate on the stage's copy of the library, without the mount's time limit.
func quickInStage(ctx context.Context, dir, stage, stamp string, r *compat.Resolver, l *rekordbox.Library, stagedOnly, flip bool, readRels []string, progress func(done, total int)) (enginedb.Result, error) {
	if _, err := os.Stat(filepath.Join(stage, baseCountsName)); errors.Is(err, os.ErrNotExist) {
		if err = snapshotLive(ctx, dir, stage, r, l); err != nil {
			return enginedb.Result{}, err
		}
	}
	if progress != nil {
		progress(0, len(l.Tracks))
	}
	if _, err := quickPaths(stage, l, flip); err != nil {
		return enginedb.Result{}, err
	}
	res, err := enginedb.SyncResolved(ctx, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, StagedOnly: stagedOnly, MatchByPath: true, PathFirst: flip, DatabaseDir: stage})
	if err != nil {
		return res, err
	}
	cut("stage list committed")
	if progress != nil {
		progress(len(l.Tracks), len(l.Tracks))
	}
	log.Printf("rb-bridge: the re-export's list is ready (%d new tracks); a remount publishes it before its files are read", res.Added)
	if err := writeMark(stage, readName, readLibrariesMark(stamp, readRels)); err != nil {
		return res, err
	}
	return res, completeStage(stage, stamp)
}

// keepSpellings gives the tracks of l that the library holds their rows' spelling of their path,
// which an import resolved on the volume, and counts the others.
func keepSpellings(l *rekordbox.Library, known map[string]string) (fresh int, err error) {
	for i := range l.Tracks {
		rel, err := compat.CleanRelative(l.Tracks[i].Path)
		if err != nil {
			return 0, err
		}
		if stored, ok := known[foldPath(rel)]; ok {
			l.Tracks[i].Path = "/" + stored
		} else {
			fresh++
		}
	}
	return fresh, nil
}

// ownedPaths maps the source ID of each track the bridge imported to its row's path, for the rows
// whose pdbImportKey is theirs alone (libraries of early builds share one key per import).
func ownedPaths(dir string) (map[uint32]string, error) {
	rows := map[uint32]string{}
	state := filepath.Join(dir, stateName)
	if _, err := os.Stat(state); errors.Is(err, os.ErrNotExist) {
		return rows, nil
	}
	ro := func(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }
	db, err := sql.Open("sqlite", ro(filepath.Join(dir, "m.db"))+"&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, ro(state)); err != nil {
		return nil, err
	}
	q, err := db.Query(`SELECT t.pdbImportKey,min(s.path),count(*) FROM bridge.BridgeState s JOIN Track t ON t.id=s.trackId WHERE t.pdbImportKey BETWEEN 1 AND 4294967295 GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var key int64
		var path string
		var n int
		if err = q.Scan(&key, &path, &n); err != nil {
			return nil, err
		}
		if n == 1 {
			rows[uint32(key)] = strings.TrimPrefix(path, "../")
		}
	}
	return rows, q.Err()
}

// ownedSpellings maps the folded path of each track the bridge imported to its row's spelling.
func ownedSpellings(dir string) (map[string]string, error) {
	known := map[string]string{}
	p := filepath.Join(dir, stateName)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return known, nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT path FROM BridgeState`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err = rows.Scan(&path); err != nil {
			return nil, err
		}
		rel := strings.TrimPrefix(path, "../")
		known[foldPath(rel)] = rel
	}
	return known, rows.Err()
}

// snapshotRetries and snapshotWait bound how long the job waits for Engine to stop writing.
var (
	snapshotRetries = 10
	snapshotWait    = 2 * time.Second
)

// snapshotLive copies the live library into the stage while Engine may have it open. Engine
// takes no file locks, so the copy is a plain one, kept only if the library's identity (size,
// change counter, no journal under way) is the same before and after it. l is the export the
// update imports, r the volume's resolver.
func snapshotLive(ctx context.Context, dir, stage string, r *compat.Resolver, l *rekordbox.Library) error {
	live := filepath.Join(dir, "m.db")
	checked := map[string]bool{} // the files confirmedRefs looked at, across retries
	for try := 0; try < snapshotRetries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(snapshotWait):
			}
		}
		before := liveIdentity(live)
		if strings.HasSuffix(before, "-journal") || strings.HasSuffix(before, "-wal") || before == "absent" || before == "unreadable" {
			continue
		}
		// A library Engine made, which rb-bridge never imported, has no bridge state: the
		// passes start one in the stage, and MergeLive reads the live one as empty.
		names := []string{"m.db", stateName}
		if _, err := os.Stat(filepath.Join(dir, stateName)); errors.Is(err, os.ErrNotExist) {
			names = names[:1]
			os.Remove(filepath.Join(stage, stateName))
		}
		for _, name := range names {
			os.Remove(filepath.Join(stage, name+".copy"))
		}
		// Only the library's pages, not its free ones (VACUUM INTO). Immutable: no lock is taken
		// and no journal is looked at, so this reader never writes Engine's library; the identity
		// before and after decides whether the copy is consistent.
		if err := vacuumInto(ctx, live, filepath.Join(stage, "m.db.copy")); err != nil {
			return err
		}
		// The state without the waveform copies of earlier builds and its free pages, built
		// against the copy just made: a byte copy of it was 2.7 GB on a 6000-track stick (#253).
		if len(names) > 1 {
			keys, err := enginedb.WaveformRowKeys(ctx, filepath.Join(dir, stateName), filepath.Join(stage, "m.db.copy"))
			if err != nil {
				return err
			}
			refs := confirmedRefs(r, l, keys, checked)
			if err = enginedb.CopyBridgeStateCompact(ctx, filepath.Join(dir, stateName), filepath.Join(stage, "m.db.copy"), filepath.Join(stage, stateName+".copy"), refs); err != nil {
				return err
			}
		}
		if liveIdentity(live) != before {
			continue
		}
		for _, name := range names {
			if err := os.Rename(filepath.Join(stage, name+".copy"), filepath.Join(stage, name)); err != nil {
				return err
			}
		}
		cut("snapshot copied")
		counts, err := enginedb.CountTables(ctx, stage)
		if err != nil {
			return err
		}
		// The times first: the counts are what says the snapshot is there.
		times, err := enginedb.SnapshotTimes(ctx, stage)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(times)
		if err = writeMark(stage, baseTimesName, string(b)); err != nil {
			return err
		}
		cut("snapshot times")
		b, _ = json.Marshal(counts)
		if err = writeMark(stage, baseCountsName, string(b)); err != nil {
			return err
		}
		cut("snapshot counts")
		if err = writeMark(stage, baseName, before); err != nil {
			return err
		}
		return syncFile(stage)
	}
	return errors.New("Engine kept writing the library; the update goes on at the next mount")
}

// vacuumInto writes the SQLite database src to dst with VACUUM INTO, reading src as immutable.
func vacuumInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: src, RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy of %s: %w", filepath.Base(src), err)
	}
	return syncFile(dst)
}

// updateInBackground runs the import passes of an update on the stage's copy of the library.
func updateInBackground(ctx context.Context, mount, stage, stamp string, r *compat.Resolver, progress func(done, total int)) (enginedb.Result, error) {
	dir := filepath.Dir(stage)
	l, stagedOnly, readRels, err := readLibraryDetailed(ctx, mount)
	if errors.Is(err, errUnreadableExport) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if err != nil {
		return enginedb.Result{}, err
	}
	if readMark(stage, quickName) == stamp {
		return quickInStage(ctx, dir, stage, stamp, r, l, stagedOnly, primaryFlipped(readMark(dir, stampName), stamp), readRels, progress)
	}
	if err = ReconcileContext(ctx, r, l); err != nil {
		return enginedb.Result{}, err
	}
	old := readMark(dir, stampName)
	legacy, legacyAAC := legacyGridOptions(old)
	if _, err := os.Stat(filepath.Join(stage, baseCountsName)); errors.Is(err, os.ErrNotExist) {
		if !stagedOnly && !legacy {
			if res, handed, err := handToMounts(ctx, r, l, dir, stage, stamp); handed {
				return res, err
			}
		}
		if err = snapshotLive(ctx, dir, stage, r, l); err != nil {
			return enginedb.Result{}, err
		}
	}
	total, left, window := len(l.Tracks), -1, importWindow
	if progress != nil {
		progress(0, total)
	}
	changed := readMark(stage, resumeName) != "" // an earlier pass may have changed it
	for {
		o := enginedb.Options{DatabaseDir: stage, StagedOnly: stagedOnly, MatchByPath: true, PathFirst: primaryFlipped(readMark(dir, stampName), stamp), LegacyOffsets: legacy, LegacyAACOffsets: legacyAAC,
			Resume: readMark(stage, resumeName) == stamp, Deadline: time.Now().Add(window)}
		res, err := enginedb.SyncResolved(ctx, r, l, o)
		if err != nil {
			return res, err
		}
		cut("stage pass committed")
		changed = changed || res.Added+res.Updated > 0 || res.PlaylistsChanged
		if progress != nil {
			progress(total-res.Deferred, total)
		}
		if res.Deferred == 0 {
			if !changed {
				// Nothing to publish: the live library already is this export's.
				log.Printf("rb-bridge: update of %s changed nothing", mount)
				writeStamp(dir, stamp)
				cut("unchanged update stamped")
				os.Remove(filepath.Join(dir, pendingName))
				return enginedb.Result{Bypassed: true}, dropStage(stage)
			}
			if err := writeMark(stage, readName, readLibrariesMark(stamp, readRels)); err != nil {
				return res, err
			}
			return res, completeStage(stage, stamp)
		}
		if err = writeMark(stage, resumeName, stamp); err != nil {
			return res, err
		}
		cut("stage resume marked")
		if left >= 0 && res.Deferred >= left {
			if window *= 2; window > max(64*importWindow, 10*time.Minute) {
				return res, fmt.Errorf("update stopped making progress with %d tracks left", res.Deferred)
			}
		}
		if left < 0 || res.Deferred < left {
			left = res.Deferred
		}
	}
}

// inPlaceUpdateTracks is the most tracks an update leaves to a mount to read in place: about what
// a mount reads in its 8 s on a USB stick (BH-GC1-2). A larger update goes on in the stage.
// perTrackReadBytes is about what reading one track's files costs (its ANLZ files are about
// 350 KB): an update is left to the mounts only where copying the library costs more.
var (
	inPlaceUpdateTracks       = 100
	perTrackReadBytes   int64 = 400 << 10
)

// rereadName holds an update a mount finishes in place: its stamp and the imported tracks whose
// files changed, which a Resume import does not look at otherwise.
const rereadName = ".rb-bridge-reread"

type rereadMark struct {
	Stamp string
	Paths []string
	// Refs are the tracks whose state rows still holding a waveform the remount's compaction may
	// replace by references: the job checked their ANLZ files (confirmedRefs).
	Refs []uint32 `json:",omitempty"`
}

// handToMounts leaves an update with little to read to the mounts, in place, without a copy of
// the library (#253): the quick update at the mount brought the playlists and new tracks, and
// what is left is the files of a few tracks. The background job then only checks every track's
// files, reading nothing into a database, and its remount reads those few in place before Engine
// sees the volume. handed is false when the update needs its stage after all.
func handToMounts(ctx context.Context, r *compat.Resolver, l *rekordbox.Library, dir, stage, stamp string) (res enginedb.Result, handed bool, err error) {
	st, err := os.Stat(filepath.Join(dir, "m.db"))
	if err != nil || st.Size() <= int64(inPlaceUpdateTracks)*perTrackReadBytes {
		return enginedb.Result{}, false, nil // a copy of so small a library costs less
	}
	start := time.Now()
	// A track the library holds is looked for under its row's spelling first: the resolver then
	// confirms it from the folder it lists anyway.
	known, err := ownedSpellings(dir)
	if err != nil {
		return enginedb.Result{}, false, nil
	}
	plan := *l
	plan.Tracks = append([]rekordbox.Track(nil), l.Tracks...)
	if _, err = keepSpellings(&plan, known); err != nil {
		return enginedb.Result{}, false, nil
	}
	reread, added, err := enginedb.PlanUpdate(ctx, r, &plan, dir)
	if err != nil {
		if ctx.Err() != nil {
			return enginedb.Result{}, true, err
		}
		log.Printf("rb-bridge: update not planned, it goes on in its stage: %v", err)
		return enginedb.Result{}, false, nil
	}
	work := len(reread) + added
	log.Printf("rb-bridge: update checked in %s: %d tracks to read again, %d to add", time.Since(start).Round(time.Millisecond), len(reread), added)
	if work > inPlaceUpdateTracks || int64(work)*perTrackReadBytes >= st.Size() {
		return enginedb.Result{}, false, nil
	}
	if work == 0 {
		// Nothing to read: the live library already is this export's.
		writeStamp(dir, stamp)
		cut("handoff stamped")
		os.Remove(filepath.Join(dir, pendingName))
		os.Remove(filepath.Join(dir, rereadName))
		return enginedb.Result{Bypassed: true}, true, dropStage(stage)
	}
	mark := rereadMark{Stamp: stamp, Paths: reread}
	if fileBytes(filepath.Join(dir, stateName)) >= compactStateBytes {
		// The remount compacts the state: its waveform rows become references only where the
		// files are there to read (#253 review 2), checked here and not while the mount waits.
		keys, err := enginedb.WaveformRowKeys(ctx, filepath.Join(dir, stateName), filepath.Join(dir, "m.db"))
		if err != nil {
			log.Printf("rb-bridge: waveform rows not checked, the remount keeps them: %v", err)
		}
		for key := range confirmedRefs(r, l, keys, map[string]bool{}) {
			mark.Refs = append(mark.Refs, key)
		}
	}
	b, _ := json.Marshal(mark)
	if err = os.WriteFile(filepath.Join(dir, rereadName), b, 0o644); err != nil {
		return enginedb.Result{}, true, err
	}
	if err = syncFile(dir); err != nil {
		return enginedb.Result{}, true, err
	}
	cut("reread marked")
	// Not bypassed: the job remounts the volume, and that mount reads them.
	return enginedb.Result{Deferred: work}, true, dropStage(stage)
}

// compactState rewrites the live bridge state without the waveform copies of earlier builds and its
// free pages, at a mount before Engine sees the volume, once the import of l into the library has
// committed (#253): the rows it rewrites name the ANLZ files of l, which hold the same waveforms,
// so the library and its state change together and the display loses nothing wherever it is cut
// (#253 review). Only the tracks of keys, whose files the background job checked, get references;
// the mount reads no ANLZ file (#253 review 2). A state below compactStateBytes is left as it is.
// It never stops the mount: on any error the state stays as it was, and a leftover copy goes at
// the next mount.
func compactState(ctx context.Context, dir string, l *rekordbox.Library, keys map[uint32]bool) {
	state := filepath.Join(dir, stateName)
	if st, err := os.Stat(state); err != nil || st.Size() < compactStateBytes {
		return
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".rb-bridge.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	start, before := time.Now(), fileBytes(state)
	tmp := state + ".compact"
	refs := enginedb.ExportRefs(l)
	for id := range refs {
		if !keys[id] {
			delete(refs, id)
		}
	}
	if err = enginedb.CopyBridgeStateCompact(ctx, state, filepath.Join(dir, "m.db"), tmp, refs); err == nil {
		cut("state compacted aside")
		for _, side := range []string{"-journal", "-wal", "-shm"} {
			os.Remove(state + side)
		}
		if err = os.Rename(tmp, state); err == nil {
			cut("state compacted")
			err = syncFile(dir)
		}
	}
	if err != nil {
		os.Remove(tmp)
		log.Printf("rb-bridge: bridge state not compacted: %v", err)
		return
	}
	log.Printf("rb-bridge: bridge state compacted from %d to %d bytes in %s", before, fileBytes(state), time.Since(start).Round(time.Millisecond))
}

// compactStateBytes is the size from which compactState rewrites a state: a state of this build is
// about 300 bytes a track.
var compactStateBytes int64 = 16 << 20

func fileBytes(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return st.Size()
}

// readReread is the tracks an update left to the mounts of stamp's export to read again; nil when
// it left none.
func readReread(dir, stamp string) map[string]bool {
	m := rereadOf(dir, stamp)
	if m == nil {
		return nil
	}
	paths := make(map[string]bool, len(m.Paths))
	for _, p := range m.Paths {
		paths[p] = true
	}
	return paths
}

func rereadOf(dir, stamp string) *rereadMark {
	b, err := os.ReadFile(filepath.Join(dir, rereadName))
	if err != nil {
		return nil
	}
	var m rereadMark
	if json.Unmarshal(b, &m) != nil || m.Stamp != stamp {
		return nil
	}
	return &m
}

// rereadRefs are the tracks of the reread mark whose waveform rows may become references.
func rereadRefs(dir, stamp string) map[uint32]bool {
	keys := map[uint32]bool{}
	if m := rereadOf(dir, stamp); m != nil {
		for _, k := range m.Refs {
			keys[k] = true
		}
	}
	return keys
}

// confirmedRefs are the ANLZ files the export l names for the tracks of keys, whose state rows
// still hold a waveform, where those files give the display that waveform (enginedb.DetailServes):
// the reader that serves the detail, without the waveforms' bodies. A track whose files are
// missing, truncated, renamed or malformed keeps its row (#253 reviews 2 and 3). Only the background job calls it. checked holds the answer for each list of
// files already looked at.
func confirmedRefs(r *compat.Resolver, l *rekordbox.Library, keys map[uint32]bool, checked map[string]bool) map[uint32][]string {
	refs := map[uint32][]string{}
	if len(keys) == 0 {
		return refs
	}
	start, files, read := time.Now(), 0, int64(0)
	for id, list := range enginedb.ExportRefs(l) {
		if !keys[id] {
			continue
		}
		key := strings.Join(list, "\x00")
		ok, seen := checked[key]
		if !seen {
			var n int64
			ok, n = enginedb.DetailServes(r, list)
			checked[key], files, read = ok, files+1, read+n
		}
		if ok {
			refs[id] = list
		}
	}
	log.Printf("rb-bridge: %d of %d waveform rows can become references; ANLZ files checked in %s (%d tracks, %d bytes read)", len(refs), len(keys), time.Since(start).Round(time.Millisecond), files, read)
	return refs
}

// publishUpdate replaces the live library with the complete update in the stage, at a mount
// before Engine sees the volume, with what Engine wrote since the snapshot. A change Engine made
// that the update cannot take keeps the live library, and the next mount updates it again.
func publishUpdate(ctx context.Context, mount, dir string) (enginedb.Result, error) {
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	stage := filepath.Join(dir, importStageName)
	if liveIdentity(filepath.Join(dir, "m.db")) != readMark(stage, baseName) {
		var base enginedb.BaseCounts
		if err := json.Unmarshal([]byte(readMark(stage, baseCountsName)), &base); err != nil {
			return enginedb.Result{}, fmt.Errorf("update stage without its snapshot counts: %w", err)
		}
		// A stage of a build that recorded no times: every track is compared, as it was.
		var times enginedb.BaseTimes
		if json.Unmarshal([]byte(readMark(stage, baseTimesName)), &times) != nil {
			times = enginedb.BaseTimes{}
		}
		start := time.Now()
		written, err := enginedb.MergeLive(ctx, stage, dir, base, times)
		if errors.Is(err, enginedb.ErrMergeConflict) {
			log.Printf("rb-bridge: update not published: %v; the next mount updates the library again", err)
			return enginedb.Result{}, dropStage(stage)
		}
		if err != nil {
			return enginedb.Result{}, err
		}
		log.Printf("rb-bridge: Engine's changes since the update started carried over in %s (%d tracks Engine wrote)", time.Since(start).Round(time.Millisecond), written)
		cut("live changes merged")
	}
	stamp := readMark(stage, stampName)
	quick := readMark(stage, quickName) == stamp
	if quick {
		// The list alone is published as pending, not imported: the publish moves the mark in
		// with the library, so no interruption leaves a stamp that says its files were read.
		if err := os.Rename(filepath.Join(stage, stampName), filepath.Join(stage, pendingName)); err != nil {
			return enginedb.Result{}, err
		}
		cut("list stage pending")
	}
	if err := renameStage(dir, stage); err != nil {
		return enginedb.Result{}, err
	}
	if quick {
		// The update that reads the files starts now (completeLater).
		log.Printf("rb-bridge: the re-export's list published; its files are read next")
		cut("list published")
		if err := startUpdate(stage, stamp); err != nil {
			return enginedb.Result{}, err
		}
		return enginedb.Result{PlaylistsChanged: true, Background: true}, nil
	}
	log.Printf("rb-bridge: update published")
	return enginedb.Result{PlaylistsChanged: true}, nil
}

// LibraryHasFile reports whether a library of mount, the one a remount publishes or Engine's live
// one (which has the tracks Engine added), has a track whose file is called name. A remount waits
// while a deck holds Engine's copy of such a track (#301). By name only: the export's file size
// may be out of date, and a remount too many only waits. True when it cannot tell.
func LibraryHasFile(mount, name string) bool {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	for _, p := range []string{filepath.Join(dir, importStageName, "m.db"), filepath.Join(dir, "m.db")} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(1500)&_pragma=query_only(1)"}).String())
		if err != nil {
			return true
		}
		var has bool
		err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Track WHERE filename=?)`, name).Scan(&has)
		db.Close()
		if err != nil || has {
			return true
		}
	}
	return false
}
