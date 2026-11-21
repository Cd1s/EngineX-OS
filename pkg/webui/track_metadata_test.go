package webui

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestTrackMetadataPostUpdatesEngineLibrary(t *testing.T) {
	media := t.TempDir()
	drive := filepath.Join(media, "USB")
	dbDir := filepath.Join(drive, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE Track (id INTEGER PRIMARY KEY, genre TEXT, comment TEXT, label TEXT); INSERT INTO Track VALUES (7, 'Old', 'Old comment', 'Old label')`); err != nil {
		t.Fatal(err)
	}

	s := New(Config{AccessToken: "t", MediaRoot: media, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	body, _ := json.Marshal(map[string]any{
		"drive":   "USB",
		"trackId": 7,
		"genre":   "House",
		"comment": "Warm-up",
		"label":   "EngineX",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/track-metadata", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Pending bool `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pending {
		t.Fatal("metadata unexpectedly queued")
	}
	var genre, comment, label string
	if err := db.QueryRow(`SELECT genre, comment, label FROM Track WHERE id=7`).Scan(&genre, &comment, &label); err != nil {
		t.Fatal(err)
	}
	if genre != "House" || comment != "Warm-up" || label != "EngineX" {
		t.Fatalf("metadata=%q/%q/%q", genre, comment, label)
	}
}

func TestTrackMetadataQueuesWhenLibraryIsOpen(t *testing.T) {
	media := t.TempDir()
	drive := filepath.Join(media, "USB")
	dbDir := filepath.Join(drive, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE Track (id INTEGER PRIMARY KEY, genre TEXT, comment TEXT, label TEXT); INSERT INTO Track VALUES (7, 'Old', 'Old comment', 'Old label')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	f, err := os.Open(dbPath)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	holder := exec.Command("sleep", "60")
	holder.ExtraFiles = []*os.File{f}
	if err := holder.Start(); err != nil {
		f.Close()
		db.Close()
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait(); _ = db.Close() })

	s := New(Config{AccessToken: "t", MediaRoot: media, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	body := bytes.NewBufferString(`{"drive":"USB","trackId":7,"genre":"Queued Genre","comment":"Queued Comment","label":"Queued Label"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/track-metadata", body)
	req.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response trackMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Pending {
		t.Fatal("metadata edit did not report pending")
	}
	var genre string
	if err := db.QueryRow(`SELECT genre FROM Track WHERE id=7`).Scan(&genre); err != nil {
		t.Fatal(err)
	}
	if genre != "Old" {
		t.Fatalf("m.db changed while held open: %q", genre)
	}
	var pending enginedb.Pending
	queued, err := os.ReadFile(filepath.Join(dbDir, enginedb.PendingFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(queued, &pending); err != nil || len(pending.TrackMetadata) != 1 || pending.TrackMetadata[0].Genre != "Queued Genre" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
}

func TestTrackMetadataKeepsStockEditQueuedWhenDriveIsAbsent(t *testing.T) {
	media := t.TempDir()
	s := New(Config{AccessToken: "t", MediaRoot: media, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/track-metadata", bytes.NewBufferString(`{"drive":"USB","trackId":7,"stockEdit":true,"genre":"Queued"}`))
	req.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response trackMetadataResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Pending || response.Genre != "Queued" {
		t.Fatalf("response=%+v", response)
	}
}
