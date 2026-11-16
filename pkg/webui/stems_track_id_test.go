package webui

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/migration"
)

func TestStemsTrackID(t *testing.T) {
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	dir := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library/Database2")
	os.MkdirAll(dir, 0o755)
	db, err := sql.Open("sqlite", filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE Track (id INTEGER PRIMARY KEY, path TEXT, filename TEXT)`,
		`INSERT INTO Track VALUES (5, '../../../../` + migration.DataName + `/Stream/netease-1.flac', 'netease-1.flac'), (86, '../../../../` + migration.DataName + `/Stream/netease-1.flac', 'netease-1.flac')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	srv := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	var got struct{ TrackID int64 }
	// The file is named by its path: the library row is the one whose own path it is.
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id?path="+url.QueryEscape(filepath.Join(data, migration.DataName, "Stream/netease-1.flac")), 200, &got)
	if got.TrackID != 86 {
		t.Fatalf("trackId %d, want newest 86", got.TrackID)
	}
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id?path=%2Fx%2Fnone.flac", 200, &got)
	if got.TrackID != 0 {
		t.Fatalf("trackId %d, want 0", got.TrackID)
	}
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id", 400, nil)
}

// Two imports can have the same readable name in different directories. Each
// is its own library row with its own Stems: a lookup by file name gave both
// the newest row, so the one without Stems was listed as ready and the render
// was asked for the wrong track.
func TestStemsRowIsFoundByItsFileNotItsName(t *testing.T) {
	data := t.TempDir()
	shadowDataRoot = data
	defer func() { shadowDataRoot = "/data" }()
	lib := filepath.Join(data, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	os.MkdirAll(filepath.Join(lib, "Database2"), 0o755)
	os.MkdirAll(filepath.Join(lib, "Stems"), 0o755)
	a := filepath.Join(data, "enginex/Stream/Stems/a/A - Song.flac")
	b := filepath.Join(data, "enginex/Stream/Stems/b/A - Song.flac")
	for _, p := range []string{a, b} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(lib, "Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE Track (id INTEGER PRIMARY KEY, path TEXT, filename TEXT)`,
		`INSERT INTO Track VALUES (11, '../../../../enginex/Stream/Stems/a/A - Song.flac', 'A - Song.flac'), (22, '../../../../enginex/Stream/Stems/b/A - Song.flac', 'A - Song.flac')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	os.WriteFile(filepath.Join(lib, "Stems", "22 library.stems"), []byte("stems"), 0o644)

	pending := stemsPending(context.Background(), []string{a, b})
	if !pending[a] || pending[b] {
		t.Fatalf("pending %v: want the first file waiting for its own Stems and the second ready", pending)
	}
	srv := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	for p, want := range map[string]int64{a: 11, b: 22} {
		var got struct{ TrackID int64 }
		tidalGet(t, srv.URL, "/v1/enginex/stems/track-id?path="+url.QueryEscape(p), 200, &got)
		if got.TrackID != want {
			t.Errorf("%s: trackId %d, want %d", p, got.TrackID, want)
		}
	}
	// A path that is the same file through a link in a directory name (the
	// pre-5.3.0.5 data name) is still that row.
	if err := os.Symlink(filepath.Join(data, "enginex"), filepath.Join(data, migration.DataName)); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(data, migration.DataName, "Stream/Stems/a/A - Song.flac")
	if ids, err := internalTrackIDs(context.Background(), linked); err != nil || ids[linked] != 11 {
		t.Fatalf("through the link: %v, %v", ids, err)
	}
}
