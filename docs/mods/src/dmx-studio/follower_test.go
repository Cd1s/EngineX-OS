package dmxstudio

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func TestClockPitchChangeAndJitterWithinOneFrame(t *testing.T) {
	// Continuous pitch fader move (+0% -> +8% -> -4% of 125 BPM) under uneven
	// 10..85 ms poll jitter and occasional dropped frames stays within 1 DMX frame.
	pitchTempo := func(t float64) float64 {
		switch {
		case t < 10:
			return 125.0
		case t < 20:
			return 125.0 * (1 + 0.08*(t-10)/10) // ramp to +8% (135 BPM)
		case t < 35:
			return 135.0
		case t < 45:
			return 135.0 - 15.0*(t-35)/10 // ramp down to 120 BPM (-4%)
		default:
			return 120.0
		}
	}
	deck1 := simDeck{bpm: pitchTempo, offset: 1.1}
	c := newBeatClock()
	fd := &feeder{c: &c, manual: 120}
	for now := 0.0; now < 60; now += 1.0 / 38 {
		// Irregular 10..85 ms arrival jitter
		lag := 0.01 + 0.075*math.Abs(math.Sin(now*11.3)*math.Cos(now*3.7))
		fd.feed(now, 1, true, deck1, lag)
		if now < 1.5 {
			continue
		}
		err := math.Abs(wrapBar(c.beats(now, 120) - deck1.beats(now)))
		frame := deck1.bpm(now) / 60 / 38
		if err > frame {
			t.Fatalf("t=%.3f: pitch+jitter error %.4f beats exceeds 1 DMX frame (%.4f)", now, err, frame)
		}
	}
}

func TestClockPausedFreezesSlewAndZeroBPMFallsBack(t *testing.T) {
	c := newBeatClock()
	c.update(0, 0, 120, clockSample{deck: 1, playing: true, beat: 0, bpm: 120, position: math.NaN()})
	// Introduce a 1.0 beat jump, then immediately pause: paused deck must hold still.
	c.update(0.5, 0.5, 120, clockSample{deck: 1, playing: false, beat: 2.0, bpm: 120, position: 1000})
	held := c.beats(0.5, 120)
	if got := c.beats(1.2, 120); math.Abs(got-held) > 1e-9 {
		t.Fatalf("paused deck slewed while paused: %.4f -> %.4f", held, got)
	}
	// Unanalyzed track (bpm == 0) hands over to manual BPM instead of freezing at 0 BPM.
	c.update(1.5, 1.5, 124, clockSample{deck: 1, playing: true, beat: 2.0, bpm: 0, position: 2000})
	if c.deck != 0 {
		t.Fatalf("0 BPM deck must fall back to manual, got deck %d", c.deck)
	}
	if rate := c.beats(2.5, 124) - c.beats(1.5, 124); math.Abs(rate-124.0/60) > 1e-9 {
		t.Fatalf("manual fallback rate = %.4f beats/s, want %.4f", rate, 124.0/60)
	}
}

func TestSelectClockDeckPriority(t *testing.T) {
	// 1. Playing MASTER beats playing non-MASTER and paused MASTER.
	decks := []deckSnapshot{
		{Deck: 1, Source: "local", Master: true, Playing: false, CurrentBPM: 120, Beat: 1},
		{Deck: 2, Source: "local", Master: false, Playing: true, CurrentBPM: 128, Beat: 2},
		{Deck: 3, Source: "prolink", Master: true, Playing: true, CurrentBPM: 130, Beat: 3},
	}
	picked := selectClockDeck(decks)
	if picked == nil || picked.Deck != 3 || picked.Source != "prolink" {
		t.Fatalf("want playing prolink MASTER deck 3, got %+v", picked)
	}

	// 2. Playing non-MASTER beats paused MASTER.
	decks = []deckSnapshot{
		{Deck: 1, Source: "local", Master: true, Playing: false, CurrentBPM: 120, Beat: 1},
		{Deck: 2, Source: "local", Master: false, Playing: true, CurrentBPM: 128, Beat: 2},
	}
	picked = selectClockDeck(decks)
	if picked == nil || picked.Deck != 2 {
		t.Fatalf("want playing deck 2 when master is paused, got %+v", picked)
	}

	// 3. Paused MASTER holds look when nothing plays.
	decks = []deckSnapshot{
		{Deck: 1, Source: "local", Master: true, Playing: false, CurrentBPM: 120, Beat: 1},
		{Deck: 2, Source: "local", Master: false, Playing: false, CurrentBPM: 128, Beat: 2},
	}
	picked = selectClockDeck(decks)
	if picked == nil || picked.Deck != 1 || picked.Playing {
		t.Fatalf("want paused master deck 1, got %+v", picked)
	}

	// 4. Stale / 0-BPM decks ignored -> nil (manual fallback).
	decks = []deckSnapshot{
		{Deck: 1, Source: "local", Master: true, Playing: true, BPM: 0, CurrentBPM: 0},
		{Deck: 2, Source: "local", Master: true, Playing: true, CurrentBPM: 125, Stale: true},
	}
	if picked := selectClockDeck(decks); picked != nil {
		t.Fatalf("want nil for stale/0-BPM decks, got %+v", picked)
	}
}

func TestStreamSyncPollerWithoutDeckPanelAndFallback5304(t *testing.T) {
	var mu sync.Mutex
	mode5304 := false
	clockHits5304 := 0
	decks := []deckSnapshot{
		{Deck: 1, Source: "local", SourceID: "local:1", Master: true, Lead: true, Playing: true, BPM: 120, CurrentBPM: 126, Beat: 1.5, PositionMS: 10000},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		nowMS := time.Now().UnixMilli()
		switch r.URL.Path {
		case "/v1/sc5000/clock":
			if mode5304 {
				clockHits5304++
				// Simulate 5.3.0.4 tidalGateway catch-all emptyPage (no clockApi field).
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
				return
			}
			out := make([]deckSnapshot, len(decks))
			for i, d := range decks {
				d.At = nowMS
				out[i] = d
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"clockApi": true, "now": nowMS, "decks": out})
		case "/peer/decks":
			_ = json.NewEncoder(w).Encode(map[string]any{"decks": []map[string]any{
				{"deck": 2, "master": true, "playing": true, "bpm": 132, "currentBpm": 132, "beat": 2.25, "sent": nowMS},
			}})
		case "/v1/sc5000/prolink":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	t.Setenv("ENGINEX_WEBUI_URL", srv.URL)
	feat, err := New(modkit.Env{State: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	f := feat.(*Feature)
	defer f.Close()

	// Wait for the background poller (no Panel QML running!) to lock onto Deck 1 at 126 BPM.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := f.Status()
		clk := st.(map[string]any)["clock"].(map[string]any)
		if clk["source"] == "deck" && clk["deck"] == 1 && clk["bpm"] == 126.0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	st, _ := f.Status()
	clk := st.(map[string]any)["clock"].(map[string]any)
	if clk["source"] != "deck" || clk["sourceOrigin"] != "local" || clk["deck"] != 1 || clk["bpm"] != 126.0 {
		t.Fatalf("poller did not follow Deck 1 without Panel: %+v", clk)
	}
	// Never writes show.json while running.
	if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
		t.Fatalf("poller wrote files to state dir: %v", entries)
	}

	// Switch MASTER to Deck 2 and test 5.3.0.4 /peer/decks fallback + negative-cached probe.
	mu.Lock()
	mode5304 = true
	mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ = f.Status()
		clk = st.(map[string]any)["clock"].(map[string]any)
		if clk["deck"] == 2 && clk["bpm"] == 132.0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if clk["deck"] != 2 || clk["bpm"] != 132.0 {
		t.Fatalf("5.3.0.4 fallback did not switch to Deck 2: %+v", clk)
	}
	// Let several fallback polls run and confirm /v1/sc5000/clock was probed only once.
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	hits := clockHits5304
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("5.3.0.4 mode hit /v1/sc5000/clock %d times, want 1 (must negative-cache)", hits)
	}

	// Non-persisting follow toggle turns off deck following without writing show.json.
	if _, err := action(t, f, `{"action":"follow","enabled":false}`); err != nil {
		t.Fatal(err)
	}
	st, _ = f.Status()
	clk = st.(map[string]any)["clock"].(map[string]any)
	if clk["source"] != "manual" || clk["followDeck"] != false {
		t.Fatalf("follow=false should fall back to manual: %+v", clk)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "show.json")); !os.IsNotExist(err) {
		t.Fatal("follow toggle must not write show.json")
	}
}

func TestPhraseAwarenessAndMIDIClockTracker(t *testing.T) {
	f, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := action(t, f, `{"action":"configure","config":{
		"bpm": 120,
		"intensity": 200,
		"phraseBeats": 16,
		"patch": [{"profile":"LED Par","universe":1,"address":1}],
		"phraseScenes": [
			{"red": 255, "green": 0, "blue": 0, "intensity": 180},
			{"red": 0, "green": 0, "blue": 255, "intensity": 255}
		]
	}}`); err != nil {
		t.Fatal(err)
	}
	// Phrase 0: t=0..7.99s (beats 0..15.99 at 120 BPM) -> Red 255, Intensity 180
	fr0 := f.Render(2.0)
	if fr0[1][0] != 180 || fr0[1][1] != 255 || fr0[1][3] != 0 {
		t.Fatalf("phrase 0 frame mismatch: %v", fr0[1][:4])
	}
	// Phrase 1: t=8.5s (beat 17 at 120 BPM) -> Blue 255, Intensity 255
	fr1 := f.Render(8.5)
	if fr1[1][0] != 255 || fr1[1][1] != 0 || fr1[1][3] != 255 {
		t.Fatalf("phrase 1 frame mismatch: %v", fr1[1][:4])
	}
	// Built-in palette rotation when phraseScenes is empty.
	if _, err := action(t, f, `{"action":"configure","config":{"phraseScenes":[]}}`); err != nil {
		t.Fatal(err)
	}
	frPal0 := f.Render(1.0)
	frPal1 := f.Render(8.5)
	if frPal0[1][1] == frPal1[1][1] && frPal0[1][2] == frPal1[1][2] && frPal0[1][3] == frPal1[1][3] {
		t.Fatalf("built-in phrase palette did not rotate across phrase boundary")
	}

	// MIDI clock 24 PPQN tracker: 4 beats = 96 clock ticks + Start, Stop, Continue, and no burst on seek.
	var tr midiClockTracker
	evs := tr.step(true, 0.0)
	if len(evs) != 1 || evs[0] != midiStart {
		t.Fatalf("initial play want [Start], got %v", evs)
	}
	clocks := 0
	for i := 1; i <= 400; i++ {
		b := float64(i) / 100.0 // 0.01..4.00 beats
		for _, e := range tr.step(true, b) {
			if e == midiClock {
				clocks++
			}
		}
	}
	if clocks != 96 {
		t.Fatalf("4 beats must emit 96 MIDI clocks (24 PPQN), got %d", clocks)
	}
	if evs = tr.step(false, 4.0); len(evs) != 1 || evs[0] != midiStop {
		t.Fatalf("pause want [Stop], got %v", evs)
	}
	if evs = tr.step(true, 4.0); len(evs) != 1 || evs[0] != midiContinue {
		t.Fatalf("resume mid-track want [Continue], got %v", evs)
	}
	// Large jump (+16 beats): never bursts 384 clocks.
	if evs = tr.step(true, 20.0); len(evs) > 1 {
		t.Fatalf("large seek burst %d MIDI events", len(evs))
	}
}

type fakeMIDISink struct {
	mu     sync.Mutex
	events []midiEventKind
	closed bool
}

func (s *fakeMIDISink) Send(k midiEventKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, k)
	return nil
}

func (s *fakeMIDISink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestMIDIClockLoopLifecycle(t *testing.T) {
	sink := &fakeMIDISink{}
	f, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.webUIURL = ""
	f.openMIDISink = func() (midiSink, error) { return sink, nil }
	f.looping = true
	go f.loop()
	go f.clockLoop()
	defer f.Close()

	if _, err := action(t, f, `{"action":"configure","config":{"midiClock":true}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := action(t, f, `{"action":"clock","deck":1,"playing":true,"beat":0,"bpm":180}`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.events)
		sink.mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.mu.Lock()
	got := append([]midiEventKind{}, sink.events...)
	sink.mu.Unlock()
	if len(got) < 4 || got[0] != midiStart {
		t.Fatalf("expected Start followed by MIDI clocks, got %v", got)
	}
	if _, err := action(t, f, `{"action":"configure","config":{"midiClock":false}}`); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	closed := sink.closed
	sink.mu.Unlock()
	if !closed {
		t.Fatal("disabling midiClock must close MIDI sink")
	}

	// Rapid toggle true -> false -> true under -race must leave MIDI clock running cleanly,
	// and a stale deck (>clockStale) must drop from playing to stopped and emit midiStop.
	if _, err := action(t, f, `{"action":"configure","config":{"midiClock":true}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := action(t, f, `{"action":"configure","config":{"midiClock":false}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := action(t, f, `{"action":"configure","config":{"midiClock":true}}`); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	sink.events = nil
	sink.mu.Unlock()
	if _, err := action(t, f, `{"action":"clock","deck":1,"playing":true,"beat":0,"bpm":180}`); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.events)
		sink.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.mu.Lock()
	if len(sink.events) == 0 || sink.events[0] != midiStart {
		ev := append([]midiEventKind{}, sink.events...)
		sink.mu.Unlock()
		t.Fatalf("expected midiStart after re-enable, got %v", ev)
	}
	sink.events = nil
	sink.mu.Unlock()

	f.mu.Lock()
	f.clock.updated = f.now() - clockStale - 0.1
	f.mu.Unlock()
	sawStop := false
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !sawStop {
		sink.mu.Lock()
		for _, e := range sink.events {
			if e == midiStop {
				sawStop = true
			}
		}
		sink.mu.Unlock()
		if !sawStop {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !sawStop {
		sink.mu.Lock()
		last := append([]midiEventKind{}, sink.events...)
		sink.mu.Unlock()
		t.Fatalf("stale deck must emit midiStop, events=%v", last)
	}
}

func TestProlinkFallbackCueStateAndExternalClockPrecedence(t *testing.T) {
	// parseHTTPBase edge cases.
	for _, tc := range []struct {
		in, wantAddr, wantPrefix string
		wantErr                  bool
	}{
		{"http://127.0.0.1:8080", "127.0.0.1:8080", "", false},
		{"http://127.0.0.1", "127.0.0.1:80", "", false},
		{"http://[::1]:8080/sub/", "[::1]:8080", "/sub", false},
		{"https://127.0.0.1:8080", "", "", true},
		{"http:///path", "", "", true},
	} {
		addr, pref, err := parseHTTPBase(tc.in)
		if (err != nil) != tc.wantErr || addr != tc.wantAddr || pref != tc.wantPrefix {
			t.Fatalf("parseHTTPBase(%q) = (%q, %q, %v), want (%q, %q, err=%v)", tc.in, addr, pref, err, tc.wantAddr, tc.wantPrefix, tc.wantErr)
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	oldStr := time.Now().Add(-3 * time.Second).UTC().Format(time.RFC3339Nano)
	var mu sync.Mutex
	errMode := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if errMode {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1/sc5000/clock":
			http.NotFound(w, r)
		case "/peer/decks":
			_ = json.NewEncoder(w).Encode(map[string]any{"decks": []any{}})
		case "/v1/sc5000/prolink":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"devices": []any{
					// EngineX self-echo ignored
					map[string]any{"name": "EngineX-Link", "number": 1, "bpm": 128, "effective_bpm": 128, "playing": true, "master": true, "last_seen": nowStr},
					// Stale CDJ ignored
					map[string]any{"name": "CDJ-2000", "number": 2, "bpm": 130, "effective_bpm": 130, "playing": true, "master": true, "last_seen": oldStr},
					// Paused at CUE (play_state_1 == 6): MUST stay playing=false!
					map[string]any{"name": "CDJ-3000", "number": 3, "bpm": 124, "effective_bpm": 124, "beat_in_bar": 2, "beat_phase": 0.5, "play_state_1": 6, "playing": false, "master": true, "last_seen": nowStr},
					// CUE held play (play_state_1 == 7): MUST count as playing!
					map[string]any{"name": "CDJ-3000", "number": 4, "bpm": 128, "effective_bpm": 128, "beat_in_bar": 1, "beat_phase": 0.25, "play_state_1": 7, "playing": false, "master": false, "last_seen": nowStr},
				},
			})
		}
	}))
	defer srv.Close()

	f, err := NewFeature(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.webUIURL = srv.URL

	decks, fallback, err := f.fetchDeckSnapshotsMode()
	if err != nil || !fallback {
		t.Fatalf("expected fallback=true without error, got fallback=%v err=%v", fallback, err)
	}
	if len(decks) != 2 {
		t.Fatalf("expected 2 valid CDJ decks (3 and 4), got %+v", decks)
	}
	for _, d := range decks {
		if d.Deck == 3 && d.Playing {
			t.Fatalf("CDJ play_state_1 == 6 (paused at cue) must NOT count as playing: %+v", d)
		}
		if d.Deck == 4 && (!d.Cue || !d.Playing) {
			t.Fatalf("CDJ play_state_1 == 7 (CUE held) MUST count as playing: %+v", d)
		}
	}
	// Playing deck 4 (CUE held) beats paused master deck 3.
	picked := selectClockDeck(decks)
	if picked == nil || picked.Deck != 4 {
		t.Fatalf("want playing CUE-held deck 4 over paused cue deck 3, got %+v", picked)
	}

	// External clock action takes precedence over poller until clockStale expires.
	if _, err := action(t, f, `{"action":"clock","deck":1,"playing":true,"beat":1.0,"bpm":140}`); err != nil {
		t.Fatal(err)
	}
	if d := f.pollClockOnce(); d != 150*time.Millisecond {
		t.Fatalf("expected pollClockOnce deferral (150ms) during external clock precedence, got %v", d)
	}
	f.mu.Lock()
	if f.clock.bpm != 140 {
		f.mu.Unlock()
		t.Fatalf("poller overwrote external clock bpm=140 -> %v", f.clock.bpm)
	}
	f.lastExternalClock = f.now() - clockStale - 0.1
	f.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	mu.Lock()
	nowStr = time.Now().UTC().Format(time.RFC3339Nano)
	mu.Unlock()
	_ = f.pollClockOnce()
	f.mu.Lock()
	if f.clock.deck != 4 || f.clock.bpm != 128 {
		f.mu.Unlock()
		t.Fatalf("expected poller resume to CDJ deck 4 (128 BPM) after external clock expired, got deck=%d bpm=%v", f.clock.deck, f.clock.bpm)
	}
	f.clockAPIAbsentUntil = time.Time{}
	f.mu.Unlock()

	// HTTP error path returns error cleanly.
	mu.Lock()
	errMode = true
	mu.Unlock()
	var out any
	if err := f.fetchJSON("/v1/sc5000/clock", &out); err == nil {
		t.Fatal("expected HTTP 403 error from fetchJSON")
	}
}
