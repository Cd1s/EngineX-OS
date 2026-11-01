// Package dbhealer repairs Engine OS 5.0.4 libraries without discarding
// PerformanceData rows or changing their cue points.
package dbhealer

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultMaxSessions = 50

type Report struct {
	Mount             string    `json:"mount"`
	ReadOnly          bool      `json:"read_only"`
	Healthy           bool      `json:"healthy"`
	PlaylistEntities  int       `json:"playlist_entities_relinked"`
	Playlists         int       `json:"playlists_relinked"`
	Waveforms         int       `json:"waveforms_replaced"`
	Loops             int       `json:"loops_aligned"`
	Artwork           int       `json:"artwork_resized"`
	OrphanArtwork     int       `json:"orphan_artwork_references"`
	ReanalysisPending int       `json:"reanalysis_pending"`
	HistoryPruned     bool      `json:"history_pruned"`
	Warnings          []string  `json:"warnings,omitempty"`
	CheckedAt         time.Time `json:"checked_at"`
}

// Open never creates a missing library. A single connection keeps connection-local
// tuning consistent. Read-only opens never recover or create a journal.
func Open(path string, readOnly bool) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "rw")
	q.Add("_pragma", "busy_timeout(3000)")
	if readOnly {
		for _, suffix := range []string{"-wal", "-journal"} {
			if s, e := os.Stat(path + suffix); e == nil && s.Size() > 0 {
				return nil, fmt.Errorf("read-only library has pending %s; recover on writable media first", suffix)
			} else if e != nil && !errors.Is(e, os.ErrNotExist) {
				return nil, e
			}
		}
		q.Set("mode", "ro")
		q.Set("immutable", "1")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "foreign_keys(1)")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func Tune(db *sql.DB, readOnly bool) error {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if readOnly {
		return nil
	}
	for _, p := range []string{"PRAGMA synchronous=NORMAL", "PRAGMA mmap_size=134217728", "PRAGMA temp_store=MEMORY", "PRAGMA optimize"} {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	return nil
}

func transaction(db *sql.DB, f func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Heal runs all m.db mutations atomically. Integrity errors are reported; a
// corrupt library is never replaced with an empty one.
func Heal(db *sql.DB) (Report, error) {
	r := Report{CheckedAt: time.Now().UTC()}
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return r, err
	}
	if check != "ok" {
		return r, fmt.Errorf("database integrity: %s", check)
	}
	var major, minor, patch int
	if err := db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return r, fmt.Errorf("missing library version: %w", err)
	}
	if major != 3 || minor != 0 || patch != 2 {
		return r, fmt.Errorf("unsupported library schema %d.%d.%d (supported 3.0.2)", major, minor, patch)
	}
	if err := Tune(db, false); err != nil {
		return r, err
	}
	err := transaction(db, func(tx *sql.Tx) error {
		// An unrecognized future schema is not a license to rewrite its blobs.
		var schema string
		if err := tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='PerformanceData'").Scan(&schema); err != nil {
			return fmt.Errorf("unsupported library schema: %w", err)
		}
		var err error
		if err = fixSchema(tx); err != nil {
			return err
		}
		if r.PlaylistEntities, err = healChains(tx, false); err != nil {
			return err
		}
		if r.Playlists, err = healChains(tx, true); err != nil {
			return err
		}
		if err = materialize(tx); err != nil {
			return err
		}
		if err = healPerformance(tx, &r); err != nil {
			return err
		}
		if err = healArtwork(tx, &r); err != nil {
			return err
		}
		return tx.QueryRow("SELECT count(*) FROM GuardianReanalysis").Scan(&r.ReanalysisPending)
	})
	r.Healthy = err == nil
	return r, err
}

func writable(path string) (bool, error) {
	s, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(filepath.Dir(path), &fs); err != nil {
		return false, err
	}
	d, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	return fs.Flags&1 == 0 && s.Mode().Perm()&0222 != 0 && d.Mode().Perm()&0222 != 0, nil
}

// HealMount only opens Engine's two known database names; it never scans music.
func HealMount(mount string, maxSessions int) (Report, error) {
	r := Report{Mount: mount, CheckedAt: time.Now().UTC()}
	if maxSessions < 0 {
		return r, fmt.Errorf("maxSessions must not be negative")
	}
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	rw, err := writable(path)
	if err != nil {
		return r, err
	}
	db, err := Open(path, !rw)
	if err != nil {
		return r, err
	}
	if !rw {
		db.Close()
		r, err = InspectMount(mount)
		r.Warnings = append(r.Warnings, "read-only media: inspected without repairs or journal writes")
		return r, err
	}
	r, err = Heal(db)
	closeErr := db.Close()
	r.Mount = mount
	if err != nil {
		return r, err
	}
	if closeErr != nil {
		return r, closeErr
	}
	history := filepath.Join(filepath.Dir(path), "hm.db")
	if _, e := os.Stat(history); e == nil {
		historyRW, e := writable(history)
		if e != nil {
			return r, e
		}
		if !historyRW {
			r.Warnings = append(r.Warnings, "history library is read-only: skipped repairs and pruning")
			return r, nil
		}
		hdb, e := Open(history, false)
		if e != nil {
			return r, e
		}
		hr, e := Heal(hdb)
		ce := hdb.Close()
		if e != nil {
			r.Healthy = false
			return r, fmt.Errorf("history repair: %w", e)
		}
		if ce != nil {
			return r, ce
		}
		r.PlaylistEntities += hr.PlaylistEntities
		r.Playlists += hr.Playlists
		r.Waveforms += hr.Waveforms
		r.Loops += hr.Loops
		r.Artwork += hr.Artwork
		r.OrphanArtwork += hr.OrphanArtwork
		for _, warning := range hr.Warnings {
			r.Warnings = append(r.Warnings, "history: "+warning)
		}
		if err = PruneHistoryDB(history, maxSessions); err != nil {
			r.Healthy = false
			return r, fmt.Errorf("history: %w", err)
		}
		r.HistoryPruned = true
		hdb, e = Open(history, false)
		if e != nil {
			return r, e
		}
		var pending int
		e = hdb.QueryRow("SELECT count(*) FROM GuardianReanalysis").Scan(&pending)
		ce = hdb.Close()
		if e != nil {
			return r, e
		}
		if ce != nil {
			return r, ce
		}
		r.ReanalysisPending += pending
	} else if !errors.Is(e, os.ErrNotExist) {
		return r, e
	}
	return r, nil
}

// InspectMount returns a JSON-ready integrity report without altering databases,
// cache state, or sidecars. Pending writable recovery is reported as an error.
func InspectMount(mount string) (Report, error) {
	r := Report{Mount: mount, CheckedAt: time.Now().UTC()}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	rw, err := writable(filepath.Join(dir, "m.db"))
	if err != nil {
		return r, err
	}
	r.ReadOnly = !rw
	for _, name := range []string{"m.db", "hm.db"} {
		path := filepath.Join(dir, name)
		if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) && name == "hm.db" {
			continue
		} else if e != nil {
			return r, e
		}
		db, e := Open(path, true)
		if e != nil {
			return r, fmt.Errorf("%s: %w", name, e)
		}
		var check string
		e = db.QueryRow("PRAGMA quick_check").Scan(&check)
		if e == nil && check != "ok" {
			e = fmt.Errorf("integrity: %s", check)
		}
		if e == nil {
			var exists int
			e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='GuardianReanalysis' AND type='table'").Scan(&exists)
			if e == nil && exists != 0 {
				var pending int
				e = db.QueryRow("SELECT count(*) FROM GuardianReanalysis").Scan(&pending)
				r.ReanalysisPending += pending
			}
		}
		closeErr := db.Close()
		if e != nil {
			return r, fmt.Errorf("%s: %w", name, e)
		}
		if closeErr != nil {
			return r, closeErr
		}
	}
	r.Healthy = true
	return r, nil
}
