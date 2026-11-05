package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #59: a library staged from a Pioneer player lands under its own root folder,
// plays from the staged copy, and never breaks the drive's own import.
func TestStagedProlinkLibraryImportsUnderItsFolder(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	name := "CDJ-3000 2 USB"
	staged := filepath.Join(root, ".prolink-cache", name)
	testfixture.Export(t, staged)
	if err := os.Remove(filepath.Join(staged, "MUSIC", "five.wav")); err != nil { // not copied yet
		t.Fatal(err)
	}
	broken := filepath.Join(root, ".prolink-cache", "Broken 3 SD")
	testfixture.Write(t, broken, "PIONEER/rekordbox/export.pdb", []byte("not a pdb"))

	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Added != 5+4 {
		t.Fatalf("own 5 + staged 4 tracks: %+v", r)
	}
	db := dbOpen(t, root)
	folder := scalar[int](t, db, `SELECT id FROM Playlist WHERE parentListId=0 AND title=?`, "[Pro DJ Link: "+name+"]")
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE parentListId=?`, folder); n == 0 {
		t.Fatal("staged playlists not under the folder")
	}
	path := scalar[string](t, db, `SELECT path FROM Track WHERE path LIKE '%two.flac' AND path LIKE '%.prolink-cache%'`)
	if path != "../.prolink-cache/"+name+"/MUSIC/two.flac" {
		t.Fatalf("staged path %q", path)
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library", path)); err != nil {
		t.Fatal("Engine-relative path cannot open the staged audio", err)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE path LIKE '%.prolink-cache%five.wav'`); n != 0 {
		t.Fatal("a track without staged audio was imported")
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE path NOT LIKE '%.prolink-cache%'`); n != 5 {
		t.Fatalf("own tracks: %d", n)
	}
	db.Close()

	// Unchanged: skipped. A restaged library (new export.pdb) imports again.
	if r, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("unchanged: %+v %v", r, err)
	}
	testfixture.Write(t, staged, "MUSIC/five.wav", testfixture.WAV(false))
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(staged, "PIONEER", "rekordbox", "export.pdb"), later, later); err != nil {
		t.Fatal(err)
	}
	if r, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true); err != nil || r.Bypassed || r.Added != 1 {
		t.Fatalf("restaged: %+v %v", r, err)
	}
}

func TestStagedProlinkWithoutLocalRekordboxExport(t *testing.T) {
	for _, mode := range []string{"empty", "pioneer-directory", "native-engine"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			if mode == "pioneer-directory" {
				if err := os.MkdirAll(filepath.Join(root, "PIONEER"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "native-engine" {
				if _, err := enginedb.Sync(ctx, root, &rekordbox.Library{}, enginedb.Options{}); err != nil {
					t.Fatal(err)
				}
				db := dbOpen(t, root)
				if _, err := db.Exec(`INSERT INTO Track(path,title) VALUES('../MUSIC/native.wav','Native preserved')`); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			staged := filepath.Join(root, ".prolink-cache", "CDJ-3000")
			testfixture.Export(t, staged)
			l, err := bridge.ReadLibrary(ctx, root)
			if err != nil || len(l.Tracks) != 5 {
				t.Fatal("staged-only read", err)
			}
			r, err := bridge.Convert(ctx, root, enginedb.Options{})
			if err != nil || r.Bypassed || r.Added != 5 {
				t.Fatalf("staged-only convert: %+v %v", r, err)
			}
			db := dbOpen(t, root)
			folder := scalar[int](t, db, `SELECT id FROM Playlist WHERE parentListId=0 AND title='[Pro DJ Link: CDJ-3000]'`)
			if scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE parentListId=?`, folder) == 0 {
				t.Fatal("missing playlists")
			}
			if scalar[int](t, db, `SELECT count(*) FROM Track WHERE path LIKE '%.prolink-cache/CDJ-3000/%'`) != 5 {
				t.Fatal("missing playable staged tracks")
			}
			if mode == "native-engine" && scalar[int](t, db, `SELECT count(*) FROM Track WHERE title='Native preserved'`) != 1 {
				t.Fatal("native Engine track lost")
			}
			db.Close()
			if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || r.Added != 0 {
				t.Fatalf("repeat staged-only import: %+v %v", r, err)
			}
			if _, err = os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); !os.IsNotExist(err) {
				t.Fatal("created a fake local Rekordbox export")
			}
		})
	}
}

func TestStagedOnlyRetainsPreviouslyImportedLocalSource(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	staged := filepath.Join(root, ".prolink-cache", "CDJ-3000")
	testfixture.Export(t, staged)
	if r, err := bridge.Convert(context.Background(), root, enginedb.Options{SmartMatch: true, CurrentTrack: 1}); err != nil || r.Added != 10 {
		t.Fatalf("initial %+v %v", r, err)
	}
	db := dbOpen(t, root)
	localTrack := scalar[int64](t, db, `SELECT id FROM Track WHERE path='../MUSIC/two.flac'`)
	localList := scalar[int64](t, db, `SELECT listId FROM BridgePlaylist WHERE sourceId=12`)
	oldMembers := scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity WHERE listId=?`, localList)
	db.Close()
	if err := os.Rename(filepath.Join(root, "PIONEER"), filepath.Join(root, "PIONEER.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(staged, "MUSIC/five.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Convert(context.Background(), root, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	db = dbOpen(t, root)
	if scalar[int64](t, db, `SELECT id FROM Track WHERE path='../MUSIC/two.flac'`) != localTrack {
		t.Fatal("prior local track deleted or replaced")
	}
	if scalar[int64](t, db, `SELECT listId FROM BridgePlaylist WHERE sourceId=12`) != localList || scalar[int](t, db, `SELECT count(*) FROM PlaylistEntity WHERE listId=?`, localList) != oldMembers {
		t.Fatal("prior local playlist rebuilt/deleted")
	}
	if scalar[int](t, db, `SELECT count(*) FROM Track WHERE path NOT LIKE '%.prolink-cache/%'`) != 5 || scalar[int](t, db, `SELECT count(*) FROM Track WHERE path LIKE '%.prolink-cache/%'`) != 4 {
		t.Fatal("staged pruning changed unrelated local source")
	}
	db.Close()
	// An explicit staged Smart Match replaces its own recommendation list while
	// retaining the absent local source's ordinary playlists and tracks.
	for range 2 {
		if _, err := bridge.Convert(context.Background(), root, enginedb.Options{SmartMatch: true, CurrentTrack: 1<<24 | 1}); err != nil {
			t.Fatal(err)
		}
	}
	db = dbOpen(t, root)
	if scalar[int64](t, db, `SELECT listId FROM BridgePlaylist WHERE sourceId=12`) != localList {
		t.Fatal("Smart Match removed unrelated local playlist")
	}
}

func TestMalformedLocalExportDoesNotFallBackToStaged(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, filepath.Join(root, ".prolink-cache", "CDJ-3000"))
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("broken local source"))
	if _, err := bridge.ReadLibrary(context.Background(), root); err == nil {
		t.Fatal("malformed local export ignored")
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatal("read created an Engine library")
	}
}
