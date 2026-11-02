package bridge_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testfixture"
	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
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
	if _, e = db.Exec(`DELETE FROM BridgePlaylist`); e != nil {
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
