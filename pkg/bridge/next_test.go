package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// nextStick is a stick of n tracks whose first library a window built, with every track staged.
func nextStick(t *testing.T, n int) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, n)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	return mount
}

// makeNext is the job's part: the next library, ready for the swap.
func makeNext(t *testing.T, mount string) {
	t.Helper()
	r, err := newResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	layout := rbstate.LayoutFolders
	s, _ := rbstate.Load(dbDir(mount))
	if s != nil {
		layout = s.Layout
	}
	v, err := readJobView(context.Background(), r, mount, layout, "")
	if err != nil {
		t.Fatal(err)
	}
	n := &nextBuild{ctx: context.Background(), mount: mount, dir: dbDir(mount), view: v.view, live: s}
	if err = n.build(false, true); err != nil {
		t.Fatal(err)
	}
}

func engineWrites(t *testing.T, mount string, queries ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range queries {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func hookWindowNow(t *testing.T, mount string) (MountResult, string) {
	t.Helper()
	out := captureLog(t)
	res := mountNow(t, mount, MountOptions{Publish: true, Deadline: time.Now().Add(hookWindow), StartBackground: func() error { return nil }})
	return res, out.String()
}

func nextLeftovers(t *testing.T, mount string) {
	t.Helper()
	for _, name := range []string{nextDB, nextDB + "-journal", rbstate.SwapName, rbstate.SnapshotName + ".next"} {
		if _, err := os.Stat(filepath.Join(dbDir(mount), name)); err == nil {
			t.Errorf("%s is left in Database2", name)
		}
	}
	if q := query(t, mount, `PRAGMA integrity_check`); q[0][0] != "ok" {
		t.Errorf("integrity: %v", q)
	}
}

// What the job publishes as a next library is, after the one window that swaps it in, the library
// the windows would have written in place: the same rows under the same IDs, lists, performance
// data and snapshot. The job never writes the stick's m.db, and the window does no row work.
func TestNextLibrarySwappedInIsTheLibraryTheWindowsWrite(t *testing.T) {
	mount := nextStick(t, 60)
	base, stage := filepath.Dir(mount), os.Getenv(rbstate.StageEnv)
	copyTree(t, mount, filepath.Join(base, "inplace", "stick"))
	copyTree(t, stage, filepath.Join(base, "inplace", "stage"))
	ids := trackIDs(t, mount)
	live := filepath.Join(dbDir(mount), "m.db")
	before, _ := os.ReadFile(live)
	makeNext(t, mount)
	if after, _ := os.ReadFile(live); !bytes.Equal(before, after) {
		t.Fatal("the job wrote the stick's library")
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL`); n[0][0] != "0" {
		t.Fatalf("%s rows of the stick's library have staged data before the swap", n[0][0])
	}
	res, out := hookWindowNow(t, mount)
	if !res.Bypassed || !strings.Contains(out, "mount swap") || strings.Contains(out, "mount read") {
		t.Fatalf("the window of the remount: %+v\n%s", res, out)
	}
	got := state(t, mount)
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "inplace", "stage"))
	mountNow(t, filepath.Join(base, "inplace", "stick"), MountOptions{Publish: true})
	want := state(t, filepath.Join(base, "inplace", "stick"))
	t.Setenv(rbstate.StageEnv, stage)
	if got != want {
		t.Errorf("after the swap:\n%s\nin place:\n%s", got, want)
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.beatData IS NOT NULL AND p.overviewWaveFormData IS NOT NULL AND t.albumArtId IS NOT NULL`); n[0][0] != "60" {
		t.Errorf("%s of 60 rows hold the staged data and their cover", n[0][0])
	}
	if !reflect.DeepEqual(trackIDs(t, mount), ids) {
		t.Error("row IDs changed with the swap")
	}
	nextLeftovers(t, mount)
	// The records stayed in the stage for the deck's loads until the swap; the job then drops them.
	st, err := rbstate.OpenStage(snapshot(t, mount).LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if entries, _, _ := st.Entries(); len(entries) != 60 {
		t.Fatalf("%d records in the stage after the swap, want the 60 kept", len(entries))
	}
	if r := mustFill(t, mount, FillOptions{}); r.Publish {
		t.Fatalf("the job after the swap asks for a remount: %+v", r)
	}
	if entries, _, _ := st.Entries(); len(entries) != 0 {
		t.Fatalf("%d records left in the stage by the job after the swap", len(entries))
	}
	if again := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(hookWindow)}); !again.Bypassed {
		t.Fatalf("the stick inserted again: %+v", again)
	}
}

// What Engine wrote to the stick's library after the job made the next one ready is in the
// library after the swap: a played mark, a prepared track, a hot cue. The tracks concerned get
// their staged data in the job's next round, by the rule for a row the deck worked on.
func TestNextLibraryKeepsWhatTheDeckWroteBeforeTheSwap(t *testing.T) {
	mount := nextStick(t, 30)
	mountNow(t, mount, MountOptions{Publish: true})
	// Every track is to be published again (as after a change of the rules), through a next library.
	s := snapshot(t, mount)
	for _, tr := range s.Tracks {
		tr.Built, tr.Perf, tr.Written = rbstate.TrackSig{}, rbstate.PerfNone, nil
	}
	s.Pending.Perf = len(s.Tracks)
	if err := s.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}
	stageAll(t, mount)
	// (Rows written long ago: one written in the very second the job takes it is taken again.)
	engineWrites(t, mount, `UPDATE PerformanceData SET overviewWaveFormData=NULL`, `UPDATE Track SET lastEditTime=1700000000`)
	makeNext(t, mount)
	position := 250.0
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = enginedb.SetQuickCue(context.Background(), db, 2, 0, "deck edit", "#12AB34", &position); err != nil {
		t.Fatal(err)
	}
	db.Close()
	engineWrites(t, mount, `UPDATE Track SET isPlayed=1,timeLastPlayed=1700000000 WHERE id=1`, `INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(3,1)`)
	cue := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=2`)[0][0]
	res, out := hookWindowNow(t, mount)
	if !strings.Contains(out, "(2 rows the deck wrote") || !strings.Contains(out, "mount swap") || strings.Contains(out, "mount read") || res.Pending.Rows != 2 {
		t.Fatalf("the window of the remount takes the deck's two rows, swaps and does nothing else: %+v\n%s", res, out)
	}
	// The two tracks are published by the job's next round, in place: one window does it.
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData WHERE overviewWaveFormData IS NOT NULL`); n[0][0] != "28" {
		t.Fatalf("%s rows with staged data after the swap, want the 28 the deck did not write", n[0][0])
	}
	if r := mustFill(t, mount, FillOptions{}); !r.Publish {
		t.Fatalf("the job after the swap: %+v", r)
	}
	if res, out = hookWindowNow(t, mount); res.Applied != 2 || !res.Pending.None() {
		t.Fatalf("the window for the two tracks: %+v\n%s", res, out)
	}
	got := query(t, mount, `SELECT (SELECT isPlayed FROM Track WHERE id=1),(SELECT count(*) FROM PreparelistEntity WHERE trackId=3),
		(SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL AND overviewWaveFormData IS NOT NULL),(SELECT hex(quickCues) FROM PerformanceData WHERE trackId=2)`)[0]
	if got[0] != "1" || got[1] != "1" || got[2] != "30" {
		t.Fatalf("played %s, prepared %s, rows with staged data %s of 30", got[0], got[1], got[2])
	}
	if got[3] != cue {
		t.Fatal("the deck's hot cue was overwritten")
	}
	nextLeftovers(t, mount)
}

// A cut at any point of the window that swaps the next library in, and the next window on what
// was on the stick then, end in the library of an undisturbed swap.
func TestSwapCutAtEveryStepEndsInTheSameLibrary(t *testing.T) {
	mount := nextStick(t, 12)
	makeNext(t, mount)
	base, stage := filepath.Dir(mount), os.Getenv(rbstate.StageEnv)
	var cuts []string
	cut = func(step string) {
		at := filepath.Join(base, fmt.Sprintf("cut-%d", len(cuts)))
		copyTree(t, mount, filepath.Join(at, "stick"))
		copyTree(t, stage, filepath.Join(at, "stage"))
		cuts = append(cuts, step)
	}
	hookWindowNow(t, mount)
	cut = func(string) {}
	want := state(t, mount)
	if len(cuts) < 2 {
		t.Fatalf("cut points: %v", cuts)
	}
	for i, step := range cuts {
		at := filepath.Join(base, fmt.Sprintf("cut-%d", i))
		t.Setenv(rbstate.StageEnv, filepath.Join(at, "stage"))
		hookWindowNow(t, filepath.Join(at, "stick"))
		if got := state(t, filepath.Join(at, "stick")); got != want {
			t.Errorf("cut %d (%s) ends in another library:\n%s\nwant:\n%s", i, step, got, want)
		}
		nextLeftovers(t, filepath.Join(at, "stick"))
	}
	t.Setenv(rbstate.StageEnv, stage)
}

// A window of the hook that cannot end 3 s before the kill leaves the work to the job and gives
// Engine the stick: a never-seen export gets its library from the job's next library and one
// remount; a re-export keeps the old library on show until then, and its rows keep their IDs.
func TestMountLeavesWhatDoesNotFitToTheJob(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 80)
	started := 0
	short := func() MountOptions {
		return MountOptions{Deadline: time.Now().Add(spare + 20*time.Millisecond), StartBackground: func() error { started++; return nil }}
	}
	out := captureLog(t)
	res := mountNow(t, mount, short())
	if _, err := os.Stat(filepath.Join(dbDir(mount), "m.db")); err == nil || !res.Background || started != 1 || !strings.Contains(out.String(), "leaves the exports to the job") {
		t.Fatalf("first insert without the time: %+v, job started %d times\n%s", res, started, out)
	}
	if r := mustFill(t, mount, FillOptions{}); !r.Publish {
		t.Fatalf("the job: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), "m.db")); err == nil {
		t.Fatal("the job wrote the stick's library itself")
	}
	res = mountNow(t, mount, short())
	if n := query(t, mount, `SELECT (SELECT count(*) FROM Track)||' '||(SELECT count(DISTINCT trackId) FROM PlaylistEntity)`); n[0][0] != "80 80" || res.Pending.Rows != 0 || res.Pending.Lists {
		t.Fatalf("after the remount: %s tracks and listed tracks, %+v", n[0][0], res)
	}
	nextLeftovers(t, mount)
	ids := trackIDs(t, mount)

	// A re-export with 170 more tracks, and again no time.
	testfixture.LargeExport(t, mount, 250)
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
	res = mountNow(t, mount, short())
	if n := query(t, mount, `SELECT count(*) FROM Track`); n[0][0] != "80" || res.Added != 0 {
		t.Fatalf("re-export without the time: %s rows, %+v", n[0][0], res)
	}
	if r := mustFill(t, mount, FillOptions{}); !r.Publish {
		t.Fatalf("the job after the re-export: %+v", r)
	}
	res = mountNow(t, mount, short())
	if n := query(t, mount, `SELECT (SELECT count(*) FROM Track)||' '||(SELECT count(DISTINCT trackId) FROM PlaylistEntity)`); n[0][0] != "250 250" || res.Pending.Rows != 0 || res.Pending.Lists {
		t.Fatalf("after the second remount: %s, %+v", n[0][0], res)
	}
	now := trackIDs(t, mount)
	for path, id := range ids {
		if now[path] != id {
			t.Fatalf("row of %s went from ID %s to %s", path, id, now[path])
		}
	}
	nextLeftovers(t, mount)
}

// The estimate that keeps a window 3 s clear of the kill, against the replica's measured steps
// (KDATA, 5,962 tracks, and UNTITLED, 2,416; report rb-window-fable.md) and at the sizes no
// window can do.
func TestWorkCostAgainstTheReplica(t *testing.T) {
	const kdataOne, untitledOne, untitledPDB = 2818048, 1187840, 1691648
	measured := []struct {
		name string
		w    work
		ms   int64
	}{
		{"KDATA read, key derived", work{oneLibrary: kdataOne}, 7630},
		{"KDATA read, key cached", work{oneLibrary: kdataOne, warm: true}, 2200},
		{"UNTITLED read of both, key cached", work{oneLibrary: untitledOne, deviceLibrary: untitledPDB, warm: true}, 1250},
		{"UNTITLED view of two libraries in three folders", work{view: 2416}, 318}, // KDATA's one library: 560 ms for 5,962
		{"KDATA snapshot", work{load: 5962}, 570},
		{"KDATA first library", work{build: 5962}, 3750},
		{"KDATA take-over rows and lists", work{removes: 91, lists: 605}, 5680},
		{"KDATA staged window", work{staged: 320}, 10350},
		{"KDATA commit", work{commits: 1, tracks: 5962}, 640},
	}
	for _, m := range measured {
		if got := m.w.cost().Milliseconds(); got < m.ms*85/100 || got > m.ms*135/100 {
			t.Errorf("%s: estimated %d ms, measured %d ms", m.name, got, m.ms)
		}
	}
	window := &steps{kill: time.Now().Add(hookWindow)}
	first := func(bytes int64) work {
		w := exportWork([]rbstate.FileSig{{Rel: oneLibraryDB, Size: bytes}}, 0)
		w.build = w.view
		return w
	}
	if !window.fits(first(kdataOne)) {
		t.Error("KDATA's first insert is left to the job; it ends 4.7 s before the kill")
	}
	for _, tracks := range []int64{20000, 50000} {
		if window.fits(first(tracks * 480)) {
			t.Errorf("a first insert of %d tracks is not left to the job", tracks)
		}
		if window.fits(exportWork([]rbstate.FileSig{{Rel: oneLibraryDB, Size: tracks * 480}}, int(tracks))) {
			t.Errorf("a re-export of %d tracks is read in the window", tracks)
		}
	}
	if (&steps{}).fits(first(50000*480)) != true {
		t.Error("a run by hand has no limit")
	}
}
