package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// A stick whose Engine library rb-bridge never imported (no .rb-bridge-state.db) and whose
// export is large goes through the background import (review B2: the snapshot required the
// bridge state, and the import failed with ENOENT at every insert). The published library has
// the export's tracks and keeps the stick's own playlist.
func TestIssue301BackgroundImportOfANativeLibrary(t *testing.T) {
	root, db := issue301Stick(t, 20)
	if _, err := db.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native set',0,1,0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db")); !os.IsNotExist(err) {
		t.Fatalf("fixture has bridge state: %v", err)
	}
	restore := bridge.SetLargeExportForTest()
	defer restore()
	ctx := context.Background()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("mount: %+v %v", r, err)
	}
	if _, err = bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("background import of a library rb-bridge never imported: %v", err)
	}
	// Engine records a play of the stick's own track while the job ran (re-check of 2e227f88:
	// the merge then restored the native playlist's old sibling link, a UNIQUE conflict).
	edited := dbOpen(t, root)
	if _, err = edited.Exec(`UPDATE Track SET isPlayed=1 WHERE title='loaded on a deck'`); err != nil {
		t.Fatal(err)
	}
	edited.Close()
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.PlaylistsChanged {
		t.Fatalf("publishing mount: %+v %v", r, err)
	}
	live := dbOpen(t, root)
	defer live.Close()
	if got := scalar[int](t, live, `SELECT count(*) FROM Playlist WHERE title='Native set'`); got != 1 {
		t.Fatal("the stick's own playlist was lost")
	}
	if n := scalar[int](t, live, `SELECT count(*) FROM Track WHERE title='loaded on a deck' AND isPlayed=1`); n != 1 {
		t.Fatalf("the play Engine recorded during the import was lost: %d", n)
	}
	if n := scalar[int](t, live, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("%d foreign key violations", n)
	}
	// As in issue301Updated's library: the fixture's track 1 is a row Engine already has.
	if n := scalar[int](t, live, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL`); n != 19 {
		t.Fatalf("published %d of 19 export tracks with their grid", n)
	}
	if got := scalar[string](t, live, `PRAGMA integrity_check`); got != "ok" {
		t.Fatal(got)
	}
}
