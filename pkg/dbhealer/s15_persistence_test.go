package dbhealer

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestS15RestoresStrippedPlaylistPersistenceAndFolderHierarchy(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	var localUUID string
	if err := db.QueryRow(`SELECT COALESCE(uuid,'') FROM Information LIMIT 1`).Scan(&localUUID); err != nil {
		t.Fatal(err)
	}
	if localUUID == "" {
		localUUID = "s15-local-drive-uuid"
		if _, err := db.Exec(`UPDATE Information SET uuid=?`, localUUID); err != nil {
			t.Fatal(err)
		}
	}

	// Create 3 local tracks on disk.
	for i := 1; i <= 3; i++ {
		rel := filepath.ToSlash(filepath.Join("..", "Music", "track-"+string(rune('0'+i))+".mp3"))
		full := filepath.Join(mount, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO Track(id,path,filename,fileBytes,isAnalyzed) VALUES(?,?,?,?,0)`,
			i, rel, filepath.Base(rel), 5,
		); err != nil {
			t.Fatal(err)
		}
	}

	// Disable stock insert trigger temporarily to simulate an Engine DJ Desktop 5.0.0
	// export that stripped isPersisted=0 and isExplicitlyExported=0 across a nested
	// folder tree (t/70166: MASTER > DEEP APERO > DEEP 1 > DEEP 1/2025) as well as a
	// persisted child under a stripped parent folder, while keeping legitimate edge cases.
	var trigDDL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trigger_after_insert_isPersist'`).Scan(&trigDDL); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if trigDDL != "" {
		if _, err := db.Exec(`DROP TRIGGER trigger_after_insert_isPersist`); err != nil {
			t.Fatal(err)
		}
	}

	stmts := []string{
		// 1-4: 5.0.0-poisoned nested hierarchy (all stripped to 0/0, leaf 4 has local tracks).
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(1,'MASTER',0,0,5,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(2,'DEEP APERO',1,0,0,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(3,'DEEP 1',2,0,0,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(4,'DEEP 1/2025',3,0,0,1700000000,0)`,
		// 5-6: Already-healthy auto-included parent (1/0) + explicitly exported child (1/1) -> MUST stay 1/0 and 1/1.
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(5,'Healthy Folder',0,1,7,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(6,'Healthy Child',5,1,0,1700000000,1)`,
		// 7-8: Legitimately deleted empty folder + empty child (0/0, zero tracks) -> MUST NOT be resurrected.
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(7,'Deleted Folder',0,0,9,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(8,'Deleted Empty List',7,0,0,1700000000,0)`,
		// 9: Foreign-drive stub playlist (0/0, entities only reference another drive UUID) -> MUST stay 0/0.
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(9,'Foreign Drive Stub',0,0,10,1700000000,0)`,
		// 10: Orphan-entity playlist (0/0, entity points to nonexistent Track 999) -> MUST stay 0/0.
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(10,'Orphan Entity Tombstone',0,0,11,1700000000,0)`,
		// 11-12: Stripped parent folder (0/0) holding a persisted child edited on deck (1/1) -> parent 11 restored to 1/1.
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(11,'Stripped Parent Of Live',0,0,0,1700000000,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(12,'Deck Edited Child',11,1,0,1700000000,1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if trigDDL != "" {
		if _, err := db.Exec(trigDDL); err != nil {
			t.Fatal(err)
		}
	}

	// Populate PlaylistEntity rows.
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,4,1,?,2,0),(2,4,2,?,0,0)`, localUUID, localUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(3,6,3,?,0,0)`, localUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(4,9,1,'other-drive-uuid',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(5,10,999,?,0,0)`, localUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(6,12,2,?,0,0)`, localUUID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil {
		t.Fatalf("HealMountDataOnly failed: %v", err)
	}
	if !r.Healthy {
		t.Fatalf("expected Healthy=true, got %+v", r)
	}
	// Expect exactly 5 playlists restored: IDs 1, 2, 3, 4 (5.0.0-poisoned tree) + 11 (stripped parent).
	if r.PlaylistsPersisted != 5 {
		t.Fatalf("PlaylistsPersisted=%d, want 5 (report=%+v)", r.PlaylistsPersisted, r)
	}

	checkDB, err := Open(filepath.Join(mount, "Engine Library", "Database2", "m.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()

	want := map[int64][2]int{
		1:  {1, 1}, // MASTER
		2:  {1, 1}, // DEEP APERO
		3:  {1, 1}, // DEEP 1
		4:  {1, 1}, // DEEP 1/2025
		5:  {1, 0}, // Healthy auto-included folder untouched
		6:  {1, 1}, // Healthy child untouched
		7:  {0, 0}, // Deleted folder stays 0
		8:  {0, 0}, // Deleted empty list stays 0
		9:  {0, 0}, // Foreign-drive stub stays 0
		10: {0, 0}, // Orphan-entity tombstone stays 0
		11: {1, 1}, // Stripped parent of deck-edited child restored
		12: {1, 1}, // Deck-edited child untouched
	}
	for id, exp := range want {
		var p, e int
		if err := checkDB.QueryRow(`SELECT COALESCE(isPersisted,0), COALESCE(isExplicitlyExported,0) FROM Playlist WHERE id=?`, id).Scan(&p, &e); err != nil {
			t.Fatalf("id %d: %v", id, err)
		}
		if p != exp[0] || e != exp[1] {
			t.Errorf("Playlist id=%d got (isPersisted=%d, isExplicitlyExported=%d), want (%d, %d)", id, p, e, exp[0], exp[1])
		}
	}

	// Verify Engine 5.3.0's exact persisted-playlist visibility query sees all 8 persisted rows.
	var persistedCount int
	if err := checkDB.QueryRow(`SELECT COUNT(*) FROM Playlist WHERE isPersisted = TRUE`).Scan(&persistedCount); err != nil {
		t.Fatal(err)
	}
	if persistedCount != 8 {
		t.Fatalf("SELECT COUNT(*) FROM Playlist WHERE isPersisted = TRUE => %d, want 8", persistedCount)
	}
	checkDB.Close()

	// Idempotent second mount pass must make zero further persistence edits.
	r2, err := HealMountDataOnly(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r2.PlaylistsPersisted != 0 {
		t.Fatalf("second pass PlaylistsPersisted=%d, want 0", r2.PlaylistsPersisted)
	}
}

func TestS15ZeroWritesOnHealthyLibrary(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	var localUUID string
	if err := db.QueryRow(`SELECT COALESCE(uuid,'') FROM Information LIMIT 1`).Scan(&localUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,isAnalyzed) VALUES(1,'../Music/a.mp3','a.mp3',4,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(1,'Folder',0,1,0,1700000000,0),(2,'Set',1,1,0,1700000000,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,2,1,?,0,0)`, localUUID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := HealMountDataOnly(mount, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.PlaylistsPersisted != 0 {
		t.Fatalf("healthy library modified PlaylistsPersisted=%d, want 0", r.PlaylistsPersisted)
	}
}
