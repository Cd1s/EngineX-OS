package webui

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryStatusEndpointAndSkeletonRejection(t *testing.T) {
	mediaRoot := t.TempDir()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), MediaRoot: mediaRoot}, nil)
	defer s.Close()

	// 1. Non-loopback requests must be rejected with 403.
	lanReq := httptest.NewRequest(http.MethodGet, "/v1/enginex/library-status?path="+url.QueryEscape(filepath.Join(mediaRoot, "USB")), nil)
	lanReq.RemoteAddr = "192.168.1.42:54321"
	lanRec := httptest.NewRecorder()
	s.ServeHTTP(lanRec, lanReq)
	if lanRec.Code != http.StatusForbidden {
		t.Fatalf("LAN request code = %d, want 403", lanRec.Code)
	}

	query := func(p string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/enginex/library-status?path="+url.QueryEscape(p), nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET library-status(%q) code = %d body=%s", p, rec.Code, rec.Body.String())
		}
		var body struct {
			Converted *bool `json:"converted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON for %q: %v (%s)", p, err, rec.Body.String())
		}
		if body.Converted == nil {
			t.Fatalf("missing converted field in response: %s", rec.Body.String())
		}
		return *body.Converted
	}

	// 2. Empty, relative, mediaRoot itself, or path outside mediaRoot -> converted: false.
	for _, bad := range []string{"", "USB", mediaRoot, filepath.Join(mediaRoot, "..", "other"), "/etc"} {
		if query(bad) {
			t.Fatalf("expected converted=false for rejected path %q", bad)
		}
	}

	// 3. Read-only HFS+ or unconverted Rekordbox stick with no Engine Library -> false.
	roStick := filepath.Join(mediaRoot, "HFS_RO_RB")
	if err := os.MkdirAll(filepath.Join(roStick, "PIONEER", "rekordbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if query(roStick) {
		t.Fatal("stick without Engine Library must return converted=false")
	}

	// 4. Writable Rekordbox 7 stick where pre-mount conversion missed budget and
	//    stock Engine created an empty skeleton m.db (Information.uuid set, 0 tracks) -> false.
	skelStick := filepath.Join(mediaRoot, "RB7_BIG")
	skelDBDir := filepath.Join(skelStick, "Engine Library", "Database2")
	if err := os.MkdirAll(skelDBDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(skelDBDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`
		CREATE TABLE Information(id INTEGER PRIMARY KEY, uuid TEXT);
		CREATE TABLE Track(id INTEGER PRIMARY KEY, path TEXT);
		INSERT INTO Information(id, uuid) VALUES(1, 'e7a89052-6441-43bb-9c6d-e3bed7011d00');
	`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if query(skelStick) {
		db.Close()
		t.Fatal("0-track Engine skeleton m.db must return converted=false")
	}

	// 5. Once rb-bridge converts tracks into m.db -> converted: true (even when Scope == "tidal").
	if _, err = db.Exec(`INSERT INTO Track(id, path) VALUES(1, '../Contents/track1.flac')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if !query(skelStick) {
		t.Fatal("populated Engine Library m.db must return converted=true")
	}
}
