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
	if out[0].At != 0 || out[0].Stale {
		t.Fatalf("ownForPeer must keep At == 0 and Stale == false, got At=%d Stale=%v", out[0].At, out[0].Stale)
	}

	// Paused deck does not advance Beat or PositionMS.
	s.shadow.mu.Lock()
	s.shadow.own[0].Playing = false
	s.shadow.own[0].Beat = 2.25
	s.shadow.own[0].PositionMS = 9000
	s.shadow.own[0].At = time.Now().Add(-1500 * time.Millisecond).UnixMilli()
	s.shadow.ownAt = time.Now()
	out = s.shadow.ownForPeer(time.Now())
	s.shadow.mu.Unlock()
	if out[0].Beat != 2.25 || out[0].PositionMS != 9000 {
		t.Fatalf("paused ownForPeer moved Beat=%.3f Pos=%.1f", out[0].Beat, out[0].PositionMS)
	}

	// Deck older than 2 s is marked Stale=true and stops carrying past 2 s.
	s.shadow.mu.Lock()
	s.shadow.own[0].Playing = true
	s.shadow.own[0].Beat = 1.0
	s.shadow.own[0].At = time.Now().Add(-3 * time.Second).UnixMilli()
	s.shadow.ownAt = time.Now().Add(-3 * time.Second)
	out = s.shadow.ownForPeer(time.Now())
	s.shadow.mu.Unlock()
	if !out[0].Stale {
		t.Fatal("ownAt older than 2 s must set Stale=true")
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

	// Envelope form {"decks":[...]} deduplicates decks (<=4), and invalid JSON returns 400.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/sc5000/clock", strings.NewReader("{bad json"))
	req.RemoteAddr = "127.0.0.1:4007"
	s.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("invalid JSON POST /v1/sc5000/clock = %d, want 400", rec.Code)
	}

	nowMS = time.Now().UnixMilli()
	envBody := fmt.Sprintf(`{"decks":[{"deck":1,"playing":false,"master":true,"bpm":120,"currentBpm":120,"beat":2.5,"positionMs":5000,"at":%d},{"deck":1,"playing":true,"bpm":130,"beat":0.1,"at":%d},{"deck":5,"playing":true,"bpm":120,"beat":0.1,"at":%d}]}`, nowMS, nowMS, nowMS)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/sc5000/clock", strings.NewReader(envBody))
	req.RemoteAddr = "127.0.0.1:4008"
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("envelope POST = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4009"
	s.ServeHTTP(rec, req)
	snap = struct {
		ClockAPI bool        `json:"clockApi"`
		Now      int64       `json:"now"`
		Decks    []clockDeck `json:"decks"`
	}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	if len(snap.Decks) != 1 || snap.Decks[0].Playing || snap.Decks[0].Beat != 2.5 || snap.Decks[0].PositionMS != 5000 {
		t.Fatalf("paused envelope / dedup check failed: %+v", snap.Decks)
	}

	// Fresh peerDecks are exposed with carried beats; stale peerDecks (>=2s) are skipped.
	masterTrue := true
	s.shadow.mu.Lock()
	s.shadow.peerDecks[3] = peerDeck{
		deckState: deckState{Deck: 3, Playing: true, Master: &masterTrue, BPM: 120, CurrentBPM: 120, Beat: 1.0, Link: "peerA"},
		ip:        "192.168.1.21",
		at:        time.Now().Add(-500 * time.Millisecond),
	}
	s.shadow.peerDecks[4] = peerDeck{
		deckState: deckState{Deck: 4, Playing: true, BPM: 120, CurrentBPM: 120, Beat: 1.0, Link: "peerB"},
		ip:        "192.168.1.22",
		at:        time.Now().Add(-3 * time.Second),
	}
	s.shadow.mu.Unlock()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/clock", nil)
	req.RemoteAddr = "127.0.0.1:4010"
	s.ServeHTTP(rec, req)
	snap = struct {
		ClockAPI bool        `json:"clockApi"`
		Now      int64       `json:"now"`
		Decks    []clockDeck `json:"decks"`
	}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &snap)
	foundPeer3, foundPeer4 := false, false
	for _, d := range snap.Decks {
		if d.Source == "peer" && d.Deck == 3 {
			foundPeer3 = true
			if diff := math.Abs(d.Beat - 2.0); diff > 0.15 {
				t.Fatalf("peer deck 3 carried beat = %.3f, want ~2.0", d.Beat)
			}
		}
		if d.Source == "peer" && d.Deck == 4 {
			foundPeer4 = true
		}
	}
	if !foundPeer3 || foundPeer4 {
		t.Fatalf("peer deck filtering mismatch: found3=%v found4=%v decks=%+v", foundPeer3, foundPeer4, snap.Decks)
	}

	// After 15 s without GET /clock (and dmx-studio disabled), deckClock returns to false/omitted.
	s.shadow.mu.Lock()
	s.shadow.clockPolled = time.Now().Add(-16 * time.Second)
	s.shadow.mu.Unlock()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
	req.RemoteAddr = "127.0.0.1:4011"
	s.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `"deckClock"`) {
		t.Fatalf("expired clockPolled should omit deckClock: %s", rec.Body.String())
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

func TestUIStatusNeverBlocksOnModsManagerLock(t *testing.T) {
	base := t.TempDir()
	uiDir := filepath.Join(base, "ui")
	modDir := filepath.Join(base, "mods", "slow-mod")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(base, "started")
	release := filepath.Join(base, "release")
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\nwhile [ ! -f %q ]; do sleep 0.02; done\n", started, release)
	if err := os.WriteFile(filepath.Join(modDir, "mod.json"), []byte(`{"id":"slow-mod","version":"1.0.0","script":"start.sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "start.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "mods-state.json"), []byte(`{"mods":{"slow-mod":{"enabled":true},"dmx-studio":{"enabled":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{UIDir: uiDir, StateDir: t.TempDir()}, nil)
	defer s.Close()

	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		s.modsResume()
	}()
	defer func() {
		_ = os.WriteFile(release, nil, 0o644)
		<-resumeDone
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("slow mod start script did not acquire manager lock")
	}

	uiDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/sc5000/ui", nil)
		req.RemoteAddr = "127.0.0.1:4020"
		s.ServeHTTP(rec, req)
		uiDone <- rec.Code
	}()
	select {
	case code := <-uiDone:
		if code != 200 {
			t.Fatalf("GET /v1/sc5000/ui = %d, want 200", code)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("GET /v1/sc5000/ui stalled while StartEnabled held managerMu")
	}
}
