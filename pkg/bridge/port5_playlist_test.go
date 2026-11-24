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
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func p5DB(t *testing.T, mount string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dbDir(mount), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func p5ListID(t *testing.T, db *sql.DB, title string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM Playlist WHERE title=?`, title).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func p5NativeList(t *testing.T, db *sql.DB, title string, parent int64, tracks []int64, exported bool) int64 {
	t.Helper()
	flag := 0
	if exported {
		flag = 1
	}
	res, err := db.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) VALUES(?,?,1,0,?)`, title, parent, flag)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var uuid string
	if err = db.QueryRow(`SELECT uuid FROM Information`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	for _, track := range tracks {
		if _, err = db.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(?,?,?,0,0)`, id, track, uuid); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func p5Members(t *testing.T, db *sql.DB, id int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT trackId FROM PlaylistEntity WHERE listId=? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var track int64
		if err := rows.Scan(&track); err != nil {
			t.Fatal(err)
		}
		out = append(out, track)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func p5Reexport(t *testing.T, mount string, pdb []byte) {
	t.Helper()
	path := filepath.Join(mount, "PIONEER/rekordbox/export.pdb")
	testfixture.Write(t, mount, "PIONEER/rekordbox/export.pdb", pdb)
	when := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func p5FolderGone(t *testing.T, mount string, list string) {
	t.Helper()
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, list)
	pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("GoneXX"), 1)
	p5Reexport(t, mount, pdb)
}

func p5AssertNative(t *testing.T, db *sql.DB, id int64, parent int64, title string, tracks []int64) {
	t.Helper()
	var gotParent int64
	var gotTitle string
	if err := db.QueryRow(`SELECT parentListId,title FROM Playlist WHERE id=?`, id).Scan(&gotParent, &gotTitle); err != nil {
		t.Fatal(err)
	}
	if gotParent != parent || gotTitle != title {
		t.Fatalf("native playlist %d = parent %d title %q, want parent %d title %q", id, gotParent, gotTitle, parent, title)
	}
	if got := p5Members(t, db, id); !reflect.DeepEqual(got, tracks) {
		t.Fatalf("native playlist %d members=%v, want %v", id, got, tracks)
	}
}

func TestPort5NativeSiblingChainSurvivesFolderRebuild(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	folder := p5ListID(t, db, "Folder")
	third := p5NativeList(t, db, "Native 3", folder, nil, false)
	second := p5NativeList(t, db, "Native 2", folder, nil, false)
	first := p5NativeList(t, db, "Native 1", folder, nil, false)
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=-id-100 WHERE parentListId=?`, folder); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=? WHERE id=?`, third, second); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=? WHERE id=?`, second, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=0 WHERE id=?`, third); err != nil {
		t.Fatal(err)
	}
	db.Close()
	p5FolderGone(t, mount, "New Set")
	mountNow(t, mount, MountOptions{})
	if got := query(t, mount, `SELECT parentListId FROM Playlist WHERE id IN (?,?,?) ORDER BY id`, first, second, third); len(got) != 3 || got[0][0] != "0" || got[1][0] != "0" || got[2][0] != "0" {
		t.Fatalf("native parents after rebuild: %v", got)
	}
	if got := query(t, mount, `SELECT nextListId FROM Playlist WHERE id=?`, first); got[0][0] != fmt.Sprint(second) {
		t.Fatalf("first next=%s, want %d", got[0][0], second)
	}
	if got := query(t, mount, `SELECT nextListId FROM Playlist WHERE id=?`, second); got[0][0] != fmt.Sprint(third) {
		t.Fatalf("second next=%s, want %d", got[0][0], third)
	}
}

func TestPort5NestedNativeHierarchySurvivesFolderRename(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	folder := p5ListID(t, db, "Folder")
	subfolder := p5NativeList(t, db, "Native Subfolder", folder, nil, false)
	playlist := p5NativeList(t, db, "Deep Playlist", subfolder, []int64{3}, false)
	db.Close()
	p5FolderGone(t, mount, "New Set")
	mountNow(t, mount, MountOptions{})
	db = p5DB(t, mount)
	p5AssertNative(t, db, subfolder, 0, "Native Subfolder", nil)
	p5AssertNative(t, db, playlist, subfolder, "Deep Playlist", []int64{3})
}

func TestPort5NativeChildParentChangesPreserveData(t *testing.T) {
	for _, mode := range []string{"rename", "remove"} {
		t.Run(mode, func(t *testing.T) {
			mount := stick(t)
			mountNow(t, mount, MountOptions{})
			db := p5DB(t, mount)
			folder := p5ListID(t, db, "Folder")
			id := p5NativeList(t, db, "Keep", folder, []int64{3}, false)
			db.Close()
			pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
			if mode == "rename" {
				pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("GoneXX"), 1)
			} else {
				pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("RootXX"), 1)
			}
			p5Reexport(t, mount, pdb)
			mountNow(t, mount, MountOptions{})
			db = p5DB(t, mount)
			p5AssertNative(t, db, id, 0, "Keep", []int64{3})
			if got := query(t, mount, `SELECT count(*) FROM pragma_foreign_key_check`); got[0][0] != "0" {
				t.Fatalf("foreign keys: %s", got[0][0])
			}
		})
	}
}

func TestPort5ExportedNativeChildIsNeverAdopted(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	folder := p5ListID(t, db, "Folder")
	id := p5NativeList(t, db, "RootXX", folder, []int64{3}, true)
	db.Close()
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("RootXX"), 1)
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	db = p5DB(t, mount)
	p5AssertNative(t, db, id, 0, "RootXX", []int64{3})
	if got := query(t, mount, `SELECT count(*) FROM Playlist WHERE title='RootXX (rekordbox)'`); got[0][0] != "1" {
		t.Fatalf("imported colliding folder count=%s", got[0][0])
	}
	pdb = bytes.Replace(pdb, utf16le("New Set"), utf16le("AgainXX"), 1)
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	p5AssertNative(t, db, id, 0, "RootXX", []int64{3})
}

func TestPort5NativeChildSurvivesLostBridgeState(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	folder := p5ListID(t, db, "Folder")
	id := p5NativeList(t, db, "RootXX", folder, []int64{3}, true)
	db.Close()
	p5FolderGone(t, mount, "New Set")
	mountNow(t, mount, MountOptions{})
	db = p5DB(t, mount)
	p5AssertNative(t, db, id, 0, "RootXX", []int64{3})
	db.Close()
	if err := os.Remove(filepath.Join(dbDir(mount), rbstate.SnapshotName)); err != nil {
		t.Fatal(err)
	}
	pdb := testfixture.PDBNamed([5]string{testfixture.UnicodeName, "two.flac", "three.wav", "four.wav", "five.wav"}, "New Set")
	pdb = bytes.Replace(pdb, utf16le("Folder"), utf16le("RootXX"), 1)
	p5Reexport(t, mount, pdb)
	mountNow(t, mount, MountOptions{})
	db = p5DB(t, mount)
	p5AssertNative(t, db, id, 0, "RootXX", []int64{3})
	if got := query(t, mount, `SELECT count(*) FROM Playlist WHERE title LIKE 'RootXX%rekordbox%'`); got[0][0] == "0" {
		t.Fatal("lost-state import did not create the separate rekordbox list")
	}
}

func TestPort5NativeSiblingRelativeOrderSurvivesRebuild(t *testing.T) {
	mount := stick(t)
	mountNow(t, mount, MountOptions{})
	db := p5DB(t, mount)
	folder := p5ListID(t, db, "Folder")
	owned := p5ListID(t, db, "Nested")
	second := p5NativeList(t, db, "Second", folder, nil, false)
	first := p5NativeList(t, db, "First", folder, nil, false)
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=? WHERE id=?`, owned, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=? WHERE id=?`, second, owned); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Playlist SET nextListId=0 WHERE id=?`, second); err != nil {
		t.Fatal(err)
	}
	db.Close()
	p5FolderGone(t, mount, "New Set")
	mountNow(t, mount, MountOptions{})
	lines := libraryTree(t, mount)
	var native []string
	for _, line := range lines {
		if strings.HasSuffix(line, "First") || strings.HasSuffix(line, "Second") {
			native = append(native, line)
		}
	}
	if !reflect.DeepEqual(native, []string{"First", "Second"}) {
		t.Fatalf("native relative order=%v", native)
	}
}

func TestPort5PlaylistRebuildRollsBackNativeChildren(t *testing.T) {
	for _, point := range []string{"second-reparent", "delete"} {
		t.Run(point, func(t *testing.T) {
			mount := stick(t)
			mountNow(t, mount, MountOptions{})
			db := p5DB(t, mount)
			folder := p5ListID(t, db, "Folder")
			id := p5NativeList(t, db, "Keep", folder, []int64{3}, true)
			second := p5NativeList(t, db, "Keep Two", folder, nil, false)
			var trigger string
			switch point {
			case "second-reparent":
				trigger = fmt.Sprintf(`CREATE TRIGGER p5_playlist_fail BEFORE UPDATE OF parentListId ON Playlist WHEN OLD.id=%d AND (SELECT parentListId FROM Playlist WHERE id=%d)=0 BEGIN SELECT RAISE(ABORT,'playlist-injected'); END`, second, id)
			case "delete":
				trigger = fmt.Sprintf(`CREATE TRIGGER p5_playlist_fail BEFORE DELETE ON Playlist WHEN OLD.id=%d AND (SELECT parentListId FROM Playlist WHERE id=%d)=0 BEGIN SELECT RAISE(ABORT,'playlist-injected'); END`, folder, id)
			}
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			beforeDB, err := os.ReadFile(filepath.Join(dbDir(mount), "m.db"))
			if err != nil {
				t.Fatal(err)
			}
			beforeSnap, err := os.ReadFile(filepath.Join(dbDir(mount), rbstate.SnapshotName))
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			p5FolderGone(t, mount, "New Set")
			if _, err := ConvertAtMount(context.Background(), mount, MountOptions{}); err == nil || !strings.Contains(err.Error(), "playlist-injected") {
				t.Fatalf("injected failure not reached: %v", err)
			}
			if after, _ := os.ReadFile(filepath.Join(dbDir(mount), "m.db")); !bytes.Equal(beforeDB, after) {
				t.Fatal("m.db changed after playlist rollback")
			}
			if after, _ := os.ReadFile(filepath.Join(dbDir(mount), rbstate.SnapshotName)); !bytes.Equal(beforeSnap, after) {
				t.Fatal("snapshot changed after playlist rollback")
			}
			db = p5DB(t, mount)
			if _, err := db.Exec(`DROP TRIGGER p5_playlist_fail`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			mountNow(t, mount, MountOptions{})
			db = p5DB(t, mount)
			p5AssertNative(t, db, id, 0, "Keep", []int64{3})
		})
	}
}
