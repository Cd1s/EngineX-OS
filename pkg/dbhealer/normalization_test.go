package dbhealer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestHealMountDataOnlyNormalizesMissingNFDPaths(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	write := func(rel string) int64 {
		p := filepath.Join(root, "Engine Library", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		const size = 32
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return size
	}
	cases := []struct {
		id       int
		path     string
		wantPath string
	}{
		{1, "../Music/Cafe\u0301.mp3", "../Music/Café.mp3"},
		{2, "../Music/Café-control.mp3", "../Music/Café-control.mp3"},
		{3, "../Music/Cafe\u0301-nfd.mp3", "../Music/Cafe\u0301-nfd.mp3"},
		{4, "../Music/Beyonce\u0301/한글.mp3", "../Music/Beyoncé/한글.mp3"},
	}
	for _, tc := range cases {
		if tc.id == 3 {
			write(tc.path)
		} else {
			write(tc.wantPath)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, tc.id, tc.path, filepath.Base(tc.path), 32); err != nil {
			t.Fatal(err)
		}
	}
	streamingPath := "../Music/Cafe\u0301-stream.mp3"
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(5,?,?,?)`, streamingPath, filepath.Base(streamingPath), "tidal"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	got, err := HealMountDataOnly(root, 0)
	if err != nil {
		t.Fatalf("heal: %+v %v", got, err)
	}
	if got.PathsNormalized != 2 {
		t.Fatalf("paths normalized=%d, want 2: %+v", got.PathsNormalized, got)
	}
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for _, tc := range cases {
		var path, filename string
		if err := check.QueryRow(`SELECT path,filename FROM Track WHERE id=?`, tc.id).Scan(&path, &filename); err != nil {
			t.Fatal(err)
		}
		if path != tc.wantPath || filename != filepath.Base(tc.wantPath) {
			t.Errorf("track %d path=%q filename=%q, want %q/%q", tc.id, path, filename, tc.wantPath, filepath.Base(tc.wantPath))
		}
	}
	var gotStreamingPath string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=5`).Scan(&gotStreamingPath); err != nil {
		t.Fatal(err)
	}
	if norm.NFC.String(gotStreamingPath) == gotStreamingPath {
		t.Fatal("streaming path unexpectedly normalized")
	}
}

func TestHealMountDataOnlyStopsAtContextDeadline(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 200; i++ {
		path := filepath.ToSlash(filepath.Join("..", "Music", "Cafe\u0301", "track", "track-"+strconv.Itoa(i)+".mp3"))
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename) VALUES(?,?,?)`, i, path, filepath.Base(path)); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	got, err := HealMountDataOnlyContext(ctx, root, 0)
	if err == nil {
		t.Fatalf("deadline heal succeeded: %+v", got)
	}
	var n int
	check, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if err := check.QueryRow(`SELECT count(*) FROM Track WHERE path LIKE '%Café%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("deadline transaction partially committed %d rows", n)
	}
}
