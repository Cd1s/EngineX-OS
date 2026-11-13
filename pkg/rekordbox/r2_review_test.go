package rekordbox

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// r2 review: the cue import loaded every cue row as a map of all its columns
// (375 MB VmHWM on armhf for 20000 tracks x 8 cues). It must stream only the
// columns it uses, so a wide unused column costs nothing.
func TestOneLibraryCueImportReadsOnlyUsedColumns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "exportLibrary.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT);
CREATE TABLE cue(cue_id INTEGER PRIMARY KEY, content_id INTEGER, kind INTEGER, inUsec INTEGER, outUsec INTEGER, cueComment TEXT, unusedPayload BLOB);
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<200) INSERT INTO content SELECT i, 'T'||i, '/Contents/t'||i||'.mp3' FROM n;
WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<19999) INSERT INTO cue SELECT i+1, i%200+1, (i/200)%9, (i/200)*1000000+500000, -1, 'c'||i, zeroblob(4096) FROM n;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	l, err := ReadSQLite(context.Background(), p, nil)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 40<<20 {
		t.Fatalf("cue import allocated %d MiB for 80 MiB of unused cue payload", got>>20)
	}
	cues := 0
	for _, tr := range l.Tracks {
		cues += len(tr.Cues)
		if tr.ID == 1 && (len(tr.Cues) == 0 || tr.Cues[0].TimeMS != 500) {
			t.Fatalf("track 1 cues: %+v", tr.Cues)
		}
	}
	if cues == 0 {
		t.Fatal("no cues imported")
	}
}

// r2 review: the plaintext copy went to /tmp (RAM tmpfs on the decks), had no
// space check, and a copy left by a SIGKILLed rb-bridge stayed forever.
func TestOneLibraryPlaintextCopyGoesToDataAndIsSwept(t *testing.T) {
	if MaxSQLCipherBytes != 256<<20 {
		t.Fatalf("MaxSQLCipherBytes = %d", MaxSQLCipherBytes)
	}
	dir := filepath.Join(t.TempDir(), "enginex", "tmp")
	oldDir, oldFree := oneLibraryTempDir, freeBytes
	t.Cleanup(func() { oneLibraryTempDir, freeBytes = oldDir, oldFree })
	oneLibraryTempDir = func() string { return dir }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, fmt.Sprintf("rb-onelibrary-%d-1.db", dead.Process.Pid))
	live := filepath.Join(dir, "rb-onelibrary-1-2.db") // PID 1 is always alive
	other := filepath.Join(dir, "unrelated.db")
	for _, p := range []string{stale, live, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src := "testdata/sqlcipher4_onelibrary.db"
	if _, err := ReadSQLite(context.Background(), src, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale plaintext copy of a dead rb-bridge not removed")
	}
	for _, p := range []string{live, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("temp dir mode: %v %v", st.Mode(), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("plaintext copy left behind: %v", entries)
	}
	// Not enough room on /data for the copy: refuse before writing anything.
	freeBytes = func(string) (uint64, error) { return 64 << 20, nil }
	if _, err := ReadSQLite(context.Background(), src, nil); err == nil || !strings.Contains(err.Error(), "needed") {
		t.Fatalf("decrypted without free space: %v", err)
	}
	if entries, _ = os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("file written despite the space check: %v", entries)
	}
}

func TestOneLibraryTempDirNeverUsesHostDataInTests(t *testing.T) {
	if got := oneLibraryTempDir(); strings.HasPrefix(got, "/data/") {
		t.Fatalf("oneLibraryTempDir() returned host %q during go test; must use os.TempDir()", got)
	}
	fakeData := filepath.Join(t.TempDir(), "enginex")
	if err := os.MkdirAll(fakeData, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := selectOneLibraryTempDir(fakeData, true); got == filepath.Join(fakeData, "tmp") {
		t.Fatalf("selectOneLibraryTempDir(inTest=true) used %q instead of os.TempDir()", got)
	}
	if got := selectOneLibraryTempDir(fakeData, false); got != filepath.Join(fakeData, "tmp") {
		t.Fatalf("selectOneLibraryTempDir(inTest=false) = %q, want %q", got, filepath.Join(fakeData, "tmp"))
	}
}

// The fallback was os.TempDir() itself, and prepareOneLibraryTempDir chmods its
// directory to 0700: every go test run as root turned the build host's /tmp
// to 0700, which broke the r4 release build (Engine runs as nobody there).
func TestOneLibraryTempDirLeavesSystemTempModeAlone(t *testing.T) {
	sysTmp := t.TempDir()
	if err := os.Chmod(sysTmp, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", sysTmp)
	for _, inTest := range []bool{true, false} {
		dir := selectOneLibraryTempDir(filepath.Join(sysTmp, "no-data"), inTest)
		if filepath.Dir(dir) != sysTmp {
			t.Fatalf("selectOneLibraryTempDir(inTest=%v) = %q, want a directory inside %q", inTest, dir, sysTmp)
		}
		if err := prepareOneLibraryTempDir(dir); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(sysTmp)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&(os.ModePerm|os.ModeSticky) != os.ModePerm|os.ModeSticky {
			t.Fatalf("system temp dir mode = %v after prepareOneLibraryTempDir, want 1777", st.Mode())
		}
	}
}
