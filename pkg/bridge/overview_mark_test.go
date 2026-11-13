package bridge

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// The mark of a completed library stands until the next import. Review of 0bf0f1e: with
// rb-bridge's database in WAL mode an import commits to the log and leaves the header the
// mark holds as it was; and beside a journal the header is that of a write cut short.
// Neither is a generation to mark a library with.
func TestImportGenerationChangesWithEveryImportOrIsNone(t *testing.T) {
	dir := t.TempDir()
	if g := importGeneration(dir); g != "" {
		t.Fatal("a generation without rb-bridge's database:", g)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, stateName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('a',1,'x')`)
	first := importGeneration(dir)
	exec(`UPDATE BridgeState SET fingerprint='y'`) // an import: every track it encodes gets a new fingerprint
	second := importGeneration(dir)
	if first == "" || second == "" || first == second {
		t.Fatalf("an import left the generation as it was: %q %q", first, second)
	}
	writeMark(dir, overviewName, second)
	journal := filepath.Join(dir, stateName+"-journal")
	if err := os.WriteFile(journal, []byte("journal"), 0o644); err != nil {
		t.Fatal(err)
	}
	if overviewsCompleted(dir) {
		t.Fatal("marked beside the journal of a write cut short")
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if !overviewsCompleted(dir) {
		t.Fatal("fixture: the mark does not stand")
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatal(mode, err)
	}
	header := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, stateName))
		if err != nil || len(b) < 100 {
			t.Fatal(len(b), err)
		}
		return string(b[:100])
	}
	before := header()
	exec(`UPDATE BridgeState SET fingerprint='z'`) // the import, with db still open
	if header() != before {
		t.Fatal("fixture: the import changed the header, this is not the reviewed case")
	}
	if g := importGeneration(dir); g != "" {
		t.Fatal("a generation for a database whose header does not show its imports:", g)
	}
}
