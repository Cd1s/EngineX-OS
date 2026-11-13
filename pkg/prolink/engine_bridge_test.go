package prolink_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
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
	if len(beats) != 0 || durMS != 0 {
		t.Errorf("unanalysed audio acquired invented beats: %d, duration %d", len(beats), durMS)
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

// These blobs follow the captured Engine mixed-endian structures, independently
// of the export encoder. Sparse adjusted markers exercise tempo changes and a
// non-zero downbeat; the captured DDL provides the real tables and triggers.
func bridgeEnginePerformance(t *testing.T) (beat, quick, loops, overview []byte) {
	t.Helper()
	var b bytes.Buffer
	put := func(buf *bytes.Buffer, order binary.ByteOrder, v any) {
		t.Helper()
		if err := binary.Write(buf, order, v); err != nil {
			t.Fatal(err)
		}
	}
	qc := func(raw []byte) []byte {
		t.Helper()
		v, err := enginedb.QCompress(raw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	put(&b, binary.BigEndian, float64(48000))
	put(&b, binary.BigEndian, float64(192000))
	b.WriteByte(1)
	for _, markers := range [][][2]int64{{{0, 0}, {192000, 8}}, {{24000, 0}, {72000, 2}, {168000, 5}}} {
		put(&b, binary.BigEndian, uint64(len(markers)))
		for _, m := range markers {
			put(&b, binary.LittleEndian, float64(m[0]))
			put(&b, binary.LittleEndian, m[1])
			put(&b, binary.LittleEndian, uint32(1))
			put(&b, binary.LittleEndian, uint32(0))
		}
	}
	b.WriteByte(0)
	beat = qc(b.Bytes())
	b.Reset()
	put(&b, binary.BigEndian, uint64(8))
	for slot := 0; slot < 8; slot++ {
		name := ""
		position := float64(-1)
		if slot == 0 {
			name = "一 🚀"
			position = 48000
		}
		if slot == 7 {
			name = "H"
			position = 144000
		}
		b.WriteByte(byte(len(name)))
		b.WriteString(name)
		put(&b, binary.BigEndian, position)
		b.Write([]byte{255, 17, 34, 51})
	}
	put(&b, binary.BigEndian, float64(12000))
	b.WriteByte(1)
	put(&b, binary.BigEndian, float64(0))
	quick = qc(b.Bytes())
	b.Reset()
	put(&b, binary.LittleEndian, uint64(8))
	for slot := 0; slot < 8; slot++ {
		name := ""
		start, end := float64(-1), float64(-1)
		flag := byte(0)
		if slot == 0 {
			name = "一 🚀"
			start = 48000
			end = 96000
			flag = 1
		}
		if slot == 1 {
			name = "Memory loop"
			start = 60000
			end = 84000
			flag = 1
		}
		b.WriteByte(byte(len(name)))
		b.WriteString(name)
		put(&b, binary.LittleEndian, start)
		put(&b, binary.LittleEndian, end)
		b.Write([]byte{flag, flag, 255, 17, 34, 51})
	}
	loops = append([]byte(nil), b.Bytes()...)
	b.Reset()
	put(&b, binary.BigEndian, uint64(1024))
	put(&b, binary.BigEndian, uint64(1024))
	put(&b, binary.BigEndian, float64(187.5))
	for i := 0; i < 1024; i++ {
		if i < 512 {
			b.Write([]byte{10, 20, 30})
		} else {
			b.Write([]byte{40, 50, 60})
		}
	}
	b.Write([]byte{0, 0, 0})
	overview = qc(b.Bytes())
	return
}

func bridgeNoStreams(t *testing.T) {
	t.Helper()
	old := prolink.StreamDirs
	prolink.StreamDirs = nil
	t.Cleanup(func() { prolink.StreamDirs = old })
}

func bridgeExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func bridgeReadLibrary(t *testing.T, root string) *rekordbox.Library {
	t.Helper()
	lib, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Validate(); err != nil {
		t.Fatal(err)
	}
	return lib
}

func bridgeReadAnalysis(t *testing.T, root, path string) *rekordbox.Analysis {
	t.Helper()
	var b []byte
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err = os.ReadFile(filepath.Join(root, path))
		if err == nil || !os.IsNotExist(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	a, err := rekordbox.ParseAnalysis(b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestEngineBridgeNativePerformance(t *testing.T) {
	bridgeNoStreams(t)
	mdb, root, db := testlibrary.Create(t)
	for id := 1; id <= 3; id++ {
		bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length,bpm,bpmAnalyzed,key) VALUES(?,?, 'Artist',?,?,4,120,120,?)`, id, fmt.Sprintf("Track %d", id), fmt.Sprintf("../Music/%d.wav", id), fmt.Sprintf("%d.wav", id), []any{19, 0, nil}[id-1])
		testfixture.Write(t, root, fmt.Sprintf("Music/%d.wav", id), testfixture.WAV(true))
	}
	beat, quick, loops, overview := bridgeEnginePerformance(t)
	bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?,overviewWaveFormData=? WHERE trackId=1`, beat, quick, loops, overview)
	var cover bytes.Buffer
	if err := jpeg.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	bridgeExec(t, db, `INSERT INTO AlbumArt(id,hash,albumArt) VALUES(1,'fixture',?)`, cover.Bytes())
	bridgeExec(t, db, `UPDATE Track SET albumArtId=1 WHERE id=1`)
	bridgeExec(t, db, `INSERT INTO Playlist(id,title,parentListId) VALUES(10,'Ordered',0)`)
	// id order is 1,2,3,4; the actual chain is 3 -> 1 -> missing -> 2.
	bridgeExec(t, db, `INSERT INTO PlaylistEntity(id,listId,trackId,nextEntityId) VALUES(1,10,1,4),(2,10,2,0),(3,10,3,1),(4,10,9999,2)`)
	before, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("Engine source database changed")
	}
	lib := bridgeReadLibrary(t, root)
	if len(lib.Tracks) != 3 || lib.Tracks[0].Key != "5A" || lib.Tracks[1].Key != "8B" || lib.Tracks[2].Key != "" {
		t.Fatalf("keys: %+v", lib.Tracks)
	}
	if len(lib.Playlists) != 1 || !reflect.DeepEqual(lib.Playlists[0].Tracks, []uint32{3, 1, 2}) {
		t.Fatalf("playlist: %+v", lib.Playlists)
	}
	tr := lib.Tracks[0]
	if tr.SampleRate != 48000 || tr.Duration != 4 {
		t.Fatalf("actual audio metadata: %+v", tr)
	}
	if tr.ArtworkPath == "" {
		t.Fatal("missing artwork row/reference")
	}
	art, err := os.ReadFile(filepath.Join(root, tr.ArtworkPath))
	if err != nil || !bytes.Equal(art, cover.Bytes()) {
		t.Fatalf("artwork: %v", err)
	}
	dat := bridgeReadAnalysis(t, root, tr.AnalysisPath)
	rawDAT, err := os.ReadFile(filepath.Join(root, tr.AnalysisPath))
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(rawDAT, []byte("PQTZ"))
	if at < 0 || binary.BigEndian.Uint32(rawDAT[at+4:]) != 24 || binary.BigEndian.Uint32(rawDAT[at+16:]) != 0x00800000 {
		t.Fatal("PQTZ wire header differs from the documented byte layout")
	}
	wantTimes := []uint32{0, 500, 1000, 1500, 2167, 2833, 3500}
	if len(dat.Beats) != len(wantTimes) {
		t.Fatalf("beats: %+v", dat.Beats)
	}
	for i, b := range dat.Beats {
		wantBPM := 120.0
		if i >= 3 {
			wantBPM = 90
		}
		if b.TimeMS != wantTimes[i] || math.Abs(b.BPM-wantBPM) > .01 || b.Number != uint16((i+3)%4+1) {
			t.Errorf("beat %d: %+v", i, b)
		}
	}
	ext := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+"EXT")
	two := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+"2EX")
	for _, a := range []*rekordbox.Analysis{dat, ext} {
		if len(a.Cues) != 4 {
			t.Fatalf("cues: %+v", a.Cues)
		}
		got := map[uint32]rekordbox.Cue{}
		var memory []rekordbox.Cue
		for _, c := range a.Cues {
			if c.HotCue > 0 {
				got[c.HotCue] = c
			} else {
				memory = append(memory, c)
			}
		}
		if len(got) != 2 || !got[1].Loop || got[1].TimeMS != 1000 || got[1].EndMS != 2000 || got[8].TimeMS != 3000 {
			t.Errorf("hot cues: %+v", got)
		}
		if len(memory) != 2 || memory[0].TimeMS != 250 || !memory[1].Loop || memory[1].TimeMS != 1250 || memory[1].EndMS != 1750 {
			t.Errorf("memory: %+v", memory)
		}
		if a == ext && (got[1].Name != "一 🚀" || got[1].Color != [4]byte{255, 17, 34, 51} || got[1].ColorCode == 0) {
			t.Errorf("extended cue: %+v", got[1])
		}
	}
	if len(ext.Waveforms) != 1 || len(two.Waveforms) != 1 {
		t.Fatal("missing measured waveforms")
	}
	w5, w7 := ext.Waveforms[0], two.Waveforms[0]
	if w5.Tag != "PWV5" || w5.EntryBytes != 2 || len(w5.Data) != 600*2 || w7.Tag != "PWV7" || len(w7.Data) != 600*3 {
		t.Fatalf("waveform timeline: %+v / %d", w5, len(w7.Data))
	}
	if !bytes.Equal(w7.Data[:3], []byte{10, 20, 30}) || !bytes.Equal(w7.Data[len(w7.Data)-3:], []byte{40, 50, 60}) {
		t.Fatal("PWV7 bands/variation lost")
	}
	if binary.BigEndian.Uint16(w5.Data) != uint16(7<<13|7<<10|5<<7|2<<2) || binary.BigEndian.Uint16(w5.Data[len(w5.Data)-2:]) != uint16(7<<13|5<<10|3<<7|8<<2) {
		t.Fatal("PWV5 packing/variation lost")
	}
	// Empty performance must stay empty, rather than generating 400 fake points.
	missing := bridgeReadAnalysis(t, root, strings.TrimSuffix(lib.Tracks[1].AnalysisPath, "DAT")+"EXT")
	if len(missing.Waveforms) != 0 || len(missing.Cues) != 0 {
		t.Fatal("invented missing analysis")
	}
	bridgeExec(t, db, `DELETE FROM Track WHERE id=2`)
	bridgeExec(t, db, `UPDATE Playlist SET title='Renamed' WHERE id=10`)
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	refreshed := bridgeReadLibrary(t, root)
	if len(refreshed.Tracks) != 2 || len(refreshed.Playlists) != 1 || refreshed.Playlists[0].Name != "Renamed" || !reflect.DeepEqual(refreshed.Playlists[0].Tracks, []uint32{3, 1}) {
		t.Fatalf("stale tracks/playlists retained after source edit: %+v / %+v", refreshed.Tracks, refreshed.Playlists)
	}

}

func TestEngineBridgeIncrementalAndWALRefresh(t *testing.T) {
	bridgeNoStreams(t)
	mdb, root, db := testlibrary.Create(t)
	bridgeExec(t, db, `PRAGMA journal_mode=WAL`)
	bridgeExec(t, db, `PRAGMA wal_autocheckpoint=0`)
	for id := 1; id <= 70; id++ {
		bridgeExec(t, db, `INSERT INTO Track(id,title,path,filename,length) VALUES(?,?,?,?,4)`, id, fmt.Sprintf("Track %d", id), fmt.Sprintf("../Music/%d.wav", id), fmt.Sprintf("%d.wav", id))
	}
	beat, quick, loops, overview := bridgeEnginePerformance(t)
	bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?,overviewWaveFormData=? WHERE trackId=70`, beat, quick, loops, overview)
	start := time.Now()
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("startup blocked for %s", elapsed)
	}
	lib := bridgeReadLibrary(t, root)
	if len(lib.Tracks) != 70 {
		t.Fatalf("only a partial library was published: %d", len(lib.Tracks))
	}
	pdbBefore, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	bridgeWaitExportComplete(t, root)
	last := bridgeReadAnalysis(t, root, lib.Tracks[69].AnalysisPath)
	if len(last.Beats) != 7 || len(last.Cues) != 4 {
		t.Fatal("background job did not export its actual grid/cues")
	}
	if lib.Tracks[69].SampleRate != 48000 {
		t.Fatal("deferred track lost its actual audio metadata")
	}
	lastWave := bridgeReadAnalysis(t, root, strings.TrimSuffix(lib.Tracks[69].AnalysisPath, "DAT")+"2EX")
	if len(lastWave.Waveforms) != 1 || len(lastWave.Waveforms[0].Data) != 600*3 {
		t.Fatal("background waveform did not complete")
	}
	pdbAfter, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil || !bytes.Equal(pdbBefore, pdbAfter) {
		t.Fatalf("background analysis changed immutable browse metadata: %v", err)
	}
	source, err := os.Stat(mdb)
	if err != nil {
		t.Fatal(err)
	}
	bridgeExec(t, db, `UPDATE Track SET key=19,title='Changed' WHERE id=1`)
	sourceAfter, err := os.Stat(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if source.ModTime() != sourceAfter.ModTime() || source.Size() != sourceAfter.Size() {
		t.Fatal("fixture did not isolate WAL-only update")
	}
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	bridgeWaitExportComplete(t, root)
	lib = bridgeReadLibrary(t, root)
	if len(lib.Tracks) != 70 || lib.Tracks[0].ID != 1 || lib.Tracks[0].Key != "5A" || lib.Tracks[0].Title != "Changed" {
		t.Fatalf("stale/reassigned track: %+v", lib.Tracks[0])
	}
}

func TestEngineBridgeRejectsDamagedPerformance(t *testing.T) {
	for _, field := range []string{"beatData", "quickCues", "loops", "overviewWaveFormData"} {
		t.Run(field, func(t *testing.T) {
			bridgeNoStreams(t)
			_, root, db := testlibrary.Create(t)
			bridgeExec(t, db, `INSERT INTO Track(id,path,filename,length) VALUES(1,'../Music/a.wav','a.wav',4)`)
			beat, quick, loops, overview := bridgeEnginePerformance(t)
			bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,loops=?,overviewWaveFormData=? WHERE trackId=1`, beat, quick, loops, overview)
			testfixture.Write(t, root, "Music/a.wav", testfixture.WAV(true))
			bad := []byte{1, 2, 3}
			bridgeExec(t, db, "UPDATE PerformanceData SET "+field+"=? WHERE trackId=1", bad)
			if err := prolink.SyncEngineToPioneerExport(root); err != nil {
				t.Fatal(err)
			}
			tr := bridgeReadLibrary(t, root).Tracks[0]
			dat := bridgeReadAnalysis(t, root, tr.AnalysisPath)
			ext := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+"EXT")
			if field == "beatData" && len(dat.Beats) != 0 {
				t.Fatal("damaged beats were exported")
			}
			if field == "overviewWaveFormData" && len(ext.Waveforms) != 0 {
				t.Fatal("damaged waveform was exported")
			}
			if field != "beatData" && len(dat.Beats) == 0 {
				t.Fatal("valid independent beat grid was lost")
			}
			if field == "loops" {
				for _, c := range ext.Cues {
					if c.Loop {
						t.Fatal("damaged loop was exported")
					}
				}
			}
		})
	}
}

func TestEngineBridgeArtworkFileAndAllHotCueSlots(t *testing.T) {
	bridgeNoStreams(t)
	_, root, db := testlibrary.Create(t)
	bridgeExec(t, db, `INSERT INTO Track(id,path,filename,length,key,albumArtId) VALUES(1,'../Music/a.wav','a.wav',4,23,7)`)
	beat, _, _, overview := bridgeEnginePerformance(t)
	var q bytes.Buffer
	binary.Write(&q, binary.BigEndian, uint64(8))
	for slot := 1; slot <= 8; slot++ {
		q.WriteByte(1)
		q.WriteByte(byte('A' + slot - 1))
		binary.Write(&q, binary.BigEndian, float64(slot*12000))
		q.Write([]byte{255, 10, 20, 30})
	}
	binary.Write(&q, binary.BigEndian, float64(-1))
	q.WriteByte(0)
	binary.Write(&q, binary.BigEndian, float64(-1))
	quick, err := enginedb.QCompress(q.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	bridgeExec(t, db, `UPDATE PerformanceData SET beatData=?,quickCues=?,overviewWaveFormData=? WHERE trackId=1`, beat, quick, overview)
	hash := bytes.Repeat([]byte{7}, 20)
	bridgeExec(t, db, `INSERT INTO AlbumArt(id,hash) VALUES(7,?)`, hash)
	var cover bytes.Buffer
	if err := jpeg.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 3, 3)), nil); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "Engine Library/Artwork/"+base64.RawURLEncoding.EncodeToString(hash)+".jpg", cover.Bytes())
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	lib := bridgeReadLibrary(t, root)
	tr := lib.Tracks[0]
	if tr.ArtworkPath == "" || tr.Key != "7A" {
		t.Fatalf("track metadata: %+v", tr)
	}
	for _, ext := range []string{"DAT", "EXT"} {
		a := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+ext)
		if len(a.Cues) != 8 {
			t.Fatalf("%s: %+v", ext, a.Cues)
		}
		for i, c := range a.Cues {
			if c.HotCue != uint32(i+1) || c.TimeMS != uint32((i+1)*250) {
				t.Errorf("%s slot %d: %+v", ext, i, c)
			}
		}
	}
}

func TestEngineBridgeRejectsExtremeBeatAnchors(t *testing.T) {
	for _, kind := range []string{"count", "nan", "tiny-step"} {
		t.Run(kind, func(t *testing.T) {
			bridgeNoStreams(t)
			_, root, db := testlibrary.Create(t)
			bridgeExec(t, db, `INSERT INTO Track(id,path,filename,length) VALUES(1,'../Music/a.wav','a.wav',4)`)
			beat, _, _, _ := bridgeEnginePerformance(t)
			raw, err := enginedb.QUncompress(beat, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "count":
				binary.BigEndian.PutUint64(raw[17:], math.MaxUint64)
			case "nan":
				binary.BigEndian.PutUint64(raw, math.Float64bits(math.NaN()))
			case "tiny-step":
				// The selected adjusted grid starts at 17+8+2*24+8. Almost
				// coincident anchors must fail before float-to-int/capacity conversion.
				at := 81
				binary.LittleEndian.PutUint64(raw[at:], math.Float64bits(1e-300))
				binary.LittleEndian.PutUint64(raw[at+24:], math.Float64bits(2e-300))
				binary.LittleEndian.PutUint64(raw[at+48:], math.Float64bits(3e-300))
			}
			damaged, err := enginedb.QCompress(raw)
			if err != nil {
				t.Fatal(err)
			}
			bridgeExec(t, db, `UPDATE PerformanceData SET beatData=? WHERE trackId=1`, damaged)
			if err := prolink.SyncEngineToPioneerExport(root); err != nil {
				t.Fatal(err)
			}
			tr := bridgeReadLibrary(t, root).Tracks[0]
			if len(bridgeReadAnalysis(t, root, tr.AnalysisPath).Beats) != 0 {
				t.Fatal("extreme grid was exported")
			}
		})
	}
}

func TestEngineBridgeDoesNotFollowAnalysisOutputSymlinks(t *testing.T) {
	bridgeNoStreams(t)
	mdb, root, db := testlibrary.Create(t)
	bridgeExec(t, db, `INSERT INTO Track(id,path,filename,length) VALUES(1,'../Music/a.wav','a.wav',4)`)
	dir := filepath.Join(root, "PIONEER/USBANLZ/P001")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mdb, filepath.Join(dir, "ANLZ0000.DAT")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(mdb)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("source database was changed via output symlink: %v", err)
	}
}

func TestEngineBridgeDoesNotInventUnknownDuration(t *testing.T) {
	bridgeNoStreams(t)
	_, root, db := testlibrary.Create(t)
	bridgeExec(t, db, `INSERT INTO Track(id,path,filename) VALUES(1,'../Music/absent.wav','absent.wav')`)
	_, _, _, overview := bridgeEnginePerformance(t)
	bridgeExec(t, db, `UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=1`, overview)
	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	tr := bridgeReadLibrary(t, root).Tracks[0]
	if tr.Duration != 0 || tr.BPM != 0 {
		t.Fatalf("invented metadata: %+v", tr)
	}
	a := bridgeReadAnalysis(t, root, strings.TrimSuffix(tr.AnalysisPath, "DAT")+"EXT")
	if len(a.Waveforms) != 0 {
		t.Fatal("overview was stretched onto an invented duration")
	}
}

func TestEngineBridgePackedPathsTraversalAndStreamingRows(t *testing.T) {
	bridgeNoStreams(t)
	_, root, db := testlibrary.Create(t)
	// Packed Engine Desktop Sync Manager track lives under <root>/Engine Library/Music/...
	testfixture.Write(t, root, "Engine Library/Music/Artist/Packed.wav", bytes.Repeat([]byte{0}, 1024))
	bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length) VALUES(1,'Packed','Artist','Music/Artist/Packed.wav','Packed.wav',180)`)
	// Mid-path .. traversal escaping root must be skipped, not probed outside root.
	bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length) VALUES(2,'Escape','Bad','sub/../../../outside.wav','outside.wav',180)`)
	// Streaming rows (empty/NULL path or non-empty streamingSource) must not export as "/.".
	bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,streamingSource,length) VALUES(3,'Tidal Song','DJ',NULL,'','tidal',200)`)
	bridgeExec(t, db, `INSERT INTO Track(id,title,artist,path,filename,length) VALUES(4,'Empty Path','DJ','','',200)`)

	if err := prolink.SyncEngineToPioneerExport(root); err != nil {
		t.Fatal(err)
	}
	lib := bridgeReadLibrary(t, root)
	if len(lib.Tracks) != 1 {
		t.Fatalf("expected only packed local track exported, got %d: %+v", len(lib.Tracks), lib.Tracks)
	}
	if lib.Tracks[0].Path != "/Engine Library/Music/Artist/Packed.wav" || lib.Tracks[0].FileSize != 1024 {
		t.Fatalf("packed track misresolved: %+v", lib.Tracks[0])
	}
}

// Completion is independent of the caller and never requires another OFF/ON.
func bridgeWaitExportComplete(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/.enginex-progress"))
		var p struct{ Stamp string }
		if err == nil && json.Unmarshal(b, &p) == nil {
			stamp, err := os.ReadFile(filepath.Join(root, "PIONEER/rekordbox/.enginex-stamp"))
			if err == nil && string(stamp) == p.Stamp {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("background export did not finish automatically")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
