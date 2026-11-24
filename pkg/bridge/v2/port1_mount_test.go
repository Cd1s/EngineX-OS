package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	_ "modernc.org/sqlite"
)

func p1Open(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func p1LegacyState(t *testing.T, mount string, malformedAnalysis bool, withWritten bool) *rbstate.Snapshot {
	t.Helper()
	dir := dbDir(mount)
	s := snapshot(t, mount)
	db := p1Open(t, mount)
	type digestRow struct {
		id    int64
		path  string
		beats []byte
		cues  []byte
		loops []byte
	}
	var rows []digestRow
	for _, tr := range s.Tracks {
		var r digestRow
		r.id, r.path = tr.EngineID, "../"+tr.Rel
		if err := db.QueryRow(`SELECT beatData,quickCues,loops FROM PerformanceData WHERE trackId=?`, r.id).Scan(&r.beats, &r.cues, &r.loops); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(dir, oldStateName)
	state, err := sql.Open("sqlite", statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	_, err = state.Exec(`
		CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL);
		CREATE TABLE BridgeAnalysis(trackId INTEGER PRIMARY KEY, sourceJSON BLOB NOT NULL, audioJSON BLOB NOT NULL);
		CREATE TABLE BridgePlaylist(sourceId INTEGER PRIMARY KEY, listId INTEGER NOT NULL UNIQUE);
		CREATE TABLE BridgeLibrary(key TEXT PRIMARY KEY, value BLOB);
		CREATE TABLE BridgeWritten(trackId INTEGER PRIMARY KEY, digest BLOB NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err = state.Exec(`INSERT INTO BridgeState(path,trackId,fingerprint) VALUES(?,?,?)`, r.path, r.id, "legacy"); err != nil {
			t.Fatal(err)
		}
		if withWritten {
			digest := enginedb.WrittenDigest(r.beats, r.cues, r.loops)
			if _, err = state.Exec(`INSERT INTO BridgeWritten(trackId,digest) VALUES(?,?)`, r.id, digest); err != nil {
				t.Fatal(err)
			}
		}
		analysis := []byte(`{"track":1}`)
		if malformedAnalysis {
			analysis = []byte{0}
		}
		if _, err = state.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,?)`, r.id, analysis, analysis); err != nil {
			t.Fatal(err)
		}
	}
	listSourceID := 1
	for _, l := range s.Lists {
		if _, err = state.Exec(`INSERT INTO BridgePlaylist(sourceId,listId) VALUES(?,?)`, listSourceID, l.EngineID); err != nil {
			t.Fatal(err)
		}
		listSourceID++
	}
	if err = os.Remove(filepath.Join(dir, rbstate.SnapshotName)); err != nil {
		t.Fatal(err)
	}
	return s
}

func p1SetDeckEdits(t *testing.T, mount string, ids []int64) map[int64][]byte {
	t.Helper()
	db := p1Open(t, mount)
	if _, err := db.Exec(`UPDATE Track SET dateAdded=1738108800,isPlayed=CASE WHEN id BETWEEN 3 AND 8 THEN 1 ELSE 0 END`); err != nil {
		t.Fatal(err)
	}
	want := make(map[int64][]byte, len(ids))
	for _, id := range ids {
		cue := []byte(fmt.Sprintf("deck-cue-%d", id))
		loop := []byte(fmt.Sprintf("deck-loop-%d", id))
		if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=?,loops=?,activeOnLoadLoops=2 WHERE trackId=?`, cue, loop, id); err != nil {
			t.Fatal(err)
		}
		want[id] = cue
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return want
}

func p1ShiftPDBIDs(b []byte, delta uint32) {
	for page := 0; page+testfixture.PageSize <= len(b); page += testfixture.PageSize {
		typ := binary.LittleEndian.Uint32(b[page+8:])
		if typ != 0 && typ != 8 {
			continue
		}
		count := int(binary.LittleEndian.Uint16(b[page+24:]))
		for rowNo := 0; rowNo < count; rowNo++ {
			footer := page + testfixture.PageSize - (rowNo/16)*36
			off := int(binary.LittleEndian.Uint16(b[footer-6-2*(rowNo%16):]))
			row := page + 40 + off
			if typ == 0 {
				if row+76 > len(b) {
					continue
				}
				id := binary.LittleEndian.Uint32(b[row+72:])
				if id >= 1 && id <= 5 {
					binary.LittleEndian.PutUint32(b[row+72:], id+delta)
				}
			} else if row+8 <= len(b) {
				id := binary.LittleEndian.Uint32(b[row+4:])
				if id >= 1 && id <= 5 {
					binary.LittleEndian.PutUint32(b[row+4:], id+delta)
				}
			}
		}
	}
}

func TestPort1LegacyTakeoverPreservesDeckEdits(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 10)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	old := snapshot(t, mount)
	var ids []int64
	for _, tr := range old.Tracks {
		ids = append(ids, tr.EngineID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// The earlier build recorded what it wrote; the deck's edits came after that.
	p1LegacyState(t, mount, false, true)
	wantCues := p1SetDeckEdits(t, mount, ids[2:])

	res := mountNow(t, mount, MountOptions{})
	if res.Added != 0 || res.Removed != 0 {
		t.Fatalf("takeover changed rows: %+v", res)
	}
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	db := p1Open(t, mount)
	for id, cue := range wantCues {
		var got []byte
		var played, added, active int64
		if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, cue) {
			t.Errorf("track %d cue changed to %q", id, got)
		}
		if err := db.QueryRow(`SELECT isPlayed,dateAdded,activeOnLoadLoops FROM Track JOIN PerformanceData ON PerformanceData.trackId=Track.id WHERE Track.id=?`, id).Scan(&played, &added, &active); err != nil {
			t.Fatal(err)
		}
		wantPlayed := int64(0)
		if id >= 3 && id <= 8 {
			wantPlayed = 1
		}
		if played != wantPlayed || added != 1738108800 || active != 2 {
			t.Errorf("track %d deck fields changed: played=%d added=%d active=%d", id, played, added, active)
		}
	}
	if got := libraryTree(t, mount); len(got) == 0 {
		t.Fatal("playlist tree disappeared during takeover")
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), oldStateName)); !os.IsNotExist(err) {
		t.Fatalf("old state remains after takeover: %v", err)
	}
	if again := mountNow(t, mount, MountOptions{}); !again.Bypassed {
		t.Fatalf("completed takeover is not bypassed: %+v", again)
	}
}

func TestPort1LegacyTakeoverUsesOwnedPathsWithMalformedAnalysis(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 10)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	want := snapshot(t, mount)
	p1LegacyState(t, mount, true, true)
	stage, err := rbstate.OpenStage(want.LibraryUUID)
	if err == nil {
		if err := stage.Remove(); err != nil {
			t.Fatal(err)
		}
	}
	res := mountNow(t, mount, MountOptions{})
	if res.Added != 0 || res.Removed != 0 {
		t.Fatalf("takeover changed rows: %+v", res)
	}
	got := snapshot(t, mount)
	if got.Pending.Verify == false {
		t.Fatalf("takeover did not request verification: %+v", got.Pending)
	}
	for key, before := range want.Tracks {
		after := got.Tracks[key]
		if after == nil || after.EngineID != before.EngineID || after.Rel != before.Rel || after.Perf != rbstate.PerfNone || len(after.Written) == 0 {
			t.Fatalf("track %s takeover: before=%+v after=%+v", key, before, after)
		}
	}
	if _, err := os.Stat(filepath.Join(dbDir(mount), oldStateName)); !os.IsNotExist(err) {
		t.Fatalf("old state remains after snapshot save: %v", err)
	}
}

func TestPort1ReexportKeepsDeckPerformanceFieldsFieldByField(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	s := snapshot(t, mount)
	db := p1Open(t, mount)
	edited := s.Tracks["music/two.flac"]
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=X'4445434B',loops=X'4C4F4F50' WHERE trackId=?`, edited.EngineID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET bpm=166,bpmAnalyzed=166,isBeatGridLocked=1,rating=80 WHERE id=?`, edited.EngineID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "Reexport")
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	when := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), when, when); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	db = p1Open(t, mount)
	var cue, loop []byte
	var bpm, locked, rating int
	if err := db.QueryRow(`SELECT p.quickCues,p.loops,t.bpm,t.isBeatGridLocked,t.rating FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.path='../Music/two.flac'`).Scan(&cue, &loop, &bpm, &locked, &rating); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cue, []byte("DECK")) || !bytes.Equal(loop, []byte("LOOP")) || bpm != 166 || locked != 1 || rating != 80 {
		t.Fatalf("deck fields were not kept: cue=%q loop=%q bpm=%d locked=%d rating=%d", cue, loop, bpm, locked, rating)
	}
}

func TestPort1NativeChildPlaylistSurvivesImportedFolderRemoval(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p1Open(t, mount)
	var folderID int64
	if err := db.QueryRow(`SELECT id FROM Playlist WHERE title='Folder'`).Scan(&folderID); err != nil {
		t.Fatal(err)
	}
	var dbUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&dbUUID); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Deck Set',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,0,0),(?,?,?,0,0)`, nativeID, 1, dbUUID, nativeID, 2, dbUUID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	names := [5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}
	pdb := testfixture.PDBNamed(names, "Other")
	pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("GoneXX"), 1)
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	when := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), when, when); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{})
	db = p1Open(t, mount)
	var parent int64
	if err := db.QueryRow(`SELECT parentListId FROM Playlist WHERE id=?`, nativeID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent != 0 {
		t.Fatalf("native list parent=%d, want root", parent)
	}
	var members []int64
	rows, err := db.Query(`SELECT trackId FROM PlaylistEntity WHERE listId=? ORDER BY id`, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		members = append(members, id)
	}
	_ = rows.Close()
	if !reflect.DeepEqual(members, []int64{1, 2}) {
		t.Fatalf("native list members=%v", members)
	}
	if got := query(t, mount, `SELECT count(*) FROM pragma_foreign_key_check`); got[0][0] != "0" {
		t.Fatalf("foreign keys: %s", got[0][0])
	}
}

func TestPort1UnreadableTrackNamesRefuseRemovalUntilRestored(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 100)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	pdb := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	st, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	when := st.ModTime().Add(time.Hour)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	var hidden []string
	for i := 1; i <= 7; i++ {
		old := filepath.Join(mount, testfixture.LargeTrackPath(i))
		newPath := filepath.Join(filepath.Dir(old), "\xe6"+filepath.Base(old))
		if err := os.Rename(old, newPath); err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, old)
	}
	// Files lie where those names should be: the job takes the names for unreadable, says so and
	// stages no removal. What it could read is published all the same.
	out := captureLog(t)
	res, err := FillInBackground(context.Background(), mount, FillOptions{})
	if !errors.Is(err, ErrFilesUnreadable) || res.Missing != 0 {
		t.Fatalf("fill: %+v, error %v, want ErrFilesUnreadable", res, err)
	}
	if !strings.Contains(out.String(), "NOT REMOVED: the files of 7 of the export's 100 tracks") {
		t.Fatalf("the journal does not say why nothing was removed:\n%s", out)
	}
	entries := query(t, mount, `SELECT count(*) FROM PlaylistEntity`)
	mountNow(t, mount, MountOptions{Publish: true})
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "100" {
		t.Fatalf("the refused update removed rows: %s left", got[0][0])
	}
	if got := query(t, mount, `SELECT count(*) FROM PlaylistEntity`); got[0][0] != entries[0][0] {
		t.Fatalf("the refused update removed list entries: %s of %s left", got[0][0], entries[0][0])
	}
	// With the names readable again the same stick imports as before, every row in place.
	for _, old := range hidden {
		newPath := filepath.Join(filepath.Dir(old), "\xe6"+filepath.Base(old))
		if err := os.Rename(newPath, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Removed != 0 || !res.Pending.None() {
		t.Fatalf("after the names came back: %+v", res)
	}
	// Files deleted by hand, far more than the limit, are gone: their rows go (row 353).
	for i := 1; i <= 40; i++ {
		if err := os.Remove(filepath.Join(mount, testfixture.LargeTrackPath(i))); err != nil {
			t.Fatal(err)
		}
	}
	when = when.Add(time.Hour)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{})
	if res, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil || res.Missing != 40 {
		t.Fatalf("fill after 40 files were deleted: %+v %v", res, err)
	}
	if res := mountNow(t, mount, MountOptions{Publish: true}); res.Removed != 40 {
		t.Fatalf("window after 40 files were deleted: %+v", res)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "60" {
		t.Fatalf("%s rows after 40 files were deleted", got[0][0])
	}
}

func TestPort1DeletedTracksAreRemovedAfterVerifiedFill(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 20)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	for i := 1; i <= 4; i++ {
		if err := os.Remove(filepath.Join(mount, testfixture.LargeTrackPath(i))); err != nil {
			t.Fatal(err)
		}
	}
	pdb := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	st, err := os.Stat(pdb)
	if err != nil {
		t.Fatal(err)
	}
	when := st.ModTime().Add(time.Hour)
	if err := os.Chtimes(pdb, when, when); err != nil {
		t.Fatal(err)
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	res := mountNow(t, mount, MountOptions{Publish: true})
	if res.Removed != 4 {
		t.Fatalf("removed=%d, want 4", res.Removed)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "16" {
		t.Fatalf("tracks=%s, want 16", got[0][0])
	}
}

func TestPort1SkeletonLibraryIsReplacedByExport(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	_, skeletonRoot, db := testlibrary.Create(t)
	if _, err := db.Exec(`INSERT INTO Track(path,filename,title) VALUES('../MUSIC/native.wav','native.wav','Deck loaded')`); err != nil {
		t.Fatal(err)
	}
	var skeletonUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&skeletonUUID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "Engine Library/Database2/m.db")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(skeletonRoot, "Engine Library/Database2/m.db"), filepath.Join(root, "Engine Library/Database2/m.db")); err != nil {
		t.Fatal(err)
	}
	res := mountNow(t, root, MountOptions{})
	if res.Added != 5 {
		t.Fatalf("skeleton conversion: %+v", res)
	}
	if got := query(t, root, `SELECT count(*) FROM Track`); got[0][0] != "6" {
		t.Fatalf("skeleton native row was not kept: %s", got[0][0])
	}
	// The row Engine made is Engine's still, and the library is the same one (its UUID).
	if got := query(t, root, `SELECT title FROM Track WHERE path='../MUSIC/native.wav'`); len(got) != 1 || got[0][0] != "Deck loaded" {
		t.Fatalf("the row of the track the DJ had loaded: %v", got)
	}
	if got := query(t, root, `SELECT uuid FROM Information`); got[0][0] != skeletonUUID {
		t.Fatalf("library uuid %s, the skeleton's was %s", got[0][0], skeletonUUID)
	}
}

func TestPort1CancelledFirstBuildLeavesNoEngineLibrary(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ConvertAtMount(ctx, root, MountOptions{}); err == nil {
		t.Fatal("cancelled first build succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatalf("cancelled first build left Engine Library: %v", err)
	}
	complete := t.TempDir()
	testfixture.Export(t, complete)
	mountNow(t, complete, MountOptions{})
	if got := query(t, complete, `SELECT count(*) FROM pragma_foreign_key_check`); got[0][0] != "0" {
		t.Fatalf("foreign_key_check=%s", got[0][0])
	}
	if got := query(t, complete, `PRAGMA integrity_check`); got[0][0] != "ok" {
		t.Fatalf("integrity_check=%s", got[0][0])
	}
}

func TestPort1MovedAndSwappedPathsRetainEngineIDs(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	ids := trackIDs(t, mount)
	db := p1Open(t, mount)
	editedID := fmt.Sprint(snapshot(t, mount).Tracks["music/two.flac"].EngineID)
	if _, err := db.Exec(`UPDATE PerformanceData SET quickCues=X'45444954' WHERE trackId=?`, editedID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"two.flac", "moved.flac"}, {"three.wav", "four.wav"}, {"four.wav", "three.wav"}} {
		old := filepath.Join(mount, "MUSIC", pair[0])
		newPath := filepath.Join(mount, "MUSIC", pair[1]+".tmp")
		if pair[0] == "two.flac" {
			newPath = filepath.Join(mount, "MUSIC", pair[1])
		}
		if err := os.Rename(old, newPath); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(mount, "MUSIC", "four.wav.tmp"), filepath.Join(mount, "MUSIC", "three.wav")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mount, "MUSIC", "three.wav.tmp"), filepath.Join(mount, "MUSIC", "four.wav")); err != nil {
		t.Fatal(err)
	}
	names := [5]string{testfixture.UnicodeName, "moved.flac", "four.wav", "three.wav", "five.wav"}
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", testfixture.PDBNamed(names, "同名"))
	st, err := os.Stat(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	when := st.ModTime().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"), when, when); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{})
	got := trackIDs(t, mount)
	for oldPath, id := range ids {
		want := oldPath
		switch oldPath {
		case "../Music/two.flac":
			want = "../Music/moved.flac"
		}
		if got[want] != id {
			t.Errorf("%s moved to %s: got id %s, want %s", oldPath, want, got[want], id)
		}
	}
	if got := query(t, mount, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=?`, editedID); got[0][0] != "45444954" {
		t.Fatalf("deck cue lost on move: %s", got[0][0])
	}
	if _, err := FillInBackground(context.Background(), mount, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{Publish: true})
	if again := mountNow(t, mount, MountOptions{}); !again.Bypassed {
		t.Fatalf("second moved-path mount changed the library: %+v", again)
	}
}

func TestPort1PublishingWindowCarriesEngineWrites(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	db := p1Open(t, mount)
	id := snapshot(t, mount).Tracks["music/three.wav"].EngineID
	if _, err := db.Exec(`UPDATE Track SET isPlayed=1,timeLastPlayed=1763370000 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(?,1),(?,2)`, id, id+1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Information SET currentPlayedIndiciator=4242`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{Publish: true})
	db = p1Open(t, mount)
	var played, indicator int64
	if err := db.QueryRow(`SELECT isPlayed FROM Track WHERE id=?`, id).Scan(&played); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT currentPlayedIndiciator FROM Information`).Scan(&indicator); err != nil {
		t.Fatal(err)
	}
	if played != 1 || indicator != 4242 {
		t.Fatalf("Engine writes lost: played=%d indicator=%d", played, indicator)
	}
	if got := query(t, mount, `SELECT count(*) FROM PreparelistEntity WHERE trackId IN (?,?)`, id, id+1); got[0][0] != "2" {
		t.Fatalf("prepare list entries=%s", got[0][0])
	}
	if got := query(t, mount, `PRAGMA integrity_check`); got[0][0] != "ok" {
		t.Fatalf("integrity_check=%s", got[0][0])
	}
}

func TestPort1OpenLibraryRefusesConcurrentProcess(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	dir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "m.db")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("sleep", "30")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	_, err = ConvertAtMount(context.Background(), root, MountOptions{})
	if !errors.Is(err, enginedb.ErrLibraryInUse) {
		t.Fatalf("error=%v, want ErrLibraryInUse", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("open m.db was removed: %v", err)
	}
}

func TestPort1RenumberedSourcesKeepIDsUUIDAndRGB(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	stageAll(t, mount)
	mountNow(t, mount, MountOptions{Publish: true})
	before := trackIDs(t, mount)
	s := snapshot(t, mount)
	db := p1Open(t, mount)
	var uuid string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tr := range s.Tracks {
		testfixture.Write(t, mount, filepath.Join("Engine Library/Database2/OverviewData", uuid, fmt.Sprintf("%d.rgb", tr.EngineID)), []byte(fmt.Sprintf("rgb-%d", tr.EngineID)))
	}
	rgbBefore := map[int64][]byte{}
	for _, tr := range s.Tracks {
		p := filepath.Join(dbDir(mount), "OverviewData", uuid, fmt.Sprintf("%d.rgb", tr.EngineID))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rgbBefore[tr.EngineID] = b
	}
	oldPDB := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	b, err := os.ReadFile(oldPDB)
	if err != nil {
		t.Fatal(err)
	}
	p1ShiftPDBIDs(b, 5000)
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", b)
	when := time.Now().Add(time.Hour)
	if err := os.Chtimes(oldPDB, when, when); err != nil {
		t.Fatal(err)
	}
	mountNow(t, mount, MountOptions{})
	if got := trackIDs(t, mount); !reflect.DeepEqual(got, before) {
		t.Fatalf("Engine IDs changed after source renumbering: before=%v after=%v", before, got)
	}
	db = p1Open(t, mount)
	var afterUUID string
	if err := db.QueryRow(`SELECT uuid FROM Information`).Scan(&afterUUID); err != nil {
		t.Fatal(err)
	}
	if afterUUID != uuid {
		t.Fatalf("uuid changed from %q to %q", uuid, afterUUID)
	}
	now := snapshot(t, mount)
	for key, tr := range now.Tracks {
		if len(tr.Refs) != 1 || tr.Refs[0].ID != s.Tracks[key].Refs[0].ID+5000 {
			t.Fatalf("%s source ID was not refreshed: before=%+v after=%+v", key, s.Tracks[key].Refs, tr.Refs)
		}
	}
	for _, tr := range s.Tracks {
		path := filepath.Join(dbDir(mount), "OverviewData", uuid, fmt.Sprintf("%d.rgb", tr.EngineID))
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, rgbBefore[tr.EngineID]) {
			t.Fatalf("RGB overview for %d changed: %v", tr.EngineID, err)
		}
	}
}

func TestPort1BrokenOneLibraryLeavesDeviceRowsAndRetries(t *testing.T) {
	mount := stick(t)
	testfixture.Write(t, mount, "PIONEER/rekordbox/exportLibrary.db", []byte("corrupt one-library"))
	res := mountNow(t, mount, MountOptions{})
	if res.Added != 5 {
		t.Fatalf("device library was hidden by corrupt OneLibrary: %+v", res)
	}
	if got := query(t, mount, `SELECT count(*) FROM Track`); got[0][0] != "5" {
		t.Fatalf("device rows=%s", got[0][0])
	}
	s := snapshot(t, mount)
	if s.Stamp != "" || !s.Pending.Verify {
		t.Fatalf("partial import stamp/pending: stamp=%q pending=%+v", s.Stamp, s.Pending)
	}
	if again := mountNow(t, mount, MountOptions{}); again.Bypassed {
		t.Fatal("partial import was bypassed on the next mount")
	}
}

func TestPort1StagedOnlyStickConvertsRepeatsAndRestages(t *testing.T) {
	for _, mode := range []string{"empty", "pioneer-directory", "native-engine"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if mode == "pioneer-directory" {
				if err := os.MkdirAll(filepath.Join(root, "PIONEER"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "native-engine" {
				_, skeleton, db := testlibrary.Create(t)
				if _, err := db.Exec(`INSERT INTO Track(path,filename,title) VALUES('../MUSIC/native.wav','native.wav','Native preserved')`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "Engine Library/Database2/m.db")), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(skeleton, "Engine Library/Database2/m.db"), filepath.Join(root, "Engine Library/Database2/m.db")); err != nil {
					t.Fatal(err)
				}
			}
			staged := filepath.Join(root, ".prolink-cache", "CDJ-3000")
			testfixture.Export(t, staged)
			res := mountNow(t, root, MountOptions{})
			if res.Added != 5 {
				t.Fatalf("staged-only import: %+v", res)
			}
			if got := query(t, root, `SELECT count(*) FROM Track WHERE path LIKE '../.prolink-cache/CDJ-3000/%'`); got[0][0] != "5" {
				t.Fatalf("staged rows=%s", got[0][0])
			}
			if got := query(t, root, `SELECT count(*) FROM Playlist WHERE title='[Pro DJ Link: CDJ-3000]'`); got[0][0] != "1" {
				t.Fatalf("staged root folder count=%s", got[0][0])
			}
			if mode == "native-engine" {
				if got := query(t, root, `SELECT count(*) FROM Track WHERE title='Native preserved'`); got[0][0] != "1" {
					t.Fatalf("native Engine row was lost: %s", got[0][0])
				}
			}
			if _, err := FillInBackground(context.Background(), root, FillOptions{}); err != nil {
				t.Fatal(err)
			}
			mountNow(t, root, MountOptions{Publish: true})
			if again := mountNow(t, root, MountOptions{}); !again.Bypassed {
				t.Fatalf("repeat staged-only mount: %+v", again)
			}
			pdb := filepath.Join(staged, "PIONEER/rekordbox/export.pdb")
			st, err := os.Stat(pdb)
			if err != nil {
				t.Fatal(err)
			}
			when := st.ModTime().Add(time.Hour)
			if err := os.Chtimes(pdb, when, when); err != nil {
				t.Fatal(err)
			}
			if changed := mountNow(t, root, MountOptions{}); changed.Bypassed {
				t.Fatalf("restaged export bypassed: %+v", changed)
			}
			if _, err := FillInBackground(context.Background(), root, FillOptions{}); err != nil {
				t.Fatal(err)
			}
			mountNow(t, root, MountOptions{Publish: true})
			if _, err := os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); !os.IsNotExist(err) && mode != "pioneer-directory" {
				t.Fatalf("created local export: %v", err)
			}
		})
	}
}

func TestPort1StagedAndLocalLibrariesRetainLocalRowsAndMergeHiddenExport(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	staged := filepath.Join(root, ".prolink-cache", "CDJ 1 USB")
	testfixture.Export(t, staged)
	if err := os.Rename(filepath.Join(staged, "PIONEER"), filepath.Join(staged, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	if got := mountNow(t, root, MountOptions{}); got.Added != 10 {
		t.Fatalf("local + hidden staged import: %+v", got)
	}
	if _, err := FillInBackground(context.Background(), root, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	mountNow(t, root, MountOptions{Publish: true})
	localID := trackIDs(t, root)["../MUSIC/two.flac"]
	if err := os.Rename(filepath.Join(root, "PIONEER"), filepath.Join(root, "PIONEER.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(staged, "MUSIC/five.wav")); err != nil {
		t.Fatal(err)
	}
	mountNow(t, root, MountOptions{})
	if _, err := FillInBackground(context.Background(), root, FillOptions{}); err != nil {
		t.Fatal(err)
	}
	mountNow(t, root, MountOptions{Publish: true})
	if got := trackIDs(t, root)["../MUSIC/two.flac"]; got != localID {
		t.Fatalf("local row changed from %s to %s", localID, got)
	}
	if got := query(t, root, `SELECT count(*) FROM Track WHERE path NOT LIKE '../.prolink-cache/%'`); got[0][0] != "5" {
		t.Fatalf("local rows=%s", got[0][0])
	}
	if got := query(t, root, `SELECT count(*) FROM Track WHERE path LIKE '../.prolink-cache/%'`); got[0][0] != "4" {
		t.Fatalf("staged rows=%s", got[0][0])
	}
	if got := query(t, root, `SELECT count(*) FROM Track`); got[0][0] != "9" {
		t.Fatalf("merged rows=%s", got[0][0])
	}
	if got := mountNow(t, root, MountOptions{}); !got.Bypassed {
		t.Fatalf("unchanged merged view was not bypassed: %+v", got)
	}
}
