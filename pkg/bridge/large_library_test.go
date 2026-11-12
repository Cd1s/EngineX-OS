package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// BH-GC1-2: a library over 500 tracks was imported from the export alone at
// the first mount and stamped as imported, so its hot cues, memory cues,
// loops, beat grids and waveforms were never read on any later mount.
func TestLargeLibraryGetsItsCuesLoopsAndGridOnTheNextMount(t *testing.T) {
	const n = largeTracks
	root := largeExport(t)
	stamp := filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")
	ctx := context.Background()

	// First mount: playlists and tracks at once (#182), from the export alone.
	r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
	if err != nil || r.Added != n || !r.PlaylistsChanged {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	engine := dbOpen(t, root)
	if got := scalar[int](t, engine, `SELECT count(*) FROM PlaylistEntity e JOIN Playlist p ON p.id=e.listId WHERE p.title='Big Set'`); got != n {
		t.Fatalf("first mount: %d tracks in the playlist", got)
	}
	if _, err = os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatal("a library imported without its cues was recorded as imported")
	}

	// Next mount: the files are read and the import is recorded. The deadline
	// stands in for the mount's read budget, which -race on a busy host can use up.
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)}, false); err != nil || r.Deferred != 0 || r.Updated != n {
		t.Fatalf("second mount: %+v %v", r, err)
	}
	if got := scalar[int](t, engine, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL AND quickCues IS NOT NULL AND loops IS NOT NULL`); got != n {
		t.Fatalf("%d of %d tracks have their beat grid, cues and loops", got, n)
	}
	loops := scalar[[]byte](t, engine, `SELECT p.loops FROM PerformanceData p JOIN Track t ON t.id=p.trackId WHERE t.pdbImportKey=?`, n)
	if !bytes.Contains(loops, []byte("H Loop")) || !bytes.Contains(loops, []byte("Memory Loop")) {
		t.Fatalf("loops of track %d: %q", n, loops)
	}
	if got := scalar[int](t, engine, `SELECT count(*) FROM PlaylistEntity e JOIN Playlist p ON p.id=e.listId WHERE p.title='Big Set'`); got != n {
		t.Fatalf("second mount: %d tracks in the playlist", got)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Bypassed {
		t.Fatalf("complete import not recorded: %+v %v", r, err)
	}
}

const largeTracks = 520

// largeExport writes a OneLibrary export of largeTracks tracks, each with its
// own WAV and the fixture's analysis files (beat grid, hot cues, loops).
func largeExport(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testfixture.Export(t, root) // the analysis files every track below points to
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE content(id INTEGER PRIMARY KEY,title TEXT,file_path TEXT,analysis_path TEXT,bpm INTEGER,key_name TEXT);
CREATE TABLE playlist(id INTEGER PRIMARY KEY,parent_id INTEGER,name TEXT,attribute INTEGER,seq INTEGER,smart_list TEXT);
CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,track_no INTEGER);
INSERT INTO playlist VALUES(1,0,'Big Set',0,1,'');`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= largeTracks; i++ {
		p := fmt.Sprintf("MUSIC/big/t%03d.wav", i)
		testfixture.Write(t, root, p, testfixture.WAV(true))
		if _, err = db.Exec(`INSERT INTO content VALUES(?,?,?,?,12800,'8A')`, i, fmt.Sprintf("Track %03d", i), "/"+p, "/PIONEER/USBANLZ/P001/ANLZ0000.DAT"); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO playlist_content VALUES(1,?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return root
}

// The mount after a partial import of the same export reads only the tracks
// still without their files; a finished track is not stat'ed or re-read, so a
// large library does not spend each mount's budget re-checking finished tracks.
// A stick that an earlier build stamped right after the export-only import is
// finished too, instead of being skipped for good.
func TestLargeLibraryResumesAndHealsAnOldStamp(t *testing.T) {
	root := largeExport(t)
	ctx := context.Background()
	far := enginedb.Options{Deadline: time.Now().Add(time.Hour)}
	dir := filepath.Join(root, "Engine Library/Database2")
	if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	// What an earlier build left: the export-only import recorded as complete.
	if err := os.Rename(filepath.Join(dir, ".rb-bridge-pending"), filepath.Join(dir, ".rb-bridge-stamp")); err != nil {
		t.Fatal(err)
	}
	r, err := bridge.ConvertAtMount(ctx, root, far, false)
	if err != nil || r.Bypassed || r.Updated != largeTracks || r.Deferred != 0 {
		t.Fatalf("old stamp: %+v %v", r, err)
	}
	engine := dbOpen(t, root)
	if got := scalar[int](t, engine, `SELECT count(*) FROM PerformanceData WHERE beatData IS NOT NULL`); got != largeTracks {
		t.Fatalf("%d of %d tracks have their beat grid", got, largeTracks)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, far, false); err != nil || !r.Bypassed {
		t.Fatalf("finished import not recorded: %+v %v", r, err)
	}
	// One track left without its files, one finished track whose file changed
	// since: only the first is read.
	if _, err = engine.Exec(`UPDATE bridge.BridgeState SET fingerprint='fast:x' WHERE trackId=(SELECT id FROM Track WHERE pdbImportKey=7)`); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err = os.Chtimes(filepath.Join(root, "MUSIC/big/t008.wav"), later, later); err != nil {
		t.Fatal(err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, far, false); err != nil || r.Updated != 1 || r.Unchanged != largeTracks-1 || r.Deferred != 0 {
		t.Fatalf("resumed mount: %+v %v", r, err)
	}
	if r, err = bridge.ConvertAtMount(ctx, root, far, false); err != nil || !r.Bypassed {
		t.Fatalf("resumed import not recorded: %+v %v", r, err)
	}
}
