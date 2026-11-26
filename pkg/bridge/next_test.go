package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
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
func makeNext(t *testing.T, mount string) { t.Helper(); makeNextOf(t, mount, nil) }

// makeNextOf is makeNext with the view the job read changed first (a re-export the fixtures cannot write).
func makeNextOf(t *testing.T, mount string, edit func(*View)) {
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
	if edit != nil {
		edit(v.view)
	}
	n := &nextBuild{ctx: context.Background(), mount: mount, dir: dbDir(mount), view: v.view, live: s}
	if err = n.build(false, true); err != nil {
		t.Fatal(err)
	}
}

// recodeCues is a quickCues blob with its uncompressed bytes changed by edit.
func recodeCues(t *testing.T, cues []byte, edit func(raw []byte)) []byte {
	t.Helper()
	raw, err := enginedb.QUncompress(cues, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	edit(raw)
	if cues, err = enginedb.QCompress(raw); err != nil {
		t.Fatal(err)
	}
	return cues
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
	// Two tracks have a list waveform: one staged before it had its row, one with it (the job
	// wrote that file then).
	first := snapshot(t, mount)
	records, err := rbstate.OpenStage(first.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	var late, early int64
	for i, withRow := range []bool{false, true} {
		key := rbstate.FoldPath(testfixture.LargeTrackPath(i + 1))
		rec, err := records.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		rec.RGB = []byte{0, 0, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8}
		if id := first.Tracks[key].EngineID; withRow {
			rec.EngineID, early = id, id
		} else {
			late = id
		}
		if err = records.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err = records.Close(); err != nil {
		t.Fatal(err)
	}
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
	rgb, _ := filepath.Glob(filepath.Join(dbDir(mount), "OverviewData", first.LibraryUUID, "*.rgb"))
	if len(rgb) != 1 || filepath.Base(rgb[0]) != fmt.Sprintf("%d.rgb", late) {
		t.Fatalf("list waveforms after the swap: %v, want only row %d's (row %d's is the job's from the staging)", rgb, late, early)
	}
	if again := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(hookWindow)}); !again.Bypassed {
		t.Fatalf("the stick inserted again: %+v", again)
	}
	// A library whose export alone no window can read (50,000 tracks) with nothing left to publish:
	// the job makes no next library and asks for no remount. It did after every swap, without end.
	defer func(d time.Duration) { spare = d }(spare)
	spare = hookWindow
	if r := mustFill(t, mount, FillOptions{}); r.Publish {
		t.Fatalf("the job with nothing to publish asks for a remount: %+v", r)
	}
	nextLeftovers(t, mount)
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
	// (A window that has no time to look for the deck's rows does not swap: the job takes them.)
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

// What Engine made itself is in the library after a swap as it was (#252): a track of its own with
// its cue, the DJ's list, a prepared and a played track, and the history of the night, whether
// Engine made them before the job copied the library or while the next library waited for its
// remount.
func TestSwapKeepsWhatEngineMadeItself(t *testing.T) {
	mount := nextStick(t, 20)
	uuid := snapshot(t, mount).LibraryUUID
	// Engine's history is a file of its own beside m.db: its entries name tracks by ID and library.
	hm := filepath.Join(dbDir(mount), "hm.db")
	hdb, err := sql.Open("sqlite", "file:"+hm)
	if err != nil {
		t.Fatal(err)
	}
	defer hdb.Close()
	if _, err = hdb.Exec(`CREATE TABLE Historylist(id INTEGER PRIMARY KEY,title TEXT,startTime INTEGER);
		CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,listId INTEGER,trackId INTEGER,databaseUuid TEXT,startTime INTEGER);
		INSERT INTO Historylist VALUES(1,'Night',1700000000)`); err != nil {
		t.Fatal(err)
	}
	deck := func(n int, played int64) { // Engine's writes of one track played and one track of its own
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		res, err := db.Exec(`INSERT INTO Track(path,filename,title,rating) VALUES(?,?,?,80)`, fmt.Sprintf("../Music/own%d.mp3", n), fmt.Sprintf("own%d.mp3", n), fmt.Sprintf("Deck made %d", n))
		if err != nil {
			t.Fatal(err)
		}
		own, _ := res.LastInsertId()
		p5NativeList(t, db, fmt.Sprintf("Deck Set %d", n), 0, []int64{played, own}, false)
		for _, q := range []string{`INSERT OR REPLACE INTO PerformanceData(trackId,quickCues) VALUES(?2,x'D0')`, `INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(?1,?3)`,
			`UPDATE Track SET isPlayed=1,timeLastPlayed=1700000000+?3 WHERE id=?1`} {
			if _, err = db.Exec(q, played, own, n); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if _, err = hdb.Exec(`INSERT INTO HistorylistEntity(listId,trackId,databaseUuid,startTime) VALUES(1,?1,?3,1700000000),(1,?2,?3,1700000100)`, played, own, uuid); err != nil {
			t.Fatal(err)
		}
	}
	engineMade := func() string {
		var b strings.Builder
		fmt.Fprintln(&b, query(t, mount, `SELECT t.id,t.path,t.title,t.rating,t.isPlayed,t.timeLastPlayed,CASE WHEN t.id>20 THEN hex(p.quickCues) END
			FROM Track t JOIN PerformanceData p ON p.trackId=t.id WHERE t.id>20 OR t.isPlayed=1 ORDER BY t.id`))
		fmt.Fprintln(&b, query(t, mount, `SELECT trackId,trackNumber FROM PreparelistEntity ORDER BY trackNumber`))
		fmt.Fprintln(&b, query(t, mount, `SELECT l.title,e.trackId FROM PlaylistEntity e JOIN Playlist l ON l.id=e.listId WHERE l.title LIKE 'Deck Set%' ORDER BY l.title,e.id`))
		for _, line := range libraryTree(t, mount) {
			if strings.Contains(line, "Deck Set") {
				fmt.Fprintln(&b, line)
			}
		}
		// The history: each entry with the track it names in the stick's library.
		rows, err := hdb.Query(`SELECT trackId,databaseUuid FROM HistorylistEntity ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var lib string
			if err = rows.Scan(&id, &lib); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&b, "played", query(t, mount, `SELECT path,(SELECT uuid FROM Information)=?2 FROM Track WHERE id=?1`, id, lib))
		}
		return b.String()
	}
	deck(1, 3)
	makeNext(t, mount)
	deck(2, 5)
	want := engineMade()
	if strings.Count(want, "[Deck Set") != 4 || !strings.Contains(want, "own2.mp3 Deck made 2 80") || strings.Count(want, "played [[../") != 4 {
		t.Fatalf("what Engine made before the swap:\n%s", want)
	}
	history, _ := os.ReadFile(hm)
	if _, out := hookWindowNow(t, mount); !strings.Contains(out, "mount swap") {
		t.Fatalf("the window did not swap:\n%s", out)
	}
	if got := engineMade(); got != want {
		t.Errorf("after the swap:\n%s\nwant:\n%s", got, want)
	}
	// The job's next round gives the rows the deck wrote their staged data, by the deck-edit rules.
	if r := mustFill(t, mount, FillOptions{}); r.Publish {
		hookWindowNow(t, mount)
	}
	if got := engineMade(); got != want {
		t.Errorf("after the job's next round:\n%s\nwant:\n%s", got, want)
	}
	if after, _ := os.ReadFile(hm); !bytes.Equal(after, history) {
		t.Error("Engine's history file changed")
	}
	if n := query(t, mount, `SELECT (SELECT count(*) FROM Track),(SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL)`)[0]; n[0] != "22" || n[1] != "20" {
		t.Errorf("%s tracks, %s with staged data; want the 20 of the export with their data and Engine's 2", n[0], n[1])
	}
	for id := range snapshot(t, mount).EngineIDs() {
		if id > 20 {
			t.Errorf("the snapshot owns row %d, which Engine made", id)
		}
	}
	nextLeftovers(t, mount)
}

// The damage of 2026-11-23 (KDATA: tracks showed other tracks' cues and grids) came from matching
// rows by rekordbox's IDs, which a re-export renumbers. A track is its path, across a swap too: a
// re-export in which every ID names another file and five tracks are new is published as a next
// library, while Engine adds a track of its own and rates one. Every row then holds the data built
// for its own file, under the row ID it had.
func TestSwapNeverGivesATrackAnotherTracksData(t *testing.T) {
	mount := nextStick(t, 40)
	ids := trackIDs(t, mount)
	testfixture.LargeExport(t, mount, 45)
	number := func(path string) (n int) { // of LargeTrackPath; 0 for any other file
		fmt.Sscanf(strings.ToLower(filepath.Base(path)), "track%05d.mp3", &n)
		return n
	}
	r, err := newResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, mount)
	v, err := readJobView(context.Background(), r, mount, s.Layout, "")
	if err != nil {
		t.Fatal(err)
	}
	// Every track's staged data is its own: hot cue A lies 1,000 samples times the track's number in.
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := stage.Get(v.tracks[0].key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range v.view.Tracks {
		vt := &v.view.Tracks[i]
		rec, err := stage.Get(vt.Key)
		if err != nil { // a new track, staged as the job stages it
			c := *model
			c.Key, c.Rel, c.EngineID, c.Sig.Row, c.Sig.Audio.Rel = vt.Key, vt.Rel, 0, vt.Hash, vt.Rel
			rec = &c
		}
		rec.Perf.QuickCues = recodeCues(t, model.Perf.QuickCues, func(raw []byte) {
			binary.BigEndian.PutUint64(raw[8+1+int(raw[8]):], math.Float64bits(float64(1000*number(vt.Rel))))
		})
		rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
		if err = stage.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err = stage.Close(); err != nil {
		t.Fatal(err)
	}
	renumbered := func(view *View) { // every ID names the next file
		for i := range view.Tracks {
			vt := &view.Tracks[i]
			vt.Track.ID = vt.Track.ID%45 + 1
			for j := range vt.Refs {
				vt.Refs[j].ID = vt.Refs[j].ID%45 + 1
			}
		}
	}
	// own checks every row: an export's track holds its own cue or none yet, Engine's track none.
	own := func(when string) (with int) {
		t.Helper()
		seen := map[string]bool{}
		for _, row := range query(t, mount, `SELECT t.id,t.path,COALESCE(hex(p.quickCues),'') FROM Track t JOIN PerformanceData p ON p.trackId=t.id`) {
			if seen[strings.ToLower(row[1])] {
				t.Errorf("%s: two rows for %s", when, row[1])
			}
			seen[strings.ToLower(row[1])] = true
			n := number(row[1])
			if was, ok := ids[row[1]]; ok && was != row[0] {
				t.Errorf("%s: %s is row %s, was row %s", when, row[1], row[0], was)
			}
			if row[2] == "" {
				continue
			}
			var cues []byte
			fmt.Sscanf(row[2], "%X", &cues)
			raw, err := enginedb.QUncompress(cues, 1<<20)
			if err != nil {
				t.Fatalf("%s: row %s: %v", when, row[0], err)
			}
			if at := math.Float64frombits(binary.BigEndian.Uint64(raw[8+1+int(raw[8]):])); n == 0 || at != float64(1000*n) {
				t.Errorf("%s: row %s (%s) holds the cue of track %v", when, row[0], row[1], at/1000)
			}
			with++
		}
		return with
	}
	makeNextOf(t, mount, renumbered)
	engineWrites(t, mount, `INSERT INTO Track(path,filename,title) VALUES('../Music/own.mp3','own.mp3','Deck made')`, `UPDATE Track SET rating=80 WHERE id=7`)
	if _, out := hookWindowNow(t, mount); !strings.Contains(out, "mount swap") {
		t.Fatalf("the window did not swap:\n%s", out)
	}
	if n := own("after the swap"); n < 40 {
		t.Errorf("%d rows with their data after the swap", n)
	}
	// The row the deck rated was taken as the deck left it; the job's next round gives it its data.
	makeNextOf(t, mount, renumbered)
	if _, out := hookWindowNow(t, mount); !strings.Contains(out, "mount swap") {
		t.Fatalf("the second window did not swap:\n%s", out)
	}
	if n := own("after the second swap"); n != 45 {
		t.Errorf("%d rows with their data after the second swap, want the export's 45", n)
	}
	if got := query(t, mount, `SELECT (SELECT count(*) FROM Track),(SELECT rating FROM Track WHERE id=7),(SELECT title FROM Track WHERE path='../Music/own.mp3')`)[0]; got[0] != "46" || got[1] != "80" || got[2] != "Deck made" {
		t.Errorf("tracks, the deck's rating, Engine's own track: %v", got)
	}
	after := snapshot(t, mount)
	for key, tr := range after.Tracks {
		row := query(t, mount, `SELECT path FROM Track WHERE id=?`, tr.EngineID)
		if len(row) != 1 || rbstate.FoldPath(strings.TrimPrefix(row[0][0], "../")) != key {
			t.Errorf("the snapshot's row of %s is %v", key, row)
		}
		if want := uint32(number(tr.Rel)%45 + 1); len(tr.Refs) != 1 || tr.Refs[0].ID != want {
			t.Errorf("%s: refs %+v, want the re-export's ID %d", key, tr.Refs, want)
		}
	}
	nextLeftovers(t, mount)
}

// A deck's /data cannot hold the stage of a big library (KDATA: 306 MiB for 5,962 tracks; the
// stage stops 256 MiB short of a full disk). At that floor the job writes what is staged into the
// next library, takes it out of the stage and goes on, and one remount publishes every track:
// also the ones whose records left the stage, however few are staged at the end.
func TestFillGoesOnPastAFullStageThroughTheNextLibrary(t *testing.T) {
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.LargeExport(t, mount, 30)
	mountNow(t, mount, MountOptions{})
	old := putStaged
	defer func() { putStaged = old }()
	putStaged = func(s *rbstate.Stage, rec *rbstate.Staged) error {
		if held, _ := filepath.Glob(filepath.Join(s.Dir(), "*.bin")); len(held) >= 8 { // room for eight tracks
			return rbstate.ErrStageFull
		}
		return old(s, rec)
	}
	out := captureLog(t)
	res := mustFill(t, mount, FillOptions{})
	if res.Staged != 30 || res.Left != 0 || !res.Publish || strings.Count(out.String(), "is full after") != 3 {
		t.Fatalf("the job with a stage of eight tracks: %+v\n%s", res, out)
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL`); n[0][0] != "0" {
		t.Fatalf("%s rows of the stick's library have data before the remount", n[0][0])
	}
	if _, log := hookWindowNow(t, mount); !strings.Contains(log, "mount swap") {
		t.Fatalf("the window of the remount did not swap:\n%s", log)
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL AND overviewWaveFormData IS NOT NULL`); n[0][0] != "30" {
		t.Fatalf("%s of 30 rows have their data after the one remount", n[0][0])
	}
	if s := snapshot(t, mount); !s.Pending.None() {
		t.Fatalf("pending after the remount: %+v", s.Pending)
	}
	if r := mustFill(t, mount, FillOptions{}); r.Publish || r.Staged != 0 {
		t.Fatalf("the job after the remount: %+v", r)
	}
	nextLeftovers(t, mount)
}

// A window that cannot look for the rows the deck wrote and still end 3 s before the kill does
// not swap: the stick's library stays as it is, the job takes the rows, and the next window
// swaps. (50,000 tracks in one list: the look read 100,002 rows in 15.7 s and the window was
// killed; the estimate counted the tracks only.)
func TestSwapWaitsForTheJobWhenTheDecksRowsCannotBeLookedForInTime(t *testing.T) {
	mount := nextStick(t, 12)
	makeNext(t, mount)
	engineWrites(t, mount, `UPDATE Track SET rating=80 WHERE id=2`)
	live := filepath.Join(dbDir(mount), "m.db")
	before, _ := os.ReadFile(live)
	old := spare
	spare = hookWindow - 100*time.Millisecond // no step fits
	_, out := hookWindowNow(t, mount)
	spare = old
	if strings.Contains(out, "mount swap") || strings.Contains(out, "mount carry") || !strings.Contains(out, "the job takes the changes first") {
		t.Fatalf("the window without time:\n%s", out)
	}
	if after, _ := os.ReadFile(live); !bytes.Equal(after, before) {
		t.Fatal("the window without time changed the stick's library")
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), nextDB)); err != nil {
		t.Fatalf("the next library is gone: %v", err)
	}
	makeNext(t, mount) // the job's next round takes the deck's row
	if _, out = hookWindowNow(t, mount); !strings.Contains(out, "mount swap") {
		t.Fatalf("the window after the job's next round did not swap:\n%s", out)
	}
	if got := query(t, mount, `SELECT rating FROM Track WHERE id=2`); got[0][0] != "80" {
		t.Fatalf("the deck's rating after the swap: %v", got)
	}
	nextLeftovers(t, mount)
}

// The estimate can be wrong (rows that lie between waveforms cost twice the replica's figure): a
// look for the deck's rows that is still running where the window's rule says it must end is
// stopped there. The window does not swap, the stick's library is as it was, and the job's next
// round and its window take the rows.
func TestSwapStopsALookForTheDecksRowsThatOutlastsItsEstimate(t *testing.T) {
	mount := nextStick(t, 12)
	makeNext(t, mount)
	engineWrites(t, mount, `UPDATE Track SET rating=80 WHERE id=2`)
	live := filepath.Join(dbDir(mount), "m.db")
	before, _ := os.ReadFile(live)
	oldSpare, oldLook := spare, mirrorLibrary
	defer func() { spare, mirrorLibrary = oldSpare, oldLook }()
	spare = hookWindow - time.Second // the estimate (a few ms of rows, 250 ms of saving) fits; the look has about 0.7 s
	var stopped time.Duration
	mirrorLibrary = func(ctx context.Context, live, next string, m *enginedb.Mirror, pace func(time.Duration) error) (enginedb.MirrorChange, error) {
		start := time.Now()
		select { // a look that would not end in time
		case <-ctx.Done():
			stopped = time.Since(start)
		case <-time.After(10 * time.Second):
		}
		return enginedb.MirrorChange{}, ctx.Err()
	}
	start := time.Now()
	_, out := hookWindowNow(t, mount)
	if took := time.Since(start); stopped == 0 || took > 3*time.Second {
		t.Fatalf("the look was not stopped at the window's limit (stopped after %v, window %v)", stopped, took)
	}
	if strings.Contains(out, "mount swap") || !strings.Contains(out, "the job takes the changes first") {
		t.Fatalf("the window whose look was stopped:\n%s", out)
	}
	if after, _ := os.ReadFile(live); !bytes.Equal(after, before) {
		t.Fatal("the window whose look was stopped changed the stick's library")
	}
	mirrorLibrary, spare = oldLook, oldSpare
	makeNext(t, mount) // the job's next round takes the deck's row
	if _, out = hookWindowNow(t, mount); !strings.Contains(out, "mount swap") {
		t.Fatalf("the window after the job's next round did not swap:\n%s", out)
	}
	if got := query(t, mount, `SELECT rating FROM Track WHERE id=2`); got[0][0] != "80" {
		t.Fatalf("the deck's rating after the swap: %v", got)
	}
	nextLeftovers(t, mount)
}

// The remount helper takes the deck's rows right before it triggers the remount, with no
// window's limit (design 1.11 item 6): the window that had no time to look for them (the test
// above) then finds the library the next one was made from, and only renames.
func TestRowsTakenBeforeTheRemountLetAWindowWithoutTimeSwap(t *testing.T) {
	for _, c := range []struct {
		name, write, check, want string
		rows                     int
	}{
		// What Engine writes when it mounts a stick: no track's row, and the window still had to look.
		{"Engine mounted the stick", `UPDATE Information SET currentPlayedIndiciator=4711`, `SELECT currentPlayedIndiciator FROM Information`, "4711", 0},
		{"the deck rated a track", `UPDATE Track SET rating=80 WHERE id=2`, `SELECT rating FROM Track WHERE id=2`, "80", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			mount := nextStick(t, 12)
			makeNext(t, mount)
			if TakeDeckRows(context.Background(), mount) {
				t.Fatal("the stick's library is as the job left it: there is nothing to look for")
			}
			engineWrites(t, mount, c.write)
			out := captureLog(t)
			if !TakeDeckRows(context.Background(), mount) || !strings.Contains(out.String(), fmt.Sprintf("before the remount, %d rows the deck wrote taken into the next library", c.rows)) {
				t.Fatalf("what the deck wrote was not taken before the remount:\n%s", out)
			}
			if TakeDeckRows(context.Background(), mount) {
				t.Fatal("the rows were looked for again though the deck wrote nothing since")
			}
			old := spare
			spare = hookWindow - 100*time.Millisecond // no step fits
			res, text := hookWindowNow(t, mount)
			spare = old
			if !strings.Contains(text, "mount swap") || strings.Contains(text, "mount carry") || strings.Contains(text, "the job takes the changes first") || res.Pending.Rows != c.rows {
				t.Fatalf("the window without time after the helper's pass: %+v\n%s", res, text)
			}
			if got := query(t, mount, c.check); got[0][0] != c.want {
				t.Fatalf("after the swap: %v, want %s", got, c.want)
			}
			nextLeftovers(t, mount)
			// Without a next library that waits there is nothing to do.
			if TakeDeckRows(context.Background(), mount) || TakeDeckRows(context.Background(), t.TempDir()) {
				t.Fatal("rows were looked for without a next library")
			}
		})
	}
}

// Finding 3.3: once the remount helper's context is cancelled (Eject / SIGTERM),
// TakeDeckRows and carryOver must not open or rewrite .rb-bridge-snapshot.swap.
func TestTakeDeckRowsAndCarryOverRespectCancelledContext(t *testing.T) {
	t.Run("cancelled before TakeDeckRows", func(t *testing.T) {
		mount := nextStick(t, 12)
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET rating=80 WHERE id=2`)

		oldLook := mirrorLibrary
		defer func() { mirrorLibrary = oldLook }()
		called := false
		mirrorLibrary = func(ctx context.Context, live, next string, m *enginedb.Mirror, pace func(time.Duration) error) (enginedb.MirrorChange, error) {
			called = true
			return oldLook(ctx, live, next, m, pace)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if TakeDeckRows(ctx, mount) {
			t.Fatal("TakeDeckRows reported work on an already-cancelled context")
		}
		if called {
			t.Fatal("TakeDeckRows called mirrorLibrary on an already-cancelled context")
		}
	})

	t.Run("cancelled before carryOver writes swap", func(t *testing.T) {
		mount := nextStick(t, 12)
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET rating=80 WHERE id=2`)
		swapPath := filepath.Join(dbDir(mount), rbstate.SwapName)
		before, err := os.ReadFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		oldLook := mirrorLibrary
		defer func() { mirrorLibrary = oldLook }()
		mirrorLibrary = func(c context.Context, live, next string, m *enginedb.Mirror, pace func(time.Duration) error) (enginedb.MirrorChange, error) {
			ch, err := oldLook(c, live, next, m, pace)
			cancel()
			return ch, err
		}

		TakeDeckRows(ctx, mount)
		after, err := os.ReadFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("carryOver rewrote .rb-bridge-snapshot.swap after ctx was cancelled")
		}
	})
}

// What the helper took is noted in the next library's state on the deck, so that whoever comes
// next reads on from it. Engine writes again before the remount (and had added a cover before the
// helper's pass): the window takes that one row only, and the cover is not copied a second time.
// And when no window swaps (Engine still held the file), the job's next round finds the rows the
// helper took as pending and writes their staged data again: none is left without.
func TestRowsTakenBeforeTheRemountAreReadOnFrom(t *testing.T) {
	prepare := func(t *testing.T) string {
		mount := nextStick(t, 12)
		mountNow(t, mount, MountOptions{Publish: true})
		s := snapshot(t, mount) // every track is to be published again, through a next library
		for _, tr := range s.Tracks {
			tr.Built, tr.Perf, tr.Written = rbstate.TrackSig{}, rbstate.PerfNone, nil
		}
		s.Pending.Perf = len(s.Tracks)
		if err := s.Save(dbDir(mount)); err != nil {
			t.Fatal(err)
		}
		stageAll(t, mount)
		engineWrites(t, mount, `UPDATE PerformanceData SET overviewWaveFormData=NULL`, `UPDATE Track SET lastEditTime=1700000000`)
		makeNext(t, mount)
		engineWrites(t, mount, `INSERT INTO AlbumArt(hash,albumArt) VALUES('deck',x'FFD8FF')`, `UPDATE Track SET isPlayed=1,timeLastPlayed=1700000000 WHERE id=2`)
		if !TakeDeckRows(context.Background(), mount) {
			t.Fatal("the deck's row was not taken before the remount")
		}
		return mount
	}
	withData := `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL AND overviewWaveFormData IS NOT NULL`
	t.Run("the window", func(t *testing.T) {
		mount := prepare(t)
		engineWrites(t, mount, `UPDATE Track SET isPlayed=1,timeLastPlayed=1700000001 WHERE id=3`)
		res, out := hookWindowNow(t, mount)
		if !strings.Contains(out, "(1 rows the deck wrote") || !strings.Contains(out, "mount swap") || res.Pending.Rows != 2 {
			t.Fatalf("the window after the helper's pass and one more row of the deck: %+v\n%s", res, out)
		}
		got := query(t, mount, `SELECT (SELECT count(*) FROM Track WHERE isPlayed=1 AND id IN (2,3)),(SELECT count(*) FROM AlbumArt WHERE hash='deck'),(`+withData+`)`)[0]
		if got[0] != "2" || got[1] != "1" || got[2] != "10" {
			t.Fatalf("played %s of 2, the deck's cover %s times, rows with staged data %s (want the 10 the deck did not write)", got[0], got[1], got[2])
		}
		nextLeftovers(t, mount)
	})
	t.Run("the job", func(t *testing.T) {
		mount := prepare(t)
		makeNext(t, mount)
		res, out := hookWindowNow(t, mount)
		if !strings.Contains(out, "mount swap") || strings.Contains(out, "mount carry") || !res.Pending.None() {
			t.Fatalf("the window after the job's next round: %+v\n%s", res, out)
		}
		got := query(t, mount, `SELECT (SELECT isPlayed FROM Track WHERE id=2),(SELECT count(*) FROM AlbumArt WHERE hash='deck'),(`+withData+`)`)[0]
		if got[0] != "1" || got[1] != "1" || got[2] != "12" {
			t.Fatalf("played %s, the deck's cover %s times, rows with staged data %s of 12", got[0], got[1], got[2])
		}
		nextLeftovers(t, mount)
	})
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
		{"KDATA read, key derived", work{oneLibrary: kdataOne}, 7196}, // the reads at 7b3efe4f (7fa6a009 merged)
		{"KDATA read, key cached", work{oneLibrary: kdataOne, warm: true}, 1348},
		{"UNTITLED read of both, key derived", work{oneLibrary: untitledOne, deviceLibrary: untitledPDB}, 6584},
		{"UNTITLED read of both, key cached", work{oneLibrary: untitledOne, deviceLibrary: untitledPDB, warm: true}, 820},
		{"UNTITLED view of two libraries in three folders", work{view: 2416}, 270}, // KDATA's one library: 530 ms for 5,962
		{"KDATA snapshot", work{load: 5962}, 570},
		{"KDATA first library", work{build: 5962}, 3750},
		{"KDATA take-over rows and lists", work{removes: 91, lists: 605, entries: 6523}, 5680},
		{"30 tracks more in a list of 20,030 (synthetic)", work{adds: 30, lists: 1, entries: 20030, commits: 1, tracks: 20000}, 4389},
		{"KDATA staged window", work{staged: 320}, 10350},
		{"KDATA commit", work{commits: 1, tracks: 5962}, 640},
		{"KDATA deck-row look (13,169 rows, scattered)", work{scan: 13169}, 4400},
	}
	for _, m := range measured {
		if got := m.w.cost().Milliseconds(); got < m.ms*85/100 || got > m.ms*135/100 {
			t.Errorf("%s: estimated %d ms, measured %d ms", m.name, got, m.ms)
		}
	}
	if got := (work{scan: 100002}).cost(); got < 33*time.Second {
		t.Errorf("50,000-track deck-row scan estimated %s, want >= 33s", got)
	}
	window := &steps{kill: time.Now().Add(hookWindow)}
	first := func(bytes int64) work {
		w := exportWork([]rbstate.FileSig{{Rel: oneLibraryDB, Size: bytes}}, 0)
		w.build = w.view
		return w
	}
	if !window.fits(first(kdataOne)) {
		t.Error("KDATA's first insert is left to the job; it ends 4.6 s before the kill")
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

func TestReconcileOrphanSwapPromotesCutSwapAndDropsStaleOrCorruptSwap(t *testing.T) {
	t.Run("cut_swap_promoted_by_background_job", func(t *testing.T) {
		mount := nextStick(t, 5)
		makeNext(t, mount)
		dir := dbDir(mount)
		swapPath := filepath.Join(dir, rbstate.SwapName)
		wantHead, err := rbstate.ReadHeaderFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate a 17 s SIGKILL right between renaming .rb-next.db -> m.db and renaming
		// .rb-bridge-snapshot.swap -> .rb-bridge-snapshot.
		if err := os.Rename(filepath.Join(dir, nextDB), filepath.Join(dir, "m.db")); err != nil {
			t.Fatal(err)
		}
		// FillInBackground must promote .rb-bridge-snapshot.swap -> .rb-bridge-snapshot under LockVolume.
		if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
			t.Fatalf("FillInBackground: %v", err)
		}
		if _, err := os.Stat(swapPath); !os.IsNotExist(err) {
			t.Fatalf("leftover %s still exists after FillInBackground: err=%v", rbstate.SwapName, err)
		}
		gotHead, err := rbstate.ReadHeader(dir)
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}
		if gotHead.Commit != wantHead.Commit {
			t.Fatalf("snapshot commit = %d, want promoted swap commit %d", gotHead.Commit, wantHead.Commit)
		}
	})

	t.Run("corrupt_swap_removed_by_swapNext", func(t *testing.T) {
		mount := nextStick(t, 3)
		dir := dbDir(mount)
		swapPath := filepath.Join(dir, rbstate.SwapName)
		if err := os.WriteFile(swapPath, []byte("truncated-garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		swapped, _ := swapNext(context.Background(), mount, dir, newSteps(MountOptions{}), false, "")
		if swapped {
			t.Fatal("swapNext swapped a corrupt swap file")
		}
		if _, err := os.Stat(swapPath); !os.IsNotExist(err) {
			t.Fatalf("corrupt %s was not removed: err=%v", rbstate.SwapName, err)
		}
	})

	t.Run("transient_live_db_error_keeps_valid_swap", func(t *testing.T) {
		mount := nextStick(t, 3)
		makeNext(t, mount)
		dir := dbDir(mount)
		swapPath := filepath.Join(dir, rbstate.SwapName)
		if err := os.Remove(filepath.Join(dir, nextDB)); err != nil {
			t.Fatal(err)
		}
		// Corrupt live m.db so openRO / SQL query returns an error; reconcileOrphanSwap and swapNext must keep swap!
		if err := os.WriteFile(filepath.Join(dir, "m.db"), []byte("temporarily-unreadable-db"), 0o644); err != nil {
			t.Fatal(err)
		}
		if promoted, _ := reconcileOrphanSwap(context.Background(), dir); promoted {
			t.Fatal("reconcileOrphanSwap promoted swap despite corrupt m.db")
		}
		if swapped, _ := swapNext(context.Background(), mount, dir, newSteps(MountOptions{}), false, ""); swapped {
			t.Fatal("swapNext promoted swap despite corrupt m.db")
		}
		if _, err := os.Stat(swapPath); err != nil {
			t.Fatalf("valid %s deleted on live DB read error: %v", rbstate.SwapName, err)
		}
	})
}

func TestInsertPathOrphanSwapReconciliationAndCarryOverBrowseIndex(t *testing.T) {
	t.Run("same_commit_different_uuid_drops_orphan_swap", func(t *testing.T) {
		mount := nextStick(t, 3)
		makeNext(t, mount)
		dir := dbDir(mount)
		swapPath := filepath.Join(dir, rbstate.SwapName)
		wantHead, err := rbstate.ReadHeaderFile(swapPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, nextDB)); err != nil {
			t.Fatal(err)
		}
		// Set live m.db commit to swap's commit while changing UUID so only a buggy commit-only check promotes it.
		engineWrites(t, mount, fmt.Sprintf("UPDATE Information SET lastRekordBoxLibraryImportReadCounter=%d, uuid='other-uuid-0000'", wantHead.Commit))
		if swapped, _ := swapNext(context.Background(), mount, dir, newSteps(MountOptions{}), false, ""); swapped {
			t.Fatal("swapNext promoted orphan swap with mismatched UUID")
		}
		if _, err := os.Stat(swapPath); !os.IsNotExist(err) {
			t.Fatalf("obsolete UUID-mismatched %s was not removed: err=%v", rbstate.SwapName, err)
		}
	})

	t.Run("ondemand_interrupted_swap_and_carryover_browse_tag", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
		mount := filepath.Join(base, "stick")
		testfixture.LargeExport(t, mount, 4)

		if _, err := FillInBackground(context.Background(), mount, FillOptions{OnDemand: true}); err != nil {
			t.Fatalf("FillInBackground(OnDemand): %v", err)
		}
		// Add a deck edit to m.db? Make an initial live mount first so live DB has m.db, then build next.
		mount2 := nextStick(t, 4)
		dir := dbDir(mount2)
		mountNow(t, mount2, MountOptions{OnDemand: true})
		r, err := newResolver(mount2)
		if err != nil {
			t.Fatal(err)
		}
		s, err := rbstate.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		v, err := readJobView(context.Background(), r, mount2, s.Layout, "")
		if err != nil {
			t.Fatal(err)
		}
		v.view.Tracks[0].Track.Title = "updated-title-next"
		v.view.Tracks[0].Hash = "updated-hash-next"
		nb := &nextBuild{ctx: context.Background(), mount: mount2, dir: dir, view: v.view, live: s, onDemand: true}
		if err := nb.build(false, true); err != nil {
			t.Fatalf("nextBuild.build: %v", err)
		}
		// Simulate deck edit between next build and swap -> carryOverChecked must rewrite matching BrowseSwapName!
		engineWrites(t, mount2, "UPDATE Track SET rating=60, lastEditTime=1700000999 WHERE id=2")
		looked, err := TakeDeckRowsChecked(context.Background(), mount2)
		if err != nil || !looked {
			t.Fatalf("TakeDeckRowsChecked: looked=%v err=%v", looked, err)
		}
		swapHdr, err := rbstate.ReadHeaderFile(filepath.Join(dir, rbstate.SwapName))
		if err != nil {
			t.Fatal(err)
		}
		if !rbstate.BrowseIndexMatchesFile(filepath.Join(dir, rbstate.BrowseSwapName), swapHdr) {
			t.Fatalf("%s does not match carried-over swap header", rbstate.BrowseSwapName)
		}
		// Now simulate interrupted swap right between renaming .rb-next.db -> m.db and renaming .swap -> snapshot.
		if err := os.Rename(filepath.Join(dir, nextDB), filepath.Join(dir, "m.db")); err != nil {
			t.Fatal(err)
		}
		res := mountNow(t, mount2, MountOptions{OnDemand: true, Publish: true})
		if res.Pending.Rows == 0 && !res.Pending.Lists && !res.Bypassed {
			// OK: swapped in cleanly or updated pending carried row
		}
		liveHdr := snapshot(t, mount2)
		if liveHdr.Commit != swapHdr.Commit {
			t.Fatalf("interrupted swap not promoted on ConvertAtMount: live commit=%d swap=%d", liveHdr.Commit, swapHdr.Commit)
		}
		if !rbstate.BrowseIndexMatches(dir, liveHdr.Header) {
			t.Fatalf("live %s does not match promoted snapshot after interrupted swap", rbstate.BrowseIndexName)
		}
	})
}
