package bridge_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #301: an update too long for the mount runs after it on a copy of the live library. Engine
// keeps writing the live library meanwhile; a publish must not lose what it wrote.

// issue301Updated is a stick published from its export, whose update has run up to its publish.
func issue301Updated(t *testing.T, n int) (root string, ctx context.Context) {
	t.Helper()
	root, skeleton := issue301Stick(t, n)
	skeleton.Close()
	ctx = context.Background()
	restore := bridge.SetLargeExportForTest()
	defer restore()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	if _, err = bridge.ImportInBackground(ctx, root, nil); err != nil {
		t.Fatalf("build from the export: %v", err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background || !r.PlaylistsChanged {
		t.Fatalf("first publishing mount: %+v %v", r, err)
	}
	if r, err = bridge.ImportInBackground(ctx, root, nil); err != nil || r.Deferred != 0 {
		t.Fatalf("update: %+v %v", r, err)
	}
	return root, ctx
}

func TestIssue301UpdateCarriesWhatEngineWroteMeanwhile(t *testing.T) {
	root, ctx := issue301Updated(t, 20)
	live := dbOpen(t, root)
	id := scalar[int64](t, live, `SELECT id FROM Track WHERE pdbImportKey=3`)
	// While the update ran: a play, two tracks in the prepare list, the play history's place.
	for _, q := range []string{`UPDATE Track SET isPlayed=1,timeLastPlayed=1763370000 WHERE id=?1`,
		`INSERT INTO PreparelistEntity(trackId,trackNumber) VALUES(?1,1),(?1+1,2)`,
		`UPDATE Information SET currentPlayedIndiciator=4242`} {
		if _, err := live.Exec(q, id); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	live.Close()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Background || !r.PlaylistsChanged {
		t.Fatalf("mount publishing the update: %+v %v", r, err)
	}
	db := dbOpen(t, root)
	if got := scalar[int](t, db, `SELECT count(*) FROM Track WHERE id=? AND isPlayed=1 AND timeLastPlayed=1763370000`, id); got != 1 {
		t.Fatal("the play Engine recorded during the update was lost")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM PreparelistEntity WHERE trackId IN (?,?)`, id, id+1); got != 2 {
		t.Fatalf("prepare list: %d of 2 entries", got)
	}
	if got := scalar[int64](t, db, `SELECT currentPlayedIndiciator FROM Information`); got != 4242 {
		t.Fatalf("currentPlayedIndiciator %d", got)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL`); n != 19 {
		t.Fatalf("published %d tracks with their grid", n)
	}
	if got := scalar[string](t, db, `PRAGMA integrity_check`); got != "ok" {
		t.Fatal(got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM pragma_foreign_key_check`); got != 0 {
		t.Fatalf("%d foreign key violations", got)
	}
}

// A playlist Engine added during the update is a change the update cannot take: the live library
// stays as it is, and the next mount updates it again, in place.
func TestIssue301UpdateLeavesALibraryEngineChanged(t *testing.T) {
	root, ctx := issue301Updated(t, 20)
	live := dbOpen(t, root)
	if _, err := live.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Made on the deck',0,1,0)`); err != nil {
		t.Fatal(err)
	}
	live.Close()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || r.Background {
		t.Fatalf("mount with a conflicting update: %+v %v", r, err)
	}
	if _, err := os.Stat(stagePath(root)); !os.IsNotExist(err) {
		t.Fatalf("stage of the conflicting update left: %v", err)
	}
	db := dbOpen(t, root)
	if got := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE title='Made on the deck'`); got != 1 {
		t.Fatal("Engine's playlist lost")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL`); n != 0 {
		t.Fatalf("%d tracks of the conflicting update published", n)
	}
	db.Close()
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)}, false)
	if err != nil || r.Background || r.Updated == 0 {
		t.Fatalf("next mount: %+v %v", r, err)
	}
	db = dbOpen(t, root)
	if got := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE title='Made on the deck'`); got != 1 {
		t.Fatal("Engine's playlist lost by the next update")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey>0 AND beatData IS NOT NULL`); n != 19 {
		t.Fatalf("next mount: %d tracks with their grid", n)
	}
}
