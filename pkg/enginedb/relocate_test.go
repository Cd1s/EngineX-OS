package enginedb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func writeAudio(t *testing.T, root, rel string, size int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRelocateMissingTracks(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	writeAudio(t, root, "Music/ok.mp3", 10)             // present: left alone
	writeAudio(t, root, "New/Exact.mp3", 100)           // tier 1
	writeAudio(t, root, "Other/Exact.mp3", 5)           // same name, other size: size picks the right one
	writeAudio(t, root, "New/CASE.FLAC", 7)             // tier 2
	writeAudio(t, root, "Sorted/01 - Some_Song.wav", 3) // tier 3
	writeAudio(t, root, "Twin/a.mp3", 1)                // ambiguous
	writeAudio(t, root, "Twin2/a.mp3", 1)
	writeAudio(t, root, "Music/used.mp3", 9) // belongs to another track after the move
	writeAudio(t, root, "Engine Library/Music/inside.mp3", 1)
	for _, r := range []struct {
		id    int
		path  string
		size  int
		title string
	}{
		{1, "../Music/ok.mp3", 10, ""},
		{2, "../Old/Exact.mp3", 100, ""},
		{3, "../Old/case.flac", 0, ""},
		{4, "../Old/somesong.wav", 0, "Some Song"},
		{5, "../Old/a.mp3", 1, ""},
		{6, "../Old/nothing.mp3", 1, ""},
		{7, "../Music/used.mp3", 9, ""},
	} {
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes,title) VALUES(?,?,?,?,?)`, r.id, r.path, filepath.Base(r.path), r.size, r.title); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(8,'../gone.mp3','gone.mp3','tidal')`); err != nil {
		t.Fatal(err)
	}
	dry, err := Relocate(context.Background(), RelocateOptions{Root: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{2: "../New/Exact.mp3", 3: "../New/CASE.FLAC", 4: "../Sorted/01 - Some_Song.wav"}
	if dry.Missing != 5 || dry.Unresolved != 2 || len(dry.Moves) != 3 || dry.Applied {
		t.Fatalf("dry run: %+v", dry)
	}
	for _, m := range dry.Moves {
		if want[m.ID] != m.NewPath || m.Tier != int(m.ID)-1 {
			t.Errorf("track %d: %+v", m.ID, m)
		}
	}
	var path string
	db.QueryRow(`SELECT path FROM Track WHERE id=2`).Scan(&path)
	if path != "../Old/Exact.mp3" {
		t.Fatalf("dry run wrote: %s", path)
	}

	res, err := Relocate(context.Background(), RelocateOptions{Root: root})
	if err != nil || !res.Applied || len(res.Moves) != 3 {
		t.Fatalf("%v %+v", err, res)
	}
	for id, p := range want {
		var path, name string
		db.QueryRow(`SELECT path, filename FROM Track WHERE id=?`, id).Scan(&path, &name)
		if path != p || name != filepath.Base(p) {
			t.Errorf("track %d: %s %s", id, path, name)
		}
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	// A second run finds nothing more to do and keeps the first backup.
	again, err := Relocate(context.Background(), RelocateOptions{Root: root})
	if err != nil || again.Missing != 2 || len(again.Moves) != 0 || again.Applied {
		t.Fatalf("%v %+v", err, again)
	}
}

func TestRelocateNeedsALibrary(t *testing.T) {
	if _, err := Relocate(context.Background(), RelocateOptions{Root: t.TempDir()}); err == nil {
		t.Fatal("empty drive accepted")
	}
}
