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
	mp3GridStampFile      = ".enginex-mp3-grid-stamp"
	mp3GridCursorFile     = ".enginex-mp3-grid-cursor"
)

var (
	errDatabaseCorrupt   = errors.New("database corrupt")
	errUnsupportedSchema = errors.New("unsupported library schema")

	snapshotMinFreeBytes = defaultMinFreeSpace
	statfsFreeBytesFn    = statfsFreeBytes
	snapshotNowFn        = func() time.Time { return time.Now().UTC() }
	syncDirFn            = syncDir
)

type dbSnapshotMeta struct {
	Generation       uint64 `json:"generation,omitempty"`
	File             string `json:"file,omitempty"`
	UUID             string `json:"uuid"`
	Size             int64  `json:"size"`
	ChangeCounter    uint32 `json:"change_counter"`
	SHA256           string `json:"sha256"`
	Tracks           int64  `json:"tracks"`
	Playlists        int64  `json:"playlists"`
	PlaylistEntities int64  `json:"playlist_entities"`
	MP3GridStamp     string `json:"mp3_grid_stamp,omitempty"`
	MP3GridCursor    string `json:"mp3_grid_cursor,omitempty"`
	SavedAt          string `json:"saved_at"`
}

type dbSnapshotManifest struct {
	Version   int             `json:"version"`
	NextGen   uint64          `json:"next_gen"`
	Current   *dbSnapshotMeta `json:"current,omitempty"`
	Previous  *dbSnapshotMeta `json:"previous,omitempty"`
	Populated *dbSnapshotMeta `json:"populated,omitempty"`
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
// empty, truncated, or non-SQLite files. Operational read errors are returned
// unchanged rather than disguised as header corruption.
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
	if info.IsDir() {
		return 0, 0, false, &os.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	size = info.Size()
	if size < 100 {
		return size, 0, false, nil
	}
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return size, 0, false, nil
		}
		return size, 0, false, err
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

func isOperationalIOError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EIO) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EROFS) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "disk i/o error") || strings.Contains(msg, "sqlite_ioerr")
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
		errors.Is(err, errLibraryInUse) ||
		isOperationalIOError(err) {
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
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func readMainDBStats(ctx context.Context, db *sql.DB) (mainDBStats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var st mainDBStats
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(uuid,'') FROM Information LIMIT 1").Scan(&st.UUID); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM Track").Scan(&st.Tracks); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM Playlist").Scan(&st.Playlists); err != nil {
		return st, err
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM PlaylistEntity").Scan(&st.PlaylistEntities); err != nil {
		return st, err
	}
	return st, nil
}

func isSafeSnapshotFileName(name string) bool {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return false
	}
	if name == "m.db" {
		return true
	}
	if !strings.HasPrefix(name, "m.db.gen-") {
		return false
	}
	rest := strings.TrimPrefix(name, "m.db.gen-")
	if rest == "" {
		return false
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

func validSnapshotMeta(m *dbSnapshotMeta) bool {
	if m == nil || m.UUID == "" || m.SHA256 == "" || m.Size < 100 {
		return false
	}
	if m.File == "" {
		m.File = "m.db"
	}
	return isSafeSnapshotFileName(m.File)
}

func cloneSnapshotMeta(m *dbSnapshotMeta) *dbSnapshotMeta {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

func loadSnapshotManifest(mount string) (dbSnapshotManifest, bool) {
	var manifest dbSnapshotManifest
	raw, err := os.ReadFile(snapshotMetaPath(mount))
	if err != nil {
		return manifest, false
	}
	if err := json.Unmarshal(raw, &manifest); err == nil && (manifest.Version >= 2 || manifest.Current != nil || manifest.Populated != nil || manifest.Previous != nil) {
		if !validSnapshotMeta(manifest.Current) {
			manifest.Current = nil
		}
		if !validSnapshotMeta(manifest.Previous) {
			manifest.Previous = nil
		}
		if !validSnapshotMeta(manifest.Populated) {
			manifest.Populated = nil
		}
		if manifest.Current == nil && manifest.Previous == nil && manifest.Populated == nil {
			return manifest, false
		}
		maxGen := uint64(0)
		for _, g := range []*dbSnapshotMeta{manifest.Current, manifest.Previous, manifest.Populated} {
			if g != nil && g.Generation > maxGen {
				maxGen = g.Generation
			}
		}
		if manifest.NextGen <= maxGen {
			manifest.NextGen = maxGen + 1
		}
		if manifest.NextGen == 0 {
			manifest.NextGen = 1
		}
		return manifest, true
	}

	// Backward compatibility with legacy v1 single-file m.db snapshot manifests.
	var legacy dbSnapshotMeta
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return manifest, false
	}
	if legacy.File == "" {
		legacy.File = "m.db"
	}
	if !validSnapshotMeta(&legacy) {
		return manifest, false
	}
	cur := legacy
	manifest = dbSnapshotManifest{
		Version: 1,
		NextGen: legacy.Generation + 1,
		Current: &cur,
	}
	if manifest.NextGen == 0 {
		manifest.NextGen = 1
	}
	if cur.Tracks > 0 || cur.Playlists > 0 {
		manifest.Populated = cloneSnapshotMeta(&cur)
	}
	return manifest, true
}

func loadSnapshotMeta(mount string) (dbSnapshotMeta, bool) {
	m, ok := loadSnapshotManifest(mount)
	if !ok {
		return dbSnapshotMeta{}, false
	}
	if m.Current != nil {
		return *m.Current, true
	}
	if m.Populated != nil {
		return *m.Populated, true
	}
	if m.Previous != nil {
		return *m.Previous, true
	}
	return dbSnapshotMeta{}, false
}

func snapshotGenFilePath(mount string, meta dbSnapshotMeta) string {
	name := meta.File
	if name == "" {
		name = "m.db"
	}
	return filepath.Join(snapshotDir(mount), name)
}

func snapshotGenFilePresent(mount string, meta dbSnapshotMeta) bool {
	if !validSnapshotMeta(&meta) {
		return false
	}
	st, err := os.Stat(snapshotGenFilePath(mount, meta))
	return err == nil && st.Mode().IsRegular() && st.Size() == meta.Size
}

func readSmallMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func currentMP3GridState(mount string) (stamp, cursor string) {
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	return readSmallMarker(filepath.Join(dbDir, mp3GridStampFile)),
		readSmallMarker(filepath.Join(dbDir, mp3GridCursorFile))
}

func writeSmallMarkerAtomic(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".enginex-marker-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.WriteString(content + "\n"); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Chmod(tmpPath, 0o644)
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// reconcileMP3GridStateAfterRestore ensures the on-disk MP3 grid completion
// stamp and resume cursor match the restored snapshot generation. Restoring a
// pre-stamp generation removes an obsolete stamp so FixMP3Grids repairs the
// restored library; restoring an already-stamped generation preserves its stamp
// so FixMP3Grids does not double-shift beat grids.
func reconcileMP3GridStateAfterRestore(dbDir string, meta dbSnapshotMeta) error {
	changed := false
	for _, item := range []struct {
		name string
		want string
	}{
		{mp3GridStampFile, strings.TrimSpace(meta.MP3GridStamp)},
		{mp3GridCursorFile, strings.TrimSpace(meta.MP3GridCursor)},
	} {
		p := filepath.Join(dbDir, item.name)
		cur := readSmallMarker(p)
		if cur == item.want {
			continue
		}
		if item.want == "" {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			if err := writeSmallMarkerAtomic(p, item.want); err != nil {
				return err
			}
		}
		changed = true
	}
	if changed {
		return syncDirFn(dbDir)
	}
	return nil
}

// isLogicalCollapse defines the strict threshold for automatic logical
// rollback of a live m.db (t/71200).
func isLogicalCollapse(prev dbSnapshotMeta, cur mainDBStats) bool {
	if prev.UUID == "" || prev.UUID != cur.UUID {
		return false
	}
	if prev.Playlists < 3 || prev.PlaylistEntities < 10 || prev.Tracks < 20 {
		return false
	}
	return cur.Playlists == 0 && cur.PlaylistEntities == 0 && cur.Tracks*2 <= prev.Tracks
}

// isRetentionProtectedDrop separates snapshot retention protection from the
// automatic rollback threshold so small libraries retain their populated
// backup when emptied or logically wiped while audio files remain on disk.
func isRetentionProtectedDrop(prev dbSnapshotMeta, cur mainDBStats) bool {
	if prev.UUID == "" || prev.UUID != cur.UUID {
		return false
	}
	if prev.Tracks == 0 && prev.Playlists == 0 {
		return false
	}
	if cur.Tracks == 0 && cur.Playlists == 0 {
		return true
	}
	return prev.Playlists > 0 && cur.Playlists == 0 && cur.PlaylistEntities == 0 && cur.Tracks*2 <= prev.Tracks
}

func hashFileSHA256Context(ctx context.Context, path string) (string, int64, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", 0, ctx.Err()
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var total int64
	for {
		if ctx != nil && ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func verifySnapshotFile(ctx context.Context, snapPath string, meta dbSnapshotMeta) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sum, size, err := hashFileSHA256Context(ctx, snapPath)
	if err != nil {
		return err
	}
	if size != meta.Size || sum != meta.SHA256 {
		return fmt.Errorf("snapshot digest mismatch")
	}
	res, magicOK, err := Reserve(snapPath)
	if err != nil || !magicOK || res != 0 {
		return fmt.Errorf("snapshot header invalid (reserve=%d ok=%v err=%v)", res, magicOK, err)
	}
	sdb, err := OpenContext(ctx, snapPath, true)
	if err != nil {
		return err
	}
	defer sdb.Close()
	var check string
	if err := sdb.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("snapshot integrity failed: %q %v", check, err)
	}
	var major, minor, patch int
	if err := sdb.QueryRowContext(ctx, "SELECT schemaVersionMajor,schemaVersionMinor,schemaVersionPatch FROM Information LIMIT 1").Scan(&major, &minor, &patch); err != nil {
		return err
	}
	if major != 3 || minor != 0 || patch != 2 {
		return fmt.Errorf("snapshot schema %d.%d.%d unsupported", major, minor, patch)
	}
	var snapUUID string
	if err := sdb.QueryRowContext(ctx, "SELECT COALESCE(uuid,'') FROM Information LIMIT 1").Scan(&snapUUID); err != nil {
		return err
	}
	if meta.UUID != "" && strings.TrimSpace(snapUUID) != meta.UUID {
		return fmt.Errorf("snapshot uuid mismatch (%q != %q)", snapUUID, meta.UUID)
	}
	return nil
}

func verifyGeneration(ctx context.Context, mount string, meta dbSnapshotMeta) (string, dbSnapshotMeta, error) {
	if !validSnapshotMeta(&meta) {
		return "", meta, fmt.Errorf("invalid snapshot metadata")
	}
	snapPath := snapshotGenFilePath(mount, meta)
	if err := verifySnapshotFile(ctx, snapPath, meta); err != nil {
		return "", meta, err
	}
	return snapPath, meta, nil
}

func verifySnapshotContext(ctx context.Context, mount string) (string, dbSnapshotMeta, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", dbSnapshotMeta{}, ctx.Err()
	}
	m, ok := loadSnapshotManifest(mount)
	if !ok {
		return "", dbSnapshotMeta{}, os.ErrNotExist
	}
	seen := map[string]bool{}
	var firstErr error
	for _, cand := range []*dbSnapshotMeta{m.Current, m.Previous, m.Populated} {
		if cand == nil || seen[cand.File] {
			continue
		}
		seen[cand.File] = true
		snapPath, verified, err := verifyGeneration(ctx, mount, *cand)
		if err == nil {
			return snapPath, verified, nil
		}
		if ctx != nil && ctx.Err() != nil {
			return "", dbSnapshotMeta{}, ctx.Err()
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = os.ErrNotExist
	}
	return "", dbSnapshotMeta{}, firstErr
}

func verifySnapshot(mount string) (string, dbSnapshotMeta, error) {
	return verifySnapshotContext(context.Background(), mount)
}

// readLiveUUIDIfReadable inspects a live m.db read-only without mutating it or
// replaying journals. If the file has a readable non-empty Information.uuid it
// returns (uuid, true, nil). Operational read errors and context cancellation
// are returned as errors so recovery never overwrites a database on I/O failure.
func readLiveUUIDIfReadable(ctx context.Context, path string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	_, _, magicOK, herr := readSQLiteHeader(path)
	if errors.Is(herr, os.ErrNotExist) {
		return "", false, nil
	}
	if herr != nil {
		return "", false, herr
	}
	if !magicOK {
		return "", false, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	db, err := sql.Open("sqlite", fileURI(abs)+"?mode=ro&immutable=1&_pragma=query_only(1)&_pragma=busy_timeout(1000)")
	if err != nil {
		return "", false, nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var u string
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(uuid,'') FROM Information LIMIT 1").Scan(&u); err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if isOperationalIOError(err) {
			return "", false, err
		}
		return "", false, nil
	}
	u = strings.TrimSpace(u)
	if u == "" {
		return "", false, nil
	}
	return u, true, nil
}

// missingTracksRemainingRatio samples up to logicalCollapseSample local tracks
// present in snapPath but missing from curDB, returning how many were sampled
// and how many still exist as regular files on disk.
func missingTracksRemainingRatio(ctx context.Context, mount, snapPath string, curDB *sql.DB) (sampled, onDisk int, ok bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return 0, 0, false
	}
	sdb, err := OpenContext(ctx, snapPath, true)
	if err != nil {
		return 0, 0, false
	}
	defer sdb.Close()

	curPaths := make(map[string]struct{})
	crows, err := curDB.QueryContext(ctx, `SELECT path FROM Track WHERE path IS NOT NULL AND path <> ''`)
	if err != nil {
		return 0, 0, false
	}
	for crows.Next() {
		if ctx.Err() != nil {
			crows.Close()
			return 0, 0, false
		}
		var p string
		if err := crows.Scan(&p); err == nil {
			curPaths[p] = struct{}{}
		}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return 0, 0, false
	}
	crows.Close()

	srows, err := sdb.QueryContext(ctx, `SELECT path FROM Track WHERE COALESCE(streamingSource,'')='' AND path IS NOT NULL AND path <> '' ORDER BY id`)
	if err != nil {
		return 0, 0, false
	}
	defer srows.Close()

	for srows.Next() && sampled < logicalCollapseSample {
		if ctx.Err() != nil {
			return 0, 0, false
		}
		var p string
		if err := srows.Scan(&p); err != nil {
			continue
		}
		if _, exists := curPaths[p]; exists {
			continue
		}
		full, valid := trackPath(mount, p)
		if !valid {
			continue
		}
		sampled++
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
			onDisk++
		}
	}
	if err := srows.Err(); err != nil {
		return 0, 0, false
	}
	return sampled, onDisk, true
}

// missingTracksStillOnDisk returns true only when at least 4 missing local
// tracks were sampled and >=75% of their audio files still exist on disk.
func missingTracksStillOnDisk(ctx context.Context, mount, snapPath string, curDB *sql.DB) bool {
	sampled, onDisk, ok := missingTracksRemainingRatio(ctx, mount, snapPath, curDB)
	return ok && sampled >= 4 && onDisk*4 >= sampled*3
}

func makeQuarantineDir(mount string) (string, error) {
	base := snapshotDir(mount)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	if err := syncDirFn(filepath.Dir(base)); err != nil {
		return "", fmt.Errorf("sync backup parent dir: %w", err)
	}
	stamp := snapshotNowFn().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := "corrupt-" + stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		dir := filepath.Join(base, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			if serr := syncDirFn(base); serr != nil {
				return "", fmt.Errorf("sync backup dir: %w", serr)
			}
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not create quarantine directory")
}

func copyFileChunked(ctx context.Context, srcPath, dstPath string) (string, int64, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", 0, ctx.Err()
	}
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

// quarantineDBSet durably copies dbPath and all SQLite companion files
// (-journal, -wal, -shm) into a timestamped quarantine directory and fsyncs
// the quarantine directory BEFORE unlinking any live companion or database
// file, propagating all directory sync errors.
func quarantineDBSet(ctx context.Context, mount, dbPath string) (string, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	var existing []string
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		existing = append(existing, suffix)
	}
	if len(existing) == 0 {
		return "", nil
	}
	qdir, err := makeQuarantineDir(mount)
	if err != nil {
		return "", err
	}
	baseName := filepath.Base(dbPath)
	for _, suffix := range existing {
		src := dbPath + suffix
		dst := filepath.Join(qdir, baseName+suffix)
		if _, _, err := copyFileChunked(ctx, src, dst); err != nil {
			return "", err
		}
	}
	if err := syncDirFn(qdir); err != nil {
		return "", fmt.Errorf("sync quarantine dir: %w", err)
	}
	if ctx != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	// Unlink companions before the main sidecar file so any interrupted
	// quarantine remains detectable and restartable on the next mount.
	for _, suffix := range []string{"-journal", "-wal", "-shm", ""} {
		src := dbPath + suffix
		if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove quarantined %s: %w", filepath.Base(src), err)
		}
	}
	if err := syncDirFn(filepath.Dir(dbPath)); err != nil {
		return "", fmt.Errorf("sync Database2 after quarantine: %w", err)
	}
	rel, err := filepath.Rel(mount, qdir)
	if err != nil {
		return qdir, nil
	}
	return filepath.ToSlash(rel), nil
}

// quarantineDBCopy durably copies dbPath and any SQLite journal/WAL/SHM
// companions into a timestamped quarantine directory and fsyncs the quarantine
// directory without modifying any live file.
func quarantineDBCopy(ctx context.Context, mount, dbPath string) (string, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	var existing []string
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		existing = append(existing, suffix)
	}
	if len(existing) == 0 {
		return "", nil
	}
	qdir, err := makeQuarantineDir(mount)
	if err != nil {
		return "", err
	}
	baseName := filepath.Base(dbPath)
	for _, suffix := range existing {
		src := dbPath + suffix
		dst := filepath.Join(qdir, baseName+suffix)
		if _, _, err := copyFileChunked(ctx, src, dst); err != nil {
			return "", err
		}
	}
	if err := syncDirFn(qdir); err != nil {
		return "", fmt.Errorf("sync quarantine dir: %w", err)
	}
	rel, err := filepath.Rel(mount, qdir)
	if err != nil {
		return qdir, nil
	}
	return filepath.ToSlash(rel), nil
}

func publishStagedRestore(ctx context.Context, tmp, livePath string, meta dbSnapshotMeta) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := os.Remove(livePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale m.db%s: %w", suffix, err)
		}
	}
	if err := os.Rename(tmp, livePath); err != nil {
		return fmt.Errorf("publish restored m.db: %w", err)
	}
	dbDir := filepath.Dir(livePath)
	if err := syncDirFn(dbDir); err != nil {
		return fmt.Errorf("sync Database2 after restore: %w", err)
	}
	if err := reconcileMP3GridStateAfterRestore(dbDir, meta); err != nil {
		return fmt.Errorf("reconcile mp3 grid state after restore: %w", err)
	}
	return nil
}

// finishInterruptedRestore completes a previously staged, verified
// .m.db.restore.tmp left behind by a power cut or killed process between
// journal unlink and atomic rename, or cleans up an incomplete staging file.
func finishInterruptedRestore(ctx context.Context, mount, livePath string) (dbSnapshotMeta, bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return dbSnapshotMeta{}, false, ctx.Err()
	}
	dbDir := filepath.Dir(livePath)
	tmp := filepath.Join(dbDir, ".m.db.restore.tmp")
	st, err := os.Stat(tmp)
	if errors.Is(err, os.ErrNotExist) {
		return dbSnapshotMeta{}, false, nil
	}
	if err != nil {
		return dbSnapshotMeta{}, false, err
	}
	if !st.Mode().IsRegular() {
		return dbSnapshotMeta{}, false, nil
	}
	m, hasManifest := loadSnapshotManifest(mount)
	if !hasManifest {
		_ = os.Remove(tmp)
		_ = syncDirFn(dbDir)
		return dbSnapshotMeta{}, false, nil
	}
	sum, size, err := hashFileSHA256Context(ctx, tmp)
	if err != nil {
		return dbSnapshotMeta{}, false, err
	}
	var matched *dbSnapshotMeta
	for _, cand := range []*dbSnapshotMeta{m.Current, m.Previous, m.Populated} {
		if cand != nil && cand.Size == size && cand.SHA256 == sum {
			matched = cand
			break
		}
	}
	if matched == nil || verifySnapshotFile(ctx, tmp, *matched) != nil {
		if ctx != nil && ctx.Err() != nil {
			return dbSnapshotMeta{}, false, ctx.Err()
		}
		_ = os.Remove(tmp)
		_ = syncDirFn(dbDir)
		return dbSnapshotMeta{}, false, nil
	}
	liveUUID, hasLiveUUID, uerr := readLiveUUIDIfReadable(ctx, livePath)
	if uerr != nil {
		return *matched, false, uerr
	}
	if hasLiveUUID && matched.UUID != "" && liveUUID != matched.UUID {
		_ = os.Remove(tmp)
		_ = syncDirFn(dbDir)
		return *matched, false, nil
	}
	if err := publishStagedRestore(ctx, tmp, livePath, *matched); err != nil {
		return *matched, false, err
	}
	log.Printf("dbhealer: mount %s completed interrupted restore of m.db from staged verified snapshot (%d tracks, %d playlists)",
		mount, matched.Tracks, matched.Playlists)
	return *matched, true, nil
}

// restoreMainSnapshot durably copies any damaged live m.db (+ journals) into
// quarantine, stages the verified snapshot into Database2/.m.db.restore.tmp
// with fsync + Database2 dir fsync, and ONLY THEN unlinks stale live journals
// and atomically renames the staged file over m.db.
func restoreMainSnapshot(ctx context.Context, mount, livePath, reason string) (string, dbSnapshotMeta, bool, error) {
	if ctx != nil && ctx.Err() != nil {
		return "", dbSnapshotMeta{}, false, ctx.Err()
	}
	snapPath, meta, err := verifySnapshotContext(ctx, mount)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return "", meta, false, ctx.Err()
		}
		return "", meta, false, nil
	}
	liveUUID, hasLiveUUID, uerr := readLiveUUIDIfReadable(ctx, livePath)
	if uerr != nil {
		return "", meta, false, uerr
	}
	if hasLiveUUID && meta.UUID != "" && liveUUID != meta.UUID {
		return "", meta, false, fmt.Errorf("refusing to overwrite m.db: live uuid %q differs from snapshot uuid %q", liveUUID, meta.UUID)
	}
	qrel, err := quarantineDBCopy(ctx, mount, livePath)
	if err != nil {
		return "", meta, false, fmt.Errorf("quarantine damaged m.db: %w", err)
	}
	dbDir := filepath.Dir(livePath)
	tmp := filepath.Join(dbDir, ".m.db.restore.tmp")
	_ = os.Remove(tmp)
	sum, size, err := copyFileChunked(ctx, snapPath, tmp)
	if err != nil {
		return qrel, meta, false, fmt.Errorf("stage snapshot restore: %w", err)
	}
	if size != meta.Size || sum != meta.SHA256 {
		_ = os.Remove(tmp)
		return qrel, meta, false, fmt.Errorf("staged restore digest mismatch")
	}
	if err := syncDirFn(dbDir); err != nil {
		_ = os.Remove(tmp)
		return qrel, meta, false, fmt.Errorf("sync Database2 before restore swap: %w", err)
	}
	if ctx != nil && ctx.Err() != nil {
		_ = os.Remove(tmp)
		return qrel, meta, false, ctx.Err()
	}
	if err := publishStagedRestore(ctx, tmp, livePath, meta); err != nil {
		return qrel, meta, false, err
	}
	if qrel != "" {
		log.Printf("dbhealer: mount %s restored m.db from verified snapshot (%d tracks, %d playlists) after %s; damaged copy in %s",
			mount, meta.Tracks, meta.Playlists, reason, qrel)
	} else {
		log.Printf("dbhealer: mount %s restored m.db from verified snapshot (%d tracks, %d playlists) after %s",
			mount, meta.Tracks, meta.Playlists, reason)
	}
	return qrel, meta, true, nil
}

func gcUnreferencedSnapshots(mount string, manifest dbSnapshotManifest) {
	dir := snapshotDir(mount)
	keep := map[string]bool{}
	for _, g := range []*dbSnapshotMeta{manifest.Current, manifest.Previous, manifest.Populated} {
		if g != nil && g.File != "" {
			keep[g.File] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	removed := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if name == "m.db" || strings.HasPrefix(name, "m.db.gen-") || (strings.HasPrefix(name, ".m.db.gen-") && strings.HasSuffix(name, ".tmp")) {
			if !keep[name] {
				if os.Remove(filepath.Join(dir, name)) == nil {
					removed = true
				}
			}
		}
	}
	if removed {
		_ = syncDirFn(dir)
	}
}

// PublishMountSnapshot maintains a crash-safe, multi-generation verified
// snapshot of mount's Engine Library/Database2/m.db under /.enginex-backup/db/.
// Unchanged libraries fast-skip using only the 100-byte SQLite header and
// manifest with zero bytes copied and zero SQLite opens.
func PublishMountSnapshot(ctx context.Context, mount string, r *Report) {
	livePath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	maybeSaveMainSnapshot(ctx, mount, livePath, nil, r)
}

func maybeSaveMainSnapshot(ctx context.Context, mount, livePath string, knownStats *mainDBStats, r *Report) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < snapshotMinBudget {
		return
	}
	if hasPendingJournal(livePath) {
		return
	}
	curSize, changeCounter, magicOK, err := readSQLiteHeader(livePath)
	if err != nil || !magicOK || curSize < 100 {
		return
	}
	curStamp, curCursor := currentMP3GridState(mount)
	manifest, hasManifest := loadSnapshotManifest(mount)
	if hasManifest && manifest.Current != nil {
		prev := *manifest.Current
		if prev.Size == curSize &&
			prev.ChangeCounter == changeCounter &&
			prev.MP3GridStamp == curStamp &&
			prev.MP3GridCursor == curCursor &&
			snapshotGenFilePresent(mount, prev) {
			if knownStats == nil ||
				(prev.UUID == knownStats.UUID &&
					prev.Tracks == knownStats.Tracks &&
					prev.Playlists == knownStats.Playlists &&
					prev.PlaylistEntities == knownStats.PlaylistEntities) {
				return
			}
		}
	}

	var stats mainDBStats
	var curDB *sql.DB
	closeCurDB := func() {
		if curDB != nil {
			_ = curDB.Close()
			curDB = nil
		}
	}
	defer closeCurDB()
	if knownStats != nil {
		stats = *knownStats
	} else {
		var oerr error
		curDB, oerr = OpenContext(ctx, livePath, true)
		if oerr != nil {
			return
		}
		var serr error
		stats, serr = readMainDBStats(ctx, curDB)
		if serr != nil {
			return
		}
	}
	if stats.UUID == "" {
		return
	}

	// Re-check exact fast-skip with stats in case header matched an older record
	// or counts changed without stamp changes.
	if hasManifest && manifest.Current != nil {
		prev := *manifest.Current
		if prev.UUID == stats.UUID &&
			prev.Size == curSize &&
			prev.ChangeCounter == changeCounter &&
			prev.Tracks == stats.Tracks &&
			prev.Playlists == stats.Playlists &&
			prev.PlaylistEntities == stats.PlaylistEntities &&
			prev.MP3GridStamp == curStamp &&
			prev.MP3GridCursor == curCursor &&
			snapshotGenFilePresent(mount, prev) {
			return
		}
	}

	// Retention protection: separate from automatic rollback eligibility so
	// small-library logical wipes preserve the last populated backup instead
	// of overwriting it with an empty database, while intentional Sync Manager
	// deletions (audio files removed from disk) advance current cleanly.
	if hasManifest {
		var ref *dbSnapshotMeta
		if manifest.Populated != nil && isRetentionProtectedDrop(*manifest.Populated, stats) {
			ref = manifest.Populated
		} else if manifest.Current != nil && isRetentionProtectedDrop(*manifest.Current, stats) {
			ref = manifest.Current
		}
		if ref != nil {
			if curDB == nil {
				var oerr error
				curDB, oerr = OpenContext(ctx, livePath, true)
				if oerr != nil {
					return
				}
			}
			refPath := snapshotGenFilePath(mount, *ref)
			sampled, onDisk, ok := missingTracksRemainingRatio(ctx, mount, refPath, curDB)
			if ctx.Err() != nil {
				return
			}
			audioIntact := !ok || (sampled > 0 && onDisk*4 >= sampled*3) || (sampled == 0 && stats.Tracks == 0 && stats.Playlists == 0)
			if audioIntact {
				if r != nil {
					r.Warnings = append(r.Warnings, fmt.Sprintf(
						"preserved prior m.db snapshot (%d tracks, %d playlists) instead of overwriting with emptied library (%d tracks, %d playlists)",
						ref.Tracks, ref.Playlists, stats.Tracks, stats.Playlists,
					))
				}
				return
			}
		}
	}
	closeCurDB()

	free, err := statfsFreeBytesFn(mount)
	if err == nil && free < uint64(curSize)*3+snapshotMinFreeBytes {
		return
	}
	dir := snapshotDir(mount)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	if err := syncDirFn(filepath.Dir(dir)); err != nil {
		return
	}

	nextGen := uint64(1)
	if hasManifest && manifest.NextGen > nextGen {
		nextGen = manifest.NextGen
	}
	genFile := fmt.Sprintf("m.db.gen-%d", nextGen)
	tmpDB := filepath.Join(dir, "."+genFile+".tmp")
	genPath := filepath.Join(dir, genFile)
	_ = os.Remove(tmpDB)
	sum, copied, err := copyFileChunked(ctx, livePath, tmpDB)
	if err != nil || copied != curSize {
		_ = os.Remove(tmpDB)
		return
	}
	if err := os.Rename(tmpDB, genPath); err != nil {
		_ = os.Remove(tmpDB)
		return
	}
	if err := syncDirFn(dir); err != nil {
		_ = os.Remove(genPath)
		return
	}

	newGen := dbSnapshotMeta{
		Generation:       nextGen,
		File:             genFile,
		UUID:             stats.UUID,
		Size:             curSize,
		ChangeCounter:    changeCounter,
		SHA256:           sum,
		Tracks:           stats.Tracks,
		Playlists:        stats.Playlists,
		PlaylistEntities: stats.PlaylistEntities,
		MP3GridStamp:     curStamp,
		MP3GridCursor:    curCursor,
		SavedAt:          snapshotNowFn().Format(time.RFC3339),
	}
	newManifest := dbSnapshotManifest{
		Version: 2,
		NextGen: nextGen + 1,
		Current: &newGen,
	}
	if hasManifest && manifest.Current != nil && manifest.Current.UUID == stats.UUID && manifest.Current.File != newGen.File {
		newManifest.Previous = cloneSnapshotMeta(manifest.Current)
	}
	if stats.Tracks > 0 || stats.Playlists > 0 {
		newManifest.Populated = cloneSnapshotMeta(&newGen)
	} else if hasManifest && manifest.Populated != nil && manifest.Populated.UUID == stats.UUID {
		newManifest.Populated = cloneSnapshotMeta(manifest.Populated)
	}

	raw, err := json.Marshal(newManifest)
	if err != nil {
		_ = os.Remove(genPath)
		return
	}
	tmpMeta := filepath.Join(dir, ".m.db.meta.tmp")
	f, err := os.OpenFile(tmpMeta, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		_ = os.Remove(genPath)
		return
	}
	_, werr := f.Write(raw)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmpMeta)
		_ = os.Remove(genPath)
		return
	}
	if err := os.Rename(tmpMeta, snapshotMetaPath(mount)); err != nil {
		_ = os.Remove(tmpMeta)
		_ = os.Remove(genPath)
		return
	}
	if err := syncDirFn(dir); err != nil {
		return
	}
	// Remove older unreferenced generation files ONLY AFTER the new manifest
	// is durably synced on disk.
	gcUnreferencedSnapshots(mount, newManifest)
}

// checkAndQuarantineSidecars verifies secondary Engine databases (sm.db,
// stm.db) that Engine attaches alongside m.db. A structurally corrupt sidecar
// triggers Engine's handleCorruptDB() and locks out the healthy m.db; durably
// quarantining the corrupt sidecar lets Engine recreate it cleanly on attach.
func checkAndQuarantineSidecars(ctx context.Context, mount, dbDir string, r *Report) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, name := range []string{"sm.db", "stm.db"} {
		if ctx.Err() != nil {
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
		if _, _, magicOK, herr := readSQLiteHeader(p); herr != nil {
			// Preserve operational read errors without misclassifying as corruption.
			continue
		} else if !magicOK {
			corruptReason = "invalid or truncated SQLite header"
		} else if sdb, err := OpenContext(ctx, p, false); err != nil {
			if isStructuralCorruption(err) {
				corruptReason = err.Error()
			}
		} else {
			var check string
			qerr := sdb.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
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
		qrel, err := quarantineDBSet(ctx, mount, p)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("corrupt %s could not be quarantined: %v", name, err))
			continue
		}
		r.QuarantinedDBs = append(r.QuarantinedDBs, name)
		r.Warnings = append(r.Warnings, fmt.Sprintf("quarantined corrupt %s (%s) to %s so m.db attaches cleanly", name, corruptReason, qrel))
		log.Printf("dbhealer: mount %s quarantined corrupt %s (%s) to %s", mount, name, corruptReason, qrel)
	}
}
