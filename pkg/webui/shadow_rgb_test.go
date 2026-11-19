package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func rgbTestRaw(samplesPerBin float64, fill byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, [2]uint64{1024, 1024})
	binary.Write(&b, binary.BigEndian, samplesPerBin)
	b.Write(bytes.Repeat([]byte{fill}, 1024*3+3))
	return b.Bytes()
}

// The source serves the RGB overview its Engine built for the deck's track, and the
// receiver keeps it beside the copy as <copy>.perf.rgb (LEFT, 2026-11-20: a copy replayed
// the stored 3-band overview as its RGB one and drew it in other colours than the source).
func TestShadowCopyGetsTheSourcesRGBOverview(t *testing.T) {
	data := t.TempDir()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	dir := filepath.Join(lib, "Database2")
	if err := os.MkdirAll(filepath.Join(dir, "OverviewData", "lib-uuid"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := enginedb.QCompress(rgbTestRaw(17803, 0x10))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE Information (uuid TEXT)`, `INSERT INTO Information VALUES ('lib-uuid')`,
		`CREATE TABLE Track (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT)`,
		`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB)`,
		`INSERT INTO Track VALUES (5884, 'Music/Centauri.flac')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO PerformanceData VALUES (5884, ?)`, stored); err != nil {
		t.Fatal(err)
	}
	db.Close()
	engineRGB := rgbTestRaw(17803, 0x20)
	blob, err := enginedb.QCompress(engineRGB)
	if err != nil {
		t.Fatal(err)
	}
	rgbFile := filepath.Join(dir, "OverviewData", "lib-uuid", "5884.rgb")
	if err := os.WriteFile(rgbFile, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(lib, "Music", "Centauri.flac")
	got, err := libraryRGB(context.Background(), track, data, filepath.Join(data, "media"))
	if err != nil || !bytes.Equal(got, engineRGB) {
		t.Fatalf("libraryRGB = %d bytes, %v; want Engine's RGB overview", len(got), err)
	}
	// One that is the stored 3-band overview (an old replay's) is not served.
	if err := os.WriteFile(rgbFile, stored, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := libraryRGB(context.Background(), track, data, filepath.Join(data, "media")); err == nil {
		t.Fatal("an old replay's RGB overview was served as the source's")
	}

	answer := engineRGB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "rgb" {
			t.Errorf("asked %s", r.URL)
		}
		w.Write(answer)
	}))
	defer srv.Close()
	local := filepath.Join(t.TempDir(), "Centauri.flac")
	saveShadowRGB(context.Background(), local, srv.URL+"/peer/decks/2/perf?key=k")
	if b, err := os.ReadFile(local + ".perf.rgb"); err != nil || !bytes.Equal(b, engineRGB) {
		t.Fatalf("sidecar %d bytes, %v", len(b), err)
	}
	// An older source answers the unknown format with EXPF: no sidecar, and the old one goes.
	answer = []byte("EXPF\x01\x00\x00\x00not an overview")
	saveShadowRGB(context.Background(), local, srv.URL+"/peer/decks/2/perf?key=k")
	if _, err := os.Stat(local + ".perf.rgb"); !os.IsNotExist(err) {
		t.Fatal("an EXPF answer was kept as the RGB overview")
	}
}
