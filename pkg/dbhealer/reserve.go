package dbhealer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	sqlite "modernc.org/sqlite"
)

func init() {
	// Enable Linux Open File Description (OFD) locks before any SQLite connection
	// opens so closing an unrelated descriptor on a database inode never strips
	// another connection's SQLite lock in the same process.
	_, _ = sqlite.OFDLocking(true)
}

var (
	sqliteMagic            = []byte("SQLite format 3\x00")
	reserveWritable        = writable
	reserveLink            = os.Link
	reserveRename          = os.Rename
	reserveAfterHeaderRead func(string)
	reserveBeforeCopy      func(string)
	reserveBeforeRename    func(string)
	reserveAfterRename     func(string)
)

// reserveBeforeSidecarUnlink, in tests, runs between the cleanup's validation and its unlinks.
var reserveBeforeSidecarUnlink func(string)

// Reserve reports header byte 20 of a plain SQLite file; ok is false for a
// missing file or anything that is not a SQLite database.
func Reserve(path string) (reserve byte, ok bool, err error) {
	return reserveHeaderContext(context.Background(), path)
}

func reserveHeaderContext(ctx context.Context, path string) (reserve byte, ok bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return 0, false, serr
	}
	if st.IsDir() {
		return 0, false, &os.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	if !st.Mode().IsRegular() {
		return 0, false, &os.PathError{Op: "read", Path: path, Err: syscall.ESPIPE}
	}
	if err := checkReadablePath(path); err != nil {
		return 0, false, err
	}
	// Never open or close path in the parent process for header inspection: even if
	// sqlite.OFDLockingEnabled() is still true before the first lock attempt, a later
	// connection falling back to POSIX F_SETLK on EINVAL must never have its lock
	// dropped by a parent-process close().
	r, ok, err := readReserveHeaderIsolated(ctx, path)
	if err != nil {
		return 0, false, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return 0, false, cerr
	}
	if reserveAfterHeaderRead != nil {
		reserveAfterHeaderRead(path)
	}
	if cerr := ctx.Err(); cerr != nil {
		return 0, false, cerr
	}
	return r, ok, nil
}

func parseReserveHeader(h []byte) (reserve byte, ok bool) {
	if len(h) < 100 || !bytes.HasPrefix(h, sqliteMagic) {
		return 0, false
	}
	return h[20], true
}

func configureIsolatedCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 50 * time.Millisecond
}

func readReserveHeaderIsolated(ctx context.Context, path string) (reserve byte, ok bool, err error) {
	return readReserveHeaderFromPinIsolated(ctx, nil, path)
}

func readReserveHeaderFromPinIsolated(ctx context.Context, pin *os.File, path string) (reserve byte, ok bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	var cmd *exec.Cmd
	if pin != nil {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `exec < /proc/self/fd/3 && exec dd bs=100 count=1 2>/dev/null`)
		cmd.ExtraFiles = []*os.File{pin}
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `exec < "$1" && exec dd bs=100 count=1 2>/dev/null`, "_", path)
	}
	configureIsolatedCommand(cmd)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if runErr := cmd.Run(); runErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return 0, false, cerr
		}
		if pin == nil {
			if aerr := checkReadablePath(path); aerr != nil {
				return 0, false, aerr
			}
		}
		msg := strings.TrimSpace(errBuf.String())
		if strings.Contains(strings.ToLower(msg), "permission denied") {
			return 0, false, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		if msg != "" {
			return 0, false, fmt.Errorf("read reserve header %s: %w (%s)", path, runErr, msg)
		}
		return 0, false, fmt.Errorf("read reserve header %s: %w", path, runErr)
	}
	if cerr := ctx.Err(); cerr != nil {
		return 0, false, cerr
	}
	r, ok := parseReserveHeader(outBuf.Bytes())
	return r, ok, nil
}

func copyFileFromPinIsolated(ctx context.Context, pin *os.File, srcPath string, dst *os.File) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var cmd *exec.Cmd
	if pin != nil {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `exec < /proc/self/fd/3 && exec cat`)
		cmd.ExtraFiles = []*os.File{pin}
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", `exec < "$1" && exec cat`, "_", srcPath)
	}
	configureIsolatedCommand(cmd)
	var errBuf bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &errBuf
	if runErr := cmd.Run(); runErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return fmt.Errorf("copy %s: %w (%s)", srcPath, runErr, msg)
		}
		return fmt.Errorf("copy %s: %w", srcPath, runErr)
	}
	return ctx.Err()
}

// NormalizeReserve rebuilds a plain library whose per-page reserve byte is not 0.
// Engine 5.3.0's SQLCipher treats a main database with reserve > 0 as encrypted, so
// every ATTACH without a key fails with "sqlcipherCodecAttach: no key" and
// SQLITE_MISUSE (a `.recover`ed m.db had 12; stock files have 0). VACUUM cannot
// lower the reserve and editing the byte would change overflow thresholds, so the
// file is copied into a fresh database. Triggers and indexes are created after the
// rows so nothing fires. The caller must make sure Engine does not hold the file.
// The old file stays beside it as <path>.pre-reserve.
func NormalizeReserve(path string) (bool, error) {
	return NormalizeReserveContext(context.Background(), path)
}

// NormalizeReserveContext is NormalizeReserve while honoring ctx cancellation before rebuild steps and backup copies.
func NormalizeReserveContext(ctx context.Context, path string) (bool, error) {
	return normalizeReserveContext(ctx, path, nil)
}

func normalizeReserveContext(ctx context.Context, path string, validate func(context.Context, sqlQuerier) error) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r, ok, err := reserveHeaderContext(ctx, path)
	if err != nil {
		return false, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return false, cerr
	}
	if !ok || r == 0 {
		return false, nil
	}
	if err := checkNoPendingJournals(path); err != nil {
		return false, err
	}
	// The volume lock first: every bridge window and job section holds it from its last look
	// at the library through its rename and snapshot promotion, so neither side can publish
	// over what the other has just validated. Everything below is checked again under it.
	unlockVolume, err := lockLibraryVolume(ctx, path)
	if err != nil {
		return false, fmt.Errorf("lock volume of %s: %w", path, err)
	}
	defer unlockVolume()
	unlockDir, err := rbstate.LockDatabaseDir(filepath.Dir(path), 0)
	if err != nil {
		return false, fmt.Errorf("lock %s: %w", filepath.Dir(path), err)
	}
	defer unlockDir()
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	srcDB, err := sql.Open("sqlite", fileURI(path))
	if err != nil {
		return false, err
	}
	defer srcDB.Close()
	srcDB.SetMaxOpenConns(1)

	srcConn, err := srcDB.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer srcConn.Close()

	if _, err = srcConn.ExecContext(ctx, "PRAGMA main.busy_timeout=1"); err != nil {
		return false, err
	}
	if _, err = srcConn.ExecContext(ctx, "PRAGMA main.locking_mode=EXCLUSIVE"); err != nil {
		return false, err
	}
	var jmode string
	if err = srcConn.QueryRowContext(ctx, "PRAGMA main.journal_mode").Scan(&jmode); err != nil {
		return false, err
	}
	if strings.EqualFold(jmode, "wal") {
		var busy, logFrames, ckptFrames int
		if err = srcConn.QueryRowContext(ctx, "PRAGMA main.wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckptFrames); err != nil {
			return false, err
		}
		if busy != 0 {
			return false, fmt.Errorf("%s is busy (active WAL reader or writer)", path)
		}
	}
	// Hold BEGIN EXCLUSIVE from validation/copy through atomic os.Rename so no
	// concurrent BEGIN IMMEDIATE/EXCLUSIVE writer or reader can mutate or be stranded on the replaced inode.
	if _, err = srcConn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return false, err
	}
	defer func() {
		_, _ = srcConn.ExecContext(context.Background(), "ROLLBACK")
	}()

	origPin, perr := openInodePin(path)
	if perr != nil {
		return false, perr
	}
	var origInfo os.FileInfo
	if origPin != nil {
		defer origPin.Close()
		if origInfo, err = origPin.Stat(); err != nil {
			return false, err
		}
	} else if origInfo, err = os.Stat(path); err != nil {
		return false, err
	}
	stPath, serr := os.Stat(path)
	if serr != nil {
		return false, serr
	}
	if !os.SameFile(info, origInfo) || !os.SameFile(origInfo, stPath) {
		return false, fmt.Errorf("%s changed before lock acquisition", path)
	}
	r, ok, err = readReserveHeaderFromPinIsolated(ctx, origPin, path)
	if err != nil {
		return false, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return false, cerr
	}
	if !ok || r == 0 {
		return false, nil
	}
	if err := checkNoPendingJournals(path); err != nil {
		return false, err
	}
	if validate != nil {
		if err := validate(ctx, srcConn); err != nil {
			return false, err
		}
	}
	if reserveBeforeCopy != nil {
		reserveBeforeCopy(path)
	}

	tmp := path + ".reserve-rebuild"
	os.Remove(tmp)
	if err = rebuildPlainFromConnContext(ctx, srcConn, tmp); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if err = os.Chmod(tmp, origInfo.Mode().Perm()); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err = syncFile(tmp); err != nil {
		os.Remove(tmp)
		return false, err
	}
	// Never move the live name out of the way: the CLI deadline can end this
	// process between any two syscalls. Publish a durable backup first, then
	// atomically replace the still-present original while still holding BEGIN EXCLUSIVE.
	if err = backupReserveFromFileContext(ctx, path, origPin, origInfo); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if reserveBeforeRename != nil {
		reserveBeforeRename(path)
	}
	if err = ctx.Err(); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err = ensureSameLiveFile(path, origInfo); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if strings.EqualFold(jmode, "wal") {
		// Keep sqlite3WalClose from unlinking path+"-wal" or path+"-shm" when
		// srcDB.Close() runs after os.Rename publishes the new inode (where a new
		// WAL writer may already own those pathnames).
		if err = srcConn.Raw(func(dc any) error {
			fc, ok := dc.(sqlite.FileControl)
			if !ok {
				return errors.New("sqlite driver connection does not support FileControl")
			}
			_, ferr := fc.FileControlPersistWAL("main", 1)
			return ferr
		}); err != nil {
			os.Remove(tmp)
			return false, err
		}
	}
	// Verify the live inode and complete sidecar set BEFORE unlinking any sidecar,
	// so a replacement WAL writer can never lose its active -shm.
	if err = cleanupReserveSidecars(path, origInfo); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err = reserveRename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if reserveAfterRename != nil {
		reserveAfterRename(path)
	}
	return true, nil
}

// lockLibraryVolume takes the lock of the volume whose library path belongs to, the one
// rbstate.LockVolume gives the bridge: path is a file of <mount>/Engine Library/Database2.
// A ctx that holds it already (the mount's heal) is reused. A file anywhere else is no
// volume's library; no bridge publishes there and the directory lock alone applies.
func lockLibraryVolume(ctx context.Context, path string) (unlock func(), err error) {
	dir := filepath.Dir(path)
	lib := filepath.Dir(dir)
	if !strings.EqualFold(filepath.Base(dir), "Database2") || !strings.EqualFold(filepath.Base(lib), "Engine Library") {
		return func() {}, nil
	}
	_, unlock, err = rbstate.HoldVolume(ctx, filepath.Dir(lib), 0)
	return unlock, err
}

func ensureSameLiveFile(path string, origInfo os.FileInfo) error {
	cur, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(origInfo, cur) {
		return fmt.Errorf("%s was replaced during reserve rebuild; aborting publication", path)
	}
	return nil
}

func checkSidecarRegular(path, suffix string, checkNonEmpty bool) error {
	_, err := statSidecar(path, suffix, checkNonEmpty)
	return err
}

// statSidecar is checkSidecarRegular that also returns the entry it checked, nil when there is none.
func statSidecar(path, suffix string, checkNonEmpty bool) (os.FileInfo, error) {
	lst, err := os.Lstat(path + suffix)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !lst.Mode().IsRegular() {
		return nil, fmt.Errorf("%s%s is not a regular file (%s)", path, suffix, lst.Mode())
	}
	if checkNonEmpty && lst.Size() > 0 {
		return nil, fmt.Errorf("%s has pending %s; let Engine recover it first", path, suffix)
	}
	return lst, nil
}

func checkNoPendingJournals(path string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		if err := checkSidecarRegular(path, suffix, true); err != nil {
			return err
		}
	}
	return checkSidecarRegular(path, "-shm", false)
}

// cleanupReserveSidecars removes the original library's stale -shm and empty -wal before the
// rename. The caller holds the volume lock. It unlinks only an entry it validated: a name
// that was absent then, or that names another file or holds work now, belongs to whoever
// made it (the writer of a library put in place meanwhile) and stays where it is.
func cleanupReserveSidecars(path string, origInfo os.FileInfo) error {
	// Phase 1: read-only validation of live inode ownership and all sidecars before unlinking anything.
	if err := ensureSameLiveFile(path, origInfo); err != nil {
		return err
	}
	type sidecar struct {
		suffix string
		info   os.FileInfo
	}
	var seen []sidecar
	wal, err := statSidecar(path, "-wal", true)
	if err != nil {
		return fmt.Errorf("%s-wal: %w", path, err)
	}
	if err := checkSidecarRegular(path, "-journal", true); err != nil {
		return fmt.Errorf("%s-journal: %w", path, err)
	}
	shm, err := statSidecar(path, "-shm", false)
	if err != nil {
		return fmt.Errorf("%s-shm: %w", path, err)
	}
	if shm != nil {
		seen = append(seen, sidecar{"-shm", shm})
	}
	if wal != nil {
		seen = append(seen, sidecar{"-wal", wal})
	}
	if reserveBeforeSidecarUnlink != nil {
		reserveBeforeSidecarUnlink(path)
	}
	// Phase 2: remove only the validated stale -shm / 0-byte -wal, then re-verify ownership.
	for _, s := range seen {
		if err := ensureSameLiveFile(path, origInfo); err != nil {
			return err
		}
		cur, err := statSidecar(path, s.suffix, s.suffix == "-wal")
		if err != nil {
			return fmt.Errorf("%s%s: %w", path, s.suffix, err)
		}
		if cur == nil {
			continue
		}
		if !os.SameFile(s.info, cur) {
			return fmt.Errorf("%s%s was replaced during reserve rebuild; aborting publication", path, s.suffix)
		}
		if remErr := os.Remove(path + s.suffix); remErr != nil && !errors.Is(remErr, os.ErrNotExist) {
			return fmt.Errorf("%s%s: %w", path, s.suffix, remErr)
		}
	}
	return ensureSameLiveFile(path, origInfo)
}

func backupReserve(path string) error {
	return backupReserveContext(context.Background(), path)
}

func backupReserveContext(ctx context.Context, path string) error {
	pin, err := openInodePin(path)
	if err != nil {
		return err
	}
	var info os.FileInfo
	if pin != nil {
		defer pin.Close()
		info, err = pin.Stat()
	} else {
		info, err = os.Stat(path)
	}
	if err != nil {
		return err
	}
	return backupReserveFromFileContext(ctx, path, pin, info)
}

func backupReserveFromFileContext(ctx context.Context, path string, origPin *os.File, info os.FileInfo) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp := path + ".pre-reserve.tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defer os.Remove(tmp)
	// Internal ext4 needs no second full copy. Removable filesystems may not
	// support hard links, so copy from origPin via an isolated child process that
	// never closes a normal fd in the parent process under POSIX locking.
	linkErr := reserveLink(path, tmp)
	if linkErr == nil {
		tmpInfo, lerr := os.Lstat(tmp)
		if lerr != nil || !os.SameFile(info, tmpInfo) {
			_ = os.Remove(tmp)
			if lerr != nil {
				linkErr = lerr
			} else {
				linkErr = fmt.Errorf("%s replaced before backup hard link", path)
			}
		}
	}
	if linkErr != nil {
		dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		err = copyFileFromPinIsolated(ctx, origPin, path, dst)
		if err == nil {
			err = dst.Sync()
		}
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path+".pre-reserve"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// NormalizeDatabase2 fixes every Engine library file in dir (Database2); names
// starting with "." belong to other tools.
func NormalizeDatabase2(dir string) (fixed []string, err error) {
	return NormalizeDatabase2Context(context.Background(), dir)
}

// NormalizeDatabase2Context fixes every writable Engine library file in dir while
// honoring ctx cancellation and propagating writability and schema check errors.
func NormalizeDatabase2Context(ctx context.Context, dir string) (fixed []string, err error) {
	fixed, _, err = normalizeDatabase2(ctx, ctx, dir, nil, false)
	return fixed, err
}

// NormalizeMountReserve fixes every Engine library file in mount/Engine Library/Database2
// and refuses to modify sibling files when m.db is missing or invalid.
func NormalizeMountReserve(mount string) (fixed []string, err error) {
	return NormalizeMountReserveContext(context.Background(), mount)
}

// NormalizeMountReserveContext is NormalizeMountReserve while honoring ctx cancellation.
func NormalizeMountReserveContext(ctx context.Context, mount string) (fixed []string, err error) {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	fixed, _, err = normalizeDatabase2(ctx, ctx, dir, nil, true)
	return fixed, err
}

func normalizeMountDatabase2(ctx, probeCtx context.Context, dir string, cleanPaths map[string]bool) (fixed []string, incompleteOrPending bool, err error) {
	return normalizeDatabase2(ctx, probeCtx, dir, cleanPaths, true)
}

func validateMainDBForReserve(ctx context.Context, mPath string) (hasHeader bool, err error) {
	_, ok, rerr := reserveHeaderContext(ctx, mPath)
	if rerr != nil {
		return false, fmt.Errorf("main library %s: %w", mPath, rerr)
	}
	if !ok {
		return false, fmt.Errorf("main library %s: not a valid SQLite database", mPath)
	}
	roDB, roErr := openReadOnlyLocking(ctx, mPath, probeBusyTimeoutMS(ctx))
	if roErr != nil {
		return true, roErr
	}
	verr := validateMainLibrarySchemaForReserve(ctx, roDB)
	cerr := roDB.Close()
	if verr != nil {
		return true, verr
	}
	if cerr != nil {
		return true, cerr
	}
	return true, nil
}

func validateMainLibrarySchemaForReserve(ctx context.Context, q sqlQuerier) error {
	infos, err := readLibraryIdentityRows(ctx, q)
	if err != nil {
		return err
	}
	if _, err := validateLibraryIdentityRows(infos); err != nil {
		return err
	}
	return hasPerformanceDataTable(ctx, q)
}

func anySiblingReserveNonZero(ctx context.Context, dir string, skip map[string]bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return true, err
	}
	for _, p := range names {
		if strings.HasPrefix(filepath.Base(p), ".") || skip[p] {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return true, cerr
		}
		w, we := reserveWritable(p)
		if we != nil {
			return true, we
		}
		if !w {
			continue
		}
		res, ok, rerr := reserveHeaderContext(ctx, p)
		if rerr != nil {
			return true, rerr
		}
		if ok && res != 0 {
			return true, nil
		}
	}
	return false, nil
}

func normalizeDatabase2(ctx, probeCtx context.Context, dir string, cleanPaths map[string]bool, requireMain bool) (fixed []string, incompleteOrPending bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if probeCtx == nil {
		probeCtx = ctx
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	// Validate m.db before touching any sibling database even if m.db has reserve 0
	// or follows another file alphabetically.
	mPath := filepath.Join(dir, "m.db")
	var mainErr error
	if !cleanPaths[mPath] {
		var hasHeader bool
		hasHeader, mainErr = validateMainDBForReserve(ctx, mPath)
		if mainErr != nil && (hasHeader || requireMain) {
			return nil, true, mainErr
		}
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, true, err
	}
	ordered := make([]string, 0, len(names))
	for _, p := range names {
		if strings.EqualFold(filepath.Base(p), "m.db") {
			ordered = append(ordered, p)
		}
	}
	for _, p := range names {
		if !strings.EqualFold(filepath.Base(p), "m.db") {
			ordered = append(ordered, p)
		}
	}
	for _, p := range ordered {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") || cleanPaths[p] {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fixed, true, errors.Join(err, ctxErr)
		}
		w, we := reserveWritable(p)
		if we != nil {
			incompleteOrPending = true
			err = errors.Join(err, we)
			continue
		}
		if !w {
			continue
		}
		res, ok, rerr := reserveHeaderContext(probeCtx, p)
		if rerr != nil {
			incompleteOrPending = true
			err = errors.Join(err, rerr)
			if probeCtx.Err() != nil || ctx.Err() != nil {
				return fixed, true, err
			}
			continue
		}
		if !ok || res == 0 {
			continue
		}
		if !strings.EqualFold(base, "m.db") && !cleanPaths[mPath] && mainErr != nil {
			incompleteOrPending = true
			err = errors.Join(err, mainErr)
			continue
		}
		var validate func(context.Context, sqlQuerier) error
		switch {
		case strings.EqualFold(base, "hm.db"):
			validate = validateHistorySchema
		case strings.EqualFold(base, "m.db"):
			validate = validateMainLibrarySchemaForReserve
		}
		rebuilt, e := normalizeReserveContext(ctx, p, validate)
		if e != nil {
			incompleteOrPending = true
			err = errors.Join(err, e)
		} else if rebuilt {
			fixed = append(fixed, base)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}
	return fixed, incompleteOrPending, err
}

func onlyContextError(err error) bool {
	if err == nil {
		return false
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		subs := multi.Unwrap()
		nonNil := 0
		for _, sub := range subs {
			if sub == nil {
				continue
			}
			nonNil++
			if !onlyContextError(sub) {
				return false
			}
		}
		return nonNil > 0
	}
	if inner := errors.Unwrap(err); inner != nil {
		return onlyContextError(inner)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// fileURI keeps '#', '?' and '%' in a stick label part of the path for SQLite.
func fileURI(p string) string { return "file:" + (&url.URL{Path: filepath.ToSlash(p)}).EscapedPath() }

func rebuildPlainFromConnContext(ctx context.Context, srcConn *sql.Conn, dst string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Properties that must survive; encoding, page_size and auto_vacuum have to be set first.
	var encoding string
	if err := srcConn.QueryRowContext(ctx, "PRAGMA main.encoding").Scan(&encoding); err != nil {
		return err
	}
	var encodingStmt string
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8":
		encodingStmt = `PRAGMA main.encoding="UTF-8"`
	case "utf-16le":
		encodingStmt = `PRAGMA main.encoding="UTF-16le"`
	case "utf-16be":
		encodingStmt = `PRAGMA main.encoding="UTF-16be"`
	default:
		return fmt.Errorf("unsupported SQLite encoding %q", encoding)
	}
	var pageSize, autoVacuum, userVersion, appID int64
	for pragma, v := range map[string]*int64{"page_size": &pageSize, "auto_vacuum": &autoVacuum, "user_version": &userVersion, "application_id": &appID} {
		if err := srcConn.QueryRowContext(ctx, "PRAGMA main."+pragma).Scan(v); err != nil {
			return err
		}
	}
	type object struct{ typ, name, sql string }
	var objects []object
	rows, err := srcConn.QueryContext(ctx, "SELECT type,name,sql FROM main.sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var o object
		if err = rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			_ = rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(objects) == 0 {
		return errors.New("source has no schema; refusing to replace it with an empty database")
	}

	db, err := sql.Open("sqlite", fileURI(dst))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		return err
	}
	for _, s := range []string{encodingStmt, fmt.Sprintf("PRAGMA main.page_size=%d", pageSize), fmt.Sprintf("PRAGMA main.auto_vacuum=%d", autoVacuum)} {
		if _, err = db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables []object
	for _, o := range objects {
		if o.typ == "table" {
			if _, err = tx.ExecContext(ctx, o.sql); err != nil {
				return fmt.Errorf("create %s: %w", o.name, err)
			}
			tables = append(tables, o)
		}
	}
	for _, t := range tables {
		spec, e := inspectTableCopySpec(ctx, srcConn, t.name)
		if e != nil {
			return e
		}
		if err = copyTableRowsFromConn(ctx, srcConn, tx, t.name, spec); err != nil {
			return fmt.Errorf("copy %s: %w", t.name, err)
		}
	}
	var hasSeq int
	if err = srcConn.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_master WHERE name='sqlite_sequence'").Scan(&hasSeq); err != nil {
		return err
	}
	if hasSeq > 0 {
		// SQLite makes sqlite_sequence with the first AUTOINCREMENT table and keeps it
		// when the last one is dropped: the old file can have it and the new one not.
		// Its rows then name no table, so there is nothing to carry over (BH2-A1-5).
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_master WHERE name='sqlite_sequence'").Scan(&hasSeq); err != nil {
			return err
		}
	}
	if hasSeq > 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM main.sqlite_sequence"); err != nil {
			return err
		}
		seqRows, err := srcConn.QueryContext(ctx, "SELECT name,seq FROM main.sqlite_sequence")
		if err != nil {
			return err
		}
		for seqRows.Next() {
			var seqName string
			var seqVal int64
			if err = seqRows.Scan(&seqName, &seqVal); err != nil {
				_ = seqRows.Close()
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO main.sqlite_sequence(name,seq) VALUES(?,?)", seqName, seqVal); err != nil {
				_ = seqRows.Close()
				return err
			}
		}
		if err = seqRows.Err(); err != nil {
			_ = seqRows.Close()
			return err
		}
		if err = seqRows.Close(); err != nil {
			return err
		}
	}
	for _, typ := range []string{"index", "view", "trigger"} {
		for _, o := range objects {
			if o.typ == typ {
				if _, err = tx.ExecContext(ctx, o.sql); err != nil {
					return fmt.Errorf("create %s %s: %w", typ, o.name, err)
				}
			}
		}
	}
	for _, t := range tables {
		var a, b int64
		if err = srcConn.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM main.%s", quote(t.name))).Scan(&a); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM main.%s", quote(t.name))).Scan(&b); err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("copy %s: %d rows became %d", t.name, a, b)
		}
	}
	var made int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'").Scan(&made); err != nil {
		return err
	}
	if made != len(objects) {
		return fmt.Errorf("schema has %d objects, rebuilt %d", len(objects), made)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, s := range []string{fmt.Sprintf("PRAGMA main.user_version=%d", userVersion), fmt.Sprintf("PRAGMA main.application_id=%d", appID)} {
		if _, err = db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA main.integrity_check").Scan(&check); err != nil {
		return fmt.Errorf("rebuilt database integrity: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("rebuilt database integrity: %s", check)
	}
	if err = db.Close(); err != nil {
		return err
	}
	r, ok, e := reserveHeaderContext(ctx, dst)
	if e != nil {
		return fmt.Errorf("verify rebuilt database reserve: %w", e)
	}
	if !ok || r != 0 {
		return fmt.Errorf("rebuilt database still has reserve %d (ok=%v)", r, ok)
	}
	return nil
}

type tableCopySpec struct {
	cols              []string
	explicitRowidExpr string
}

func normalizeScannedValue(val any) any {
	b, ok := val.([]byte)
	if !ok {
		return val
	}
	if b == nil {
		return []byte{}
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

func asciiLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func copyTableRowsFromConn(ctx context.Context, srcConn *sql.Conn, dstTx *sql.Tx, table string, spec tableCopySpec) error {
	insertCols := make([]string, 0, len(spec.cols)+1)
	if spec.explicitRowidExpr != "" {
		// Keep rowids when not already aliased by INTEGER PRIMARY KEY; other tables and Engine refer to them.
		insertCols = append(insertCols, spec.explicitRowidExpr)
	}
	insertCols = append(insertCols, spec.cols...)

	var tailStmt *sql.Stmt
	selectCols := spec.cols
	if spec.explicitRowidExpr != "" && len(spec.cols) >= 2000 {
		// SQLite caps SELECT result width at SQLITE_MAX_COLUMN (2000) while INSERT
		// allows an explicit rowid target plus all 2000 columns; stream the tail
		// column per rowid so large payloads never buffer the whole column in RAM.
		var err error
		tailStmt, err = srcConn.PrepareContext(ctx, fmt.Sprintf(
			"SELECT CASE WHEN 1 THEN %s END FROM main.%s WHERE %s = ?",
			spec.cols[len(spec.cols)-1], quote(table), spec.explicitRowidExpr,
		))
		if err != nil {
			return err
		}
		defer tailStmt.Close()
		selectCols = spec.cols[:len(spec.cols)-1]
	}

	selectExprs := make([]string, 0, len(selectCols)+1)
	if spec.explicitRowidExpr != "" {
		selectExprs = append(selectExprs, spec.explicitRowidExpr)
	}
	for _, col := range selectCols {
		// Wrap each column in a trivial CASE expression so sqlite3_column_decltype
		// is empty (stopping modernc.org/sqlite from parsing DATETIME columns into
		// time.Time) while still keeping one projection column per table column and
		// letting SQLite transcode UTF-8/UTF-16 text cleanly via sqlite3_column_text.
		selectExprs = append(selectExprs, "CASE WHEN 1 THEN "+col+" END")
	}

	rows, err := srcConn.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM main.%s", strings.Join(selectExprs, ","), quote(table)))
	if err != nil {
		return err
	}
	defer rows.Close()
	numInsert := len(insertCols)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", numInsert), ",")
	insStmt, err := dstTx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO main.%s(%s) VALUES(%s)", quote(table), strings.Join(insertCols, ","), placeholders))
	if err != nil {
		return err
	}
	defer insStmt.Close()

	numSelect := len(selectExprs)
	rawVals := make([]any, numSelect)
	scanDest := make([]any, numSelect)
	for i := range rawVals {
		scanDest[i] = &rawVals[i]
	}
	bindVals := make([]any, numInsert)
	for rowIdx := 0; rows.Next(); rowIdx++ {
		if rowIdx&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := rows.Scan(scanDest...); err != nil {
			return err
		}
		for i, val := range rawVals {
			bindVals[i] = normalizeScannedValue(val)
		}
		if tailStmt != nil {
			rid, ok := rawVals[0].(int64)
			if !ok {
				return fmt.Errorf("unexpected rowid type %T", rawVals[0])
			}
			var tailVal any
			if err := tailStmt.QueryRowContext(ctx, rid).Scan(&tailVal); err != nil {
				return fmt.Errorf("read tail column for rowid %d: %w", rid, err)
			}
			bindVals[numSelect] = normalizeScannedValue(tailVal)
		}
		if _, err := insStmt.ExecContext(ctx, bindVals...); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if tailStmt != nil {
		if err := tailStmt.Close(); err != nil {
			return err
		}
	}
	return insStmt.Close()
}

func inspectTableCopySpec(ctx context.Context, srcConn *sql.Conn, table string) (tableCopySpec, error) {
	// Use PRAGMA table_xinfo so generated/hidden columns (hidden != 0) still
	// register as user-defined identifiers when choosing an unshadowed physical
	// rowid alias, while keeping only writable ordinary columns in spec.cols
	// even if a user table is named pragma_table_xinfo.
	rows, err := srcConn.QueryContext(ctx, fmt.Sprintf("PRAGMA main.table_xinfo(%s)", quote(table)))
	if err != nil {
		return tableCopySpec{}, err
	}
	defer rows.Close()
	var cols []string
	userCols := make(map[string]bool)
	var pkCount int
	var pkType string
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var c, colType string
		var dflt any
		if err = rows.Scan(&cid, &c, &colType, &notnull, &dflt, &pk, &hidden); err != nil {
			return tableCopySpec{}, err
		}
		userCols[asciiLower(c)] = true
		if hidden != 0 {
			continue
		}
		cols = append(cols, quote(c))
		if pk > 0 {
			pkCount++
			pkType = colType
		}
	}
	if err := rows.Err(); err != nil {
		return tableCopySpec{}, err
	}
	if err := rows.Close(); err != nil {
		return tableCopySpec{}, err
	}

	idxRows, err := srcConn.QueryContext(ctx, fmt.Sprintf("PRAGMA main.index_list(%s)", quote(table)))
	if err != nil {
		return tableCopySpec{}, err
	}
	defer idxRows.Close()
	hasPKIndex := false
	for idxRows.Next() {
		var seq int
		var name, origin string
		var uniq, partial int
		if err = idxRows.Scan(&seq, &name, &uniq, &origin, &partial); err != nil {
			return tableCopySpec{}, err
		}
		if origin == "pk" {
			hasPKIndex = true
		}
	}
	if err := idxRows.Err(); err != nil {
		return tableCopySpec{}, err
	}
	if err := idxRows.Close(); err != nil {
		return tableCopySpec{}, err
	}

	var rowidExpr string
	for _, cand := range []string{"rowid", "_rowid_", "oid"} {
		if userCols[cand] {
			continue
		}
		probeRows, qerr := srcConn.QueryContext(ctx, fmt.Sprintf("SELECT main.%s.%s FROM main.%s LIMIT 0", quote(table), cand, quote(table)))
		if qerr == nil {
			_ = probeRows.Close()
			rowidExpr = cand
			break
		}
	}

	isIntPKAlias := pkCount == 1 &&
		asciiLower(strings.TrimSpace(pkType)) == "integer" &&
		!hasPKIndex
	spec := tableCopySpec{cols: cols}
	if !isIntPKAlias && rowidExpr != "" {
		spec.explicitRowidExpr = rowidExpr
	}
	return spec, nil
}
