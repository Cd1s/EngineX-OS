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
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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

// Past this much export, a full first import cannot finish while the mount waits: a USB stick
// reads the files of about 12 tracks a second (6000 in 8 minutes), and resolving a cold
// stick's folders took 4 minutes for 20000 tracks (#301). About 1000 tracks. A larger export
// is published from the export alone when that fits (fastPublish), else built in the background.
var attemptExportBytes int64 = 512 << 10

// Past this many tracks a first import is published from the export alone, its files read by
// later mounts: reading them took 8 minutes for 6000 tracks on a USB stick (BH-GC1-2).
const fastImportTracks = 500

// bulkDir is where fastPublish builds, inside the stage; whatever is left of it is dropped.
const bulkDir = ".bulk"

// An export this large is not read at the mount at all: decrypting and parsing it alone would
// take most of the mount's time on a deck.
var maxPublishExportBytes int64 = 32 << 20

// What a fastPublish costs on an SC5000 (Cortex-A17), the slowest deck: the bulk publish
// measured on the aarch64 build host (native armhf, the throttled 477 GB copy of #301, 20 000
// tracks, 59 764 entries: 7.0 s) times 3, the most the deck took longer to read the same export.
var (
	publishPerTrack = 800 * time.Microsecond
	publishPerEntry = 80 * time.Microsecond
	publishMargin   = 2 * time.Second // the renames of publishImport, and a busy deck
)

// fastPublishFits reports that fastPublish of l ends in left, the mount's time after the read.
func fastPublishFits(l *rekordbox.Library, left time.Duration) bool {
	entries := 0
	for _, p := range l.Playlists {
		entries += len(p.Tracks)
	}
	return time.Duration(len(l.Tracks))*publishPerTrack+time.Duration(entries)*publishPerEntry+publishMargin <= left
}

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
		return completeLater(dir, stage, stamp, enginedb.Result{PlaylistsChanged: true})
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
	size := exportBytes(r, mount)
	if opts.Deadline.IsZero() && size > maxPublishExportBytes {
		log.Printf("rb-bridge: first import of a large export goes on after the mount")
		return enginedb.Result{Background: true}, nil
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err == nil && (len(l.Tracks) > fastImportTracks || opts.Deadline.IsZero() && size > attemptExportBytes) {
		if !fastPublishFits(l, time.Until(limit)) {
			log.Printf("rb-bridge: first import of %d tracks goes on after the mount", len(l.Tracks))
			return enginedb.Result{Background: true}, nil
		}
		res, err := fastPublish(ctx, mount, dir, stage, stamp, r, l, stagedOnly)
		if ctx.Err() != nil && err != nil {
			log.Printf("rb-bridge: first import goes on after the mount: %v", err)
			return enginedb.Result{Background: true}, nil
		}
		return res, err
	}
	if err == nil {
		err = ReconcileContext(ctx, r, l)
	}
	var res enginedb.Result
	if err == nil {
		mergeByPath(l)
		o := *opts
		o.DatabaseDir, o.StagedOnly, o.FastCues = stage, stagedOnly, true
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
	}
	// Tracks the mount had no time to read are "fast:": the next mount reads them (#301).
	if res.Deferred > 0 {
		log.Printf("rb-bridge: first import published with %d of %d tracks without grids yet", res.Deferred, len(l.Tracks))
	}
	if err = completeStage(stage, stamp); err == nil {
		err = publishImport(mount, dir)
	}
	if err == nil && res.Deferred > 0 {
		err = keepPending(dir)
	}
	return res, err
}

// keepPending records a published export as pending, not imported: later mounts read the files
// of its "fast:" tracks, as after an import in place that ran out of time (BH-GC1-2, BH2-A4-5).
func keepPending(dir string) error {
	return os.Rename(filepath.Join(dir, stampName), filepath.Join(dir, pendingName))
}

// completeLater goes on with a first import the background job built from the export alone:
// the update that reads its files starts at once, and a remount publishes it (#301).
func completeLater(dir, stage, stamp string, res enginedb.Result) (enginedb.Result, error) {
	if !hasFastTracks(dir) {
		return res, nil
	}
	if err := keepPending(dir); err != nil {
		return res, err
	}
	if err := startUpdate(stage, stamp); err != nil {
		return res, err
	}
	res.Background = true
	return res, nil
}

// fastPublish publishes every playlist and track of a first import at the mount from the
// export alone (#301): paths as the export spells them, cues from its own database, no file
// read, loaded in bulk. Grids, waveforms, artwork and the exact cue positions come with the
// full import a later mount runs on the "fast:" tracks.
func fastPublish(ctx context.Context, mount, dir, stage, stamp string, r *compat.Resolver, l *rekordbox.Library, stagedOnly bool) (enginedb.Result, error) {
	res, err := buildFast(ctx, stage, r, l, stagedOnly)
	if err != nil {
		return res, err
	}
	if err = completeStage(stage, stamp); err != nil {
		return res, err
	}
	log.Printf("rb-bridge: first import of %d tracks published from the export; grids and waveforms follow", res.Added)
	if err = publishImport(mount, dir); err != nil {
		return res, err
	}
	if exportBytes(r, mount) > attemptExportBytes {
		// The next mount would only start the update that reads the files of so large an
		// export: it starts now, and a remount publishes it (#253).
		return completeLater(dir, stage, stamp, res)
	}
	return res, keepPending(dir)
}

// buildFast builds the stage's library from the export alone, in bulk: every playlist and track,
// the cues of the export's own database, no file read.
func buildFast(ctx context.Context, stage string, r *compat.Resolver, l *rekordbox.Library, stagedOnly bool) (enginedb.Result, error) {
	build := filepath.Join(stage, bulkDir)
	if err := os.RemoveAll(build); err != nil {
		return enginedb.Result{}, err
	}
	if err := os.Mkdir(build, 0o755); err != nil {
		return enginedb.Result{}, err
	}
	defer os.RemoveAll(build)
	// The stage's snapshot of the live library, for its UUID and Engine's skeleton rows.
	if err := copyFile(filepath.Join(stage, "m.db"), filepath.Join(build, "m.db")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return enginedb.Result{}, err
	}
	mergeByPath(l)
	res, err := enginedb.SyncResolved(ctx, r, l, enginedb.Options{Fast: true, TrustPaths: true, FastCues: true, Bulk: true, DatabaseDir: build, StagedOnly: stagedOnly})
	if err != nil {
		return res, err
	}
	for _, name := range []string{"m.db", stateName} {
		if err = os.Rename(filepath.Join(build, name), filepath.Join(stage, name)); err != nil {
			return res, err
		}
	}
	return res, syncFile(stage)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if e := out.Close(); err == nil {
		err = e
	}
	return err
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
	cut("stage stamped")
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
	return renameStage(dir, stage)
}

// renameStage moves a complete stage into place: READY first, then finishPublish moves the
// state, the stamp and m.db, and completes a publish an interruption cut short at the next mount.
func renameStage(dir, stage string) error {
	// Engine's RGB overview files are keyed by track id: one the published library gives
	// another track goes now, while the library they were written against is still here.
	pruneRGBOverviews(context.Background(), dir, filepath.Join(stage, "m.db"), filepath.Join(dir, "m.db"))
	if err := os.WriteFile(filepath.Join(stage, publishReady), nil, 0o644); err != nil {
		return err
	}
	if err := syncFile(stage); err != nil {
		return err
	}
	cut("publish ready")
	if err := os.Rename(stage, filepath.Join(dir, publishDir)); err != nil {
		return err
	}
	cut("publish moved")
	if err := os.Remove(filepath.Join(dir, pendingName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cut("publish pending removed")
	return finishPublish(dir, stateName, stampName, "m.db", pendingName)
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
	if isUpdateStage(stage) {
		return updateInBackground(ctx, mount, stage, stamp, r, progress)
	}
	if err = os.RemoveAll(filepath.Join(stage, bulkDir)); err != nil { // a fastPublish cut short
		return enginedb.Result{}, err
	}
	l, stagedOnly, err := readLibrary(ctx, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	// A stage no pass has written yet gets the fast build first: the remount that publishes it
	// shows every playlist, track and cue within a minute or so, and the update that mount
	// starts brings the grids and waveforms with a second remount (#301).
	if _, err = os.Lstat(filepath.Join(stage, stateName)); errors.Is(err, os.ErrNotExist) {
		if progress != nil {
			progress(0, len(l.Tracks))
		}
		res, err := buildFast(ctx, stage, r, l, stagedOnly)
		if err != nil {
			return res, err
		}
		if progress != nil {
			progress(len(l.Tracks), len(l.Tracks))
		}
		return res, completeStage(stage, stamp)
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
