package enginedb

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

// holdOpen keeps path open in another process, as Engine keeps a drive's m.db.
func holdOpen(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	release := func() { cmd.Process.Kill(); cmd.Wait() }
	t.Cleanup(release)
	return release
}

// BH-GC1-3 / BH-GC2-1: Relocate wrote m.db (VACUUM INTO, UPDATE Track) while
// Engine had the drive's library open. It now leaves m.db alone and queues the
// moves in the library; engine-guardian applies them before Engine next opens it.
func TestRelocateWaitsWhileAnotherProcessHoldsTheLibrary(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeAudio(t, root, "New/Exact.mp3", 100)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(2,'../Old/Exact.mp3','Exact.mp3',100)`); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	release := holdOpen(t, filepath.Join(dbDir, "m.db"))
	res, err := Relocate(context.Background(), RelocateOptions{Root: root})
	if err != nil || res.Applied || len(res.Moves) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	var path string
	db.QueryRow(`SELECT path FROM Track WHERE id=2`).Scan(&path)
	if path != "../Old/Exact.mp3" {
		t.Fatalf("m.db written while another process has it open: %s", path)
	}
	if m, _ := filepath.Glob(filepath.Join(dbDir, "m.db.bak*")); len(m) != 0 {
		t.Fatalf("backup written beside an open library: %v", m)
	}
	if _, err := os.Stat(filepath.Join(dbDir, "enginex-pending.json")); err != nil {
		t.Fatalf("moves not queued: %v", err)
	}
	if !res.Queued {
		t.Fatalf("result does not say the moves wait: %+v", res)
	}
	// Still open: the guardian refuses too.
	if _, err := ApplyPending(context.Background(), root); !errors.Is(err, ErrLibraryInUse) {
		t.Fatalf("applied while open: %v", err)
	}
	// Closed (the mount hook's pre-mount): applied once, with a backup.
	release()
	n, err := ApplyPending(context.Background(), root)
	if err != nil || n != 1 {
		t.Fatalf("apply: %d %v", n, err)
	}
	db.QueryRow(`SELECT path FROM Track WHERE id=2`).Scan(&path)
	if path != "../New/Exact.mp3" {
		t.Fatalf("queued move not applied: %s", path)
	}
	if _, err := os.Stat(filepath.Join(dbDir, "m.db.bak.relocate")); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dbDir, "enginex-pending.json")); !os.IsNotExist(err) {
		t.Fatalf("queue kept: %v", err)
	}
	if n, err := ApplyPending(context.Background(), root); err != nil || n != 0 {
		t.Fatalf("second apply: %d %v", n, err)
	}
}

// BH-GC1-3: every Relocate added another full m.db copy to Database2. The first
// backup and the latest one stay; copies older builds left are removed.
func TestRelocateKeepsTwoBackups(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	dbDir := filepath.Join(root, "Engine Library", "Database2")
	stale := filepath.Join(dbDir, "m.db.bak.relocate.1700000000000000000")
	if err := os.WriteFile(stale, []byte("old copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		name := filepath.Join("Moved", string(rune('a'+i)), "song.mp3")
		writeAudio(t, root, name, 10+i)
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 10+i, "../Gone/"+string(rune('a'+i))+"/song.mp3", "song.mp3", 10+i); err != nil {
			t.Fatal(err)
		}
		if res, err := Relocate(context.Background(), RelocateOptions{Root: root}); err != nil || !res.Applied {
			t.Fatalf("%v %+v", err, res)
		}
	}
	m, _ := filepath.Glob(filepath.Join(dbDir, "m.db.bak*"))
	if len(m) != 2 {
		t.Fatalf("backups: %v", m)
	}
	for _, f := range []string{"m.db.bak.relocate", "m.db.bak.relocate.last"} {
		if _, err := os.Stat(filepath.Join(dbDir, f)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenElsewhereIgnoresThisProcess(t *testing.T) {
	_, root, _ := testlibrary.Create(t)
	p := filepath.Join(root, "Engine Library", "Database2", "m.db")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if pid := OpenElsewhere(p); pid != 0 {
		t.Fatalf("own descriptor counted: %d", pid)
	}
	holdOpen(t, p)
	if pid := OpenElsewhere(p); pid == 0 {
		t.Fatal("another process's descriptor missed")
	}
	if OpenElsewhere(filepath.Join(root, "missing")) != 0 {
		t.Fatal("missing file reported open")
	}
}
