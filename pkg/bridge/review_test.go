package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
)

func TestUppercasePDBAndSymlinkWatchRoot(t *testing.T) {
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
	link := filepath.Join(t.TempDir(), "media")
	if e := os.Symlink(root, link); e != nil {
		t.Fatal(e)
	}
	xs, e := bridge.Candidates(link)
	if e != nil || !reflect.DeepEqual(xs, []string{root}) {
		t.Fatal(xs, e)
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
	for _, swap := range []bool{false, true} {
		t.Run(map[bool]string{false: "move", true: "swap"}[swap], func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			convert(t, root, enginedb.Options{})
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
			r, e := enginedb.Sync(context.Background(), root, l, enginedb.Options{})
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
			r, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{})
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
	if n := scalar[int](t, db, `SELECT count(*) FROM sqlite_master WHERE name LIKE 'Bridge%'`); n != 0 {
		t.Fatal("schema committed despite rollback")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(root, "Engine Library/Database2"))) {
		t.Fatal("failed native import changed database bytes")
	}
}
