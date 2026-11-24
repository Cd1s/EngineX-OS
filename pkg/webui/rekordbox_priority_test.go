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

	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge/v2"
)

func TestRekordboxPriorityFromDecksAndBrowse(t *testing.T) {
	priDir := t.TempDir()
	restore := bridge.SetPriorityDirForTest(priDir)
	defer restore()

	mediaRoot := t.TempDir()
	stick := filepath.Join(mediaRoot, "UNTITLED")
	trackA := filepath.Join(stick, "Contents", "Artist", "TrackA.mp3")
	trackB := filepath.Join(stick, "Contents", "Artist", "TrackB.mp3")
	if err := os.MkdirAll(filepath.Dir(trackA), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackA, []byte("mp3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackB, []byte("mp3"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{StateDir: t.TempDir(), MediaRoot: mediaRoot}, nil)
	s.shadow.mediaRoot = mediaRoot
	s.shadow.dataRoot = t.TempDir()

	// 1. Deck load + PlayState via POST /v1/enginex/decks enqueues TrackA and marks deck active.
	body, _ := json.Marshal([]deckState{{
		Deck:    1,
		Net:     "net://u/UNTITLED/Contents/Artist/TrackA.mp3",
		Playing: true,
		At:      time.Now().UnixMilli(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/enginex/decks", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.decksPost(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("decksPost status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !bridge.IsDeckActive() {
		t.Fatal("expected deck active after PlayState=true")
	}
	pending, _ := bridge.PendingPriority(stick)
	if len(pending) != 1 || pending[0].Path != "../Contents/Artist/TrackA.mp3" || pending[0].Source != "deck" {
		t.Fatalf("deck load priority = %+v", pending)
	}

	// 2. Browse preview tap via POST /v1/enginex/rekordbox/priority puts TrackB at head.
	pBody, _ := json.Marshal(rbPriorityRequest{Path: trackB, Source: "preview"})
	pReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/priority", bytes.NewReader(pBody))
	pReq.RemoteAddr = "127.0.0.1:12346"
	pRR := httptest.NewRecorder()
	s.handleRekordboxPriority(pRR, pReq)
	if pRR.Code != http.StatusOK {
		t.Fatalf("priority endpoint status=%d body=%s", pRR.Code, pRR.Body.String())
	}
	pending, _ = bridge.PendingPriority(stick)
	if len(pending) != 2 || pending[0].Path != "../Contents/Artist/TrackB.mp3" || pending[0].Source != "preview" {
		t.Fatalf("browse preview priority = %+v", pending)
	}

	// 3. Non-loopback caller is rejected.
	badReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/rekordbox/priority", bytes.NewReader(pBody))
	badReq.RemoteAddr = "192.168.1.50:12346"
	badRR := httptest.NewRecorder()
	s.handleRekordboxPriority(badRR, badReq)
	if badRR.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status=%d, want 403", badRR.Code)
	}
}
