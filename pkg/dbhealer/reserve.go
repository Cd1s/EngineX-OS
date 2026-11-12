package dbhealer

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var sqliteMagic = []byte("SQLite format 3\x00")

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
	if r, ok, err := Reserve(path); err != nil || !ok || r == 0 {
		return false, err
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if s, e := os.Stat(path + suffix); e == nil && s.Size() > 0 {
			return false, fmt.Errorf("%s has pending %s; let Engine recover it first", path, suffix)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	tmp := path + ".reserve-rebuild"
	os.Remove(tmp)
	if err = rebuildPlain(path, tmp); err != nil {
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
	// atomically replace the still-present original with the rebuilt database.
	if err = backupReserve(path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

func backupReserve(path string) error {
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
		_, err = io.Copy(dst, src)
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
	names, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, err
	}
	for _, p := range names {
		if strings.HasPrefix(filepath.Base(p), ".") {
			continue
		}
		ok, e := NormalizeReserve(p)
		if ok {
			fixed = append(fixed, filepath.Base(p))
		}
		err = errors.Join(err, e)
	}
	return fixed, err
}

// fileURI keeps '#', '?' and '%' in a stick label part of the path for SQLite.
func fileURI(p string) string { return "file:" + (&url.URL{Path: filepath.ToSlash(p)}).EscapedPath() }

func rebuildPlain(src, dst string) error {
	db, err := sql.Open("sqlite", fileURI(dst))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Escaped: a stick label with '#' or '?' cut the URI there, ATTACHed an empty
	// file and the empty rebuild replaced the library (BH-GC1-1).
	if _, err = db.Exec("ATTACH DATABASE ? AS old", fileURI(src)+"?mode=ro&immutable=1"); err != nil {
		return err
	}
	// Properties that must survive; page_size and auto_vacuum have to be set first.
	var pageSize, autoVacuum, userVersion, appID int64
	for pragma, v := range map[string]*int64{"page_size": &pageSize, "auto_vacuum": &autoVacuum, "user_version": &userVersion, "application_id": &appID} {
		if err = db.QueryRow("PRAGMA old." + pragma).Scan(v); err != nil {
			return err
		}
	}
	for _, s := range []string{fmt.Sprintf("PRAGMA main.page_size=%d", pageSize), fmt.Sprintf("PRAGMA main.auto_vacuum=%d", autoVacuum)} {
		if _, err = db.Exec(s); err != nil {
			return err
		}
	}
	type object struct{ typ, name, sql string }
	var objects []object
	rows, err := db.Query("SELECT type,name,sql FROM old.sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var o object
		if err = rows.Scan(&o.typ, &o.name, &o.sql); err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(objects) == 0 {
		return errors.New("source has no schema; refusing to replace it with an empty database")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables []object
	for _, o := range objects {
		if o.typ == "table" {
			if _, err = tx.Exec(o.sql); err != nil {
				return fmt.Errorf("create %s: %w", o.name, err)
			}
			tables = append(tables, o)
		}
	}
	for _, t := range tables {
		cols, e := tableColumns(tx, t.name)
		if e != nil {
			return e
		}
		list := strings.Join(cols, ",")
		if !strings.Contains(strings.ToUpper(t.sql), "WITHOUT ROWID") {
			// Keep rowids: other tables and Engine refer to them.
			list = "rowid," + list
		}
		if _, err = tx.Exec(fmt.Sprintf("INSERT INTO main.%s(%s) SELECT %s FROM old.%s", quote(t.name), list, list, quote(t.name))); err != nil {
			return fmt.Errorf("copy %s: %w", t.name, err)
		}
	}
	var hasSeq int
	if err = tx.QueryRow("SELECT count(*) FROM old.sqlite_master WHERE name='sqlite_sequence'").Scan(&hasSeq); err != nil {
		return err
	}
	if hasSeq > 0 {
		if _, err = tx.Exec("DELETE FROM main.sqlite_sequence"); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO main.sqlite_sequence SELECT name,seq FROM old.sqlite_sequence"); err != nil {
			return err
		}
	}
	for _, typ := range []string{"index", "view", "trigger"} {
		for _, o := range objects {
			if o.typ == typ {
				if _, err = tx.Exec(o.sql); err != nil {
					return fmt.Errorf("create %s %s: %w", typ, o.name, err)
				}
			}
		}
	}
	for _, t := range tables {
		var a, b int64
		if err = tx.QueryRow(fmt.Sprintf("SELECT (SELECT count(*) FROM old.%[1]s),(SELECT count(*) FROM main.%[1]s)", quote(t.name))).Scan(&a, &b); err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("copy %s: %d rows became %d", t.name, a, b)
		}
	}
	var made int
	if err = tx.QueryRow("SELECT count(*) FROM main.sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'").Scan(&made); err != nil {
		return err
	}
	if made != len(objects) {
		return fmt.Errorf("schema has %d objects, rebuilt %d", len(objects), made)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, s := range []string{fmt.Sprintf("PRAGMA main.user_version=%d", userVersion), fmt.Sprintf("PRAGMA main.application_id=%d", appID)} {
		if _, err = db.Exec(s); err != nil {
			return err
		}
	}
	if _, err = db.Exec("DETACH DATABASE old"); err != nil {
		return err
	}
	var check string
	if err = db.QueryRow("PRAGMA main.integrity_check").Scan(&check); err != nil || check != "ok" {
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

func tableColumns(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query("SELECT name FROM pragma_table_info(?, 'old')", table)
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
