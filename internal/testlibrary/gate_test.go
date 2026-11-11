package testlibrary

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestGateLibrary writes the populated internal-drive Engine Library fixture
// that tools/az01-packer/release-gate.sh bind-mounts at /media/az01-internal
// across first boot, hard power cut, and second boot (#140).
func TestGateLibrary(t *testing.T) {
	dir := os.Getenv("SC5000_GATE_LIBRARY")
	if dir == "" {
		t.Skip("set SC5000_GATE_LIBRARY to write the release-gate internal library")
	}
	ExportDrive(t, dir)
}

func TestExportDrive(t *testing.T) {
	dir := t.TempDir()
	ExportDrive(t, dir)

	dbPath := filepath.Join(dir, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var major, minor, patch int
	if err := db.QueryRow("SELECT schemaVersionMajor, schemaVersionMinor, schemaVersionPatch FROM Information").Scan(&major, &minor, &patch); err != nil {
		t.Fatalf("read Information: %v", err)
	}
	if major != 3 || minor != 0 || patch != 2 {
		t.Fatalf("schema version = %d.%d.%d, want 3.0.2", major, minor, patch)
	}

	var tracks, entities int
	if err := db.QueryRow("SELECT COUNT(*) FROM Track").Scan(&tracks); err != nil {
		t.Fatalf("count Track: %v", err)
	}
	if tracks != 8 {
		t.Fatalf("Track count = %d, want 8", tracks)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM PlaylistEntity").Scan(&entities); err != nil {
		t.Fatalf("count PlaylistEntity: %v", err)
	}
	if entities != 8 {
		t.Fatalf("PlaylistEntity count = %d, want 8", entities)
	}

	rows, err := db.Query("SELECT path FROM Track ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	dbDir := filepath.Dir(dbPath)
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dbDir, rel))
		if err != nil {
			t.Fatalf("read audio %s: %v", rel, err)
		}
		if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
			t.Fatalf("invalid WAV header for %s (%d bytes)", rel, len(b))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
