package bridge_test

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/dbhealer"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
)

func TestDeletedSourceReleasesPathAndMembership(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	removed := l.Tracks[0].ID
	oldID := scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=?`, removed)
	native, err := db.Exec(`INSERT INTO Playlist(title,parentListId,nextListId,isPersisted) VALUES('Native refs',0,NULL,1)`)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := native.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId) VALUES(?,?,(SELECT uuid FROM Information),0),(?,?,'other-library',0)`, nativeID, oldID, nativeID, oldID); err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].ID = 100 // Deleted source path now belongs to a newly exported track.
	for i := range l.Playlists {
		for j, id := range l.Playlists[i].Tracks {
			if id == removed {
				l.Playlists[i].Tracks[j] = 100
			}
		}
	}
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT count(*) FROM Track WHERE id=?`, `SELECT count(*) FROM BridgeState WHERE trackId=?`, `SELECT count(*) FROM PlaylistEntity WHERE trackId=? AND (databaseUuid IS NULL OR databaseUuid='' OR databaseUuid=(SELECT uuid FROM Information))`} {
		if n := scalar[int](t, db, q, oldID); n != 0 {
			t.Fatalf("stale owned row: %s = %d", q, n)
		}
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity WHERE listId=? AND databaseUuid='other-library' AND trackId=?`, nativeID, oldID); n != 1 {
		t.Fatal("foreign-library membership was removed", n)
	}
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal("repeat import:", err)
	}
}

func TestMissingBridgePlaylistRebuildAndNullNativeTail(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	// Force ownership count to diverge without changing the source fingerprint.
	if _, err := db.Exec(`DELETE FROM Playlist WHERE id=(SELECT max(listId) FROM BridgePlaylist)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native',0,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	result := convert(t, root, enginedb.Options{})
	if !result.PlaylistsChanged {
		t.Fatal("missing owned playlist did not rebuild")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE nextListId IS NULL`); n != 0 {
		t.Fatal("NULL tail remains")
	}
}

func TestArtworkRejectsDirectoryAndOversizedFile(t *testing.T) {
	for _, kind := range []string{"directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			l, err := bridge.ReadLibrary(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(root, "bad-art")
			if kind == "directory" {
				err = os.Mkdir(p, 0755)
			} else {
				var f *os.File
				f, err = os.Create(p)
				if err == nil {
					err = f.Truncate((32 << 20) + 1)
					f.Close()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			l.Tracks[0].ArtworkPath = "/bad-art"
			if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err == nil {
				t.Fatal("accepted unsafe artwork")
			}
		})
	}
}

func TestDeletedSourceCleanupRollsBackOnBadAnalysis(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	removed := l.Tracks[0].ID
	l.Tracks[0].ID = 100
	for i := range l.Playlists {
		for j, id := range l.Playlists[i].Tracks {
			if id == removed {
				l.Playlists[i].Tracks[j] = 100
			}
		}
	}
	if err = os.WriteFile(filepath.Join(root, "PIONEER/USBANLZ/P001/ANLZ0000.DAT"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err == nil {
		t.Fatal("accepted corrupt analysis")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey=?`, removed); n != 1 {
		t.Fatal("deletion escaped rolled-back transaction")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM BridgeState`); n != 5 {
		t.Fatal("ownership lost on rollback", n)
	}
}

func TestHealedArtworkKeepsBridgeDeduplication(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	f, err := os.Create(filepath.Join(root, "large.png"))
	if err != nil {
		t.Fatal(err)
	}
	err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 1600, 800)))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].ArtworkPath = "/large.png"
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	report, err := dbhealer.Heal(db)
	if err != nil || report.Artwork != 1 {
		t.Fatal(report, err)
	}
	l.Tracks[0].Title = "Updated after healing"
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM AlbumArt`); n != 1 {
		t.Fatal("healed artwork broke source deduplication", n)
	}
}

func TestMultipleNullNativePlaylistTailsPreserveChains(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	if _, err := db.Exec(`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId) VALUES(900,'Native Folder',0,1,NULL),(1000,'Native A',900,1,1001),(1001,'Native B',900,1,NULL),(1002,'Native C',900,1,NULL),(1003,'Native D',900,1,0)`); err != nil {
		t.Fatal(err)
	}
	// Rebuild bridge playlists while keeping disconnected native chains.
	if _, err := db.Exec(`UPDATE BridgeLibrary SET value='invalidated' WHERE key='playlists'`); err != nil {
		t.Fatal(err)
	}
	convert(t, root, enginedb.Options{})
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE id>=1000 AND id<=1003`); n != 4 {
		t.Fatal("native playlists removed", n)
	}
	if n := scalar[int](t, db, `SELECT nextListId FROM Playlist WHERE id=1000`); n != 1001 {
		t.Fatal("existing native chain changed", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE nextListId IS NULL`); n != 0 {
		t.Fatal("NULL tails retained", n)
	}
}
