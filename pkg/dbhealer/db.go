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

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
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
	DropboxLocalized  int       `json:"dropbox_localized,omitempty"`
	HistoryPruned     bool      `json:"history_pruned"`
	ReserveFixed      []string  `json:"reserve_fixed,omitempty"`
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
		return r, fmt.Errorf("database integrity: %s", check)
	}
	var major, minor, patch int
	if err := db.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return r, fmt.Errorf("missing library version: %w", err)
	}
	if major != 3 || minor != 0 || patch != 2 {
		return r, fmt.Errorf("unsupported library schema %d.%d.%d (supported 3.0.2)", major, minor, patch)
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
			return fmt.Errorf("unsupported library schema: %w", err)
		}
		// GL-010: Engine rejects libraries where Information.id != 1 or uuid is empty.
		if err := repairLibraryIdentityTx(tx); err != nil {
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
	dbUUID, dropboxSchemaOK, schemaErr := librarySchema302UUID(ctx, db)
	if schemaErr != nil && isContextStop(ctx, schemaErr) {
		if idErr := repairLibraryIdentity(db); idErr != nil {
			_ = db.Close()
			r.Mount = mount
			return r, idErr
		}
		r.Healthy = true
		closeErr := db.Close()
		r.Mount = mount
		if closeErr != nil {
			return r, closeErr
		}
		return r, nil
	}
	if dropboxSchemaOK && dbUUID == "" {
		if idErr := repairLibraryIdentity(db); idErr == nil {
			dbUUID, _, _ = librarySchema302UUID(context.Background(), db)
		}
	}
	passRes, err := runMountTrackPass(ctx, db, mount, &r, mountTrackPassOptions{
		runNFD:     true,
		runDropbox: dropboxSchemaOK,
		dataOnly:   dataOnly,
		dbUUID:     dbUUID,
		pageSize:   mountDiscoveryPageSize,
	})
	if err != nil && !isContextStop(ctx, err) {
		// A non-context database error during the track pass means the library
		// may be damaged. Run the existing base heal as the recovery path.
		pathsFixed := r.PathsNormalized
		dropboxFixed := r.DropboxLocalized
		r, healErr := heal(db, dataOnly)
		r.ReserveFixed = fixed
		r.PathsNormalized = pathsFixed
		r.DropboxLocalized = dropboxFixed
		_ = db.Close()
		r.Mount = mount
		if healErr != nil {
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
		// Preserve GL-010 library identity before returning early from any
		// budgeted data-only mount pass so Engine can still attach the library.
		if dropboxSchemaOK {
			if idErr := repairLibraryIdentity(db); idErr != nil {
				_ = db.Close()
				r.Mount = mount
				return r, idErr
			}
		}
		r.Healthy = true
		closeErr := db.Close()
		r.Mount = mount
		log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, passRes.nfdRemaining)
		if r.DropboxLocalized > 0 || passRes.dropboxRemaining > 0 {
			log.Printf("dbhealer: mount %s localized %d dropbox tracks, %d remaining", mount, r.DropboxLocalized, passRes.dropboxRemaining)
		}
		if closeErr != nil {
			return r, closeErr
		}
		return r, nil
	}
	pathsFixed := r.PathsNormalized
	dropboxFixed := r.DropboxLocalized
	r, err = heal(db, dataOnly)
	r.ReserveFixed = fixed
	r.PathsNormalized = pathsFixed
	r.DropboxLocalized = dropboxFixed
	if err != nil {
		_ = db.Close()
		r.Mount = mount
		return r, err
	}
	closeErr := db.Close()
	r.Mount = mount
	log.Printf("dbhealer: mount %s normalized %d track paths, %d remaining", mount, r.PathsNormalized, passRes.nfdRemaining)
	if r.DropboxLocalized > 0 || passRes.dropboxRemaining > 0 {
		log.Printf("dbhealer: mount %s localized %d dropbox tracks, %d remaining", mount, r.DropboxLocalized, passRes.dropboxRemaining)
	}
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
		hdb, e := Open(history, false)
		if e != nil {
			return r, e
		}
		hr, e := heal(hdb, dataOnly)
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
			return r, nil
		}
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
	path := filepath.Join(root, "Engine Library", "Database2", "m.db")
	var want unix.Stat_t
	if err := unix.Stat(path, &want); err != nil {
		return false
	}
	wantDev, wantIno := uint64(want.Dev), uint64(want.Ino)
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
			if pidHoldsFile(pid, wantDev, wantIno, &found) {
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
				if pidHoldsFile(pids[idx], wantDev, wantIno, &found) {
					found.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()
	return found.Load()
}

func pidHoldsFile(pid string, wantDev, wantIno uint64, found *atomic.Bool) bool {
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
		if unix.Fstatat(dirFD, fd, &st, 0) == nil && uint64(st.Dev) == wantDev && uint64(st.Ino) == wantIno {
			_ = fdDir.Close()
			return true
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

func repairLibraryIdentityTx(tx *sql.Tx) error {
	var infoCount int
	if err := tx.QueryRow("SELECT count(*) FROM Information").Scan(&infoCount); err != nil {
		return err
	}
	if infoCount == 1 {
		if _, err := tx.Exec("UPDATE Information SET id=1 WHERE id<>1"); err != nil {
			return err
		}
	}
	_, err := tx.Exec("UPDATE Information SET uuid=? WHERE uuid IS NULL OR trim(uuid)=''", uuid.NewString())
	return err
}

func repairLibraryIdentity(db *sql.DB) error {
	rows, err := db.Query("SELECT id, COALESCE(uuid,''), schemaVersionMajor, schemaVersionMinor, schemaVersionPatch FROM Information LIMIT 2")
	if err != nil {
		return err
	}
	type infoRow struct {
		id                  int
		uuid                string
		major, minor, patch int
	}
	var infos []infoRow
	for rows.Next() {
		var row infoRow
		if err := rows.Scan(&row.id, &row.uuid, &row.major, &row.minor, &row.patch); err != nil {
			_ = rows.Close()
			return err
		}
		infos = append(infos, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(infos) == 0 {
		return fmt.Errorf("missing library version: %w", sql.ErrNoRows)
	}
	if infos[0].major != 3 || infos[0].minor != 0 || infos[0].patch != 2 {
		return fmt.Errorf("unsupported library schema %d.%d.%d (supported 3.0.2)", infos[0].major, infos[0].minor, infos[0].patch)
	}
	if len(infos) == 1 && infos[0].id == 1 && strings.TrimSpace(infos[0].uuid) != "" {
		return nil
	}
	return transaction(db, repairLibraryIdentityTx)
}

func isContextStop(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return err != nil && ctx.Err() != nil
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

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func prepareMountPageScanStmt(ctx context.Context, db *sql.DB, runNFD, runDropbox bool) (*sql.Stmt, error) {
	switch {
	case runNFD && runDropbox:
		return db.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> '' AND (
			            octet_length(path) <> length(path)
			            OR streamingSource <> ''
			            OR uri <> ''
			        ) THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource, uri FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	case runDropbox:
		return db.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> '' AND (streamingSource <> '' OR uri <> '') THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource, uri FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	default:
		return db.PrepareContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(id), 0),
			        COALESCE(SUM(CASE WHEN path <> ''
			            AND COALESCE(streamingSource,'') = ''
			            AND octet_length(path) <> length(path)
			        THEN 1 ELSE 0 END), 0)
			 FROM (SELECT id, path, streamingSource FROM Track WHERE id > ? ORDER BY id LIMIT ?)`,
		)
	}
}

func prepareMountPageDetailStmt(ctx context.Context, db *sql.DB, runNFD, runDropbox bool) (*sql.Stmt, error) {
	switch {
	case runNFD && runDropbox:
		return db.PrepareContext(ctx,
			`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,''), COALESCE(fileBytes,0)
			 FROM Track
			 WHERE id > ? AND id <= ?
			   AND path <> ''
			   AND (octet_length(path) <> length(path) OR streamingSource <> '' OR uri <> '')
			 ORDER BY id`,
		)
	case runDropbox:
		return db.PrepareContext(ctx,
			`SELECT id, COALESCE(path,''), COALESCE(streamingSource,''), COALESCE(uri,''), COALESCE(fileBytes,0)
			 FROM Track
			 WHERE id > ? AND id <= ?
			   AND path <> ''
			   AND (streamingSource <> '' OR uri <> '')
			 ORDER BY id`,
		)
	default:
		return db.PrepareContext(ctx,
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
	db *sql.DB,
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
		*detailStmt, err = prepareMountPageDetailStmt(ctx, db, runNFD, runDropbox)
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
		if runNFD && src == "" && !isASCII(path) && !norm.NFC.IsNormalString(path) {
			page.nfd = append(page.nfd, trackPathCandidate{id: id, path: path, nfcPath: norm.NFC.String(path)})
		}
		if runDropbox && isDropboxLocalizableTag(src, uri) {
			page.dropbox = append(page.dropbox, dropboxTrackCandidate{id: id, path: path, fileBytes: fileBytes})
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
	var res mountTrackPassResult
	pageSize := opts.pageSize
	if pageSize <= 0 {
		pageSize = mountDiscoveryPageSize
	}
	var startAfterID int64
	var hadCursor bool
	if opts.runDropbox {
		startAfterID, hadCursor = readDropboxCursor(mount, opts.dbUUID)
		res.resumed = hadCursor && startAfterID > 0
	}
	completedID := startAfterID
	saveCursorIfAdvanced := func(cursorID int64) {
		if opts.runDropbox && cursorID > startAfterID {
			writeDropboxCursor(mount, opts.dbUUID, cursorID)
		}
	}
	if ctx.Err() != nil {
		res.stoppedEarly = true
		return res, nil
	}

	pageStmt, err := prepareMountPageScanStmt(ctx, db, opts.runNFD, opts.runDropbox)
	if err != nil {
		if isContextStop(ctx, err) {
			res.stoppedEarly = true
			return res, nil
		}
		return res, err
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
		if ctx.Err() != nil {
			saveCursorIfAdvanced(completedID)
			res.stoppedEarly = true
			return res, nil
		}
		page, err := scanMountTrackPage(ctx, db, pageStmt, &detailStmt, afterID, pageSize, opts.runNFD, opts.runDropbox)
		if err != nil {
			if isContextStop(ctx, err) {
				saveCursorIfAdvanced(completedID)
				res.stoppedEarly = true
				return res, nil
			}
			return res, err
		}
		res.scannedRows += page.count
		if page.count == 0 {
			if opts.runDropbox && hadCursor {
				removeDropboxCursor(mount)
			}
			return res, nil
		}

		// 1. #322 NFD->NFC normalization for ordinary non-streaming rows on this page.
		var rewritten map[int64]string
		if opts.runNFD && len(page.nfd) > 0 {
			bridgeOK, berr := ensureBridge()
			if berr != nil {
				if isContextStop(ctx, berr) {
					saveCursorIfAdvanced(completedID)
					res.stoppedEarly = true
					return res, nil
				}
				return res, berr
			}
			if bridgeOK {
				nfdRem, nfdLastID, rw, nerr := normalizeTrackPathsBatch(ctx, db, mount, page.nfd, bridgeManaged, claimedNFC, dirs, r)
				if nerr != nil {
					if isContextStop(ctx, nerr) {
						safeID := completedID
						if nfdLastID > safeID && (len(page.dropbox) == 0 || nfdLastID < page.dropbox[0].id) {
							safeID = nfdLastID
						}
						saveCursorIfAdvanced(safeID)
						res.nfdRemaining = nfdRem
						res.stoppedEarly = true
						return res, nil
					}
					return res, nerr
				}
				rewritten = rw
				nfdSlow := opts.dataOnly && r.PathsNormalized != nfdBefore && time.Since(passStart) >= time.Second
				if nfdRem > 0 || ctx.Err() != nil || nfdSlow {
					safeID := completedID
					if nfdLastID > safeID && (len(page.dropbox) == 0 || nfdLastID < page.dropbox[0].id) {
						safeID = nfdLastID
					}
					if nfdRem == 0 && len(page.dropbox) == 0 && page.eof {
						if opts.runDropbox && hadCursor {
							removeDropboxCursor(mount)
						}
					} else {
						saveCursorIfAdvanced(safeID)
					}
					res.nfdRemaining = nfdRem
					res.stoppedEarly = true
					return res, nil
				}
			}
		}

		// Apply any paths normalized above to Dropbox candidates on this page
		// (e.g. packTracks rows with empty streamingSource + Dropbox URI).
		if len(rewritten) > 0 && len(page.dropbox) > 0 {
			for i := range page.dropbox {
				if nextPath, ok := rewritten[page.dropbox[i].id]; ok {
					page.dropbox[i].path = nextPath
				}
			}
		}

		// 2. S22 Dropbox localization for complete local files on this page.
		if opts.runDropbox && len(page.dropbox) > 0 {
			bridgeOK, berr := ensureBridge()
			if berr != nil {
				if isContextStop(ctx, berr) {
					saveCursorIfAdvanced(completedID)
					res.stoppedEarly = true
					return res, nil
				}
				return res, berr
			}
			if bridgeOK {
				dbRem, examinedID, interrupted, derr := processDropboxCandidates(
					ctx, db, mount, r, page.dropbox, bridgeManaged, safeDirs, claimedNFC,
				)
				if derr != nil {
					if isContextStop(ctx, derr) {
						cursorID := completedID
						if examinedID > cursorID {
							cursorID = examinedID
						}
						saveCursorIfAdvanced(cursorID)
						res.dropboxRemaining = dbRem
						res.stoppedEarly = true
						return res, nil
					}
					return res, derr
				}
				dropboxSlow := opts.dataOnly && r.DropboxLocalized != dropboxBefore && time.Since(passStart) >= time.Second
				if interrupted || dbRem > 0 || ctx.Err() != nil || dropboxSlow {
					cursorID := completedID
					if interrupted || dbRem > 0 {
						if examinedID > cursorID {
							cursorID = examinedID
						}
					} else if page.lastID > cursorID {
						cursorID = page.lastID
					}
					if !page.eof || dbRem > 0 || interrupted {
						saveCursorIfAdvanced(cursorID)
					} else if hadCursor {
						removeDropboxCursor(mount)
					}
					res.dropboxRemaining = dbRem
					res.stoppedEarly = true
					return res, nil
				}
			}
		}

		completedID = page.lastID
		afterID = page.lastID
		if page.eof {
			if opts.runDropbox && hadCursor {
				removeDropboxCursor(mount)
			}
			return res, nil
		}
	}
}

func normalizeTrackPaths(ctx context.Context, db *sql.DB, mount string, r *Report) (int, error) {
	candidates, err := collectTrackPathCandidates(db)
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
	rem, _, _, err := normalizeTrackPathsBatch(
		ctx, db, mount, candidates, bridgeManaged,
		map[string]int64{}, make(map[string]map[string]bool), r,
	)
	return rem, err
}

func normalizeTrackPathsBatch(
	ctx context.Context,
	db *sql.DB,
	mount string,
	candidates []trackPathCandidate,
	bridgeManaged map[int64]bool,
	claimed map[string]int64,
	dirs map[string]map[string]bool,
	r *Report,
) (remaining int, lastCompletedID int64, rewritten map[int64]string, err error) {
	if len(candidates) == 0 {
		return 0, 0, nil, nil
	}
	eligible := make([]trackPathCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return len(candidates), lastCompletedID, nil, nil
		}
		if bridgeManaged[candidate.id] {
			continue
		}
		var other int64
		err := db.QueryRowContext(ctx, `SELECT id FROM Track WHERE path=?`, candidate.nfcPath).Scan(&other)
		if err == nil && other != candidate.id {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, lastCompletedID, nil, err
		}
		if other, collision := claimed[candidate.nfcPath]; collision && other != candidate.id {
			continue
		}
		claimed[candidate.nfcPath] = candidate.id
		eligible = append(eligible, candidate)
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
		if ctx.Err() != nil {
			return len(candidates) - start, lastCompletedID, rewritten, nil
		}
		end := min(start+64, len(candidates))
		tx, err := db.Begin()
		if err != nil {
			return remaining, lastCompletedID, rewritten, err
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
				_ = tx.Rollback()
				return remaining, lastCompletedID, rewritten, err
			}
			batchFixed++
			batchRewritten = append(batchRewritten, candidate)
		}
		if err := tx.Commit(); err != nil {
			return remaining, lastCompletedID, rewritten, err
		}
		if batchLastID > lastCompletedID {
			lastCompletedID = batchLastID
		}
		for _, candidate := range batchRewritten {
			rewritten[candidate.id] = candidate.nfcPath
		}
		r.PathsNormalized += batchFixed
		if normalizeTrackPathBatch != nil {
			normalizeTrackPathBatch()
		}
		if next < end {
			return len(candidates) - next, lastCompletedID, rewritten, nil
		}
		remaining = len(candidates) - end
	}
	return remaining, lastCompletedID, rewritten, nil
}

func collectTrackPathCandidates(db *sql.DB) ([]trackPathCandidate, error) {
	rows, err := db.Query(`SELECT id, path, COALESCE(streamingSource,'') FROM Track WHERE path IS NOT NULL AND path <> ''`)
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
