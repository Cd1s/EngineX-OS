package dbhealer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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
	collisionPath := "../Music/Collision\u0301.mp3"
	collisionNFC := "../Music/Collisioné.mp3"
	write(collisionNFC)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 6, collisionPath, filepath.Base(collisionPath), 32); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 7, collisionNFC, filepath.Base(collisionNFC), 32); err != nil {
		t.Fatal(err)
	}
	bridgePath := "../Music/Bridge\u0301.mp3"
	bridgeNFC := "../Music/Bridgé.mp3"
	write(bridgeNFC)
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,?)`, 8, bridgePath, filepath.Base(bridgePath), 32); err != nil {
		t.Fatal(err)
	}
	streamingPath := "../Music/Cafe\u0301-stream.mp3"
	if _, err := db.Exec(`INSERT INTO Track(id,path,filename,streamingSource) VALUES(9,?,?,?)`, streamingPath, filepath.Base(streamingPath), "tidal"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	state, err := sql.Open("sqlite", filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Exec(`CREATE TABLE BridgeState(path TEXT PRIMARY KEY, trackId INTEGER NOT NULL, fingerprint TEXT NOT NULL); INSERT INTO BridgeState VALUES('../Music/Bridgé.mp3',8,'test')`); err != nil {
		t.Fatal(err)
	}
	state.Close()

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
	var path string
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=6`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != collisionPath {
		t.Fatalf("collision row changed to %q", path)
	}
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=8`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != bridgePath {
		t.Fatalf("bridge-managed row changed to %q", path)
	}
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
	if err := check.QueryRow(`SELECT path FROM Track WHERE id=9`).Scan(&gotStreamingPath); err != nil {
		t.Fatal(err)
	}
	if norm.NFC.String(gotStreamingPath) == gotStreamingPath {
		t.Fatal("streaming path unexpectedly normalized")
	}
}

func TestHealMountDataOnlyStopsAtContextDeadline(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	for i := 1; i <= 130; i++ {
		path := filepath.ToSlash(filepath.Join("..", "Music", "Cafe\u0301", "track-"+strconv.Itoa(i)+".mp3"))
		actual := filepath.Join(root, "Engine Library", filepath.FromSlash(filepath.ToSlash(filepath.Join("..", "Music", "Café", "track-"+strconv.Itoa(i)+".mp3"))))
		if err := os.MkdirAll(filepath.Dir(actual), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(actual, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO Track(id,path,filename,fileBytes) VALUES(?,?,?,1)`, i, path, filepath.Base(path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE Information SET id=99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counted := 0
	normalizeTrackPathBatch = func() {
		counted++
		if counted == 1 {
			cancel()
		}
	}
	defer func() { normalizeTrackPathBatch = nil }()
	got, err := HealMountDataOnlyContext(ctx, root, 0)
	if err != nil || !got.Healthy {
		t.Fatalf("budget stop changed heal result: %+v %v", got, err)
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
	if n != 64 {
		t.Fatalf("first pass committed %d rows, want 64", n)
	}
	var infoID int
	if err := check.QueryRow(`SELECT id FROM Information`).Scan(&infoID); err != nil {
		t.Fatal(err)
	}
	if infoID != 1 {
		t.Fatalf("base heal was rolled back, Information.id=%d", infoID)
	}
	got, err = HealMountDataOnly(root, 0)
	if err != nil || !got.Healthy || got.PathsNormalized != 66 {
		t.Fatalf("second pass did not finish remaining rows: %+v %v", got, err)
	}
}
