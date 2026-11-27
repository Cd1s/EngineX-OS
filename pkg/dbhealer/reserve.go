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
	"path/filepath"
	"strings"
)

var (
	sqliteMagic     = []byte("SQLite format 3\x00")
	reserveWritable = writable
)

// Reserve reports header byte 20 of a plain SQLite file; ok is false for a
// missing file or anything that is not a SQLite database.
func Reserve(path string) (reserve byte, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	h := make([]byte, 100)
	if n, _ := f.Read(h); n < 100 || !bytes.HasPrefix(h, sqliteMagic) {
		return 0, false, nil
	}
	return h[20], true, nil
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
	if r, ok, err := Reserve(path); err != nil || !ok || r == 0 {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := checkNoPendingJournals(path); err != nil {
		return false, err
	}
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

	if _, err = srcConn.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		return false, err
	}
	// Hold BEGIN EXCLUSIVE from validation/copy through atomic os.Rename so no
	// concurrent BEGIN IMMEDIATE/EXCLUSIVE writer or reader can mutate or be stranded on the replaced inode.
	if _, err = srcConn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return false, err
	}
	defer func() {
		_, _ = srcConn.ExecContext(context.Background(), "ROLLBACK")
	}()

	if r, ok, err := Reserve(path); err != nil || !ok || r == 0 {
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

	tmp := path + ".reserve-rebuild"
	os.Remove(tmp)
	if err = rebuildPlainFromConnContext(ctx, srcConn, tmp); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if err = os.Chmod(tmp, info.Mode().Perm()); err != nil {
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
	if err = backupReserveContext(ctx, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return false, err
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
	// support hard links, so copy without ever renaming the source there.
	if err := os.Link(path, tmp); err != nil {
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		info, err := src.Stat()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
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
	fixed, _, err = normalizeDatabase2(ctx, dir, nil)
	return fixed, err
}

func normalizeMountDatabase2(ctx context.Context, dir string, cleanPaths map[string]bool) (fixed []string, incompleteOrPending bool, err error) {
	return normalizeDatabase2(ctx, dir, cleanPaths)
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

func normalizeDatabase2(ctx context.Context, dir string, cleanPaths map[string]bool) (fixed []string, incompleteOrPending bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, true, err
	}
	for _, p := range names {
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
	// Properties that must survive; page_size and auto_vacuum have to be set first.
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
	for _, s := range []string{fmt.Sprintf("PRAGMA main.page_size=%d", pageSize), fmt.Sprintf("PRAGMA main.auto_vacuum=%d", autoVacuum)} {
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
		cols, e := tableColumnsContext(ctx, srcConn, t.name)
		if e != nil {
			return e
		}
		list := strings.Join(cols, ",")
		numCols := len(cols)
		if !strings.Contains(strings.ToUpper(t.sql), "WITHOUT ROWID") {
			// Keep rowids: other tables and Engine refer to them.
			list = "rowid," + list
			numCols++
		}
		if err = copyTableRowsFromConn(ctx, srcConn, tx, t.name, list, numCols); err != nil {
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

func copyTableRowsFromConn(ctx context.Context, srcConn *sql.Conn, dstTx *sql.Tx, table, colList string, numCols int) error {
	rows, err := srcConn.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM main.%s", colList, quote(table)))
	if err != nil {
		return err
	}
	defer rows.Close()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", numCols), ",")
	insStmt, err := dstTx.PrepareContext(ctx, fmt.Sprintf("INSERT INTO main.%s(%s) VALUES(%s)", quote(table), colList, placeholders))
	if err != nil {
		return err
	}
	defer insStmt.Close()

	vals := make([]any, numCols)
	scanDest := make([]any, numCols)
	for i := range vals {
		scanDest[i] = &vals[i]
	}
	for rowIdx := 0; rows.Next(); rowIdx++ {
		if rowIdx&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := rows.Scan(scanDest...); err != nil {
			return err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				if b == nil {
					// Preserve zero-length X'' BLOB values; typed nil []byte becomes SQL NULL in database/sql.
					vals[i] = []byte{}
				} else {
					cp := make([]byte, len(b))
					copy(cp, b)
					vals[i] = cp
				}
			}
		}
		if _, err := insStmt.ExecContext(ctx, vals...); err != nil {
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

func tableColumnsContext(ctx context.Context, q sqlQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main')", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, quote(c))
	}
	return cols, rows.Err()
}
