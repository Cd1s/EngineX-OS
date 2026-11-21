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
	if err := writeMark(stage, pendingName, stamp); err != nil {
		return err
	}
	return syncFile(stage)
}

// What a quick update costs on an SC5000, the slowest deck, on top of the export read: a track the
// library holds is looked up and left as it is, a new one is inserted with Engine's indexes and
// triggers in place (unlike fastPublish's bulk load), and each playlist entry as in fastPublish.
// Measured native armhf on the aarch64 build host (6031 tracks, 19 985 entries) times 3.
var (
	quickPerKnown = 150 * time.Microsecond
	quickPerNew   = 2 * time.Millisecond
)

// quickUpdate brings a re-export's playlists and new tracks into the live library while the mount
// waits, from the export alone, as fastPublish does for a first import (#253). Tracks the library
// holds keep their rows unread; new tracks come with the export's own cues; tracks the export
// dropped go. The background update reads the files afterwards. Before, a large export's update
// only marked its stage here, and the playlists appeared once a remount published the update:
// 11.5 minutes on the maintainer's 6000-track stick, and not at all while a deck held one of its
// tracks. limit is the hook's kill; nothing is written when the update would not end before it.
func quickUpdate(ctx context.Context, mount, dir string, r *compat.Resolver, limit time.Time) (enginedb.Result, error) {
	ctx, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	known, err := ownedSpellings(dir)
	if err != nil {
		return enginedb.Result{}, err
	}
	rows, err := ownedPaths(dir)
	if err != nil {
		return enginedb.Result{}, err
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	// The export's spelling of a path is trusted for a new track; one the library holds keeps its
	// row's path, which an import resolved on the volume, even when the export moved its file: a
	// quick update moves no row. A row it moved would be imported as new from the export alone and
	// then no longer count as the import's, and the background update would replace what a deck
	// changed on it; the update moves it with its files read.
	for i := range l.Tracks {
		if p, ok := rows[l.Tracks[i].ID]; ok {
			l.Tracks[i].Path = "/" + p
		}
	}
	fresh, err := keepSpellings(l, known)
	if err != nil {
		return enginedb.Result{}, err
	}
	entries := 0
	for _, p := range l.Playlists {
		entries += len(p.Tracks)
	}
	cost := time.Duration(len(l.Tracks)-fresh)*quickPerKnown + time.Duration(fresh)*quickPerNew + time.Duration(entries)*publishPerEntry + publishMargin
	if left := time.Until(limit); cost > left {
		return enginedb.Result{}, fmt.Errorf("%d new tracks and %d playlist entries need about %s, %s left", fresh, entries, cost.Round(time.Second), left.Round(time.Second))
	}
	mergeByPath(l)
	return enginedb.SyncResolved(ctx, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, StagedOnly: stagedOnly})
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
// change counter, no journal under way) is the same before and after it.
func snapshotLive(ctx context.Context, dir, stage string) error {
	live := filepath.Join(dir, "m.db")
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
			if err := enginedb.CopyBridgeStateCompact(ctx, filepath.Join(dir, stateName), filepath.Join(stage, "m.db.copy"), filepath.Join(stage, stateName+".copy")); err != nil {
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
		b, _ = json.Marshal(counts)
		if err = writeMark(stage, baseCountsName, string(b)); err != nil {
			return err
		}
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
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
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
		if err = snapshotLive(ctx, dir, stage); err != nil {
			return enginedb.Result{}, err
		}
	}
	total, left, window := len(l.Tracks), -1, importWindow
	if progress != nil {
		progress(0, total)
	}
	changed := readMark(stage, resumeName) != "" // an earlier pass may have changed it
	for {
		o := enginedb.Options{DatabaseDir: stage, StagedOnly: stagedOnly, LegacyOffsets: legacy, LegacyAACOffsets: legacyAAC,
			Resume: readMark(stage, resumeName) == stamp, Deadline: time.Now().Add(window)}
		res, err := enginedb.SyncResolved(ctx, r, l, o)
		if err != nil {
			return res, err
		}
		changed = changed || res.Added+res.Updated > 0 || res.PlaylistsChanged
		if progress != nil {
			progress(total-res.Deferred, total)
		}
		if res.Deferred == 0 {
			if !changed {
				// Nothing to publish: the live library already is this export's.
				log.Printf("rb-bridge: update of %s changed nothing", mount)
				writeStamp(dir, stamp)
				os.Remove(filepath.Join(dir, pendingName))
				return enginedb.Result{Bypassed: true}, dropStage(stage)
			}
			return res, completeStage(stage, stamp)
		}
		if err = writeMark(stage, resumeName, stamp); err != nil {
			return res, err
		}
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
		os.Remove(filepath.Join(dir, pendingName))
		os.Remove(filepath.Join(dir, rereadName))
		return enginedb.Result{Bypassed: true}, true, dropStage(stage)
	}
	b, _ := json.Marshal(rereadMark{stamp, reread})
	if err = os.WriteFile(filepath.Join(dir, rereadName), b, 0o644); err != nil {
		return enginedb.Result{}, true, err
	}
	if err = syncFile(dir); err != nil {
		return enginedb.Result{}, true, err
	}
	// Not bypassed: the job remounts the volume, and that mount reads them.
	return enginedb.Result{Deferred: work}, true, dropStage(stage)
}

// compactState rewrites the live bridge state without the waveform copies of earlier builds and its
// free pages, at a mount before Engine sees the volume, when an import pass follows that gives the
// rows their references (#253). A state below compactStateBytes is left as it is. It never stops
// the mount: on any error the state stays as it was.
func compactState(ctx context.Context, dir string) {
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
	if err = enginedb.CopyBridgeStateCompact(ctx, state, filepath.Join(dir, "m.db"), tmp); err == nil {
		for _, side := range []string{"-journal", "-wal", "-shm"} {
			os.Remove(state + side)
		}
		if err = os.Rename(tmp, state); err == nil {
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
	b, err := os.ReadFile(filepath.Join(dir, rereadName))
	if err != nil {
		return nil
	}
	var m rereadMark
	if json.Unmarshal(b, &m) != nil || m.Stamp != stamp {
		return nil
	}
	paths := make(map[string]bool, len(m.Paths))
	for _, p := range m.Paths {
		paths[p] = true
	}
	return paths
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
	}
	if err := renameStage(dir, stage); err != nil {
		return enginedb.Result{}, err
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
