package dbhealer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	snapshotRelDir        = ".enginex-backup/db"
	snapshotMinBudget     = 2 * time.Second
	defaultMinFreeSpace   = uint64(64 << 20)
	logicalCollapseSample = 16
)

var (
	errDatabaseCorrupt   = errors.New("database corrupt")
	errUnsupportedSchema = errors.New("unsupported library schema")

	snapshotMinFreeBytes = defaultMinFreeSpace
	statfsFreeBytesFn    = statfsFreeBytes
	snapshotNowFn        = func() time.Time { return time.Now().UTC() }
)

type dbSnapshotMeta struct {
	UUID             string `json:"uuid"`
	Size             int64  `json:"size"`
	ChangeCounter    uint32 `json:"change_counter"`
	SHA256           string `json:"sha256"`
	Tracks           int64  `json:"tracks"`
	Playlists        int64  `json:"playlists"`
	PlaylistEntities int64  `json:"playlist_entities"`
	SavedAt          string `json:"saved_at"`
}

type mainDBStats struct {
	UUID             string
	Tracks           int64
	Playlists        int64
	PlaylistEntities int64
}

func snapshotDir(mount string) string {
	return filepath.Join(mount, ".enginex-backup", "db")
}

func snapshotMainPath(mount string) string {
	return filepath.Join(snapshotDir(mount), "m.db")
}

func snapshotMetaPath(mount string) string {
	return filepath.Join(snapshotDir(mount), "m.db.meta")
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func statfsFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, nil
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// readSQLiteHeader inspects the 100-byte SQLite header, returning file size
// and the big-endian file change counter (bytes 24..27). ok is false for
// empty, truncated, or non-SQLite files.
func readSQLiteHeader(path string) (size int64, changeCounter uint32, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	size = info.Size()
	if size < 100 {
		return size, 0, false, nil
	}
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return size, 0, false, nil
	}
	if !bytes.HasPrefix(h[:], sqliteMagic) {
		return size, 0, false, nil
	}
	return size, binary.BigEndian.Uint32(h[24:28]), true, nil
}

func hasPendingJournal(path string) bool {
	for _, suffix := range []string{"-journal", "-wal"} {
		if st, err := os.Stat(path + suffix); err == nil && st.Size() > 0 {
			return true
		}
	}
	return false
}

func isStructuralCorruption(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errUnsupportedSchema) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errLibraryInUse) {
		return false
	}
	if errors.Is(err, errDatabaseCorrupt) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"database integrity:",
		"file is not a database",
		"database disk image is malformed",
		"malformed",
		"corrupt",
		"missing library version",
		"no such table:",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func readMainDBStats(db *sql.DB) (mainDBStats, error) {
	var st mainDBStats
	if err := db.QueryRow("SELECT COALESCE(uuid,'') FROM Information LIMIT 1").Scan(&st.UUID); err != nil {
		return st, err
	}
	if err := db.QueryRow("SELECT count(*) FROM Track").Scan(&st.Tracks); err != nil {
		return st, err
	}
	if err := db.QueryRow("SELECT count(*) FROM Playlist").Scan(&st.Playlists); err != nil {
		return st, err
	}
	if err := db.QueryRow("SELECT count(*) FROM PlaylistEntity").Scan(&st.PlaylistEntities); err != nil {
		return st, err
	}
	return st, nil
}

func loadSnapshotMeta(mount string) (dbSnapshotMeta, bool) {
	var meta dbSnapshotMeta
	raw, err := os.ReadFile(snapshotMetaPath(mount))
	if err != nil {
		return meta, false
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, false
	}
	if meta.UUID == "" || meta.SHA256 == "" || meta.Size < 100 {
		return meta, false
	}
	return meta, true
}

func isLogicalCollapse(prev dbSnapshotMeta, cur mainDBStats) bool {
	if prev.UUID == "" || prev.UUID != cur.UUID {
		return false
	}
	if prev.Playlists < 3 || prev.PlaylistEntities < 10 || prev.Tracks < 20 {
		return false
	}
	return cur.Playlists == 0 && cur.PlaylistEntities == 0 && cur.Tracks*2 <= prev.Tracks
}

func hashFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func verifySnapshot(mount string) (string, dbSnapshotMeta, error) {
	meta, ok := loadSnapshotMeta(mount)
	if !ok {
		return "", meta, os.ErrNotExist
	}
	snapPath := snapshotMainPath(mount)
	sum, size, err := hashFileSHA256(snapPath)
	if err != nil {
		return "", meta, err
	}
	if size != meta.Size || sum != meta.SHA256 {
		return "", meta, fmt.Errorf("snapshot digest mismatch")
	}
	res, magicOK, err := Reserve(snapPath)
	if err != nil || !magicOK || res != 0 {
		return "", meta, fmt.Errorf("snapshot header invalid (reserve=%d ok=%v err=%v)", res, magicOK, err)
	}
	sdb, err := Open(snapPath, true)
	if err != nil {
		return "", meta, err
	}
	defer sdb.Close()
	var check string
	if err := sdb.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return "", meta, fmt.Errorf("snapshot integrity failed: %q %v", check, err)
	}
	var major, minor, patch int
	if err := sdb.QueryRow("SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return "", meta, err
	}
	if major != 3 || minor != 0 || patch != 2 {
		return "", meta, fmt.Errorf("snapshot schema %d.%d.%d unsupported", major, minor, patch)
	}
	return snapPath, meta, nil
}

// missingTracksStillOnDisk samples up to logicalCollapseSample local tracks
// present in snapPath but missing from curDB, returning true only when at
// least 4 such tracks were sampled and >=75% of their audio files still exist
// on disk (distinguishing database truncation corruption from intentional
// Sync Manager track removals that delete audio files from the stick).
func missingTracksStillOnDisk(mount, snapPath string, curDB *sql.DB) bool {
	sdb, err := Open(snapPath, true)
	if err != nil {
		return false
	}
	defer sdb.Close()

	curPaths := make(map[string]struct{})
	crows, err := curDB.Query(`SELECT path FROM Track WHERE path IS NOT NULL AND path <> ''`)
	if err != nil {
		return false
	}
	for crows.Next() {
		var p string
		if err := crows.Scan(&p); err == nil {
			curPaths[p] = struct{}{}
		}
	}
	crows.Close()

	srows, err := sdb.Query(`SELECT path FROM Track WHERE COALESCE(streamingSource,'')='' AND path IS NOT NULL AND path <> '' ORDER BY id`)
	if err != nil {
		return false
	}
	defer srows.Close()

	sampled, onDisk := 0, 0
	for srows.Next() && sampled < logicalCollapseSample {
		var p string
		if err := srows.Scan(&p); err != nil {
			continue
		}
		if _, exists := curPaths[p]; exists {
			continue
		}
		full, ok := trackPath(mount, p)
		if !ok {
			continue
		}
		sampled++
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
			onDisk++
		}
	}
	return sampled >= 4 && onDisk*4 >= sampled*3
}

func makeQuarantineDir(mount string) (string, error) {
	base := snapshotDir(mount)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	_ = syncDir(filepath.Dir(base))
	stamp := snapshotNowFn().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := "corrupt-" + stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		dir := filepath.Join(base, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			_ = syncDir(base)
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not create quarantine directory")
}

func copyFileChunked(ctx context.Context, srcPath, dstPath string) (string, int64, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	cleanup := func() {
		_ = dst.Close()
		_ = os.Remove(dstPath)
	}
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var total int64
	for {
		if ctx != nil && ctx.Err() != nil {
			cleanup()
			return "", 0, ctx.Err()
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				cleanup()
				return "", 0, werr
			}
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cleanup()
			return "", 0, rerr
		}
	}
	if err := dst.Sync(); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dstPath)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

// quarantineDBSet moves dbPath and any SQLite journal/WAL/SHM sidecars into
// a timestamped quarantine directory under /.enginex-backup/db/ without
// losing forensic data.
func quarantineDBSet(mount, dbPath string) (string, error) {
	qdir, err := makeQuarantineDir(mount)
	if err != nil {
		return "", err
	}
	baseName := filepath.Base(dbPath)
	movedAny := false
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		dst := filepath.Join(qdir, baseName+suffix)
		if err := os.Rename(src, dst); err != nil {
			// Cross-device or FAT rename fallback: durable copy then remove.
			if _, _, cerr := copyFileChunked(context.Background(), src, dst); cerr != nil {
				return "", cerr
			}
			if rerr := os.Remove(src); rerr != nil {
				return "", rerr
			}
		}
		movedAny = true
	}
	if movedAny {
		_ = syncDir(qdir)
		_ = syncDir(filepath.Dir(dbPath))
	}
	rel, err := filepath.Rel(mount, qdir)
	if err != nil {
		return qdir, nil
	}
	return filepath.ToSlash(rel), nil
}

// quarantineDBCopy copies dbPath and any SQLite journal/WAL/SHM sidecars into
// a timestamped quarantine directory without removing dbPath first, so a live
// file can be atomically replaced via rename with no window where it is absent.
func quarantineDBCopy(mount, dbPath string) (string, error) {
	qdir, err := makeQuarantineDir(mount)
	if err != nil {
		return "", err
	}
	baseName := filepath.Base(dbPath)
	copiedAny := false
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		dst := filepath.Join(qdir, baseName+suffix)
		if _, _, err := copyFileChunked(context.Background(), src, dst); err != nil {
			return "", err
		}
		copiedAny = true
	}
	if copiedAny {
		_ = syncDir(qdir)
	}
	rel, err := filepath.Rel(mount, qdir)
	if err != nil {
		return qdir, nil
	}
	return filepath.ToSlash(rel), nil
}

// restoreMainSnapshot durably copies the damaged m.db (+ journals) into
// quarantine, stages the verified snapshot, removes stale journals, and
// atomically renames the staged file over m.db so m.db is never absent.
func restoreMainSnapshot(mount, livePath, reason string) (string, dbSnapshotMeta, bool, error) {
	snapPath, meta, err := verifySnapshot(mount)
	if err != nil {
		return "", meta, false, nil
	}
	qrel, err := quarantineDBCopy(mount, livePath)
	if err != nil {
		return "", meta, false, fmt.Errorf("quarantine damaged m.db: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(livePath), ".m.db.restore.tmp")
	_ = os.Remove(tmp)
	sum, size, err := copyFileChunked(context.Background(), snapPath, tmp)
	if err != nil {
		return qrel, meta, false, fmt.Errorf("stage snapshot restore: %w", err)
	}
	if size != meta.Size || sum != meta.SHA256 {
		_ = os.Remove(tmp)
		return qrel, meta, false, fmt.Errorf("staged restore digest mismatch")
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := os.Remove(livePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return qrel, meta, false, fmt.Errorf("remove stale m.db%s: %w", suffix, err)
		}
	}
	if err := os.Rename(tmp, livePath); err != nil {
		_ = os.Remove(tmp)
		return qrel, meta, false, fmt.Errorf("publish restored m.db: %w", err)
	}
	if err := syncDir(filepath.Dir(livePath)); err != nil {
		return qrel, meta, false, fmt.Errorf("sync Database2 after restore: %w", err)
	}
	log.Printf("dbhealer: mount %s restored m.db from verified snapshot (%d tracks, %d playlists) after %s; damaged copy in %s",
		mount, meta.Tracks, meta.Playlists, reason, qrel)
	return qrel, meta, true, nil
}

// maybeSaveMainSnapshot maintains a crash-safe rolling known-good copy of m.db
// under /.enginex-backup/db/. Unchanged databases fast-skip via the SQLite
// header change counter with zero bytes copied.
func maybeSaveMainSnapshot(ctx context.Context, mount, livePath string, stats mainDBStats, r *Report) {
	if stats.UUID == "" || hasPendingJournal(livePath) {
		return
	}
	if ctx != nil {
		if ctx.Err() != nil {
			return
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < snapshotMinBudget {
			return
		}
	}
	curSize, changeCounter, magicOK, err := readSQLiteHeader(livePath)
	if err != nil || !magicOK || curSize < 100 {
		return
	}
	snapPath := snapshotMainPath(mount)
	if prev, hasPrev := loadSnapshotMeta(mount); hasPrev {
		if prev.UUID == stats.UUID &&
			prev.Size == curSize &&
			prev.ChangeCounter == changeCounter &&
			prev.Tracks == stats.Tracks &&
			prev.Playlists == stats.Playlists &&
			prev.PlaylistEntities == stats.PlaylistEntities {
			if st, err := os.Stat(snapPath); err == nil && st.Size() == prev.Size {
				return
			}
		}
		// Anti-clobber guard: never overwrite a populated verified snapshot
		// with an emptied/collapsed database of the same library UUID.
		if isLogicalCollapse(prev, stats) {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"preserved prior m.db snapshot (%d tracks, %d playlists) instead of overwriting with emptied library (%d tracks, %d playlists)",
				prev.Tracks, prev.Playlists, stats.Tracks, stats.Playlists,
			))
			return
		}
	}
	free, err := statfsFreeBytesFn(mount)
	if err == nil && free < uint64(curSize)*3+snapshotMinFreeBytes {
		return
	}
	dir := snapshotDir(mount)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = syncDir(filepath.Dir(dir))
	tmpDB := filepath.Join(dir, ".m.db.tmp")
	_ = os.Remove(tmpDB)
	sum, copied, err := copyFileChunked(ctx, livePath, tmpDB)
	if err != nil || copied != curSize {
		_ = os.Remove(tmpDB)
		return
	}
	if err := os.Rename(tmpDB, snapPath); err != nil {
		_ = os.Remove(tmpDB)
		return
	}
	if err := syncDir(dir); err != nil {
		return
	}
	meta := dbSnapshotMeta{
		UUID:             stats.UUID,
		Size:             curSize,
		ChangeCounter:    changeCounter,
		SHA256:           sum,
		Tracks:           stats.Tracks,
		Playlists:        stats.Playlists,
		PlaylistEntities: stats.PlaylistEntities,
		SavedAt:          snapshotNowFn().Format(time.RFC3339),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return
	}
	tmpMeta := filepath.Join(dir, ".m.db.meta.tmp")
	f, err := os.OpenFile(tmpMeta, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_, werr := f.Write(raw)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmpMeta)
		return
	}
	if err := os.Rename(tmpMeta, snapshotMetaPath(mount)); err != nil {
		_ = os.Remove(tmpMeta)
		return
	}
	_ = syncDir(dir)
}

// checkAndQuarantineSidecars verifies secondary Engine databases (sm.db,
// stm.db) that Engine attaches alongside m.db. A structurally corrupt sidecar
// triggers Engine's handleCorruptDB() and locks out the healthy m.db; moving
// the corrupt sidecar to quarantine lets Engine recreate it cleanly on attach.
func checkAndQuarantineSidecars(ctx context.Context, mount, dbDir string, r *Report) {
	for _, name := range []string{"sm.db", "stm.db"} {
		if ctx != nil && ctx.Err() != nil {
			return
		}
		p := filepath.Join(dbDir, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		rw, err := writable(p)
		if err != nil || !rw {
			continue
		}
		corruptReason := ""
		if _, _, magicOK, err := readSQLiteHeader(p); err == nil && !magicOK {
			corruptReason = "invalid or truncated SQLite header"
		} else if sdb, err := Open(p, false); err != nil {
			if isStructuralCorruption(err) {
				corruptReason = err.Error()
			}
		} else {
			var check string
			qerr := sdb.QueryRow("PRAGMA quick_check").Scan(&check)
			_ = sdb.Close()
			if qerr != nil {
				if isStructuralCorruption(qerr) {
					corruptReason = qerr.Error()
				}
			} else if check != "ok" {
				corruptReason = "integrity: " + check
			}
		}
		if corruptReason == "" {
			continue
		}
		qrel, err := quarantineDBSet(mount, p)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("corrupt %s could not be quarantined: %v", name, err))
			continue
		}
		r.QuarantinedDBs = append(r.QuarantinedDBs, name)
		r.Warnings = append(r.Warnings, fmt.Sprintf("quarantined corrupt %s (%s) to %s so m.db attaches cleanly", name, corruptReason, qrel))
		log.Printf("dbhealer: mount %s quarantined corrupt %s (%s) to %s", mount, name, corruptReason, qrel)
	}
}
