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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
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

	setupWALReserve12 := func(t *testing.T) (string, string, uint64) {
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
		dbDir, mPath, beforeIno := setupWALReserve12(t)
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
		dbDir, mPath, beforeIno := setupWALReserve12(t)
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
