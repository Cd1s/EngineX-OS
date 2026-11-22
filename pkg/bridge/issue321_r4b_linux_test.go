package bridge_test

import (
	"bufio"
	"context"
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321 review R4b (c′): every file a pass writes on the stick to rename or remove later (a staged
// copy, its mark's temporary file, the first original's backup copy) has its directory synced
// after the file is created and before the file's own fsync. That order puts the volume's
// allocation of the file's clusters (exFAT's bitmap, the FAT) on the stick before the entry that
// names them, so a pull never leaves an entry naming clusters the volume does not give it, and
// the next insert can remove the file (R4 kept such files on exFAT; the next pass then shared
// their clusters and a disk check cut live files). A first pass on a OneLibrary stick runs under
// strace in a child process; the order of its fsync calls is checked.
func TestIssue321AStagedFilesDirectoryIsSyncedBeforeTheFile(t *testing.T) {
	if os.Getenv("ISSUE321_R4B_CHILD") == "1" {
		issue321FirstPassOnOneLibrary(t)
		return
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skipf("strace: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "trace.txt")
	cmd := exec.Command(strace, "-f", "-qq", "-y", "-e", "trace=openat,fsync", "-o", trace,
		os.Args[0], "-test.run", "^TestIssue321AStagedFilesDirectoryIsSyncedBeforeTheFile$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ISSUE321_R4B_CHILD=1", "TMPDIR="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the pass under strace: %v\n%s", err, out)
	}

	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	staged := regexp.MustCompile(`/\.(enginex-[^/]*|bak-[^/]*)\.tmp$`)
	created := regexp.MustCompile(`openat\([^"]*"([^"]+)", [^)]*O_CREAT`)
	synced := regexp.MustCompile(`fsync\(\d+<([^>]+)>`)
	createdAt := map[string]int{} // a staged file: the line that created it
	lastDirSync := map[string]int{}
	checked := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if m := created.FindStringSubmatch(line); m != nil && strings.HasPrefix(m[1], root) && staged.MatchString(m[1]) {
			if _, seen := createdAt[m[1]]; !seen {
				createdAt[m[1]] = n
			}
			continue
		}
		m := synced.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[1], root) {
			continue
		}
		p := m[1]
		if !staged.MatchString(p) {
			lastDirSync[p] = n // a directory, or a file the pass does not stage
			continue
		}
		c, ok := createdAt[p]
		if !ok || checked[p] {
			continue
		}
		checked[p] = true
		if d := lastDirSync[filepath.Dir(p)]; d <= c {
			t.Errorf("%s: synced (trace line %d) before its directory was, after it was created (line %d)", strings.TrimPrefix(p, root), n, c)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// The pass staged each kind of file this order is for.
	for _, want := range []string{"/PIONEER/USBANLZ/", "/PIONEER/rekordbox/.enginex-onelib-", "/.enginex-backup/", "/Engine Library/Database2/.enginex-"} {
		found := false
		for p := range checked {
			found = found || strings.Contains(p, want)
		}
		if !found {
			t.Errorf("no staged file under %q was written and synced (%d checked)", want, len(checked))
		}
	}
}

// issue321FirstPassOnOneLibrary is the first write-back pass, after one deck edit, of a converted
// OneLibrary stick.
func issue321FirstPassOnOneLibrary(t *testing.T) {
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := bridge.Convert(ctx, stick, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	issue321Settle()
	issue321Edit(t, db, 1, "Order 321", 1000)
	issue321Settle()
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", Session: &bridge.CueSession{}, Keys: rekordbox.NewKeyCache(), Settled: func() error { return nil }})
	if err != nil || res.FilesUpdated == 0 || !res.OneLibraryUpdated || res.BackupDir == "" {
		t.Fatalf("first pass: %+v %v", res, err)
	}
}
