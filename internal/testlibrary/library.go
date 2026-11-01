// Package testlibrary creates fixtures using the exact captured Engine DDL.
package testlibrary

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func Create(t *testing.T) (path, root string, db *sql.DB) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../docs/re-report/ENGINE-5.0.4-m.db-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	schema := strings.ReplaceAll(string(b), "CREATE TABLE sqlite_sequence(name,seq);", "")
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('test-library-uuid',3,0,2)"); err != nil {
		t.Fatal(err)
	}
	return
}
