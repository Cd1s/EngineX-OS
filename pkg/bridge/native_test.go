package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

func TestNativeTrackConflictIsReferencedNotAdopted(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	testfixture.Write(t, root, "MUSIC/native.wav", testfixture.WAV(true))
	if _, e := db.Exec(`INSERT INTO Track(path,title) VALUES('../MUSIC/native.wav','Native title')`); e != nil {
		t.Fatal(e)
	}
	id := scalar[int64](t, db, `SELECT id FROM Track WHERE title='Native title'`)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	l.Tracks = append(l.Tracks, rekordbox.Track{ID: 99, Title: "must not replace native", Path: "/MUSIC/native.wav"})
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); e != nil {
		t.Fatal("native path conflict must be skipped, not fatal:", e)
	}
	if got := scalar[string](t, db, `SELECT title FROM Track WHERE id=?`, id); got != "Native title" {
		t.Fatal(got)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM BridgeState WHERE trackId=?`, id); n != 0 {
		t.Fatal("native row now owned")
	}
}

func TestArtworkDeduplicatesWithoutSourceChanges(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	testfixture.Write(t, root, "PIONEER/Artwork/é.jpg", []byte{0xff, 0xd8, 0xff, 0xd9})
	before := snapshot(t, root)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	l.Tracks[0].ArtworkPath = "/PIONEER/Artwork/é.jpg"
	l.Tracks[1].ArtworkPath = l.Tracks[0].ArtworkPath
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); e != nil {
		t.Fatal(e)
	}
	db := dbOpen(t, root)
	if n := scalar[int](t, db, `SELECT count(*) FROM AlbumArt`); n != 1 {
		t.Fatal("art not deduplicated", n)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE albumArtId=(SELECT id FROM AlbumArt)`); n != 2 {
		t.Fatal("art foreign keys", n)
	}
	after := snapshot(t, root)
	for p, h := range before {
		if after[p] != h {
			t.Fatal("source changed", p)
		}
	}
}

func TestSourcePathAliasDoesNotOverwriteCues(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	before := scalar[string](t, db, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=1`)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	duplicate := l.Tracks[0]
	duplicate.ID = 99
	duplicate.Title = "duplicate source record"
	l.Tracks = append(l.Tracks, duplicate)
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); e == nil {
		t.Fatal("two source records silently share a Track row")
	}
	if after := scalar[string](t, db, `SELECT hex(quickCues) FROM PerformanceData WHERE trackId=1`); after != before {
		t.Fatal("alias replaced source cues")
	}
}

func TestOneLibraryWALIsNeverTouched(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "PIONEER/rekordbox")
	if e := os.MkdirAll(dir, 0755); e != nil {
		t.Fatal(e)
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "exportLibrary.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`PRAGMA journal_mode=WAL;CREATE TABLE content(id INTEGER,path TEXT);INSERT INTO content VALUES(1,'/MUSIC/a.wav')`); e != nil {
		t.Fatal(e)
	}
	before := snapshot(t, root)
	if _, e = bridge.ReadLibrary(context.Background(), root); e == nil {
		t.Fatal("uncheckpointed WAL snapshot silently read")
	}
	if !reflect.DeepEqual(before, snapshot(t, root)) {
		t.Fatal("read-only source WAL/SHM was modified")
	}
}

func TestPlaylistNameConflictGetsSuffix(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	convert(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil || len(l.Playlists) == 0 {
		t.Fatal(e)
	}
	if _, e = db.Exec(`DELETE FROM BridgePlaylist; UPDATE main.Playlist SET isExplicitlyExported=0`); e != nil {
		t.Fatal(e)
	}
	// The previously imported rows are now native playlists with the same names.
	if _, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{}); e != nil {
		t.Fatal("playlist name conflict must not abort the import:", e)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM Playlist WHERE title LIKE '% (rekordbox)'`); n == 0 {
		t.Fatal("conflicting playlist not renamed")
	}
}

func TestConvertSurvivesDuplicateRecordsAndMissingFiles(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	base, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	if len(base.Tracks) == 0 {
		t.Fatal("empty fixture")
	}
	resolver, e := compat.NewResolver(root)
	if e != nil {
		t.Fatal(e)
	}
	duplicate, gone := base.Tracks[0], base.Tracks[0]
	duplicate.ID, gone.ID = 99, 98
	gone.Path = "/Contents/not-on-this-stick.flac"
	l := &rekordbox.Library{Tracks: append(slices.Clone(base.Tracks), duplicate, gone), Playlists: []rekordbox.Playlist{{ID: 1, Name: "Mix", Tracks: []uint32{98, 99, base.Tracks[0].ID}}}}
	bridge.Reconcile(resolver, l)
	if len(l.Tracks) != len(base.Tracks) {
		t.Fatalf("%d tracks", len(l.Tracks))
	}
	if got := l.Playlists[0].Tracks; len(got) != 2 || got[0] != base.Tracks[0].ID || got[1] != base.Tracks[0].ID {
		t.Fatalf("%v", got)
	}
}

func TestMountImportSkipsAnUnchangedExport(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	first, e := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if e != nil || first.Bypassed || first.Added == 0 {
		t.Fatalf("%+v %v", first, e)
	}
	again, e := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if e != nil || !again.Bypassed {
		t.Fatalf("not skipped: %+v %v", again, e)
	}
	forced, e := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, true)
	if e != nil || forced.Bypassed {
		t.Fatalf("--full skipped: %+v %v", forced, e)
	}
	// A database that has gone missing is rebuilt despite the stamp.
	if e = os.Remove(filepath.Join(root, "Engine Library/Database2/m.db")); e != nil {
		t.Fatal(e)
	}
	rebuilt, e := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if e != nil || rebuilt.Bypassed {
		t.Fatalf("%+v %v", rebuilt, e)
	}
}

func TestFastImportReadsOnlyTheExport(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	// Every other file on the volume is gone, and the import must not notice.
	for _, p := range []string{"Contents", "MUSIC", "PIONEER/USBANLZ"} {
		os.RemoveAll(filepath.Join(root, p))
	}
	dir := t.TempDir()
	r, e := enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true, DatabaseDir: dir})
	if e != nil || r.Added != len(l.Tracks) {
		t.Fatalf("%+v %v", r, e)
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var tracks, analysed int
	db.QueryRow(`SELECT count(*) FROM Track`).Scan(&tracks)
	db.QueryRow(`SELECT count(*) FROM PerformanceData WHERE quickCues IS NOT NULL OR beatData IS NOT NULL`).Scan(&analysed)
	if tracks != len(l.Tracks) || analysed != 0 {
		t.Fatalf("%d tracks, %d with performance data", tracks, analysed)
	}
	// A later fast pass leaves imported tracks alone.
	r, e = enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true, DatabaseDir: dir})
	if e != nil || r.Unchanged != len(l.Tracks) || r.Added != 0 {
		t.Fatalf("%+v %v", r, e)
	}
}
