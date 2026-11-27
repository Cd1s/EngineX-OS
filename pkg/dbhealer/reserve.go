package dbhealer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	sqlite "modernc.org/sqlite"
)

func init() {
	// Enable Linux Open File Description (OFD) locks before any SQLite connection
	// opens so closing an unrelated descriptor on a database inode never strips
	// another connection's SQLite lock in the same process.
	_, _ = sqlite.OFDLocking(true)
}

var (
	sqliteMagic         = []byte("SQLite format 3\x00")
	reserveWritable     = writable
	reserveLink         = os.Link
	reserveRename       = os.Rename
	reserveBeforeCopy   func(string)
	reserveBeforeRename func(string)
	reserveAfterRename  func(string)

	reserveFilesMu   sync.Mutex
	reserveFilesCond = sync.NewCond(&reserveFilesMu)
	reserveFiles     = map[fileDevIno]*activeReserveEntry{}
)

type activeReserveEntry struct {
	f       *os.File
	refs    int
	closing bool
}

func statDevIno(info os.FileInfo) (fileDevIno, bool) {
	if info == nil {
		return fileDevIno{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileDevIno{}, false
	}
	return fileDevIno{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

func pinActiveReserveFile(key fileDevIno) (*os.File, func()) {
	reserveFilesMu.Lock()
	defer reserveFilesMu.Unlock()
	e := reserveFiles[key]
	if e == nil || e.closing {
		return nil, nil
	}
	e.refs++
	return e.f, func() {
		reserveFilesMu.Lock()
		e.refs--
		if e.refs == 0 {
			reserveFilesCond.Broadcast()
		}
		reserveFilesMu.Unlock()
	}
}

func registerActiveReserveFile(key fileDevIno, f *os.File) func() {
	reserveFilesMu.Lock()
	for reserveFiles[key] != nil {
		reserveFilesCond.Wait()
	}
	e := &activeReserveEntry{f: f}
	reserveFiles[key] = e
	reserveFilesMu.Unlock()
	return func() {
		reserveFilesMu.Lock()
		e.closing = true
		delete(reserveFiles, key)
		for e.refs > 0 {
			reserveFilesCond.Wait()
		}
		reserveFilesCond.Broadcast()
		reserveFilesMu.Unlock()
		_ = f.Close()
	}
}

// Reserve reports header byte 20 of a plain SQLite file; ok is false for a
// missing file or anything that is not a SQLite database.
func Reserve(path string) (reserve byte, ok bool, err error) {
	st, serr := os.Stat(path)
	if serr != nil {
		return 0, false, serr
	}
	if st.IsDir() {
		return 0, false, &os.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	if key, okKey := statDevIno(st); okKey {
		if f, release := pinActiveReserveFile(key); f != nil {
			defer release()
			return readReserveFromFile(f)
		}
	}
	if !sqlite.OFDLockingEnabled() {
		// Under POSIX F_SETLK fallback, opening and closing path inside this process
		// would drop any same-process SQLite locks on the inode; inspect in a child PID.
		return readReserveHeaderIsolated(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	return readReserveFromFile(f)
}

func parseReserveHeader(h []byte) (reserve byte, ok bool) {
	if len(h) < 100 || !bytes.HasPrefix(h, sqliteMagic) {
		return 0, false
	}
	return h[20], true
}

func readReserveFromFile(f *os.File) (reserve byte, ok bool, err error) {
	h := make([]byte, 100)
	if n, rerr := f.ReadAt(h, 0); n < 100 {
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return 0, false, rerr
		}
		return 0, false, nil
	}
	r, ok := parseReserveHeader(h)
	return r, ok, nil
}

func readReserveHeaderIsolated(path string) (reserve byte, ok bool, err error) {
	cmd := exec.Command("/bin/sh", "-c", `exec < "$1" && exec dd bs=100 count=1 2>/dev/null`, "_", path)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return 0, false, fmt.Errorf("read reserve header %s: %w (%s)", path, runErr, msg)
		}
		return 0, false, fmt.Errorf("read reserve header %s: %w", path, runErr)
	}
	r, ok := parseReserveHeader(outBuf.Bytes())
	return r, ok, nil
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
	if r, ok, err := Reserve(path); err != nil || !ok || r == 0 {
		return false, err
	}
	if err := checkNoPendingJournals(path); err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}

	// Close origFile last (after ROLLBACK and srcDB.Close) so an extra descriptor
	// close on path cannot strip active SQLite locks under POSIX fcntl semantics.
	var cleanupOrigFile func()
	defer func() {
		if cleanupOrigFile != nil {
			cleanupOrigFile()
		}
	}()

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
	inTx := true
	defer func() {
		if inTx {
			_, _ = srcConn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	origFile, err := os.Open(path)
	if err != nil {
		return false, err
	}
	origInfo, err := origFile.Stat()
	if err != nil {
		cleanupOrigFile = func() { _ = origFile.Close() }
		return false, err
	}
	if key, okKey := statDevIno(origInfo); okKey {
		cleanupOrigFile = registerActiveReserveFile(key, origFile)
	} else {
		cleanupOrigFile = func() { _ = origFile.Close() }
	}

	stPath, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, origInfo) || !os.SameFile(origInfo, stPath) {
		return false, fmt.Errorf("%s changed before lock acquisition", path)
	}
	if r, ok, err := readReserveFromFile(origFile); err != nil || !ok || r == 0 {
		return false, err
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
	if err = backupReserveFromFileContext(ctx, path, origFile, origInfo); err != nil {
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
		// Clean up any stale pre-existing -shm / 0-byte -wal (e.g. from the read-only
		// preflight) BEFORE os.Rename while still holding BEGIN EXCLUSIVE on path,
		// without mutating path's header bytes or hardlinked .pre-reserve backup.
		_ = os.Remove(path + "-shm")
		if st, serr := os.Stat(path + "-wal"); serr == nil && st.Size() == 0 {
			_ = os.Remove(path + "-wal")
		}
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

func checkNoPendingJournals(path string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		if s, e := os.Stat(path + suffix); e == nil && s.Size() > 0 {
			return fmt.Errorf("%s has pending %s; let Engine recover it first", path, suffix)
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return nil
}

func backupReserve(path string) error {
	return backupReserveContext(context.Background(), path)
}

func backupReserveContext(ctx context.Context, path string) error {
	origFile, err := os.Open(path)
	if err != nil {
		return err
	}
	defer origFile.Close()
	info, err := origFile.Stat()
	if err != nil {
		return err
	}
	return backupReserveFromFileContext(ctx, path, origFile, info)
}

func backupReserveFromFileContext(ctx context.Context, path string, origFile *os.File, info os.FileInfo) error {
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
	// support hard links, so copy via the already-open origFile without ever
	// calling open/close on path (which would release fcntl locks on Linux).
	if err := reserveLink(path, tmp); err != nil {
		dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		src := io.NewSectionReader(origFile, 0, info.Size())
		buf := make([]byte, 64<<10)
		for {
			if err = ctx.Err(); err != nil {
				break
			}
			var n int
			n, err = src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					err = werr
					break
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				break
			}
		}
		if err == nil {
			err = ctx.Err()
		}
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
	fixed, _, err = normalizeDatabase2(ctx, dir, nil, false)
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
	fixed, _, err = normalizeDatabase2(ctx, dir, nil, true)
	return fixed, err
}

func normalizeMountDatabase2(ctx context.Context, dir string, cleanPaths map[string]bool) (fixed []string, incompleteOrPending bool, err error) {
	return normalizeDatabase2(ctx, dir, cleanPaths, true)
}

func validateMainDBForReserve(ctx context.Context, mPath string) (hasHeader bool, err error) {
	_, ok, rerr := Reserve(mPath)
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

func anySiblingReserveNonZero(dir string, skip map[string]bool) (bool, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return true, err
	}
	for _, p := range names {
		if strings.HasPrefix(filepath.Base(p), ".") || skip[p] {
			continue
		}
		w, we := reserveWritable(p)
		if we != nil {
			return true, we
		}
		if !w {
			continue
		}
		res, ok, rerr := Reserve(p)
		if rerr != nil {
			return true, rerr
		}
		if ok && res != 0 {
			return true, nil
		}
	}
	return false, nil
}

func normalizeDatabase2(ctx context.Context, dir string, cleanPaths map[string]bool, requireMain bool) (fixed []string, incompleteOrPending bool, err error) {
	if ctx == nil {
		ctx = context.Background()
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
		res, ok, rerr := Reserve(p)
		if rerr != nil {
			incompleteOrPending = true
			err = errors.Join(err, rerr)
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
		cols, hasRowIDAlias, e := tableColumnsContext(ctx, srcConn, t.name, t.sql)
		if e != nil {
			return e
		}
		normSQL := strings.Join(strings.Fields(strings.ToUpper(t.sql)), " ")
		needExplicitRowID := !strings.Contains(normSQL, "WITHOUT ROWID") && !hasRowIDAlias
		if err = copyTableRowsFromConn(ctx, srcConn, tx, t.name, cols, needExplicitRowID); err != nil {
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
	if err = db.QueryRowContext(ctx, "PRAGMA main.integrity_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("rebuilt database integrity: %v %w", check, err)
	}
	if err = db.Close(); err != nil {
		return err
	}
	if r, _, e := Reserve(dst); e != nil || r != 0 {
		return fmt.Errorf("rebuilt database still has reserve %d: %v", r, e)
	}
	return nil
}

func copyTableRowsFromConn(ctx context.Context, srcConn *sql.Conn, dstTx *sql.Tx, table string, cols []string, hasRowID bool) error {
	insertCols := make([]string, 0, len(cols)+1)
	selectExprs := make([]string, 0, len(cols)+1)
	if hasRowID {
		// Keep rowids: other tables and Engine refer to them.
		insertCols = append(insertCols, "rowid")
		selectExprs = append(selectExprs, "rowid")
	}
	for _, col := range cols {
		insertCols = append(insertCols, col)
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

	rawVals := make([]any, numInsert)
	scanDest := make([]any, numInsert)
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
			if b, ok := val.([]byte); ok {
				if b == nil {
					bindVals[i] = []byte{}
				} else {
					cp := make([]byte, len(b))
					copy(cp, b)
					bindVals[i] = cp
				}
			} else {
				bindVals[i] = val
			}
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
	return insStmt.Close()
}

func tableColumnsContext(ctx context.Context, q sqlQuerier, table, createSQL string) ([]string, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, type, pk FROM pragma_table_info(?, 'main') ORDER BY cid", table)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var cols []string
	var pkCount int
	var pkType string
	hasLiteralRowID := false
	for rows.Next() {
		var c, colType string
		var pk int
		if err = rows.Scan(&c, &colType, &pk); err != nil {
			return nil, false, err
		}
		cols = append(cols, quote(c))
		if strings.EqualFold(c, "rowid") {
			hasLiteralRowID = true
		}
		if pk > 0 {
			pkCount++
			pkType = colType
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	normSQL := strings.Join(strings.Fields(strings.ToUpper(createSQL)), " ")
	isIntPKAlias := pkCount == 1 &&
		strings.EqualFold(strings.TrimSpace(pkType), "INTEGER") &&
		!strings.Contains(normSQL, "WITHOUT ROWID") &&
		!strings.Contains(normSQL, "PRIMARY KEY DESC")
	return cols, isIntPKAlias || hasLiteralRowID, nil
}
