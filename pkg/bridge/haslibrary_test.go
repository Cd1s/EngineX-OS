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

func TestHasPopulatedLibrary(t *testing.T) {
	mount := filepath.Join(t.TempDir(), "RB7 STICK")
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if HasPopulatedLibrary("") || HasPopulatedLibrary("relative/path") {
		t.Fatal("invalid mount path should be false")
	}
	if HasPopulatedLibrary(mount) {
		t.Fatal("missing m.db should be false")
	}

	mdb := filepath.Join(dbDir, "m.db")
	if err := os.WriteFile(mdb, []byte("corrupt non-sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasPopulatedLibrary(mount) {
		t.Fatal("unreadable m.db must be false for UI selection")
	}
	if err := os.Remove(mdb); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", mdb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`
		CREATE TABLE Information(id INTEGER PRIMARY KEY, uuid TEXT);
		CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT);
		INSERT INTO Information(id, uuid) VALUES(1, 'e7a89052-6441-43bb-9c6d-e3bed7011d00');
	`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if HasPopulatedLibrary(mount) {
		db.Close()
		t.Fatal("stock Engine 0-track skeleton m.db must return false")
	}

	// Track row present but Information.uuid empty -> still invalid (GL-010).
	if _, err = db.Exec(`UPDATE Information SET uuid=''; INSERT INTO Track(id, path) VALUES(1, '../MUSIC/a.wav')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if HasPopulatedLibrary(mount) {
		db.Close()
		t.Fatal("empty Information.uuid must return false")
	}

	// Valid Information.uuid + Track row -> populated library.
	if _, err = db.Exec(`UPDATE Information SET uuid='e7a89052-6441-43bb-9c6d-e3bed7011d00'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if !HasPopulatedLibrary(mount) {
		t.Fatal("populated m.db should return true")
	}

	// Read-only directory and read-only m.db (e.g. HFS+ or ro media) still read cleanly.
	if err := os.Chmod(mdb, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dbDir, 0o755)
		_ = os.Chmod(mdb, 0o644)
	})
	if !HasPopulatedLibrary(mount) {
		t.Fatal("read-only populated m.db should return true")
	}

	// Symlinked Engine Library must be rejected.
	symMount := filepath.Join(t.TempDir(), "SYM")
	if err := os.MkdirAll(symMount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(mount, "Engine Library"), filepath.Join(symMount, "Engine Library")); err != nil {
		t.Fatal(err)
	}
	if HasPopulatedLibrary(symMount) {
		t.Fatal("symlinked Engine Library must return false")
	}
}
