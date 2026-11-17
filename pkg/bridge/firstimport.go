package bridge

// A volume's first import is built in a stage beside the library it becomes and published by
// renaming once it is complete (#301). The mount does what its time allows. A library that
// needs longer is finished by ImportInBackground after the mount, a pass at a time, however
// long that takes, and the next mount publishes it. Engine never opens the stage, so it never
// sees a library that is not complete, and a kill, an eject or a power cut costs at most the
// pass under way. Before, the whole first import ran while the mount waited, in one
// transaction on tmpfs: a library too large for that was killed with the mount's time and
// started again from nothing on every insert.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

const (
	importStageName = ".rb-bridge-import"
	// baseName holds liveIdentity of the live m.db the stage was built on.
	baseName = "BASE"
	// beforeImportName keeps a live m.db that changed after the stage was built on it:
	// Engine's skeleton gains a row for each track the DJ loads from the OneLibrary
	// export while the import runs.
	beforeImportName = "m.db.before-import"
)

// importWindow is one background pass, the most a kill, an eject or a power cut can cost.
// A variable only for tests.
var importWindow = 30 * time.Second

// Past this much export, a first import cannot finish while the mount waits, so the mount does
// not try: a USB stick reads the files of about 12 tracks a second (6000 in 8 minutes), and
// resolving a cold stick's folders took 4 minutes for 20000 tracks (#301). About 1000 tracks.
const attemptExportBytes = 512 << 10

// firstImport reports that the volume has no library an import must merge into: no m.db,
// or Engine's skeleton. The skeleton gains a track for each one the DJ loads while Engine
// shows the OneLibrary export, and it has no playlists and no bridge state.
func firstImport(dir string) bool {
	if !hasLibrary(dir) {
		return true
	}
	if _, err := os.Lstat(filepath.Join(dir, stateName)); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	p := filepath.Join(dir, "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return false
	}
	defer db.Close()
	var playlists int
	return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Playlist)`).Scan(&playlists) == nil && playlists == 0
}

// firstImportAtMount publishes a complete stage, or starts one and imports what the mount's
// time allows into it. Result.Background asks for ImportInBackground.
func firstImportAtMount(ctx context.Context, mount, dir, stamp string, opts *enginedb.Options, r *compat.Resolver, mountStart time.Time) (enginedb.Result, error) {
	stage := filepath.Join(dir, importStageName)
	switch {
	case readMark(stage, stampName) == stamp:
		if err := publishImport(mount, dir); err != nil {
			return enginedb.Result{}, err
		}
		log.Printf("rb-bridge: first import published")
		return enginedb.Result{PlaylistsChanged: true}, nil
	case readMark(stage, pendingName) == stamp:
		return enginedb.Result{Background: true}, nil
	}
	// Never past the edisksd hook's kill: a kill is what used to throw the import away.
	limit := mountStart.Add(maxMountHookBudget)
	if opts.Deadline.After(limit) {
		limit = opts.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, limit)
	defer cancel()
	if enginedb.LibraryInUse(mount) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	if err := dropStage(stage); err != nil { // another export's
		return enginedb.Result{}, err
	}
	fail := func(err error) (enginedb.Result, error) {
		if errors.Is(err, os.ErrNotExist) {
			err = nil // nothing to import after all
		}
		dropStage(stage)
		cleanEmptyEngineLibrary(dir)
		return enginedb.Result{Bypassed: err == nil}, err
	}
	if err := newStage(ctx, dir, stage, stamp); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("Engine's library on this volume could not be copied in the mount's time: %w", err)
		}
		return fail(err)
	}
	if opts.Deadline.IsZero() && exportBytes(r, mount) > attemptExportBytes {
		log.Printf("rb-bridge: first import of a large export goes on after the mount")
		return enginedb.Result{Background: true}, nil
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err == nil {
		err = ReconcileContext(ctx, r, l)
	}
	var res enginedb.Result
	if err == nil {
		mergeByPath(l)
		o := *opts
		o.DatabaseDir, o.StagedOnly = stage, stagedOnly
		if o.Deadline.IsZero() { // reading the export does not spend the file-reading budget
			o.Deadline = time.Now().Add(min(mountReadBudget, max(time.Until(limit), time.Second)))
		}
		opts.Deadline = o.Deadline
		res, err = enginedb.SyncResolved(ctx, r, l, o)
	}
	if opts.Deadline.IsZero() {
		opts.Deadline = limit
	}
	switch {
	case ctx.Err() != nil && err != nil:
		log.Printf("rb-bridge: first import goes on after the mount: %v", err)
		return enginedb.Result{Background: true}, nil
	case err != nil:
		return fail(err)
	case res.Deferred > 0:
		log.Printf("rb-bridge: first import goes on after the mount: %d of %d tracks without cues yet", res.Deferred, len(l.Tracks))
		res.Background = true
		return res, nil
	}
	if err = completeStage(stage, stamp); err == nil {
		err = publishImport(mount, dir)
	}
	return res, err
}

func exportBytes(r *compat.Resolver, mount string) int64 {
	candidates, _ := exportCandidates(r, mount)
	var n int64
	for _, c := range candidates {
		n += c.size
	}
	return n
}

// newStage starts a stage on the live library as it is, so its UUID and anything Engine put
// in the skeleton carry over.
func newStage(ctx context.Context, dir, stage, stamp string) error {
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	live := filepath.Join(dir, "m.db")
	if _, err := os.Lstat(live); err == nil {
		if err = snapshot(ctx, live, filepath.Join(stage, "m.db")); err != nil {
			return err
		}
	}
	if err := writeMark(stage, baseName, liveIdentity(live)); err != nil {
		return err
	}
	if err := writeMark(stage, pendingName, stamp); err != nil { // the stage is this export's from here on
		return err
	}
	return syncFile(stage)
}

func completeStage(stage, stamp string) error {
	if err := writeMark(stage, stampName, stamp); err != nil {
		return err
	}
	return os.Remove(filepath.Join(stage, pendingName))
}

// publishImport renames a complete stage into place. It runs only at a mount, before Engine
// sees the volume, and finishPublish completes it if it is cut short.
func publishImport(mount, dir string) error {
	if enginedb.LibraryInUse(mount) {
		return enginedb.ErrLibraryInUse
	}
	stage := filepath.Join(dir, importStageName)
	live := filepath.Join(dir, "m.db")
	if id := liveIdentity(live); id != "absent" && id != readMark(stage, baseName) {
		// Engine wrote to it since the stage was built on it. With playlists it is a library
		// of its own now, which the next mount imports into in place.
		if !firstImport(dir) {
			if err := dropStage(stage); err != nil {
				return err
			}
			return errors.New("Engine library changed during import; retry on next mount")
		}
		for _, side := range []string{"", "-journal", "-wal"} {
			if err := os.Rename(live+side, filepath.Join(dir, beforeImportName+side)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		log.Printf("rb-bridge: Engine's library on this volume changed during the first import; kept as %s", beforeImportName)
	}
	if err := os.WriteFile(filepath.Join(stage, publishReady), nil, 0o644); err != nil {
		return err
	}
	if err := syncFile(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, filepath.Join(dir, publishDir)); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, pendingName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return finishPublish(dir, stateName, stampName, "m.db")
}

// liveIdentity names the state of an SQLite file: its size and the change counter that
// rollback-journal mode, Engine's, bumps on every commit. Any journal or WAL beside it counts.
func liveIdentity(p string) string {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		return "unreadable"
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "unreadable"
	}
	header := make([]byte, 28)
	n, _ := io.ReadFull(f, header)
	id := fmt.Sprintf("%d %x", st.Size(), header[min(n, 24):n])
	for _, side := range []string{"-journal", "-wal"} {
		if st, err := os.Stat(p + side); err == nil && st.Size() > 0 {
			id += side
		}
	}
	return id
}

// dropStage removes a stage unless an import holds it.
func dropStage(stage string) error {
	if _, err := os.Lstat(stage); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if f, err := os.OpenFile(filepath.Join(stage, ".rb-bridge.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0); err == nil {
		defer f.Close()
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("first import still running: %w", err)
		}
	}
	return os.RemoveAll(stage)
}

// ImportInBackground finishes the first import a mount left in the stage on the volume at
// mount, a pass of importWindow at a time; each pass commits, so stopping it loses at most
// one. Result.Bypassed: no stage of the volume's current export to finish. progress gets
// the tracks done and their total after each pass.
func ImportInBackground(ctx context.Context, mount string, progress func(done, total int)) (enginedb.Result, error) {
	r, err := compat.NewResolver(mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	stamp, err := exportStamp(r, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	stage := filepath.Join(mount, "Engine Library", "Database2", importStageName)
	if stamp == "" || readMark(stage, stampName) == stamp {
		return enginedb.Result{Bypassed: stamp == ""}, nil
	}
	if readMark(stage, pendingName) != stamp {
		return enginedb.Result{Bypassed: true}, nil
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	if err = ReconcileContext(ctx, r, l); err != nil {
		return enginedb.Result{}, err
	}
	mergeByPath(l)
	total, left, window := len(l.Tracks), -1, importWindow
	if progress != nil {
		progress(0, total)
	}
	for {
		o := enginedb.Options{DatabaseDir: stage, Resume: true, StagedOnly: stagedOnly, Deadline: time.Now().Add(window)}
		res, err := enginedb.SyncResolved(ctx, r, l, o)
		if err != nil {
			return res, err
		}
		if progress != nil {
			progress(total-res.Deferred, total)
		}
		if res.Deferred == 0 {
			return res, completeStage(stage, stamp)
		}
		// A pass whose window went on what every pass costs (the finished tracks' lookups, a
		// track Engine owns) gets a longer one.
		if left >= 0 && res.Deferred >= left {
			if window *= 2; window > max(64*importWindow, 10*time.Minute) {
				return res, fmt.Errorf("first import stopped making progress with %d tracks left", res.Deferred)
			}
		}
		if left < 0 || res.Deferred < left {
			left = res.Deferred
		}
	}
}
