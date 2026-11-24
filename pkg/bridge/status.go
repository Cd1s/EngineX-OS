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

// LibraryHasFile reports whether the stick's library has a track whose file is called name. A
// remount waits while a deck holds Engine's copy of such a track (#301). By name only: the
// export's file size may be out of date, and a remount too many only waits. True when it
// cannot tell.
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

// HasPopulatedLibrary reports whether mount carries an Engine library to show instead of the
// stock rekordbox view: a readable m.db with a valid Information row and at least one track,
// and the bridge's snapshot or playlists beside it. Stock Engine creates an empty skeleton on
// a writable stick and adds a track to it for each one the DJ loads from the export: a skeleton
// with a few tracks is not a library (#301). Anything missing, linked or unreadable is false, so
// the UI falls back to the stock view.
func HasPopulatedLibrary(mount string) bool {
	if mount == "" || !filepath.IsAbs(mount) {
		return false
	}
	engDir := filepath.Join(mount, "Engine Library")
	dbDir := filepath.Join(engDir, "Database2")
	p := filepath.Join(dbDir, "m.db")
	for _, sub := range []string{engDir, dbDir, p} {
		lst, err := os.Lstat(sub)
		if err != nil || lst.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return false
	}
	db, err := openRO(p, 1500)
	if err != nil {
		return false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var ok int
	err = db.QueryRow(`SELECT CASE WHEN EXISTS(SELECT 1 FROM Information WHERE id=1 AND trim(COALESCE(uuid,''))<>'') AND EXISTS(SELECT 1 FROM Track) THEN 1 ELSE 0 END`).Scan(&ok)
	if err != nil || ok != 1 {
		return false
	}
	for _, name := range []string{rbstate.SnapshotName, oldStateName} {
		if st, err := os.Lstat(filepath.Join(dbDir, name)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	var playlists int
	return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Playlist)`).Scan(&playlists) == nil && playlists == 1
}
