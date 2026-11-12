package dbhealer

import (
	"database/sql"
	"fmt"
	"strings"
)

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func columns(tx *sql.Tx, table string) (map[string]string, error) {
	rows, err := tx.Query("PRAGMA table_info(" + quote(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var def any
		if err = rows.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = name
	}
	return out, rows.Err()
}
func selectColumn(cols map[string]string, names ...string) string {
	for _, name := range names {
		if actual, ok := cols[strings.ToLower(name)]; ok {
			return quote(actual)
		}
	}
	return ""
}

// PruneHistoryDB retains the latest N sessions (50 for zero), all tracks still
// referenced by playlists/preparation lists, and the performance data of those
// tracks. Unknown history layouts fail before any deletion.
func PruneHistoryDB(path string, maxSessions int) error {
	if maxSessions < 0 {
		return fmt.Errorf("maxSessions must not be negative")
	}
	if maxSessions == 0 {
		maxSessions = DefaultMaxSessions
	}
	rw, err := writable(path)
	if err != nil {
		return err
	}
	if !rw {
		return fmt.Errorf("history library is read-only")
	}
	db, err := Open(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("history integrity: %s", check)
	}
	var information int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='Information'").Scan(&information); err != nil {
		return err
	}
	if information > 0 {
		var major, minor, patch int
		if err = db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
			return err
		}
		if major != 3 || minor != 0 || patch != 2 {
			return fmt.Errorf("unsupported history schema %d.%d.%d", major, minor, patch)
		}
	}
	if err = Tune(db, false); err != nil {
		return err
	}
	deleted := int64(0)
	err = transaction(db, func(tx *sql.Tx) error {
		sessions, err := columns(tx, "Historylist")
		if err != nil {
			return err
		}
		entities, err := columns(tx, "HistorylistEntity")
		if err != nil {
			return err
		}
		id := selectColumn(sessions, "id")
		listID := selectColumn(entities, "listId", "historylistId")
		trackID := selectColumn(entities, "trackId")
		if id == "" || listID == "" || trackID == "" {
			return fmt.Errorf("unsupported Historylist/HistorylistEntity schema")
		}
		timestamp := selectColumn(sessions, "startTime", "date", "createdAt", "dateCreated", "timestamp", "lastEditTime")
		ordering := id + " DESC"
		if timestamp != "" {
			ordering = timestamp + " DESC," + ordering
		}
		if _, err = tx.Exec("CREATE TEMP TABLE guardian_keep_history(id INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		// The newest visible sessions: one the DJ deleted on the deck (isDeleted)
		// must not take a visible one's place (BH-GC1-13); deleted ones go.
		visible := ""
		if softDeleted := selectColumn(sessions, "isDeleted"); softDeleted != "" {
			visible = " WHERE COALESCE(" + softDeleted + ",0)=0"
		}
		if _, err = tx.Exec("INSERT INTO guardian_keep_history SELECT "+id+" FROM Historylist"+visible+" ORDER BY "+ordering+" LIMIT ?", maxSessions); err != nil {
			return err
		}
		execDelete := func(q string) error {
			res, e := tx.Exec(q)
			if e != nil {
				return e
			}
			n, e := res.RowsAffected()
			deleted += n
			return e
		}
		if err = execDelete("DELETE FROM HistorylistEntity WHERE " + listID + " IS NULL OR " + listID + " NOT IN (SELECT id FROM guardian_keep_history)"); err != nil {
			return err
		}
		if err = execDelete("DELETE FROM Historylist WHERE " + id + " NOT IN (SELECT id FROM guardian_keep_history)"); err != nil {
			return err
		}
		if _, err = tx.Exec("DROP TABLE guardian_keep_history"); err != nil {
			return err
		}
		// Any table with trackId can retain a track (including optional Engine tables).
		// PerformanceData is dependent data, not an independent retention reference.
		rows, err := tx.Query("SELECT name FROM sqlite_master WHERE type='table'")
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		refs := []string{}
		for _, table := range tables {
			if table == "Track" || table == "PerformanceData" || strings.HasPrefix(table, "Guardian") || strings.HasPrefix(table, "sqlite_") {
				continue
			}
			cols, e := columns(tx, table)
			if e != nil {
				return e
			}
			if col := selectColumn(cols, "trackId"); col != "" {
				refs = append(refs, "SELECT "+col+" FROM "+quote(table)+" WHERE "+col+" IS NOT NULL")
			}
		}
		keep := strings.Join(refs, " UNION ")
		if keep == "" {
			return fmt.Errorf("no history track references")
		}
		predicate := " NOT IN (" + keep + ")"
		perfCols, e := columns(tx, "PerformanceData")
		if e != nil {
			return e
		}
		if col := selectColumn(perfCols, "trackId"); col != "" {
			if err = execDelete("DELETE FROM PerformanceData WHERE " + col + predicate + " OR " + col + " NOT IN (SELECT id FROM Track)"); err != nil {
				return err
			}
		}
		if err = execDelete("DELETE FROM Track WHERE id" + predicate); err != nil {
			return err
		}
		queue, e := columns(tx, "GuardianReanalysis")
		if e != nil {
			return e
		}
		if col := selectColumn(queue, "trackId"); col != "" {
			if err = execDelete("DELETE FROM GuardianReanalysis WHERE " + col + " NOT IN (SELECT id FROM Track)"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if deleted > 0 {
		_, err = db.Exec("VACUUM")
		if err != nil {
			return fmt.Errorf("history pruned; compaction failed: %w", err)
		}
	}
	_, err = db.Exec("PRAGMA optimize")
	return err
}
