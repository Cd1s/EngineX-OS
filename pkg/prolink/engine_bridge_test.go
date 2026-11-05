package prolink_test

import (
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEngineBridgeSyncAndAnalysis(t *testing.T) {
	dir := t.TempDir()

	dbDir := filepath.Join(dir, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}

	audioDir := filepath.Join(dir, "Music", "Stream")
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(audioDir, "netease_track.wav")
	if err := os.WriteFile(audioPath, []byte("RIFF4WAVEfmt mock audio"), 0644); err != nil {
		t.Fatal(err)
	}

	mdbPath := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", mdbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE Track (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT,
		artist TEXT,
		bpm INTEGER,
		bpmAnalyzed REAL,
		length INTEGER,
		path TEXT,
		filename TEXT
	);
	CREATE TABLE Playlist (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT,
		parentListId INTEGER,
		nextListId INTEGER
	);
	CREATE TABLE PlaylistEntity (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		listId INTEGER,
		trackId INTEGER,
		nextEntityId INTEGER
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, err = db.Exec(`
		INSERT INTO Track(id, title, artist, bpm, bpmAnalyzed, length, path, filename)
		VALUES(1, 'Streaming Echoes', 'VirtualDJ', 128, 128.0, 240, '../Music/Stream/netease_track.wav', 'netease_track.wav');
		INSERT INTO Playlist(id, title, parentListId, nextListId)
		VALUES(10, '[SC5000 Streaming]', 0, 0);
		INSERT INTO PlaylistEntity(id, listId, trackId, nextEntityId)
		VALUES(1, 10, 1, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Initial export.pdb should not exist before bridge run
	pdbPath := filepath.Join(dir, "PIONEER", "rekordbox", "export.pdb")
	if _, err := os.Stat(pdbPath); err == nil {
		t.Fatal("export.pdb should not exist yet")
	}

	if err := prolink.SyncEngineToPioneerExport(dir); err != nil {
		t.Fatalf("SyncEngineToPioneerExport: %v", err)
	}
	if !prolink.GeneratedExport(dir) {
		t.Fatal("generated export.pdb is not marked as ours")
	}

	// Verify export.pdb was created and is valid
	lib, err := rekordbox.ReadPDB(pdbPath)
	if err != nil {
		t.Fatalf("ReadPDB failed: %v", err)
	}
	if len(lib.Tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(lib.Tracks))
	}
	tr := lib.Tracks[0]
	if tr.ID != 1 || tr.Title != "Streaming Echoes" || tr.Artist != "VirtualDJ" {
		t.Errorf("track mismatch: %+v", tr)
	}
	if tr.BPM != 128.0 || tr.Path != "/Music/Stream/netease_track.wav" {
		t.Errorf("track path or BPM mismatch: %+v", tr)
	}
	if len(lib.Playlists) != 1 {
		t.Fatalf("expected 1 playlist, got %d", len(lib.Playlists))
	}
	pl := lib.Playlists[0]
	if pl.ID != 10 || pl.Name != "[SC5000 Streaming]" || len(pl.Tracks) != 1 || pl.Tracks[0] != 1 {
		t.Errorf("playlist mismatch: %+v", pl)
	}

	// Check ANLZ files
	anlzDir := filepath.Join(dir, "PIONEER", "USBANLZ", "P001")
	datPath := filepath.Join(anlzDir, "ANLZ0000.DAT")
	extPath := filepath.Join(anlzDir, "ANLZ0000.EXT")
	twoExPath := filepath.Join(anlzDir, "ANLZ0000.2EX")

	for _, p := range []string{datPath, extPath, twoExPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing expected ANLZ file: %s (%v)", p, err)
		}
	}

	datBytes, err := os.ReadFile(datPath)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ParseAnalysis(datBytes)
	if err != nil {
		t.Fatalf("ParseAnalysis DAT failed: %v", err)
	}
	wave, beats, durMS := prolink.ExtractANLZ(a)
	_ = wave
	if len(beats) < 100 {
		t.Errorf("expected generated beatgrid, got %d beats", len(beats))
	}
	if durMS == 0 {
		t.Errorf("expected non-zero duration from beatgrid")
	}

	// Calling SyncEngineToPioneerExport again should be a no-op (stamp match)
	fiBefore, err := os.Stat(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := prolink.SyncEngineToPioneerExport(dir); err != nil {
		t.Fatal(err)
	}
	fiAfter, err := os.Stat(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !fiBefore.ModTime().Equal(fiAfter.ModTime()) {
		t.Errorf("stamp caching did not prevent re-export")
	}

	// Verify Exporter automatically runs bridge and serves
	exp := prolink.NewExporter(prolink.ExportConfig{
		Root:          dir,
		IP:            net.IPv4(127, 63, 0, 1),
		MAC:           net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		DBAddr:        "127.63.0.1:0",
		PortmapAddr:   "127.63.0.1:0",
		DiscoveryAddr: "127.63.0.1:0",
		StatusAddr:    "127.63.0.1:0",
		AnnounceAddr:  "127.63.0.1:50000",
	})
	if err := exp.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled failed: %v", err)
	}
	defer exp.Close()

	st := exp.Status()
	if !st.Enabled || st.Root != dir {
		t.Errorf("unexpected status: %+v", st)
	}
}

func TestEngineBridgeLeavesARealExportAlone(t *testing.T) {
	root := t.TempDir()
	// Export standard 5-track Pioneer test fixture
	testfixture.Export(t, root)

	pdbPath := filepath.Join(root, "PIONEER", "rekordbox", "export.pdb")
	beforeLib, err := rekordbox.ReadPDB(pdbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeLib.Tracks) != 5 {
		t.Fatalf("expected 5 tracks in initial export, got %d", len(beforeLib.Tracks))
	}

	// Now add an Engine m.db containing 1 existing track + 1 new streaming track
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		t.Fatal(err)
	}
	audioDir := filepath.Join(root, "Music", "Stream")
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(audioDir, "new_stream.wav"), []byte("RIFF4WAVE"), 0644)

	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE Track (
			id INTEGER PRIMARY KEY,
			title TEXT,
			artist TEXT,
			bpm INTEGER,
			bpmAnalyzed REAL,
			length INTEGER,
			path TEXT,
			filename TEXT
		);
		CREATE TABLE Playlist (
			id INTEGER PRIMARY KEY,
			title TEXT,
			parentListId INTEGER,
			nextListId INTEGER
		);
		CREATE TABLE PlaylistEntity (
			id INTEGER PRIMARY KEY,
			listId INTEGER,
			trackId INTEGER,
			nextEntityId INTEGER
		);
		-- Track 1 corresponds to existing /Music/two.flac
		INSERT INTO Track VALUES(1, 'two.flac', 'Artist', 124, 124.0, 180, '../Music/two.flac', 'two.flac');
		-- Track 2 is a new streaming track
		INSERT INTO Track VALUES(2, 'Cyberpunk VIP', 'StreamStar', 126, 126.0, 200, '../Music/Stream/new_stream.wav', 'new_stream.wav');
		INSERT INTO Playlist VALUES(100, '[SC5000 Streaming]', 0, 0);
		INSERT INTO PlaylistEntity VALUES(1, 100, 2, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	before, _ := os.ReadFile(pdbPath)
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatalf("SyncEngineToPioneerExport: %v", err)
	}
	// The DJ's own rekordbox export is never rewritten (#63): it may hold data our writer drops.
	if after, _ := os.ReadFile(pdbPath); string(after) != string(before) || prolink.GeneratedExport(root) {
		t.Fatal("a real rekordbox export was rewritten")
	}
}
