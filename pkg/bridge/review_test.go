package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestUppercasePDB(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	old := filepath.Join(root, "PIONEER/rekordbox")
	upper := filepath.Join(root, "PIONEER/REKORDBOX")
	if e := os.Rename(old, upper); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(filepath.Join(upper, "export.pdb"), filepath.Join(upper, "EXPORT.PDB")); e != nil {
		t.Fatal(e)
	}
	if r := convert(t, root, enginedb.Options{}); r.Added != 5 {
		t.Fatal(r)
	}
}

func TestSmartMatchUsesCachedAnalysisBPM(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	l.Tracks[0].BPM = 0
	opts := enginedb.Options{SmartMatch: true, CurrentTrack: 1}
	r, e := enginedb.Sync(context.Background(), root, l, opts)
	if e != nil {
		t.Fatal(e)
	}
	db := dbOpen(t, root)
	id := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='[Smart Match]'`)
	if got := entityChain(t, db, id); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatal("analysis BPM not used", got)
	}
	r, e = enginedb.Sync(context.Background(), root, l, opts)
	if e != nil || r.Unchanged != 5 || r.PlaylistsChanged {
		t.Fatal("cached BPM unstable", r, e)
	}
	if l.Tracks[0].BPM != 0 {
		t.Fatal("inferred metadata changed source fingerprint")
	}
}

func TestMoveAndSwapPathsRetainEngineIDs(t *testing.T) {
	// #4: the fast (mount-time) import must finish staged moves too, after a
	// full import and after a fast one.
	for _, c := range []struct {
		name        string
		swap        bool
		first, then enginedb.Options
	}{
		{"move", false, enginedb.Options{}, enginedb.Options{}},
		{"swap", true, enginedb.Options{}, enginedb.Options{}},
		{"full-to-fast move", false, enginedb.Options{}, enginedb.Options{Fast: true}},
		{"fast-to-fast swap", true, enginedb.Options{Fast: true}, enginedb.Options{Fast: true}},
	} {
		swap := c.swap
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			convert(t, root, c.first)
			db := dbOpen(t, root)
			l, e := bridge.ReadLibrary(context.Background(), root)
			if e != nil {
				t.Fatal(e)
			}
			updated := 1
			if swap {
				l.Tracks[2].Path = "/MUSIC/four.wav"
				l.Tracks[3].Path = "/MUSIC/three.wav"
				updated = 2
			} else {
				if e = os.Rename(filepath.Join(root, "MUSIC/three.wav"), filepath.Join(root, "MUSIC/renamed.wav")); e != nil {
					t.Fatal(e)
				}
				l.Tracks[2].Path = "/MUSIC/renamed.wav"
			}
			r, e := enginedb.Sync(context.Background(), root, l, c.then)
			if e != nil {
				t.Fatal(e)
			}
			if r.Added != 0 || r.Updated != updated || scalar[int](t, db, `SELECT count(*) FROM Track`) != 5 {
				t.Fatal("move created a ghost Track", r)
			}
			if id := scalar[int64](t, db, `SELECT id FROM Track WHERE pdbImportKey=3`); id != 3 {
				t.Fatal("identity changed", id)
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE path LIKE '.rb-bridge-pending-%'`); n != 0 {
				t.Fatal("temporary path committed")
			}
			r, e = enginedb.Sync(context.Background(), root, l, c.then)
			if e != nil || r.Unchanged != 5 || r.PlaylistsChanged {
				t.Fatal("move not stable", r, e)
			}
		})
	}
}

func TestFailedImportRollsBackBridgeSchema(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	// Existing tracks are now native; they are referenced, and the import then
	// fails late while writing playlist entries.
	if _, e := db.Exec(`DROP TABLE BridgeState;DROP TABLE BridgeAnalysis;DROP TABLE BridgePlaylist;DROP TABLE BridgeLibrary;
CREATE TRIGGER fail_import BEFORE INSERT ON PlaylistEntity BEGIN SELECT RAISE(ABORT,'injected'); END;`); e != nil {
		t.Fatal(e)
	}
	before := snapshot(t, filepath.Join(root, "Engine Library/Database2"))
	if _, e := bridge.Convert(context.Background(), root, enginedb.Options{}); e == nil {
		t.Fatal("injected failure ignored")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey IS NOT NULL AND id IN (SELECT trackId FROM PerformanceData WHERE thirdPartySourceId=1)`); n == 0 {
		t.Fatal("fixture lost its tracks")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM bridge.sqlite_master WHERE name LIKE 'Bridge%'`); n != 0 {
		t.Fatal("schema committed despite rollback")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(root, "Engine Library/Database2"))) {
		t.Fatal("failed native import changed database bytes")
	}
}

// Engine drops unknown tables from m.db on attach, then resets again on every
// attach if they return, so m.db must stay free of them across repeat imports.
func TestEngineSchemaResetKeepsBridgeState(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM main.sqlite_master WHERE name LIKE 'Bridge%'`); n != 0 {
		t.Fatal("bridge tables in m.db")
	}
	if v := scalar[int](t, db, `PRAGMA main.user_version`); v != 4194305 {
		t.Fatal("user_version", v)
	}
	if v := scalar[int64](t, db, `SELECT currentPlayedIndiciator FROM main.Information`); v <= 0 {
		t.Fatal("played indicator", v)
	}
	playlists := scalar[int](t, db, `SELECT count(*) FROM main.Playlist`)
	result := convert(t, root, enginedb.Options{})
	if result.PlaylistsChanged || result.Added != 0 || result.Updated != 0 {
		t.Fatalf("repeat import rewrote the library: %+v", result)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM main.Playlist`); n != playlists {
		t.Fatalf("playlists grew from %d to %d", playlists, n)
	}
}

// Engine may recreate imported playlists under new ids. The next import must
// refill them instead of adding "(rekordbox)" copies.
func TestRebuiltPlaylistIsAdoptedNotDuplicated(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	before := scalar[int](t, db, `SELECT count(*) FROM main.Playlist`)
	entities := scalar[int](t, db, `SELECT count(*) FROM main.PlaylistEntity`)
	if _, e := db.Exec(`DELETE FROM main.PlaylistEntity;
CREATE TEMP TABLE saved AS SELECT title,parentListId FROM main.Playlist WHERE parentListId=0;
DELETE FROM main.Playlist WHERE parentListId<>0;
DELETE FROM main.Playlist;
INSERT INTO main.Playlist(title,parentListId,isPersisted,nextListId,isExplicitlyExported) SELECT title,parentListId,1,0,1 FROM saved;
DROP TABLE saved;`); e != nil {
		t.Fatal(e)
	}
	if !convert(t, root, enginedb.Options{}).PlaylistsChanged {
		t.Fatal("orphaned playlists were not rebuilt")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM main.Playlist WHERE title LIKE '%(rekordbox%'`); n != 0 {
		t.Fatalf("%d duplicate playlists", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM main.Playlist`); n != before {
		t.Fatalf("playlists %d, want %d", n, before)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM main.PlaylistEntity`); n != entities {
		t.Fatalf("entities %d, want %d", n, entities)
	}
}

// #5: an interrupted publish is completed by the next run, so m.db and the
// bridge state never come from different imports; stale sidecars are dropped.
func TestInterruptedFastPublishIsCompleted(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".rb-bridge-publish")
	os.Mkdir(stage, 0o755)
	os.WriteFile(filepath.Join(stage, "m.db"), []byte("new db"), 0o644)
	os.WriteFile(filepath.Join(dir, ".rb-bridge-state.db"), []byte("new state"), 0o644) // already moved
	os.WriteFile(filepath.Join(dir, "m.db"), []byte("old db"), 0o644)
	os.WriteFile(filepath.Join(dir, "m.db-wal"), []byte("old wal"), 0o644)
	os.WriteFile(filepath.Join(stage, "READY"), nil, 0o644)
	if err := bridge.FinishPublishForTest(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "m.db")); string(b) != "new db" {
		t.Fatalf("m.db = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "m.db-wal")); !os.IsNotExist(err) {
		t.Fatal("old WAL kept beside the new database")
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stage left behind")
	}
	// A stage that never became ready is discarded, not published.
	os.Mkdir(stage, 0o755)
	os.WriteFile(filepath.Join(stage, "m.db"), []byte("half"), 0o644)
	if err := bridge.FinishPublishForTest(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "m.db")); string(b) != "new db" {
		t.Fatalf("unready stage published: %q", b)
	}
}

// #6: the fast import stores the spelling on the volume, not the export's.
func TestFastImportResolvesTheVolumeSpelling(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if err := os.Rename(filepath.Join(root, "MUSIC/three.wav"), filepath.Join(root, "MUSIC/Three.WAV")); err != nil {
		t.Fatal(err)
	}
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true}); e != nil {
		t.Fatal(e)
	}
	db := dbOpen(t, root)
	if p := scalar[string](t, db, `SELECT path FROM Track WHERE pdbImportKey=3`); p != "../MUSIC/Three.WAV" {
		t.Fatalf("stored %q", p)
	}
}
