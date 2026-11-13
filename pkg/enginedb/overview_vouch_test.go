package enginedb

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The vouch holds only while nobody has imported since: every import writes rb-bridge's
// own database, an earlier build's on another deck too, and that build leaves overviews
// without band maxima.
func TestOverviewVouchEndsWithTheNextImport(t *testing.T) {
	dir := t.TempDir()
	if err := VouchOverviews(dir, nil); err == nil {
		t.Fatal("vouched for a library rb-bridge has no state of")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('a',1,'x')`); err != nil {
		t.Fatal(err)
	}
	vouch := filepath.Join(dir, overviewVouchFile)
	for _, earlier := range []string{"", "1\n"} { // none, and the mark of the build before this file had a meaning
		if earlier != "" {
			if err := os.WriteFile(vouch, []byte(earlier), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := OverviewVouch(dir); ok {
			t.Fatalf("vouched for with %q", earlier)
		}
	}
	raw := []string{"../Contents/unreadable.mp3"}
	if err := VouchOverviews(dir, raw); err != nil {
		t.Fatal(err)
	}
	if got, ok := OverviewVouch(dir); !ok || !slices.Equal(got, raw) {
		t.Fatal(got, ok)
	}
	// A symlink in its place vouches for nothing: it could lead to another library's.
	moved := filepath.Join(t.TempDir(), "vouch")
	if err := os.Rename(vouch, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, vouch); err != nil {
		t.Fatal(err)
	}
	if _, ok := OverviewVouch(dir); ok {
		t.Fatal("vouched for through a symlink")
	}
	if err := VouchOverviews(dir, raw); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if err := os.Remove(vouch); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, vouch); err != nil {
		t.Fatal(err)
	}
	if _, ok := OverviewVouch(dir); !ok {
		t.Fatal("the vouch did not survive the move back")
	}
	// The import: every track it encodes gets a new fingerprint.
	if _, err := db.Exec(`UPDATE BridgeState SET fingerprint='y'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := OverviewVouch(dir); ok {
		t.Fatal("still vouched for after an import")
	}
}
