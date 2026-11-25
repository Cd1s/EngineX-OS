package bridge

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

// stick is a test's volume with the 5-track Device Library fixture, and the deck's stage beside it.
func stick(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(base, "stage"))
	mount := filepath.Join(base, "stick")
	testfixture.Export(t, mount)
	return mount
}

func dbDir(mount string) string { return filepath.Join(mount, "Engine Library", "Database2") }

func mountNow(t *testing.T, mount string, o MountOptions) MountResult {
	t.Helper()
	res, err := ConvertAtMount(context.Background(), mount, o)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	return res
}

func query(t *testing.T, mount, q string, args ...any) [][]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		for i := range vals {
			vals[i] = new(sql.NullString)
		}
		if err = rows.Scan(vals...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i := range vals {
			row[i] = vals[i].(*sql.NullString).String
		}
		out = append(out, row)
	}
	return out
}

// libraryTree is the library's lists as the deck shows them: "Folder/Nested/name [track file names]",
// following Engine's sibling chain, which must be whole.
func libraryTree(t *testing.T, mount string) []string {
	t.Helper()
	type list struct {
		id, parent, next int64
		title            string
	}
	var lists []list
	for _, r := range query(t, mount, `SELECT id,COALESCE(parentListId,0),COALESCE(nextListId,0),title FROM Playlist`) {
		var l list
		fmt.Sscan(r[0], &l.id)
		fmt.Sscan(r[1], &l.parent)
		fmt.Sscan(r[2], &l.next)
		l.title = r[3]
		lists = append(lists, l)
	}
	var out []string
	var walk func(parent int64, prefix string)
	walk = func(parent int64, prefix string) {
		var siblings []list
		pointed := map[int64]bool{}
		for _, l := range lists {
			if l.parent == parent {
				siblings = append(siblings, l)
				pointed[l.next] = true
			}
		}
		var at *list
		for i := range siblings {
			if !pointed[siblings[i].id] {
				if at != nil {
					t.Fatalf("two first lists under %d: %q and %q", parent, at.title, siblings[i].title)
				}
				at = &siblings[i]
			}
		}
		for n := 0; at != nil; n++ {
			if n > len(siblings) {
				t.Fatalf("the chain under %d does not end", parent)
			}
			line := prefix + at.title
			var names []string
			id := int64(0)
			for {
				rows := query(t, mount, `SELECT e.id,e.nextEntityId,t.filename FROM PlaylistEntity e JOIN Track t ON t.id=e.trackId WHERE e.listId=? AND e.id NOT IN (SELECT nextEntityId FROM PlaylistEntity WHERE listId=?1)`, at.id)
				if id != 0 {
					rows = query(t, mount, `SELECT e.id,e.nextEntityId,t.filename FROM PlaylistEntity e JOIN Track t ON t.id=e.trackId WHERE e.id=?`, id)
				}
				if len(rows) != 1 {
					break
				}
				names = append(names, rows[0][2])
				fmt.Sscan(rows[0][1], &id)
				if id == 0 {
					break
				}
			}
			if len(names) > 0 {
				line += " [" + strings.Join(names, " ") + "]"
			}
			out = append(out, line)
			walk(at.id, line[:len(prefix)+len(at.title)]+"/")
			var next *list
			for i := range siblings {
				if siblings[i].id == at.next {
					next = &siblings[i]
				}
			}
			at = next
		}
		if got := len(siblings); got > 0 {
			seen := 0
			for _, line := range out {
				if strings.HasPrefix(line, prefix) && !strings.Contains(strings.TrimPrefix(strings.SplitN(line, " [", 2)[0], prefix), "/") {
					seen++
				}
			}
			if seen != got {
				t.Fatalf("%d lists under %q, the chain reaches %d", got, prefix, seen)
			}
		}
	}
	walk(0, "")
	return out
}

func trackIDs(t *testing.T, mount string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, r := range query(t, mount, `SELECT path,id FROM Track`) {
		ids[r[0]] = r[1]
	}
	return ids
}

func snapshot(t *testing.T, mount string) *rbstate.Snapshot {
	t.Helper()
	s, err := rbstate.Load(dbDir(mount))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// stageAll plays the background job: every track of the snapshot without applied data gets a
// staged record built from the fixture's analysis file.
// stagedCover is the cover stageAll gives every track: an album's tracks share one image.
var stagedCover = []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}

func stageAll(t *testing.T, mount string) int {
	t.Helper()
	s := snapshot(t, mount)
	r, files := mustSources(t, mount)
	libs, _, err := ReadSources(context.Background(), r, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := BuildView(append(libs, readStaged(mount)...), s.Layout, "")
	if err != nil {
		t.Fatal(err)
	}
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	a, err := rekordbox.ParseAnalysis(testfixture.DAT())
	if err != nil {
		t.Fatal(err)
	}
	for _, more := range [][]byte{testfixture.EXT(), testfixture.EX2()} {
		b, err := rekordbox.ParseAnalysis(more)
		if err != nil {
			t.Fatal(err)
		}
		a.Merge(b)
	}
	n := 0
	for _, vt := range view.Tracks {
		st := s.Tracks[vt.Key]
		if st == nil || st.Native || (st.Perf != rbstate.PerfNone && st.Built.Row == vt.Hash) {
			continue
		}
		tr := vt.Track
		tr.SampleRate, tr.Duration = 44100, 120
		p, err := enginedb.EncodePerformance(tr, a)
		if err != nil {
			t.Fatal(err)
		}
		rec := &rbstate.Staged{Key: vt.Key, Rel: vt.Rel, Sig: rbstate.TrackSig{Rules: rbstate.Rules, Row: vt.Hash, Audio: rbstate.FileSig{Rel: vt.Rel, Size: 1},
			ANLZ: []rbstate.FileSig{{Rel: "PIONEER/USBANLZ/P001/ANLZ0000.DAT", Size: 1}, {Rel: "PIONEER/USBANLZ/P001/ANLZ0000.EXT", Size: 1}}},
			Facts: rbstate.Facts{Duration: 120, SampleRate: 44100},
			Perf:  rbstate.Perf{TrackData: p.TrackData, Overview: p.Overview, Beats: p.Beats, QuickCues: p.QuickCues, Loops: p.Loops, ActiveLoops: p.ActiveLoops, Complete: p.Complete},
			Art:   stagedCover, Digest: enginedb.WrittenDigest(p.Beats, p.QuickCues, p.Loops)}
		if err = stage.Put(rec); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if err = stage.MarkVerified(rbstate.Stamp(s.Layout, files, stagedStamp(mount))); err != nil {
		t.Fatal(err)
	}
	return n
}

func stageWithoutMemoryCue(t *testing.T, mount, key string) {
	t.Helper()
	s := snapshot(t, mount)
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	rec, err := stage.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	rec.Perf.QuickCues = recodeCues(t, rec.Perf.QuickCues, func(raw []byte) {
		binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(-1))
		raw[len(raw)-9] = 0
		binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(-1))
	})
	rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
	if err = stage.Put(rec); err != nil {
		t.Fatal(err)
	}
}

func mustSources(t *testing.T, mount string) (*resolverT, []rbstate.FileSig) {
	t.Helper()
	r, err := newResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := FindSources(r, mount)
	if err != nil {
		t.Fatal(err)
	}
	return r, files
}

// captureLog returns what the window logs.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

var fixtureTree = []string{"Folder", "Folder/Nested", "Folder/Nested/同名 [" + testfixture.UnicodeName + " two.flac]", "Other", "Other/同名 [three.wav four.wav]"}

// The first insert of a never-seen export: rows and lists from the export alone, the snapshot
// beside m.db, the background job asked for, and everything else pending.
func TestMountFirstInsert(t *testing.T) {
	mount := stick(t)
	out := captureLog(t)
	started := 0
	res := mountNow(t, mount, MountOptions{StartBackground: func() error { started++; return nil }})
	if res.Added != 5 || !res.PlaylistsChanged || res.Bypassed || !res.Background || started != 1 {
		t.Fatalf("result %+v, job started %d times", res, started)
	}
	if got := libraryTree(t, mount); !reflect.DeepEqual(got, fixtureTree) {
		t.Fatalf("lists:\n%s", strings.Join(got, "\n"))
	}
	if n := query(t, mount, `SELECT count(*) FROM Track WHERE path LIKE '../MUSIC/%' AND isAvailable=1 AND originTrackId=id AND originDatabaseUuid=(SELECT uuid FROM Information)`); n[0][0] != "5" {
		t.Fatalf("%s of 5 rows are as Engine expects them", n[0][0])
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData`); n[0][0] != "5" {
		t.Fatalf("%s PerformanceData rows", n[0][0])
	}
	if n := query(t, mount, `SELECT count(*) FROM sqlite_master WHERE type='index' AND tbl_name='Track'`); n[0][0] == "0" {
		t.Fatal("Engine's indexes were not rebuilt after the build")
	}
	s := snapshot(t, mount)
	ids := trackIDs(t, mount)
	if len(s.Tracks) != 5 || len(s.Lists) != 5 || s.Pending.Perf != 5 || !s.Pending.Verify || s.Pending.Rows != 0 || s.Pending.Lists {
		t.Fatalf("snapshot: %d tracks, %d lists, pending %+v", len(s.Tracks), len(s.Lists), s.Pending)
	}
	for _, tr := range s.Tracks {
		if ids["../"+tr.Rel] != fmt.Sprint(tr.EngineID) || tr.Perf != rbstate.PerfNone || len(tr.Refs) != 1 {
			t.Fatalf("snapshot track %+v, row id %s", tr, ids["../"+tr.Rel])
		}
	}
	for _, step := range []string{"mount stat ", "mount read ", "mount view ", "mount build ", "mount install ", "mount done in "} {
		if !strings.Contains(out.String(), "rb-bridge: "+step) {
			t.Fatalf("no timing line for %q in:\n%s", step, out)
		}
	}
	if entries, _ := os.ReadDir(dbDir(mount)); len(entries) != 2 {
		t.Fatalf("Database2 holds %v, want m.db and the snapshot", entries)
	}
}

// An export that has not changed since a complete import costs a stat and one line: the
// exports are not opened (their bytes are garbage here, with the same size and time).
func TestMountUnchangedIsStatOnly(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	res := mountNow(t, mount, MountOptions{Publish: true})
	if res.Applied != 5 || !res.Pending.None() {
		t.Fatalf("publish window: %+v", res)
	}
	pdb := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	st, _ := os.Stat(pdb)
	if err := os.WriteFile(pdb, bytes.Repeat([]byte{0xff}, int(st.Size())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pdb, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	out := captureLog(t)
	started := 0
	res = mountNow(t, mount, MountOptions{StartBackground: func() error { started++; return nil }})
	if !res.Bypassed || res.Background || started != 0 {
		t.Fatalf("unchanged insert: %+v, job started %d times", res, started)
	}
	if strings.Contains(out.String(), "mount read") {
		t.Fatalf("the exports were read:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(dbDir(mount), "m.db")); !bytes.Equal(before, after) {
		t.Fatal("m.db changed on an unchanged insert")
	}
}

// A re-export that only changes one list writes that list and nothing else: every track keeps
// its row, the other lists keep their rows and entries, and the new list is there at the mount.
func TestMountPlaylistOnlyReexportWritesOnlyThatList(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	ids := trackIDs(t, mount)
	edits := query(t, mount, `SELECT id,lastEditTime FROM Track ORDER BY id`)
	other := query(t, mount, `SELECT e.id,e.trackId,e.nextEntityId FROM PlaylistEntity e JOIN Playlist p ON p.id=e.listId WHERE p.parentListId=(SELECT id FROM Playlist WHERE title='Other') ORDER BY e.id`)
	lists := query(t, mount, `SELECT id,title FROM Playlist WHERE title IN ('Folder','Nested','Other') ORDER BY id`)

	names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", testfixture.PDBNamed(names, "New Set"))
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
	out := captureLog(t)
	started := 0
	res := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(17 * time.Second), StartBackground: func() error { started++; return nil }})
	if res.Added != 0 || res.Updated != 0 || res.Removed != 0 || !res.PlaylistsChanged || started != 1 {
		t.Fatalf("result %+v, job started %d times", res, started)
	}
	want := append([]string(nil), fixtureTree...)
	want[2] = "Folder/Nested/New Set [" + testfixture.UnicodeName + " two.flac]"
	if got := libraryTree(t, mount); !reflect.DeepEqual(got, want) {
		t.Fatalf("lists:\n%s", strings.Join(got, "\n"))
	}
	if !reflect.DeepEqual(trackIDs(t, mount), ids) {
		t.Fatal("track rows moved")
	}
	if got := query(t, mount, `SELECT id,lastEditTime FROM Track ORDER BY id`); !reflect.DeepEqual(got, edits) {
		t.Fatal("track rows were written")
	}
	if got := query(t, mount, `SELECT e.id,e.trackId,e.nextEntityId FROM PlaylistEntity e JOIN Playlist p ON p.id=e.listId WHERE p.parentListId=(SELECT id FROM Playlist WHERE title='Other') ORDER BY e.id`); !reflect.DeepEqual(got, other) {
		t.Fatal("the entries of an unchanged list were written again")
	}
	if got := query(t, mount, `SELECT id,title FROM Playlist WHERE title IN ('Folder','Nested','Other') ORDER BY id`); !reflect.DeepEqual(got, lists) {
		t.Fatal("unchanged lists got new rows")
	}
	if !strings.Contains(out.String(), "mount diff") || !strings.Contains(out.String(), "+0 ~0 >0 -0 tracks, 1 lists to write, 1 to remove") {
		t.Fatalf("the plan was not one list:\n%s", out)
	}
	// The export changed, so the job has to compare the tracks' files again: write-back waits for that.
	if s := snapshot(t, mount); !s.Pending.Verify || s.Pending.Perf != 0 || s.Pending.Lists {
		t.Fatalf("pending after the re-export: %+v", s.Pending)
	}
}

// Engine's trigger that marks a new list's folders as persisted walks every list of the library
// for each insert: the 506 lists of a real stick's three folders took 12 s at deck speed and the
// first insert was killed (replica, 2026-11-24). The bridge's folders are persisted when they are
// made, so a window suspends the trigger while it adds lists and leaves it as Engine made it.
func TestMountAddsListsWithoutEnginesWalkOfEveryList(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	const name = "trigger_after_insert_isPersist"
	made := query(t, mount, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name)
	if len(made) != 1 {
		t.Fatalf("the first build left %d triggers named %s", len(made), name)
	}
	// The same trigger with a probe: it leaves a row each time it fires.
	probed := strings.Replace(made[0][0], "BEGIN", "BEGIN\n\tINSERT INTO PersistProbe VALUES(new.id);", 1)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`CREATE TABLE PersistProbe(id INTEGER)`, `DROP TRIGGER ` + name, probed} {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", testfixture.PDBNamed(names, "New Set"))
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
	if res := mountNow(t, mount, MountOptions{}); !res.PlaylistsChanged {
		t.Fatalf("result %+v", res)
	}
	if n := query(t, mount, `SELECT count(*) FROM PersistProbe`); n[0][0] != "0" {
		t.Fatalf("Engine's trigger walked the library's lists %s times", n[0][0])
	}
	if got := query(t, mount, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name); len(got) != 1 || got[0][0] != probed {
		t.Fatalf("the trigger is not as the window found it: %q", got)
	}
	if n := query(t, mount, `SELECT count(*) FROM Playlist WHERE isPersisted IS NOT 1`); n[0][0] != "0" {
		t.Fatalf("%s lists are not persisted", n[0][0])
	}
}

// The keys derived for OneLibrary exports are kept in the deck's data directory, so a power-on
// does not cost the first insert a derivation again (5.5 s at deck speed, replica 2026-11-24).
// Where there is no such directory, and in every test, pkg/rekordbox's own choice stands.
func TestExportKeysAreKeptInTheDecksDataDirectory(t *testing.T) {
	data := t.TempDir()
	if got := persistentKeyDir(data); got != filepath.Join(data, "rb-keys") {
		t.Fatalf("key directory %q", got)
	}
	if got := persistentKeyDir(filepath.Join(data, "missing")); got != "" {
		t.Fatalf("a deck without its data directory got %q", got)
	}
	old := keyDataDir
	keyDataDir = data
	defer func() { keyDataDir = old }()
	t.Setenv(keyCacheEnv, "")
	if newKeyCache() == nil || os.Getenv(keyCacheEnv) != "" {
		t.Fatalf("a test was pointed at the data directory: %q", os.Getenv(keyCacheEnv))
	}
}

// The background job is asked for before the exports are read: an export that cannot be read,
// or a window that is killed while reading it, still leaves the job started.
func TestMountStartsTheJobBeforeReadingTheExports(t *testing.T) {
	mount := stick(t)
	pdb := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	if err := os.WriteFile(pdb, []byte("not a pdb"), 0o644); err != nil {
		t.Fatal(err)
	}
	started := 0
	_, err := ConvertAtMount(context.Background(), mount, MountOptions{StartBackground: func() error { started++; return nil }})
	if err == nil || started != 1 {
		t.Fatalf("err %v, job started %d times", err, started)
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), "m.db")); err == nil {
		t.Fatal("a library was made from an unreadable export")
	}
}

// A window that has no time left for new rows leaves them pending and still commits what it
// did; the next window finishes, and the lists then hold every track.
func TestMountOutOfTimeLeavesRowsPendingAndFinishesLater(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	testfixture.LargeExport(t, mount, 250)
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
	// The hook's kill is closer than the reserve: nothing optional, no new rows.
	res := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(commitReserve + time.Second)})
	if res.Added != 0 || res.Pending.Rows == 0 || !res.Pending.Lists {
		t.Fatalf("short window: %+v", res)
	}
	waiting := res.Pending.Rows
	res = mountNow(t, mount, MountOptions{})
	if res.Added+res.Updated != waiting || res.Added == 0 || res.Pending.Rows != 0 || res.Pending.Lists {
		t.Fatalf("next window: %+v, %d rows were waiting", res, waiting)
	}
	if n := query(t, mount, `SELECT count(*) FROM Track`); n[0][0] != "250" {
		t.Fatalf("%s rows, want the export's 250", n[0][0])
	}
	if n := query(t, mount, `SELECT count(DISTINCT trackId) FROM PlaylistEntity`); n[0][0] == "0" {
		t.Fatal("the lists have no entries after their tracks got rows")
	}
}

// A row that holds the export's fields already is not written, and what write-back noted of it
// stands (replica, 2026-11-25: the take-over of KDATA's 5,940 rows wrote every one of them and
// read its cues, 31 ms a row on the cold stick; three windows of 14 s left 4,620 rows pending,
// nothing staged was applied and cue write-back stayed locked out).
func TestMountLeavesRowsThatHoldTheExportsFieldsUnwritten(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 250)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	exec := func(q string, args ...any) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err = db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE zzRowWrites(id INTEGER)`)
	exec(`CREATE TRIGGER zzRowWrite AFTER UPDATE ON Track BEGIN INSERT INTO zzRowWrites VALUES(new.id); END`)
	asAfterTakeOver := func() *rbstate.Snapshot {
		t.Helper()
		s := snapshot(t, mount)
		for _, tr := range s.Tracks {
			tr.Row = ""
		}
		if err := s.Save(dbDir(mount)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := asAfterTakeOver()
	res := mountNow(t, mount, MountOptions{Full: true})
	if res.Updated != 250 || res.Pending.Rows != 0 {
		t.Fatalf("window: %+v", res)
	}
	if n := query(t, mount, `SELECT count(*) FROM zzRowWrites`)[0][0]; n != "0" {
		t.Fatalf("%s rows written although every row held the export's fields", n)
	}
	after := snapshot(t, mount)
	for key, tr := range before.Tracks {
		got := after.Tracks[key]
		if got.Row == "" || got.Edit != tr.Edit || !bytes.Equal(got.Synced, tr.Synced) || (tr.Synced == nil) != (got.Synced == nil) {
			t.Fatalf("%s: row %q, noted %d/%x, was %d/%x", key, got.Row, got.Edit, got.Synced, tr.Edit, tr.Synced)
		}
	}
	// One row holds another title: that row alone is written.
	var one *rbstate.Track
	for _, tr := range after.Tracks {
		if !tr.Native && tr.EngineID != 0 {
			one = tr
			break
		}
	}
	exec(`DROP TRIGGER zzRowWrite`)
	exec(`UPDATE Track SET title='not the export''s' WHERE id=?`, one.EngineID)
	exec(`CREATE TRIGGER zzRowWrite AFTER UPDATE ON Track BEGIN INSERT INTO zzRowWrites VALUES(new.id); END`)
	asAfterTakeOver()
	if res = mountNow(t, mount, MountOptions{Full: true}); res.Updated != 250 || res.Pending.Rows != 0 {
		t.Fatalf("window after one row changed: %+v", res)
	}
	// Engine's own trigger dates the row, which is a second write of it.
	if n := query(t, mount, `SELECT count(DISTINCT id), min(id) FROM zzRowWrites`)[0]; n[0] != "1" || n[1] != fmt.Sprint(one.EngineID) {
		t.Fatalf("rows written: %v, want the one row %d", n, one.EngineID)
	}
	if n := query(t, mount, `SELECT count(*) FROM Track WHERE title='not the export''s'`)[0][0]; n != "0" {
		t.Fatal("the row kept a title the export does not have")
	}
}

// Rows whose export fields changed are written after the rows and lists are committed, a
// batch a transaction: a window that has no time for them commits everything else and leaves
// them pending, and the next window does them (replica, 2026-11-25: the take-over of an older
// library updates every row, which in one transaction with the lists was past the hook's kill).
func TestMountUpdatesRowsInBatchesAfterTheLists(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 250)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	// As after a take-over: no row is known to hold the export's fields.
	s := snapshot(t, mount)
	for _, tr := range s.Tracks {
		tr.Row = ""
	}
	if err := s.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}
	old := updateBatch
	updateBatch = 100
	defer func() { updateBatch = old }()
	counter := func() string {
		return query(t, mount, `SELECT lastRekordBoxLibraryImportReadCounter FROM Information`)[0][0]
	}
	before := counter()
	res := mountNow(t, mount, MountOptions{Full: true, Deadline: time.Now().Add(commitReserve + applyReserve/2)})
	if res.Updated != 0 || res.Pending.Rows != 250 || counter() != before {
		t.Fatalf("window without time: %+v, commit counter %s to %s", res, before, counter())
	}
	res = mountNow(t, mount, MountOptions{})
	if res.Updated != 250 || res.Pending.Rows != 0 {
		t.Fatalf("next window: %+v", res)
	}
	if n, _ := strconv.Atoi(counter()); fmt.Sprint(n-3) != before {
		t.Fatalf("250 rows in batches of 100 took %s to %d, want 3 transactions", before, n)
	}
	if again := mountNow(t, mount, MountOptions{}); !again.Bypassed {
		t.Fatalf("after the updates: %+v", again)
	}
}

// A stick without room for a second copy of its library gets no next library. Row updates one
// window cannot write (a take-over: every row) are then written in place, the batches that fit a
// window: left to the job, as on a stick with room, nothing would ever write them.
func TestRowUpdatesGoOnInPlaceOnAStickWithoutRoomForANextLibrary(t *testing.T) {
	mount := t.TempDir()
	t.Setenv(rbstate.StageEnv, filepath.Join(t.TempDir(), "stage"))
	testfixture.LargeExport(t, mount, 250)
	mountNow(t, mount, MountOptions{})
	unknown := func() { // as after a take-over: no row is known to hold the export's fields
		s := snapshot(t, mount)
		for _, tr := range s.Tracks {
			tr.Row = ""
		}
		if err := s.Save(dbDir(mount)); err != nil {
			t.Fatal(err)
		}
	}
	unknown()
	oldBatch, oldSpare, oldFree := updateBatch, spare, stickFree
	updateBatch, spare = 50, hookWindow-1700*time.Millisecond // 250 rows do not fit a window, 50 do
	defer func() { updateBatch, spare, stickFree = oldBatch, oldSpare, oldFree }()
	window := func() (MountResult, string) {
		out := captureLog(t)
		res := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(hookWindow), StartBackground: func() error { return nil }})
		return res, out.String()
	}
	// With room the rows are the job's: its next library takes them all.
	if res, out := window(); res.Updated != 0 || res.Pending.Rows != 250 || !strings.Contains(out, "leaves the rows' export fields to the job") {
		t.Fatalf("with room for a next library: %+v\n%s", res, out)
	}
	stickFree = func(string) (int64, bool) { return 1 << 20, true }
	if err := (&nextBuild{ctx: context.Background(), mount: mount, dir: dbDir(mount)}).mirror(filepath.Join(dbDir(mount), "m.db"), filepath.Join(dbDir(mount), nextDB), snapshot(t, mount).LibraryUUID, "", 0); !errors.Is(err, errNoNext) {
		t.Fatalf("the job on a stick with 1 MiB free: %v, want no next library", err)
	}
	left := 250
	for i := 0; left > 0; i++ {
		res, out := window()
		if res.Updated == 0 || res.Updated%50 != 0 && res.Updated != left || res.Pending.Rows != left-res.Updated || i > 6 {
			t.Fatalf("window %d on the stick without room: %d rows were waiting, %+v\n%s", i, left, res, out)
		}
		if res.Updated < left && !strings.Contains(out, fmt.Sprintf("%d of %d rows get the export's fields in this window", res.Updated, left)) {
			t.Fatalf("window %d does not say what it did:\n%s", i, out)
		}
		left -= res.Updated
	}
	if res, out := window(); res.Updated != 0 || res.Pending.Rows != 0 || strings.Contains(out, "mount update") {
		t.Fatalf("after the updates: %+v\n%s", res, out)
	}
}

// Staged data is written once; what the deck changes afterwards stays when the export changes
// the track and its data is staged again; a row nobody touched is replaced.
func TestMountStagedDataAndDeckEdits(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	stageWithoutMemoryCue(t, mount, "music/three.wav")
	res := mountNow(t, mount, MountOptions{Publish: true})
	if res.Applied != 5 {
		t.Fatalf("applied %d", res.Applied)
	}
	if n := query(t, mount, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE p.beatData IS NOT NULL AND p.trackData IS NOT NULL AND p.overviewWaveFormData IS NOT NULL AND p.thirdPartySourceId IS NULL AND t.isAnalyzed=1`); n[0][0] != "5" {
		t.Fatalf("%s of 5 rows hold the staged data", n[0][0])
	}
	s := snapshot(t, mount)
	for _, tr := range s.Tracks {
		if tr.Perf != rbstate.PerfApplied || tr.Written == nil || !tr.Resolved || len(enginedb.DetailFiles(tr.Built.ANLZ)) != 1 {
			t.Fatalf("snapshot after the publish: %+v", tr)
		}
	}
	// The covers came with the staged tracks: one AlbumArt row with Engine's 20-byte hash for the
	// image they share, every row pointing at it, and the file Engine serves to other decks.
	art := query(t, mount, `SELECT count(*),min(length(hash)),(SELECT count(*) FROM Track WHERE albumArtId IN (SELECT id FROM AlbumArt WHERE albumArt IS NOT NULL)) FROM AlbumArt WHERE albumArt IS NOT NULL`)
	if art[0][0] != "1" || art[0][1] != "20" || art[0][2] != "5" {
		t.Fatalf("covers: %v", art)
	}
	sum := sha1.Sum(stagedCover)
	if b, err := os.ReadFile(filepath.Join(mount, "Engine Library", "Artwork", base64.RawURLEncoding.EncodeToString(sum[:])+".jpg")); err != nil || !bytes.Equal(b, stagedCover) {
		t.Fatalf("the cover file for other decks: %v", err)
	}
	// The deck sets a hot cue on one track (Engine writes the row), then the export changes two titles.
	edited := s.Tracks["music/two.flac"]
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET quickCues=X'00010203' WHERE trackId=?`, edited.EngineID); err != nil {
		t.Fatal(err)
	}
	// A deck only loads another: Engine 5.3.0 writes its own figure into the main cue and the main
	// cue as imported of a track whose main cue is not adjusted (measured: both -1105.0, flag 0).
	loaded := s.Tracks["music/three.wav"]
	var cues []byte
	if err = db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, loaded.EngineID).Scan(&cues); err != nil {
		t.Fatal(err)
	}
	cues = recodeCues(t, cues, func(raw []byte) {
		if raw[len(raw)-9] != 0 || math.Float64frombits(binary.BigEndian.Uint64(raw[len(raw)-17:])) == -1105 {
			t.Fatalf("the bridge's main cue: % x", raw[len(raw)-17:])
		}
		binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(-1105))
		binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(-1105))
	})
	if _, err = db.Exec(`UPDATE PerformanceData SET quickCues=? WHERE trackId=?`, cues, loaded.EngineID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
	pdb := testfixture.PDBNamed(names, "同名")
	// Titles are UTF-16 in the fixture: change one code unit of two track titles, lengths kept.
	for _, name := range []string{"two", "three"} {
		u := utf16le(name)
		i := bytes.Index(pdb, u)
		if i < 0 {
			t.Fatalf("title %q not found in the fixture", name)
		}
		pdb[i] = 'X'
	}
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
	res = mountNow(t, mount, MountOptions{})
	if res.Updated != 2 || res.Added != 0 {
		t.Fatalf("re-export of two titles: %+v", res)
	}
	if n := stageAll(t, mount); n != 2 {
		t.Fatalf("%d tracks to stage again, want the 2 whose row changed", n)
	}
	stageWithoutMemoryCue(t, mount, "music/three.wav")
	res = mountNow(t, mount, MountOptions{Publish: true})
	if res.Applied != 2 {
		t.Fatalf("applied %d", res.Applied)
	}
	if got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, edited.EngineID); got[0][0] != "00010203" {
		t.Fatalf("the deck's cue was replaced: %s", got[0][0])
	}
	s = snapshot(t, mount)
	if s.Tracks["music/two.flac"].Perf != rbstate.PerfDeck || s.Tracks["music/two.flac"].Written != nil || s.Tracks["music/three.wav"].Perf != rbstate.PerfApplied {
		t.Fatalf("after the second publish: %+v %+v", s.Tracks["music/two.flac"], s.Tracks["music/three.wav"])
	}
	// The load alone was no edit: the row is the bridge's still. Its cues are the staged ones but for
	// Engine's own figure, so nothing was written (Engine would put it back at the next load).
	if got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, loaded.EngineID); s.Tracks["music/three.wav"].Written == nil || got[0][0] != fmt.Sprintf("%X", cues) {
		t.Fatalf("the row a deck only loaded: written %x, cues changed %v", s.Tracks["music/three.wav"].Written, got[0][0] != fmt.Sprintf("%X", cues))
	}
}

// A re-export that moves a track's main cue (rekordbox's earliest memory cue) and nothing else
// reaches the row the bridge wrote when no deck loaded the track since. Reading a row's main cue
// as the staged record has it is only for a row that holds neither what the bridge wrote nor what
// is staged: here the row is exactly what the bridge wrote, and it is replaced.
func TestMountWritesAMainCueTheExportMoved(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 5 {
		t.Fatalf("applied %d", res.Applied)
	}
	const key = "music/three.wav"
	s := snapshot(t, mount)
	wrote := s.Tracks[key].Written
	s.Tracks[key].Built.Row = "" // staged again, as after a re-export
	s.Pending.Perf = 1
	if err := s.Save(dbDir(mount)); err != nil {
		t.Fatal(err)
	}
	if n := stageAll(t, mount); n != 1 {
		t.Fatalf("%d tracks staged again, want 1", n)
	}
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := stage.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	rec.Perf.QuickCues = recodeCues(t, rec.Perf.QuickCues, func(raw []byte) {
		moved := math.Float64bits(math.Float64frombits(binary.BigEndian.Uint64(raw[len(raw)-17:])) + 4410)
		binary.BigEndian.PutUint64(raw[len(raw)-17:], moved)
		binary.BigEndian.PutUint64(raw[len(raw)-8:], moved)
	})
	rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
	if err = stage.Put(rec); err != nil {
		t.Fatal(err)
	}
	if err = stage.Close(); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 1 {
		t.Fatalf("applied %d, want the one track staged again", res.Applied)
	}
	after := snapshot(t, mount).Tracks[key]
	got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, after.EngineID)
	if got[0][0] != fmt.Sprintf("%X", rec.Perf.QuickCues) {
		t.Fatal("the row still holds the main cue of the export before")
	}
	if after.Perf != rbstate.PerfApplied || !bytes.Equal(after.Written, rec.Digest) || bytes.Equal(wrote, rec.Digest) {
		t.Fatalf("the snapshot's record of the row: %+v", after)
	}
}

func utf16le(s string) []byte {
	var b []byte
	for _, r := range s {
		b = append(b, byte(r), 0)
	}
	return b
}

// state is everything of a stick's library that the DJ and the next window see.
func state(t *testing.T, mount string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(strings.Join(libraryTree(t, mount), "\n"))
	for _, r := range query(t, mount, `SELECT t.path,t.title,p.beatData IS NOT NULL,hex(substr(p.quickCues,1,8)) FROM Track t JOIN PerformanceData p ON p.trackId=t.id ORDER BY t.path`) {
		b.WriteString("\n" + strings.Join(r, "|"))
	}
	s := snapshot(t, mount)
	var keys []string
	for k, tr := range s.Tracks {
		keys = append(keys, fmt.Sprintf("%s perf=%d row=%s", k, tr.Perf, tr.Row))
	}
	sort.Strings(keys)
	fmt.Fprintf(&b, "\n%s\npending %+v stamp %s", strings.Join(keys, "\n"), s.Pending, s.Stamp)
	return b.String()
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		if err = os.WriteFile(filepath.Join(dst, rel), b, 0o644); err != nil {
			return err
		}
		return os.Chtimes(filepath.Join(dst, rel), info.ModTime(), info.ModTime())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A power cut or the hook's kill at any point between two writes of a window: the next window
// on what was on the stick at that moment ends in the same library as an undisturbed one, with
// every track once.
func TestMountCutAtEveryStepEndsInTheSameLibrary(t *testing.T) {
	scenarios := map[string]func(t *testing.T, mount string){
		"first insert": func(t *testing.T, mount string) {},
		"re-export with a new list and two new titles": func(t *testing.T, mount string) {
			mountNow(t, mount, MountOptions{})
			names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
			pdb := testfixture.PDBNamed(names, "New Set")
			pdb[bytes.Index(pdb, utf16le("four"))] = 'X'
			testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
			later := time.Now().Add(time.Hour)
			os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), later, later)
		},
		"staged data": func(t *testing.T, mount string) {
			mountNow(t, mount, MountOptions{})
			stageAll(t, mount)
		},
	}
	for name, prepare := range scenarios {
		t.Run(name, func(t *testing.T) {
			mount := stick(t)
			prepare(t, mount)
			base := filepath.Dir(mount)
			stageDir := os.Getenv(rbstate.StageEnv)
			var cuts []string
			cut = func(step string) {
				at := filepath.Join(base, fmt.Sprintf("cut-%d", len(cuts)))
				copyTree(t, mount, filepath.Join(at, "stick"))
				if _, err := os.Stat(stageDir); err == nil {
					copyTree(t, stageDir, filepath.Join(at, "stage"))
				}
				cuts = append(cuts, step)
			}
			mountNow(t, mount, MountOptions{Publish: true})
			cut = func(string) {}
			mountNow(t, mount, MountOptions{Publish: true}) // what an undisturbed second window leaves
			want := state(t, mount)
			if len(cuts) < 3 {
				t.Fatalf("only %d cut points: %v", len(cuts), cuts)
			}
			for i, step := range cuts {
				at := filepath.Join(base, fmt.Sprintf("cut-%d", i))
				t.Setenv(rbstate.StageEnv, filepath.Join(at, "stage"))
				copy := filepath.Join(at, "stick")
				mountNow(t, copy, MountOptions{Publish: true})
				mountNow(t, copy, MountOptions{Publish: true})
				if got := state(t, copy); got != want {
					t.Errorf("cut %d (%s) ends in another library:\n%s\nwant:\n%s", i, step, got, want)
				}
				if dup := query(t, copy, `SELECT count(*) FROM (SELECT path FROM Track GROUP BY path HAVING count(*)>1)`); dup[0][0] != "0" {
					t.Errorf("cut %d (%s): a track has two rows", i, step)
				}
				if _, err := os.Stat(filepath.Join(dbDir(copy), rbstate.SnapshotName+".next")); err == nil {
					t.Errorf("cut %d (%s): a next snapshot is left", i, step)
				}
			}
			t.Setenv(rbstate.StageEnv, stageDir)
		})
	}
}

// A re-export that only renumbers the export's own IDs writes nothing to m.db, and the snapshot
// then names every track by its new ID: write-back finds a track's rows in the libraries by them.
func TestMountRenumberedReexportWritesNoRowAndRefreshesRefs(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	before, _ := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
	old := snapshot(t, mount)

	// The same five files under other IDs, in the fixture's second layout (LargePDB numbers from 1
	// too, so the IDs are swapped by hand: the PDB stores them little-endian after the row header).
	r, files := mustSources(t, mount)
	libs, _, err := ReadSources(context.Background(), r, mount, files, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range libs[0].Lib.Tracks {
		libs[0].Lib.Tracks[i].ID += 5000
	}
	for i := range libs[0].Lib.Playlists {
		for j := range libs[0].Lib.Playlists[i].Tracks {
			libs[0].Lib.Playlists[i].Tracks[j] += 5000
		}
	}
	view, err := BuildView(libs, old.Layout, "")
	if err != nil {
		t.Fatal(err)
	}
	view.Stamp, view.Sources = old.Stamp+" renumbered", files
	w := &window{ctx: context.Background(), mount: mount, dir: dbDir(mount), view: view, st: newSteps(MountOptions{Full: true}),
		load: func() (*rbstate.Snapshot, error) { return rbstate.Load(dbDir(mount)) }, res: &MountResult{}}
	defer w.close()
	if err = w.open(); err != nil {
		t.Fatal(err)
	}
	p := Diff(view, w.snap)
	if len(p.AddTracks)+len(p.UpdateTracks)+len(p.Moves)+len(p.RemoveTracks)+len(p.PutLists)+len(p.RemoveLists) != 0 {
		t.Fatalf("a renumbering re-export has a plan: %+v", p)
	}
	w.close()
	if err = w.run(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(dbDir(mount), "m.db")); !bytes.Equal(before, after) {
		t.Fatal("m.db was written for a re-export that changed no row and no list")
	}
	now := snapshot(t, mount)
	for key, tr := range now.Tracks {
		if tr.Refs[0].ID != old.Tracks[key].Refs[0].ID+5000 || tr.EngineID != old.Tracks[key].EngineID {
			t.Fatalf("%s: ref %d (was %d), row %d (was %d)", key, tr.Refs[0].ID, old.Tracks[key].Refs[0].ID, tr.EngineID, old.Tracks[key].EngineID)
		}
	}
	if now.Stamp != view.Stamp || !now.Pending.Verify {
		t.Fatalf("stamp %q, pending %+v", now.Stamp, now.Pending)
	}
}

// A deck loaded staged tracks before their window, and the deck's UI hook wrote the staged data
// into the rows itself (filled.jsonl); Engine then put its own figure into the main cue, as it
// does at every load of a track whose main cue the DJ has not adjusted. Those rows are the
// bridge's own writes: a newer staged record replaces them, and the snapshot holds them as
// applied. What the DJ changed after the fill stays the deck's.
func TestMountTakesRowsTheDeckHookFilledAsTheBridgesOwn(t *testing.T) {
	for _, load := range []struct {
		name    string
		mainAdj byte
		rewrite func(raw []byte) // Engine's write of the main cue's 17 bytes at a load
	}{
		// Real Engine 5.3.0 on JP07 and SC LIVE 4: on an unadjusted row (flag 0), Engine rewrites
		// mainCue and defaultMainCue to -1105.0; on a memory-cue row (flag 1), Engine leaves it untouched.
		{"Engine 5.3.0: both fields", 0, func(raw []byte) {
			binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(-1105))
			binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(-1105))
		}},
		{"the last field only", 0, func(raw []byte) { binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(-1105)) }},
		{"memory cue (adj=1): untouched on load", 1, func(raw []byte) {}},
	} {
		t.Run(load.name, func(t *testing.T) { rowsTheDeckHookFilled(t, load.mainAdj, load.rewrite) })
	}
}

func rowsTheDeckHookFilled(t *testing.T, mainAdj byte, engineLoad func(raw []byte)) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	s := snapshot(t, mount)
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	// fill is the hook's write of a staged record (one with a hot cue) to its row, its line in
	// filled.jsonl, and the load.
	fill := func(key string, journal bool, deck func(raw []byte)) *rbstate.Staged {
		rec, err := stage.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		rec.Perf.QuickCues = recodeCues(t, rec.Perf.QuickCues, func(raw []byte) {
			binary.BigEndian.PutUint64(raw[8+1+int(raw[8]):], math.Float64bits(22050))
			binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(9920)) // rekordbox's main cue, as the bridge writes it: twice
			raw[len(raw)-9] = mainAdj
			binary.BigEndian.PutUint64(raw[len(raw)-8:], math.Float64bits(9920))
		})
		rec.Digest = enginedb.WrittenDigest(rec.Perf.Beats, rec.Perf.QuickCues, rec.Perf.Loops)
		if err = stage.Put(rec); err != nil {
			t.Fatal(err)
		}
		id := s.Tracks[key].EngineID
		cues := recodeCues(t, rec.Perf.QuickCues, func(raw []byte) {
			engineLoad(raw)
			if deck != nil {
				deck(raw)
			}
		})
		if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?,thirdPartySourceId=NULL WHERE trackId=?`,
			rec.Perf.TrackData, rec.Perf.Overview, rec.Perf.Beats, cues, rec.Perf.Loops, id); err != nil {
			t.Fatal(err)
		}
		if journal {
			f, err := os.OpenFile(filepath.Join(stage.Dir(), "filled.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				_, err = fmt.Fprintf(f, `{"id":%d,"k":%q,"wr":%q,"v":%d,"w":%q,"at":1795570000}`+"\n", id, key, base64.StdEncoding.EncodeToString(rec.Digest), rbstate.Rules, rec.Sig.Row)
				f.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		return rec
	}
	// one: filled, then the DJ moved hot cue 1 without touching the main cue: must stay the deck's.
	oneKey := rbstate.FoldPath("Music/" + testfixture.UnicodeName)
	var oneDeckCues []byte
	fill(oneKey, true, func(raw []byte) {
		binary.BigEndian.PutUint64(raw[8+1+int(raw[8]):], math.Float64bits(88200))
	})
	if err = db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, s.Tracks[oneKey].EngineID).Scan(&oneDeckCues); err != nil {
		t.Fatal(err)
	}
	// two: filled, then the job staged it again with other cues (a re-export): replaced.
	two := fill("music/two.flac", true, nil)
	two.Perf.QuickCues = recodeCues(t, two.Perf.QuickCues, func(raw []byte) { raw[8+1+int(raw[8])+8] ^= 0xFF }) // the first hot cue's colour
	two.Digest = enginedb.WrittenDigest(two.Perf.Beats, two.Perf.QuickCues, two.Perf.Loops)
	if err = stage.Put(two); err != nil {
		t.Fatal(err)
	}
	// three: filled, and the hook's line is lost: the load alone is no edit.
	three := fill("music/three.wav", false, nil)
	// four: filled, then the DJ moved the main cue: the deck's.
	fill("music/four.wav", true, func(raw []byte) {
		binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(4410))
		raw[len(raw)-9] = 1
	})
	db.Close()
	if err = stage.Close(); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 5 {
		t.Fatalf("applied %d", res.Applied)
	}
	after := snapshot(t, mount)
	if tr := after.Tracks[oneKey]; tr.Perf != rbstate.PerfDeck || tr.Written != nil {
		t.Fatalf("the filled row whose hot cue the DJ moved: %+v", tr)
	}
	if got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, after.Tracks[oneKey].EngineID); got[0][0] != fmt.Sprintf("%X", oneDeckCues) {
		t.Fatal("the DJ's hot cue on a filled row was overwritten")
	}
	if tr := after.Tracks["music/two.flac"]; tr.Perf != rbstate.PerfApplied || !bytes.Equal(tr.Written, two.Digest) {
		t.Fatalf("the filled row that was staged again: %+v", tr)
	}
	if got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, after.Tracks["music/two.flac"].EngineID); got[0][0] != fmt.Sprintf("%X", two.Perf.QuickCues) {
		t.Fatal("the filled row did not get the newer staged cues")
	}
	if tr := after.Tracks["music/three.wav"]; tr.Perf != rbstate.PerfApplied || !bytes.Equal(tr.Written, three.Digest) {
		t.Fatalf("the filled row a deck loaded: %+v", tr)
	}
	if tr := after.Tracks["music/four.wav"]; tr.Perf != rbstate.PerfDeck || tr.Written != nil {
		t.Fatalf("the filled row whose main cue the DJ moved: %+v", tr)
	}
	if tr := after.Tracks["music/five.wav"]; tr.Perf != rbstate.PerfApplied || tr.Written == nil {
		t.Fatalf("a row nobody filled: %+v", tr)
	}
}

// The upkeep at the end of a window cannot outlast its budget. The grid fix reads every row of a
// library it has not stamped, and its own budget does not stop that statement (KDATA: 6.5 s at
// every insert of the unchanged stick): it is stopped after half a second in such an insert and
// after a second in the window of the job's remount, and being stopped is not an error.
func TestMountUpkeepIsStoppedAtItsBudget(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	var limits []time.Duration
	real := fixMP3Grids
	defer func() { fixMP3Grids = real }()
	fixMP3Grids = func(ctx context.Context, _ string, _ enginedb.MP3GridOptions) (enginedb.MP3GridReport, error) {
		if at, ok := ctx.Deadline(); ok {
			limits = append(limits, time.Until(at))
		}
		<-ctx.Done() // a scan that would not end
		return enginedb.MP3GridReport{Interrupted: true}, ctx.Err()
	}
	out := captureLog(t)
	start := time.Now()
	if res := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(hookWindow)}); !res.Bypassed {
		t.Fatalf("the unchanged stick: %+v", res)
	}
	if took := time.Since(start); len(limits) != 1 || limits[0] > 500*time.Millisecond || took > 900*time.Millisecond {
		t.Fatalf("the insert of the unchanged stick took %s, its grid fix had %v", took, limits)
	}
	if res := mountNow(t, mount, MountOptions{Deadline: time.Now().Add(hookWindow), Publish: true}); !res.Bypassed {
		t.Fatalf("the window of a remount: %+v", res)
	}
	if len(limits) != 2 || limits[1] <= 500*time.Millisecond || limits[1] > time.Second {
		t.Fatalf("the grid fix's time in the two windows: %v", limits)
	}
	if strings.Contains(out.String(), "grid fix not finished") {
		t.Fatalf("a grid fix stopped at its budget is logged as a failure:\n%s", out)
	}
}

// A window does not write, or look for, the list waveform of a track the job staged with its row:
// the job wrote the file then (the stage line names the row). The folder holds a file a track,
// and a look into it costs a window seconds on a big stick.
func TestMountLeavesTheListWaveformToTheJobThatStagedTheRow(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	s := snapshot(t, mount)
	stage, err := rbstate.OpenStage(s.LibraryUUID)
	if err != nil {
		t.Fatal(err)
	}
	rgb := []byte{0, 0, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8}
	for key, tr := range s.Tracks {
		rec, err := stage.Get(key)
		if err != nil {
			t.Fatal(err)
		}
		if rec.RGB = rgb; key != "music/two.flac" {
			rec.EngineID = tr.EngineID // staged by the job when the row was there
		}
		if err = stage.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err = stage.Close(); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Applied != 5 {
		t.Fatalf("applied %d", res.Applied)
	}
	files, _ := filepath.Glob(filepath.Join(dbDir(mount), "OverviewData", s.LibraryUUID, "*.rgb"))
	if want := fmt.Sprintf("%d.rgb", s.Tracks["music/two.flac"].EngineID); len(files) != 1 || filepath.Base(files[0]) != want {
		t.Fatalf("list waveforms the window wrote: %v, want only %s", files, want)
	}
}

// Finding 2.1: ConvertAtMount enforces a deadline context with >= 3 s commit margin, checks the
// deadline inside scaling loops (carryOver fallback, RemoveTracks, MoveTracks, AddTracks, putLists),
// and uses >= 0.33 ms/row for work.scan and a conservative work.adds.
func TestFinding21MountDeadlineAndCostModel(t *testing.T) {
	t.Run("cost_model_scan_and_adds", func(t *testing.T) {
		if got := (work{scan: 13169}).cost(); got < 13169*330*time.Microsecond {
			t.Fatalf("work.scan(13169) = %s, want >= 0.33 ms/row (%s)", got, 13169*330*time.Microsecond)
		}
		if got := (work{adds: 100}).cost(); got < 1500*time.Millisecond {
			t.Fatalf("work.adds(100) = %s, want >= 15 ms/row (1.5s)", got)
		}
		win := &steps{kill: time.Now().Add(hookWindow)}
		if win.fits(work{scan: 40000, commits: 1, tracks: 20000}) {
			t.Fatal("20,000-track deck-row scan (40,000 rows) must not fit a 17 s window")
		}
	})

	t.Run("carryOver_deadline_returns_without_running_rest_of_mount", func(t *testing.T) {
		mount := nextStick(t, 20)
		makeNext(t, mount)
		engineWrites(t, mount, `UPDATE Track SET isPlayed=1,timeLastPlayed=1700000001 WHERE id=1`)

		kill := time.Now().Add(spare + 1500*time.Millisecond)
		oldMirror := mirrorLibrary
		var sawDeadline time.Time
		var hadDeadline bool
		mirrorLibrary = func(ctx context.Context, live, next string, m *enginedb.Mirror, pace func(time.Duration) error) (enginedb.MirrorChange, error) {
			sawDeadline, hadDeadline = ctx.Deadline()
			<-ctx.Done()
			return enginedb.MirrorChange{}, ctx.Err()
		}
		defer func() { mirrorLibrary = oldMirror }()

		out := captureLog(t)
		started := 0
		res, err := ConvertAtMount(context.Background(), mount, MountOptions{
			Publish:         true,
			Deadline:        kill,
			StartBackground: func() error { started++; return nil },
		})
		if err != nil {
			t.Fatalf("ConvertAtMount returned error on deadline handoff: %v", err)
		}
		if !hadDeadline || kill.Sub(sawDeadline) < spare-50*time.Millisecond {
			t.Fatalf("carryOver ctx deadline margin too small: had=%v margin=%s want >= %s", hadDeadline, kill.Sub(sawDeadline), spare)
		}
		if left := time.Until(kill); left < spare-500*time.Millisecond {
			t.Fatalf("ConvertAtMount finished too close to kill: %s left", left)
		}
		if strings.Contains(out.String(), "mount read") || strings.Contains(out.String(), "mount stat") {
			t.Fatalf("ConvertAtMount kept running after carryOver deadline:\n%s", out.String())
		}
		if started != 1 || !res.Background {
			t.Fatalf("background job not started on carryOver deadline handoff: started=%d res=%+v", started, res)
		}
	})

	t.Run("remove_tracks_loop_checks_deadline", func(t *testing.T) {
		mount := stick(t)
		mountNow(t, mount, MountOptions{})
		lib, err := enginedb.OpenLibrary(context.Background(), mount)
		if err != nil {
			t.Fatal(err)
		}
		defer lib.Close()
		snap, err := rbstate.Load(dbDir(mount))
		if err != nil {
			t.Fatal(err)
		}
		// Empty view triggers RemoveTracks for every track; st.owed is already expired while
		// st.kill is zero so fits() passes and only the in-loop deadline check can stop deletes.
		emptyView := &View{Layout: snap.Layout, Stamp: "empty"}
		var res MountResult
		w := &window{
			ctx:   context.Background(),
			mount: mount,
			dir:   dbDir(mount),
			view:  emptyView,
			st:    &steps{start: time.Now(), last: time.Now(), owed: time.Now().Add(-time.Second)},
			res:   &res,
			lib:   lib,
			snap:  snap,
		}
		if w.index, err = lib.TrackIndex(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = w.apply(Diff(emptyView, snap)); err != nil {
			t.Fatalf("w.apply: %v", err)
		}
		if res.Removed != 0 {
			t.Fatalf("RemoveTracks deleted %d tracks past owed deadline, want 0", res.Removed)
		}
	})
}
