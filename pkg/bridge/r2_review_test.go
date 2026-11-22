package bridge_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// r2 review: a mount of a small library with an m.db read (and, for a
// OneLibrary, decrypted) the export twice, and the file-reading budget started
// before the first read, so a slow read left tracks without cues.
func TestMountReadsTheExportOnceAndBudgetsAfterIt(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := bridge.ReadLibrary(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	// An earlier import that took only what the export holds: every track still needs its files read.
	if _, err = enginedb.Sync(context.Background(), root, l, enginedb.Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	// The read outlasts the budget; the 5 tracks after it fit in it even under -race on a busy host.
	restore := bridge.SlowExportReadForTest(3*time.Second, 2500*time.Millisecond)
	defer restore()
	before := bridge.ExportReadsForTest()
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := bridge.ExportReadsForTest() - before; n != 1 {
		t.Fatalf("export read %d times in one mount", n)
	}
	if r.Deferred != 0 {
		t.Fatalf("%d tracks deferred: the budget was spent on reading the export", r.Deferred)
	}
	if _, err = os.Stat(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")); err != nil {
		t.Fatal("complete import not stamped", err)
	}
}

// Prefer the richer export on timestamp ties, retaining genuinely newer PDBs.
func TestExportSelectionFreshness(t *testing.T) {
	cases := []struct {
		name       string
		oneLibDiff time.Duration
		wantPDB    bool
	}{
		{"same export", 3 * time.Second, false},
		{"pdb newer", -time.Minute, true},
		{"onelibrary newer", 10 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			testfixture.Export(t, root)
			writePlainOneLibrary(t, root)
			base := time.Now().Add(-time.Hour)
			os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/export.pdb"), base, base)
			ol := base.Add(tc.oneLibDiff)
			os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"), ol, ol)
			l, err := bridge.ReadLibrary(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			var shared *rekordbox.Track
			for i := range l.Tracks {
				if strings.EqualFold(l.Tracks[i].Path, "/MUSIC/three.wav") {
					shared = &l.Tracks[i]
					break
				}
			}
			if len(l.Tracks) != 5 || shared == nil {
				t.Fatalf("preferred source changed the union: tracks=%d shared=%v", len(l.Tracks), shared != nil)
			}
			if tc.wantPDB && shared.Title != "three.wav" {
				t.Fatalf("PDB metadata did not win: %q", shared.Title)
			}
			if !tc.wantPDB && shared.Title != "Only in OneLibrary" {
				t.Fatalf("OneLibrary metadata did not win: %q", shared.Title)
			}
		})
	}
	// A damaged export.pdb falls back to the OneLibrary.
	root := t.TempDir()
	testfixture.Export(t, root)
	writePlainOneLibrary(t, root)
	testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("not a pdb"))
	if l, err := bridge.ReadLibrary(context.Background(), root); err != nil || len(l.Tracks) != 1 {
		t.Fatalf("no fallback to the OneLibrary: %v", err)
	}
}

func TestExportRatingReachesEngineWhenSourceIDAndPathStayTheSame(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if err := os.Remove(filepath.Join(root, "PIONEER/rekordbox/export.pdb")); err != nil {
		t.Fatal(err)
	}
	testfixture.Write(t, root, "MUSIC/three.wav", testfixture.WAV(true))
	writePlainOneLibrary(t, root)
	ctx := context.Background()
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, true); err != nil || res.Added != 1 {
		t.Fatalf("initial OneLibrary import: %+v %v", res, err)
	}
	engine := dbOpen(t, root)
	if _, err := engine.Exec(`UPDATE Track SET rating=77`); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	db, err := sql.Open("sqlite", filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE content SET rating=5`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db"), when, when); err != nil {
		t.Fatal(err)
	}
	if res, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || res.Bypassed {
		t.Fatalf("re-export: %+v %v", res, err)
	}
	engine = dbOpen(t, root)
	if got := scalar[int](t, engine, `SELECT rating FROM Track`); got != 5 {
		t.Fatalf("export rating was not imported: %d", got)
	}
}

// A drive with only export.pdb keeps the stamp format of dc4d52d, so sticks
// imported by earlier builds are not imported again.
func TestSingleExportStampUnchanged(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("v4 PIONEER/rekordbox/export.pdb %d %d\n", st.Size(), st.ModTime().UnixNano()); string(b) != want {
		t.Fatalf("stamp %q, want %q", b, want)
	}
}

func writePlainOneLibrary(t *testing.T, root string) {
	t.Helper()
	p := filepath.Join(root, "PIONEER/rekordbox/exportLibrary.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE content(content_id INTEGER PRIMARY KEY, title TEXT, path TEXT, rating INTEGER DEFAULT 0); INSERT INTO content VALUES(77,'Only in OneLibrary','/MUSIC/three.wav',0)`); err != nil {
		t.Fatal(err)
	}
}

// r2 review: a failed mount removed .rb-bridge.lock even while another
// rb-bridge held it, so a third one could lock a new file and run beside it.
func TestCleanupKeepsALockHeldByAnotherBridge(t *testing.T) {
	for _, held := range []bool{true, false} {
		root := t.TempDir()
		testfixture.Write(t, root, "PIONEER/rekordbox/export.pdb", []byte("not a pdb"))
		dir := filepath.Join(root, "Engine Library/Database2")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(dir, ".rb-bridge.lock")
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if held {
			if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err == nil {
			t.Fatal("damaged export.pdb imported")
		}
		_, err = os.Stat(lock)
		f.Close()
		if held && err != nil {
			t.Fatalf("held lock removed: %v", err)
		}
		if !held {
			if _, err = os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
				t.Fatalf("free lock and empty Engine Library left behind: %v", err)
			}
		}
	}
}
