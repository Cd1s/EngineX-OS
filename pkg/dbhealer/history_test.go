package dbhealer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
