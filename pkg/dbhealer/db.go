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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
)

const DefaultMaxSessions = 50

type Report struct {
	Mount                string    `json:"mount"`
	ReadOnly             bool      `json:"read_only"`
	Healthy              bool      `json:"healthy"`
	PlaylistEntities     int       `json:"playlist_entities_relinked"`
	Playlists            int       `json:"playlists_relinked"`
	Waveforms            int       `json:"waveforms_replaced"`
	Loops                int       `json:"loops_aligned"`
	Artwork              int       `json:"artwork_resized"`
	ArtworkReconciled    int       `json:"artwork_reconciled,omitempty"`
	OrphanArtwork        int       `json:"orphan_artwork_references"`
	ReanalysisPending    int       `json:"reanalysis_pending"`
	PathsNormalized      int       `json:"paths_normalized"`
	RGBOverviewsMigrated int       `json:"rgb_overviews_migrated,omitempty"`
	DropboxLocalized     int       `json:"dropbox_localized,omitempty"`
	HistoryPruned        bool      `json:"history_pruned"`
	HistoryCleaned       int       `json:"history_cleaned,omitempty"`
	HistoryCompacted     bool      `json:"history_compacted,omitempty"`
	ReserveFixed         []string  `json:"reserve_fixed,omitempty"`
	Warnings             []string  `json:"warnings,omitempty"`
	CheckedAt            time.Time `json:"checked_at"`
}

const minDataOnlyProbeTimeout = 500 * time.Millisecond

func probeBusyTimeoutMS(ctx context.Context) int {
	if ctx == nil {
		return 3000
	}
	if ctx.Err() != nil {
		return 1
	}
	if deadline, ok := ctx.Deadline(); ok {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 1
		}
		ms := int(rem / time.Millisecond)
		if ms < 1 {
			return 1
		}
		if ms > 3000 {
			return 3000
		}
		return ms
	}
	return 3000
}

func identityProbeContext(ctx context.Context, dataOnly bool) (context.Context, context.CancelFunc) {
	if !dataOnly {
		return ctx, func() {}
	}
	if ctx.Err() != nil {
		return context.WithTimeout(context.Background(), minDataOnlyProbeTimeout)
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < minDataOnlyProbeTimeout {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), minDataOnlyProbeTimeout)
		stopWatch := context.AfterFunc(ctx, func() {
			if errors.Is(ctx.Err(), context.Canceled) {
				cancelProbe()
			}
		})
		return probeCtx, func() {
			stopWatch()
			cancelProbe()
		}
	}
	return ctx, func() {}
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

func clampBusyTimeoutMS(ctx context.Context, db *sql.DB, busyMS int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyMS))
	return err
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
	return clampBusyTimeoutMS(ctx, db, busyMS)
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

func budgetExpired(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if err != nil && strings.Contains(err.Error(), "SQLITE_BUSY") {
		if d, ok := ctx.Deadline(); ok && time.Until(d) <= 25*time.Millisecond {
			return true
		}
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
	return openWithBusyTimeout(ctx, path, readOnly, busyMS)
}

func openWithBusyTimeout(ctx context.Context, path string, readOnly bool, busyMS int) (*sql.DB, error) {
	return openDatabaseWithOptions(ctx, path, readOnly, readOnly, busyMS)
}

func openReadOnlyLocking(ctx context.Context, path string, busyMS int) (*sql.DB, error) {
	return openDatabaseWithOptions(ctx, path, true, false, busyMS)
}

func openDatabaseWithOptions(ctx context.Context, path string, readOnly, immutable bool, busyMS int) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
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
		if immutable {
			q.Set("immutable", "1")
		}
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
	if ctx != nil {
		if _, ok := ctx.Deadline(); ok {
			busyMS, err := effectiveBusyTimeoutMS(ctx)
			if err != nil {
				return err
			}
			return transactionContextWithBusy(ctx, db, busyMS, f)
		}
	}
	return runTransactionContext(ctx, db, f)
}

func transactionContextWithBusy(ctx context.Context, db *sql.DB, busyMS int, f func(*sql.Tx) error) error {
	return runTransactionContextWithBusy(ctx, db, busyMS, true, f)
}

func runTransactionContext(ctx context.Context, db *sql.DB, f func(*sql.Tx) error) error {
	return runTransactionContextWithBusy(ctx, db, 0, false, f)
}

func runTransactionContextWithBusy(ctx context.Context, db *sql.DB, busyMS int, setBusy bool, f func(*sql.Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Pin the transaction's own connection so defer conn.Close() synchronizes with
	// any asynchronous awaitDone rollback via conn's closemu without queuing behind
	// unrelated pool borrowers.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if setBusy {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyMS)); err != nil {
			return err
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err = f(tx); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		if errors.Is(err, sql.ErrTxDone) {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		return err
	}
	committed = true
	return nil
}

// Heal runs all m.db mutations atomically. Integrity errors are reported; a
// corrupt library is never replaced with an empty one.
func Heal(db *sql.DB) (Report, error) {
	r, _, err := healWithResizedContext(context.Background(), db, false)
	return r, err
}

// heal with dataOnly leaves Engine's views, triggers and indexes byte-identical:
// Engine compares its schema on attach, then drops and recreates anything altered.
func heal(db *sql.DB, dataOnly bool) (Report, error) {
	r, _, err := healWithResizedContext(context.Background(), db, dataOnly)
	return r, err
}

func healContext(ctx context.Context, db *sql.DB, dataOnly bool, preHeal func(context.Context, *sql.Tx) error) (Report, error) {
	r, _, err := healWithPreHealAndResizedContext(ctx, db, dataOnly, preHeal)
	return r, err
}

func healWithResizedContext(ctx context.Context, db *sql.DB, dataOnly bool) (Report, map[int64]bool, error) {
	return healWithHooksAndResizedContext(ctx, db, dataOnly, nil, nil)
}

func healWithPreHealAndResizedContext(ctx context.Context, db *sql.DB, dataOnly bool, preHeal func(context.Context, *sql.Tx) error) (Report, map[int64]bool, error) {
	return healWithHooksAndResizedContext(ctx, db, dataOnly, preHeal, nil)
}

func healWithHooksAndResizedContext(
	ctx context.Context,
	db *sql.DB,
	dataOnly bool,
	preHeal func(context.Context, *sql.Tx) error,
	postHeal func(context.Context, *sql.Tx, *Report) error,
) (Report, map[int64]bool, error) {
	r := Report{CheckedAt: time.Now().UTC()}
	if err := clampBusyTimeout(ctx, db); err != nil {
		return r, nil, err
	}
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return r, nil, err
	}
	if check != "ok" {
		return r, nil, fmt.Errorf("database integrity: %s", check)
	}
	infos, err := readLibraryIdentityRows(ctx, db)
	if err != nil {
		return r, nil, err
	}
	if _, err := validateLibraryIdentityRows(infos); err != nil {
		return r, nil, err
	}
	if dataOnly {
		// PRAGMA optimize could add sqlite_stat1, and the queue must stay out of
		// the file: Engine drops unknown tables on attach.
		if _, err := db.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS GuardianReanalysis(trackId INTEGER NOT NULL,field TEXT NOT NULL,reason TEXT NOT NULL,PRIMARY KEY(trackId,field))"); err != nil {
			return r, nil, err
		}
	} else if err := tuneContext(ctx, db, false); err != nil {
		return r, nil, err
	}
	var resized map[int64]bool
	var txReport Report
	err = transactionContext(ctx, db, func(tx *sql.Tx) error {
		txReport = r
		// An unrecognized future schema is not a license to rewrite its blobs.
		var schema string
		if err := tx.QueryRowContext(ctx, "SELECT sql FROM main.sqlite_master WHERE type='table' AND name='PerformanceData' COLLATE NOCASE").Scan(&schema); err != nil {
			return fmt.Errorf("unsupported library schema: %w", err)
		}
		if preHeal != nil {
			if err := preHeal(ctx, tx); err != nil {
				return err
			}
		}
		// GL-010: Engine rejects libraries where Information.id != 1 or uuid is empty.
		if err := repairLibraryIdentityTx(ctx, tx); err != nil {
			return err
		}
		var err error
		if !dataOnly {
			if err = fixSchema(ctx, tx); err != nil {
				return err
			}
		}
		if txReport.PlaylistEntities, err = healChains(ctx, tx, false); err != nil {
			return err
		}
		if txReport.Playlists, err = healChains(ctx, tx, true); err != nil {
			return err
		}
		if !dataOnly {
			if err = materialize(ctx, tx); err != nil {
				return err
			}
		}
		if err = healPerformance(ctx, tx, &txReport); err != nil {
			return err
		}
		if err = healArtworkMetadata(ctx, tx, &txReport); err != nil {
			return err
		}
		if !dataOnly {
			if resized, err = resizeOversizedArtworkBlobsTx(ctx, tx, &txReport); err != nil {
				return err
			}
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM GuardianReanalysis").Scan(&txReport.ReanalysisPending); err != nil {
			return err
		}
		if postHeal != nil {
			if err := postHeal(ctx, tx, &txReport); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.Healthy = false
		return r, nil, err
	}
	r = txReport
	if dataOnly && ctx.Err() == nil {
		if resized, err = resizeOversizedArtworkBlobsMount(ctx, db, &r); err != nil {
			r.Healthy = false
			return r, resized, err
		}
	}
	r.Healthy = true
	return r, resized, nil
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

// MountOptions configures a mount-time heal pass.
type MountOptions struct {
	MaxSessions                   int
	DataOnly                      bool
	ExtractEmbeddedArtwork        func(audioPath string) ([]byte, error)
	ExtractEmbeddedArtworkContext func(ctx context.Context, audioPath string) ([]byte, error)
}

func resolveEmbeddedArtworkExtractor(opts MountOptions) func(context.Context, string) ([]byte, error) {
	if opts.ExtractEmbeddedArtworkContext != nil {
		return opts.ExtractEmbeddedArtworkContext
	}
	if opts.ExtractEmbeddedArtwork != nil {
		legacy := opts.ExtractEmbeddedArtwork
		return func(ctx context.Context, audioPath string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return legacy(audioPath)
		}
	}
	return nil
}

// HealMount only opens Engine's two known database names; it never scans music.
func HealMount(mount string, maxSessions int) (Report, error) {
	return HealMountWithOptions(context.Background(), mount, MountOptions{MaxSessions: maxSessions})
}

// HealMountDataOnly is HealMount without schema edits, for libraries Engine will
// attach next.
func HealMountDataOnly(mount string, maxSessions int) (Report, error) {
	return HealMountDataOnlyContext(context.Background(), mount, maxSessions)
}

// HealMountDataOnlyContext is the mount hook heal with a caller-owned budget.
// The base heal commits first. Path normalization then commits in batches and
// stops cleanly when the caller's budget expires.
func HealMountDataOnlyContext(ctx context.Context, mount string, maxSessions int) (Report, error) {
	return HealMountWithOptions(ctx, mount, MountOptions{MaxSessions: maxSessions, DataOnly: true})
}

// HealMountWithOptions runs the mount heal with caller-supplied budget and optional
// embedded-artwork fallback for missing post-5.0 Artwork/<hash>.jpg files.
func HealMountWithOptions(ctx context.Context, mount string, opts MountOptions) (Report, error) {
	return healMountContext(ctx, mount, opts)
}

func healMountContext(ctx context.Context, mount string, opts MountOptions) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	enteredCanceled := ctx.Err() != nil
	maxSessions := opts.MaxSessions
	dataOnly := opts.DataOnly
	r := Report{Mount: mount, CheckedAt: time.Now().UTC()}
	if maxSessions < 0 {
		return r, fmt.Errorf("maxSessions must not be negative")
	}
	if mountLibraryInUse(mount) {
		return r, errLibraryInUse
	}
	if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
		return r, ctx.Err()
	}
	path := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	rw, err := writable(path)
	if err != nil {
		return r, err
	}
	if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
		return r, ctx.Err()
	}
	busyMS := probeBusyTimeoutMS(ctx)
	checkCtx, cancelProbe := identityProbeContext(ctx, dataOnly)
	defer cancelProbe()
	if !rw {
		r, err = inspectMountContext(checkCtx, mount)
		if err != nil {
			r.Healthy = false
			r.Mount = mount
			if !isContextStop(checkCtx, err) {
				if res, ok, rerr := reserveHeaderContext(checkCtx, path); rerr == nil && ok && res != 0 {
					r.Warnings = append(r.Warnings, fmt.Sprintf("m.db page reserve is %d: Engine 5.3.0 cannot ATTACH it; rebuild on writable media", res))
				}
				r.Warnings = append(r.Warnings, "read-only media: inspected without repairs or journal writes")
			}
			return r, err
		}
		res, ok, resErr := reserveHeaderContext(checkCtx, path)
		if resErr != nil {
			r.Healthy = false
			r.Mount = mount
			return r, resErr
		}
		if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
			r.Healthy = false
			r.Mount = mount
			return r, ctx.Err()
		}
		if ok && res != 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("m.db page reserve is %d: Engine 5.3.0 cannot ATTACH it; rebuild on writable media", res))
		}
		r.Warnings = append(r.Warnings, "read-only media: inspected without repairs or journal writes")
		return r, nil
	}
	var db *sql.DB
	var dbUUID string
	var needsIdentityRepair bool
	mainRes, mainResOK, resErr := reserveHeaderContext(checkCtx, path)
	if resErr != nil {
		return r, resErr
	}
	if mainResOK && mainRes != 0 {
		// Validate schema and identity with lock-respecting read-only open before rewriting reserve.
		roDB, roErr := openReadOnlyLocking(checkCtx, path, busyMS)
		if roErr != nil {
			if dataOnly && (isContextStop(checkCtx, roErr) || budgetExpired(checkCtx, roErr) || isSQLiteBusy(roErr)) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, roErr
		}
		var schemaErr error
		dbUUID, needsIdentityRepair, schemaErr = validateLibraryIdentityReadOnlyWithBusy(checkCtx, roDB, busyMS)
		if schemaErr == nil && !dataOnly {
			schemaErr = hasPerformanceDataTable(checkCtx, roDB)
		}
		roCloseErr := roDB.Close()
		if schemaErr != nil {
			if dataOnly && (isContextStop(checkCtx, schemaErr) || budgetExpired(checkCtx, schemaErr) || isSQLiteBusy(schemaErr)) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, schemaErr
		}
		if roCloseErr != nil {
			r.Healthy = false
			r.Mount = mount
			return r, roCloseErr
		}
		if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
			r.Healthy = false
			r.Mount = mount
			return r, ctx.Err()
		}
		if dataOnly && ctx.Err() != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("m.db page reserve is %d: mount budget exceeded before rebuild", mainRes))
			r.Healthy = false
			r.Mount = mount
			return r, ctx.Err()
		}
		// Before Engine opens the volume: a nonzero page reserve breaks every ATTACH.
		if r.ReserveFixed, _, err = normalizeMountDatabase2(ctx, ctx, filepath.Dir(path), nil); err != nil {
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
		db, err = openContext(ctx, path, false)
		if err != nil {
			if dataOnly && (isContextStop(ctx, err) || budgetExpired(ctx, err) || isSQLiteBusy(err)) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
	} else {
		db, err = openWithBusyTimeout(checkCtx, path, false, busyMS)
		if err != nil {
			if dataOnly && (isContextStop(checkCtx, err) || budgetExpired(checkCtx, err) || isSQLiteBusy(err)) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
		var schemaErr error
		dbUUID, needsIdentityRepair, schemaErr = validateLibraryIdentityReadOnlyWithBusy(checkCtx, db, busyMS)
		if schemaErr == nil && !dataOnly {
			schemaErr = hasPerformanceDataTable(checkCtx, db)
		}
		if schemaErr != nil {
			_ = db.Close()
			if dataOnly && (isContextStop(checkCtx, schemaErr) || budgetExpired(checkCtx, schemaErr) || isSQLiteBusy(schemaErr)) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, schemaErr
		}
		if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, ctx.Err()
		}
		if dataOnly && ctx.Err() != nil {
			if pendingOther, sibErr := anySiblingReserveNonZero(checkCtx, filepath.Dir(path), map[string]bool{path: true}); sibErr != nil {
				_ = db.Close()
				r.Healthy = false
				r.Mount = mount
				return r, sibErr
			} else if pendingOther {
				_ = db.Close()
				r.Warnings = append(r.Warnings, "database page reserve is non-zero: mount budget exceeded before rebuild")
				r.Healthy = false
				r.Mount = mount
				return r, ctx.Err()
			}
			if needsIdentityRepair {
				var repairErr error
				dbUUID, repairErr = validateAndRepairLibraryIdentityWithBusy(checkCtx, db, busyMS)
				if repairErr != nil {
					_ = db.Close()
					if isContextStop(checkCtx, repairErr) || budgetExpired(checkCtx, repairErr) || isSQLiteBusy(repairErr) {
						r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
					}
					r.Healthy = false
					r.Mount = mount
					return r, repairErr
				}
			}
			closeErr := db.Close()
			r.Mount = mount
			r.Healthy = closeErr == nil
			return r, closeErr
		}
		// Before Engine opens the volume: a nonzero page reserve breaks every ATTACH.
		var pendingReserve bool
		if r.ReserveFixed, pendingReserve, err = normalizeMountDatabase2(ctx, checkCtx, filepath.Dir(path), map[string]bool{path: true}); err != nil {
			if dataOnly && !pendingReserve && onlyContextError(err) {
				if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
					_ = db.Close()
					r.Healthy = false
					r.Mount = mount
					return r, ctx.Err()
				}
				if needsIdentityRepair {
					var repairErr error
					dbUUID, repairErr = validateAndRepairLibraryIdentityWithBusy(checkCtx, db, probeBusyTimeoutMS(ctx))
					if repairErr != nil {
						_ = db.Close()
						r.Healthy = false
						r.Mount = mount
						return r, repairErr
					}
				}
				r.Warnings = append(r.Warnings, "mount budget exceeded during reserve scan")
				closeErr := db.Close()
				r.Mount = mount
				r.Healthy = closeErr == nil
				return r, closeErr
			}
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
	}
	fixed := r.ReserveFixed
	if !dataOnly {
		// Full heal validates and commits m.db repairs and Track sync updates
		// inside a single rollbackable transaction so any schema/trigger/cancel
		// failure leaves zero partial mutations behind.
		var (
			resizedIDs map[int64]bool
			passRes    mountTrackPassResult
			hadCursor  bool
		)
		postHeal := func(ctx context.Context, tx *sql.Tx, rep *Report) error {
			var uerr error
			if dbUUID, _, uerr = validateLibraryIdentityReadOnlyWithBusy(ctx, tx, probeBusyTimeoutMS(ctx)); uerr != nil {
				return uerr
			}
			var perr error
			passRes, hadCursor, perr = runMountTrackPassTx(ctx, tx, mount, rep, dbUUID, mountDiscoveryPageSize)
			return perr
		}
		r, resizedIDs, err = healWithHooksAndResizedContext(ctx, db, false, nil, postHeal)
		r.ReserveFixed = fixed
		if err != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
		if hadCursor {
			removeDropboxCursor(mount)
		}
		artCache := newArtworkDirCache()
		extractEmbedded := resolveEmbeddedArtworkExtractor(opts)
		if cerr := ctx.Err(); cerr != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, cerr
		}
		if artErr := reconcileArtwork(ctx, db, mount, resizedIDs, extractEmbedded, artCache, &r); artErr != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, artErr
		}
		if cerr := ctx.Err(); cerr != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, cerr
		}
		reconcileOverviewDataDir(ctx, db, filepath.Dir(path), &r)
		if cerr := ctx.Err(); cerr != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, cerr
		}
		closeErr := db.Close()
		r.Mount = mount
		log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, passRes.nfdRemaining)
		if r.DropboxLocalized > 0 || passRes.dropboxRemaining > 0 {
			log.Printf("dbhealer: mount %s localized %d dropbox tracks, %d remaining", mount, r.DropboxLocalized, passRes.dropboxRemaining)
		}
		if closeErr != nil {
			r.Healthy = false
			return r, closeErr
		}
		return finishHistoryHeal(ctx, path, mount, maxSessions, false, extractEmbedded, artCache, r)
	}
	if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
		_ = db.Close()
		r.Healthy = false
		r.Mount = mount
		return r, ctx.Err()
	}
	if needsIdentityRepair {
		var repairErr error
		if dbUUID, repairErr = validateAndRepairLibraryIdentityWithBusy(checkCtx, db, probeBusyTimeoutMS(ctx)); repairErr != nil {
			_ = db.Close()
			if isContextStop(checkCtx, repairErr) || budgetExpired(checkCtx, repairErr) || isSQLiteBusy(repairErr) {
				r.Warnings = append(r.Warnings, "mount budget exceeded before schema and identity check")
			}
			r.Healthy = false
			r.Mount = mount
			return r, repairErr
		}
	}
	if ctx.Err() != nil {
		if !enteredCanceled && errors.Is(ctx.Err(), context.Canceled) {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, ctx.Err()
		}
		closeErr := db.Close()
		r.Mount = mount
		r.Healthy = closeErr == nil
		return r, closeErr
	}
	if err := clampBusyTimeout(ctx, db); err != nil {
		_ = db.Close()
		if dataOnly && (isContextStop(ctx, err) || budgetExpired(ctx, err) || isSQLiteBusy(err)) {
			r.Warnings = append(r.Warnings, "mount budget exceeded before track pass")
			r.Healthy = true
			r.Mount = mount
			return r, nil
		}
		r.Healthy = false
		r.Mount = mount
		return r, err
	}
	passRes, err := runMountTrackPass(ctx, db, mount, &r, mountTrackPassOptions{
		runNFD:     true,
		runDropbox: true,
		dataOnly:   dataOnly,
		dbUUID:     dbUUID,
		pageSize:   mountDiscoveryPageSize,
	})
	if err != nil && !isContextStop(ctx, err) {
		if dataOnly && ctx.Err() == nil && budgetExpired(ctx, err) {
			_ = db.Close()
			r.Healthy = true
			r.Mount = mount
			return r, nil
		}
		if dataOnly && ctx.Err() != nil {
			_ = db.Close()
			r.Healthy = false
			r.Mount = mount
			return r, err
		}
		// A non-context database error during the track pass means the library
		// may be damaged. Run the existing base heal as the recovery path.
		pathsFixed := r.PathsNormalized
		dropboxFixed := r.DropboxLocalized
		r, _, healErr := healWithResizedContext(ctx, db, dataOnly)
		r.ReserveFixed = fixed
		r.PathsNormalized = pathsFixed
		r.DropboxLocalized = dropboxFixed
		_ = db.Close()
		r.Mount = mount
		r.Healthy = false
		if healErr != nil {
			if isContextStop(ctx, healErr) || (dataOnly && budgetExpired(ctx, healErr)) {
				return r, err
			}
			return r, healErr
		}
		return r, err
	}
	deferBaseHeal := false
	if dataOnly {
		if passRes.stoppedEarly || ctx.Err() != nil {
			deferBaseHeal = true
		} else if deadline, hasDeadline := ctx.Deadline(); hasDeadline && (passRes.resumed || passRes.scannedRows >= 4096) && time.Until(deadline) < 250*time.Millisecond {
			deferBaseHeal = true
		}
	}
	if deferBaseHeal {
		r.Healthy = true
		closeErr := db.Close()
		r.Mount = mount
		log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, passRes.nfdRemaining)
		if r.DropboxLocalized > 0 || passRes.dropboxRemaining > 0 {
			log.Printf("dbhealer: mount %s localized %d dropbox tracks, %d remaining", mount, r.DropboxLocalized, passRes.dropboxRemaining)
		}
		if closeErr != nil {
			r.Healthy = false
			return r, closeErr
		}
		return r, nil
	}
	pathsFixed := r.PathsNormalized
	var resizedIDs map[int64]bool
	dropboxFixed := r.DropboxLocalized
	r, resizedIDs, err = healWithResizedContext(ctx, db, dataOnly)
	r.ReserveFixed = fixed
	r.PathsNormalized = pathsFixed
	r.DropboxLocalized = dropboxFixed
	if err != nil {
		_ = db.Close()
		r.Mount = mount
		if dataOnly && budgetExpired(ctx, err) {
			r.Healthy = true
			return r, nil
		}
		return r, err
	}
	artCache := newArtworkDirCache()
	extractEmbedded := resolveEmbeddedArtworkExtractor(opts)
	if ctx.Err() == nil {
		if artErr := reconcileArtwork(ctx, db, mount, resizedIDs, extractEmbedded, artCache, &r); artErr != nil {
			_ = db.Close()
			r.Mount = mount
			if dataOnly && budgetExpired(ctx, artErr) {
				r.Healthy = true
				return r, nil
			}
			return r, artErr
		}
	}
	reconcileOverviewDataDir(ctx, db, filepath.Dir(path), &r)
	closeErr := db.Close()
	r.Mount = mount
	log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, passRes.nfdRemaining)
	if r.DropboxLocalized > 0 || passRes.dropboxRemaining > 0 {
		log.Printf("dbhealer: mount %s localized %d dropbox tracks, %d remaining", mount, r.DropboxLocalized, passRes.dropboxRemaining)
	}
	if closeErr != nil {
		return r, closeErr
	}
	return finishHistoryHeal(ctx, path, mount, maxSessions, dataOnly, extractEmbedded, artCache, r)
}

func finishHistoryHeal(
	ctx context.Context,
	path, mount string,
	maxSessions int,
	dataOnly bool,
	extractEmbedded func(context.Context, string) ([]byte, error),
	artCache *artworkDirCache,
	r Report,
) (Report, error) {
	history := filepath.Join(filepath.Dir(path), "hm.db")
	if _, e := os.Stat(history); e == nil {
		if cerr := ctx.Err(); cerr != nil {
			if !dataOnly {
				r.Healthy = false
				return r, cerr
			}
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
				if historyBudgetExpired(ctx, e) || budgetExpired(ctx, e) {
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
			hr, resizedH, e := healWithPreHealAndResizedContext(ctx, hdb, true, func(ctx context.Context, tx *sql.Tx) error {
				var cleanErr error
				cleaned, cleanErr = cleanHistoryOrphansTx(ctx, tx)
				return cleanErr
			})
			if e == nil && ctx.Err() == nil {
				e = reconcileArtwork(ctx, hdb, mount, resizedH, extractEmbedded, artCache, &hr)
			}
			if e != nil {
				ce := hdb.Close()
				if historyBudgetExpired(ctx, e) || budgetExpired(ctx, e) {
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
			r.ArtworkReconciled += hr.ArtworkReconciled
			r.OrphanArtwork += hr.OrphanArtwork
			r.ReanalysisPending += hr.ReanalysisPending
			r.PathsNormalized += hr.PathsNormalized
			for _, warning := range hr.Warnings {
				r.Warnings = append(r.Warnings, "history: "+warning)
			}
			compactHistoryIfNeeded(ctx, hdb, history, cleaned > 0 || hr.Artwork > 0 || hr.ArtworkReconciled > 0, &r)
			ce := hdb.Close()
			if r.HistoryCleaned > 0 || r.HistoryCompacted {
				log.Printf("dbhealer: mount %s cleaned %d dead history rows (compacted=%v)", mount, r.HistoryCleaned, r.HistoryCompacted)
			}
			if ce != nil {
				return r, ce
			}
			return r, nil
		}
		hdb, e := openContext(ctx, history, false)
		if e != nil {
			return r, e
		}
		hr, resizedH, e := healWithResizedContext(ctx, hdb, false)
		if e == nil {
			if cerr := ctx.Err(); cerr != nil {
				e = cerr
			} else {
				e = reconcileArtwork(ctx, hdb, mount, resizedH, extractEmbedded, artCache, &hr)
			}
		}
		ce := hdb.Close()
		if e != nil {
			if dataOnly && budgetExpired(ctx, e) {
				return r, nil
			}
			r.Healthy = false
			return r, fmt.Errorf("history repair: %w", e)
		}
		if ce != nil {
			return r, ce
		}
		if cerr := ctx.Err(); cerr != nil {
			r.Healthy = false
			return r, cerr
		}
		r.PlaylistEntities += hr.PlaylistEntities
		r.Playlists += hr.Playlists
		r.Waveforms += hr.Waveforms
		r.Loops += hr.Loops
		r.Artwork += hr.Artwork
		r.ArtworkReconciled += hr.ArtworkReconciled
		r.OrphanArtwork += hr.OrphanArtwork
		for _, warning := range hr.Warnings {
			r.Warnings = append(r.Warnings, "history: "+warning)
		}
		// An explicit full heal prunes to the newest visible sessions and compacts.
		pruned, err := pruneHistoryDBContext(ctx, history, maxSessions)
		if pruned {
			r.HistoryPruned = true
		}
		if err != nil {
			r.Healthy = false
			return r, fmt.Errorf("history: %w", err)
		}
		hdb, e = openContext(ctx, history, false)
		if e != nil {
			return r, e
		}
		var pending int
		e = hdb.QueryRowContext(ctx, "SELECT count(*) FROM GuardianReanalysis").Scan(&pending)
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

var (
	errLibraryInUse   = errors.New("Engine has this drive's library open")
	mountLibraryInUse = libraryInUse
)

type fileDevIno struct {
	dev uint64
	ino uint64
}

func libraryInUse(root string) bool {
	dir := filepath.Join(root, "Engine Library", "Database2")
	var wants []fileDevIno
	for _, name := range []string{"m.db", "hm.db"} {
		var st unix.Stat_t
		if unix.Stat(filepath.Join(dir, name), &st) == nil {
			wants = append(wants, fileDevIno{dev: uint64(st.Dev), ino: uint64(st.Ino)})
		}
	}
	if len(wants) == 0 {
		return false
	}
	procDir, err := os.Open("/proc")
	if err != nil {
		return false
	}
	entries, err := procDir.Readdirnames(-1)
	_ = procDir.Close()
	if err != nil {
		return false
	}
	self := os.Getpid()
	pids := make([]string, 0, len(entries))
	for _, name := range entries {
		if name == "" || name[0] < '1' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		pids = append(pids, name)
	}
	if len(pids) == 0 {
		return false
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > 4 {
		workers = 4
	}
	if workers > len(pids) {
		workers = len(pids)
	}
	var found atomic.Bool
	if workers <= 1 {
		for _, pid := range pids {
			if pidHoldsAnyFile(pid, wants, &found) {
				return true
			}
		}
		return false
	}
	var next atomic.Uint64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for !found.Load() {
				idx := int(next.Add(1) - 1)
				if idx >= len(pids) {
					return
				}
				if pidHoldsAnyFile(pids[idx], wants, &found) {
					found.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()
	return found.Load()
}

func pidHoldsAnyFile(pid string, wants []fileDevIno, found *atomic.Bool) bool {
	fdDir, err := os.Open("/proc/" + pid + "/fd")
	if err != nil {
		return false
	}
	fds, err := fdDir.Readdirnames(-1)
	if err != nil {
		_ = fdDir.Close()
		return false
	}
	dirFD := int(fdDir.Fd())
	for _, fd := range fds {
		if found.Load() {
			_ = fdDir.Close()
			return false
		}
		var st unix.Stat_t
		if unix.Fstatat(dirFD, fd, &st, 0) == nil {
			dev, ino := uint64(st.Dev), uint64(st.Ino)
			for _, want := range wants {
				if dev == want.dev && ino == want.ino {
					_ = fdDir.Close()
					return true
				}
			}
		}
	}
	_ = fdDir.Close()
	return false
}

type trackPathCandidate struct {
	id      int64
	path    string
	nfcPath string
}

var normalizeTrackPathBatch func()

type libraryInfoRow struct {
	id                  int
	uuid                string
	major, minor, patch int
}

func readLibraryIdentityRows(ctx context.Context, q sqlQuerier) ([]libraryInfoRow, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, COALESCE(uuid,''), schemaVersionMajor, schemaVersionMinor, schemaVersionPatch FROM Information ORDER BY id LIMIT 2")
	if err != nil {
		if isContextStop(ctx, err) || budgetExpired(ctx, err) {
			return nil, err
		}
		return nil, fmt.Errorf("missing library version: %w", err)
	}
	var infos []libraryInfoRow
	for rows.Next() {
		var row libraryInfoRow
		if err := rows.Scan(&row.id, &row.uuid, &row.major, &row.minor, &row.patch); err != nil {
			_ = rows.Close()
			if isContextStop(ctx, err) || budgetExpired(ctx, err) {
				return nil, err
			}
			return nil, fmt.Errorf("missing library version: %w", err)
		}
		infos = append(infos, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		if isContextStop(ctx, err) || budgetExpired(ctx, err) {
			return nil, err
		}
		return nil, fmt.Errorf("missing library version: %w", err)
	}
	if err := rows.Close(); err != nil {
		if isContextStop(ctx, err) || budgetExpired(ctx, err) {
			return nil, err
		}
		return nil, fmt.Errorf("missing library version: %w", err)
	}
	return infos, nil
}

func validateLibraryIdentityRows(infos []libraryInfoRow) (libraryInfoRow, error) {
	if len(infos) == 0 {
		return libraryInfoRow{}, fmt.Errorf("missing library version: %w", sql.ErrNoRows)
	}
	for _, row := range infos {
		if row.major != 3 || row.minor != 0 || row.patch != 2 {
			return libraryInfoRow{}, fmt.Errorf("unsupported library schema %d.%d.%d (supported 3.0.2)", row.major, row.minor, row.patch)
		}
	}
	if len(infos) > 1 {
		return libraryInfoRow{}, fmt.Errorf("ambiguous library identity: Information has multiple rows")
	}
	return infos[0], nil
}

func hasPerformanceDataTable(ctx context.Context, q sqlQuerier) error {
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_master WHERE type='table' AND name='PerformanceData' COLLATE NOCASE").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("unsupported library schema: missing PerformanceData table")
	}
	return nil
}

func repairLibraryIdentityTx(ctx context.Context, tx *sql.Tx) error {
	infos, err := readLibraryIdentityRows(ctx, tx)
	if err != nil {
		return err
	}
	row, err := validateLibraryIdentityRows(infos)
	if err != nil {
		return err
	}
	targetID := row.id
	if row.id != 1 {
		if _, err := tx.ExecContext(ctx, "UPDATE Information SET id=1 WHERE id=?", row.id); err != nil {
			return err
		}
		targetID = 1
	}
	if strings.TrimSpace(row.uuid) == "" {
		if _, err := tx.ExecContext(ctx, "UPDATE Information SET uuid=? WHERE id=?", uuid.NewString(), targetID); err != nil {
			return err
		}
	}
	afterInfos, err := readLibraryIdentityRows(ctx, tx)
	if err != nil {
		return err
	}
	after, err := validateLibraryIdentityRows(afterInfos)
	if err != nil {
		return err
	}
	if after.id != 1 || strings.TrimSpace(after.uuid) == "" {
		return fmt.Errorf("library identity repair failed")
	}
	return nil
}

func validateLibraryIdentityReadOnlyWithBusy(ctx context.Context, q sqlQuerier, busyMS int) (string, bool, error) {
	if db, ok := q.(*sql.DB); ok {
		if err := clampBusyTimeoutMS(ctx, db, busyMS); err != nil {
			return "", false, err
		}
	}
	infos, err := readLibraryIdentityRows(ctx, q)
	if err != nil {
		return "", false, err
	}
	row, err := validateLibraryIdentityRows(infos)
	if err != nil {
		return "", false, err
	}
	trimmed := strings.TrimSpace(row.uuid)
	return trimmed, row.id != 1 || trimmed == "", nil
}

func validateAndRepairLibraryIdentityWithBusy(ctx context.Context, db *sql.DB, busyMS int) (string, error) {
	dbUUID, needsRepair, err := validateLibraryIdentityReadOnlyWithBusy(ctx, db, busyMS)
	if err != nil {
		return "", err
	}
	if !needsRepair {
		return dbUUID, nil
	}
	if err := transactionContextWithBusy(ctx, db, busyMS, func(tx *sql.Tx) error {
		return repairLibraryIdentityTx(ctx, tx)
	}); err != nil {
		return "", err
	}
	dbUUID, stillNeedsRepair, err := validateLibraryIdentityReadOnlyWithBusy(ctx, db, busyMS)
	if err != nil {
		return "", err
	}
	if stillNeedsRepair {
		return "", fmt.Errorf("library identity repair failed")
	}
	return dbUUID, nil
}

const sqliteInterruptCode = 9 // SQLITE_INTERRUPT

func isContextStop(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	var se interface{ Code() int }
	return errors.As(err, &se) && (se.Code()&0xff) == sqliteInterruptCode
}

type mountTrackScan struct {
	nfd           []trackPathCandidate
	dropbox       []dropboxTrackCandidate
	lastScannedID int64
	eof           bool
}

type mountTrackPage struct {
	nfd     []trackPathCandidate
	dropbox []dropboxTrackCandidate
	count   int64
	lastID  int64
	eof     bool
}

type mountTrackPassOptions struct {
	runNFD     bool
	runDropbox bool
	dataOnly   bool
	dbUUID     string
	pageSize   int
}

type mountTrackPassResult struct {
	nfdRemaining     int
	dropboxRemaining int
	scannedRows      int64
	resumed          bool
	stoppedEarly     bool
}

const mountDiscoveryPageSize = 1024

type sqlStmtQuerier interface {
	sqlQuerier
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func prepareMountPageScanStmt(ctx context.Context, q sqlStmtQuerier, runNFD, runDropbox bool) (*sql.Stmt, error) {
	switch {
	case runNFD && runDropbox:
		return q.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> '' AND (
			            octet_length(path) <> length(path)
			            OR streamingSource <> ''
			            OR uri <> ''
			        ) THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource, uri FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	case runDropbox:
		return q.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> '' AND (streamingSource <> '' OR uri <> '') THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource, uri FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	default:
		return q.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> ''
			            AND COALESCE(streamingSource,'') = ''
			            AND octet_length(path) <> length(path)
			        THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	}
}

func prepareMountPageDetailStmt(ctx context.Context, q sqlStmtQuerier, runNFD, runDropbox bool) (*sql.Stmt, error) {
	switch {
	case runNFD && runDropbox:
		return q.PrepareContext(ctx,
			`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,''), COALESCE(fileBytes,0)
			 FROM Track
			 WHERE id > ? AND id <= ?
			   AND path <> ''
			   AND (octet_length(path) <> length(path) OR streamingSource <> '' OR uri <> '')
			 ORDER BY id`,
		)
	case runDropbox:
		return q.PrepareContext(ctx,
			`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,''), COALESCE(fileBytes,0)
			 FROM Track
			 WHERE id > ? AND id <= ?
			   AND path <> ''
			   AND (streamingSource <> '' OR uri <> '')
			 ORDER BY id`,
		)
	default:
		return q.PrepareContext(ctx,
			`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), '', 0
			 FROM Track
			 WHERE id > ? AND id <= ?
			   AND path <> ''
			   AND COALESCE(streamingSource,'') = ''
			   AND octet_length(path) <> length(path)
			 ORDER BY id`,
		)
	}
}

func scanMountTrackPage(
	ctx context.Context,
	q sqlStmtQuerier,
	pageStmt *sql.Stmt,
	detailStmt **sql.Stmt,
	afterID int64,
	pageSize int,
	runNFD, runDropbox bool,
) (mountTrackPage, error) {
	var page mountTrackPage
	page.lastID = afterID
	if ctx.Err() != nil {
		return page, ctx.Err()
	}
	var count, maxID, hits int64
	if err := pageStmt.QueryRowContext(ctx, afterID, pageSize).Scan(&count, &maxID, &hits); err != nil {
		return page, err
	}
	page.count = count
	if count == 0 {
		page.eof = true
		return page, nil
	}
	page.lastID = maxID
	page.eof = count < int64(pageSize)
	if hits == 0 {
		return page, nil
	}
	if ctx.Err() != nil {
		return page, ctx.Err()
	}
	if *detailStmt == nil {
		var err error
		*detailStmt, err = prepareMountPageDetailStmt(ctx, q, runNFD, runDropbox)
		if err != nil {
			return page, err
		}
	}
	rows, err := (*detailStmt).QueryContext(ctx, afterID, maxID)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var id, fileBytes int64
		var path, src, uri string
		if err := rows.Scan(&id, &path, &src, &uri, &fileBytes); err != nil {
			_ = rows.Close()
			return page, err
		}
		if path == "" {
			continue
		}
		if runDropbox && isDropboxLocalizableTag(src, uri) {
			page.dropbox = append(page.dropbox, dropboxTrackCandidate{id: id, path: path, fileBytes: fileBytes})
		} else if runNFD && src == "" && !isASCII(path) && !norm.NFC.IsNormalString(path) {
			page.nfd = append(page.nfd, trackPathCandidate{id: id, path: path, nfcPath: norm.NFC.String(path)})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return page, err
	}
	if err := rows.Close(); err != nil {
		return page, err
	}
	return page, nil
}

func discoverMountTrackCandidates(ctx context.Context, db *sql.DB, collectDropbox bool, dropboxAfterID int64) (mountTrackScan, error) {
	var scan mountTrackScan
	if ctx.Err() != nil {
		return scan, nil
	}
	pageStmt, err := prepareMountPageScanStmt(ctx, db, true, collectDropbox)
	if err != nil {
		if isContextStop(ctx, err) {
			return scan, nil
		}
		return scan, err
	}
	defer pageStmt.Close()

	var detailStmt *sql.Stmt
	defer func() {
		if detailStmt != nil {
			_ = detailStmt.Close()
		}
	}()

	var afterID int64
	for {
		page, err := scanMountTrackPage(ctx, db, pageStmt, &detailStmt, afterID, mountDiscoveryPageSize, true, collectDropbox)
		if err != nil {
			if isContextStop(ctx, err) {
				return scan, nil
			}
			return scan, err
		}
		if page.count == 0 {
			scan.eof = true
			return scan, nil
		}
		scan.nfd = append(scan.nfd, page.nfd...)
		for _, cand := range page.dropbox {
			if cand.id > dropboxAfterID {
				scan.dropbox = append(scan.dropbox, cand)
			}
		}
		afterID = page.lastID
		scan.lastScannedID = page.lastID
		if page.eof {
			scan.eof = true
			return scan, nil
		}
	}
}

func runMountTrackPass(ctx context.Context, db *sql.DB, mount string, r *Report, opts mountTrackPassOptions) (mountTrackPassResult, error) {
	res, _, err := runMountTrackPassCore(ctx, db, nil, mount, r, opts)
	return res, err
}

func runMountTrackPassTx(ctx context.Context, tx *sql.Tx, mount string, r *Report, dbUUID string, pageSize int) (mountTrackPassResult, bool, error) {
	return runMountTrackPassCore(ctx, tx, tx, mount, r, mountTrackPassOptions{
		runNFD:     true,
		runDropbox: true,
		dataOnly:   false,
		dbUUID:     dbUUID,
		pageSize:   pageSize,
	})
}

func runMountTrackPassCore(
	ctx context.Context,
	q sqlStmtQuerier,
	outerTx *sql.Tx,
	mount string,
	r *Report,
	opts mountTrackPassOptions,
) (mountTrackPassResult, bool, error) {
	var res mountTrackPassResult
	pageSize := opts.pageSize
	if pageSize <= 0 {
		pageSize = mountDiscoveryPageSize
	}
	var startAfterID int64
	var hadCursor bool
	if opts.runDropbox {
		startAfterID, hadCursor = readDropboxCursor(mount, opts.dbUUID)
		if outerTx != nil {
			startAfterID = 0
		}
		res.resumed = outerTx == nil && hadCursor && startAfterID > 0
	}
	completedID := startAfterID
	saveCursorIfAdvanced := func(cursorID int64) {
		if outerTx == nil && opts.runDropbox && cursorID > startAfterID {
			writeDropboxCursor(mount, opts.dbUUID, cursorID)
		}
	}
	if err := ctx.Err(); err != nil {
		res.stoppedEarly = true
		if outerTx != nil {
			return res, hadCursor, err
		}
		return res, hadCursor, nil
	}

	pageStmt, err := prepareMountPageScanStmt(ctx, q, opts.runNFD, opts.runDropbox)
	if err != nil {
		if outerTx == nil && isContextStop(ctx, err) {
			res.stoppedEarly = true
			return res, hadCursor, nil
		}
		return res, hadCursor, err
	}
	defer pageStmt.Close()

	var detailStmt *sql.Stmt
	defer func() {
		if detailStmt != nil {
			_ = detailStmt.Close()
		}
	}()

	passStart := time.Now()
	nfdBefore := r.PathsNormalized
	dropboxBefore := r.DropboxLocalized
	claimedNFC := map[string]int64{}
	dirs := make(map[string]map[string]bool)
	safeDirs := map[string]bool{}
	var bridgeManaged map[int64]bool
	bridgeLoaded := false
	bridgeUnreadable := false

	ensureBridge := func() (bool, error) {
		if bridgeLoaded {
			return !bridgeUnreadable, nil
		}
		loaded, err := bridgeManagedTracksContext(ctx, mount)
		if err != nil {
			if isContextStop(ctx, err) {
				return false, err
			}
			bridgeLoaded = true
			bridgeUnreadable = true
			return false, nil
		}
		bridgeLoaded = true
		bridgeManaged = loaded
		return true, nil
	}

	afterID := startAfterID
	for {
		if err := ctx.Err(); err != nil {
			saveCursorIfAdvanced(completedID)
			res.stoppedEarly = true
			if outerTx != nil {
				return res, hadCursor, err
			}
			return res, hadCursor, nil
		}
		page, err := scanMountTrackPage(ctx, q, pageStmt, &detailStmt, afterID, pageSize, opts.runNFD, opts.runDropbox)
		if err != nil {
			if outerTx == nil && isContextStop(ctx, err) {
				saveCursorIfAdvanced(completedID)
				res.stoppedEarly = true
				return res, hadCursor, nil
			}
			return res, hadCursor, err
		}
		res.scannedRows += page.count
		if page.count == 0 {
			if outerTx == nil && opts.runDropbox && hadCursor {
				removeDropboxCursor(mount)
			}
			return res, hadCursor, nil
		}

		// Interleave #322 NFD normalization and S22 Dropbox localization in
		// ascending Track.id order so neither workstream starves the other
		// across budgeted passes on a mixed page. Invariant: every Track with
		// id <= completedID has finished all enabled workstreams.
		var nfdSlice []trackPathCandidate
		if opts.runNFD {
			nfdSlice = page.nfd
		}
		var dropboxSlice []dropboxTrackCandidate
		if opts.runDropbox {
			dropboxSlice = page.dropbox
		}
		ni, di := 0, 0
		for ni < len(nfdSlice) || di < len(dropboxSlice) {
			if err := ctx.Err(); err != nil {
				saveCursorIfAdvanced(completedID)
				res.nfdRemaining = len(nfdSlice) - ni
				res.dropboxRemaining = len(dropboxSlice) - di
				res.stoppedEarly = true
				if outerTx != nil {
					return res, hadCursor, err
				}
				return res, hadCursor, nil
			}
			if di < len(dropboxSlice) && (ni >= len(nfdSlice) || dropboxSlice[di].id < nfdSlice[ni].id) {
				end := di
				for end < len(dropboxSlice) && end-di < 64 && (ni >= len(nfdSlice) || dropboxSlice[end].id < nfdSlice[ni].id) {
					end++
				}
				bridgeOK, berr := ensureBridge()
				if berr != nil {
					if outerTx == nil && isContextStop(ctx, berr) {
						saveCursorIfAdvanced(completedID)
						res.nfdRemaining = len(nfdSlice) - ni
						res.dropboxRemaining = len(dropboxSlice) - di
						res.stoppedEarly = true
						return res, hadCursor, nil
					}
					return res, hadCursor, berr
				}
				if !bridgeOK {
					ni = len(nfdSlice)
					di = len(dropboxSlice)
					break
				}
				dbRem, examinedID, interrupted, derr := processDropboxCandidates(
					ctx, q, outerTx, mount, r, dropboxSlice[di:end], bridgeManaged, safeDirs, claimedNFC,
				)
				if derr != nil {
					if outerTx == nil && isContextStop(ctx, derr) {
						if examinedID > completedID {
							completedID = examinedID
						}
						saveCursorIfAdvanced(completedID)
						res.nfdRemaining = len(nfdSlice) - ni
						res.dropboxRemaining = (len(dropboxSlice) - end) + dbRem
						res.stoppedEarly = true
						return res, hadCursor, nil
					}
					return res, hadCursor, derr
				}
				if examinedID > completedID {
					completedID = examinedID
				}
				if !interrupted && dbRem == 0 {
					if lastChunkID := dropboxSlice[end-1].id; lastChunkID > completedID {
						completedID = lastChunkID
					}
				}
				di = end - dbRem
				if outerTx != nil {
					if err := ctx.Err(); err != nil {
						res.nfdRemaining = len(nfdSlice) - ni
						res.dropboxRemaining = len(dropboxSlice) - di
						res.stoppedEarly = true
						return res, hadCursor, err
					}
					continue
				}
				dropboxSlow := opts.dataOnly && r.DropboxLocalized != dropboxBefore && time.Since(passStart) >= time.Second
				if interrupted || dbRem > 0 || ctx.Err() != nil || dropboxSlow {
					nfdRemTotal := len(nfdSlice) - ni
					dbRemTotal := len(dropboxSlice) - di
					if nfdRemTotal == 0 && dbRemTotal == 0 && page.eof {
						if opts.runDropbox && hadCursor {
							removeDropboxCursor(mount)
						}
					} else {
						saveCursorIfAdvanced(completedID)
					}
					res.nfdRemaining = nfdRemTotal
					res.dropboxRemaining = dbRemTotal
					res.stoppedEarly = true
					return res, hadCursor, nil
				}
				continue
			}

			// NFD leads (page.nfd and page.dropbox are disjoint in scanMountTrackPage).
			end := ni
			for end < len(nfdSlice) && end-ni < 64 && (di >= len(dropboxSlice) || nfdSlice[end].id < dropboxSlice[di].id) {
				end++
			}
			bridgeOK, berr := ensureBridge()
			if berr != nil {
				if outerTx == nil && isContextStop(ctx, berr) {
					saveCursorIfAdvanced(completedID)
					res.nfdRemaining = len(nfdSlice) - ni
					res.dropboxRemaining = len(dropboxSlice) - di
					res.stoppedEarly = true
					return res, hadCursor, nil
				}
				return res, hadCursor, berr
			}
			if !bridgeOK {
				ni = len(nfdSlice)
				di = len(dropboxSlice)
				break
			}
			nfdRem, nfdLastID, _, nfdInterrupted, nerr := normalizeTrackPathsBatch(
				ctx, q, outerTx, mount, nfdSlice[ni:end], bridgeManaged, claimedNFC, dirs, r,
			)
			if nerr != nil {
				if outerTx == nil && isContextStop(ctx, nerr) {
					if nfdLastID > completedID {
						completedID = nfdLastID
					}
					saveCursorIfAdvanced(completedID)
					res.nfdRemaining = (len(nfdSlice) - end) + nfdRem
					res.dropboxRemaining = len(dropboxSlice) - di
					res.stoppedEarly = true
					return res, hadCursor, nil
				}
				return res, hadCursor, nerr
			}
			if nfdLastID > completedID {
				completedID = nfdLastID
			}
			if !nfdInterrupted && nfdRem == 0 {
				if lastChunkID := nfdSlice[end-1].id; lastChunkID > completedID {
					completedID = lastChunkID
				}
				ni = end
			} else {
				ni = end - nfdRem
			}
			if outerTx != nil {
				if err := ctx.Err(); err != nil {
					res.nfdRemaining = len(nfdSlice) - ni
					res.dropboxRemaining = len(dropboxSlice) - di
					res.stoppedEarly = true
					return res, hadCursor, err
				}
				continue
			}
			nfdSlow := opts.dataOnly && r.PathsNormalized != nfdBefore && time.Since(passStart) >= time.Second
			if nfdInterrupted || nfdRem > 0 || ctx.Err() != nil || nfdSlow {
				nfdRemTotal := len(nfdSlice) - ni
				dbRemTotal := len(dropboxSlice) - di
				if nfdRemTotal == 0 && dbRemTotal == 0 && page.eof {
					if opts.runDropbox && hadCursor {
						removeDropboxCursor(mount)
					}
				} else {
					saveCursorIfAdvanced(completedID)
				}
				res.nfdRemaining = nfdRemTotal
				res.dropboxRemaining = dbRemTotal
				res.stoppedEarly = true
				return res, hadCursor, nil
			}
		}

		completedID = page.lastID
		afterID = page.lastID
		if page.eof {
			if outerTx == nil && opts.runDropbox && hadCursor {
				removeDropboxCursor(mount)
			}
			return res, hadCursor, nil
		}
	}
}

func normalizeTrackPaths(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	candidates, err := collectTrackPathCandidates(ctx, db)
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	bridgeManaged, err := bridgeManagedTracksContext(ctx, mount)
	if err != nil {
		if isContextStop(ctx, err) {
			return len(candidates), nil
		}
		bridgeManaged = map[int64]bool{}
		for _, candidate := range candidates {
			bridgeManaged[candidate.id] = true
		}
	}
	rem, _, _, _, err := normalizeTrackPathsBatch(
		ctx, db, nil, mount, candidates, bridgeManaged,
		map[string]int64{}, make(map[string]map[string]bool), r,
	)
	return rem, err
}

func normalizeTrackPathsBatch(
	ctx context.Context,
	q sqlQuerier,
	outerTx *sql.Tx,
	mount string,
	candidates []trackPathCandidate,
	bridgeManaged map[int64]bool,
	claimed map[string]int64,
	dirs map[string]map[string]bool,
	r *Report,
) (remaining int, lastCompletedID int64, rewritten map[int64]string, interrupted bool, err error) {
	if len(candidates) == 0 {
		return 0, 0, nil, false, nil
	}
	db, _ := q.(*sql.DB)
	allCandidates := candidates
	eligible := make([]trackPathCandidate, 0, len(candidates))
	for i, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			if outerTx != nil {
				return len(candidates) - i, lastCompletedID, nil, true, err
			}
			return len(candidates) - i, lastCompletedID, nil, true, nil
		}
		if bridgeManaged[candidate.id] {
			if len(eligible) == 0 && candidate.id > lastCompletedID {
				lastCompletedID = candidate.id
			}
			continue
		}
		var other int64
		err := q.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, candidate.nfcPath).Scan(&other)
		if err == nil && other != candidate.id {
			if len(eligible) == 0 && candidate.id > lastCompletedID {
				lastCompletedID = candidate.id
			}
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return len(candidates) - i, lastCompletedID, nil, false, err
		}
		if other, collision := claimed[candidate.nfcPath]; collision && other != candidate.id {
			if len(eligible) == 0 && candidate.id > lastCompletedID {
				lastCompletedID = candidate.id
			}
			continue
		}
		claimed[candidate.nfcPath] = candidate.id
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return 0, allCandidates[len(allCandidates)-1].id, nil, false, nil
	}
	candidates = eligible
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
	rewritten = map[int64]string{}
	for start := 0; start < len(candidates); start += 64 {
		if err := ctx.Err(); err != nil {
			if outerTx != nil {
				return len(candidates) - start, lastCompletedID, rewritten, true, err
			}
			return len(candidates) - start, lastCompletedID, rewritten, true, nil
		}
		end := min(start+64, len(candidates))
		tx := outerTx
		if tx == nil {
			tx, err = db.Begin()
			if err != nil {
				return len(candidates) - start, lastCompletedID, rewritten, false, err
			}
		}
		next := start
		batchFixed := 0
		batchLastID := lastCompletedID
		batchRewritten := make([]trackPathCandidate, 0, end-start)
		for _, candidate := range candidates[start:end] {
			if ctx.Err() != nil {
				break
			}
			next++
			if candidate.id > batchLastID {
				batchLastID = candidate.id
			}
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
				if outerTx == nil {
					_ = tx.Rollback()
				}
				return len(candidates) - start, lastCompletedID, rewritten, false, err
			}
			batchFixed++
			batchRewritten = append(batchRewritten, candidate)
		}
		if outerTx == nil {
			if err := tx.Commit(); err != nil {
				return len(candidates) - start, lastCompletedID, rewritten, false, err
			}
		}
		if batchLastID > lastCompletedID {
			lastCompletedID = batchLastID
		}
		if next == end && end == len(candidates) {
			if tailID := allCandidates[len(allCandidates)-1].id; tailID > lastCompletedID {
				lastCompletedID = tailID
			}
		}
		for _, candidate := range batchRewritten {
			rewritten[candidate.id] = candidate.nfcPath
		}
		r.PathsNormalized += batchFixed
		if normalizeTrackPathBatch != nil {
			normalizeTrackPathBatch()
		}
		if next < end {
			if outerTx != nil {
				return len(candidates) - next, lastCompletedID, rewritten, true, ctx.Err()
			}
			return len(candidates) - next, lastCompletedID, rewritten, true, nil
		}
		remaining = len(candidates) - end
		if err := ctx.Err(); err != nil {
			if outerTx != nil {
				return remaining, lastCompletedID, rewritten, true, err
			}
			return remaining, lastCompletedID, rewritten, true, nil
		}
	}
	return 0, lastCompletedID, rewritten, false, nil
}

func collectTrackPathCandidates(ctx context.Context, db *sql.DB) ([]trackPathCandidate, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, path, COALESCE(streamingSource,'') FROM Track WHERE path IS NOT NULL AND path <> ''`)
	if err != nil {
		return nil, err
	}
	candidates := []trackPathCandidate{}
	for rows.Next() {
		var id int64
		var path, streaming string
		if err := rows.Scan(&id, &path, &streaming); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if streaming == "" && !norm.NFC.IsNormalString(path) {
			candidates = append(candidates, trackPathCandidate{id: id, path: path, nfcPath: norm.NFC.String(path)})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func bridgeManagedTracks(mount string) (map[int64]bool, error) {
	return bridgeManagedTracksContext(context.Background(), mount)
}

func bridgeManagedTracksContext(ctx context.Context, mount string) (map[int64]bool, error) {
	managed := map[int64]bool{}
	if err := ctx.Err(); err != nil {
		return managed, err
	}
	// The rekordbox bridge's snapshot of this library; a library an earlier build imported, and
	// that no mount has taken over yet, has the state database read below instead.
	snapPath := filepath.Join(mount, "Engine Library", "Database2", rbstate.SnapshotName)
	f, openErr := os.Open(snapPath)
	if openErr == nil {
		snap, loadErr := rbstate.LoadReaderContext(ctx, f)
		_ = f.Close()
		if loadErr != nil {
			return managed, loadErr
		}
		ids, err := snap.EngineIDsContext(ctx)
		if err != nil {
			return managed, err
		}
		for id := range ids {
			managed[id] = true
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return managed, openErr
	}
	if err := ctx.Err(); err != nil {
		return managed, err
	}
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
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='BridgeState'`).Scan(&exists); err != nil {
		return managed, err
	}
	if exists == 0 {
		return managed, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT trackId FROM BridgeState`)
	if err != nil {
		return managed, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return managed, err
		}
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
	return inspectMountContext(context.Background(), mount)
}

func inspectMountContext(ctx context.Context, mount string) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r := Report{Mount: mount, CheckedAt: time.Now().UTC()}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	rw, err := writable(filepath.Join(dir, "m.db"))
	if err != nil {
		return r, err
	}
	r.ReadOnly = !rw
	for _, name := range []string{"m.db", "hm.db"} {
		if cerr := ctx.Err(); cerr != nil {
			return r, cerr
		}
		path := filepath.Join(dir, name)
		if _, e := os.Stat(path); errors.Is(e, os.ErrNotExist) && name == "hm.db" {
			continue
		} else if e != nil {
			return r, e
		}
		db, e := openContext(ctx, path, true)
		if e != nil {
			return r, fmt.Errorf("%s: %w", name, e)
		}
		var check string
		e = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
		if e == nil && check != "ok" {
			e = fmt.Errorf("integrity: %s", check)
		}
		if e == nil {
			var exists int
			e = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='GuardianReanalysis' AND type='table'").Scan(&exists)
			if e == nil && exists != 0 {
				var pending int
				e = db.QueryRowContext(ctx, "SELECT count(*) FROM GuardianReanalysis").Scan(&pending)
				if e == nil {
					r.ReanalysisPending += pending
				}
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
	if err := ctx.Err(); err != nil {
		return r, err
	}
	r.Healthy = true
	return r, nil
}
