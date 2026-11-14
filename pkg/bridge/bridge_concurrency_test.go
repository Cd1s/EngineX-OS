package bridge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestMountImportDoesNotRemoveEngineOpenEmptyDatabase(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	p := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if _, err = ConvertAtMount(context.Background(), root, enginedb.Options{}, false); !errors.Is(err, enginedb.ErrLibraryInUse) {
		t.Fatalf("expected library-in-use refusal: %v", err)
	}
	if _, err = os.Stat(p); err != nil {
		t.Fatalf("removed Engine's open database: %v", err)
	}
}

func TestMountImportPreservesConcurrentEngineWrite(t *testing.T) {
	for _, mode := range []string{"DELETE", "WAL"} {
		t.Run(mode, func(t *testing.T) {
			p, root, engine := testlibrary.Create(t)
			testfixture.Export(t, root)
			if _, err := engine.Exec("PRAGMA journal_mode=" + mode); err != nil {
				t.Fatal(err)
			}
			wrote := false
			_, err := ConvertAtMount(context.Background(), root, enginedb.Options{
				Deadline: time.Now().Add(time.Hour),
				Now: func() time.Time {
					// Sync starts after the snapshot. Engine commits before publish.
					if !wrote {
						wrote = true
						if _, e := engine.Exec(`INSERT INTO Playlist(id,title) VALUES(99,'Live set')`); e != nil {
							t.Fatal(e)
						}
					}
					return time.Now()
				},
			}, false)
			if !wrote {
				t.Fatal("Engine write was not exercised")
			}
			if err == nil {
				t.Error("published a stale snapshot after Engine committed a playlist")
			}
			// Read through a new connection: an old descriptor can hide replacement.
			db, e := sql.Open("sqlite", p)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			var title string
			if e = db.QueryRow(`SELECT title FROM Playlist WHERE id=99`).Scan(&title); e != nil || title != "Live set" {
				t.Fatalf("Engine playlist lost: %q %v", title, e)
			}
			for _, name := range []string{stampName, pendingName, stateName} {
				if _, e = os.Stat(filepath.Join(filepath.Dir(p), name)); !os.IsNotExist(e) {
					t.Fatalf("aborted import left %s: %v", name, e)
				}
			}
			if r, e := ConvertAtMount(context.Background(), root, enginedb.Options{}, false); e != nil || r.Bypassed || r.Added != 5 {
				t.Fatalf("next mount did not retry: %+v %v", r, e)
			}
		})
	}
}
