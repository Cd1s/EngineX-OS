package webui

import (
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
		`INSERT INTO Track VALUES (5, '../../../../sc5000-custom/Stream/netease-1.flac', 'netease-1.flac'), (86, '../../../../sc5000-custom/Stream/netease-1.flac', 'netease-1.flac')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	srv := httptest.NewServer(New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	var got struct{ TrackID int64 }
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id?path=%2Fdata%2Fsc5000-custom%2FStream%2Fnetease-1.flac", 200, &got)
	if got.TrackID != 86 {
		t.Fatalf("trackId %d, want newest 86", got.TrackID)
	}
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id?path=%2Fx%2Fnone.flac", 200, &got)
	if got.TrackID != 0 {
		t.Fatalf("trackId %d, want 0", got.TrackID)
	}
	tidalGet(t, srv.URL, "/v1/enginex/stems/track-id", 400, nil)
}
