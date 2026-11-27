package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyDB(t *testing.T, column string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hm.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, fmt.Sprintf(`PRAGMA foreign_keys=ON;
 CREATE TABLE Track(id INTEGER PRIMARY KEY,data BLOB);
 CREATE TABLE Historylist(id INTEGER PRIMARY KEY,startTime INTEGER);
 CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,%s INTEGER,trackId INTEGER,FOREIGN KEY(%s) REFERENCES Historylist(id) ON DELETE CASCADE);
 CREATE TABLE PlaylistEntity(id INTEGER PRIMARY KEY,trackId INTEGER);
 CREATE TABLE PreparelistEntity(id INTEGER PRIMARY KEY,trackId INTEGER);
 CREATE TABLE PerformanceData(trackId INTEGER PRIMARY KEY,quickCues BLOB,FOREIGN KEY(trackId) REFERENCES Track(id) ON DELETE CASCADE);`, column, column))
	return db, path
}
func TestEOS14PruneHistoryKeepsRecentSessionsAndReferencedTracks(t *testing.T) {
	for _, col := range []string{"listId", "historylistId"} {
		t.Run(col, func(t *testing.T) {
			db, path := historyDB(t, col)
			for i := 1; i <= 6; i++ {
				mustExec(t, db, "INSERT INTO Track VALUES(?,zeroblob(8192))", i)
				mustExec(t, db, "INSERT INTO PerformanceData VALUES(?,?)", i, []byte{byte(i)})
			}
			// Recency is determined by session timestamps, not the numeric ID.
			for _, s := range []struct{ id, date int }{{1, 500}, {2, 200}, {3, 400}, {4, 100}} {
				mustExec(t, db, "INSERT INTO Historylist VALUES(?,?)", s.id, s.date)
				mustExec(t, db, fmt.Sprintf("INSERT INTO HistorylistEntity VALUES(?,?,?)"), s.id, s.id, s.id)
			}
			mustExec(t, db, "INSERT INTO PlaylistEntity VALUES(1,5)")
			mustExec(t, db, "INSERT INTO PreparelistEntity VALUES(1,6)")
			if err := PruneHistoryDB(path, 2); err != nil {
				t.Fatal(err)
			}
			var count int
			db.QueryRow("SELECT count(*) FROM Historylist WHERE id IN (1,3)").Scan(&count)
			if count != 2 {
				t.Fatal(count)
			}
			db.QueryRow("SELECT count(*) FROM Historylist").Scan(&count)
			if count != 2 {
				t.Fatal(count)
			}
			db.QueryRow("SELECT count(*) FROM Track").Scan(&count)
			if count != 4 {
				t.Fatal(count)
			}
			db.QueryRow("SELECT count(*) FROM Track WHERE id IN (1,3,5,6)").Scan(&count)
			if count != 4 {
				t.Fatal(count)
			}
			db.QueryRow("SELECT count(*) FROM PerformanceData").Scan(&count)
			if count != 4 {
				t.Fatal("lost retained performance data")
			}
			var free int
			db.QueryRow("PRAGMA freelist_count").Scan(&free)
			if free != 0 {
				t.Fatalf("vacuum not performed: %d", free)
			}
			var check string
			db.QueryRow("PRAGMA integrity_check").Scan(&check)
			if check != "ok" {
				t.Fatal(check)
			}
			if err := PruneHistoryDB(path, 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestHistoryDefaultLimitAndUnsupportedRollback(t *testing.T) {
	db, path := historyDB(t, "listId")
	for i := 1; i <= 55; i++ {
		mustExec(t, db, "INSERT INTO Historylist VALUES(?,?)", i, i)
	}
	if err := PruneHistoryDB(path, 0); err != nil {
		t.Fatal(err)
	}
	var count int
	db.QueryRow("SELECT count(*) FROM Historylist").Scan(&count)
	if count != 50 {
		t.Fatal(count)
	}
	mustExec(t, db, "ALTER TABLE HistorylistEntity RENAME COLUMN listId TO unknown")
	if err := PruneHistoryDB(path, 2); err == nil {
		t.Fatal("unknown layout accepted")
	}
	db.QueryRow("SELECT count(*) FROM Historylist").Scan(&count)
	if count != 50 {
		t.Fatal("unknown schema was modified")
	}
	if err := PruneHistoryDB(path, -1); err == nil {
		t.Fatal("negative limit accepted")
	}
}
func TestReadOnlyMountNoWritesAndMissingDBNotCreated(t *testing.T) {
	db, path := newLibrary(t)
	track(t, db, 1)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "m.db")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dest, before, 0444); err != nil {
		t.Fatal(err)
	}
	r, err := HealMount(mount, 50)
	if err != nil || !r.ReadOnly || !r.Healthy {
		t.Fatalf("%+v %v", r, err)
	}
	after, _ := os.ReadFile(dest)
	if string(after) != string(before) {
		t.Fatal("read-only DB changed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("created sidecars")
	}
	if _, err = Open(filepath.Join(dir, "missing.db"), false); err == nil {
		t.Fatal("missing db created")
	}
	if err = os.WriteFile(dest+"-wal", []byte("pending WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dest, true); err == nil {
		t.Fatal("read-only pending WAL was ignored")
	}
}
func TestHealMountWithHistory(t *testing.T) {
	db, path := newLibrary(t)
	track(t, db, 1)
	db.Close()
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	os.MkdirAll(dir, 0755)
	blob, _ := os.ReadFile(path)
	os.WriteFile(filepath.Join(dir, "m.db"), blob, 0644)
	hist, hpath := newLibrary(t)
	track(t, hist, 1)
	track(t, hist, 2)
	mustExec(t, hist, `CREATE TABLE Historylist(id INTEGER PRIMARY KEY,startTime INTEGER);
    CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,listId INTEGER,trackId INTEGER);
    INSERT INTO Historylist VALUES(1,100);
    INSERT INTO HistorylistEntity VALUES(1,1,1);
    INSERT INTO Playlist(id,title,parentListId,nextListId) VALUES(1,'History Set',0,0);
    INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,1,1,999);`)
	hist.Close()
	blob, _ = os.ReadFile(hpath)
	os.WriteFile(filepath.Join(dir, "hm.db"), blob, 0644)
	r, err := HealMount(mount, 1)
	if err != nil || !r.Healthy || !r.HistoryPruned || r.Waveforms != 3 || r.PlaylistEntities != 1 || r.ReanalysisPending != 6 {
		t.Fatalf("%+v %v", r, err)
	}
	healed, e := Open(filepath.Join(dir, "hm.db"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer healed.Close()
	chainIDs(t, healed, "PlaylistEntity", "listId", "nextEntityId", 1)
	var count int
	healed.QueryRow("SELECT count(*) FROM Track").Scan(&count)
	if count != 1 {
		t.Fatal("orphan history track retained")
	}
	status, e := InspectMount(mount)
	if e != nil || status.ReanalysisPending != 6 {
		t.Fatalf("%+v %v", status, e)
	}
}

func TestHistorySchemaVersionProtection(t *testing.T) {
	db, path := newLibrary(t)
	mustExec(t, db, "CREATE TABLE Historylist(id INTEGER PRIMARY KEY);CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,listId INTEGER,trackId INTEGER); INSERT INTO Historylist VALUES(1),(2)")
	mustExec(t, db, "UPDATE Information SET schemaVersionMajor=99")
	if err := PruneHistoryDB(path, 1); err == nil {
		t.Fatal("future history schema pruned")
	}
	var count int
	db.QueryRow("SELECT count(*) FROM Historylist").Scan(&count)
	if count != 2 {
		t.Fatal("future history data changed")
	}
}

// historyMount is a drive with m.db and an hm.db holding sessions, given as
// (startTime, isDeleted) pairs, each with one entry for track 1.
func historyMount(t *testing.T, sessions [][2]int) (mount, hm string) {
	t.Helper()
	db, path := newLibrary(t)
	track(t, db, 1)
	db.Close()
	mount = t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	os.MkdirAll(dir, 0755)
	blob, _ := os.ReadFile(path)
	os.WriteFile(filepath.Join(dir, "m.db"), blob, 0644)
	hist, hpath := newLibrary(t)
	track(t, hist, 1)
	mustExec(t, hist, `CREATE TABLE Historylist(id INTEGER PRIMARY KEY,startTime INTEGER,isDeleted BOOLEAN);
    CREATE TABLE HistorylistEntity(id INTEGER PRIMARY KEY,listId INTEGER,trackId INTEGER);`)
	for i, s := range sessions {
		mustExec(t, hist, `INSERT INTO Historylist VALUES(?,?,?)`, i+1, s[0], s[1])
		mustExec(t, hist, `INSERT INTO HistorylistEntity VALUES(?,?,1)`, i+1, i+1)
	}
	hist.Close()
	blob, _ = os.ReadFile(hpath)
	hm = filepath.Join(dir, "hm.db")
	os.WriteFile(hm, blob, 0644)
	return mount, hm
}

func historyCount(t *testing.T, hm, where string) (n int) {
	t.Helper()
	db, err := Open(hm, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM Historylist " + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// BH-F52-2: the mount hook's heal (--data-only) deleted every history session
// but the newest 50 on each insert, and added sqlite_stat1 to hm.db (BH-GC1-13).
// Stock Engine keeps them all.
func TestDataOnlyHealKeepsTheWholeHistory(t *testing.T) {
	var sessions [][2]int
	for i := 0; i < 60; i++ {
		sessions = append(sessions, [2]int{100 + i, 0})
	}
	mount, hm := historyMount(t, sessions)
	r, err := HealMountDataOnly(mount, 0)
	if err != nil || r.HistoryPruned {
		t.Fatalf("%+v %v", r, err)
	}
	if n := historyCount(t, hm, ""); n != 60 {
		t.Fatalf("sessions after the mount heal: %d, want 60", n)
	}
	db, _ := Open(hm, true)
	defer db.Close()
	var stat int
	db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'").Scan(&stat)
	if stat != 0 {
		t.Fatal("the data-only heal changed hm.db's schema (sqlite_stat1)")
	}
}

// BH-GC1-13: sessions the DJ deleted on the deck (isDeleted=1) counted towards
// the limit, so newer deleted ones took the place of the visible history.
func TestPruneHistoryKeepsVisibleSessionsOverDeletedOnes(t *testing.T) {
	_, hm := historyMount(t, [][2]int{{100, 0}, {200, 1}, {300, 1}})
	if err := PruneHistoryDB(hm, 2); err != nil {
		t.Fatal(err)
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)=0"); n != 1 {
		t.Fatalf("visible sessions left: %d, want 1", n)
	}
}

func stockHistoryMount(t *testing.T) (mount, hm string) {
	t.Helper()
	db, path := newLibrary(t)
	track(t, db, 1)
	db.Close()
	mount = t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.db"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	hm = filepath.Join(dir, "hm.db")
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
	if err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	mustExec(t, hdb, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, hdb, "INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'hist-s16',3,0,2)")
	mustExec(t, hdb, "PRAGMA foreign_keys=ON")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	return mount, hm
}

func schemaDump(t *testing.T, path string) string {
	t.Helper()
	db, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s string
	if err := db.QueryRow(`SELECT group_concat(type||name||COALESCE(sql,''),char(10)) FROM (SELECT * FROM main.sqlite_master ORDER BY type,name)`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS16DataOnlyCleansDeletedTombstonesAndOrphansPreservingVisibleHistory(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	// Sentinel empty AlbumArt row (id=1), live cover (id=2), and orphan covers (id=3..42).
	mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
	mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(2,'live-art',zeroblob(2048))")
	for a := 3; a <= 42; a++ {
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(?,?,zeroblob(8192))", a, fmt.Sprintf("orphan-art-%d", a))
	}
	// Live tracks: 1..64 in visible sessions, 65 in PlaylistEntity only.
	for i := 1; i <= 65; i++ {
		artID := 1
		if i == 1 {
			artID = 2
		}
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(?,?,?,?,?,0)",
			i, fmt.Sprintf("../Music/live-%d.mp3", i), fmt.Sprintf("live-%d.mp3", i), fmt.Sprintf("Live %d", i), artID)
	}
	// 65 visible sessions (exceeding 50 to protect checklist row 82), including one empty
	// visible session (id=65) and one repeated track play in session 1.
	for s := 1; s <= 65; s++ {
		mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(?,?,?,'UTC',0)", s, fmt.Sprintf("Set %d", s), 1700000000+s*3600)
		if s <= 64 {
			mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(?,?,?)", s, s, 1700000000+s*3600)
		}
	}
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,?)", 1700000300)
	// Track 65 is retained via PlaylistEntity even without HistorylistEntity.
	mustExec(t, hdb, "INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId) VALUES(1,'Saved',0,1,0)")
	mustExec(t, hdb, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,1,65,'hist-s16',0,0)")

	// 120 soft-deleted tombstones (isDeleted=1) + their tracks/performanceData/orphan covers,
	// plus 30 unreferenced orphan tracks and dangling HistorylistEntity rows.
	for i := 66; i <= 215; i++ {
		artID := 3 + (i % 40)
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(?,?,?,?,?,0)",
			i, fmt.Sprintf("../Music/dead-%d.mp3", i), fmt.Sprintf("dead-%d.mp3", i), fmt.Sprintf("Dead %d", i), artID)
		mustExec(t, hdb, "UPDATE PerformanceData SET overviewWaveFormData=zeroblob(4096) WHERE trackId=?", i)
	}
	for s := 66; s <= 185; s++ {
		mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(?,?,?,'UTC',1)", s, fmt.Sprintf("Deleted %d", s), 1600000000+s*60)
		mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(?,?,?)", s, s, 1600000000+s*60)
	}
	mustExec(t, hdb, "PRAGMA foreign_keys=OFF")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(NULL,1,1600000000),(999999,1,1600000001),(1,888888,1600000002)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}

	schemaBefore := schemaDump(t, hm)
	sizeBefore, err := os.Stat(hm)
	if err != nil {
		t.Fatal(err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy || r.HistoryPruned || r.HistoryCleaned == 0 || !r.HistoryCompacted {
		t.Fatalf("unexpected data-only heal report: %+v err=%v", r, err)
	}

	if got := schemaDump(t, hm); got != schemaBefore {
		t.Fatalf("data-only history cleanup altered hm.db schema:\nbefore:\n%s\nafter:\n%s", schemaBefore, got)
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)=0"); n != 65 {
		t.Fatalf("visible sessions=%d, want 65 (row 82)", n)
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)<>0"); n != 0 {
		t.Fatalf("deleted tombstones remaining=%d, want 0", n)
	}

	check, err := Open(hm, true)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	assertCount := func(query string, want int) {
		t.Helper()
		var got int
		if err := check.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
	assertCount("SELECT count(*) FROM HistorylistEntity", 65) // 64 sessions + 1 repeated play
	assertCount("SELECT count(*) FROM Track", 65)             // 64 history tracks + track 65 in PlaylistEntity
	assertCount("SELECT count(*) FROM PerformanceData", 65)
	assertCount("SELECT count(*) FROM AlbumArt", 2) // id=1 empty sentinel + id=2 live cover
	assertCount("SELECT count(*) FROM sqlite_master WHERE name='sqlite_stat1'", 0)

	var integrity string
	if err := check.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check=%q err=%v", integrity, err)
	}
	var fkRows int
	rows, err := check.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		fkRows++
	}
	rows.Close()
	if fkRows != 0 {
		t.Fatalf("foreign_key_check violations=%d, want 0", fkRows)
	}
	var freelist int
	if err := check.QueryRow("PRAGMA freelist_count").Scan(&freelist); err != nil || freelist != 0 {
		t.Fatalf("freelist_count=%d err=%v, want 0 after compaction", freelist, err)
	}
	check.Close()

	if res, ok, err := Reserve(hm); err != nil || !ok || res != 0 {
		t.Fatalf("hm.db reserve=%d ok=%v err=%v, want 0", res, ok, err)
	}
	sizeAfter, err := os.Stat(hm)
	if err != nil {
		t.Fatal(err)
	}
	if sizeAfter.Size() >= sizeBefore.Size() {
		t.Fatalf("hm.db did not shrink: before=%d after=%d", sizeBefore.Size(), sizeAfter.Size())
	}

	// Second pass on the cleaned hm.db must be an idempotent no-op.
	r2, err := HealMountDataOnly(mount, 0)
	if err != nil || !r2.Healthy || r2.HistoryPruned || r2.HistoryCleaned != 0 || r2.HistoryCompacted {
		t.Fatalf("second pass was not a no-op: %+v err=%v", r2, err)
	}

	// Inserting a new Track afterwards must satisfy trigger_after_insert_Track_check_id.
	wdb, err := Open(hm, false)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, wdb, "INSERT INTO Track(path,filename,title,albumArtId,isAnalyzed) VALUES('../Music/new.mp3','new.mp3','New',1,0)")
	if err := wdb.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestS16DataOnlyCompactsPreExistingFreelistBloat(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,isAnalyzed) VALUES(1,'../Music/keep.mp3','keep.mp3','Keep',0)")
	mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep Set',1700000000,'UTC',0)")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,1700000000)")
	// Simulate stock Engine deleting rows without VACUUM, leaving >1 MiB (>=256 pages) freelist bloat.
	for a := 1; a <= 40; a++ {
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(?,?,zeroblob(32768))", a, fmt.Sprintf("tmp-%d", a))
	}
	mustExec(t, hdb, "DELETE FROM AlbumArt")
	var freeBefore int
	if err := hdb.QueryRow("PRAGMA freelist_count").Scan(&freeBefore); err != nil || freeBefore < 256 {
		t.Fatalf("seed freelist_count=%d err=%v, want >=256", freeBefore, err)
	}
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}

	sizeBefore, _ := os.Stat(hm)
	schemaBefore := schemaDump(t, hm)
	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy || r.HistoryPruned {
		t.Fatalf("%+v %v", r, err)
	}
	if got := schemaDump(t, hm); got != schemaBefore {
		t.Fatalf("freelist compaction altered schema")
	}
	check, err := Open(hm, true)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var freeAfter int
	if err := check.QueryRow("PRAGMA freelist_count").Scan(&freeAfter); err != nil || freeAfter != 0 {
		t.Fatalf("freelist_count=%d err=%v, want 0", freeAfter, err)
	}
	sizeAfter, _ := os.Stat(hm)
	if sizeAfter.Size() >= sizeBefore.Size() {
		t.Fatalf("freelist bloat was not compacted: before=%d after=%d", sizeBefore.Size(), sizeAfter.Size())
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)=0"); n != 1 {
		t.Fatalf("visible session lost: %d", n)
	}
}

func TestS16DataOnlyRefusesWhenHistoryDBInUseAndRespectsBudget(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,isAnalyzed) VALUES(1,'../Music/dead.mp3','dead.mp3','Dead',0)")
	mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Del',1600000000,'UTC',1)")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,1600000000)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}

	// 1. Hold hm.db open in a child process (m.db closed): data-only heal must refuse.
	f, err := os.Open(hm)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	_, err = HealMountDataOnly(mount, 0)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !errors.Is(err, errLibraryInUse) {
		t.Fatalf("expected errLibraryInUse when hm.db is open elsewhere, got %v", err)
	}

	// 2. Expired context must not error and must leave hm.db untouched.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r, err := HealMountDataOnlyContext(expired, mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("expired context failed: %+v %v", r, err)
	}
	if n := historyCount(t, hm, ""); n != 1 {
		t.Fatalf("expired context modified hm.db: rows=%d", n)
	}

	// 3. Tight budget (< minHistoryVacuumBudget) cleans dead rows without running VACUUM.
	tight, cancelTight := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelTight()
	r, err = HealMountDataOnlyContext(tight, mount, 0)
	if err != nil || !r.Healthy || r.HistoryCleaned == 0 || r.HistoryCompacted {
		t.Fatalf("tight budget report=%+v err=%v, want cleaned>0 and compacted=false", r, err)
	}
	if n := historyCount(t, hm, ""); n != 0 {
		t.Fatalf("tombstone not cleaned under tight budget: rows=%d", n)
	}

	// 4. Read-only hm.db is left byte-identical with a warning.
	beforeRO, err := os.ReadFile(hm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hm, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hm, 0o644) })
	r, err = HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy || r.HistoryCleaned != 0 || r.HistoryCompacted {
		t.Fatalf("read-only hm.db report=%+v err=%v", r, err)
	}
	afterRO, err := os.ReadFile(hm)
	if err != nil || string(afterRO) != string(beforeRO) {
		t.Fatalf("read-only hm.db was modified")
	}

	// 5. Unsupported future hm.db schema fails without modifying hm.db.
	if err := os.Chmod(hm, 0o644); err != nil {
		t.Fatal(err)
	}
	wdb, err := Open(hm, false)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, wdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep',100,'UTC',1)")
	mustExec(t, wdb, "UPDATE Information SET schemaVersionMajor=99")
	if err := wdb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := HealMountDataOnly(mount, 0); err == nil {
		t.Fatal("unsupported future hm.db schema accepted in data-only mode")
	}
	if n := historyCount(t, hm, ""); n != 1 {
		t.Fatalf("unsupported future hm.db was modified: rows=%d", n)
	}
}

func TestS16PruneHistoryRemovesOrphanAlbumArt(t *testing.T) {
	_, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL),(2,'keep',zeroblob(1024)),(3,'drop',zeroblob(4096))")
	mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(1,'../Music/1.mp3','1.mp3','One',2,0),(2,'../Music/2.mp3','2.mp3','Two',3,0)")
	mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'New',200,'UTC',0),(2,'Old',100,'UTC',0)")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,200),(2,2,100)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}

	if err := PruneHistoryDB(hm, 1); err != nil {
		t.Fatal(err)
	}
	check, err := Open(hm, true)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var artCount int
	if err := check.QueryRow("SELECT count(*) FROM AlbumArt").Scan(&artCount); err != nil {
		t.Fatal(err)
	}
	if artCount != 2 {
		t.Fatalf("AlbumArt count after prune=%d, want 2 (sentinel id=1 + live id=2)", artCount)
	}
}

func TestS16UnsupportedSchemaDoesNotMutate(t *testing.T) {
	cases := []struct {
		name      string
		dropTable string
	}{
		{name: "missing_Information", dropTable: "Information"},
		{name: "missing_PerformanceData", dropTable: "PerformanceData"},
		{name: "missing_Playlist", dropTable: "Playlist"},
		{name: "missing_AlbumArt", dropTable: "AlbumArt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mount, hm := stockHistoryMount(t)
			hdb, err := sql.Open("sqlite", hm)
			if err != nil {
				t.Fatal(err)
			}
			hdb.SetMaxOpenConns(1)
			mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
			mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(1,'../Music/live.mp3','live.mp3','Live',1,0),(2,'../Music/dead.mp3','dead.mp3','Dead',1,0)")
			mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep',200,'UTC',0),(2,'Del',100,'UTC',1)")
			mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,200),(2,2,100)")
			mustExec(t, hdb, "PRAGMA foreign_keys=OFF")
			mustExec(t, hdb, "DROP TABLE "+tc.dropTable)
			if err := hdb.Close(); err != nil {
				t.Fatal(err)
			}

			r, err := HealMountDataOnly(mount, 0)
			if err == nil || r.Healthy {
				t.Fatalf("expected error for %s, got report=%+v err=%v", tc.name, r, err)
			}
			if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)<>0"); n != 1 {
				t.Fatalf("%s mutated hm.db before schema validation: deleted tombstones remaining=%d, want 1", tc.name, n)
			}
			if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)=0"); n != 1 {
				t.Fatalf("%s altered visible sessions: remaining=%d, want 1", tc.name, n)
			}
			check, err := Open(hm, true)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var tracks int
			if err := check.QueryRow("SELECT count(*) FROM Track").Scan(&tracks); err != nil || tracks != 2 {
				t.Fatalf("%s mutated Track rows: count=%d err=%v, want 2", tc.name, tracks, err)
			}
		})
	}
}

func TestS16DataOnlyHistoryRespectsMountDeadlineWhenLocked(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,isAnalyzed) VALUES(1,'../Music/live.mp3','live.mp3','Live',0),(2,'../Music/dead.mp3','dead.mp3','Dead',0)")
	mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep',200,'UTC',0),(2,'Del',100,'UTC',1)")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,200),(2,2,100)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}

	// Hold an EXCLUSIVE lock from the same process so libraryInUse (other PIDs)
	// does not short-circuit and the hm.db phase must honor ctx.Deadline().
	locker, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := HealMountDataOnlyContext(ctx, mount, 0)
	elapsed := time.Since(start)

	if _, rbErr := locker.Exec("ROLLBACK"); rbErr != nil {
		t.Fatal(rbErr)
	}
	if err := locker.Close(); err != nil {
		t.Fatal(err)
	}

	if elapsed >= time.Second {
		t.Fatalf("locked hm.db blew past 200ms mount deadline: elapsed=%s (busy_timeout not bounded by ctx)", elapsed)
	}
	if err != nil || !r.Healthy || r.HistoryCleaned != 0 || r.HistoryCompacted {
		t.Fatalf("locked hm.db under short mount budget should stop cleanly: report=%+v err=%v", r, err)
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)<>0"); n != 1 {
		t.Fatalf("locked hm.db was unexpectedly mutated: tombstones=%d, want 1", n)
	}

	// Once unlocked, a normal-budget pass cleans the tombstone cleanly.
	r2, err := HealMountDataOnly(mount, 0)
	if err != nil || !r2.Healthy || r2.HistoryCleaned == 0 {
		t.Fatalf("unlocked follow-up pass failed: report=%+v err=%v", r2, err)
	}
	if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)<>0"); n != 0 {
		t.Fatalf("unlocked follow-up left tombstones=%d, want 0", n)
	}
}

func TestS16MergeMalformedPlaylistEntityAndAlbumArtColumnsDoNotMutate(t *testing.T) {
	t.Run("missing_PlaylistEntity_trackId", func(t *testing.T) {
		mount, hm := stockHistoryMount(t)
		hdb, err := sql.Open("sqlite", hm)
		if err != nil {
			t.Fatal(err)
		}
		hdb.SetMaxOpenConns(1)
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(1,'../Music/pl.mp3','pl.mp3','PL',1,0),(2,'../Music/dead.mp3','dead.mp3','Dead',1,0)")
		mustExec(t, hdb, "INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId) VALUES(1,'Saved',0,1,0)")
		mustExec(t, hdb, "INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,1,1,'hist-s16',0,0)")
		mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Del',100,'UTC',1)")
		mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,2,100)")
		mustExec(t, hdb, "ALTER TABLE PlaylistEntity RENAME COLUMN trackId TO renamedTrackId")
		if err := hdb.Close(); err != nil {
			t.Fatal(err)
		}

		r, err := HealMountDataOnly(mount, 0)
		if err == nil || r.Healthy {
			t.Fatalf("malformed PlaylistEntity succeeded: rep=%+v err=%v", r, err)
		}
		check, err := Open(hm, true)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var tracks int
		if err := check.QueryRow("SELECT count(*) FROM Track").Scan(&tracks); err != nil || tracks != 2 {
			t.Fatalf("malformed PlaylistEntity deleted tracks: count=%d err=%v, want 2", tracks, err)
		}
	})

	t.Run("missing_AlbumArt_hash", func(t *testing.T) {
		mount, hm := stockHistoryMount(t)
		hdb, err := sql.Open("sqlite", hm)
		if err != nil {
			t.Fatal(err)
		}
		hdb.SetMaxOpenConns(1)
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(1,'../Music/live.mp3','live.mp3','Live',1,0),(2,'../Music/dead.mp3','dead.mp3','Dead',1,0)")
		mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep',200,'UTC',0),(2,'Del',100,'UTC',1)")
		mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,200),(2,2,100)")
		mustExec(t, hdb, "PRAGMA foreign_keys=OFF")
		mustExec(t, hdb, "ALTER TABLE AlbumArt RENAME COLUMN hash TO renamedHash")
		if err := hdb.Close(); err != nil {
			t.Fatal(err)
		}

		r, err := HealMountDataOnly(mount, 0)
		if err == nil || r.Healthy {
			t.Fatalf("malformed AlbumArt succeeded: rep=%+v err=%v", r, err)
		}
		if n := historyCount(t, hm, "WHERE COALESCE(isDeleted,0)<>0"); n != 1 {
			t.Fatalf("malformed AlbumArt committed tombstone deletion: remaining=%d, want 1", n)
		}
		check, err := Open(hm, true)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var sentinel int
		if err := check.QueryRow("SELECT count(*) FROM AlbumArt WHERE id=1").Scan(&sentinel); err != nil || sentinel != 1 {
			t.Fatalf("malformed AlbumArt deleted id=1 sentinel: count=%d err=%v", sentinel, err)
		}
	})
}

func TestS16MergeCleanHistoryZeroWriteUnderBeginImmediate(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	hdb, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
	mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(1,'../Music/live.mp3','live.mp3','Live',1,1)")
	mustExec(t, hdb, "INSERT INTO Historylist(id,title,startTime,timezone,isDeleted) VALUES(1,'Keep',200,'UTC',0)")
	mustExec(t, hdb, "INSERT INTO HistorylistEntity(listId,trackId,startTime) VALUES(1,1,200)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	// Baseline clean state before locking for writes.
	if r0, err := HealMountDataOnly(mount, 0); err != nil || !r0.Healthy {
		t.Fatalf("initial clean setup failed: rep=%+v err=%v", r0, err)
	}

	locker, err := sql.Open("sqlite", hm)
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer locker.Close()
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = locker.Exec("ROLLBACK") }()

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("clean hm.db attempted unnecessary write under BEGIN IMMEDIATE: rep=%+v err=%v", r, err)
	}
}

func TestS16MergeReadOnlyHistoryWithReserveNotRewritten(t *testing.T) {
	mount, hm := stockHistoryMount(t)
	seed := reserve12Seed(t, "hm-res12.db")
	hdb, err := sql.Open("sqlite", seed)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
	if err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	mustExec(t, hdb, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, hdb, "INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'hist-res12',3,0,2)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	resBytes, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hm, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hm, resBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hm, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hm, 0o644) })

	if r, ok, err := Reserve(hm); err != nil || !ok || r != 12 {
		t.Fatalf("setup reserve=%d ok=%v err=%v, want 12", r, ok, err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil || !r.Healthy {
		t.Fatalf("HealMountDataOnly with read-only hm.db: rep=%+v err=%v", r, err)
	}
	for _, fixed := range r.ReserveFixed {
		if fixed == "hm.db" {
			t.Fatalf("read-only hm.db was rewritten by NormalizeDatabase2: %+v", r.ReserveFixed)
		}
	}
	if !strings.Contains(strings.Join(r.Warnings, "\n"), "history library is read-only") {
		t.Fatalf("expected read-only history warning, got %v", r.Warnings)
	}
	after, err := os.ReadFile(hm)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(resBytes) {
		t.Fatal("mode-0444 hm.db bytes changed during HealMountDataOnly")
	}
}

func TestS16MergeMalformedLowercasePreparelistEntityPreservesTrack(t *testing.T) {
	t.Run("missing_trackId_column_fails_before_delete", func(t *testing.T) {
		mount, hm := stockHistoryMount(t)
		hdb, err := sql.Open("sqlite", hm)
		if err != nil {
			t.Fatal(err)
		}
		hdb.SetMaxOpenConns(1)
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(10,'../Music/prep.mp3','prep.mp3','Prep',1,1)")
		mustExec(t, hdb, `CREATE TABLE preparelistentity(id INTEGER PRIMARY KEY, track_ref INTEGER);
			INSERT INTO preparelistentity VALUES(1, 10)`)
		if err := hdb.Close(); err != nil {
			t.Fatal(err)
		}

		r, err := HealMountDataOnly(mount, 0)
		if err == nil || r.Healthy {
			t.Fatalf("malformed lowercase preparelistentity succeeded: rep=%+v err=%v", r, err)
		}
		if r.HistoryCleaned != 0 {
			t.Fatalf("expected HistoryCleaned=0 on schema failure, got %d", r.HistoryCleaned)
		}

		check, err := Open(hm, true)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var tracks, perf, prep int
		if err := check.QueryRow(`SELECT count(*) FROM Track WHERE id=10`).Scan(&tracks); err != nil {
			t.Fatal(err)
		}
		if err := check.QueryRow(`SELECT count(*) FROM PerformanceData WHERE trackId=10`).Scan(&perf); err != nil {
			t.Fatal(err)
		}
		if err := check.QueryRow(`SELECT count(*) FROM preparelistentity WHERE track_ref=10`).Scan(&prep); err != nil {
			t.Fatal(err)
		}
		if tracks != 1 || perf != 1 || prep != 1 {
			t.Fatalf("malformed schema mutated rows: tracks=%d perf=%d prep=%d", tracks, perf, prep)
		}
	})

	t.Run("valid_lowercase_preparelistentity_keeps_referenced_track", func(t *testing.T) {
		mount, hm := stockHistoryMount(t)
		hdb, err := sql.Open("sqlite", hm)
		if err != nil {
			t.Fatal(err)
		}
		hdb.SetMaxOpenConns(1)
		mustExec(t, hdb, "INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,NULL,NULL)")
		mustExec(t, hdb, "INSERT INTO Track(id,path,filename,title,albumArtId,isAnalyzed) VALUES(10,'../Music/prep.mp3','prep.mp3','Prep',1,1),(11,'../Music/dead.mp3','dead.mp3','Dead',1,1)")
		mustExec(t, hdb, `CREATE TABLE preparelistentity(id INTEGER PRIMARY KEY, trackId INTEGER);
			INSERT INTO preparelistentity VALUES(1, 10)`)
		if err := hdb.Close(); err != nil {
			t.Fatal(err)
		}

		r, err := HealMountDataOnly(mount, 0)
		if err != nil || !r.Healthy {
			t.Fatalf("lowercase preparelistentity clean failed: rep=%+v err=%v", r, err)
		}
		check, err := Open(hm, true)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var kept, removed int
		_ = check.QueryRow(`SELECT count(*) FROM Track WHERE id=10`).Scan(&kept)
		_ = check.QueryRow(`SELECT count(*) FROM Track WHERE id=11`).Scan(&removed)
		if kept != 1 || removed != 0 {
			t.Fatalf("lowercase preparelistentity track disposition: kept=%d removed=%d", kept, removed)
		}
	})
}
