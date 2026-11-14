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
// of it plain JSON in BridgeAnalysis (#272): rows must be stored compressed,
// and still decode to the full source (every waveform section included).
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
	for name, b := range map[string][]byte{"sourceJSON": source, "audioJSON": audio} {
		if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
			t.Fatalf("%s stored uncompressed: %.40q", name, b)
		}
	}
	plain, err := UnpackAnalysis(source, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(source) >= len(plain) {
		t.Fatalf("compressed row %d bytes, plain %d", len(source), len(plain))
	}
	var got struct {
		Track    rekordbox.Track
		Analysis *rekordbox.Analysis
	}
	if err = json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got.Track.ID != 1 || got.Analysis == nil || len(got.Analysis.Waveforms) != 3 || got.Analysis.Structure == nil {
		t.Fatalf("source lost in compression: %+v", got)
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
