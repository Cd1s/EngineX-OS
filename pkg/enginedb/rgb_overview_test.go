package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// rgbTestOverview is an overview blob (stored or RGB) of a track this many samples per bin long.
func rgbTestOverview(t testing.TB, samplesPerBin float64) []byte {
	return rgbTestOverviewBins(t, samplesPerBin, 0)
}

// rgbTestOverviewBins: fill tells two analyses of one track apart (Engine's RGB one is 0x20 here).
func rgbTestOverviewBins(t testing.TB, samplesPerBin float64, fill byte) []byte {
	t.Helper()
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint64(1024))
	binary.Write(&b, binary.BigEndian, uint64(1024))
	binary.Write(&b, binary.BigEndian, samplesPerBin)
	b.Write(bytes.Repeat([]byte{fill}, 1024*3))
	b.Write([]byte{fill, fill, fill})
	blob, err := QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// rgbTestLibrary writes an m.db with the columns PruneRGBOverviews reads: id -> path, overview.
func rgbTestLibrary(t testing.TB, path string, tracks map[int64]string, overviews map[int64][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE Information (uuid TEXT)`, `INSERT INTO Information VALUES ('lib-uuid')`,
		`CREATE TABLE Track (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT)`,
		`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for id, p := range tracks {
		if _, err = db.Exec(`INSERT INTO Track(id, path) VALUES (?, ?)`, id, p); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO PerformanceData VALUES (?, ?)`, id, overviews[id]); err != nil {
			t.Fatal(err)
		}
	}
}

func writeRGB(t testing.TB, dir, uuid, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, "OverviewData", uuid, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Real stick 2026-11-19: Fons et Origo (20718.8 samples per bin) drew Centauri's RGB overview
// (17803), and 140 files were the 4 bytes of an empty one; Engine never rebuilds either.
func TestPruneRGBOverviewsRemovesOtherTracksAndEmptyOnes(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "m.db")
	stored := rgbTestOverviewBins(t, 17803, 0x10)
	replayed := append([]byte(nil), stored...) // the same bins; a trailer of its own still counts
	if raw, err := QUncompress(stored, 1<<20); err == nil {
		raw[len(raw)-1] = 0x7f
		replayed, _ = QCompress(raw)
	}
	rgbTestLibrary(t, lib, map[int64]string{
		1: "../Centauri.flac", 2: "../Fons.flac", 3: "../Empty.flac", 5: "", 6: "../Short.mp3", 7: "shadow-v3/Copy.flac",
	}, map[int64][]byte{
		1: rgbTestOverview(t, 17803), 2: rgbTestOverview(t, 20718.8), 3: rgbTestOverview(t, 15000),
		6: rgbTestOverview(t, 1292), 7: stored,
	})
	keep := []string{
		writeRGB(t, dir, "lib-uuid", "1.rgb", rgbTestOverviewBins(t, 17804.1, 0x20)), // its own, a decoder's trim apart
		writeRGB(t, dir, "lib-uuid", "5.rgb", rgbTestOverview(t, 9000)),              // a track without a stored overview
		writeRGB(t, dir, "lib-uuid", "6.rgb", rgbTestOverviewBins(t, 1292+15, 0x20)), // a 30 s track, 15 400 samples trimmed
		writeRGB(t, dir, "lib-uuid", "notes.txt", nil),
		writeRGB(t, dir, "other-uuid", "2.rgb", rgbTestOverview(t, 17803)), // another library's
	}
	gone := []string{
		writeRGB(t, dir, "lib-uuid", "2.rgb", rgbTestOverview(t, 17803)), // Centauri's under Fons's id
		writeRGB(t, dir, "lib-uuid", "3.rgb", []byte{0, 0, 0, 0}),
		writeRGB(t, dir, "lib-uuid", "4.rgb", rgbTestOverview(t, 17803)), // no track 4
		writeRGB(t, dir, "lib-uuid", "7.rgb", replayed),                  // the old replay's: the row's own bins
	}
	n, err := PruneRGBOverviews(context.Background(), dir, lib)
	if err != nil || n != len(gone) {
		t.Fatalf("removed %d, %v; want %d", n, err, len(gone))
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s kept", p)
		}
	}
	// Nothing left to do: the next mount removes nothing.
	if n, err = PruneRGBOverviews(context.Background(), dir, lib); err != nil || n != 0 {
		t.Fatalf("second pass removed %d, %v", n, err)
	}
}
