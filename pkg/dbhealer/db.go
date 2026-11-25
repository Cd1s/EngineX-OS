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
	PathsNormalized    int       `json:"paths_normalized"`
	HistoryPruned      bool      `json:"history_pruned"`
	RestoredFromBackup bool      `json:"restored_from_backup,omitempty"`
	QuarantinedDBs     []string  `json:"quarantined_dbs,omitempty"`
	ReserveFixed       []string  `json:"reserve_fixed,omitempty"`
	Warnings           []string  `json:"warnings,omitempty"`
	CheckedAt          time.Time `json:"checked_at"`
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
	for _, p := range []string{"PRAGMA synchronous=FULL", "PRAGMA mmap_size=134217728", "PRAGMA temp_store=MEMORY", "PRAGMA optimize"} {
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
func Heal(db *sql.DB) (Report, error) { return heal(db, false) }

// heal with dataOnly leaves Engine's views, triggers and indexes byte-identical:
// Engine compares its schema on attach, then drops and recreates anything altered.
func heal(db *sql.DB, dataOnly bool) (Report, error) {
	r := Report{CheckedAt: time.Now().UTC()}
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		return r, err
	}
	if check != "ok" {
		return r, fmt.Errorf("database integrity: %s (%w)", check, errDatabaseCorrupt)
	}
	var major, minor, patch int
	if err := db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return r, fmt.Errorf("missing library version: %w (%w)", err, errDatabaseCorrupt)
	}
	if major != 3 || minor != 0 || patch != 2 {
		return r, fmt.Errorf("%w %d.%d.%d (supported 3.0.2)", errUnsupportedSchema, major, minor, patch)
	}
	if dataOnly {
		// PRAGMA optimize could add sqlite_stat1, and the queue must stay out of
		// the file: Engine drops unknown tables on attach.
		if _, err := db.Exec("CREATE TEMP TABLE IF NOT EXISTS GuardianReanalysis(trackId INTEGER NOT NULL,field TEXT NOT NULL,reason TEXT NOT NULL,PRIMARY KEY(trackId,field))"); err != nil {
			return r, err
		}
	} else if err := Tune(db, false); err != nil {
		return r, err
	}
	err := transaction(db, func(tx *sql.Tx) error {
		// An unrecognized future schema is not a license to rewrite its blobs.
		var schema string
		if err := tx.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='PerformanceData'").Scan(&schema); err != nil {
			return fmt.Errorf("%w: %v", errUnsupportedSchema, err)
		}
		// GL-010: Engine rejects libraries where Information.id != 1 or uuid is empty.
		var infoCount int
		if err := tx.QueryRow("SELECT count(*) FROM Information").Scan(&infoCount); err != nil {
			return err
		}
		if infoCount == 1 {
			if _, err := tx.Exec("UPDATE Information SET id=1 WHERE id<>1"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE Information SET uuid=? WHERE uuid IS NULL OR trim(uuid)=''", uuid.NewString()); err != nil {
			return err
		}
		var err error
		if !dataOnly {
			if err = fixSchema(tx); err != nil {
				return err
			}
		}
		if r.PlaylistEntities, err = healChains(tx, false); err != nil {
			return err
		}
		if r.Playlists, err = healChains(tx, true); err != nil {
			return err
		}
		if !dataOnly {
			if err = materialize(tx); err != nil {
				return err
			}
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
	if !rw {
		db, err := Open(path, true)
		if err != nil {
			return r, err
		}
		db.Close()
		r, err = InspectMount(mount)
		if res, ok, _ := Reserve(path); ok && res != 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("m.db page reserve is %d: Engine 5.3.0 cannot ATTACH it; rebuild on writable media", res))
		}
		r.Warnings = append(r.Warnings, "read-only media: inspected without repairs or journal writes")
		return r, err
	}

	// Before Engine opens the volume: a nonzero page reserve breaks every ATTACH.
	var restored bool
	var restoreWarnings []string
	var reserveFixed []string
	reserveFixed, err = NormalizeDatabase2(filepath.Dir(path))
	if err != nil {
		if isStructuralCorruption(err) && strings.Contains(err.Error(), path) {
			if qrel, meta, ok, rerr := restoreMainSnapshot(mount, path, err.Error()); rerr != nil {
				return r, rerr
			} else if ok {
				restored = true
				restoreWarnings = append(restoreWarnings, fmt.Sprintf(
					"recovered corrupt m.db from verified snapshot (%d tracks, %d playlists); damaged copy quarantined in %s",
					meta.Tracks, meta.Playlists, qrel,
				))
				reserveFixed, err = NormalizeDatabase2(filepath.Dir(path))
			}
		}
		if err != nil {
			return r, err
		}
	}

	var stats mainDBStats
	var statsErr error
	for attempt := 0; attempt < 2; attempt++ {
		var db *sql.DB
		var openErr error
		if _, _, magicOK, hdrErr := readSQLiteHeader(path); hdrErr == nil && !magicOK {
			openErr = fmt.Errorf("m.db has invalid or truncated SQLite header (%w)", errDatabaseCorrupt)
		} else {
			db, openErr = Open(path, false)
		}
		if openErr != nil {
			if attempt == 0 && !restored && isStructuralCorruption(openErr) {
				if qrel, meta, ok, rerr := restoreMainSnapshot(mount, path, openErr.Error()); rerr != nil {
					return r, rerr
				} else if ok {
					restored = true
					restoreWarnings = append(restoreWarnings, fmt.Sprintf(
						"recovered corrupt m.db from verified snapshot (%d tracks, %d playlists); damaged copy quarantined in %s",
						meta.Tracks, meta.Playlists, qrel,
					))
					continue
				}
			}
			return r, openErr
		}

		// Detect logical table collapse (t/71200: quick_check ok & schema 3.0.2,
		// but playlists wiped to 0 and tracks collapsed while audio files stay
		// on disk) before mutating the database.
		if attempt == 0 && !restored {
			if prev, hasPrev := loadSnapshotMeta(mount); hasPrev {
				var qcheck string
				var maj, min, pat int
				if db.QueryRow("PRAGMA quick_check").Scan(&qcheck) == nil && qcheck == "ok" &&
					db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&maj, &min, &pat) == nil &&
					maj == 3 && min == 0 && pat == 2 {
					if curStats, serr := readMainDBStats(db); serr == nil && isLogicalCollapse(prev, curStats) {
						if snapPath, _, verr := verifySnapshot(mount); verr == nil && missingTracksStillOnDisk(mount, snapPath, db) {
							_ = db.Close()
							reason := fmt.Sprintf("logical collapse (%d->%d tracks, %d->0 playlists with audio files intact on disk)",
								prev.Tracks, curStats.Tracks, prev.Playlists)
							qrel, _, ok, rerr := restoreMainSnapshot(mount, path, reason)
							if rerr != nil {
								return r, rerr
							}
							if ok {
								restored = true
								restoreWarnings = append(restoreWarnings, fmt.Sprintf(
									"recovered truncated m.db (%d->%d tracks, %d->0 playlists) from verified snapshot; damaged copy quarantined in %s",
									prev.Tracks, curStats.Tracks, prev.Playlists, qrel,
								))
								continue
							}
							return r, fmt.Errorf("%s (%w)", reason, errDatabaseCorrupt)
						}
					}
				}
			}
		}

		pathsBefore := r.PathsNormalized
		pathStart := time.Now()
		remaining, pathErr := normalizeTrackPaths(ctx, db, mount, &r)
		if pathErr != nil {
			// A path-pass database error means the library may be damaged. Run the
			// existing base heal so it remains the recovery path for corruption.
			pathsFixed := r.PathsNormalized
			hr, healErr := heal(db, dataOnly)
			_ = db.Close()
			if healErr != nil {
				if attempt == 0 && !restored && isStructuralCorruption(healErr) {
					if qrel, meta, ok, rerr := restoreMainSnapshot(mount, path, healErr.Error()); rerr != nil {
						return r, rerr
					} else if ok {
						restored = true
						restoreWarnings = append(restoreWarnings, fmt.Sprintf(
							"recovered corrupt m.db from verified snapshot (%d tracks, %d playlists); damaged copy quarantined in %s",
							meta.Tracks, meta.Playlists, qrel,
						))
						continue
					}
				}
				hr.ReserveFixed = reserveFixed
				hr.PathsNormalized = pathsFixed
				hr.RestoredFromBackup = restored
				hr.Warnings = append(append([]string(nil), restoreWarnings...), hr.Warnings...)
				hr.Mount = mount
				return hr, healErr
			}
			hr.ReserveFixed = reserveFixed
			hr.PathsNormalized = pathsFixed
			hr.RestoredFromBackup = restored
			hr.Warnings = append(append([]string(nil), restoreWarnings...), hr.Warnings...)
			hr.Mount = mount
			return hr, pathErr
		}
		if ctx.Err() != nil || (r.PathsNormalized != pathsBefore && time.Since(pathStart) >= time.Second) {
			r.Healthy = true
			r.ReserveFixed = reserveFixed
			r.RestoredFromBackup = restored
			r.Warnings = append(append([]string(nil), restoreWarnings...), r.Warnings...)
			closeErr := db.Close()
			r.Mount = mount
			log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, remaining)
			if closeErr != nil {
				return r, closeErr
			}
			return r, nil
		}
		pathsFixed := r.PathsNormalized
		hr, healErr := heal(db, dataOnly)
		if healErr != nil {
			_ = db.Close()
			if attempt == 0 && !restored && isStructuralCorruption(healErr) {
				if qrel, meta, ok, rerr := restoreMainSnapshot(mount, path, healErr.Error()); rerr != nil {
					return r, rerr
				} else if ok {
					restored = true
					restoreWarnings = append(restoreWarnings, fmt.Sprintf(
						"recovered corrupt m.db from verified snapshot (%d tracks, %d playlists); damaged copy quarantined in %s",
						meta.Tracks, meta.Playlists, qrel,
					))
					continue
				}
			}
			hr.ReserveFixed = reserveFixed
			hr.PathsNormalized = pathsFixed
			hr.RestoredFromBackup = restored
			hr.Warnings = append(append([]string(nil), restoreWarnings...), hr.Warnings...)
			hr.Mount = mount
			return hr, healErr
		}
		stats, statsErr = readMainDBStats(db)
		if st, werr := os.Stat(path + "-wal"); werr == nil && st.Size() > 0 {
			_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		}
		closeErr := db.Close()
		r = hr
		r.ReserveFixed = reserveFixed
		r.PathsNormalized = pathsFixed
		r.RestoredFromBackup = restored
		r.Warnings = append(append([]string(nil), restoreWarnings...), r.Warnings...)
		r.Mount = mount
		log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, remaining)
		if closeErr != nil {
			return r, closeErr
		}
		break
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
		} else {
			var hdb *sql.DB
			var hopenErr error
			if _, _, magicOK, hdrErr := readSQLiteHeader(history); hdrErr == nil && !magicOK {
				hopenErr = fmt.Errorf("hm.db has invalid or truncated SQLite header (%w)", errDatabaseCorrupt)
			} else {
				hdb, hopenErr = Open(history, false)
			}
			var hr Report
			var he error
			if hopenErr != nil {
				he = hopenErr
			} else {
				hr, he = heal(hdb, dataOnly)
				ce := hdb.Close()
				if he == nil && ce != nil {
					he = ce
				}
			}
			if he != nil {
				if isStructuralCorruption(he) {
					qrel, qerr := quarantineDBSet(mount, history)
					if qerr != nil {
						r.Healthy = false
						return r, fmt.Errorf("history repair: %w (quarantine failed: %v)", he, qerr)
					}
					r.QuarantinedDBs = append(r.QuarantinedDBs, "hm.db")
					r.Warnings = append(r.Warnings, fmt.Sprintf("quarantined corrupt hm.db (%v) to %s so m.db attaches cleanly", he, qrel))
					log.Printf("dbhealer: mount %s quarantined corrupt hm.db (%v) to %s", mount, he, qrel)
				} else {
					r.Healthy = false
					return r, fmt.Errorf("history repair: %w", he)
				}
			} else {
				r.PlaylistEntities += hr.PlaylistEntities
				r.Playlists += hr.Playlists
				r.Waveforms += hr.Waveforms
				r.Loops += hr.Loops
				r.Artwork += hr.Artwork
				r.OrphanArtwork += hr.OrphanArtwork
				for _, warning := range hr.Warnings {
					r.Warnings = append(r.Warnings, "history: "+warning)
				}
				// The mount hook's heal (--data-only) never prunes a DJ's history: stock
				// Engine keeps every session, and this deleted all but the newest 50 on
				// every insert, then added sqlite_stat1 to hm.db (BH-F52-2, BH-GC1-13).
				// An explicit full heal still prunes.
				if !dataOnly {
					if err = PruneHistoryDB(history, maxSessions); err != nil {
						r.Healthy = false
						return r, fmt.Errorf("history: %w", err)
					}
					r.HistoryPruned = true
				}
				if dataOnly {
					r.ReanalysisPending += hr.ReanalysisPending
					r.PathsNormalized += hr.PathsNormalized
				} else {
					hdb, e = Open(history, false)
					if e != nil {
						return r, e
					}
					var pending int
					e = hdb.QueryRow("SELECT count(*) FROM GuardianReanalysis").Scan(&pending)
					ce := hdb.Close()
					if e != nil {
						return r, e
					}
					if ce != nil {
						return r, ce
					}
					r.ReanalysisPending += pending
				}
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return r, e
	}

	checkAndQuarantineSidecars(ctx, mount, filepath.Dir(path), &r)
	if statsErr == nil {
		maybeSaveMainSnapshot(ctx, mount, path, stats, &r)
	}
	return r, nil
}

var errLibraryInUse = errors.New("Engine has this drive's library open")

func libraryInUse(root string) bool {
	path := filepath.Join(root, "Engine Library", "Database2", "m.db")
	want, err := os.Stat(path)
	if err != nil {
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
			if err == nil && os.SameFile(opened, want) {
				return true
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
