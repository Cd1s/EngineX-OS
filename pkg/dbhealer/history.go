package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	minHistoryVacuumBudget  = 2 * time.Second
	maxMountVacuumLiveBytes = 64 << 20
	minVacuumTargetMargin   = 8 << 20
	minVacuumTempMargin     = 32 << 20
)

type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func columns(ctx context.Context, q sqlQuerier, table string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quote(table)+")")
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

func mainTableName(ctx context.Context, q sqlQuerier, name string) (string, bool, error) {
	var actual string
	err := q.QueryRowContext(ctx, "SELECT name FROM main.sqlite_master WHERE type='table' AND name=? COLLATE NOCASE LIMIT 1", name).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return actual, true, nil
}

func mainTableExists(ctx context.Context, q sqlQuerier, name string) (bool, error) {
	_, ok, err := mainTableName(ctx, q, name)
	return ok, err
}

func listMainTables(ctx context.Context, q sqlQuerier) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM main.sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	return tables, err
}

func verifyHistoryHeader(ctx context.Context, q sqlQuerier, requireInfo bool) error {
	var check string
	if err := q.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("history integrity: %s", check)
	}
	hasInfo, err := mainTableExists(ctx, q, "Information")
	if err != nil {
		return err
	}
	if !hasInfo {
		if requireInfo {
			return fmt.Errorf("missing library version: Information table missing")
		}
		return nil
	}
	var major, minor, patch int
	if err = q.QueryRowContext(ctx, "SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return err
	}
	if major != 3 || minor != 0 || patch != 2 {
		return fmt.Errorf("unsupported history schema %d.%d.%d", major, minor, patch)
	}
	return nil
}

// trackReferenceQuery returns a UNION query of all trackId references in
// independent tables (excluding Track, PerformanceData, Guardian*, sqlite_*).
func trackReferenceQuery(ctx context.Context, q sqlQuerier) (string, error) {
	tables, err := listMainTables(ctx, q)
	if err != nil {
		return "", err
	}
	var refs []string
	for _, table := range tables {
		if strings.EqualFold(table, "Track") || strings.EqualFold(table, "PerformanceData") ||
			strings.HasPrefix(strings.ToLower(table), "guardian") || strings.HasPrefix(strings.ToLower(table), "sqlite_") {
			continue
		}
		cols, e := columns(ctx, q, table)
		if e != nil {
			return "", e
		}
		if col := selectColumn(cols, "trackId"); col != "" {
			refs = append(refs, "SELECT "+col+" FROM "+quote(table)+" WHERE "+col+" IS NOT NULL")
		}
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("no history track references")
	}
	return strings.Join(refs, " UNION "), nil
}

// albumArtOrphanWhere builds a WHERE clause matching unreferenced AlbumArt rows
// while preserving an empty id=1 sentinel row when present.
func albumArtOrphanWhere(ctx context.Context, q sqlQuerier) (string, bool, error) {
	hasArt, err := mainTableExists(ctx, q, "AlbumArt")
	if err != nil || !hasArt {
		return "", false, err
	}
	artCols, err := columns(ctx, q, "AlbumArt")
	if err != nil {
		return "", false, err
	}
	artID := selectColumn(artCols, "id")
	if artID == "" {
		return "", false, nil
	}
	tables, err := listMainTables(ctx, q)
	if err != nil {
		return "", false, err
	}
	var refs []string
	for _, table := range tables {
		if strings.EqualFold(table, "AlbumArt") ||
			strings.HasPrefix(strings.ToLower(table), "guardian") || strings.HasPrefix(strings.ToLower(table), "sqlite_") {
			continue
		}
		cols, e := columns(ctx, q, table)
		if e != nil {
			return "", false, e
		}
		if col := selectColumn(cols, "albumArtId"); col != "" {
			refs = append(refs, "SELECT "+col+" FROM "+quote(table)+" WHERE "+col+" IS NOT NULL")
		}
	}
	if len(refs) == 0 {
		return "", false, nil
	}
	where := artID + " NOT IN (" + strings.Join(refs, " UNION ") + ")"
	hashCol := selectColumn(artCols, "hash")
	blobCol := selectColumn(artCols, "albumArt")
	if hashCol != "" && blobCol != "" {
		where += " AND NOT (" + artID + "=1 AND " + hashCol + " IS NULL AND (" + blobCol + " IS NULL OR length(" + blobCol + ")=0))"
	}
	return where, true, nil
}

func rowExists(ctx context.Context, q sqlQuerier, table, where string) (bool, error) {
	var found int
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+quote(table)+" WHERE "+where+")").Scan(&found); err != nil {
		return false, err
	}
	return found != 0, nil
}

// cleanHistoryOrphansTx removes only provably dead rows from hm.db inside the
// caller's rollbackable heal transaction, after validating the complete
// required hm.db schema, without touching any visible play history session
// (COALESCE(isDeleted,0)=0) or altering hm.db's schema / sqlite_stat1
// (BH-F52-2, BH-GC1-13).
func cleanHistoryOrphansTx(ctx context.Context, tx *sql.Tx) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for _, req := range []string{"Information", "Historylist", "HistorylistEntity", "Track", "PerformanceData", "AlbumArt", "Playlist", "PlaylistEntity"} {
		ok, err := mainTableExists(ctx, tx, req)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("unsupported history schema: missing %s table", req)
		}
	}
	sessions, err := columns(ctx, tx, "Historylist")
	if err != nil {
		return 0, err
	}
	entities, err := columns(ctx, tx, "HistorylistEntity")
	if err != nil {
		return 0, err
	}
	idCol := selectColumn(sessions, "id")
	listIDCol := selectColumn(entities, "listId", "historylistId")
	trackIDCol := selectColumn(entities, "trackId")
	if idCol == "" || listIDCol == "" || trackIDCol == "" {
		return 0, fmt.Errorf("unsupported Historylist/HistorylistEntity schema")
	}
	trackCols, err := columns(ctx, tx, "Track")
	if err != nil {
		return 0, err
	}
	trackPK := selectColumn(trackCols, "id")
	if trackPK == "" || selectColumn(trackCols, "albumArtId") == "" || selectColumn(trackCols, "path") == "" {
		return 0, fmt.Errorf("unsupported Track schema: missing required column")
	}
	perfCols, err := columns(ctx, tx, "PerformanceData")
	if err != nil {
		return 0, err
	}
	perfTrackCol := selectColumn(perfCols, "trackId")
	if perfTrackCol == "" {
		return 0, fmt.Errorf("unsupported PerformanceData schema: missing trackId column")
	}
	artCols, err := columns(ctx, tx, "AlbumArt")
	if err != nil {
		return 0, err
	}
	if selectColumn(artCols, "id") == "" || selectColumn(artCols, "hash") == "" || selectColumn(artCols, "albumArt") == "" {
		return 0, fmt.Errorf("unsupported AlbumArt schema: missing required column")
	}
	plEntityCols, err := columns(ctx, tx, "PlaylistEntity")
	if err != nil {
		return 0, err
	}
	if selectColumn(plEntityCols, "trackId") == "" {
		return 0, fmt.Errorf("unsupported PlaylistEntity schema: missing trackId column")
	}
	if prepTable, hasPrep, err := mainTableName(ctx, tx, "PreparelistEntity"); err != nil {
		return 0, err
	} else if hasPrep {
		prepCols, err := columns(ctx, tx, prepTable)
		if err != nil {
			return 0, err
		}
		if selectColumn(prepCols, "trackId") == "" {
			return 0, fmt.Errorf("unsupported PreparelistEntity schema: missing trackId column")
		}
	}

	softDeleted := selectColumn(sessions, "isDeleted")
	visibleSub := "SELECT " + idCol + " FROM Historylist"
	if softDeleted != "" {
		visibleSub += " WHERE COALESCE(" + softDeleted + ",0)=0"
	}

	deadEntityWhere := listIDCol + " IS NULL OR " + listIDCol + " NOT IN (" + visibleSub + ")" +
		" OR " + trackIDCol + " IS NULL OR " + trackIDCol + " NOT IN (SELECT " + trackPK + " FROM Track)"

	keepTracks, err := trackReferenceQuery(ctx, tx)
	if err != nil {
		return 0, err
	}
	artWhere, canCleanArt, err := albumArtOrphanWhere(ctx, tx)
	if err != nil {
		return 0, err
	}

	// Fast read-only probe so healthy hm.db mounts take zero write statements.
	needClean := false
	if softDeleted != "" {
		if needClean, err = rowExists(ctx, tx, "Historylist", "COALESCE("+softDeleted+",0)<>0"); err != nil {
			return 0, err
		}
	}
	if !needClean {
		if needClean, err = rowExists(ctx, tx, "HistorylistEntity", deadEntityWhere); err != nil {
			return 0, err
		}
	}
	if !needClean {
		if needClean, err = rowExists(ctx, tx, "Track", trackPK+" NOT IN ("+keepTracks+")"); err != nil {
			return 0, err
		}
	}
	if !needClean {
		perfWhere := perfTrackCol + " NOT IN (" + keepTracks + ") OR " + perfTrackCol + " NOT IN (SELECT " + trackPK + " FROM Track)"
		if needClean, err = rowExists(ctx, tx, "PerformanceData", perfWhere); err != nil {
			return 0, err
		}
	}
	if !needClean && canCleanArt {
		if needClean, err = rowExists(ctx, tx, "AlbumArt", artWhere); err != nil {
			return 0, err
		}
	}
	if !needClean {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	deleted := int64(0)
	execDelete := func(q string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, e := tx.ExecContext(ctx, q)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		deleted += n
		return e
	}
	if err = execDelete("DELETE FROM HistorylistEntity WHERE " + deadEntityWhere); err != nil {
		return 0, err
	}
	if softDeleted != "" {
		if err = execDelete("DELETE FROM Historylist WHERE COALESCE(" + softDeleted + ",0)<>0"); err != nil {
			return 0, err
		}
	}
	keep, e := trackReferenceQuery(ctx, tx)
	if e != nil {
		return 0, e
	}
	predicate := " NOT IN (" + keep + ")"
	perfWhere := perfTrackCol + predicate + " OR " + perfTrackCol + " NOT IN (SELECT " + trackPK + " FROM Track)"
	if err = execDelete("DELETE FROM PerformanceData WHERE " + perfWhere); err != nil {
		return 0, err
	}
	if err = execDelete("DELETE FROM Track WHERE " + trackPK + predicate); err != nil {
		return 0, err
	}
	if where, ok, e := albumArtOrphanWhere(ctx, tx); e != nil {
		return 0, e
	} else if ok {
		if err = execDelete("DELETE FROM AlbumArt WHERE " + where); err != nil {
			return 0, err
		}
	}
	if queueTable, hasQueue, e := mainTableName(ctx, tx, "GuardianReanalysis"); e != nil {
		return 0, e
	} else if hasQueue {
		queueCols, e := columns(ctx, tx, queueTable)
		if e != nil {
			return 0, e
		}
		if col := selectColumn(queueCols, "trackId"); col != "" {
			if err = execDelete("DELETE FROM main." + quote(queueTable) + " WHERE " + col + " NOT IN (SELECT " + trackPK + " FROM Track)"); err != nil {
				return 0, err
			}
		}
	}
	return int(deleted), nil
}

func fsAvailableBytes(dir string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	if fs.Bsize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size %d", fs.Bsize)
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}

// compactHistoryIfNeeded reclaims dead freelist pages in hm.db with VACUUM
// without running PRAGMA optimize (never creating sqlite_stat1), gated on
// remaining mount budget and free space on both the drive and temp directory.
func compactHistoryIfNeeded(ctx context.Context, db *sql.DB, path string, changed bool, r *Report) {
	if ctx.Err() != nil {
		return
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < minHistoryVacuumBudget {
		return
	}
	if err := clampBusyTimeout(ctx, db); err != nil {
		return
	}
	var pageCount, freelist, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil || pageCount <= 0 {
		return
	}
	if err := db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&freelist); err != nil || freelist <= 0 {
		return
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil || pageSize <= 0 {
		return
	}
	shouldCompact := false
	if changed {
		shouldCompact = freelist >= 16 || freelist*4 >= pageCount
	} else {
		shouldCompact = freelist >= 256 && freelist*4 >= pageCount
	}
	if !shouldCompact {
		return
	}
	livePages := pageCount - freelist
	if livePages < 0 {
		livePages = 0
	}
	liveBytes := uint64(livePages) * uint64(pageSize)
	fileBytes := uint64(pageCount) * uint64(pageSize)
	if _, hasDeadline := ctx.Deadline(); hasDeadline && liveBytes > maxMountVacuumLiveBytes {
		r.Warnings = append(r.Warnings, "history: skipped mount compaction (live size exceeds mount budget)")
		return
	}
	targetNeed := fileBytes + liveBytes*2 + minVacuumTargetMargin
	if avail, err := fsAvailableBytes(filepath.Dir(path)); err != nil || avail < targetNeed {
		r.Warnings = append(r.Warnings, "history: skipped compaction (insufficient target drive free space)")
		return
	}
	tempNeed := liveBytes*2 + minVacuumTempMargin
	if avail, err := fsAvailableBytes(os.TempDir()); err != nil || avail < tempNeed {
		r.Warnings = append(r.Warnings, "history: skipped compaction (insufficient temporary free space)")
		return
	}
	if err := clampBusyTimeout(ctx, db); err != nil {
		return
	}
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("history: compaction skipped: %v", err))
		return
	}
	r.HistoryCompacted = true
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
	ctx := context.Background()
	db, err := Open(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = verifyHistoryHeader(ctx, db, false); err != nil {
		return err
	}
	if err = Tune(db, false); err != nil {
		return err
	}
	deleted := int64(0)
	err = transaction(db, func(tx *sql.Tx) error {
		sessions, err := columns(ctx, tx, "Historylist")
		if err != nil {
			return err
		}
		entities, err := columns(ctx, tx, "HistorylistEntity")
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
		entityWhere := listID + " IS NULL OR " + listID + " NOT IN (SELECT id FROM guardian_keep_history)"
		if hasTrack, e := mainTableExists(ctx, tx, "Track"); e != nil {
			return e
		} else if hasTrack {
			trackCols, e := columns(ctx, tx, "Track")
			if e != nil {
				return e
			}
			if trackPK := selectColumn(trackCols, "id"); trackPK != "" {
				entityWhere += " OR " + trackID + " IS NULL OR " + trackID + " NOT IN (SELECT " + trackPK + " FROM Track)"
			}
		}
		if err = execDelete("DELETE FROM HistorylistEntity WHERE " + entityWhere); err != nil {
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
		keep, err := trackReferenceQuery(ctx, tx)
		if err != nil {
			return err
		}
		predicate := " NOT IN (" + keep + ")"
		perfCols, e := columns(ctx, tx, "PerformanceData")
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
		if where, ok, e := albumArtOrphanWhere(ctx, tx); e != nil {
			return e
		} else if ok {
			if err = execDelete("DELETE FROM AlbumArt WHERE " + where); err != nil {
				return err
			}
		}
		queue, e := columns(ctx, tx, "GuardianReanalysis")
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
