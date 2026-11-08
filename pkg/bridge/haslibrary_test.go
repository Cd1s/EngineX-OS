package bridge

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestHasLibrary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Engine Library", "Database2") // the space needs URI escaping
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := filepath.Join(dir, "m.db")
	if hasLibrary(dir) {
		t.Fatal("no m.db")
	}
	os.WriteFile(m, nil, 0o644)
	if hasLibrary(dir) {
		t.Fatal("empty file")
	}
	db, err := sql.Open("sqlite", m)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if hasLibrary(dir) {
		t.Fatal("skeleton with an empty Track table")
	}
	if _, err = db.Exec(`INSERT INTO Track DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	if !hasLibrary(dir) {
		t.Fatal("m.db with a track")
	}
	os.WriteFile(m, []byte("not sqlite, but not ours to replace"), 0o644)
	if !hasLibrary(dir) {
		t.Fatal("an unreadable m.db must not be replaced")
	}
}

func TestImportedStampIgnoresSkeleton(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE Track(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	writeStamp(dir, "s1")
	if got := importedStamp(dir); got != "" {
		t.Fatalf("stamp over a 0-track skeleton must not count, got %q", got)
	}
	if _, err = db.Exec(`INSERT INTO Track DEFAULT VALUES`); err != nil {
		t.Fatal(err)
	}
	if got := importedStamp(dir); got != "s1" {
		t.Fatalf("got %q", got)
	}
}
