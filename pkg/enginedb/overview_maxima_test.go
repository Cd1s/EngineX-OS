package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Engine scales each overview band by the band maxima in the last three bytes. rb-bridge
// wrote zeros there, and Engine drew an imported track at a third of its own tracks' height.
func TestOverviewEndsWithBandMaxima(t *testing.T) {
	wave := rekordbox.Waveform{Tag: "PWV7", EntryBytes: 3, Data: []byte{10, 20, 30, 102, 5, 7, 4, 123, 90}}
	p, err := EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{wave}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := QUncompress(p.Overview, 1<<20)
	if err != nil || !bytes.Equal(raw[len(raw)-3:], []byte{102, 123, 90}) {
		t.Fatalf("trailer %v, err %v", raw[len(raw)-3:], err)
	}
}

func TestFillOverviewMaximaOnlyCompletesImportedZeroTrailers(t *testing.T) {
	mount := t.TempDir()
	dir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB, thirdPartySourceId INTEGER)`); err != nil {
		t.Fatal(err)
	}
	overview := func(trailer ...byte) []byte {
		raw := append(make([]byte, 24), 10, 200, 3, 60, 20, 90) // two bins
		blob, err := QCompress(append(raw, trailer...))
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}
	old, done := overview(0, 0, 0), overview(9, 9, 9)
	if _, err := db.Exec(`INSERT INTO PerformanceData VALUES (1,?,1),(2,?,1),(3,?,NULL),(4,x'00',1)`, old, done, old); err != nil {
		t.Fatal(err)
	}
	if err := FillOverviewMaxima(context.Background(), mount); err != nil {
		t.Fatal(err)
	}
	get := func(id int) (blob []byte) {
		if err := db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		return blob
	}
	if raw, err := QUncompress(get(1), 1<<20); err != nil || !bytes.Equal(raw[len(raw)-3:], []byte{60, 200, 90}) || !bytes.Equal(raw[24:30], []byte{10, 200, 3, 60, 20, 90}) {
		t.Fatalf("imported zero trailer: %v %v", raw, err)
	}
	if !bytes.Equal(get(2), done) || !bytes.Equal(get(3), old) || !bytes.Equal(get(4), []byte{0}) {
		t.Fatal("a completed, native or unreadable overview was rewritten")
	}
}
