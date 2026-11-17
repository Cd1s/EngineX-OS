package bridge_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #301: a 477 GB stick with a large rekordbox export never got its library. The first import
// ran while the mount waited, the edisksd hook killed it at the end of the mount's time, and
// the next insert started again from nothing. Engine's skeleton m.db, with the one track the
// DJ had loaded, was then taken for a converted library, and Engine showed it empty.

const issue301Tracks = 600

// issue301Stick is the reporter's stick: an export and the skeleton m.db stock Engine made,
// with a track the DJ loaded from the OneLibrary export. The import references that track
// from its playlists and leaves it Engine's.
func issue301Stick(t *testing.T, n int) (root string, skeleton *sql.DB) {
	t.Helper()
	_, root, skeleton = testlibrary.Create(t)
	testfixture.LargeExport(t, root, n)
	if _, err := skeleton.Exec(`INSERT INTO Track(path,filename,title) VALUES(?,?,?)`, "../"+testfixture.LargeTrackPath(1), "Track00001.mp3", "loaded on a deck"); err != nil {
		t.Fatal(err)
	}
	return root, skeleton
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func stagePath(root string, name ...string) string {
	return filepath.Join(append([]string{root, "Engine Library", "Database2", ".rb-bridge-import"}, name...)...)
}

// finished counts the stage's tracks with their files read.
func finished(t *testing.T, root string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+stagePath(root, ".rb-bridge-state.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM BridgeState WHERE fingerprint NOT LIKE 'fast:%'`).Scan(&n); err != nil {
		return -1
	}
	return n
}

func TestIssue301FirstImportPastTheMountWindowFinishesInTheBackground(t *testing.T) {
	root, skeleton := issue301Stick(t, issue301Tracks)
	live := filepath.Join(root, "Engine Library", "Database2", "m.db")
	var uuid string
	if err := skeleton.QueryRow(`SELECT uuid FROM Information`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	skeleton.Close()
	// The skeleton with a deck-loaded track is not a library to show instead of the export.
	if bridge.HasPopulatedLibrary(root) {
		t.Fatal("Engine's skeleton with a deck-loaded track counts as a converted library")
	}
	before := fileSum(t, live)
	ctx := context.Background()

	// The mount's time runs out while the export is read.
	restoreRead := bridge.SlowExportReadForTest(300*time.Millisecond, time.Second)
	restoreWindow := bridge.SetMountWindowForTest(200 * time.Millisecond)
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	reads := bridge.ExportReadsForTest()
	// Later mounts while the import goes on return at once, without reading the export.
	for mount := 2; mount <= 3; mount++ {
		if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
			t.Fatalf("mount %d: %+v %v", mount, r, err)
		}
	}
	restoreWindow()
	restoreRead()
	if got := bridge.ExportReadsForTest(); got != reads {
		t.Fatalf("mounts during the import read the export %d times", got-reads)
	}
	if fileSum(t, live) != before {
		t.Fatal("the mount wrote Engine's library")
	}
	for _, name := range []string{".rb-bridge.lock", ".rb-bridge-stamp", ".rb-bridge-pending", ".rb-bridge-state.db"} {
		if _, err = os.Stat(filepath.Join(filepath.Dir(live), name)); !os.IsNotExist(err) {
			t.Fatalf("the library got %s while it was not imported: %v", name, err)
		}
	}
	if bridge.HasPopulatedLibrary(root) {
		t.Fatal("Engine is shown a library that is not imported")
	}

	// After the mount: a pass at a time, however many it takes.
	defer bridge.SetImportWindowForTest(10 * time.Millisecond)()
	var done []int
	r, err = bridge.ImportInBackground(ctx, root, func(d, total int) {
		if total != issue301Tracks {
			t.Errorf("progress total %d", total)
		}
		done = append(done, d)
	})
	if err != nil || r.Deferred != 0 {
		t.Fatalf("background import: %+v %v", r, err)
	}
	for i := 1; i < len(done); i++ {
		if done[i] < done[i-1] {
			t.Fatalf("progress went back: %v", done)
		}
	}
	if len(done) < 3 || done[len(done)-1] != issue301Tracks {
		t.Fatalf("progress: %v", done)
	}
	if fileSum(t, live) != before {
		t.Fatal("the background import wrote Engine's library")
	}

	// The next mount publishes the whole library at once.
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background || !r.PlaylistsChanged {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	if got := scalar[string](t, db, `SELECT uuid FROM Information`); got != uuid {
		t.Fatalf("library UUID %q, Engine's was %q", got, uuid)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL AND quickCues IS NOT NULL`); n != issue301Tracks-1 {
		t.Fatalf("%d of %d imported tracks with their grid and cues", n, issue301Tracks-1)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity`); n != issue301Tracks {
		t.Fatalf("%d playlist entries", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.BridgeState WHERE fingerprint LIKE 'fast:%'`); n != 0 {
		t.Fatalf("%d tracks published without their files read", n)
	}
	if !bridge.HasPopulatedLibrary(root) {
		t.Fatal("the published library is not shown")
	}
	for _, p := range []string{stagePath(root), filepath.Join(filepath.Dir(live), ".rb-bridge-publish")} {
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("left %s: %v", p, err)
		}
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("published import not recorded: %+v %v", r, err)
	}
}

// An eject stops the job: what its passes committed stays, and the next job goes on from there.
// A kill (or a power cut) is the same, with the pass under way rolled back by SQLite.
func TestIssue301EjectOrKillKeepsTheFinishedPasses(t *testing.T) {
	if os.Getenv("ISSUE301_CHILD") != "" {
		return
	}
	const n = 1000
	root, skeleton := issue301Stick(t, n)
	skeleton.Close()
	live := filepath.Join(root, "Engine Library", "Database2", "m.db")
	before := fileSum(t, live)
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now()}, false)
	if err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	defer bridge.SetImportWindowForTest(100 * time.Millisecond)()

	// Eject: the job's context ends after some passes.
	ejected, eject := context.WithCancel(ctx)
	_, err = bridge.ImportInBackground(ejected, root, func(d, total int) {
		if d >= 100 {
			eject()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ejected job: %v", err)
	}
	afterEject := finished(t, root)
	if afterEject < 100 || afterEject >= n {
		t.Fatalf("%d tracks finished at the eject", afterEject)
	}

	// Kill: the job in another process, killed while it works.
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssue301Child$")
	cmd.Env = append(os.Environ(), "ISSUE301_CHILD="+root)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Minute); finished(t, root) < afterEject+50; {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the job made no progress")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	db, err := sql.Open("sqlite", stagePath(root, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, db, `PRAGMA integrity_check`); got != "ok" {
		t.Fatalf("stage after the kill: %s", got)
	}
	db.Close()
	afterKill := finished(t, root)
	if afterKill < afterEject+50 {
		t.Fatalf("the kill rolled back finished passes: %d, then %d", afterEject, afterKill)
	}
	if fileSum(t, live) != before {
		t.Fatal("the job wrote Engine's library")
	}

	// Nothing more can be read from here on: what has cues at the end are the tracks finished
	// before the kill, kept, not read again.
	if err = os.Rename(filepath.Join(root, "PIONEER/USBANLZ"), filepath.Join(root, "USBANLZ.gone")); err != nil {
		t.Fatal(err)
	}
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("resumed job: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.PlaylistsChanged {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	engine := dbOpen(t, root)
	read := `SELECT count(*) FROM PerformanceData WHERE quickCues=(SELECT p.quickCues FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=2)`
	if got := scalar[int](t, engine, read); got < afterKill || got >= n-1 {
		t.Fatalf("%d tracks with the cues read from their files, %d were finished before the kill", got, afterKill)
	}
	if got := scalar[int](t, engine, `SELECT count(*) FROM Track WHERE path LIKE '../Contents/%'`); got != n {
		t.Fatalf("%d tracks published", got)
	}
}

// TestIssue301Child is the job TestIssue301EjectOrKillKeepsTheFinishedPasses kills.
func TestIssue301Child(t *testing.T) {
	root := os.Getenv("ISSUE301_CHILD")
	if root == "" {
		t.Skip("run by TestIssue301EjectOrKillKeepsTheFinishedPasses")
	}
	defer bridge.SetImportWindowForTest(100 * time.Millisecond)()
	bridge.ImportInBackground(context.Background(), root, nil)
	time.Sleep(time.Minute) // finished first: the parent fails on its progress check
}

// Engine adds a track to its skeleton for each one the DJ loads while the import runs. That
// skeleton is kept beside the published library, not lost.
func TestIssue301SkeletonChangedDuringTheImportIsKept(t *testing.T) {
	root, skeleton := issue301Stick(t, 20)
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now()}, false)
	if err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err = skeleton.Exec(`INSERT INTO Track(path,filename,title) VALUES(?,?,?)`, "../"+testfixture.LargeTrackPath(2), "Track00002.mp3", "loaded during the import"); err != nil {
		t.Fatal(err)
	}
	skeleton.Close()
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("background import: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.PlaylistsChanged {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	kept, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db.before-import"))
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	if n := scalar[int](t, kept, `SELECT count(*) FROM Track WHERE title='loaded during the import'`); n != 1 {
		t.Fatalf("Engine's skeleton not kept: %d", n)
	}
	if n := scalar[int](t, dbOpen(t, root), `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL`); n != 19 {
		t.Fatalf("published %d tracks with their grid", n)
	}
}

// A stick that got a library of its own (here: copied from a computer) while a first import
// waited in its stage: the library is imported into in place and the stage goes.
func TestIssue301StageDroppedForALibraryOfItsOwn(t *testing.T) {
	root, skeleton := issue301Stick(t, 20)
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now()}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err := skeleton.Exec(`INSERT INTO Playlist(id,title) VALUES(7,'From the computer')`); err != nil {
		t.Fatal(err)
	}
	skeleton.Close()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)}, false)
	if err != nil || r.Background || r.Added != 19 {
		t.Fatalf("in-place import: %+v %v", r, err)
	}
	if _, err = os.Stat(stagePath(root)); !os.IsNotExist(err) {
		t.Fatalf("stage left: %v", err)
	}
	if got := scalar[string](t, dbOpen(t, root), `SELECT title FROM Playlist WHERE id=7`); got != "From the computer" {
		t.Fatalf("own playlist: %q", got)
	}
}

// #253 with a first import in the background: a 6000-track stick's later mounts return at
// once, without reading the export, while its import goes on.
func TestIssue301LargeFirstImportRepeatMounts253(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 6000)
	ctx := context.Background()
	start := time.Now()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	t.Logf("6000-track first mount: %s", time.Since(start))
	reads := bridge.ExportReadsForTest()
	if reads != 0 {
		t.Fatalf("the first mount of a large export read it %d times", reads)
	}
	for mount := 2; mount <= 3; mount++ {
		start = time.Now()
		if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
			t.Fatalf("mount %d: %+v %v", mount, r, err)
		}
		t.Logf("6000-track mount %d while importing: %s", mount, time.Since(start))
	}
	if got := bridge.ExportReadsForTest(); got != reads {
		t.Fatalf("mounts during the import read the export %d times", got-reads)
	}
}
