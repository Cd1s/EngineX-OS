package samplersequencer

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func TestPatternPersistenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	env := modkit.Env{State: dir, ModDir: dir}

	feat, err := New(env)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer feat.Close()

	if _, err := feat.Action(map[string]any{"action": "toggle", "slot": 2, "step": 5}); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	if _, err := feat.Action(map[string]any{"action": "set", "bpm": 128.5, "division": "1/8", "swing": 25, "followDeck": false}); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Reload from pattern.json in a fresh instance.
	feat2, err := New(env)
	if err != nil {
		t.Fatalf("New reload: %v", err)
	}
	defer feat2.Close()

	stRaw, err := feat2.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	st := stRaw.(map[string]any)
	if st["bpm"] != 128.5 || st["division"] != "1/8" || st["swing"] != 25 || st["followDeck"] != false {
		t.Fatalf("unexpected reloaded settings: %+v", st)
	}
	steps := st["steps"].([][]bool)
	if !steps[1][4] || steps[0][0] {
		t.Fatalf("unexpected reloaded steps[1][4]=%v steps[0][0]=%v", steps[1][4], steps[0][0])
	}

	if _, err := feat2.Action(map[string]any{"action": "clear"}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	f2 := feat2.(*Feature)
	if f2.state.Steps[1][4] {
		t.Fatal("expected step to be cleared")
	}

	// Verify bad requests return modkit.Bad (HTTP 400).
	badReqs := []map[string]any{
		{"action": "toggle", "slot": 0, "step": 1},
		{"action": "toggle", "slot": 1, "step": 9},
		{"action": "set", "bpm": 10},
		{"action": "set", "division": "1/32"},
		{"action": "set", "swing": 90},
		{"action": "nope"},
	}
	for _, req := range badReqs {
		if _, err := feat2.Action(req); !modkit.IsBad(err) {
			t.Fatalf("expected bad request for %v, got %v", req, err)
		}
	}
	if filepath.Base(f2.statePath) != "pattern.json" {
		t.Fatalf("unexpected statePath %q", f2.statePath)
	}
}

func TestClockEndpointAndNegativeCache(t *testing.T) {
	var enginexHits atomic.Int32
	var return404 atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/enginex/clock":
			enginexHits.Add(1)
			if return404.Load() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"clockApi":true,"decks":[{"deck":2,"source":"local","master":true,"playing":true,"currentBpm":132.0,"beat":12.5}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	now := time.Unix(1000, 0)
	dir := t.TempDir()
	raw, err := New(modkit.Env{State: dir, ModDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f := raw.(*Feature)
	f.webUIURL = srv.URL
	f.now = func() time.Time { return now }

	// First call uses /v1/enginex/clock.
	st1Raw, err := f.Status()
	if err != nil {
		t.Fatalf("Status 1: %v", err)
	}
	clk1 := st1Raw.(map[string]any)["clock"].(clockInfo)
	if clk1.Source != "enginex" || clk1.Deck != 2 || clk1.BPM != 132.0 || !clk1.Playing {
		t.Fatalf("unexpected clock 1: %+v", clk1)
	}
	if enginexHits.Load() != 1 {
		t.Fatalf("expected 1 enginex hit, got %d", enginexHits.Load())
	}

	// If /v1/enginex/clock returns 404, fall back to manual and negative-cache for 60s.
	return404.Store(true)
	st2Raw, err := f.Status()
	if err != nil {
		t.Fatalf("Status 2: %v", err)
	}
	if clk2 := st2Raw.(map[string]any)["clock"].(clockInfo); clk2.Source != "manual" {
		t.Fatalf("expected manual fallback on 404, got %+v", clk2)
	}
	now = now.Add(10 * time.Second)
	if _, err := f.Status(); err != nil {
		t.Fatalf("Status 3: %v", err)
	}
	if enginexHits.Load() != 2 {
		t.Fatalf("expected cached skip on enginex (2), got %d", enginexHits.Load())
	}

	// After 60s TTL expires, /v1/enginex/clock is probed again.
	return404.Store(false)
	now = now.Add(51 * time.Second)
	st4Raw, err := f.Status()
	if err != nil {
		t.Fatalf("Status 4: %v", err)
	}
	if clk4 := st4Raw.(map[string]any)["clock"].(clockInfo); clk4.Source != "enginex" {
		t.Fatalf("expected enginex recovery after TTL, got %+v", clk4)
	}
	if enginexHits.Load() != 3 {
		t.Fatalf("expected enginex re-probe after TTL, got %d", enginexHits.Load())
	}
}
