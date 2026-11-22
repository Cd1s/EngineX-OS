package enginedb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A 21,352-track FAT32 stick needed a 7.2 GB .rb-bridge-state.db, almost all
// of it plain JSON in BridgeAnalysis (#272): rows must stay small, and the
// selected scrolling waveform must still decode byte-for-byte. Since #253 a row
// names the track's ANLZ files instead of holding a compressed copy of the
// waveform, and the detail decodes from those files.
func TestBridgeAnalysisRowsAreCompressedAndLossless(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	l, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err = Sync(context.Background(), mount, l, Options{DatabaseDir: dir}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var source, audio []byte
	if err = db.QueryRow(`SELECT sourceJSON, audioJSON FROM BridgeAnalysis WHERE trackId=1`).Scan(&source, &audio); err != nil {
		t.Fatal(err)
	}
	if len(audio) < 2 || audio[0] != 0x1f || audio[1] != 0x8b {
		t.Fatalf("audioJSON stored uncompressed: %.40q", audio)
	}
	plain, err := UnpackAnalysis(source, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Track  rekordbox.Track
		Detail []string
	}
	if err = json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got.Track.ID != 1 || len(source) > 128 || len(got.Detail) != 2 || got.Detail[0] != "PIONEER/USBANLZ/P001/ANLZ0000.2EX" || got.Detail[1] != "PIONEER/USBANLZ/P001/ANLZ0000.EXT" {
		t.Fatalf("source row of %d bytes: %+v", len(source), got)
	}
	a := BridgeDetail(source, mount, 64<<20)
	if w := displayAnalysisWaveform(a); w == nil || w.Tag != "PWV7" || w.EntryBytes != 3 || !bytes.Equal(w.Data, []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120}) {
		t.Fatalf("detail not decoded from the ANLZ file: %+v", a)
	}
	if b, err := UnpackAnalysis(audio, 1<<20); err != nil || !json.Valid(b) {
		t.Fatalf("audioJSON: %v %q", err, b)
	}
}

func TestUnpackAnalysisReadsPlainRowsAndBoundsSize(t *testing.T) {
	plain := []byte(`{"Analysis":{"DurationMS":1}}`)
	if b, err := UnpackAnalysis(plain, 1<<10); err != nil || !bytes.Equal(b, plain) {
		t.Fatalf("plain row written by an earlier build: %q %v", b, err)
	}
	if _, err := UnpackAnalysis(plain, 4); err == nil {
		t.Fatal("plain row over the limit accepted")
	}
	// A small row that expands past the limit must not be read in full.
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(bytes.Repeat([]byte{' '}, 1<<20))
	w.Close()
	if _, err := UnpackAnalysis(buf.Bytes(), 1<<10); err == nil {
		t.Fatal("row expanding past the limit accepted")
	}
	if _, err := UnpackAnalysis([]byte{0x1f, 0x8b, 0, 0}, 1<<10); err == nil {
		t.Fatal("corrupt compressed row accepted")
	}
}
