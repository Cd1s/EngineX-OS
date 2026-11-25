// Package dbhealer repairs Engine OS 5.0.4 libraries without discarding
// PerformanceData rows or changing their cue points.
package dbhealer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
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
	PathsNormalized   int       `json:"paths_normalized"`
	HistoryPruned     bool      `json:"history_pruned"`
	HistoryCleaned    int       `json:"history_cleaned,omitempty"`
	HistoryCompacted  bool      `json:"history_compacted,omitempty"`
	ReserveFixed      []string  `json:"reserve_fixed,omitempty"`
	Warnings          []string  `json:"warnings,omitempty"`
	CheckedAt         time.Time `json:"checked_at"`
}

func effectiveBusyTimeoutMS(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	busyMS := 3000
	if deadline, ok := ctx.Deadline(); ok {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 0, context.DeadlineExceeded
		}
		// Add a 5 ms cushion so a clamped busy wait finishes at or just after
		// ctx.Deadline() instead of returning SQLITE_BUSY ahead of ctx.Err().
		ms := int((rem+time.Millisecond-1)/time.Millisecond) + 5
		if ms < 1 {
			ms = 1
		}
		if ms < busyMS {
			busyMS = ms
		}
	}
	return busyMS, nil
}

func clampBusyTimeout(ctx context.Context, db *sql.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil
	}
	busyMS, err := effectiveBusyTimeoutMS(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyMS))
	return err
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

func historyBudgetExpired(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && isSQLiteBusy(err) && time.Until(deadline) <= 50*time.Millisecond {
		return true
	}
	return false
}

// Open never creates a missing library. A single connection keeps connection-local
// tuning consistent. Read-only opens never recover or create a journal.
func Open(path string, readOnly bool) (*sql.DB, error) {
	return openContext(context.Background(), path, readOnly)
}

func openContext(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	busyMS, err := effectiveBusyTimeoutMS(ctx)
	if err != nil {
		return nil, err
	}
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
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
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
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func Tune(db *sql.DB, readOnly bool) error {
	return tuneContext(context.Background(), db, readOnly)
}

func tuneContext(ctx context.Context, db *sql.DB, readOnly bool) error {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if readOnly {
		return nil
	}
	for _, p := range []string{"PRAGMA synchronous=NORMAL", "PRAGMA mmap_size=134217728", "PRAGMA temp_store=MEMORY", "PRAGMA optimize"} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func transaction(db *sql.DB, f func(*sql.Tx) error) error {
	return transactionContext(context.Background(), db, f)
}

func transactionContext(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	if err := clampBusyTimeout(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
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
func Heal(db *sql.DB) (Report, error) { return heal(db, false) }

// heal with dataOnly leaves Engine's views, triggers and indexes byte-identical:
// Engine compares its schema on attach, then drops and recreates anything altered.
func heal(db *sql.DB, dataOnly bool) (Report, error) {
	return healContext(context.Background(), db, dataOnly, nil)
}

func healContext(ctx context.Context, db *sql.DB, dataOnly bool, preHeal func(context.Context, *sql.Tx) error) (Report, error) {
	r := Report{CheckedAt: time.Now().UTC()}
	if err := clampBusyTimeout(ctx, db); err != nil {
		return r, err
	}
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return r, err
	}
	if check != "ok" {
		return r, fmt.Errorf("database integrity: %s", check)
	}
	var major, minor, patch int
	if err := db.QueryRowContext(ctx, "SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return r, fmt.Errorf("missing library version: %w", err)
	}
	if major != 3 || minor != 0 || patch != 2 {
		return r, fmt.Errorf("unsupported library schema %d.%d.%d (supported 3.0.2)", major, minor, patch)
	}
	if dataOnly {
		// PRAGMA optimize could add sqlite_stat1, and the queue must stay out of
		// the file: Engine drops unknown tables on attach.
		if _, err := db.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS GuardianReanalysis(trackId INTEGER NOT NULL,field TEXT NOT NULL,reason TEXT NOT NULL,PRIMARY KEY(trackId,field))"); err != nil {
			return r, err
		}
	} else if err := tuneContext(ctx, db, false); err != nil {
		return r, err
	}
	err := transactionContext(ctx, db, func(tx *sql.Tx) error {
		// An unrecognized future schema is not a license to rewrite its blobs.
		var schema string
		if err := tx.QueryRowContext(ctx, "SELECT sql FROM main.sqlite_master WHERE type='table' AND name='PerformanceData'").Scan(&schema); err != nil {
			return fmt.Errorf("unsupported library schema: %w", err)
		}
		if preHeal != nil {
			if err := preHeal(ctx, tx); err != nil {
				return err
			}
		}
		// GL-010: Engine rejects libraries where Information.id != 1 or uuid is empty.
		var infoCount int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM Information").Scan(&infoCount); err != nil {
			return err
		}
		if infoCount == 1 {
			var badID int
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM Information WHERE id<>1)").Scan(&badID); err != nil {
				return err
			}
			if badID != 0 {
				if _, err := tx.ExecContext(ctx, "UPDATE Information SET id=1 WHERE id<>1"); err != nil {
					return err
				}
			}
		}
		var emptyUUID int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM Information WHERE uuid IS NULL OR trim(uuid)='')").Scan(&emptyUUID); err != nil {
			return err
		}
		if emptyUUID != 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE Information SET uuid=? WHERE uuid IS NULL OR trim(uuid)=''", uuid.NewString()); err != nil {
				return err
			}
		}
		var err error
		if !dataOnly {
			if err = fixSchema(ctx, tx); err != nil {
				return err
			}
		}
		if r.PlaylistEntities, err = healChains(ctx, tx, false); err != nil {
			return err
		}
		if r.Playlists, err = healChains(ctx, tx, true); err != nil {
			return err
		}
		if !dataOnly {
			if err = materialize(ctx, tx); err != nil {
				return err
			}
		}
		if err = healPerformance(ctx, tx, &r); err != nil {
			return err
		}
		if err = healArtwork(ctx, tx, &r); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT count(*) FROM GuardianReanalysis").Scan(&r.ReanalysisPending)
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
	return healMount(mount, maxSessions, false)
}

// HealMountDataOnly is HealMount without schema edits, for libraries Engine will
// attach next.
func HealMountDataOnly(mount string, maxSessions int) (Report, error) {
	return HealMountDataOnlyContext(context.Background(), mount, maxSessions)
}

func healMount(mount string, maxSessions int, dataOnly bool) (Report, error) {
	return healMountContext(context.Background(), mount, maxSessions, dataOnly)
}

// HealMountDataOnlyContext is the mount hook heal with a caller-owned budget.
// The base heal commits first. Path normalization then commits in batches and
// stops cleanly when the caller's budget expires.
func HealMountDataOnlyContext(ctx context.Context, mount string, maxSessions int) (Report, error) {
	return healMountContext(ctx, mount, maxSessions, true)
}

func healMountContext(ctx context.Context, mount string, maxSessions int, dataOnly bool) (Report, error) {
	r := Report{Mount: mount, CheckedAt: time.Now().UTC()}
	if maxSessions < 0 {
		return r, fmt.Errorf("maxSessions must not be negative")
	}
	if libraryInUse(mount) {
		return r, errLibraryInUse
	}
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	rw, err := writable(path)
	if err != nil {
		return r, err
	}
	if rw {
		// Before Engine opens the volume: a nonzero page reserve breaks every ATTACH.
		if r.ReserveFixed, err = NormalizeDatabase2(filepath.Dir(path)); err != nil {
			return r, err
		}
	}
	db, err := Open(path, !rw)
	if err != nil {
		return r, err
	}
	if !rw {
		db.Close()
		r, err = InspectMount(mount)
		if res, ok, _ := Reserve(path); ok && res != 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("m.db page reserve is %d: Engine 5.3.0 cannot ATTACH it; rebuild on writable media", res))
		}
		r.Warnings = append(r.Warnings, "read-only media: inspected without repairs or journal writes")
		return r, err
	}
	fixed := r.ReserveFixed
	pathsBefore := r.PathsNormalized
	pathStart := time.Now()
	remaining, err := normalizeTrackPaths(ctx, db, mount, &r)
	if err != nil {
		// A path-pass database error means the library may be damaged. Run the
		// existing base heal so it remains the recovery path for corruption.
		pathsFixed := r.PathsNormalized
		r, healErr := heal(db, dataOnly)
		r.ReserveFixed = fixed
		r.PathsNormalized = pathsFixed
		_ = db.Close()
		r.Mount = mount
		if healErr != nil {
			return r, healErr
		}
		return r, err
	}
	if ctx.Err() != nil || (r.PathsNormalized != pathsBefore && time.Since(pathStart) >= time.Second) {
		r.Healthy = true
		closeErr := db.Close()
		r.Mount = mount
		log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, remaining)
		if closeErr != nil {
			return r, closeErr
		}
		return r, nil
	}
	pathsFixed := r.PathsNormalized
	r, err = heal(db, dataOnly)
	r.ReserveFixed = fixed
	r.PathsNormalized = pathsFixed
	if err != nil {
		_ = db.Close()
		r.Mount = mount
		return r, err
	}
	closeErr := db.Close()
	r.Mount = mount
	log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, remaining)
	if closeErr != nil {
		return r, closeErr
	}
	history := filepath.Join(filepath.Dir(path), "hm.db")
	if _, e := os.Stat(history); e == nil {
		if ctx.Err() != nil {
			return r, nil
		}
		historyRW, e := writable(history)
		if e != nil {
			return r, e
		}
		if !historyRW {
			r.Warnings = append(r.Warnings, "history library is read-only: skipped repairs and pruning")
			return r, nil
		}
		if dataOnly {
			hdb, e := openContext(ctx, history, false)
			if e != nil {
				if historyBudgetExpired(ctx, e) {
					r.Warnings = append(r.Warnings, "history: skipped (mount budget exceeded)")
					return r, nil
				}
				return r, e
			}
			// Validate required hm.db schema and clean soft-deleted tombstones and
			// orphan Track/PerformanceData/AlbumArt rows atomically inside the
			// rollbackable data-only heal transaction before scanning blobs, without
			// deleting any visible play history session (isDeleted=0) or altering
			// hm.db's schema (BH-F52-2, BH-GC1-13).
			var cleaned int
			hr, e := healContext(ctx, hdb, true, func(ctx context.Context, tx *sql.Tx) error {
				var cleanErr error
				cleaned, cleanErr = cleanHistoryOrphansTx(ctx, tx)
				return cleanErr
			})
			if e != nil {
				ce := hdb.Close()
				if historyBudgetExpired(ctx, e) {
					r.Warnings = append(r.Warnings, "history: skipped (mount budget exceeded)")
					if ce != nil {
						return r, ce
					}
					return r, nil
				}
				r.Healthy = false
				return r, fmt.Errorf("history repair: %w", e)
			}
			r.HistoryCleaned = cleaned
			r.PlaylistEntities += hr.PlaylistEntities
			r.Playlists += hr.Playlists
			r.Waveforms += hr.Waveforms
			r.Loops += hr.Loops
			r.Artwork += hr.Artwork
			r.OrphanArtwork += hr.OrphanArtwork
			r.ReanalysisPending += hr.ReanalysisPending
			r.PathsNormalized += hr.PathsNormalized
			for _, warning := range hr.Warnings {
				r.Warnings = append(r.Warnings, "history: "+warning)
			}
			compactHistoryIfNeeded(ctx, hdb, history, cleaned > 0 || hr.Artwork > 0, &r)
			ce := hdb.Close()
			if r.HistoryCleaned > 0 || r.HistoryCompacted {
				log.Printf("dbhealer: mount %s cleaned %d dead history rows (compacted=%v)", mount, r.HistoryCleaned, r.HistoryCompacted)
			}
			if ce != nil {
				return r, ce
			}
			return r, nil
		}
		hdb, e := Open(history, false)
		if e != nil {
			return r, e
		}
		hr, e := heal(hdb, false)
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
		// An explicit full heal prunes to the newest visible sessions and compacts.
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

var errLibraryInUse = errors.New("Engine has this drive's library open")

func libraryInUse(root string) bool {
	dir := filepath.Join(root, "Engine Library", "Database2")
	var wants []os.FileInfo
	for _, name := range []string{"m.db", "hm.db"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil {
			wants = append(wants, st)
		}
	}
	if len(wants) == 0 {
		return false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			opened, err := os.Stat(filepath.Join("/proc", entry.Name(), "fd", fd.Name()))
			if err != nil {
				continue
			}
			for _, want := range wants {
				if os.SameFile(opened, want) {
					return true
				}
			}
		}
	}
	return false
}

type trackPathCandidate struct {
	id      int64
	path    string
	nfcPath string
}

var normalizeTrackPathBatch func()

func normalizeTrackPaths(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	candidates, bridgeManaged, err := collectTrackPathCandidates(db, mount)
	if err != nil {
		return 0, err
	}
	eligible := candidates[:0]
	claimed := map[string]int64{}
	for _, candidate := range candidates {
		if bridgeManaged[candidate.id] {
			continue
		}
		var other int64
		err := db.QueryRow(`SELECT id FROM Track WHERE path=?`, candidate.nfcPath).Scan(&other)
		if err == nil && other != candidate.id {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if other, collision := claimed[candidate.nfcPath]; collision && other != candidate.id {
			continue
		}
		claimed[candidate.nfcPath] = candidate.id
		eligible = append(eligible, candidate)
	}
	candidates = eligible
	dirs := make(map[string]map[string]bool)
	listFiles := func(dir string) map[string]bool {
		if names, ok := dirs[dir]; ok {
			return names
		}
		names := map[string]bool{}
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.Type().IsRegular() {
					names[entry.Name()] = true
				}
			}
		}
		dirs[dir] = names
		return names
	}
	remaining := 0
	for start := 0; start < len(candidates); start += 64 {
		if err := ctx.Err(); err != nil {
			return len(candidates) - start, nil
		}
		end := min(start+64, len(candidates))
		tx, err := db.Begin()
		if err != nil {
			return remaining, err
		}
		next := start
		batchFixed := 0
		for _, candidate := range candidates[start:end] {
			if ctx.Err() != nil {
				break
			}
			next++
			oldFile, ok := trackPath(mount, candidate.path)
			if !ok {
				continue
			}
			newFile, ok := trackPath(mount, candidate.nfcPath)
			if !ok {
				continue
			}
			if listFiles(filepath.Dir(oldFile))[filepath.Base(oldFile)] {
				continue
			}
			if !listFiles(filepath.Dir(newFile))[filepath.Base(newFile)] {
				continue
			}
			oldFilename := filepath.Base(candidate.path)
			newFilename := filepath.Base(candidate.nfcPath)
			if oldFilename == newFilename {
				_, err = tx.Exec(`UPDATE Track SET path=? WHERE id=? AND path=?`, candidate.nfcPath, candidate.id, candidate.path)
			} else {
				var lastEditType string
				var lastEditTime any
				if err = tx.QueryRow(`SELECT typeof(lastEditTime), CASE typeof(lastEditTime) WHEN 'text' THEN CAST(lastEditTime AS BLOB) ELSE lastEditTime END FROM Track WHERE id=? AND path=?`, candidate.id, candidate.path).Scan(&lastEditType, &lastEditTime); err == nil {
					if b, ok := lastEditTime.([]byte); ok && lastEditType == "text" {
						lastEditTime = string(b)
					} else if b, ok := lastEditTime.([]byte); ok && lastEditType == "blob" && b == nil {
						lastEditTime = []byte{}
					}
					if _, err = tx.Exec(`UPDATE Track SET path=?, filename=? WHERE id=? AND path=?`, candidate.nfcPath, newFilename, candidate.id, candidate.path); err == nil {
						_, err = tx.Exec(`UPDATE Track SET lastEditTime=? WHERE id=?`, lastEditTime, candidate.id)
					}
				}
			}
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint failed") {
					continue
				}
				_ = tx.Rollback()
				return remaining, err
			}
			batchFixed++
		}
		if err := tx.Commit(); err != nil {
			return remaining, err
		}
		r.PathsNormalized += batchFixed
		if normalizeTrackPathBatch != nil {
			normalizeTrackPathBatch()
		}
		if next < end {
			return len(candidates) - next, nil
		}
		remaining = len(candidates) - end
	}
	return remaining, nil
}

func collectTrackPathCandidates(db *sql.DB, mount string) ([]trackPathCandidate, map[int64]bool, error) {
	rows, err := db.Query(`SELECT id, path FROM Track WHERE path IS NOT NULL AND path <> ''`)
	if err != nil {
		return nil, nil, err
	}
	candidates := []trackPathCandidate{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, nil, err
		}
		if !norm.NFC.IsNormalString(path) {
			candidates = append(candidates, trackPathCandidate{id: id, path: path, nfcPath: norm.NFC.String(path)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	streamingCandidates := candidates[:0]
	for _, candidate := range candidates {
		var streaming string
		if err := db.QueryRow(`SELECT COALESCE(streamingSource,'') FROM Track WHERE id=?`, candidate.id).Scan(&streaming); err != nil {
			return nil, nil, err
		}
		if streaming == "" {
			streamingCandidates = append(streamingCandidates, candidate)
		}
	}
	candidates = streamingCandidates
	if len(candidates) == 0 {
		return candidates, map[int64]bool{}, nil
	}
	bridgeManaged, err := bridgeManagedTracks(mount)
	if err != nil {
		for _, candidate := range candidates {
			bridgeManaged[candidate.id] = true
		}
		return candidates, bridgeManaged, nil
	}
	return candidates, bridgeManaged, err
}

func bridgeManagedTracks(mount string) (map[int64]bool, error) {
	managed := map[int64]bool{}
	path := filepath.Join(mount, "Engine Library", "Database2", ".rb-bridge-state.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return managed, nil
	} else if err != nil {
		return managed, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?mode=ro")
	if err != nil {
		return managed, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeState'`).Scan(&exists); err != nil {
		return managed, err
	}
	if exists == 0 {
		return managed, nil
	}
	rows, err := db.Query(`SELECT trackId FROM BridgeState`)
	if err != nil {
		return managed, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return managed, err
		}
		managed[id] = true
	}
	return managed, rows.Err()
}

func trackPath(mount, path string) (string, bool) {
	if filepath.IsAbs(path) || path == "" || path == "." {
		return "", false
	}
	full := filepath.Join(mount, "Engine Library", filepath.FromSlash(path))
	rel, err := filepath.Rel(mount, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
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
