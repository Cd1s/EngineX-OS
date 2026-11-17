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
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const (
	kindName       = "KIND"        // "update" in the stage of an update
	baseCountsName = "BASE.counts" // enginedb.BaseCounts of the snapshot, as JSON
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
		for _, name := range []string{"m.db", stateName} {
			os.Remove(filepath.Join(stage, name+".copy"))
			if err := copyFile(filepath.Join(dir, name), filepath.Join(stage, name+".copy")); err != nil {
				return err
			}
		}
		if liveIdentity(live) != before {
			continue
		}
		for _, name := range []string{"m.db", stateName} {
			if err := os.Rename(filepath.Join(stage, name+".copy"), filepath.Join(stage, name)); err != nil {
				return err
			}
		}
		counts, err := enginedb.CountTables(ctx, stage)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(counts)
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

// updateInBackground runs the import passes of an update on the stage's copy of the library.
func updateInBackground(ctx context.Context, mount, stage, stamp string, r *compat.Resolver, progress func(done, total int)) (enginedb.Result, error) {
	dir := filepath.Dir(stage)
	if _, err := os.Stat(filepath.Join(stage, baseCountsName)); errors.Is(err, os.ErrNotExist) {
		if err = snapshotLive(ctx, dir, stage); err != nil {
			return enginedb.Result{}, err
		}
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	if err = ReconcileContext(ctx, r, l); err != nil {
		return enginedb.Result{}, err
	}
	old := readMark(dir, stampName)
	legacy := old != "" && !strings.HasPrefix(old, stampVersion+" ")
	total, left, window := len(l.Tracks), -1, importWindow
	if progress != nil {
		progress(0, total)
	}
	changed := readMark(stage, resumeName) != "" // an earlier pass may have changed it
	for {
		o := enginedb.Options{DatabaseDir: stage, StagedOnly: stagedOnly, LegacyOffsets: legacy,
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
		start := time.Now()
		err := enginedb.MergeLive(ctx, stage, dir, base)
		if errors.Is(err, enginedb.ErrMergeConflict) {
			log.Printf("rb-bridge: update not published: %v; the next mount updates the library again", err)
			return enginedb.Result{}, dropStage(stage)
		}
		if err != nil {
			return enginedb.Result{}, err
		}
		log.Printf("rb-bridge: Engine's changes since the update started carried over in %s", time.Since(start).Round(time.Millisecond))
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
