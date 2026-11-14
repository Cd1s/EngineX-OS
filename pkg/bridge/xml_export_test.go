package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	_ "modernc.org/sqlite"
)

func createTestEngineDB(t *testing.T, mountPath string) {
	dbDir := filepath.Join(mountPath, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "m.db")
	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE Track (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pdbImportKey INTEGER,
		title TEXT,
		artist TEXT,
		album TEXT,
		genre TEXT,
		comment TEXT,
		composer TEXT,
		remixer TEXT,
		label TEXT,
		year INTEGER,
		key INTEGER,
		rating INTEGER,
		bpmAnalyzed REAL,
		bpm INTEGER,
		length INTEGER,
		bitrate INTEGER,
		fileBytes INTEGER,
		filename TEXT,
		path TEXT,
		fileType TEXT
	);
	CREATE TABLE PerformanceData (
		trackId INTEGER PRIMARY KEY,
		trackData BLOB,
		overviewWaveFormData BLOB,
		beatData BLOB,
		quickCues BLOB,
		loops BLOB,
		thirdPartySourceId INTEGER
	);
	CREATE TABLE Playlist (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT,
		parentListId INTEGER
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

	// Track 1: Has 2 hot cues and 1 loop, 44100 Hz
	// Build trackData: 8 bytes sampleRate (44100.0) + 8 bytes samples (44100 * 180) + 28 bytes padding = 44 bytes
	var td [44]byte
	binary.BigEndian.PutUint64(td[:8], math.Float64bits(44100.0))
	binary.BigEndian.PutUint64(td[8:16], 44100*180)
	tdEnc, _ := enginedb.QCompress(td[:])

	// Build quickCues:
	// 8 bytes: count=8
	// Slot 0 (pad 1): label "Intro", pos=44100 (1.000s), ARGB: 255, 255, 0, 0 (Red)
	// Slot 1 (pad 2): label "Break", pos=44100*30 (30.000s), ARGB: 255, 0, 255, 0 (Green)
	// Slot 2-7: unset (pos=-1)
	// Main cue: pos=0 (0.000s)
	var qc bytes.Buffer
	var cnt [8]byte
	binary.BigEndian.PutUint64(cnt[:], 8)
	qc.Write(cnt[:])

	// Slot 0
	lbl0 := []byte("Intro")
	qc.WriteByte(byte(len(lbl0)))
	qc.Write(lbl0)
	var pos0 [8]byte
	binary.BigEndian.PutUint64(pos0[:], math.Float64bits(44100.0))
	qc.Write(pos0[:])
	qc.Write([]byte{255, 255, 0, 0}) // Red #FF0000

	// Slot 1
	lbl1 := []byte("Break")
	qc.WriteByte(byte(len(lbl1)))
	qc.Write(lbl1)
	var pos1 [8]byte
	binary.BigEndian.PutUint64(pos1[:], math.Float64bits(44100.0*30.0))
	qc.Write(pos1[:])
	qc.Write([]byte{255, 0, 255, 0}) // Green #00FF00

	// Slots 2-7 unset
	for i := 2; i < 8; i++ {
		qc.WriteByte(0)
		var unset [8]byte
		binary.BigEndian.PutUint64(unset[:], math.Float64bits(-1.0))
		qc.Write(unset[:])
		qc.Write([]byte{0, 0, 0, 0})
	}
	// Main Cue (0.0s)
	var mainCue [8]byte
	binary.BigEndian.PutUint64(mainCue[:], math.Float64bits(0.0))
	qc.Write(mainCue[:])
	qc.Write(make([]byte, 10)) // main cue tail padding
	qcEnc := dbhealer.EncodeQCompress(qc.Bytes())

	// Build loops:
	// raw[0] = 8 (count), raw[1:8] = 0
	// Slot 0: Loop "Chorus", start=44100*60 (60.0s), end=44100*90 (90.0s), flags=1, 1, ARGB=255, 0, 0, 255 (#0000FF Blue)
	var lp bytes.Buffer
	lp.WriteByte(8)
	lp.Write(make([]byte, 7))
	// Loop 0
	lpName := []byte("Chorus")
	lp.WriteByte(byte(len(lpName)))
	lp.Write(lpName)
	var lpStart, lpEnd [8]byte
	binary.LittleEndian.PutUint64(lpStart[:], math.Float64bits(44100.0*60.0))
	binary.LittleEndian.PutUint64(lpEnd[:], math.Float64bits(44100.0*90.0))
	lp.Write(lpStart[:])
	lp.Write(lpEnd[:])
	lp.Write([]byte{1, 1})
	lp.Write([]byte{255, 0, 0, 255}) // Blue #0000FF

	// Slots 1-7 unused
	for i := 1; i < 8; i++ {
		lp.WriteByte(0)
		var un [8]byte
		binary.LittleEndian.PutUint64(un[:], math.Float64bits(-1.0))
		lp.Write(un[:])
		lp.Write(un[:])
		lp.Write([]byte{0, 0})
		lp.Write([]byte{0, 0, 0, 0})
	}

	_, err = db.Exec(`
		INSERT INTO Track (
			id, pdbImportKey, title, artist, album, genre, comment, composer,
			remixer, label, year, key, rating, bpmAnalyzed, bpm, length,
			bitrate, fileBytes, filename, path, fileType
		) VALUES (
			1, 999, 'Summer Jam', 'DJ Sunny', 'Summer 2026', 'House', 'Great track', 'Composer Sunny',
			'Remixer Cool', 'Cool Records', 2026, 0, 5, 128.0, 12800, 180,
			320, 7200000, 'jam.mp3', '../Music/jam.mp3', 'mp3'
		);
		INSERT INTO PerformanceData (
			trackId, trackData, quickCues, loops
		) VALUES (
			1, ?, ?, ?
		);
		INSERT INTO Playlist (id, title, parentListId) VALUES (1, 'Favorites', NULL);
		INSERT INTO PlaylistEntity (id, listId, trackId, nextEntityId) VALUES (1, 1, 1, 0);
	`, tdEnc, qcEnc, lp.Bytes())
	if err != nil {
		t.Fatal(err)
	}
}

func TestExportRekordboxXML(t *testing.T) {
	mount := t.TempDir()
	createTestEngineDB(t, mount)

	// Test macOS export
	summary, err := bridge.ExportRekordboxXML(context.Background(), bridge.XMLExportOptions{
		MountPath: mount,
		TargetOS:  "mac",
	})
	if err != nil {
		t.Fatalf("ExportRekordboxXML: %v", err)
	}

	if summary.TotalTracks != 1 {
		t.Errorf("TotalTracks = %d, want 1", summary.TotalTracks)
	}
	if summary.CuesCount != 3 { // 2 hot cues + 1 main cue
		t.Errorf("CuesCount = %d, want 3", summary.CuesCount)
	}
	if summary.LoopsCount != 1 {
		t.Errorf("LoopsCount = %d, want 1", summary.LoopsCount)
	}
	if summary.Playlists != 1 {
		t.Errorf("Playlists = %d, want 1", summary.Playlists)
	}

	xmlPath := filepath.Join(mount, "rekordbox.xml")
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatal(err)
	}
	xmlStr := string(data)

	// Check TrackID mapped from pdbImportKey=999
	if !strings.Contains(xmlStr, `TrackID="999"`) {
		t.Errorf("missing TrackID 999: %s", xmlStr)
	}
	// Check location
	driveName := filepath.Base(mount)
	expectedLoc := "file://localhost/Volumes/" + driveName + "/Music/jam.mp3"
	if !strings.Contains(xmlStr, expectedLoc) {
		t.Errorf("expected location %s in:\n%s", expectedLoc, xmlStr)
	}
	// Check cues
	if !strings.Contains(xmlStr, `Name="Intro" Type="0" Start="1.000" Num="0" Red="255" Green="0" Blue="0"`) {
		t.Errorf("missing Intro hot cue in:\n%s", xmlStr)
	}
	if !strings.Contains(xmlStr, `Name="Break" Type="0" Start="30.000" Num="1" Red="0" Green="255" Blue="0"`) {
		t.Errorf("missing Break hot cue in:\n%s", xmlStr)
	}
	// Check loop: pad 0 is already occupied by Intro (Num="0"), so loop gracefully becomes Memory Loop (Num="-1")
	if !strings.Contains(xmlStr, `Name="Chorus" Type="4" Start="60.000" End="90.000" Num="-1" Red="0" Green="0" Blue="255"`) {
		t.Errorf("missing Chorus loop in:\n%s", xmlStr)
	}
	// Check main cue
	if !strings.Contains(xmlStr, `Type="0" Start="0.000" Num="-1"`) {
		t.Errorf("missing Main Cue in:\n%s", xmlStr)
	}
	// Check playlist
	if !strings.Contains(xmlStr, `<NODE Type="1" Name="Favorites" KeyType="0" Entries="1">`) || !strings.Contains(xmlStr, `<TRACK Key="999"`) {
		t.Errorf("missing Favorites playlist with track key 999 in:\n%s", xmlStr)
	}

	// Python verification
	cmd := exec.Command("python3", "-c", `
import xml.etree.ElementTree as ET
tree = ET.parse("`+xmlPath+`")
root = tree.getroot()
assert root.tag == "DJ_PLAYLISTS"
t = root.find("COLLECTION").find("TRACK")
assert t.attrib["TrackID"] == "999"
marks = t.findall("POSITION_MARK")
assert len(marks) == 4
loop = [m for m in marks if m.attrib["Type"] == "4"][0]
assert loop.attrib["Start"] == "60.000"
assert loop.attrib["End"] == "90.000"
assert loop.attrib["Blue"] == "255"
p = root.find(".//NODE[@Type='1']")
assert p.attrib["Name"] == "Favorites"
assert p.find("TRACK").attrib["Key"] == "999"
print("bridge xml export validated OK with python")
`)
	out, pyErr := cmd.CombinedOutput()
	if pyErr != nil {
		t.Fatalf("python validation failed: %s (%v)", string(out), pyErr)
	}
	t.Logf("python output: %s", strings.TrimSpace(string(out)))

	// Test backup of old xml
	summary2, err := bridge.ExportRekordboxXML(context.Background(), bridge.XMLExportOptions{
		MountPath:   mount,
		TargetOS:    "win",
		DriveLetter: "F",
	})
	if err != nil {
		t.Fatalf("Second export: %v", err)
	}
	if summary2.TotalTracks != 1 {
		t.Errorf("Second TotalTracks: %d", summary2.TotalTracks)
	}
	// Verify backup file exists
	bakPath := filepath.Join(mount, "rekordbox.xml.bak")
	if _, err := os.Stat(bakPath); err != nil {
		t.Errorf("expected backup file %s: %v", bakPath, err)
	}
	// Verify new file has Windows drive letter F:
	newXML, _ := os.ReadFile(xmlPath)
	if !strings.Contains(string(newXML), "file://localhost/F:/Music/jam.mp3") {
		t.Errorf("missing Windows drive F: location in:\n%s", string(newXML))
	}
}
