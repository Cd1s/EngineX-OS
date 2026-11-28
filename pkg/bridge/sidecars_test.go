package bridge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// cleanSQLiteSidecars before a replacement (the swap of the next library, the first library's
// install): it removes the stale sidecars it checked and nothing else (#336 Go review R11,
// finding 2, as a class). A sidecar made or written after the check is the work of whoever
// opened the database meanwhile.
func TestCleanSQLiteSidecarsRemovesOnlyWhatItChecked(t *testing.T) {
	defer func() { sidecarsChecked = nil }()
	newDB := func(t *testing.T) string {
		db := filepath.Join(t.TempDir(), "m.db")
		if err := os.WriteFile(db, []byte("library"), 0o644); err != nil {
			t.Fatal(err)
		}
		return db
	}
	write := func(t *testing.T, path string, b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	frames := []byte("frames of a commit")

	t.Run("stale sidecars present at the check are removed", func(t *testing.T) {
		db := newDB(t)
		write(t, db+"-journal", make([]byte, 512)) // SQLite's committed PERSIST journal
		write(t, db+"-wal", nil)
		write(t, db+"-shm", make([]byte, 32768))
		sidecarsChecked = nil
		if err := cleanSQLiteSidecars(db); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			if _, err := os.Lstat(db + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stale m.db%s left: err=%v", suffix, err)
			}
		}
	})

	t.Run("sidecars made after the check are kept", func(t *testing.T) {
		db := newDB(t)
		ran := false
		sidecarsChecked = func(p string) {
			if p != db {
				return
			}
			ran = true
			write(t, db+"-wal", frames)
			write(t, db+"-shm", make([]byte, 32768))
			write(t, db+"-journal", frames)
		}
		err := cleanSQLiteSidecars(db)
		if !ran {
			t.Fatal("the check hook never ran")
		}
		if err != nil {
			t.Errorf("cleanSQLiteSidecars: %v, want nil: it checked no sidecar and removes none", err)
		}
		for _, suffix := range []string{"-wal", "-journal"} {
			if b, rerr := os.ReadFile(db + suffix); rerr != nil || !bytes.Equal(b, frames) {
				t.Errorf("m.db%s was absent at the check and is gone or changed: %v", suffix, rerr)
			}
		}
		if _, serr := os.Lstat(db + "-shm"); serr != nil {
			t.Errorf("m.db-shm was absent at the check and is gone: %v", serr)
		}
	})

	t.Run("a checked sidecar written since stops the replacement", func(t *testing.T) {
		db := newDB(t)
		write(t, db+"-wal", nil)
		write(t, db+"-shm", make([]byte, 32768))
		sidecarsChecked = func(p string) {
			if p == db {
				write(t, db+"-wal", frames) // the same file, now with a commit in it
			}
		}
		if err := cleanSQLiteSidecars(db); err == nil {
			t.Error("cleanSQLiteSidecars: nil, want an error: the -wal it checked empty now holds work")
		}
		if b, rerr := os.ReadFile(db + "-wal"); rerr != nil || !bytes.Equal(b, frames) {
			t.Errorf("m.db-wal written after the check is gone or changed: %v", rerr)
		}
		if _, serr := os.Lstat(db + "-shm"); serr != nil {
			t.Errorf("m.db-shm of the WAL written after the check is gone: %v", serr)
		}
	})

	t.Run("a checked sidecar replaced since stops the replacement", func(t *testing.T) {
		db := newDB(t)
		write(t, db+"-shm", make([]byte, 32768))
		sidecarsChecked = func(p string) {
			if p != db {
				return
			}
			other := db + "-shm.other"
			write(t, other, make([]byte, 32768))
			if err := os.Rename(other, db+"-shm"); err != nil {
				t.Fatal(err)
			}
		}
		if err := cleanSQLiteSidecars(db); err == nil {
			t.Error("cleanSQLiteSidecars: nil, want an error: the -shm it checked is another file now")
		}
		if _, serr := os.Lstat(db + "-shm"); serr != nil {
			t.Errorf("the other file at m.db-shm is gone: %v", serr)
		}
	})
}
