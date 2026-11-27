package dbhealer

import (
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
