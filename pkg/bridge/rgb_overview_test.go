package bridge

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func rgbBridgeLibrary(t *testing.T, path string, tracks map[int64]string, overview []byte) {
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
		`CREATE TABLE Information (uuid TEXT)`, `INSERT INTO Information VALUES ('stick-uuid')`,
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
		if _, err = db.Exec(`INSERT INTO PerformanceData VALUES (?, ?)`, id, overview); err != nil {
			t.Fatal(err)
		}
	}
}

// Engine's RGB overview of a track is the file OverviewData/<uuid>/<id>.rgb. A publish that
// gives an id to another audio file must not leave the old track's overview under it, and
// a mount removes what an earlier build left stale (real stick 2026-11-19: Fons et Origo drew
// Centauri's mini overview; 140 empty files drew black).
func TestPublishRemovesRGBOverviewsOfAnotherTrack(t *testing.T) {
	t.Parallel()
	var raw bytes.Buffer
	binary.Write(&raw, binary.BigEndian, [2]uint64{1024, 1024})
	binary.Write(&raw, binary.BigEndian, float64(17803))
	raw.Write(bytes.Repeat([]byte{0}, 1024*3+3))
	overview, err := enginedb.QCompress(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "Engine Library", "Database2")
	stage := filepath.Join(dir, importStageName)
	rgbBridgeLibrary(t, filepath.Join(dir, "m.db"), map[int64]string{1: "../A.flac", 2: "../B.flac"}, overview)
	rgbBridgeLibrary(t, filepath.Join(stage, "m.db"), map[int64]string{1: "../A.flac", 2: "../C.flac"}, overview)
	rgb := func(id string) string { return filepath.Join(dir, "OverviewData", "stick-uuid", id+".rgb") }
	if err := os.MkdirAll(filepath.Dir(rgb("1")), 0o755); err != nil {
		t.Fatal(err)
	}
	raw.Bytes()[30] = 0x20 // Engine's own RGB analysis differs from the stored overview
	engineRGB, err := enginedb.QCompress(raw.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "2"} {
		if err := os.WriteFile(rgb(id), engineRGB, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := renameStage(dir, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rgb("1")); err != nil {
		t.Fatal("A kept its id and lost its RGB overview")
	}
	if _, err := os.Stat(rgb("2")); !os.IsNotExist(err) {
		t.Fatal("B's RGB overview stayed under the id C has in the published library")
	}
	// What an earlier build left: an empty one, at the mount before Engine has the volume.
	if err := os.WriteFile(rgb("1"), []byte{0, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	pruneRGBOverviews(t.Context(), dir, filepath.Join(dir, "m.db"), "")
	if _, err := os.Stat(rgb("1")); !os.IsNotExist(err) {
		t.Fatal("an empty RGB overview stayed")
	}
}
