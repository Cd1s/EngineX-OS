package bridge

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

func roURI(p string) string { return (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro"}).String() }

// openRO opens an SQLite file read-only.
func openRO(p string, busyMS int) (*sql.DB, error) {
	return sql.Open("sqlite", roURI(p)+fmt.Sprintf("&_pragma=busy_timeout(%d)&_pragma=query_only(1)", busyMS))
}

// LibraryHasFile reports whether the stick's library has a track whose file is called name: a
// remount waits while a deck holds Engine's copy of one (#301). By name only, since a remount too
// many only waits; true when it cannot tell.
func LibraryHasFile(mount, name string) bool {
	p := filepath.Join(mount, databaseSubdir, "m.db")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return false
	}
	db, err := openRO(p, 1500)
	if err != nil {
		return true
	}
	defer db.Close()
	var has bool
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Track WHERE filename=?)`, name).Scan(&has)
	return err != nil || has
}

// HasPopulatedLibrary reports whether mount carries an Engine library to show instead of the stock
// rekordbox view: a regular, unlinked m.db with a valid Information row and a track, and the
// bridge's state or a playlist beside it. The skeleton stock Engine makes, with a row for each
// track loaded from the export, is no library (#301); anything unreadable is false.
func HasPopulatedLibrary(mount string) bool {
	if !filepath.IsAbs(mount) {
		return false
	}
	dbDir := filepath.Join(mount, databaseSubdir)
	p := filepath.Join(dbDir, "m.db")
	for _, sub := range []string{filepath.Dir(dbDir), dbDir, p} {
		if st, err := os.Lstat(sub); err != nil || st.Mode()&os.ModeSymlink != 0 || sub == p && (!st.Mode().IsRegular() || st.Size() == 0) {
			return false
		}
	}
	db, err := openRO(p, 1500)
	if err != nil {
		return false
	}
	defer db.Close()
	var ok, lists bool
	if db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Information WHERE id=1 AND trim(COALESCE(uuid,''))<>'') AND EXISTS(SELECT 1 FROM Track)`).Scan(&ok) != nil || !ok {
		return false
	}
	for _, name := range []string{rbstate.SnapshotName, oldStateName} {
		if st, err := os.Lstat(filepath.Join(dbDir, name)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	// Asked on its own: a library without that table is one all the same when the bridge's state is there.
	return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Playlist)`).Scan(&lists) == nil && lists
}
