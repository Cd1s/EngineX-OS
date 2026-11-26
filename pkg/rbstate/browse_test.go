package rbstate

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeAndSaveBrowseIndex(t *testing.T) {
	s := New("11111111-2222-3333-4444-555555555555")
	s.Tracks["contents/a.mp3"] = &Track{
		Key:          "contents/a.mp3",
		EngineID:     10,
		Rel:          "Contents/a.mp3",
		AnalysisPath: "/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT",
		ArtworkPath:  "/PIONEER/Artwork/00001/a1.jpg",
		Duration:     215.5,
		SampleRate:   44100,
	}
	s.Tracks["contents/b.mp3"] = &Track{
		Key:          "contents/b.mp3",
		EngineID:     2,
		Rel:          "Contents/b.mp3",
		AnalysisPath: "/PIONEER/USBANLZ/P001/00000002/ANLZ0000.DAT",
		ArtworkPath:  "/PIONEER/Artwork/00001/a1.jpg", // duplicate artwork must deduplicate
		Duration:     180.0,
		SampleRate:   0, // defaults to 44100
	}
	s.Tracks["contents/native.mp3"] = &Track{
		Key:          "contents/native.mp3",
		EngineID:     99,
		AnalysisPath: "/PIONEER/USBANLZ/P001/00000099/ANLZ0000.DAT",
		Native:       true, // excluded
	}

	dir := t.TempDir()
	if err := SaveBrowseIndex(dir, s); err != nil {
		t.Fatalf("SaveBrowseIndex: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, BrowseIndexName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), raw)
	}
	if lines[0] != "RBB1 11111111-2222-3333-4444-555555555555" {
		t.Fatalf("header = %q", lines[0])
	}
	if lines[1] != "T\t2\t180.000\t44100\t/PIONEER/USBANLZ/P001/00000002/ANLZ0000.DAT" {
		t.Fatalf("line 1 = %q", lines[1])
	}
	if lines[2] != "T\t10\t215.500\t44100\t/PIONEER/USBANLZ/P001/00000001/ANLZ0000.DAT" {
		t.Fatalf("line 2 = %q", lines[2])
	}
	wantStem := DirectArtworkStem("/PIONEER/Artwork/00001/a1.jpg")
	if decoded, err := base64.RawURLEncoding.DecodeString(wantStem); err != nil || len(decoded) != 20 {
		t.Fatalf("stem %q invalid: len=%d err=%v", wantStem, len(decoded), err)
	}
	if lines[3] != "A\t"+wantStem+"\t/PIONEER/Artwork/00001/a1.jpg" {
		t.Fatalf("line 3 = %q, want stem %q", lines[3], wantStem)
	}
}
