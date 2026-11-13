package enginedb

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

func nativePlaylistFixture(t *testing.T) (string, *rekordbox.Library, *sql.DB, int64) {
	t.Helper()
	root := t.TempDir()
	testfixture.Export(t, root)
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	return root, lib, db, scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Folder'`)
}

func playlistExec(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func playlistChain(t *testing.T, db *sql.DB, parent int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id,COALESCE(nextListId,0) FROM Playlist WHERE parentListId=?`, parent)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[int64]int64{}
	incoming := map[int64]bool{}
	for rows.Next() {
		var id, next int64
		if err = rows.Scan(&id, &next); err != nil {
			t.Fatal(err)
		}
		nodes[id] = next
		incoming[next] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var heads []int64
	for id := range nodes {
		if !incoming[id] {
			heads = append(heads, id)
		}
	}
	if len(heads) != 1 {
		t.Fatalf("parent %d heads %v, nodes %v", parent, heads, nodes)
	}
	seen := map[int64]bool{}
	var chain []int64
	for id := heads[0]; id != 0; id = nodes[id] {
		if _, ok := nodes[id]; !ok || seen[id] {
			t.Fatalf("broken chain at %d: %v", id, nodes)
		}
		seen[id] = true
		chain = append(chain, id)
	}
	if len(chain) != len(nodes) {
		t.Fatalf("unreachable siblings: %v / %v", chain, nodes)
	}
	return chain
}

func TestNativeChildSurvivesParentChanges(t *testing.T) {
	for _, mode := range []string{"rename", "remove"} {
		t.Run(mode, func(t *testing.T) {
			root, lib, db, folder := nativePlaylistFixture(t)
			id := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Keep',?,1,0)`, folder)
			playlistExec(t, db, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId) VALUES(?,3,(SELECT uuid FROM Information),0)`, id)
			if mode == "rename" {
				for i := range lib.Playlists {
					if lib.Playlists[i].ID == 10 {
						lib.Playlists[i].Name = "Renamed Folder"
					}
				}
			} else {
				lib.Playlists = nil
			}
			if _, err := Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
				t.Fatal(err)
			}
			if scalar[int64](t, db, `SELECT parentListId FROM Playlist WHERE id=?`, id) != 0 || !reflect.DeepEqual(playlistEntityTracks(t, db, id), []int64{3}) {
				t.Fatal("native data changed")
			}
			playlistChain(t, db, 0)
		})
	}
}

func assertNativePlaylist(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if parent := scalar[int64](t, db, `SELECT parentListId FROM Playlist WHERE id=?`, id); parent != 0 {
		t.Fatalf("native playlist %d parent=%d, want 0", id, parent)
	}
	if got := playlistEntityTracks(t, db, id); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("native playlist %d tracks overwritten: %v, want [3]", id, got)
	}
	if flag := scalar[int](t, db, `SELECT isExplicitlyExported FROM Playlist WHERE id=?`, id); flag != 1 {
		t.Fatalf("native export flag changed: %d", flag)
	}
	playlistChain(t, db, 0)
}

func TestExportedNativeChildCannotBeAdopted(t *testing.T) {
	root, lib, db, folder := nativePlaylistFixture(t)
	id := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES('Native Set',?,1,0,1)`, folder)
	playlistExec(t, db, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId) VALUES(?,3,(SELECT uuid FROM Information),0)`, id)
	for i := range lib.Playlists {
		if lib.Playlists[i].ID == 10 {
			lib.Playlists[i].Name = "Native Set"
		}
	}
	// Protection must survive another transaction, not only the initial promotion.
	for _, name := range []string{"First rebuild", "Second rebuild"} {
		for i := range lib.Playlists {
			if lib.Playlists[i].ID == 11 {
				lib.Playlists[i].Name = name
			}
		}
		if _, err := Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
			t.Fatal(err)
		}
		assertNativePlaylist(t, db, id)
		if imported := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Native Set (rekordbox)' AND parentListId=0`); imported == id {
			t.Fatal("native playlist claimed as imported folder")
		}
	}
	lib.Playlists = nil
	if _, err := Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	assertNativePlaylist(t, db, id)
}

func TestPreserveInterleavedNativeSiblingOrder(t *testing.T) {
	root, lib, db, folder := nativePlaylistFixture(t)
	owned := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Nested'`)
	second := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Second',?,1,0)`, folder)
	first := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('First',?,1,?)`, folder, owned)
	if got := playlistChain(t, db, folder); !reflect.DeepEqual(got, []int64{first, owned, second}) {
		t.Fatalf("fixture chain %v", got)
	}
	lib.Playlists[0].Name += " Renamed"
	if _, err := Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	var natives []int64
	for _, id := range playlistChain(t, db, 0) {
		if id == first || id == second {
			natives = append(natives, id)
		}
	}
	if !reflect.DeepEqual(natives, []int64{first, second}) {
		t.Fatalf("native relative order reversed: %v, want %v", natives, []int64{first, second})
	}
}

func TestRollbackOnFailurePreservesNativeChildren(t *testing.T) {
	for _, point := range []string{"second-reparent", "delete", "late"} {
		t.Run(point, func(t *testing.T) {
			root, lib, db, folder := nativePlaylistFixture(t)
			id := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Keep',?,1,0)`, folder)
			playlistExec(t, db, `UPDATE Playlist SET isExplicitlyExported=1 WHERE id=?`, id)
			second := playlistExec(t, db, `INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Keep Two',?,1,0)`, folder)
			playlistExec(t, db, `INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId) VALUES(?,3,(SELECT uuid FROM Information),0)`, id)
			var trigger string
			switch point {
			case "second-reparent":
				trigger = fmt.Sprintf(`CREATE TRIGGER playlist_fail BEFORE UPDATE OF parentListId ON Playlist WHEN OLD.id=%d AND (SELECT parentListId FROM Playlist WHERE id=%d)=0 BEGIN SELECT RAISE(ABORT,'playlist-injected'); END`, second, id)
			case "delete":
				trigger = fmt.Sprintf(`CREATE TRIGGER playlist_fail BEFORE DELETE ON Playlist WHEN OLD.id=%d AND (SELECT parentListId FROM Playlist WHERE id=%d)=0 BEGIN SELECT RAISE(ABORT,'playlist-injected'); END`, folder, id)
			case "late":
				trigger = fmt.Sprintf(`CREATE TRIGGER playlist_fail BEFORE UPDATE OF lastRekordBoxLibraryImportReadCounter ON Information WHEN (SELECT parentListId FROM Playlist WHERE id=%d)=0 BEGIN SELECT RAISE(ABORT,'playlist-injected'); END`, id)
			}
			playlistExec(t, db, trigger)
			dir := filepath.Join(root, "Engine Library/Database2")
			before := map[string][]byte{}
			for _, name := range []string{"m.db", stateFile} {
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = b
			}
			for i := range lib.Playlists {
				if lib.Playlists[i].ID == 10 {
					lib.Playlists[i].Name = "Keep"
				}
			}
			_, err := Sync(context.Background(), root, lib, Options{Fast: true})
			if err == nil || !strings.Contains(err.Error(), "playlist-injected") {
				t.Fatalf("injection not reached: %v", err)
			}
			for name, b := range before {
				after, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(b, after) {
					t.Errorf("%s changed after rollback", name)
				}
			}
			if !reflect.DeepEqual(playlistEntityTracks(t, db, id), []int64{3}) {
				t.Fatal("track changed")
			}
			playlistChain(t, db, folder)
			playlistExec(t, db, `DROP TRIGGER playlist_fail`)
			if _, err = Sync(context.Background(), root, lib, Options{Fast: true}); err != nil {
				t.Fatal(err)
			}
			assertNativePlaylist(t, db, id)
		})
	}
}
