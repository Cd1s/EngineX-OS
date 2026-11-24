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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const (
	kindName       = "KIND"        // "update" in the stage of an update, fastUpdateKind once it holds a copy of fast rows
	baseCountsName = "BASE.counts" // enginedb.BaseCounts of the snapshot, as JSON
	baseTimesName  = "BASE.times"  // enginedb.BaseTimes of the snapshot, as JSON
	resumeName     = "RESUME"      // the stamp, once a pass of the update was committed
)

// fastUpdateKind marks the stage of an update whose copy of the library has fast rows (#328
// review R2). 5.3.0.5-r1 neither reads the note of what the fast import left in them
// (enginedb.FastBaseFile) nor keeps the DJ's edits on them when it publishes (its carryLiveEdits
// leaves fast rows out), so it must neither go on with such a stage nor publish it. It knows the
// stage of an update only as "update"; a stage of another kind it drops at its mount, and it then
// starts its own update from the live library, whose copy this build in turn never publishes
// (fastBaseMissing).
const fastUpdateKind = "update+fast"

func isUpdateStage(stage string) bool {
	k := readMark(stage, kindName)
	return k == "update" || k == fastUpdateKind
}

// fastBaseMissing reports a snapshot without the note of what the fast import left in the
// library's fast rows (enginedb.FastBaseFile) while the library has fast rows: a stage of #253's
// or 5.3.0.5-r1's build, or one whose note was lost. Neither published nor continued, it starts
// again from a new snapshot: the DJ's edits made on those rows after it could not be told from
// Engine's, and the publish would put rekordbox's rows over them (#328 review R1); the passes of
// such a build put rekordbox's rows over those made before it (review R2).
func fastBaseMissing(dir, stage string) bool {
	if _, err := os.Stat(filepath.Join(stage, baseCountsName)); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(stage, enginedb.FastBaseFile)); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return hasFastTracks(dir)
}

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
	start := time.Now()
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
	log.Printf("rb-bridge: mount phase=stage-write ms=%d", time.Since(start).Milliseconds())
	return requestBackground(stage)
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
func quickUpdate(ctx context.Context, mount, dir, stamp string, r *compat.Resolver, limit time.Time) (enginedb.Result, bool, error) {
	inner, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, false, enginedb.ErrLibraryInUse
	}
	start := time.Now()
	l, stagedOnly, err := readLibrary(inner, mount)
	readElapsed := time.Since(start)
	log.Printf("rb-bridge: mount phase=export-read ms=%d", readElapsed.Milliseconds())
	if err != nil {
		return enginedb.Result{}, false, quickLate(ctx, err)
	}
	read := time.Since(start)
	playlist, syncErr := enginedb.SyncPlaylistsOnly(inner, mount, l)
	if syncErr == nil {
		changed := playlist.Added + playlist.Updated + playlist.Removed
		log.Printf("rb-bridge: mount phase=playlist-transaction ms=%d", (time.Since(start) - readElapsed).Milliseconds())
		log.Printf("rb-bridge: playlist-only update changed %d lists in %s (export read %s)", changed, time.Since(start).Round(time.Millisecond), read.Round(time.Millisecond))
		if err := writeMark(dir, stampName, stamp); err != nil {
			return enginedb.Result{}, false, err
		}
		_ = os.Remove(filepath.Join(dir, pendingName))
		return enginedb.Result{PlaylistsChanged: changed > 0}, true, nil
	}
	if !errors.Is(syncErr, enginedb.ErrPlaylistTrackSetChanged) {
		return enginedb.Result{}, false, syncErr
	}
	quickPathsStart := time.Now()
	fresh, owned, spellings, err := quickPathsWithOwned(dir, l)
	log.Printf("rb-bridge: mount phase=quick-paths ms=%d", time.Since(quickPathsStart).Milliseconds())
	if err != nil {
		return enginedb.Result{}, false, err
	}
	exportFolded := make(map[string]bool, len(l.Tracks))
	for i := range l.Tracks {
		if clean, cerr := compat.CleanRelative(l.Tracks[i].Path); cerr == nil {
			exportFolded[foldPath(clean)] = true
		}
	}
	for i := range l.Tracks {
		clean, cerr := compat.CleanRelative(l.Tracks[i].Path)
		if cerr != nil {
			continue
		}
		curFold := foldPath(clean)
		if _, alreadyOwned := spellings[curFold]; alreadyOwned {
			continue
		}
		if oldPath, ok := owned[l.Tracks[i].ID]; ok {
			oldFold := foldPath(oldPath)
			if oldFold != curFold && !exportFolded[oldFold] {
				l.Tracks[i].Path = "/" + oldPath
			}
		}
	}
	reconcileStart := time.Now()
	if err = ReconcileOwnedContext(inner, r, l, owned, spellings); err != nil {
		return enginedb.Result{}, false, quickLate(ctx, err)
	}
	log.Printf("rb-bridge: mount phase=reconcile ms=%d", time.Since(reconcileStart).Milliseconds())
	entries := 0
	for _, p := range l.Playlists {
		entries += len(p.Tracks)
	}
	cost := quickCost(len(l.Tracks)-fresh, fresh, entries)
	if left := time.Until(limit); cost > left {
		return enginedb.Result{}, false, fmt.Errorf("%w: %d tracks (%d new) and %d playlist entries need about %s, %s left", errQuickTooLong, len(l.Tracks), fresh, entries, cost.Round(time.Second), left.Round(time.Second))
	}
	overviews := enginedb.ReadOverviewProgress(mount)
	syncStart := time.Now()
	res, err := enginedb.SyncResolved(inner, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, StagedOnly: stagedOnly})
	if err != nil {
		return res, false, quickLate(ctx, err)
	}
	log.Printf("rb-bridge: mount phase=live-list-write ms=%d", time.Since(syncStart).Milliseconds())
	_ = overviews.Keep(mount)
	log.Printf("rb-bridge: quick update of %d tracks (%d new), %d playlist entries in %s (export read %s, estimate %s)", len(l.Tracks), fresh, entries, time.Since(start).Round(time.Millisecond), read.Round(time.Millisecond), cost.Round(time.Millisecond))
	return res, false, nil
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
func quickPaths(dir string, l *rekordbox.Library) (fresh int, err error) {
	fresh, _, _, err = quickPathsWithOwned(dir, l)
	return fresh, err
}

func quickPathsWithOwned(dir string, l *rekordbox.Library) (fresh int, rows map[uint32]string, known map[string]string, err error) {
	known, err = ownedSpellings(dir)
	if err != nil {
		return 0, nil, nil, err
	}
	rows, err = ownedPaths(dir)
	if err != nil {
		return 0, nil, nil, err
	}
	for i := range l.Tracks {
		clean, cleanErr := compat.CleanRelative(l.Tracks[i].Path)
		if cleanErr != nil {
			return 0, nil, nil, cleanErr
		}
		if p, ok := known[foldPath(clean)]; ok {
			l.Tracks[i].Path = "/" + p
			continue
		}
		if p, ok := rows[l.Tracks[i].ID]; ok && foldPath(p) == foldPath(clean) {
			l.Tracks[i].Path = "/" + p
		}
	}
	if fresh, err = keepSpellings(l, known); err != nil {
		return 0, nil, nil, err
	}
	mergeByPath(l)
	return fresh, rows, known, nil
}

// quickName marks the stage of an update whose list did not fit the mount (a 20 000-track
// re-export on an SC5000): the job builds the list alone first, and the remount that publishes
// it starts the update that reads the files. Its playlists then wait for one remount, not for
// every file of the stick.
const quickName = "QUICK"

// quickInStage is quickUpdate on the stage's copy of the library, without the mount's time limit.
func quickInStage(ctx context.Context, dir, stage, stamp string, r *compat.Resolver, l *rekordbox.Library, stagedOnly bool, progress func(done, total int)) (enginedb.Result, error) {
	if _, err := os.Stat(filepath.Join(stage, baseCountsName)); errors.Is(err, os.ErrNotExist) {
		if err = snapshotLive(ctx, dir, stage, r, l); err != nil {
			return enginedb.Result{}, err
		}
	}
	if progress != nil {
		progress(0, len(l.Tracks))
	}
	if _, err := quickPaths(stage, l); err != nil {
		return enginedb.Result{}, err
	}
	owned, err := ownedPaths(stage)
	if err != nil {
		return enginedb.Result{}, err
	}
	spellings, err := ownedSpellings(stage)
	if err != nil {
		return enginedb.Result{}, err
	}
	if err := ReconcileOwnedContext(ctx, r, l, owned, spellings); err != nil {
		return enginedb.Result{}, err
	}
	databaseDir, cleanup, err := privateStageDatabase(stage)
	if err != nil {
		return enginedb.Result{}, err
	}
	defer cleanup()
	res, err := enginedb.SyncResolved(ctx, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, StagedOnly: stagedOnly, DatabaseDir: databaseDir, Bulk: true})
	if err != nil {
		return res, err
	}
	if err = publishPrivateStageDatabase(stage, databaseDir); err != nil {
		return res, err
	}
	cut("stage list committed")
	if progress != nil {
		progress(len(l.Tracks), len(l.Tracks))
	}
	log.Printf("rb-bridge: the re-export's list is ready (%d new tracks); a remount publishes it before its files are read", res.Added)
	return res, completeStage(stage, stamp)
}

func privateStageDatabase(stage string) (string, func(), error) {
	root := os.Getenv("ENGINEX_RB_STAGE_DIR")
	if root == "" {
		if st, err := os.Stat("/data/enginex"); err == nil && st.IsDir() {
			root = "/data/enginex/tmp"
		} else {
			root = "/var/tmp/enginex-rb-stage"
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", func() {}, err
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", func() {}, fmt.Errorf("quick stage directory is not a private directory: %s", root)
	}
	for _, stale := range globQuickStages(root) {
		_ = os.RemoveAll(stale)
	}
	need := int64(64 << 20)
	for _, name := range []string{"m.db", stateName} {
		if st, err := os.Stat(filepath.Join(stage, name)); err == nil {
			need += st.Size()
		}
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return "", func() {}, err
	}
	if fs.Bavail*uint64(fs.Bsize) < uint64(need) {
		return "", func() {}, fmt.Errorf("quick stage directory has insufficient free space")
	}
	dir, err := os.MkdirTemp(root, fmt.Sprintf("enginex-rb-quick-%d-", os.Getpid()))
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	for _, name := range []string{"m.db", stateName} {
		src := filepath.Join(stage, name)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := copyFile(src, filepath.Join(dir, name)); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	return dir, cleanup, nil
}

func globQuickStages(root string) []string {
	paths, _ := filepath.Glob(filepath.Join(root, "enginex-rb-quick-*-*"))
	var stale []string
	for _, p := range paths {
		name := filepath.Base(p)
		parts := strings.Split(name, "-")
		if len(parts) < 4 {
			continue
		}
		pid, err := strconv.Atoi(parts[3])
		if err == nil && pid > 0 && pid != os.Getpid() && syscall.Kill(pid, 0) == syscall.ESRCH {
			stale = append(stale, p)
		}
	}
	return stale
}

func publishPrivateStageDatabase(stage, databaseDir string) error {
	for _, name := range []string{"m.db", stateName} {
		src := filepath.Join(databaseDir, name)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		tmp := filepath.Join(stage, ".enginex-quick-"+name+".tmp")
		if err := copyFile(src, tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := syncFile(tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, filepath.Join(stage, name)); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return syncFile(stage)
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
	if hasFastTracks(dir) && readMark(stage, kindName) != fastUpdateKind {
		// Before anything of the copy is in the stage: no earlier build goes on with it.
		if err := writeMark(stage, kindName, fastUpdateKind); err != nil {
			return err
		}
		if err := syncFile(stage); err != nil {
			return err
		}
		cut("update stage kept from earlier builds")
	}
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
			refs, err := confirmedRefs(ctx, r, l, keys, checked)
			if err != nil {
				return err
			}
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
		// What the fast import left in the rows still fast, for the DJ's edits made on them
		// after this copy (#328): before the counts, which say the snapshot is complete.
		if err := enginedb.WriteFastBase(ctx, stage, l); err != nil {
			return err
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

var (
	stageCompactTimeout = 20 * time.Second
	stageCompactPoll    = 50 * time.Millisecond
)

func finishStageCompact(stage string) error {
	for _, name := range []string{"m.db", stateName} {
		if err := finishStageCompactFile(stage, name); err != nil {
			return err
		}
	}
	return nil
}

func finishStageCompactFile(stage, name string) error {
	p := filepath.Join(stage, name)
	tmp := p + ".compact"
	ready := p + ".compact.ready"
	if _, err := os.Stat(ready); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if rerr := os.Remove(tmp); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return rerr
			}
			return nil
		}
		return err
	}
	if _, err := os.Stat(tmp); err == nil {
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
		if err := syncFile(stage); err != nil {
			return err
		}
		cut("compact replaced " + name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, side := range []string{"-journal", "-wal", "-shm"} {
		if err := os.Remove(p + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cut("compact sidecars removed " + name)
	if err := os.Remove(ready); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncFile(stage)
}

func vacuumStageMDB(ctx context.Context, stage string) {
	if err := finishStageCompact(stage); err != nil {
		return
	}
	if ctx.Err() != nil || IsDeckActive() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, stageCompactTimeout)
	defer cancel()
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		t := time.NewTicker(stageCompactPoll)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-cctx.Done():
				return
			case <-t.C:
				if IsDeckActive() {
					cancel()
					return
				}
			}
		}
	}()
	for _, name := range []string{"m.db", stateName} {
		if cctx.Err() != nil || IsDeckActive() {
			return
		}
		if !vacuumStageFile(cctx, stage, name) {
			if cctx.Err() != nil || IsDeckActive() {
				return
			}
		}
	}
}

func vacuumStageFile(ctx context.Context, stage, name string) bool {
	p := filepath.Join(stage, name)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	tmp := p + ".compact"
	ready := p + ".compact.ready"
	_ = os.Remove(ready)
	_ = os.Remove(tmp)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return false
	}
	db.SetMaxOpenConns(1)
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if ctx.Err() != nil || IsDeckActive() {
		_ = db.Close()
		_ = os.Remove(tmp)
		return false
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, tmp)
	_ = db.Close()
	if err != nil || ctx.Err() != nil || IsDeckActive() {
		_ = os.Remove(tmp)
		return false
	}
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	cut("compact vacuumed " + name)
	if ctx.Err() != nil || IsDeckActive() {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.WriteFile(ready, []byte("1\n"), 0o644); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	if err := syncFile(ready); err != nil {
		_ = os.Remove(ready)
		_ = os.Remove(tmp)
		return false
	}
	if err := syncFile(stage); err != nil {
		_ = os.Remove(ready)
		_ = os.Remove(tmp)
		return false
	}
	cut("compact ready " + name)
	return finishStageCompactFile(stage, name) == nil
}

// jobProgress reports a background job's tracks done out of the export's to progress: once the
// export is read, before the long steps (its paths, the snapshot), then as each pass goes (its
// share of the whole count, enginedb.Options.OnFinished), never fewer than it reported (#328).
type jobProgress struct {
	progress     func(done, total int)
	total, shown int
}

func newJobProgress(progress func(done, total int), total int) *jobProgress {
	p := &jobProgress{progress: progress, total: total, shown: -1}
	p.set(0)
	return p
}

func (p *jobProgress) set(done int) {
	if p.progress == nil || min(done, p.total) <= p.shown {
		return
	}
	p.shown = min(done, p.total)
	p.progress(p.shown, p.total)
}

func (p *jobProgress) report(done, _ int) { p.set(done) }

// updateInBackground runs the import passes of an update on the stage's copy of the library.
func updateInBackground(ctx context.Context, mount, stage, stamp string, r *compat.Resolver, progress func(done, total int)) (enginedb.Result, error) {
	dir := filepath.Dir(stage)
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	shown := newJobProgress(progress, len(l.Tracks))
	if fastBaseMissing(dir, stage) {
		// The passes already done are lost, once: the snapshot is taken again.
		log.Printf("rb-bridge: the update's copy of the library has no note of its fast tracks; it starts again from a new copy")
		for _, name := range []string{resumeName, baseCountsName} {
			if err := os.Remove(filepath.Join(stage, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return enginedb.Result{}, err
			}
		}
	}
	if readMark(stage, quickName) == stamp {
		return quickInStage(ctx, dir, stage, stamp, r, l, stagedOnly, shown.report)
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
	stageUpdated := 0
	changed := readMark(stage, resumeName) != "" // an earlier pass may have changed it
	pri := newPriorityTracker()
	for {
		resuming := readMark(stage, resumeName) == stamp
		if priChanged, perr := pri.service(ctx, mount, stage, stamp, r, l, stagedOnly, legacy, legacyAAC, resuming, shown.set); perr != nil {
			if ctx.Err() != nil || !errors.Is(perr, enginedb.ErrLibraryInUse) {
				return enginedb.Result{}, perr
			}
		} else if priChanged {
			changed = true
			if err = writeMark(stage, resumeName, stamp); err != nil {
				return enginedb.Result{}, err
			}
			resuming = true
		}
		o, wasActive := backgroundPassOptions(ctx, mount, pri, window, enginedb.Options{
			DatabaseDir:      stage,
			StagedOnly:       stagedOnly,
			LegacyOffsets:    legacy,
			LegacyAACOffsets: legacyAAC,
			Resume:           resuming,
			OnFinished:       shown.set,
		})
		res, err := enginedb.SyncResolved(ctx, r, l, o)
		if err != nil {
			return res, err
		}
		cut("stage pass committed")
		stageUpdated += res.Added + res.Updated
		changed = changed || res.Added+res.Updated > 0 || res.PlaylistsChanged
		shown.set(total - res.Deferred)
		if res.Deferred == 0 {
			if stageUpdated >= 256 {
				vacuumStageMDB(ctx, stage)
			}
			if n, rerr := enginedb.BackfillStageRGBOverviews(ctx, r, l, stage, dir); rerr != nil {
				log.Printf("rb-bridge: stage RGB overview backfill skipped: %v", rerr)
			} else if n > 0 {
				log.Printf("rb-bridge: generated %d missing stage RGB overviews", n)
				changed = true
			}
			if !changed {
				// Nothing to publish: the live library already is this export's.
				log.Printf("rb-bridge: update of %s changed nothing", mount)
				_ = enginedb.InvalidateUnsyncedEdits(ctx, dir)
				writeStamp(dir, stamp)
				cut("unchanged update stamped")
				os.Remove(filepath.Join(dir, pendingName))
				return enginedb.Result{Bypassed: true}, dropStage(stage)
			}
			return res, completeStage(stage, stamp)
		}
		if err = writeMark(stage, resumeName, stamp); err != nil {
			return res, err
		}
		cut("stage resume marked")
		if left >= 0 && res.Deferred >= left {
			if pri.hasPending(mount) || (!wasActive && IsDeckActive()) {
				continue
			}
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
	if missingRGB, rerr := enginedb.CountMissingRGBOverviews(ctx, dir); rerr == nil && missingRGB > 0 {
		log.Printf("rb-bridge: %d analyzed tracks missing RGB overviews; update goes on in its stage", missingRGB)
		return enginedb.Result{}, false, nil
	}
	if work == 0 {
		// Nothing to read: the live library already is this export's.
		_ = enginedb.InvalidateUnsyncedEdits(ctx, dir)
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
		refs, err := confirmedRefs(ctx, r, l, keys, map[string]bool{})
		if err != nil {
			return enginedb.Result{}, true, err // nothing marked: the next job checks again
		}
		for key := range refs {
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
// still hold a copy of the waveform (5.3.0.5's RBA1), where those files give the display that
// waveform: enginedb.DetailServes, the display's own reading of the files, each read whole. A
// track whose files are missing, truncated, renamed, malformed or unreadable keeps its copy (#253
// reviews 2-4). Rows that are references already and tracks without a row are not in keys and are
// not read. Only the background job calls it; its reads are paced (detailPace). checked holds the
// answer for each list of files already read. A stop (ctx) returns the error and confirms nothing
// more: every track not confirmed keeps its copy, and the next job checks it again.
func confirmedRefs(ctx context.Context, r *compat.Resolver, l *rekordbox.Library, keys map[uint32]bool, checked map[string]bool) (map[uint32][]string, error) {
	refs := map[uint32][]string{}
	if len(keys) == 0 {
		return refs, nil
	}
	start, files, read := time.Now(), 0, int64(0)
	playingPace := playingDetailPace(ctx)
	defer func() {
		log.Printf("rb-bridge: %d of %d waveform copies can become references; ANLZ files read in %s (%d tracks, %d bytes)", len(refs), len(keys), time.Since(start).Round(time.Millisecond), files, read)
	}()
	exportRefs := enginedb.ExportRefs(l)
	type refJob struct {
		id   uint32
		list []string
		key  string
	}
	jobs := make([]refJob, 0, len(keys))
	for id, list := range exportRefs {
		if !keys[id] || len(list) == 0 {
			continue
		}
		jobs = append(jobs, refJob{id: id, list: list, key: strings.Join(list, "\x00")})
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].key != jobs[j].key {
			return jobs[i].key < jobs[j].key
		}
		return jobs[i].id < jobs[j].id
	})
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return refs, err
		}
		ok, seen := checked[job.key]
		if !seen {
			var n int64
			var err error
			if IsDeckActive() {
				ok, n, err = enginedb.DetailServesWithChunk(r, job.list, enginedb.PlayingReadChunk, playingPace)
			} else {
				ok, n, err = enginedb.DetailServes(r, job.list, nil)
			}
			files, read = files+1, read+n
			if err != nil {
				return refs, err
			}
			if err := ctx.Err(); err != nil {
				return refs, err
			}
			checked[job.key] = ok
		}
		if ok {
			refs[job.id] = job.list
		}
	}
	return refs, ctx.Err()
}

// detailPace paces the job's whole reads of ANLZ files (#253 review 4): a deck may be loading or
// playing a track from the same stick, and the decks' I/O schedulers do not honour the idle class
// the job runs in. After each read of at most 128 KiB the job sleeps as long as that read took and
// at least detailReadGap: the stick is free at least half the time, and a deck's request waits
// behind one such read at most.
func detailPace(ctx context.Context) enginedb.DetailPause {
	return detailPaceWithGap(ctx, &detailReadGap)
}

// playingDetailPace paces background ANLZ reads with 64 KiB chunks and at least playingReadGap
// (20 ms) while a deck is playing, cue-previewing, or scratching.
func playingDetailPace(ctx context.Context) enginedb.DetailPause {
	return detailPaceWithGap(ctx, &playingReadGap)
}

func detailPaceWithGap(ctx context.Context, gap *time.Duration) enginedb.DetailPause {
	return func(_ int, took time.Duration) error {
		t := time.NewTimer(max(took, *gap))
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}

// detailReadGap and playingReadGap are minimum pauses after paced reads. Variables for tests.
var (
	detailReadGap  = 10 * time.Millisecond
	playingReadGap = 20 * time.Millisecond
)

// publishUpdate replaces the live library with the complete update in the stage, at a mount
// before Engine sees the volume, with what Engine wrote since the snapshot. A change Engine made
// that the update cannot take keeps the live library, and the next mount updates it again.
func publishUpdate(ctx context.Context, mount, dir string) (enginedb.Result, error) {
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	stage := filepath.Join(dir, importStageName)
	if fastBaseMissing(dir, stage) {
		// Also when Engine wrote nothing since the snapshot: the passes of an earlier build
		// (5.3.0.5-r1's update, started at its mount after it dropped this build's stage) put
		// rekordbox's rows over the DJ's edits the copy has of fast rows (#328 review R2).
		log.Printf("rb-bridge: update not published: its copy of the library has no note of its fast tracks; it starts again from a new copy")
		return enginedb.Result{Background: true}, startUpdate(stage, readMark(stage, stampName))
	}
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
	overviews := enginedb.ReadOverviewProgress(mount)
	_ = enginedb.InvalidateUnsyncedEdits(ctx, stage)
	publishStart := time.Now()
	if err := renameStage(dir, stage); err != nil {
		return enginedb.Result{}, err
	}
	log.Printf("rb-bridge: mount phase=publish ms=%d", time.Since(publishStart).Milliseconds())
	_ = overviews.Keep(mount)
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
	_ = finishStageCompact(filepath.Join(dir, importStageName))
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
