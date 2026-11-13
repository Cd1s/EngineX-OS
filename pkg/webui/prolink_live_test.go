package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func writeLiveProlink(t *testing.T, path, title string, bpm float64) {
	t.Helper()
	d := prolink.Device{Number: 1, Name: "CDJ-3000", Title: title, BPM: bpm, Playing: true, LastSeen: time.Now()}
	b, _ := json.Marshal(prolink.Snapshot{UpdatedAt: time.Now(), Devices: []prolink.Device{d}})
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func deck1Title(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.prolinkGet(rec, httptest.NewRequest(http.MethodGet, "/v1/sc5000/prolink", nil))
	var resp prolinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Decks["1"].Title
}

// BH-GC2-8: one POST to /v1/sc5000/prolink wrote <StateDir>/prolink.json, and
// from then on (across restarts too) the server read only that frozen file and
// never saw rb-bridge's live CDJ state again.
func TestProlinkLiveStateSurvivesAPost(t *testing.T) {
	live := filepath.Join(t.TempDir(), "enginex-prolink.json")
	t.Setenv("ENGINEX_PROLINK_STATE", live)
	stateDir := t.TempDir()
	s := New(Config{StateDir: stateDir, UIDir: t.TempDir()}, nil)
	writeLiveProlink(t, live, "Track A", 128)
	if got := deck1Title(t, s); got != "Track A" {
		t.Fatalf("live state before any post: %q", got)
	}
	body, _ := json.Marshal(prolink.Device{Number: 2, Name: "CDJ-2000NXS2", Title: "Posted", LastSeen: time.Now()})
	rec := httptest.NewRecorder()
	s.prolinkPost(rec, httptest.NewRequest(http.MethodPost, "/v1/sc5000/prolink", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("post: %d", rec.Code)
	}
	time.Sleep(10 * time.Millisecond)
	writeLiveProlink(t, live, "Track B", 140)
	if got := deck1Title(t, s); got != "Track B" {
		t.Fatalf("live state after a post: %q, want Track B", got)
	}
	restarted := New(Config{StateDir: stateDir, UIDir: t.TempDir()}, nil)
	time.Sleep(10 * time.Millisecond)
	writeLiveProlink(t, live, "Track C", 124)
	if got := deck1Title(t, restarted); got != "Track C" {
		t.Fatalf("live state after a restart: %q, want Track C", got)
	}
}

// Review of 9d7c4ce: a POST for the deck rb-bridge also reports (the same player number) was laid
// over its live state for good: later live titles, BPM and play state never showed again.
func TestProlinkPostForALiveDeckDoesNotFreezeIt(t *testing.T) {
	live := filepath.Join(t.TempDir(), "enginex-prolink.json")
	t.Setenv("ENGINEX_PROLINK_STATE", live)
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	writeLiveProlink(t, live, "Track A", 128)
	body, _ := json.Marshal(prolink.Device{Number: 1, Name: "CDJ-3000", Title: "Posted", BPM: 99, LastSeen: time.Now()})
	rec := httptest.NewRecorder()
	s.prolinkPost(rec, httptest.NewRequest(http.MethodPost, "/v1/sc5000/prolink", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("post: %d", rec.Code)
	}
	if got := deck1Title(t, s); got != "Posted" {
		t.Fatalf("the newer posted report: %q", got)
	}
	time.Sleep(10 * time.Millisecond)
	writeLiveProlink(t, live, "Track B", 140)
	rec = httptest.NewRecorder()
	s.prolinkGet(rec, httptest.NewRequest(http.MethodGet, "/v1/sc5000/prolink", nil))
	var resp prolinkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if d := resp.Decks["1"]; d.Title != "Track B" || d.BPM != 140 {
		t.Fatalf("live state after a post for the same deck: %q at %v BPM, want Track B at 140", d.Title, d.BPM)
	}
}
