package rekordbox_test

import (
	"bytes"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEncodeXMLLocation(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "/Volumes/USB/Music/Track 1.mp3",
			want:  "file://localhost/Volumes/USB/Music/Track%201.mp3",
		},
		{
			input: "E:/Contents/Artist/Album/01 Track.mp3",
			want:  "file://localhost/E:/Contents/Artist/Album/01%20Track.mp3",
		},
		{
			input: "/media/USB/Music/Café.wav",
			want:  "file://localhost/media/USB/Music/Caf%C3%A9.wav",
		},
	}
	for _, tc := range tests {
		got := rekordbox.EncodeXMLLocation(tc.input)
		if got != tc.want {
			t.Errorf("EncodeXMLLocation(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseHexRGB(t *testing.T) {
	r, g, b := rekordbox.ParseHexRGB("#FF8000")
	if r != "255" || g != "128" || b != "0" {
		t.Errorf("ParseHexRGB(#FF8000) = (%q, %q, %q), want (255, 128, 0)", r, g, b)
	}
	r, g, b = rekordbox.ParseHexRGB("invalid")
	if r != "" || g != "" || b != "" {
		t.Errorf("ParseHexRGB(invalid) expected empty, got (%q, %q, %q)", r, g, b)
	}
}

func TestXMLDatabaseGenerationAndRoundtrip(t *testing.T) {
	db := rekordbox.NewXMLDatabase()
	db.Collection.Entries = 1
	db.Collection.Tracks = append(db.Collection.Tracks, rekordbox.XMLTrack{
		TrackID:    "101",
		Name:       "Festival Anthem",
		Artist:     "DJ Hero",
		Album:      "Club Hits",
		Genre:      "House",
		Kind:       "MP3 File",
		Size:       10485760,
		TotalTime:  240,
		AverageBpm: "128.00",
		SampleRate: "44100",
		BitRate:    320,
		Location:   rekordbox.EncodeXMLLocation("/Volumes/USB/Music/Anthem.mp3"),
		Tonality:   "8A",
		Rating:     204,
		Tempos: []rekordbox.XMLTempo{
			{Inizio: "0.025", Bpm: "128.00", Metro: "4/4", Battito: 1},
		},
		PositionMarks: []rekordbox.XMLPositionMark{
			{Name: "Intro", Type: "0", Start: "0.025", Num: "0", Red: "255", Green: "0", Blue: "0"},
			{Name: "Drop", Type: "0", Start: "30.000", Num: "1", Red: "0", Green: "255", Blue: "0"},
			{Name: "Outro Loop", Type: "4", Start: "120.000", End: "150.000", Num: "2", Red: "0", Green: "0", Blue: "255"},
			{Name: "", Type: "0", Start: "0.000", Num: "-1"},
		},
	})

	db.Playlists.Root.Count = 1
	db.Playlists.Root.Nodes = append(db.Playlists.Root.Nodes, rekordbox.XMLNode{
		Type:    "1",
		Name:    "Club Set",
		KeyType: "0",
		Entries: 1,
		Tracks: []rekordbox.XMLNodeTrack{
			{Key: "101"},
		},
	})

	var buf bytes.Buffer
	if _, err := db.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	xmlStr := buf.String()
	if !strings.HasPrefix(xmlStr, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Errorf("missing xml header: %s", xmlStr[:40])
	}
	if !strings.Contains(xmlStr, `<DJ_PLAYLISTS Version="1.0.0">`) {
		t.Errorf("missing DJ_PLAYLISTS root: %s", xmlStr)
	}
	if !strings.Contains(xmlStr, `TrackID="101"`) || !strings.Contains(xmlStr, `Type="4"`) {
		t.Errorf("missing track or loop mark: %s", xmlStr)
	}

	var parsed rekordbox.XMLDatabase
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("Unmarshal roundtrip failed: %v", err)
	}
	if parsed.Collection.Entries != 1 || len(parsed.Collection.Tracks) != 1 {
		t.Fatalf("parsed collection: %+v", parsed.Collection)
	}
	tr := parsed.Collection.Tracks[0]
	if tr.TrackID != "101" || tr.Name != "Festival Anthem" {
		t.Errorf("track parsed mismatch: %+v", tr)
	}
	if len(tr.PositionMarks) != 4 {
		t.Fatalf("expected 4 position marks, got %d", len(tr.PositionMarks))
	}
	loop := tr.PositionMarks[2]
	if loop.Type != "4" || loop.Start != "120.000" || loop.End != "150.000" || loop.Num != "2" || loop.Blue != "255" {
		t.Errorf("loop mark mismatch: %+v", loop)
	}

	// Python xml.etree.ElementTree parser verification
	tmpXML := filepath.Join(t.TempDir(), "test_rekordbox.xml")
	if err := os.WriteFile(tmpXML, buf.Bytes(), 0o644); err == nil {
		cmd := exec.Command("python3", "-c", `
import xml.etree.ElementTree as ET
tree = ET.parse("`+tmpXML+`")
root = tree.getroot()
assert root.tag == "DJ_PLAYLISTS"
assert root.attrib["Version"] == "1.0.0"
prod = root.find("PRODUCT")
assert prod.attrib["Name"] == "rekordbox"
tracks = root.findall(".//TRACK")
assert len(tracks) == 2  # 1 in COLLECTION, 1 in PLAYLISTS
c_track = root.find("COLLECTION").find("TRACK")
assert c_track.attrib["TrackID"] == "101"
marks = c_track.findall("POSITION_MARK")
assert len(marks) == 4
loop = [m for m in marks if m.attrib.get("Type") == "4"][0]
assert loop.attrib["Start"] == "120.000"
assert loop.attrib["End"] == "150.000"
assert loop.attrib["Num"] == "2"
assert loop.attrib["Blue"] == "255"
print("python xml.etree verified OK")
`)
		out, pyErr := cmd.CombinedOutput()
		if pyErr != nil {
			t.Fatalf("python XML check failed: %s (%v)", string(out), pyErr)
		}
		t.Logf("python verification: %s", strings.TrimSpace(string(out)))
	}
}
