package dbhealer

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copiedLibrary makes a drive root holding Music/a.mp3 and a carry-over stage with an m.db whose
// tracks have the given paths (a "stream:" prefix marks a streaming track).
func copiedLibrary(t *testing.T, paths ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "STICK")
	stage := filepath.Join(root, ".enginex-carry-test")
	if err := os.MkdirAll(filepath.Join(stage, "Database2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Music", "a.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(stage, "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('carry',3,0,2)")
	for i, p := range paths {
		source := ""
		if s, ok := strings.CutPrefix(p, "stream:"); ok {
			p, source = s, "tidal"
		}
		mustExec(t, db, "INSERT INTO Track(id,path,streamingSource) VALUES(?,?,NULLIF(?,''))", i+1, p, source)
	}
	return stage
}

func TestCheckCopyAcceptsPathsThatResolveOnTheStick(t *testing.T) {
	// Engine's own form, a file deleted on a PC since, and a streaming track with no file.
	r, err := CheckCopy(copiedLibrary(t, "../Music/a.mp3", "../Music/gone.mp3", "stream:tidal:123"))
	if err != nil || r.Tracks != 2 || r.Missing != 1 {
		t.Fatalf("CheckCopy = %+v, %v; want 2 tracks, 1 missing, no error", r, err)
	}
	if r, err = CheckCopy(copiedLibrary(t)); err != nil || r.Tracks != 0 {
		t.Fatalf("an empty library: %+v, %v", r, err)
	}
}

func TestCheckCopyRefusesPathsNotMadeForTheStick(t *testing.T) {
	for name, paths := range map[string][]string{
		"absolute":      {"../Music/a.mp3", "/media/OTHER/Music/a.mp3"},
		"leaves drive":  {"../Music/a.mp3", "../../OTHER/Music/a.mp3"},
		"none resolves": {"Music/a.mp3", "Music/b.mp3"}, // relative to something else than the library folder
		"absolute dots": {"../Music/a.mp3", "/../../x.mp3"},
	} {
		if r, err := CheckCopy(copiedLibrary(t, paths...)); err == nil {
			t.Errorf("%s: %v accepted (%+v)", name, paths, r)
		}
	}
}

func TestCheckCopyRefusesADamagedDatabase(t *testing.T) {
	stage := copiedLibrary(t, "../Music/a.mp3")
	db := filepath.Join(stage, "Database2", "m.db")
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite every page but the header page: the b-trees no longer hold together.
	for i := 4096; i < len(b); i++ {
		b[i] = 0x5a
	}
	if err = os.WriteFile(db, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := CheckCopy(stage); err == nil {
		t.Fatalf("damaged m.db accepted: %+v", r)
	}
	if _, err := CheckCopy(filepath.Join(filepath.Dir(stage), "missing")); err == nil {
		t.Fatal("a stage without m.db accepted")
	}
}
