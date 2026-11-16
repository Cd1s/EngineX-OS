package enginedb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// ShrinkOptions configures incremental RBA1 migration and compaction of
// .rb-bridge-state.db.
type ShrinkOptions struct {
	// DatabaseDir overrides filepath.Join(mount, "Engine Library", "Database2")
	// when non-empty.
	DatabaseDir string
	// Deadline bounds batch rewriting and compaction. Zero means no deadline.
	Deadline time.Time
	// MaxBridgeStateBytes caps .rb-bridge-state.db; zero uses defaultMaxBridgeStateBytes.
	MaxBridgeStateBytes int64
	// BatchSize is the number of BridgeAnalysis rows rewritten per transaction.
	// Zero uses 50.
	BatchSize int
	// FreeSpace returns available bytes on the volume holding the database.
	// Nil uses syscall.Statfs.
	FreeSpace func(string) (int64, error)
}

// ShrinkResult reports work done by ShrinkBridgeAnalysis.
type ShrinkResult struct {
	Rewritten int
	Compacted bool
	Marked    int
	Done      bool
}

const (
	analysisFormatKey = "analysis_format"
	analysisFormatVal = "rba1"
	rba1CursorKey     = "rba1_cursor"
	defaultBatchSize  = 50
)

func budgetExhausted(ctx context.Context, deadline time.Time) bool {
	if ctx.Err() != nil {
		return true
	}
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

func openStateDB(ctx context.Context, statePath string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: statePath}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func isRBA1Blob(b []byte) bool {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return false
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer r.Close()
	var magic [4]byte
	n, err := io.ReadFull(r, magic[:])
	return err == nil && n == 4 && string(magic[:]) == compactAnalysisMagic
}

func isCompactRBA1Row(sourceJSON, audioJSON []byte) bool {
	return len(audioJSON) <= 64 && isRBA1Blob(sourceJSON)
}

func availBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, nil
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func syncFilePath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDirPath(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	err = f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EBADF) {
		return nil
	}
	return err
}

// ShrinkBridgeAnalysis rewrites legacy plain/gzip JSON BridgeAnalysis rows to
// compact RBA1 in batches within opts.Deadline, compacts .rb-bridge-state.db
// via VACUUM INTO + atomic rename once all rows are RBA1 and mostly free pages,
// and marks capped BridgeState rows missing BridgeAnalysis so subsequent
// conversions backfill their shadow detail.
func ShrinkBridgeAnalysis(ctx context.Context, mount string, opts ShrinkOptions) (ShrinkResult, error) {
	var res ShrinkResult
	dir := opts.DatabaseDir
	if dir == "" {
		dir = filepath.Join(mount, "Engine Library", "Database2")
	}
	statePath := filepath.Join(dir, stateFile)
	tmpPath := statePath + ".compact"

	if budgetExhausted(ctx, opts.Deadline) {
		return res, ctx.Err()
	}
	for _, p := range []string{statePath, statePath + "-journal", statePath + "-wal", statePath + "-shm", tmpPath} {
		if err := safeFile(p); err != nil {
			return res, err
		}
	}
	if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
		res.Done = true
		return res, nil
	} else if err != nil {
		return res, err
	}
	// Clean up any leftover temp file from an interrupted VACUUM INTO.
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}

	db, err := openStateDB(ctx, statePath)
	if err != nil {
		return res, err
	}

	var hasAnalysis int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeAnalysis'`).Scan(&hasAnalysis); err != nil {
		db.Close()
		return res, err
	}
	if hasAnalysis == 0 {
		db.Close()
		res.Done = true
		return res, nil
	}

	var format string
	err = db.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key=?`, analysisFormatKey).Scan(&format)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return res, err
	}

	if format != analysisFormatVal {
		batchSize := opts.BatchSize
		if batchSize <= 0 {
			batchSize = defaultBatchSize
		}
		rewritten, done, err := rewriteAnalysisBatches(ctx, db, opts.Deadline, batchSize)
		res.Rewritten = rewritten
		if err != nil {
			db.Close()
			return res, err
		}
		if !done {
			db.Close()
			return res, ctx.Err()
		}
	}

	var pageCount, freelistCount, pageSize int64
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		db.Close()
		return res, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freelistCount); err != nil {
		db.Close()
		return res, err
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		db.Close()
		return res, err
	}
	db.Close()

	maxBytes := opts.MaxBridgeStateBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBridgeStateBytes
	}
	totalBytes := pageCount * pageSize
	mostlyFree := freelistCount > 0 && pageCount > 0 &&
		(freelistCount*2 >= pageCount || (totalBytes >= maxBytes*8/10 && freelistCount*4 >= pageCount))

	if mostlyFree {
		if budgetExhausted(ctx, opts.Deadline) {
			return res, ctx.Err()
		}
		usedBytes := (pageCount - freelistCount) * pageSize
		margin := usedBytes / 4
		if margin < 4096 {
			margin = 4096
		}
		if margin > 16<<20 {
			margin = 16 << 20
		}
		needed := usedBytes + margin
		freeFn := opts.FreeSpace
		if freeFn == nil {
			freeFn = availBytes
		}
		avail, err := freeFn(dir)
		if err == nil && avail < needed {
			return res, nil
		}
		if err := vacuumAndReplaceStateDB(ctx, dir, statePath, tmpPath); err != nil {
			return res, err
		}
		res.Compacted = true
	}

	if res.Compacted || res.Rewritten > 0 {
		if st, err := os.Stat(statePath); err == nil && st.Size() < maxBytes {
			db, err := openStateDB(ctx, statePath)
			if err != nil {
				return res, err
			}
			marked, err := markMissingAnalysisTracks(ctx, db)
			db.Close()
			if err != nil {
				return res, err
			}
			res.Marked = marked
		}
	}

	res.Done = !mostlyFree || res.Compacted
	return res, nil
}

type analysisBatchRow struct {
	trackID    int64
	sourceJSON []byte
	audioJSON  []byte
}

func rewriteAnalysisBatches(ctx context.Context, db *sql.DB, deadline time.Time, batchSize int) (int, bool, error) {
	var cursor int64
	var cursorStr string
	if err := db.QueryRowContext(ctx, `SELECT value FROM BridgeLibrary WHERE key=?`, rba1CursorKey).Scan(&cursorStr); err == nil {
		if parsed, err := strconv.ParseInt(cursorStr, 10, 64); err == nil {
			cursor = parsed
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	emptyAudio, err := packAnalysis([]byte("{}"))
	if err != nil {
		return 0, false, err
	}

	rewritten := 0
	for {
		if budgetExhausted(ctx, deadline) {
			return rewritten, false, nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return rewritten, false, err
		}

		rows, err := tx.QueryContext(ctx, `SELECT trackId, sourceJSON, audioJSON FROM BridgeAnalysis WHERE trackId > ? ORDER BY trackId LIMIT ?`, cursor, batchSize)
		if err != nil {
			tx.Rollback()
			return rewritten, false, err
		}
		var batch []analysisBatchRow
		for rows.Next() {
			var row analysisBatchRow
			if err := rows.Scan(&row.trackID, &row.sourceJSON, &row.audioJSON); err != nil {
				rows.Close()
				tx.Rollback()
				return rewritten, false, err
			}
			batch = append(batch, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			tx.Rollback()
			return rewritten, false, err
		}
		rows.Close()

		if len(batch) == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeLibrary(key,value) VALUES(?,?)`, analysisFormatKey, analysisFormatVal); err != nil {
				tx.Rollback()
				return rewritten, false, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM BridgeLibrary WHERE key=?`, rba1CursorKey); err != nil {
				tx.Rollback()
				return rewritten, false, err
			}
			if err := tx.Commit(); err != nil {
				return rewritten, false, err
			}
			return rewritten, true, nil
		}

		var lastID int64
		for _, row := range batch {
			lastID = row.trackID
			if isCompactRBA1Row(row.sourceJSON, row.audioJSON) {
				continue
			}
			newSource := row.sourceJSON
			if !isRBA1Blob(row.sourceJSON) {
				plain, err := UnpackAnalysis(row.sourceJSON, 64<<20)
				if err != nil {
					continue
				}
				var src struct {
					Track    struct{ ID uint32 }
					Analysis *rekordbox.Analysis
				}
				if err := json.Unmarshal(plain, &src); err != nil {
					continue
				}
				packed, err := packBridgeSource(src.Track.ID, src.Analysis)
				if err != nil {
					tx.Rollback()
					return rewritten, false, err
				}
				newSource = packed
			}
			if _, err := tx.ExecContext(ctx, `UPDATE BridgeAnalysis SET sourceJSON=?, audioJSON=? WHERE trackId=?`, newSource, emptyAudio, row.trackID); err != nil {
				tx.Rollback()
				return rewritten, false, err
			}
			rewritten++
		}

		if len(batch) < batchSize {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeLibrary(key,value) VALUES(?,?)`, analysisFormatKey, analysisFormatVal); err != nil {
				tx.Rollback()
				return rewritten, false, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM BridgeLibrary WHERE key=?`, rba1CursorKey); err != nil {
				tx.Rollback()
				return rewritten, false, err
			}
			if err := tx.Commit(); err != nil {
				return rewritten, false, err
			}
			return rewritten, true, nil
		}

		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO BridgeLibrary(key,value) VALUES(?,?)`, rba1CursorKey, strconv.FormatInt(lastID, 10)); err != nil {
			tx.Rollback()
			return rewritten, false, err
		}
		if err := tx.Commit(); err != nil {
			return rewritten, false, err
		}
		cursor = lastID
	}
}

func vacuumAndReplaceStateDB(ctx context.Context, dir, statePath, tmpPath string) error {
	db, err := openStateDB(ctx, statePath)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmpPath); err != nil {
		db.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("vacuum into %s: %w", filepath.Base(tmpPath), err)
	}
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := db.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := syncFilePath(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(statePath + side); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(tmpPath)
			return err
		}
	}
	if err := os.Rename(tmpPath, statePath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return syncDirPath(dir)
}

func markMissingAnalysisTracks(ctx context.Context, db *sql.DB) (int, error) {
	res, err := db.ExecContext(ctx, `UPDATE BridgeState SET fingerprint = 'fast:' || fingerprint WHERE fingerprint NOT LIKE 'fast:%' AND NOT EXISTS (SELECT 1 FROM BridgeAnalysis a WHERE a.trackId = BridgeState.trackId)`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
