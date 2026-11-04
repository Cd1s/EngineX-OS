package bridge_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestOneLibraryVendorColumnsAndMountedSync(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if e := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE content(content_id INTEGER PRIMARY KEY,title TEXT,path TEXT,bpmx100 INTEGER,length INTEGER,artist_id_artist INTEGER,artist_id_composer INTEGER,key_id INTEGER,samplingRate INTEGER,bitDepth INTEGER,analysisDataFilePath TEXT,djComment TEXT);CREATE TABLE artist(artist_id INTEGER PRIMARY KEY,name TEXT);CREATE TABLE key(key_id INTEGER PRIMARY KEY,name TEXT);INSERT INTO artist VALUES(7,'Vendor artist');INSERT INTO key VALUES(3,'Am');INSERT INTO content VALUES(42,'Vendor track','/MUSIC/three.wav',12000,2,7,7,3,48000,24,'/PIONEER/USBANLZ/P001/ANLZ0000.DAT','DJ comment');CREATE TABLE playlist(playlist_id INTEGER PRIMARY KEY,playlist_id_parent INTEGER,sequenceNo INTEGER,name TEXT,attribute INTEGER);INSERT INTO playlist VALUES(90,NULL,0,'Folder',1),(91,90,1,'Intelligent',4);CREATE TABLE playlist_content(playlist_id INTEGER,content_id INTEGER,sequenceNo INTEGER);INSERT INTO playlist_content VALUES(91,42,1);`)
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	before := snapshot(t, filepath.Join(root, "PIONEER"))
	source := bridge.MountedSource{MountPath: root}
	l, e := source.Library(context.Background(), prolink.Device{Name: "rekordbox"})
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Tracks) != 1 || l.Tracks[0].Artist != "Vendor artist" || l.Tracks[0].Composer != "Vendor artist" || l.Tracks[0].BPM != 120 || l.Tracks[0].SampleRate != 48000 || l.Tracks[0].AnalysisPath == "" || l.Tracks[0].Key != "Am" {
		t.Fatalf("actual OneLibrary columns not handled: %+v", l)
	}
	if e = prolink.Sync(context.Background(), prolink.Device{}, source, bridge.EngineSink{MountPath: root}); e != nil {
		t.Fatal(e)
	}
	engine := dbOpen(t, root)
	id := scalar[int64](t, engine, `SELECT id FROM Track WHERE pdbImportKey=42`)
	if got := playlistTracks(t, engine, "Intelligent", "Folder"); !reflect.DeepEqual(got, []int64{id}) {
		t.Fatal(got)
	}
	if scalar[int](t, engine, `SELECT isAnalyzed FROM Track WHERE id=?`, id) != 1 {
		t.Fatal("ANLZ alias not imported")
	}
	if !reflect.DeepEqual(before, snapshot(t, filepath.Join(root, "PIONEER"))) {
		t.Fatal("mounted LAN source modified")
	}
}

func TestPlaylistAndSmartMatchDelta(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	opts := enginedb.Options{SmartMatch: true, CurrentTrack: 1}
	convert(t, root, opts)
	engine := dbOpen(t, root)
	if _, e := engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('Native',0,1,0)`); e != nil {
		t.Fatal(e)
	}
	l, e := bridge.ReadLibrary(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	l.Tracks[0].BPM = 123
	for i := range l.Playlists {
		if l.Playlists[i].ID == 12 {
			l.Playlists[i].Tracks = []uint32{2, 1}
		}
		if l.Playlists[i].ID == 13 {
			l.Playlists[i].SortOrder = 0
		}
	}
	r, e := enginedb.Sync(context.Background(), root, l, opts)
	if e != nil {
		t.Fatal(e)
	}
	if r.Updated != 1 || !r.PlaylistsChanged {
		t.Fatal(r)
	}
	if got := playlistTracks(t, engine, "同名", "Nested"); !reflect.DeepEqual(got, []int64{2, 1}) {
		t.Fatal(got)
	}
	id := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='[Smart Match]'`)
	if got := entityChain(t, engine, id); !reflect.DeepEqual(got, []int64{2, 5}) {
		t.Fatalf("stale recommendations: %v", got)
	}
	if scalar[int](t, engine, `SELECT count(*) FROM Playlist WHERE title='Native'`) != 1 || scalar[int](t, engine, `SELECT count(*) FROM pragma_foreign_key_check`) != 0 {
		t.Fatal("native row or FK damaged")
	}
	r, e = enginedb.Sync(context.Background(), root, l, opts)
	if e != nil || r.Updated+r.Added != 0 || r.PlaylistsChanged {
		t.Fatal("delta not stable", r, e)
	}
	// Native children under an imported folder must block destructive rebuilds.
	folder := scalar[int64](t, engine, `SELECT id FROM Playlist WHERE title='Folder'`)
	if _, e = engine.Exec(`INSERT INTO Playlist(title,parentListId,isPersisted,nextListId) VALUES('User child',?,1,0)`, folder); e != nil {
		t.Fatal(e)
	}
	l.Playlists[0].Name += " changed"
	if _, e = enginedb.Sync(context.Background(), root, l, opts); e == nil {
		t.Fatal("native child silently deleted")
	}
	if scalar[int](t, engine, `SELECT count(*) FROM Playlist WHERE title='User child'`) != 1 {
		t.Fatal("native child lost")
	}
}

func TestWAVContainer32Valid24(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	b := testfixture.WAV(true)
	f := b[20:]
	binary.LittleEndian.PutUint16(f[14:], 32)
	binary.LittleEndian.PutUint16(f[12:], 8)
	binary.LittleEndian.PutUint32(f[8:], 384000)
	testfixture.Write(t, root, "MUSIC/three.wav", b)
	convert(t, root, enginedb.Options{})
	engine := dbOpen(t, root)
	blob := scalar[[]byte](t, engine, `SELECT trackData FROM PerformanceData WHERE trackId=3`)
	raw, e := enginedb.QUncompress(blob, 100)
	if e != nil {
		t.Fatal(e)
	}
	if got := binary.BigEndian.Uint64(raw[8:]); got != 72000 {
		t.Fatalf("valid bits confused with container width: %d", got)
	}
}

func TestCF049Bogus500BPMAndGL010InformationGuard(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].BPM = 500.0
	if _, err := enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	db := dbOpen(t, root)
	// Track 1 has valid 120 BPM PQTZ beats in the fixture, so 500 BPM header clamp falls back to beat BPM (120).
	if got := scalar[int](t, db, `SELECT bpm FROM Track WHERE pdbImportKey=1`); got != 120 {
		t.Fatalf("expected 500 BPM clamp to recover 120 BPM from beats, got %d", got)
	}
	// Track 2 has no ANLZ beats; 500 BPM clamp resets to 0 so Engine analyses on load instead of locking at 500 BPM.
	l.Tracks[1].BPM = 500.0
	if _, err := enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[int](t, db, `SELECT bpm FROM Track WHERE pdbImportKey=2`); got != 0 {
		t.Fatalf("expected unanalysed 500 BPM track to reset to 0, got %d", got)
	}
	// GL-010: Damage Information.id and uuid, verify Sync and dbhealer both restore id=1 and non-empty uuid.
	if _, err := db.Exec(`UPDATE Information SET id=7, uuid=''`); err != nil {
		t.Fatal(err)
	}
	if _, err := enginedb.Sync(context.Background(), root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	if id := scalar[int](t, db, `SELECT id FROM Information`); id != 1 {
		t.Fatalf("Information.id = %d, want 1", id)
	}
	if u := scalar[string](t, db, `SELECT uuid FROM Information`); u == "" {
		t.Fatal("Information.uuid remained empty")
	}
}
