package webui

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestOwnForPeerCarriesBeatForward(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	now := time.Now()
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`[{"deck":1,"playing":true,"positionMs":5000,"pitch":10,"bpm":120,"currentBpm":132,"beat":1.0,"master":true,"at":%d}]`, now.UnixMilli())
	s.decksPost(w, httptest.NewRequest("POST", "/v1/sc5000/decks", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("decksPost: %d", w.Code)
	}
	s.shadow.mu.Lock()
	// Simulate 1.5 s passing with alive pings only (no full decks post).
	s.shadow.own[0].At -= 1500
	s.shadow.ownAt = time.Now()
	out := s.shadow.ownForPeer(time.Now())
	s.shadow.mu.Unlock()
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	// 132 BPM = 2.2 beats/s -> +1.5 s = +3.3 beats -> wrapBarBeat(1.0 + 3.3) = 0.3
	if diff := math.Abs(out[0].Beat - 0.3); diff > 0.05 {
		t.Fatalf("carried beat = %.4f, want ~0.3", out[0].Beat)
	}
	if out[0].At != 0 {
		t.Fatalf("ownForPeer must keep At == 0, got %d", out[0].At)
	}
}

func TestDeckClockEndpointAndLoopbackGuard(t *testing.T) {
	uiDir := t.TempDir()
	s := New(Config{UIDir: uiDir, StateDir: t.TempDir()}, nil)
	defer s.Close()

	// Non-loopback requests to /v1/sc5000/clock are rejected with 403.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "192.168.1.50:12345"
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("LAN GET /v1/sc5000/clock = %d, want 403", rec.Code)
	}

	// Initially uiStatus omits deckClock (unchanged digest when dmx-studio is off).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
	req.RemoteAddr = "127.0.0.1:4000"
	s.ServeHTTP(rec, req)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"deckClock"`) {
		t.Fatalf("initial /v1/sc5000/ui should omit deckClock: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Polling GET /v1/sc5000/clock activates deckClock in /v1/sc5000/ui without starting shadow beacons.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4001"
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("loopback GET /v1/sc5000/clock = %d", rec.Code)
	}
	s.shadow.mu.Lock()
	running := s.shadow.running
	s.shadow.mu.Unlock()
	if running {
		t.Fatal("GET /v1/sc5000/clock must not start 4-deck shadow beacons")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
	req.RemoteAddr = "127.0.0.1:4002"
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"deckClock":true`) {
		t.Fatalf("expected deckClock:true after clock poll, got %s", rec.Body.String())
	}

	// Posting local clock samples updates /v1/sc5000/clock without touching shadow.own.
	nowMS := time.Now().UnixMilli()
	postBody := fmt.Sprintf(`[{"deck":1,"playing":true,"master":true,"lead":true,"bpm":120,"currentBpm":120,"beat":1.5,"positionMs":4000,"at":%d}]`, nowMS-250)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/sc5000/clock", strings.NewReader(postBody))
	req.RemoteAddr = "127.0.0.1:4003"
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /v1/sc5000/clock = %d: %s", rec.Code, rec.Body.String())
	}
	s.shadow.mu.Lock()
	ownLen := len(s.shadow.own)
	running = s.shadow.running
	s.shadow.mu.Unlock()
	if ownLen != 0 || running {
		t.Fatalf("clock post touched own=%d or shadow running=%v", ownLen, running)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4004"
	s.ServeHTTP(rec, req)
	var snap struct {
		ClockAPI bool        `json:"clockApi"`
		Now      int64       `json:"now"`
		Decks    []clockDeck `json:"decks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.ClockAPI || len(snap.Decks) != 1 || snap.Decks[0].Source != "local" || !snap.Decks[0].Master {
		t.Fatalf("unexpected clock snapshot: %+v", snap)
	}
	// 250 ms elapsed at 120 BPM = +0.5 beats -> 1.5 + 0.5 = 2.0
	if diff := math.Abs(snap.Decks[0].Beat - 2.0); diff > 0.1 {
		t.Fatalf("carried clock beat = %.3f, want ~2.0", snap.Decks[0].Beat)
	}

	// Out-of-order older POST is ignored.
	oldBody := fmt.Sprintf(`[{"deck":2,"playing":true,"master":true,"bpm":140,"currentBpm":140,"beat":0.5,"at":%d}]`, nowMS-1000)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/sc5000/clock", strings.NewReader(oldBody))
	req.RemoteAddr = "127.0.0.1:4005"
	s.ServeHTTP(rec, req)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4006"
	s.ServeHTTP(rec, req)
	snap = struct {
		ClockAPI bool        `json:"clockApi"`
		Now      int64       `json:"now"`
		Decks    []clockDeck `json:"decks"`
	}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if len(snap.Decks) != 1 || snap.Decks[0].Deck != 1 {
		t.Fatalf("stale clock post overwrote newer deck: %+v", snap.Decks)
	}
}

func TestDeckClockIncludesProlinkAndModEnabledFlag(t *testing.T) {
	uiDir := t.TempDir()
	s := New(Config{UIDir: uiDir, StateDir: t.TempDir()}, nil)
	defer s.Close()

	// Enabling dmx-studio in mods-state.json sets deckClock in /v1/sc5000/ui without polling first.
	if err := os.WriteFile(filepath.Join(uiDir, "mods-state.json"), []byte(`{"mods":{"dmx-studio":{"enabled":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
	req.RemoteAddr = "127.0.0.1:4010"
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"deckClock":true`) {
		t.Fatalf("enabled dmx-studio should set deckClock:true, got %s", rec.Body.String())
	}

	// Inject a Pro DJ Link CDJ device and verify /v1/sc5000/clock exposes it.
	prolinkMu.Lock()
	postedDevices[s] = map[string]prolink.Device{
		"2": {
			Address:      "192.168.1.20",
			Name:         "CDJ-3000",
			Number:       2,
			PacketType:   0x28,
			BPM:          125,
			EffectiveBPM: 125,
			BeatInBar:    3,
			Playing:      true,
			Master:       true,
			LastSeen:     time.Now(),
		},
	}
	prolinkMu.Unlock()

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4011"
	s.ServeHTTP(rec, req)
	var snap struct {
		Decks []clockDeck `json:"decks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Decks) != 1 || snap.Decks[0].Source != "prolink" || snap.Decks[0].Deck != 2 || !snap.Decks[0].Master {
		t.Fatalf("expected prolink CDJ deck in clock snapshot, got %+v", snap.Decks)
	}
	if snap.Decks[0].Beat < 2.0 || snap.Decks[0].Beat >= 3.0 {
		t.Fatalf("expected beat in [2, 3), got %.3f", snap.Decks[0].Beat)
	}
}
