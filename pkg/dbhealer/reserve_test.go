package dbhealer

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	sqlite "modernc.org/sqlite"
)

func TestReserveDeadlineCannotRemoveLiveDatabase(t *testing.T) {
	if path := os.Getenv("ENGINEX_RESERVE_BACKUP_TEST"); path != "" {
		if err := backupReserve(path); err != nil {
			os.Exit(1)
		}
		// The guardian deadline can fire after the backup and before publication.
		os.Exit(124)
	}
	path := reserve12Library(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReserveDeadlineCannotRemoveLiveDatabase$")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_BACKUP_TEST="+path)
	if err := cmd.Run(); err == nil {
		t.Fatal("deadline fixture did not exit")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 124 {
		t.Fatalf("backup checkpoint failed: %v", err)
	}
	for _, name := range []string{path, path + ".pre-reserve"} {
		if body, err := os.ReadFile(name); err != nil || !bytes.Equal(body, before) {
			t.Fatalf("interrupted publication lost original data at %s: %v", name, err)
		}
	}
	if fixed, err := NormalizeReserve(path); err != nil || !fixed {
		t.Fatalf("retry after interrupted publication: %v %v", fixed, err)
	}
}

// reserve12Library is a stock-schema library whose pages reserve 12 bytes, like the
// `.recover`ed m.db that broke every ATTACH on a deck. The header is patched while
// the file is still one empty page, so SQLite lays out every later page with it.
func reserve12Library(t *testing.T) string {
	t.Helper()
	path := reserve12Seed(t, "m.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('test-library',3,0,2)")
	for id := 5; id < 60; id += 11 { // gaps: rowids must survive
		mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(?,?,1)", id, strings.Repeat("Music/", 40)+fmt.Sprint(id)+".mp3")
	}
	mustExec(t, db, "UPDATE PerformanceData SET trackData=? WHERE trackId=5", bytes.Repeat([]byte{7}, 20000)) // overflow pages
	// AUTOINCREMENT counter ahead of max(id) must survive too.
	mustExec(t, db, "UPDATE sqlite_sequence SET seq=900 WHERE name='Track'")
	return path
}

// reserve12Seed is an empty database file whose pages reserve 12 bytes.
func reserve12Seed(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "PRAGMA user_version=4194305")
	db.Close()
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) != 4096 {
		t.Fatalf("seed file: %d %v", len(raw), err)
	}
	raw[20] = 12
	raw[105], raw[106] = 0x0f, 0xf4 // empty page 1: cell content must start inside the usable 4084 bytes
	if err = os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A file that once held an AUTOINCREMENT table still has sqlite_sequence after the
// table is gone; the rebuilt file has none, and the rebuild must not stop there (BH2-A1-5).
func TestNormalizeReserveRebuildsLibraryWithLeftoverSequenceTable(t *testing.T) {
	path := reserve12Seed(t, "hm.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE Gone(id INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT)")
	mustExec(t, db, "INSERT INTO Gone(v) VALUES('x')")
	mustExec(t, db, "DROP TABLE Gone")
	mustExec(t, db, "CREATE TABLE Kept(id INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO Kept(id,v) VALUES(7,'seven'),(40,'forty')")
	var seq int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='sqlite_sequence'").Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("setup: sqlite_sequence present=%d err=%v", seq, err)
	}
	db.Close()
	if r, ok, err := Reserve(path); err != nil || !ok || r != 12 {
		t.Fatalf("setup: reserve %d ok=%v err=%v", r, ok, err)
	}
	fixed, err := NormalizeReserve(path)
	if err != nil || !fixed {
		t.Fatalf("fixed=%v err=%v", fixed, err)
	}
	if r, _, _ := Reserve(path); r != 0 {
		t.Fatalf("reserve after repair = %d", r)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var rows, check string
	if err = out.QueryRow("SELECT group_concat(id||v,',') FROM (SELECT id,v FROM Kept ORDER BY id)").Scan(&rows); err != nil || rows != "7seven,40forty" {
		t.Fatalf("rows %q err=%v", rows, err)
	}
	if err = out.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity %q err=%v", check, err)
	}
}

func TestNormalizeReserveRebuildsLibraryWithReserve12(t *testing.T) {
	path := reserve12Library(t)
	if r, ok, err := Reserve(path); err != nil || !ok || r != 12 {
		t.Fatalf("setup: reserve %d ok=%v err=%v", r, ok, err)
	}
	dump := func(p string) string {
		db, err := Open(p, true)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var out strings.Builder
		for _, q := range []string{
			"SELECT group_concat(type||name,',') FROM (SELECT type,name FROM sqlite_master ORDER BY name)",
			"SELECT group_concat(id||path,',') FROM (SELECT id,path FROM Track ORDER BY id)",
			"SELECT hex(trackData) FROM PerformanceData",
			"SELECT group_concat(name||seq,',') FROM sqlite_sequence",
			"PRAGMA user_version", "PRAGMA page_size", "PRAGMA integrity_check",
		} {
			var v sql.NullString
			if err := db.QueryRow(q).Scan(&v); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			out.WriteString(v.String + "\n")
		}
		return out.String()
	}
	before := dump(path)
	fixed, err := NormalizeReserve(path)
	if err != nil || !fixed {
		t.Fatalf("fixed=%v err=%v", fixed, err)
	}
	if r, _, _ := Reserve(path); r != 0 {
		t.Fatalf("reserve after repair = %d", r)
	}
	if after := dump(path); after != before {
		t.Fatalf("content changed\nbefore %.300s\nafter  %.300s", before, after)
	}
	if r, _, _ := Reserve(path + ".pre-reserve"); r != 12 {
		t.Fatalf("original not kept: %d", r)
	}
	if _, err = os.Stat(path + ".reserve-rebuild"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if again, err := NormalizeReserve(path); again || err != nil {
		t.Fatalf("second run changed a clean library: %v %v", again, err)
	}
	// The repaired library still heals, which also proves the schema is intact.
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = Heal(db); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeReserveLeavesOtherFilesAlone(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "m.db")
	os.WriteFile(m, []byte("not a database"), 0o644)
	os.WriteFile(filepath.Join(dir, ".rb-bridge-state.db"), []byte("SQLite format 3\x00"), 0o644)
	fixed, err := NormalizeDatabase2(dir)
	if err != nil || len(fixed) != 0 {
		t.Fatalf("%v %v", fixed, err)
	}
	if b, _ := os.ReadFile(m); string(b) != "not a database" {
		t.Fatal("foreign file modified")
	}
	// A hot journal means Engine has not recovered the file yet: refuse.
	bad := reserve12Library(t)
	os.WriteFile(bad+"-journal", []byte("x"), 0o644)
	if fixed, err := NormalizeReserve(bad); fixed || err == nil {
		t.Fatalf("rebuilt under a hot journal: %v %v", fixed, err)
	}
}

// BH-GC1-1: on a stick labelled "USB #1" the unescaped URI ATTACHed an empty file
// and the empty rebuild replaced m.db, so the whole library was gone.
func TestNormalizeReserveKeepsLibraryOnLabelWithHashAndQuestionMark(t *testing.T) {
	for _, label := range []string{"USB #1", "MUSIC?2", "100% DJ"} {
		t.Run(label, func(t *testing.T) { normalizeUnderLabel(t, label) })
	}
}

func normalizeUnderLabel(t *testing.T, label string) {
	src := reserve12Library(t)
	dir := filepath.Join(t.TempDir(), label, "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "m.db")
	if err := os.Rename(src, path); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		db, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	if before == 0 {
		t.Fatal("setup: fixture has no tracks")
	}
	if fixed, err := NormalizeReserve(path); err != nil || !fixed {
		t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
	}
	if after := count(); after != before {
		t.Fatalf("tracks %d -> %d", before, after)
	}
}

func TestNormalizeDatabase2PropagatesWritableErrorWithoutRebuilding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Engine Library", "Database2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	hm := filepath.Join(dir, "hm.db")
	if err := os.WriteFile(hm, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hm, 0o644) })

	origWritable := reserveWritable
	reserveWritable = func(p string) (bool, error) {
		if filepath.Base(p) == "hm.db" {
			return false, syscall.EIO
		}
		return origWritable(p)
	}
	t.Cleanup(func() { reserveWritable = origWritable })

	fixed, err := NormalizeDatabase2(dir)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("NormalizeDatabase2 err=%v, want EIO", err)
	}
	if len(fixed) != 0 {
		t.Fatalf("NormalizeDatabase2 unexpectedly rebuilt files: %v", fixed)
	}
	if r, ok, rerr := Reserve(hm); rerr != nil || !ok || r != 12 {
		t.Fatalf("mode-0444 hm.db reserve changed: r=%d ok=%v err=%v", r, ok, rerr)
	}
	if _, serr := os.Stat(hm + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("pre-reserve backup created on EIO: err=%v", serr)
	}
}

func TestHealMountDoesNotRebuildLockedReserve12MainDB(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	db.Close()
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	if err := os.WriteFile(mPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(mPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIno := beforeInfo.Sys().(*syscall.Stat_t).Ino

	locker, err := sql.Open("sqlite", mPath)
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = locker.Exec("ROLLBACK") }()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rep, err := HealMountDataOnlyContext(ctx, mount, 0)
	if err == nil || rep.Healthy || len(rep.ReserveFixed) != 0 {
		t.Fatalf("locked reserve-12 m.db unexpectedly succeeded/rebuilt: rep=%+v err=%v", rep, err)
	}
	afterInfo, err := os.Stat(mPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterIno := afterInfo.Sys().(*syscall.Stat_t).Ino; afterIno != beforeIno {
		t.Fatalf("locked m.db inode changed: %d -> %d", beforeIno, afterIno)
	}
	if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
		t.Fatalf("locked m.db reserve changed: r=%d ok=%v err=%v", r, ok, rerr)
	}
	if _, serr := os.Stat(mPath + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("m.db.pre-reserve created while exclusively locked: err=%v", serr)
	}
}

func TestHealMountDoesNotRebuildReserve12UnderImmediateWriter(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	db.Close()
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	if err := os.WriteFile(mPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(mPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIno := beforeInfo.Sys().(*syscall.Stat_t).Ino

	writer, err := sql.Open("sqlite", mPath)
	if err != nil {
		t.Fatal(err)
	}
	writer.SetMaxOpenConns(1)
	defer writer.Close()
	if _, err := writer.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = writer.Exec("ROLLBACK")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rep, err := HealMountDataOnlyContext(ctx, mount, 0)
	if err == nil || rep.Healthy || len(rep.ReserveFixed) != 0 {
		t.Fatalf("immediate-locked reserve-12 m.db unexpectedly succeeded/rebuilt: rep=%+v err=%v", rep, err)
	}
	afterInfo, err := os.Stat(mPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterIno := afterInfo.Sys().(*syscall.Stat_t).Ino; afterIno != beforeIno {
		t.Fatalf("immediate-locked m.db inode changed: %d -> %d", beforeIno, afterIno)
	}
	if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
		t.Fatalf("immediate-locked m.db reserve changed: r=%d ok=%v err=%v", r, ok, rerr)
	}
	if _, serr := os.Stat(mPath + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("m.db.pre-reserve created while immediate writer held reservation: err=%v", serr)
	}
	if _, err := writer.Exec("UPDATE Track SET filename='updated.mp3' WHERE id=5"); err != nil {
		t.Fatalf("writer update failed after aborted reserve rebuild: %v", err)
	}
	if _, err := writer.Exec("COMMIT"); err != nil {
		t.Fatalf("writer commit failed after aborted reserve rebuild: %v", err)
	}
	committed = true
}

func TestNormalizeReservePreservesSQLiteStorageTypes(t *testing.T) {
	path := reserve12Seed(t, "types.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, `CREATE TABLE Types(id INTEGER PRIMARY KEY, val_null BLOB, val_empty_str TEXT, val_empty_blob BLOB, val_int INTEGER, val_real REAL, val_blob BLOB);
		INSERT INTO Types(id,val_null,val_empty_str,val_empty_blob,val_int,val_real,val_blob) VALUES(1, NULL, '', X'', 42, 3.25, X'0001ff');`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if fixed, err := NormalizeReserve(path); err != nil || !fixed {
		t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var got string
	if err := out.QueryRow(`SELECT typeof(val_null)||'|'||typeof(val_empty_str)||':'||length(val_empty_str)||'|'||typeof(val_empty_blob)||':'||length(val_empty_blob)||'|'||typeof(val_int)||':'||val_int||'|'||typeof(val_real)||'|'||typeof(val_blob)||':'||hex(val_blob) FROM Types WHERE id=1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "null|text:0|blob:0|integer:42|real|blob:0001FF"
	if got != want {
		t.Fatalf("storage types mismatch: got %q want %q", got, want)
	}
}

func TestOnlyContextErrorRejectsWrappedJoinedEIO(t *testing.T) {
	wrappedJoined := &os.PathError{
		Op:   "stat",
		Path: "hm.db",
		Err:  errors.Join(context.DeadlineExceeded, syscall.EIO),
	}
	if onlyContextError(wrappedJoined) {
		t.Fatal("onlyContextError returned true for PathError wrapping joined DeadlineExceeded + EIO")
	}
	pureWrappedJoined := &os.PathError{
		Op:   "stat",
		Path: "hm.db",
		Err:  errors.Join(context.DeadlineExceeded, context.Canceled),
	}
	if !onlyContextError(pureWrappedJoined) {
		t.Fatal("onlyContextError returned false for PathError wrapping only context errors")
	}
}

func TestNormalizeDatabase2BlocksArrivingProcessWriterAfterCopyStarts(t *testing.T) {
	if dbPath := os.Getenv("ENGINEX_RESERVE_CONCURRENT_WRITER_DB"); dbPath != "" {
		wdb, err := sql.Open("sqlite", dbPath)
		if err != nil {
			os.Exit(2)
		}
		defer wdb.Close()
		wdb.SetMaxOpenConns(1)
		if _, err := wdb.Exec("PRAGMA busy_timeout=1"); err != nil {
			os.Exit(3)
		}
		if _, err := wdb.Exec("BEGIN IMMEDIATE"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(12)
		}
		if _, err := wdb.Exec("UPDATE Information SET uuid='concurrent-edit'"); err != nil {
			_, _ = wdb.Exec("ROLLBACK")
			os.Exit(13)
		}
		if _, err := wdb.Exec("COMMIT"); err != nil {
			_, _ = wdb.Exec("ROLLBACK")
			os.Exit(14)
		}
		os.Exit(0)
	}

	runCase := func(t *testing.T, forceCopyBackup bool) {
		t.Helper()
		_, mount, db := testlibrary.Create(t)
		db.Close()
		seed := reserve12Library(t)
		raw, err := os.ReadFile(seed)
		if err != nil {
			t.Fatal(err)
		}
		dbDir := filepath.Join(mount, "Engine Library", "Database2")
		mPath := filepath.Join(dbDir, "m.db")
		if err := os.WriteFile(mPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if forceCopyBackup {
			origLink := reserveLink
			reserveLink = func(string, string) error { return syscall.EXDEV }
			t.Cleanup(func() { reserveLink = origLink })
		}

		attemptConcurrentWrite := func(stage string) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeDatabase2BlocksArrivingProcessWriterAfterCopyStarts$")
			cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_CONCURRENT_WRITER_DB="+mPath)
			out, werr := cmd.CombinedOutput()
			if werr == nil {
				t.Fatalf("%s: concurrent external writer unexpectedly committed during reserve rebuild", stage)
			}
			var exitErr *exec.ExitError
			if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 12 {
				t.Fatalf("%s: expected busy exit code 12, got %v output=%s", stage, werr, string(out))
			}
		}

		reserveBeforeCopy = func(p string) {
			if p == mPath {
				attemptConcurrentWrite("before_copy")
			}
		}
		reserveBeforeRename = func(p string) {
			if p == mPath {
				attemptConcurrentWrite("before_rename")
			}
		}
		t.Cleanup(func() {
			reserveBeforeCopy = nil
			reserveBeforeRename = nil
		})

		fixed, err := NormalizeDatabase2(dbDir)
		if err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
			t.Fatalf("NormalizeDatabase2 failed: fixed=%v err=%v", fixed, err)
		}
		for _, p := range []string{mPath, mPath + ".pre-reserve"} {
			chk, oerr := Open(p, true)
			if oerr != nil {
				t.Fatal(oerr)
			}
			var gotUUID string
			if qerr := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); qerr != nil {
				chk.Close()
				t.Fatal(qerr)
			}
			chk.Close()
			if gotUUID != "test-library" {
				t.Fatalf("%s uuid=%q, want test-library", p, gotUUID)
			}
		}
	}

	t.Run("hardlink_backup", func(t *testing.T) { runCase(t, false) })
	t.Run("copy_backup_fallback", func(t *testing.T) { runCase(t, true) })
}

func TestNormalizeReserveRejectsEstablishedWALReader(t *testing.T) {
	if dbPath := os.Getenv("ENGINEX_WAL_READER_DB"); dbPath != "" {
		rdb, err := sql.Open("sqlite", dbPath)
		if err != nil {
			os.Exit(2)
		}
		defer rdb.Close()
		rdb.SetMaxOpenConns(1)
		if _, err := rdb.Exec("BEGIN"); err != nil {
			os.Exit(3)
		}
		var n int
		if err := rdb.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil {
			os.Exit(4)
		}
		fmt.Println("READY")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		if err := rdb.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil {
			os.Exit(5)
		}
		_, _ = rdb.Exec("ROLLBACK")
		os.Exit(0)
	}

	assertRejectedAndPreserved := func(t *testing.T, dbDir, mPath string, beforeIno uint64) {
		t.Helper()
		fixed, err := NormalizeDatabase2(dbDir)
		if err == nil || len(fixed) != 0 {
			t.Fatalf("NormalizeDatabase2 unexpectedly succeeded under active WAL reader: fixed=%v err=%v", fixed, err)
		}
		afterInfo, err := os.Stat(mPath)
		if err != nil {
			t.Fatal(err)
		}
		if afterIno := afterInfo.Sys().(*syscall.Stat_t).Ino; afterIno != beforeIno {
			t.Fatalf("WAL reader m.db inode changed: %d -> %d", beforeIno, afterIno)
		}
		if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
			t.Fatalf("WAL reader m.db reserve changed: r=%d ok=%v err=%v", r, ok, rerr)
		}
		if _, serr := os.Stat(mPath + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("m.db.pre-reserve created while WAL reader active: err=%v", serr)
		}
	}

	t.Run("same_process", func(t *testing.T) {
		dbDir, mPath, beforeIno := setupWALReserve12Fixture(t)
		reader, err := sql.Open("sqlite", mPath)
		if err != nil {
			t.Fatal(err)
		}
		reader.SetMaxOpenConns(1)
		defer reader.Close()
		if _, err := reader.Exec("BEGIN"); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = reader.Exec("ROLLBACK") }()
		var n int
		if err := reader.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil || n == 0 {
			t.Fatalf("reader count=%d err=%v", n, err)
		}
		assertRejectedAndPreserved(t, dbDir, mPath, beforeIno)
	})

	t.Run("cross_process", func(t *testing.T) {
		dbDir, mPath, beforeIno := setupWALReserve12Fixture(t)
		cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeReserveRejectsEstablishedWALReader$")
		cmd.Env = append(os.Environ(), "ENGINEX_WAL_READER_DB="+mPath)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		line, rerr := bufio.NewReader(stdout).ReadString('\n')
		if rerr != nil || strings.TrimSpace(line) != "READY" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("reader child failed to start: line=%q err=%v", line, rerr)
		}
		assertRejectedAndPreserved(t, dbDir, mPath, beforeIno)
		_ = stdin.Close()
		if werr := cmd.Wait(); werr != nil {
			t.Fatalf("reader child failed after parent check: %v", werr)
		}
	})
}

func TestNormalizeReservePreservesStockDatetimeColumns(t *testing.T) {
	path := reserve12Library(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	const wantText = "2024-03-01 12:34:56.021"
	const wantInt int64 = 1700000000
	mustExec(t, db, "UPDATE Track SET lastEditTime=?, dateCreated=? WHERE id=5", wantText, wantInt)
	var beforeJul float64
	if err := db.QueryRow("SELECT julianday(lastEditTime) FROM Track WHERE id=5").Scan(&beforeJul); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if fixed, err := NormalizeReserve(path); err != nil || !fixed {
		t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var typLast, rawHex, typCreated string
	var createdVal int64
	var jul sql.NullFloat64
	if err := out.QueryRow(
		`SELECT typeof(lastEditTime), hex(CAST(lastEditTime AS BLOB)), julianday(lastEditTime), typeof(dateCreated), dateCreated FROM Track WHERE id=5`,
	).Scan(&typLast, &rawHex, &jul, &typCreated, &createdVal); err != nil {
		t.Fatal(err)
	}
	if typLast != "text" || rawHex != fmt.Sprintf("%X", []byte(wantText)) || !jul.Valid || jul.Float64 != beforeJul {
		t.Fatalf("DATETIME text mangled: type=%s hex=%s jul=%+v wantJul=%v", typLast, rawHex, jul, beforeJul)
	}
	if typCreated != "integer" || createdVal != wantInt {
		t.Fatalf("DATETIME int mangled: type=%s val=%d want=%d", typCreated, createdVal, wantInt)
	}
}

func setupWALReserve12Fixture(t *testing.T) (string, string, uint64) {
	t.Helper()
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(t.TempDir(), "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(dbDir, "m.db")
	if err := os.WriteFile(mPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	wdb, err := sql.Open("sqlite", mPath)
	if err != nil {
		t.Fatal(err)
	}
	wdb.SetMaxOpenConns(1)
	var mode string
	if err := wdb.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
		wdb.Close()
		t.Fatalf("enable WAL mode=%q err=%v", mode, err)
	}
	if err := wdb.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mPath + "-wal"); err == nil {
		if err := os.Truncate(mPath+"-wal", 0); err != nil {
			t.Fatal(err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(mPath+"-wal", nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(mPath)
	if err != nil {
		t.Fatal(err)
	}
	return dbDir, mPath, info.Sys().(*syscall.Stat_t).Ino
}

func attemptConcurrentExternalWriteBusy(t *testing.T, dbPath, stage string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeDatabase2BlocksArrivingProcessWriterAfterCopyStarts$")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_CONCURRENT_WRITER_DB="+dbPath)
	out, werr := cmd.CombinedOutput()
	if werr == nil {
		t.Fatalf("%s: concurrent external writer unexpectedly acquired lock and committed", stage)
	}
	var exitErr *exec.ExitError
	if !errors.As(werr, &exitErr) || exitErr.ExitCode() != 12 {
		t.Fatalf("%s: expected busy exit code 12, got %v output=%s", stage, werr, string(out))
	}
}

func TestNormalizeDatabase2OverlappingCheckPreservesRebuildLock(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	db.Close()
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	mPath := filepath.Join(dbDir, "m.db")
	if err := os.WriteFile(mPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	reserveBeforeCopy = func(p string) {
		if p != mPath {
			return
		}
		if runtime.GOOS == "linux" && !sqlite.OFDLockingEnabled() {
			t.Fatalf("expected OFD locking enabled on Linux")
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
		fixed2, err2 := NormalizeDatabase2Context(ctx2, dbDir)
		cancel2()
		if err2 == nil || len(fixed2) != 0 {
			t.Fatalf("overlapping NormalizeDatabase2 unexpectedly succeeded: fixed=%v err=%v", fixed2, err2)
		}
		attemptConcurrentExternalWriteBusy(t, mPath, "after_overlapping_normalize")
	}
	t.Cleanup(func() { reserveBeforeCopy = nil })

	fixed, err := NormalizeDatabase2(dbDir)
	if err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
		t.Fatalf("NormalizeDatabase2 failed: fixed=%v err=%v", fixed, err)
	}
	for _, p := range []string{mPath, mPath + ".pre-reserve"} {
		chk, oerr := Open(p, true)
		if oerr != nil {
			t.Fatal(oerr)
		}
		var gotUUID string
		if qerr := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); qerr != nil {
			chk.Close()
			t.Fatal(qerr)
		}
		chk.Close()
		if gotUUID != "test-library" {
			t.Fatalf("%s uuid=%q, want test-library", p, gotUUID)
		}
	}
}

func TestFailedNormalizationPreservesUnrelatedSameProcessLock(t *testing.T) {
	_, mount, db := testlibrary.Create(t)
	db.Close()
	seed := reserve12Library(t)
	raw, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(mount, "Engine Library", "Database2")
	mPath := filepath.Join(dbDir, "m.db")
	if err := os.WriteFile(mPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite", mPath)
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = locker.Exec("ROLLBACK")
		}
	}()

	if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
		t.Fatalf("Reserve while locked: r=%d ok=%v err=%v", r, ok, rerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	fixed, nerr := NormalizeDatabase2Context(ctx, dbDir)
	cancel()
	if nerr == nil || len(fixed) != 0 {
		t.Fatalf("NormalizeDatabase2Context unexpectedly succeeded while locked: fixed=%v err=%v", fixed, nerr)
	}

	attemptConcurrentExternalWriteBusy(t, mPath, "after_failed_normalization")

	if _, err := locker.Exec("UPDATE Information SET uuid='locker-owned'"); err != nil {
		t.Fatalf("locker update failed after failed normalization: %v", err)
	}
	if _, err := locker.Exec("COMMIT"); err != nil {
		t.Fatalf("locker commit failed after failed normalization: %v", err)
	}
	committed = true
	if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
		t.Fatalf("m.db reserve changed after failed normalization: r=%d ok=%v err=%v", r, ok, rerr)
	}
	if _, serr := os.Stat(mPath + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("m.db.pre-reserve created on failed normalization: err=%v", serr)
	}
}

func TestNormalizeReserveWALDoesNotRemoveNewWriterSHM(t *testing.T) {
	dbDir, mPath, _ := setupWALReserve12Fixture(t)

	var newWriter *sql.DB
	var shmInfo os.FileInfo
	newWriterCommitted := false
	t.Cleanup(func() {
		reserveBeforeRename = nil
		reserveAfterRename = nil
		if newWriter != nil {
			if !newWriterCommitted {
				_, _ = newWriter.Exec("ROLLBACK")
			}
			_ = newWriter.Close()
		}
	})

	origRename := reserveRename
	reserveRename = func(oldPath, newPath string) error {
		if newPath == mPath {
			for _, suffix := range []string{"-shm", "-wal"} {
				if _, err := os.Stat(mPath + suffix); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("old sidecar %s still present at rename: err=%v", suffix, err)
				}
			}
		}
		return origRename(oldPath, newPath)
	}

	reserveBeforeRename = func(p string) {
		if p != mPath {
			return
		}
		attemptConcurrentExternalWriteBusy(t, mPath, "before_wal_rename")
	}
	var walInfo os.FileInfo
	reserveAfterRename = func(p string) {
		if p != mPath {
			return
		}
		var oerr error
		newWriter, oerr = sql.Open("sqlite", mPath)
		if oerr != nil {
			t.Fatal(oerr)
		}
		newWriter.SetMaxOpenConns(1)
		if _, err := newWriter.Exec("PRAGMA busy_timeout=1"); err != nil {
			t.Fatal(err)
		}
		var mode string
		if err := newWriter.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || !strings.EqualFold(mode, "wal") {
			t.Fatalf("post-rename writer failed to enter WAL mode=%q err=%v", mode, err)
		}
		if _, err := newWriter.Exec("UPDATE Track SET comment='post-rename-wal' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		if _, err := newWriter.Exec("BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		if _, err := newWriter.Exec("UPDATE Track SET filename='held-writer.mp3' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		var serr error
		shmInfo, serr = os.Stat(mPath + "-shm")
		if serr != nil || shmInfo.Size() == 0 {
			t.Fatalf("new WAL writer -shm missing after rename: info=%v err=%v", shmInfo, serr)
		}
		walInfo, serr = os.Stat(mPath + "-wal")
		if serr != nil || walInfo.Size() == 0 {
			t.Fatalf("new WAL writer -wal missing/empty after rename: info=%v err=%v", walInfo, serr)
		}
	}

	t.Cleanup(func() {
		reserveRename = origRename
	})

	fixed, err := NormalizeDatabase2(dbDir)
	if err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
		t.Fatalf("NormalizeDatabase2 WAL failed: fixed=%v err=%v", fixed, err)
	}
	shmAfter, serr := os.Stat(mPath + "-shm")
	if serr != nil {
		t.Fatalf("new WAL writer -shm was removed: err=%v", serr)
	}
	if !os.SameFile(shmInfo, shmAfter) {
		t.Fatalf("new WAL writer -shm inode changed")
	}
	walAfter, werr := os.Stat(mPath + "-wal")
	if werr != nil {
		t.Fatalf("new WAL writer -wal was removed: err=%v", werr)
	}
	if !os.SameFile(walInfo, walAfter) {
		t.Fatalf("new WAL writer -wal inode changed")
	}
	bakRaw, berr := os.ReadFile(mPath + ".pre-reserve")
	if berr != nil || len(bakRaw) < 21 || bakRaw[18] != 2 || bakRaw[19] != 2 || bakRaw[20] != 12 {
		t.Fatalf("WAL .pre-reserve header corrupted: len=%d err=%v", len(bakRaw), berr)
	}

	attemptConcurrentExternalWriteBusy(t, mPath, "post_normalize_while_new_wal_writer_held")

	if _, err := newWriter.Exec("COMMIT"); err != nil {
		t.Fatalf("held WAL writer commit failed: %v", err)
	}
	newWriterCommitted = true
	if err := newWriter.Close(); err != nil {
		t.Fatal(err)
	}
	newWriter = nil

	if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 0 {
		t.Fatalf("rebuilt reserve=%d ok=%v err=%v", r, ok, rerr)
	}
	chk, err := Open(mPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer chk.Close()
	var filename, comment string
	if err := chk.QueryRow("SELECT filename, comment FROM Track WHERE id=5").Scan(&filename, &comment); err != nil {
		t.Fatal(err)
	}
	if filename != "held-writer.mp3" || comment != "post-rename-wal" {
		t.Fatalf("post-rename WAL updates lost: filename=%q comment=%q", filename, comment)
	}
}

func TestNormalizeReservePreservesUTF16LibrariesAndDatetime(t *testing.T) {
	const wantPath = "../Music/Café.mp3"
	const wantTitle = "Café Track"
	const wantUUID = "utf16-uuid-café"
	const wantTextDT = "2024-03-01 12:34:56.021"
	const wantIntDT int64 = 1700000000

	for _, enc := range []string{"UTF-8", "UTF-16le", "UTF-16be"} {
		t.Run(enc, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, fmt.Sprintf("PRAGMA encoding=%q; CREATE TABLE __seed(x); DROP TABLE __seed; VACUUM; PRAGMA user_version=4194305", enc))
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) != 4096 {
				t.Fatalf("seed len=%d err=%v", len(raw), err)
			}
			raw[20] = 12
			raw[105], raw[106] = 0x0f, 0xf4
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}

			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			schema, err := os.ReadFile("testdata/engine-5.0.4.sql")
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			mustExec(t, db, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
			mustExec(t, db, "INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(?,3,0,2)", wantUUID)
			mustExec(t, db, "INSERT INTO Track(id,path,filename,title,lastEditTime,dateCreated,isAnalyzed) VALUES(5,?,?,?,?,?,1)",
				wantPath, "Café.mp3", wantTitle, wantTextDT, wantIntDT)
			var beforeEnc, beforeLastHex string
			if err := db.QueryRow("PRAGMA main.encoding").Scan(&beforeEnc); err != nil || !strings.EqualFold(beforeEnc, enc) {
				db.Close()
				t.Fatalf("setup encoding=%q want %q err=%v", beforeEnc, enc, err)
			}
			var beforeJul float64
			if err := db.QueryRow("SELECT hex(CAST(lastEditTime AS BLOB)), julianday(lastEditTime) FROM Track WHERE id=5").Scan(&beforeLastHex, &beforeJul); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			if fixed, err := NormalizeReserve(path); err != nil || !fixed {
				t.Fatalf("NormalizeReserve(%s) = %v, %v", enc, fixed, err)
			}
			if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
				t.Fatalf("reserve after rebuild = %d ok=%v err=%v", r, ok, rerr)
			}

			out, err := Open(path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			var afterEnc, check, gotUUID, gotTitle, typLast, gotLastHex, gotLastText, typCreated string
			var gotIntDT int64
			var pathMatches int
			var afterJul float64
			if err := out.QueryRow("PRAGMA main.encoding").Scan(&afterEnc); err != nil || !strings.EqualFold(afterEnc, enc) {
				t.Fatalf("rebuilt encoding=%q want %q err=%v", afterEnc, enc, err)
			}
			if err := out.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
				t.Fatalf("integrity_check=%q err=%v", check, err)
			}
			if err := out.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); err != nil || gotUUID != wantUUID {
				t.Fatalf("uuid=%q want %q err=%v", gotUUID, wantUUID, err)
			}
			if err := out.QueryRow("SELECT title, typeof(lastEditTime), hex(CAST(lastEditTime AS BLOB)), CASE WHEN 1 THEN lastEditTime END, julianday(lastEditTime), typeof(dateCreated), dateCreated FROM Track WHERE id=5").Scan(&gotTitle, &typLast, &gotLastHex, &gotLastText, &afterJul, &typCreated, &gotIntDT); err != nil {
				t.Fatal(err)
			}
			if gotTitle != wantTitle || strings.ContainsRune(gotTitle, '\x00') {
				t.Fatalf("title corrupted: %q (%x)", gotTitle, []byte(gotTitle))
			}
			if typLast != "text" || gotLastHex != beforeLastHex || gotLastText != wantTextDT || afterJul != beforeJul || typCreated != "integer" || gotIntDT != wantIntDT {
				t.Fatalf("datetime mismatch: got (%s, %s, %q, %v, %s, %d) want (text, %s, %q, %v, integer, %d)", typLast, gotLastHex, gotLastText, afterJul, typCreated, gotIntDT, beforeLastHex, wantTextDT, beforeJul, wantIntDT)
			}
			if err := out.QueryRow("SELECT count(*) FROM Track WHERE path=?", wantPath).Scan(&pathMatches); err != nil || pathMatches != 1 {
				t.Fatalf("path lookup count=%d want 1 err=%v", pathMatches, err)
			}
		})
	}
}

func TestNormalizeReserveWideAuxiliaryTable(t *testing.T) {
	t.Run("1000_cols_implicit_rowid", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		colDefs := make([]string, 1000)
		colNames := make([]string, 1000)
		vals := make([]any, 1000)
		placeholders := make([]string, 1000)
		for i := range colDefs {
			name := fmt.Sprintf("c%d", i)
			colNames[i] = name
			colDefs[i] = name + " TEXT"
			vals[i] = fmt.Sprintf("v%d", i)
			placeholders[i] = "?"
		}
		vals[0] = "first"
		vals[500] = "mid"
		vals[999] = "last"
		mustExec(t, db, "CREATE TABLE WideAux("+strings.Join(colDefs, ",")+")")
		if _, err := db.Exec("INSERT INTO WideAux("+strings.Join(colNames, ",")+") VALUES("+strings.Join(placeholders, ",")+")", vals...); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve on 1000-col table = %v, %v", fixed, err)
		}
		if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
			t.Fatalf("reserve=%d ok=%v err=%v", r, ok, rerr)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var c0, c500, c999 string
		if err := out.QueryRow("SELECT c0, c500, c999 FROM WideAux").Scan(&c0, &c500, &c999); err != nil {
			t.Fatal(err)
		}
		if c0 != "first" || c500 != "mid" || c999 != "last" {
			t.Fatalf("WideAux values=(%q,%q,%q), want (first,mid,last)", c0, c500, c999)
		}
	})

	t.Run("2000_cols_integer_primary_key", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		colDefs := make([]string, 2000)
		colDefs[0] = "id INTEGER PRIMARY KEY"
		for i := 1; i < 2000; i++ {
			colDefs[i] = fmt.Sprintf("c%d TEXT", i)
		}
		mustExec(t, db, "CREATE TABLE Wide2000("+strings.Join(colDefs, ",")+")")
		mustExec(t, db, "INSERT INTO Wide2000(id,c1,c1000,c1999) VALUES(5,'a1','a1000','a1999'),(42,'b1','b1000','b1999')")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve on 2000-col INTEGER PRIMARY KEY table = %v, %v", fixed, err)
		}
		if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
			t.Fatalf("reserve=%d ok=%v err=%v", r, ok, rerr)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		for _, want := range []struct {
			id               int
			c1, c1000, c1999 string
		}{
			{5, "a1", "a1000", "a1999"},
			{42, "b1", "b1000", "b1999"},
		} {
			var rid int
			var c1, c1000, c1999 string
			if err := out.QueryRow("SELECT rowid, c1, c1000, c1999 FROM Wide2000 WHERE id=?", want.id).Scan(&rid, &c1, &c1000, &c1999); err != nil {
				t.Fatal(err)
			}
			if rid != want.id || c1 != want.c1 || c1000 != want.c1000 || c1999 != want.c1999 {
				t.Fatalf("id=%d got=(%d,%q,%q,%q) want=(%d,%q,%q,%q)", want.id, rid, c1, c1000, c1999, want.id, want.c1, want.c1000, want.c1999)
			}
		}
	})
}

func TestPOSIXFallbackPreservesUnrelatedSameProcessLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX fcntl close lock drop is Linux/Unix specific")
	}
	if os.Getenv("ENGINEX_RESERVE_POSIX_FALLBACK_TEST") == "1" {
		_, err := sqlite.OFDLocking(false)
		if err != nil || sqlite.OFDLockingEnabled() {
			t.Fatalf("failed to disable OFD locking in child: enabled=%v err=%v", sqlite.OFDLockingEnabled(), err)
		}

		// Scenario A: unrelated same-process BEGIN EXCLUSIVE owner under POSIX F_SETLK.
		dbDir1 := filepath.Join(t.TempDir(), "Engine Library", "Database2")
		if err := os.MkdirAll(dbDir1, 0o755); err != nil {
			t.Fatal(err)
		}
		mPath1 := filepath.Join(dbDir1, "m.db")
		seed1, err := os.ReadFile(reserve12Library(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mPath1, seed1, 0o644); err != nil {
			t.Fatal(err)
		}
		locker, err := sql.Open("sqlite", mPath1)
		if err != nil {
			t.Fatal(err)
		}
		locker.SetMaxOpenConns(1)
		defer locker.Close()
		if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = locker.Exec("ROLLBACK")
			}
		}()
		if r, ok, rerr := Reserve(mPath1); rerr != nil || !ok || r != 12 {
			t.Fatalf("POSIX Reserve=%d ok=%v err=%v", r, ok, rerr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		fixed, nerr := NormalizeDatabase2Context(ctx, dbDir1)
		cancel()
		if nerr == nil || len(fixed) != 0 {
			t.Fatalf("POSIX NormalizeDatabase2Context unexpectedly succeeded: fixed=%v err=%v", fixed, nerr)
		}
		attemptConcurrentExternalWriteBusy(t, mPath1, "posix_after_failed_normalization")
		if _, err := locker.Exec("UPDATE Information SET uuid='posix-locker-owned'"); err != nil {
			t.Fatalf("locker update failed: %v", err)
		}
		if _, err := locker.Exec("COMMIT"); err != nil {
			t.Fatalf("locker commit failed: %v", err)
		}
		committed = true
		if err := locker.Close(); err != nil {
			t.Fatal(err)
		}
		chk, err := Open(mPath1, true)
		if err != nil {
			t.Fatal(err)
		}
		var gotUUID string
		if qerr := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); qerr != nil {
			chk.Close()
			t.Fatal(qerr)
		}
		chk.Close()
		if gotUUID != "posix-locker-owned" {
			t.Fatalf("uuid=%q want posix-locker-owned", gotUUID)
		}
		if r, ok, rerr := Reserve(mPath1); rerr != nil || !ok || r != 12 {
			t.Fatalf("reserve changed=%d ok=%v err=%v", r, ok, rerr)
		}
		if _, serr := os.Stat(mPath1 + ".pre-reserve"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("unexpected pre-reserve backup: err=%v", serr)
		}

		// Scenario B: overlapping NormalizeDatabase2 in POSIX mode still blocks external writers.
		dbDir2 := filepath.Join(t.TempDir(), "Engine Library", "Database2")
		if err := os.MkdirAll(dbDir2, 0o755); err != nil {
			t.Fatal(err)
		}
		mPath2 := filepath.Join(dbDir2, "m.db")
		if err := os.WriteFile(mPath2, seed1, 0o644); err != nil {
			t.Fatal(err)
		}
		reserveBeforeCopy = func(p string) {
			if p != mPath2 {
				return
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
			fixed2, err2 := NormalizeDatabase2Context(ctx2, dbDir2)
			cancel2()
			if err2 == nil || len(fixed2) != 0 {
				t.Fatalf("POSIX overlapping NormalizeDatabase2 succeeded: fixed=%v err=%v", fixed2, err2)
			}
			attemptConcurrentExternalWriteBusy(t, mPath2, "posix_overlapping_normalize")
		}
		defer func() { reserveBeforeCopy = nil }()
		if fixed2, err := NormalizeDatabase2(dbDir2); err != nil || len(fixed2) != 1 || fixed2[0] != "m.db" {
			t.Fatalf("POSIX NormalizeDatabase2 failed: fixed=%v err=%v", fixed2, err)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXFallbackPreservesUnrelatedSameProcessLock$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_POSIX_FALLBACK_TEST=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("POSIX fallback child failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestPOSIXFallbackPreservesUnrelatedSameProcessLock")) {
		t.Fatalf("POSIX fallback child did not pass:\n%s", string(out))
	}
}

func TestNormalizeReserveWALPreservesHardlinkedBackupAndLiveHeaderOnRenameFailure(t *testing.T) {
	t.Run("clean_rebuild_keeps_wal_backup_header_and_no_stale_sidecars", func(t *testing.T) {
		dbDir, mPath, _ := setupWALReserve12Fixture(t)
		beforeBytes, err := os.ReadFile(mPath)
		if err != nil || len(beforeBytes) < 21 || beforeBytes[18] != 2 || beforeBytes[19] != 2 || beforeBytes[20] != 12 {
			t.Fatalf("unexpected WAL setup header: len=%d err=%v", len(beforeBytes), err)
		}
		fixed, err := NormalizeDatabase2(dbDir)
		if err != nil || len(fixed) != 1 || fixed[0] != "m.db" {
			t.Fatalf("NormalizeDatabase2 failed: fixed=%v err=%v", fixed, err)
		}
		bakBytes, err := os.ReadFile(mPath + ".pre-reserve")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bakBytes, beforeBytes) {
			t.Fatalf("WAL .pre-reserve mutated: got bytes[18:21]=%v want %v", bakBytes[18:21], beforeBytes[18:21])
		}
		for _, suffix := range []string{"-shm", "-wal", ".reserve-rebuild"} {
			if _, serr := os.Stat(mPath + suffix); !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf("stale sidecar %s remained after clean WAL rebuild: err=%v", suffix, serr)
			}
		}
	})

	t.Run("rename_failure_keeps_live_wal_file_byte_identical", func(t *testing.T) {
		dbDir, mPath, _ := setupWALReserve12Fixture(t)
		beforeBytes, err := os.ReadFile(mPath)
		if err != nil || len(beforeBytes) < 21 || beforeBytes[18] != 2 || beforeBytes[19] != 2 || beforeBytes[20] != 12 {
			t.Fatalf("unexpected WAL setup header: len=%d err=%v", len(beforeBytes), err)
		}
		origRename := reserveRename
		reserveRename = func(string, string) error { return syscall.EIO }
		t.Cleanup(func() { reserveRename = origRename })

		fixed, err := NormalizeDatabase2(dbDir)
		if !errors.Is(err, syscall.EIO) || len(fixed) != 0 {
			t.Fatalf("expected EIO rename failure, got fixed=%v err=%v", fixed, err)
		}
		afterBytes, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterBytes, beforeBytes) {
			t.Fatalf("live WAL file mutated on rename failure: got bytes[18:21]=%v want %v", afterBytes[18:21], beforeBytes[18:21])
		}
		bakBytes, err := os.ReadFile(mPath + ".pre-reserve")
		if err != nil || !bytes.Equal(bakBytes, beforeBytes) {
			t.Fatalf("WAL .pre-reserve mismatch on rename failure: err=%v", err)
		}
		if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf(".reserve-rebuild left after rename failure: err=%v", serr)
		}
	})
}

func TestNormalizeReserveCancelBeforeRenameStopsPublication(t *testing.T) {
	setupMode := func(t *testing.T, wal bool) string {
		t.Helper()
		if wal {
			_, mPath, _ := setupWALReserve12Fixture(t)
			return mPath
		}
		return reserve12Library(t)
	}
	for _, tc := range []struct {
		name string
		wal  bool
	}{
		{name: "delete_mode", wal: false},
		{name: "wal_mode", wal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mPath := setupMode(t, tc.wal)
			beforeBytes, err := os.ReadFile(mPath)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			reserveBeforeRename = func(p string) {
				if p == mPath {
					cancel()
				}
			}
			t.Cleanup(func() { reserveBeforeRename = nil })

			fixed, err := NormalizeReserveContext(ctx, mPath)
			if fixed || !errors.Is(err, context.Canceled) {
				t.Fatalf("expected fixed=false, Canceled; got fixed=%v err=%v", fixed, err)
			}
			afterBytes, err := os.ReadFile(mPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(afterBytes, beforeBytes) {
				t.Fatalf("canceled pre-rename modified live DB: got bytes[18:21]=%v want %v", afterBytes[18:21], beforeBytes[18:21])
			}
			if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
				t.Fatalf("reserve changed=%d ok=%v err=%v", r, ok, rerr)
			}
			if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf(".reserve-rebuild left after pre-rename cancel: err=%v", serr)
			}
		})
	}
}

func TestPOSIXFallbackPreservesNewOwnerLockDuringDelayedDrain(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX fcntl close lock drop is Linux/Unix specific")
	}
	if os.Getenv("ENGINEX_RESERVE_POSIX_DRAIN_TEST") == "1" {
		_, err := sqlite.OFDLocking(false)
		if err != nil || sqlite.OFDLockingEnabled() {
			t.Fatalf("failed to disable OFD locking: enabled=%v err=%v", sqlite.OFDLockingEnabled(), err)
		}
		for _, forceCopy := range []bool{false, true} {
			name := "hardlink"
			if forceCopy {
				name = "copy_fallback"
			}
			t.Run(name, func(t *testing.T) {
				dbDir := filepath.Join(t.TempDir(), "Engine Library", "Database2")
				if err := os.MkdirAll(dbDir, 0o755); err != nil {
					t.Fatal(err)
				}
				mPath := filepath.Join(dbDir, "m.db")
				seed, err := os.ReadFile(reserve12Library(t))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(mPath, seed, 0o644); err != nil {
					t.Fatal(err)
				}
				if forceCopy {
					origLink := reserveLink
					reserveLink = func(string, string) error { return syscall.EXDEV }
					defer func() { reserveLink = origLink }()
				}

				type reserveResult struct {
					r   byte
					ok  bool
					err error
				}
				type normResult struct {
					fixed []string
					err   error
				}
				headerPaused := make(chan struct{})
				headerRelease := make(chan struct{})
				beforeRenameReached := make(chan struct{})
				reserveDone := make(chan reserveResult, 1)
				normDone := make(chan normResult, 1)
				var overlapArmed atomic.Bool
				var overlapStarted atomic.Bool
				var beforeOnce atomic.Bool

				origAfterHeader := reserveAfterHeaderRead
				reserveAfterHeaderRead = func(p string) {
					if overlapArmed.Load() && p == mPath && overlapStarted.CompareAndSwap(false, true) {
						close(headerPaused)
						<-headerRelease
					}
				}
				defer func() { reserveAfterHeaderRead = origAfterHeader }()

				origBeforeRename := reserveBeforeRename
				reserveBeforeRename = func(p string) {
					if p != mPath || !beforeOnce.CompareAndSwap(false, true) {
						return
					}
					overlapArmed.Store(true)
					go func() {
						r, ok, rerr := Reserve(mPath)
						reserveDone <- reserveResult{r: r, ok: ok, err: rerr}
					}()
					<-headerPaused
					close(beforeRenameReached)
				}
				defer func() { reserveBeforeRename = origBeforeRename }()

				origRename := reserveRename
				reserveRename = func(string, string) error { return syscall.EIO }
				defer func() { reserveRename = origRename }()

				go func() {
					fixed, nerr := NormalizeDatabase2Context(context.Background(), dbDir)
					normDone <- normResult{fixed: fixed, err: nerr}
				}()
				<-beforeRenameReached

				owner2, err := sql.Open("sqlite", mPath)
				if err != nil {
					close(headerRelease)
					t.Fatal(err)
				}
				owner2.SetMaxOpenConns(1)
				defer owner2.Close()

				// Acquire BEGIN EXCLUSIVE as soon as normalization releases its lock,
				// without waiting on normDone before releasing the overlapping probe.
				lockDeadline := time.Now().Add(3 * time.Second)
				for {
					if _, execErr := owner2.Exec("BEGIN EXCLUSIVE"); execErr == nil {
						break
					} else if !time.Now().Before(lockDeadline) {
						close(headerRelease)
						t.Fatalf("owner2 did not acquire BEGIN EXCLUSIVE after EIO rollback: %v", execErr)
					}
					time.Sleep(10 * time.Millisecond)
				}
				attemptConcurrentExternalWriteBusy(t, mPath, "posix_before_overlap_release_"+name)

				// Release the overlapping Reserve(mPath) while owner2 holds BEGIN EXCLUSIVE.
				close(headerRelease)
				res := <-reserveDone
				if res.err != nil || !res.ok || res.r != 12 {
					t.Fatalf("overlapping POSIX Reserve: r=%d ok=%v err=%v", res.r, res.ok, res.err)
				}
				nres := <-normDone
				if !errors.Is(nres.err, syscall.EIO) || len(nres.fixed) != 0 {
					t.Fatalf("expected EIO rename failure, got fixed=%v err=%v", nres.fixed, nres.err)
				}

				attemptConcurrentExternalWriteBusy(t, mPath, "posix_after_overlap_release_"+name)
				if _, err := owner2.Exec("UPDATE Information SET uuid='second-owner-ok'"); err != nil {
					t.Fatal(err)
				}
				if _, err := owner2.Exec("COMMIT"); err != nil {
					t.Fatal(err)
				}
				if err := owner2.Close(); err != nil {
					t.Fatal(err)
				}
				chk, err := Open(mPath, true)
				if err != nil {
					t.Fatal(err)
				}
				var gotUUID string
				if qerr := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); qerr != nil {
					chk.Close()
					t.Fatal(qerr)
				}
				chk.Close()
				if gotUUID != "second-owner-ok" {
					t.Fatalf("uuid=%q want second-owner-ok", gotUUID)
				}
			})
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXFallbackPreservesNewOwnerLockDuringDelayedDrain$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_POSIX_DRAIN_TEST=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("POSIX drain child failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestPOSIXFallbackPreservesNewOwnerLockDuringDelayedDrain")) {
		t.Fatalf("POSIX drain child did not pass:\n%s", string(out))
	}
}

func TestNormalizeReservePreservesRowIDAcrossQuirksAndWidth(t *testing.T) {
	t.Run("int_pk_comment_desc", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE TDesc(id INTEGER PRIMARY KEY /* app */ DESC, name TEXT);
			CREATE TABLE TDescRef(targetRowID INTEGER);
			INSERT INTO TDesc(rowid, id, name) VALUES(41, 1, 'a'),(99, 2, 'b');
			INSERT INTO TDescRef(targetRowID) VALUES(41),(99);`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var matches int
		if err := out.QueryRow(`SELECT count(*) FROM TDesc d JOIN TDescRef r ON d._rowid_=r.targetRowID`).Scan(&matches); err != nil || matches != 2 {
			t.Fatalf("TDesc rowid join matches=%d want 2 err=%v", matches, err)
		}
	})

	t.Run("default_without_rowid_string", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE TWoRStr(id INTEGER, note TEXT DEFAULT 'WITHOUT    ROWID');
			INSERT INTO TWoRStr(rowid, id, note) VALUES(41, 1, 'a'),(99, 2, 'b');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(rowid||':'||id, ',') FROM (SELECT rowid, id FROM TWoRStr ORDER BY rowid)`).Scan(&got); err != nil || got != "41:1,99:2" {
			t.Fatalf("TWoRStr rowids=%q want 41:1,99:2 err=%v", got, err)
		}
	})

	t.Run("true_without_rowid", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE TTrueWoR(id INTEGER PRIMARY KEY, note TEXT) WITHOUT ROWID;
			INSERT INTO TTrueWoR(id, note) VALUES(10, 'x'),(20, 'y');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(id||note, ',') FROM (SELECT id, note FROM TTrueWoR ORDER BY id)`).Scan(&got); err != nil || got != "10x,20y" {
			t.Fatalf("TTrueWoR=%q want 10x,20y err=%v", got, err)
		}
	})

	t.Run("literal_rowid_column", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE TLit(rowid TEXT, note TEXT);
			INSERT INTO TLit(_rowid_, rowid, note) VALUES(41, 'user-r1', 'n1'),(99, 'user-r2', 'n2');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(_rowid_||':'||rowid||':'||note, ',') FROM (SELECT _rowid_, rowid, note FROM TLit ORDER BY _rowid_)`).Scan(&got); err != nil || got != "41:user-r1:n1,99:user-r2:n2" {
			t.Fatalf("TLit=%q want 41:user-r1:n1,99:user-r2:n2 err=%v", got, err)
		}
	})

	t.Run("2000_cols_default_pk_desc", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		colDefs := make([]string, 2000)
		colDefs[0] = "id INTEGER PRIMARY KEY"
		for i := 1; i < 1999; i++ {
			colDefs[i] = fmt.Sprintf("c%d TEXT", i)
		}
		colDefs[1999] = "c1999 TEXT DEFAULT 'PRIMARY KEY DESC'"
		mustExec(t, db, "CREATE TABLE W2000PKDesc("+strings.Join(colDefs, ",")+")")
		mustExec(t, db, "INSERT INTO W2000PKDesc(id,c1) VALUES(5,'x5'),(42,'x42')")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var c1, c1999 string
		if err := out.QueryRow("SELECT c1, c1999 FROM W2000PKDesc WHERE id=42").Scan(&c1, &c1999); err != nil {
			t.Fatal(err)
		}
		if c1 != "x42" || c1999 != "PRIMARY KEY DESC" {
			t.Fatalf("W2000PKDesc=(%q,%q) want (x42,PRIMARY KEY DESC)", c1, c1999)
		}
	})

	t.Run("2000_cols_hidden_rowid", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		colDefs := make([]string, 2000)
		for i := 0; i < 1999; i++ {
			colDefs[i] = fmt.Sprintf("c%d TEXT", i)
		}
		colDefs[1999] = "c1999 TEXT NOT NULL"
		mustExec(t, db, "CREATE TABLE W2000Hidden("+strings.Join(colDefs, ",")+")")
		mustExec(t, db, "INSERT INTO W2000Hidden(rowid,c0,c1999) VALUES(41,'a0','a1999'),(99,'b0','b1999')")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve on 2000-col hidden-rowid table = %v, %v", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(_rowid_||':'||c0||':'||c1999, ',') FROM (SELECT _rowid_, c0, c1999 FROM W2000Hidden ORDER BY _rowid_)`).Scan(&got); err != nil || got != "41:a0:a1999,99:b0:b1999" {
			t.Fatalf("W2000Hidden=%q want 41:a0:a1999,99:b0:b1999 err=%v", got, err)
		}
	})
}

func TestNormalizeReserveContextTimeoutDuringIsolatedHeaderRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX/FIFO header test is Linux/Unix specific")
	}
	if os.Getenv("ENGINEX_RESERVE_SLOW_DD_TEST") == "1" {
		_, err := sqlite.OFDLocking(false)
		if err != nil || sqlite.OFDLockingEnabled() {
			t.Fatalf("failed to disable OFD locking: enabled=%v err=%v", sqlite.OFDLockingEnabled(), err)
		}
		path := filepath.Join(t.TempDir(), "zero.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, "PRAGMA user_version=4194305")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		binDir := t.TempDir()
		realDD, err := exec.LookPath("dd")
		if err != nil {
			t.Fatal(err)
		}
		wrapper := filepath.Join(binDir, "dd")
		script := fmt.Sprintf("#!/bin/sh\nsleep 0.35\nexec %q \"$@\"\n", realDD)
		if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		fixed, rerr := NormalizeReserveContext(ctx, path)
		elapsed := time.Since(start)
		if fixed || !errors.Is(rerr, context.DeadlineExceeded) {
			t.Fatalf("expected fixed=false, DeadlineExceeded; got fixed=%v err=%v", fixed, rerr)
		}
		if elapsed >= 200*time.Millisecond {
			t.Fatalf("NormalizeReserveContext waited %v for slow dd (>= 200ms)", elapsed)
		}
		return
	}

	t.Run("named_pipe_immediate_espipe", func(t *testing.T) {
		fifo := filepath.Join(t.TempDir(), "m.db")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		fixed, err := NormalizeReserveContext(ctx, fifo)
		elapsed := time.Since(start)
		if fixed || !errors.Is(err, syscall.ESPIPE) {
			t.Fatalf("expected fixed=false, ESPIPE on FIFO; got fixed=%v err=%v", fixed, err)
		}
		if elapsed >= 150*time.Millisecond {
			t.Fatalf("FIFO check took %v (>= 150ms)", elapsed)
		}
	})

	t.Run("slow_isolated_dd_honors_deadline", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeReserveContextTimeoutDuringIsolatedHeaderRead$", "-test.v")
		cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_SLOW_DD_TEST=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("slow dd child failed: %v\n%s", err, string(out))
		}
		if !bytes.Contains(out, []byte("--- PASS: TestNormalizeReserveContextTimeoutDuringIsolatedHeaderRead")) {
			t.Fatalf("slow dd child did not pass:\n%s", string(out))
		}
	})
}

func TestNormalizeReserveWALSidecarRemovalFailureAbortsPublication(t *testing.T) {
	dbDir, mPath, _ := setupWALReserve12Fixture(t)
	beforeBytes, err := os.ReadFile(mPath)
	if err != nil || len(beforeBytes) < 21 || beforeBytes[18] != 2 || beforeBytes[19] != 2 || beforeBytes[20] != 12 {
		t.Fatalf("unexpected WAL setup header: len=%d err=%v", len(beforeBytes), err)
	}
	shmDir := mPath + "-shm"
	_ = os.RemoveAll(shmDir)
	if err := os.MkdirAll(filepath.Join(shmDir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}

	fixed, err := NormalizeDatabase2(dbDir)
	if err == nil || len(fixed) != 0 {
		t.Fatalf("expected WAL sidecar cleanup error, got fixed=%v err=%v", fixed, err)
	}
	afterBytes, rerr := os.ReadFile(mPath)
	if rerr != nil || !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatalf("live WAL file mutated on -shm cleanup failure: err=%v", rerr)
	}
	if r, ok, rr := Reserve(mPath); rr != nil || !ok || r != 12 {
		t.Fatalf("reserve changed on -shm cleanup failure: r=%d ok=%v err=%v", r, ok, rr)
	}
	if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf(".reserve-rebuild left after -shm cleanup failure: err=%v", serr)
	}
}

func TestNormalizeReserveHeaderReadSafeAcrossFirstPOSIXFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX fcntl lock fallback test is Linux specific")
	}
	if zeroDB := os.Getenv("ENGINEX_RESERVE_FIRST_FALLBACK_DB"); zeroDB != "" {
		if !sqlite.OFDLockingEnabled() {
			t.Fatalf("expected OFDLockingEnabled()=true before first SQLite connection")
		}
		paused := make(chan struct{})
		release := make(chan struct{})
		var pausedOnce atomic.Bool

		origAfter := reserveAfterHeaderRead
		reserveAfterHeaderRead = func(p string) {
			if p == zeroDB && pausedOnce.CompareAndSwap(false, true) {
				close(paused)
				<-release
			}
		}
		defer func() { reserveAfterHeaderRead = origAfter }()

		type normRes struct {
			fixed bool
			err   error
		}
		done := make(chan normRes, 1)
		go func() {
			fixed, err := NormalizeReserveContext(context.Background(), zeroDB)
			done <- normRes{fixed: fixed, err: err}
		}()
		<-paused

		// Simulate the driver's first lock attempt falling back to POSIX F_SETLK.
		if _, err := sqlite.OFDLocking(false); err != nil {
			close(release)
			t.Fatal(err)
		}
		owner2, err := sql.Open("sqlite", zeroDB)
		if err != nil {
			close(release)
			t.Fatal(err)
		}
		owner2.SetMaxOpenConns(1)
		defer owner2.Close()
		if _, err := owner2.Exec("BEGIN EXCLUSIVE"); err != nil {
			close(release)
			t.Fatal(err)
		}
		attemptConcurrentExternalWriteBusy(t, zeroDB, "before_pre_fallback_header_return")

		close(release)
		res := <-done
		if res.err != nil || res.fixed {
			t.Fatalf("NormalizeReserveContext on reserve-0 DB = (%v, %v), want (false, nil)", res.fixed, res.err)
		}

		// Returning from the earlier header read must not drop owner2's POSIX lock.
		attemptConcurrentExternalWriteBusy(t, zeroDB, "after_pre_fallback_header_return")
		if _, err := owner2.Exec("UPDATE Information SET uuid='first-fallback-ok'"); err != nil {
			t.Fatal(err)
		}
		if _, err := owner2.Exec("COMMIT"); err != nil {
			t.Fatal(err)
		}
		if err := owner2.Close(); err != nil {
			t.Fatal(err)
		}
		chk, err := Open(zeroDB, true)
		if err != nil {
			t.Fatal(err)
		}
		defer chk.Close()
		var gotUUID string
		if err := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); err != nil {
			t.Fatal(err)
		}
		if gotUUID != "first-fallback-ok" {
			t.Fatalf("uuid=%q after commit under POSIX fallback, want first-fallback-ok", gotUUID)
		}
		return
	}

	_, mount, db := testlibrary.Create(t)
	db.Close()
	zeroDB := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeReserveHeaderReadSafeAcrossFirstPOSIXFallback$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_FIRST_FALLBACK_DB="+zeroDB)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("first POSIX fallback child failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestNormalizeReserveHeaderReadSafeAcrossFirstPOSIXFallback")) {
		t.Fatalf("first POSIX fallback child did not pass:\n%s", string(out))
	}
}

func TestNormalizeReservePreservesRowIDWithUnicodeDottedI(t *testing.T) {
	path := reserve12Library(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, `CREATE TABLE AuxDotted("rowİd" TEXT, _rowid_ TEXT, oid TEXT, name TEXT);
		CREATE TABLE AuxDottedRef(aux_rid INTEGER, label TEXT);
		INSERT INTO AuxDotted(rowid, "rowİd", _rowid_, oid, name) VALUES(41, 'u1', 'u2', 'u3', 'alpha'),(99, 'v1', 'v2', 'v3', 'beta');
		INSERT INTO AuxDottedRef(aux_rid, label) VALUES(41, 'first'),(99, 'second');`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fixed, err := NormalizeReserve(path)
	if err != nil || !fixed {
		t.Fatalf("NormalizeReserve = (%v, %v), want (true, nil)", fixed, err)
	}
	if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
		t.Fatalf("post-rebuild Reserve = (%d, %v, %v), want (0, true, nil)", r, ok, rerr)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var matches int
	if err := out.QueryRow(`SELECT count(*) FROM AuxDottedRef r JOIN AuxDotted a ON a.rowid = r.aux_rid`).Scan(&matches); err != nil || matches != 2 {
		t.Fatalf("AuxDotted rowid join matches=%d want 2 err=%v", matches, err)
	}
	var got string
	if err := out.QueryRow(`SELECT group_concat(a.rowid||':'||a."rowİd"||':'||a.name, ',') FROM (SELECT rowid, "rowİd", name FROM AuxDotted ORDER BY rowid) a`).Scan(&got); err != nil || got != "41:u1:alpha,99:v1:beta" {
		t.Fatalf("AuxDotted rows=%q want 41:u1:alpha,99:v1:beta err=%v", got, err)
	}
}

func TestNormalizeReserveGeneratedRowidColumn(t *testing.T) {
	t.Run("without_rowid_generated_rowid", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE WrGenRowID(k TEXT PRIMARY KEY, note TEXT, rowid INT GENERATED ALWAYS AS (length(k)) STORED) WITHOUT ROWID;
			INSERT INTO WrGenRowID(k, note) VALUES('alpha', 'a'),('beta22', 'b');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = (%v, %v), want (true, nil)", fixed, err)
		}
		if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
			t.Fatalf("Reserve = (%d, %v, %v), want (0, true, nil)", r, ok, rerr)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(k||':'||note||':'||rowid, ',') FROM (SELECT k, note, rowid FROM WrGenRowID ORDER BY k)`).Scan(&got); err != nil || got != "alpha:a:5,beta22:b:6" {
			t.Fatalf("WrGenRowID=%q want alpha:a:5,beta22:b:6 err=%v", got, err)
		}
	})

	t.Run("rowid_table_generated_rowid", func(t *testing.T) {
		path := reserve12Library(t)
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		mustExec(t, db, `CREATE TABLE RtGenRowID(note TEXT, rowid INT GENERATED ALWAYS AS (length(note)) STORED);
			INSERT INTO RtGenRowID(_rowid_, note) VALUES(41, 'alpha'),(99, 'hi');`)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if fixed, err := NormalizeReserve(path); err != nil || !fixed {
			t.Fatalf("NormalizeReserve = (%v, %v), want (true, nil)", fixed, err)
		}
		out, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		var got string
		if err := out.QueryRow(`SELECT group_concat(_rowid_||':'||note||':'||rowid, ',') FROM (SELECT _rowid_, note, rowid FROM RtGenRowID ORDER BY _rowid_)`).Scan(&got); err != nil || got != "41:alpha:5,99:hi:2" {
			t.Fatalf("RtGenRowID=%q want 41:alpha:5,99:hi:2 err=%v", got, err)
		}
	})
}

func TestNormalizeReserve2000ColsStreamingTailBlobs(t *testing.T) {
	path := reserve12Library(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	colDefs := make([]string, 2000)
	colDefs[0] = "c0 INTEGER"
	for i := 1; i < 1999; i++ {
		colDefs[i] = fmt.Sprintf("c%d TEXT", i)
	}
	colDefs[1999] = "c1999 BLOB NOT NULL"
	mustExec(t, db, "CREATE TABLE W2000Blobs("+strings.Join(colDefs, ",")+")")
	stmt, err := db.Prepare("INSERT INTO W2000Blobs(rowid, c0, c1999) VALUES(?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	wantRowids := make([]int64, 16)
	wantBlobs := make([][]byte, 16)
	for i := range wantRowids {
		rid := int64(41 + i*37)
		wantRowids[i] = rid
		payload := bytes.Repeat([]byte{byte(i + 17), byte(250 - i)}, 8<<10)
		wantBlobs[i] = payload
		if _, err := stmt.Exec(rid, i*10, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if fixed, err := NormalizeReserve(path); err != nil || !fixed {
		t.Fatalf("NormalizeReserve = (%v, %v), want (true, nil)", fixed, err)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	rows, err := out.Query("SELECT _rowid_, c0, c1999 FROM W2000Blobs ORDER BY _rowid_")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	idx := 0
	for rows.Next() {
		var rid, c0 int64
		var got []byte
		if err := rows.Scan(&rid, &c0, &got); err != nil {
			t.Fatal(err)
		}
		if idx >= len(wantRowids) || rid != wantRowids[idx] || c0 != int64(idx*10) || !bytes.Equal(got, wantBlobs[idx]) {
			t.Fatalf("row %d mismatch: rid=%d (want %d) c0=%d blobEq=%v", idx, rid, wantRowids[idx], c0, bytes.Equal(got, wantBlobs[idx]))
		}
		idx++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if idx != len(wantRowids) {
		t.Fatalf("copied %d rows, want %d", idx, len(wantRowids))
	}
}

func setupFilteredSlowDD(t *testing.T, targetSuffix, delaySec, markerPath string) {
	t.Helper()
	realDD, err := exec.LookPath("dd")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "dd")
	script := fmt.Sprintf(`#!/bin/sh
target=$(readlink /proc/$$/fd/0 2>/dev/null || true)
case "$target" in
  *"$ENGINEX_SLOW_DD_TARGET")
    : > "$ENGINEX_SLOW_DD_MARKER"
    sleep "$ENGINEX_SLOW_DD_DELAY"
    ;;
esac
exec %q "$@"
`, realDD)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENGINEX_SLOW_DD_TARGET", targetSuffix)
	t.Setenv("ENGINEX_SLOW_DD_DELAY", delaySec)
	t.Setenv("ENGINEX_SLOW_DD_MARKER", markerPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func createCleanMountWithSibling(t *testing.T) (mount, dbDir, mPath, hmPath string) {
	t.Helper()
	_, mount, db := testlibrary.Create(t)
	db.Close()
	dbDir = filepath.Join(mount, "Engine Library", "Database2")
	mPath = filepath.Join(dbDir, "m.db")
	hmPath = filepath.Join(dbDir, "hm.db")
	hdb, err := sql.Open("sqlite", hmPath)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	schema, err := os.ReadFile("../../docs/re-report/ENGINE-5.0.4-hm.db-schema.sql")
	if err != nil {
		hdb.Close()
		t.Fatal(err)
	}
	mustExec(t, hdb, strings.ReplaceAll(string(schema), "CREATE TABLE sqlite_sequence(name,seq);", ""))
	mustExec(t, hdb, "INSERT INTO Information(id,uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(1,'hist-clean',3,0,2)")
	if err := hdb.Close(); err != nil {
		t.Fatal(err)
	}
	return mount, dbDir, mPath, hmPath
}

func waitForSlowDDMarkerOrDone[T any](t *testing.T, marker string, done <-chan T) time.Time {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return time.Now()
		}
		select {
		case res := <-done:
			t.Fatalf("call returned early before slow dd marker appeared: %+v", res)
		case <-time.After(2 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for slow dd marker %s", marker)
	return time.Time{}
}

func TestMountAndDirectoryScansHonorCancellationDuringSlowHeaderRead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX/procfs dd wrapper test is Linux specific")
	}
	if sub := os.Getenv("ENGINEX_RESERVE_SLOW_SCAN_SUBTEST"); sub != "" {
		_, err := sqlite.OFDLocking(false)
		if err != nil || sqlite.OFDLockingEnabled() {
			t.Fatalf("failed to disable OFD locking: enabled=%v err=%v", sqlite.OFDLockingEnabled(), err)
		}
		marker := filepath.Join(t.TempDir(), "dd.started")
		mount, dbDir, mPath, _ := createCleanMountWithSibling(t)
		target := "/hm.db"
		if sub == "read_only_mount_live_cancel" {
			target = "/m.db"
			if err := os.Chmod(mPath, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(mPath, 0o644) })
		}
		setupFilteredSlowDD(t, target, "0.95", marker)

		switch sub {
		case "normalize_database2_live_cancel":
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type resT struct {
				fixed []string
				err   error
			}
			done := make(chan resT, 1)
			go func() {
				fixed, err := NormalizeDatabase2Context(ctx, dbDir)
				done <- resT{fixed: fixed, err: err}
			}()
			waitForSlowDDMarkerOrDone(t, marker, done)
			cAt := time.Now()
			cancel()
			res := <-done
			if elapsed := time.Since(cAt); elapsed >= 250*time.Millisecond {
				t.Fatalf("NormalizeDatabase2Context took %v after cancel (>= 250ms)", elapsed)
			}
			if !errors.Is(res.err, context.Canceled) {
				t.Fatalf("NormalizeDatabase2Context err=%v, want context.Canceled", res.err)
			}

		case "full_mount_heal_live_cancel", "data_only_mount_heal_live_cancel", "read_only_mount_live_cancel":
			dataOnly := sub == "data_only_mount_heal_live_cancel"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type resT struct {
				rep Report
				err error
			}
			done := make(chan resT, 1)
			go func() {
				rep, err := HealMountWithOptions(ctx, mount, MountOptions{DataOnly: dataOnly})
				done <- resT{rep: rep, err: err}
			}()
			waitForSlowDDMarkerOrDone(t, marker, done)
			cAt := time.Now()
			cancel()
			res := <-done
			if elapsed := time.Since(cAt); elapsed >= 250*time.Millisecond {
				t.Fatalf("%s took %v after cancel (>= 250ms)", sub, elapsed)
			}
			if res.rep.Healthy || !errors.Is(res.err, context.Canceled) {
				t.Fatalf("%s = (Healthy=%v, err=%v), want (false, context.Canceled)", sub, res.rep.Healthy, res.err)
			}

		case "data_only_already_canceled_bounded":
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			start := time.Now()
			rep, err := HealMountDataOnlyContext(canceled, mount, 0)
			elapsed := time.Since(start)
			if elapsed >= 750*time.Millisecond {
				t.Fatalf("already-canceled data-only waited %v (>= 750ms)", elapsed)
			}
			if rep.Healthy || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("already-canceled data-only with slow sibling = (Healthy=%v, err=%v), want (false, context.DeadlineExceeded)", rep.Healthy, err)
			}

		default:
			t.Fatalf("unknown subtest %q", sub)
		}
		return
	}

	for _, sub := range []string{
		"normalize_database2_live_cancel",
		"full_mount_heal_live_cancel",
		"data_only_mount_heal_live_cancel",
		"data_only_already_canceled_bounded",
		"read_only_mount_live_cancel",
	} {
		t.Run(sub, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMountAndDirectoryScansHonorCancellationDuringSlowHeaderRead$", "-test.v")
			cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_SLOW_SCAN_SUBTEST="+sub)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s subprocess failed: %v\n%s", sub, err, string(out))
			}
			if !bytes.Contains(out, []byte("--- PASS: TestMountAndDirectoryScansHonorCancellationDuringSlowHeaderRead")) {
				t.Fatalf("%s subprocess did not pass:\n%s", sub, string(out))
			}
		})
	}
}

func TestNormalizeReserveCancelDuringFinalHeaderVerifyPreservesErrorIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs dd wrapper test is Linux specific")
	}
	if os.Getenv("ENGINEX_RESERVE_FINAL_VERIFY_CANCEL") == "1" {
		mPath := reserve12Library(t)
		marker := filepath.Join(t.TempDir(), "verify.started")
		setupFilteredSlowDD(t, ".reserve-rebuild", "0.85", marker)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type resT struct {
			fixed bool
			err   error
		}
		done := make(chan resT, 1)
		go func() {
			fixed, err := NormalizeReserveContext(ctx, mPath)
			done <- resT{fixed: fixed, err: err}
		}()
		waitForSlowDDMarkerOrDone(t, marker, done)
		cAt := time.Now()
		cancel()
		res := <-done
		if elapsed := time.Since(cAt); elapsed >= 250*time.Millisecond {
			t.Fatalf("final header verify cancel took %v (>= 250ms)", elapsed)
		}
		if res.fixed || !errors.Is(res.err, context.Canceled) {
			t.Fatalf("NormalizeReserveContext = (%v, %v), want (false, errors.Is context.Canceled)", res.fixed, res.err)
		}
		if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf(".reserve-rebuild left after cancel: err=%v", serr)
		}
		if r, ok, rerr := Reserve(mPath); rerr != nil || !ok || r != 12 {
			t.Fatalf("live reserve changed after cancel: (%d, %v, %v)", r, ok, rerr)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestNormalizeReserveCancelDuringFinalHeaderVerifyPreservesErrorIdentity$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_FINAL_VERIFY_CANCEL=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("final verify cancel subprocess failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestNormalizeReserveCancelDuringFinalHeaderVerifyPreservesErrorIdentity")) {
		t.Fatalf("final verify cancel subprocess did not pass:\n%s", string(out))
	}
}

func TestNormalizeReserveSidecarObstructionsAbortPublication(t *testing.T) {
	t.Run("delete_mode_preexisting_nonempty_shm_dir", func(t *testing.T) {
		mPath := reserve12Library(t)
		beforeBytes, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(mPath+"-shm", "keep"), 0o755); err != nil {
			t.Fatal(err)
		}
		var renameCalls atomic.Int32
		origRename := reserveRename
		reserveRename = func(oldPath, newPath string) error {
			renameCalls.Add(1)
			return origRename(oldPath, newPath)
		}
		t.Cleanup(func() { reserveRename = origRename })

		fixed, err := NormalizeReserve(mPath)
		if err == nil || fixed {
			t.Fatalf("expected DELETE -shm dir rejection, got fixed=%v err=%v", fixed, err)
		}
		if renameCalls.Load() != 0 {
			t.Fatalf("expected 0 rename calls, got %d", renameCalls.Load())
		}
		afterBytes, rerr := os.ReadFile(mPath)
		if rerr != nil || !bytes.Equal(afterBytes, beforeBytes) {
			t.Fatalf("live DB bytes altered: err=%v", rerr)
		}
		if r, ok, rr := Reserve(mPath); rr != nil || !ok || r != 12 {
			t.Fatalf("reserve changed: (%d, %v, %v)", r, ok, rr)
		}
		if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf(".reserve-rebuild left after failure: err=%v", serr)
		}
	})

	t.Run("delete_mode_late_nonempty_shm_dir", func(t *testing.T) {
		mPath := reserve12Library(t)
		beforeBytes, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatal(err)
		}
		origBeforeRename := reserveBeforeRename
		reserveBeforeRename = func(p string) {
			if p == mPath {
				if mkErr := os.MkdirAll(filepath.Join(mPath+"-shm", "keep"), 0o755); mkErr != nil {
					t.Fatal(mkErr)
				}
			}
		}
		t.Cleanup(func() { reserveBeforeRename = origBeforeRename })

		var renameCalls atomic.Int32
		origRename := reserveRename
		reserveRename = func(oldPath, newPath string) error {
			renameCalls.Add(1)
			return origRename(oldPath, newPath)
		}
		t.Cleanup(func() { reserveRename = origRename })

		fixed, err := NormalizeReserve(mPath)
		if err == nil || fixed {
			t.Fatalf("expected late DELETE -shm dir rejection, got fixed=%v err=%v", fixed, err)
		}
		if renameCalls.Load() != 0 {
			t.Fatalf("expected 0 rename calls, got %d", renameCalls.Load())
		}
		afterBytes, rerr := os.ReadFile(mPath)
		if rerr != nil || !bytes.Equal(afterBytes, beforeBytes) {
			t.Fatalf("live DB bytes altered: err=%v", rerr)
		}
		if r, ok, rr := Reserve(mPath); rr != nil || !ok || r != 12 {
			t.Fatalf("reserve changed: (%d, %v, %v)", r, ok, rr)
		}
		if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf(".reserve-rebuild left after failure: err=%v", serr)
		}
	})

	t.Run("wal_mode_late_dangling_wal_symlink", func(t *testing.T) {
		_, mPath, _ := setupWALReserve12Fixture(t)
		beforeBytes, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatal(err)
		}
		origBeforeRename := reserveBeforeRename
		reserveBeforeRename = func(p string) {
			if p != mPath {
				return
			}
			_ = os.Remove(mPath + "-wal")
			if symErr := os.Symlink("/nonexistent-wal-target", mPath+"-wal"); symErr != nil {
				t.Fatal(symErr)
			}
		}
		t.Cleanup(func() { reserveBeforeRename = origBeforeRename })

		var renameCalls atomic.Int32
		origRename := reserveRename
		reserveRename = func(oldPath, newPath string) error {
			renameCalls.Add(1)
			return origRename(oldPath, newPath)
		}
		t.Cleanup(func() { reserveRename = origRename })

		fixed, err := NormalizeReserve(mPath)
		if err == nil || fixed {
			t.Fatalf("expected late dangling -wal symlink rejection, got fixed=%v err=%v", fixed, err)
		}
		if renameCalls.Load() != 0 {
			t.Fatalf("expected 0 rename calls, got %d", renameCalls.Load())
		}
		afterBytes, rerr := os.ReadFile(mPath)
		if rerr != nil || !bytes.Equal(afterBytes, beforeBytes) {
			t.Fatalf("live WAL bytes altered: err=%v", rerr)
		}
		if r, ok, rr := Reserve(mPath); rr != nil || !ok || r != 12 {
			t.Fatalf("reserve changed: (%d, %v, %v)", r, ok, rr)
		}
		if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf(".reserve-rebuild left after failure: err=%v", serr)
		}
	})
}

func TestPOSIXFallbackOPathPinClosePreservesLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("O_PATH/POSIX fcntl lock behavior is Linux specific")
	}
	if mPath := os.Getenv("ENGINEX_RESERVE_OPATH_PIN_DB"); mPath != "" {
		_, err := sqlite.OFDLocking(false)
		if err != nil || sqlite.OFDLockingEnabled() {
			t.Fatalf("failed to disable OFD locking: enabled=%v err=%v", sqlite.OFDLockingEnabled(), err)
		}
		owner, err := sql.Open("sqlite", mPath)
		if err != nil {
			t.Fatal(err)
		}
		owner.SetMaxOpenConns(1)
		defer owner.Close()
		if _, err := owner.Exec("BEGIN EXCLUSIVE"); err != nil {
			t.Fatal(err)
		}
		attemptConcurrentExternalWriteBusy(t, mPath, "before_opath_pin_close")

		pin, err := openInodePin(mPath)
		if err != nil || pin == nil {
			t.Fatalf("openInodePin(%s) = (%v, %v)", mPath, pin, err)
		}
		outFile, err := os.CreateTemp(t.TempDir(), "pin-copy-*")
		if err != nil {
			pin.Close()
			t.Fatal(err)
		}
		copyErr := copyFileFromPinIsolated(context.Background(), pin, mPath, outFile)
		_ = outFile.Close()
		closeErr := pin.Close()
		if copyErr != nil || closeErr != nil {
			t.Fatalf("copy/close pin failed: copy=%v close=%v", copyErr, closeErr)
		}

		attemptConcurrentExternalWriteBusy(t, mPath, "after_opath_pin_close")
		if _, err := owner.Exec("UPDATE Information SET uuid='opath-pin-ok'"); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec("COMMIT"); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		chk, err := Open(mPath, true)
		if err != nil {
			t.Fatal(err)
		}
		defer chk.Close()
		var gotUUID string
		if err := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); err != nil {
			t.Fatal(err)
		}
		if gotUUID != "opath-pin-ok" {
			t.Fatalf("uuid=%q want opath-pin-ok", gotUUID)
		}
		return
	}

	mPath := reserve12Library(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestPOSIXFallbackOPathPinClosePreservesLock$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_OPATH_PIN_DB="+mPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("O_PATH pin child failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestPOSIXFallbackOPathPinClosePreservesLock")) {
		t.Fatalf("O_PATH pin child did not pass:\n%s", string(out))
	}
}

func TestNormalizeReserveAbortsWhenLiveFileAtomicallyReplaced(t *testing.T) {
	for _, forceCopy := range []bool{false, true} {
		name := "hardlink"
		if forceCopy {
			name = "copy_fallback"
		}
		t.Run(name, func(t *testing.T) {
			mPath := reserve12Library(t)
			_, mount, incDB := testlibrary.Create(t)
			if _, err := incDB.Exec(`UPDATE Information SET uuid='incoming-zero-db'`); err != nil {
				t.Fatal(err)
			}
			incDB.Close()
			incoming := filepath.Join(mount, "Engine Library", "Database2", "m.db")
			incomingBytes, err := os.ReadFile(incoming)
			if err != nil {
				t.Fatal(err)
			}

			if forceCopy {
				origLink := reserveLink
				reserveLink = func(string, string) error { return syscall.EXDEV }
				defer func() { reserveLink = origLink }()
			}
			var renameCalls atomic.Int32
			origRename := reserveRename
			reserveRename = func(oldPath, newPath string) error {
				renameCalls.Add(1)
				return origRename(oldPath, newPath)
			}
			defer func() { reserveRename = origRename }()

			origBeforeRename := reserveBeforeRename
			reserveBeforeRename = func(p string) {
				if p == mPath {
					if rerr := os.Rename(incoming, mPath); rerr != nil {
						t.Fatal(rerr)
					}
				}
			}
			defer func() { reserveBeforeRename = origBeforeRename }()

			fixed, err := NormalizeReserve(mPath)
			if err == nil || fixed {
				t.Fatalf("expected abort when live file replaced, got fixed=%v err=%v", fixed, err)
			}
			if renameCalls.Load() != 0 {
				t.Fatalf("expected 0 rename calls, got %d", renameCalls.Load())
			}
			gotBytes, rerr := os.ReadFile(mPath)
			if rerr != nil || !bytes.Equal(gotBytes, incomingBytes) {
				t.Fatalf("incoming replacement file overwritten: err=%v", rerr)
			}
			if _, serr := os.Stat(mPath + ".reserve-rebuild"); !errors.Is(serr, os.ErrNotExist) {
				t.Fatalf(".reserve-rebuild left after replacement abort: err=%v", serr)
			}
		})
	}
}

func TestNormalizeReserveReplacementWALWriterKeepsExclusion(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("WAL shared memory exclusion test is Linux specific")
	}
	mPath := reserve12Library(t)
	_, mount, incDB := testlibrary.Create(t)
	incDB.Close()
	incoming := filepath.Join(mount, "Engine Library", "Database2", "m.db")

	var replDB *sql.DB
	var shmBefore os.FileInfo
	origBeforeRename := reserveBeforeRename
	reserveBeforeRename = func(p string) {
		if p != mPath {
			return
		}
		if err := os.Rename(incoming, mPath); err != nil {
			t.Fatal(err)
		}
		var err error
		replDB, err = sql.Open("sqlite", mPath)
		if err != nil {
			t.Fatal(err)
		}
		replDB.SetMaxOpenConns(1)
		mustExec(t, replDB, "PRAGMA journal_mode=WAL")
		mustExec(t, replDB, "UPDATE Information SET uuid='replacement-wal-init'")
		mustExec(t, replDB, "BEGIN IMMEDIATE")
		attemptConcurrentExternalWriteBusy(t, mPath, "replacement_wal_before_resume")
		shmBefore, err = os.Stat(mPath + "-shm")
		if err != nil {
			t.Fatal(err)
		}
		walInfo, werr := os.Stat(mPath + "-wal")
		if werr != nil || walInfo.Size() == 0 {
			t.Fatalf("expected nonempty replacement -wal: info=%v err=%v", walInfo, werr)
		}
	}
	defer func() {
		reserveBeforeRename = origBeforeRename
		if replDB != nil {
			_ = replDB.Close()
		}
	}()

	var renameCalls atomic.Int32
	origRename := reserveRename
	reserveRename = func(oldPath, newPath string) error {
		renameCalls.Add(1)
		return origRename(oldPath, newPath)
	}
	defer func() { reserveRename = origRename }()

	fixed, err := NormalizeReserve(mPath)
	if err == nil || fixed {
		t.Fatalf("expected NormalizeReserve to abort on replacement WAL DB, got fixed=%v err=%v", fixed, err)
	}
	if renameCalls.Load() != 0 {
		t.Fatalf("expected 0 rename calls, got %d", renameCalls.Load())
	}
	shmAfter, serr := os.Stat(mPath + "-shm")
	if serr != nil {
		t.Fatalf("replacement -shm removed on aborted rebuild: %v", serr)
	}
	if !os.SameFile(shmBefore, shmAfter) {
		t.Fatalf("replacement -shm inode changed during aborted rebuild")
	}
	attemptConcurrentExternalWriteBusy(t, mPath, "replacement_wal_after_abort")

	mustExec(t, replDB, "UPDATE Information SET uuid='replacement-wal-committed'")
	mustExec(t, replDB, "COMMIT")
	if err := replDB.Close(); err != nil {
		t.Fatal(err)
	}
	replDB = nil

	chk, err := Open(mPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer chk.Close()
	var gotUUID string
	if err := chk.QueryRow("SELECT uuid FROM Information").Scan(&gotUUID); err != nil {
		t.Fatal(err)
	}
	if gotUUID != "replacement-wal-committed" {
		t.Fatalf("uuid=%q want replacement-wal-committed", gotUUID)
	}
}

func TestNormalizeReserveCopyFallbackBacksUpOriginalInode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("O_PATH copy fallback inode preservation test is Linux specific")
	}
	mPath := reserve12Library(t)
	wantOrigBytes, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}

	_, mount, incDB := testlibrary.Create(t)
	if _, err := incDB.Exec(`UPDATE Information SET uuid='incoming-during-exdev'`); err != nil {
		t.Fatal(err)
	}
	incDB.Close()
	incoming := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	incomingBytes, err := os.ReadFile(incoming)
	if err != nil {
		t.Fatal(err)
	}

	origLink := reserveLink
	reserveLink = func(oldPath, newPath string) error {
		if oldPath == mPath {
			if rerr := os.Rename(incoming, mPath); rerr != nil {
				return rerr
			}
			return syscall.EXDEV
		}
		return origLink(oldPath, newPath)
	}
	defer func() { reserveLink = origLink }()

	origRename := reserveRename
	reserveRename = func(oldPath, newPath string) error {
		return syscall.EIO
	}
	defer func() { reserveRename = origRename }()

	fixed, err := NormalizeReserve(mPath)
	if err == nil || fixed {
		t.Fatalf("expected failure when live file replaced during EXDEV backup, got fixed=%v err=%v", fixed, err)
	}
	liveBytes, rerr := os.ReadFile(mPath)
	if rerr != nil || !bytes.Equal(liveBytes, incomingBytes) {
		t.Fatalf("live m.db altered from incoming replacement: err=%v", rerr)
	}
	preBytes, perr := os.ReadFile(mPath + ".pre-reserve")
	if perr != nil {
		t.Fatalf("missing .pre-reserve after EXDEV backup: %v", perr)
	}
	if !bytes.Equal(preBytes, wantOrigBytes) {
		t.Fatalf(".pre-reserve backed up wrong database (len=%d vs orig=%d incoming=%d)", len(preBytes), len(wantOrigBytes), len(incomingBytes))
	}
}

func TestNormalizeReserveTableNamedPragmaTableXinfo(t *testing.T) {
	path := reserve12Library(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	mustExec(t, db, `CREATE TABLE pragma_table_xinfo(k TEXT PRIMARY KEY, v TEXT);
		INSERT INTO pragma_table_xinfo(k, v) VALUES('a', 'xinfo-ok');
		CREATE TABLE pragma_table_info(k TEXT PRIMARY KEY, v TEXT);
		INSERT INTO pragma_table_info(k, v) VALUES('b', 'info-ok');`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	fixed, err := NormalizeReserve(path)
	if err != nil || !fixed {
		t.Fatalf("NormalizeReserve = (%v, %v), want (true, nil)", fixed, err)
	}
	if r, ok, rerr := Reserve(path); rerr != nil || !ok || r != 0 {
		t.Fatalf("Reserve = (%d, %v, %v), want (0, true, nil)", r, ok, rerr)
	}
	out, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var vx, vi string
	if err := out.QueryRow(`SELECT v FROM pragma_table_xinfo WHERE k='a'`).Scan(&vx); err != nil || vx != "xinfo-ok" {
		t.Fatalf("pragma_table_xinfo row=%q err=%v", vx, err)
	}
	if err := out.QueryRow(`SELECT v FROM pragma_table_info WHERE k='b'`).Scan(&vi); err != nil || vi != "info-ok" {
		t.Fatalf("pragma_table_info row=%q err=%v", vi, err)
	}
}

func TestReserveUnreadableFilePreservesPermissionError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX/unprivileged EACCES test is Linux specific")
	}
	if target := os.Getenv("ENGINEX_RESERVE_EACCES_PATH"); target != "" {
		if _, err := sqlite.OFDLocking(false); err != nil {
			t.Fatal(err)
		}
		r, ok, err := Reserve(target)
		if err == nil || ok || r != 0 {
			t.Fatalf("Reserve on unreadable file = (%d, %v, %v), want error", r, ok, err)
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("errors.Is(err, os.ErrPermission) = false for err=%v", err)
		}
		var pe *os.PathError
		if !errors.As(err, &pe) || pe.Path != target {
			t.Fatalf("errors.As(err, *os.PathError) = %v (%+v), want Path=%s", errors.As(err, &pe), pe, target)
		}
		return
	}

	dir, err := os.MkdirTemp("", "reserve-eacces-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "m.db")
	if err := os.WriteFile(target, []byte("not-readable"), 0o000); err != nil {
		t.Fatal(err)
	}

	binPath := os.Args[0]
	if os.Geteuid() == 0 {
		// go test places compiled test binaries under a 0700 cache dir; copy the
		// executable into the 0755 scratch dir so unprivileged nobody can exec it.
		rawBin, berr := os.ReadFile(os.Args[0])
		if berr != nil {
			t.Fatal(berr)
		}
		binPath = filepath.Join(dir, "dbhealer.test")
		if werr := os.WriteFile(binPath, rawBin, 0o755); werr != nil {
			t.Fatal(werr)
		}
	}
	cmd := exec.Command(binPath, "-test.run=^TestReserveUnreadableFilePreservesPermissionError$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINEX_RESERVE_EACCES_PATH="+target)
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: 65534, Gid: 65534},
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("EACCES child failed: %v\n%s", err, string(out))
	}
	if !bytes.Contains(out, []byte("--- PASS: TestReserveUnreadableFilePreservesPermissionError")) {
		t.Fatalf("EACCES child did not pass:\n%s", string(out))
	}
}
