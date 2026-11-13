package enginedb

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func dbOpen(t *testing.T, root string) *sql.DB {
	t.Helper()
	p := filepath.Join(root, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", p+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) T {
	t.Helper()
	var val T
	if err := db.QueryRow(query, args...).Scan(&val); err != nil {
		t.Fatalf("scalar %q: %v", query, err)
	}
	return val
}

func playlistEntityTracks(t *testing.T, db *sql.DB, listID int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT trackId FROM PlaylistEntity WHERE listId=? ORDER BY id`, listID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// BH2-A4-3 (#252): Transactionally reparent native children before rebuilding owned folders.
func TestPreserveNativeChildPlaylistOnFolderRebuild(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Fast: true}
	r1, err := Sync(context.Background(), root, lib, opts)
	if err != nil || !r1.PlaylistsChanged {
		t.Fatalf("initial sync: %+v %v", r1, err)
	}

	engine := dbOpen(t, root)
	folderID := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)
	var dbUUID string
	if err = engine.QueryRow(`SELECT uuid FROM Information`).Scan(&dbUUID); err != nil {
		t.Fatal(err)
	}

	// Insert native child playlist under 'Folder'.
	res, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('My Native Set',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	// Add track entities to native child.
	if _, err = engine.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,1,?,0,0),(?,2,?,0,0)`, nativeID, dbUUID, nativeID, dbUUID); err != nil {
		t.Fatal(err)
	}

	// Trigger folder rebuild by altering an imported playlist's name.
	lib.Playlists[0].Name += " Renamed"
	r2, err := Sync(context.Background(), root, lib, opts)
	if err != nil {
		t.Fatalf("Sync with native child failed (should reparent, not refuse): %v", err)
	}
	if !r2.PlaylistsChanged {
		t.Fatalf("expected PlaylistsChanged=true on rebuild: %+v", r2)
	}

	// Verify native child survived with its original ID.
	if count := scalar[int](t, engine, `SELECT count(*) FROM Playlist WHERE id=?`, nativeID); count != 1 {
		t.Fatalf("native child id %d lost after rebuild", nativeID)
	}

	// Verify native child has been reparented to root (parentListId=0).
	parent := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, nativeID)
	if parent != 0 {
		t.Fatalf("native child parentListId=%d, expected 0", parent)
	}

	title := scalar[string](t, engine, `SELECT title FROM Playlist WHERE id=?`, nativeID)
	if title != "My Native Set" {
		t.Fatalf("native child title=%q, expected 'My Native Set'", title)
	}

	// Verify tracks in native child were preserved.
	tracks := playlistEntityTracks(t, engine, nativeID)
	if !reflect.DeepEqual(tracks, []int64{1, 2}) {
		t.Fatalf("tracks in native child = %v, expected [1, 2]", tracks)
	}

	// Verify foreign keys.
	if fk := scalar[int](t, engine, `SELECT count(*) FROM pragma_foreign_key_check`); fk != 0 {
		t.Fatalf("foreign key check failed: %d errors", fk)
	}
}

func TestPreserveNativeSiblingChainOnFolderRebuild(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Fast: true}
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatal(err)
	}

	engine := dbOpen(t, root)
	folderID := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)

	// Insert three native children forming a sibling chain under Folder: N1 -> N2 -> N3 -> 0.
	r3, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native 3',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	id3, _ := r3.LastInsertId()

	r2, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native 2',?,1,?)`, folderID, id3)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := r2.LastInsertId()

	r1, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native 1',?,1,?)`, folderID, id2)
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := r1.LastInsertId()

	// Trigger folder rebuild.
	lib.Playlists[0].Name += " Modified"
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify all three reparented to root.
	for _, id := range []int64{id1, id2, id3} {
		p := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, id)
		if p != 0 {
			t.Fatalf("playlist %d parentListId=%d, expected 0", id, p)
		}
	}

	// Verify sibling chain preserved: id1 -> id2 -> id3.
	next1 := scalar[int64](t, engine, `SELECT nextListId FROM Playlist WHERE id=?`, id1)
	if next1 != id2 {
		t.Fatalf("id1 nextListId=%d, expected id2 (%d)", next1, id2)
	}
	next2 := scalar[int64](t, engine, `SELECT nextListId FROM Playlist WHERE id=?`, id2)
	if next2 != id3 {
		t.Fatalf("id2 nextListId=%d, expected id3 (%d)", next2, id3)
	}

	if fk := scalar[int](t, engine, `SELECT count(*) FROM pragma_foreign_key_check`); fk != 0 {
		t.Fatalf("foreign key check failed: %d errors", fk)
	}
}

func TestResolveRootNameCollisionOnReparent(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Fast: true}
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatal(err)
	}

	engine := dbOpen(t, root)
	folderID := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)

	// Create root playlist named 'Colliding'.
	resRoot, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Colliding',0,1,0)`)
	if err != nil {
		t.Fatal(err)
	}
	rootPlaylistID, _ := resRoot.LastInsertId()

	// Create child under Folder also named 'Colliding'.
	resChild, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Colliding',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := resChild.LastInsertId()

	// Trigger rebuild.
	lib.Playlists[0].Name += " Alt"
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Verify original root playlist kept its name.
	tRoot := scalar[string](t, engine, `SELECT title FROM Playlist WHERE id=?`, rootPlaylistID)
	if tRoot != "Colliding" {
		t.Fatalf("root playlist renamed: got %q, expected 'Colliding'", tRoot)
	}

	// Verify reparented child received suffixed name 'Colliding (1)'.
	tChild := scalar[string](t, engine, `SELECT title FROM Playlist WHERE id=?`, childID)
	if tChild != "Colliding (1)" {
		t.Fatalf("reparented child title=%q, expected 'Colliding (1)'", tChild)
	}

	// Verify both are at root.
	if p := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, childID); p != 0 {
		t.Fatalf("child parentListId=%d, expected 0", p)
	}

	if fk := scalar[int](t, engine, `SELECT count(*) FROM pragma_foreign_key_check`); fk != 0 {
		t.Fatalf("foreign key check failed: %d errors", fk)
	}
}

func TestPreserveNestedNativeHierarchy(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Fast: true}
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatal(err)
	}

	engine := dbOpen(t, root)
	folderID := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)
	var dbUUID string
	if err = engine.QueryRow(`SELECT uuid FROM Information`).Scan(&dbUUID); err != nil {
		t.Fatal(err)
	}

	// Create native folder inside imported Folder.
	resNF, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native Subfolder',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	nfID, _ := resNF.LastInsertId()

	// Create native playlist inside Native Subfolder.
	resNP, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Deep Playlist',?,1,0)`, nfID)
	if err != nil {
		t.Fatal(err)
	}
	npID, _ := resNP.LastInsertId()

	// Add track entity to Deep Playlist.
	if _, err = engine.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,3,?,0,0)`, npID, dbUUID); err != nil {
		t.Fatal(err)
	}

	// Trigger rebuild.
	lib.Playlists[0].Name += " Updated"
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// NF must be reparented to root.
	if p := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, nfID); p != 0 {
		t.Fatalf("Native Subfolder parentListId=%d, expected 0", p)
	}

	// NP must remain under NF.
	if p := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, npID); p != nfID {
		t.Fatalf("Deep Playlist parentListId=%d, expected %d", p, nfID)
	}

	// Track in NP must be intact.
	tracks := playlistEntityTracks(t, engine, npID)
	if !reflect.DeepEqual(tracks, []int64{3}) {
		t.Fatalf("tracks in Deep Playlist = %v, expected [3]", tracks)
	}

	if fk := scalar[int](t, engine, `SELECT count(*) FROM pragma_foreign_key_check`); fk != 0 {
		t.Fatalf("foreign key check failed: %d errors", fk)
	}
}

func TestRollbackOnFailurePreservesNativeChildren(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Fast: true}
	if _, err = Sync(context.Background(), root, lib, opts); err != nil {
		t.Fatal(err)
	}

	engine := dbOpen(t, root)
	folderID := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)

	res, err := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Rollback Test',?,1,0)`, folderID)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, _ := res.LastInsertId()

	// Alter library name to trigger rebuild, but pass an already-cancelled context so Sync fails.
	lib.Playlists[0].Name += " Fail"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = Sync(ctx, root, lib, opts)
	if err == nil {
		t.Fatal("expected Sync to fail with cancelled context")
	}

	// Native child must still be under original folderID, not modified or deleted.
	parent := scalar[int64](t, engine, `SELECT parentListId FROM Playlist WHERE id=?`, nativeID)
	if parent != folderID {
		t.Fatalf("after rollback, parentListId=%d, expected original %d", parent, folderID)
	}
}
