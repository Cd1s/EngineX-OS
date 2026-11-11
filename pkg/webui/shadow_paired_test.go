package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// #201: the Paired Player view is a view of the 4-deck layout. Setting it turns
// the layout on with it, turning the layout off clears it, and a key left in
// settings.conf without the layout means nothing.
func TestPairedViewSetting(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{UIDir: dir, StateDir: t.TempDir()}
	srv := httptest.NewServer(New(cfg, nil))
	defer srv.Close()
	conf := func() []string {
		b, _ := os.ReadFile(filepath.Join(dir, "settings.conf"))
		return splitLines(b)
	}

	if _, v := uiPost(t, srv.URL, ""); v.PairedView || v.FourDeckWaveform {
		t.Fatalf("default is the stock 2-deck view (Layer A / B): %+v", v)
	}
	if code, v := uiPost(t, srv.URL, "pairedView=1"); code != 200 || !v.PairedView || !v.FourDeckWaveform {
		t.Fatalf("pairedView=1: %d paired=%v fourDeck=%v", code, v.PairedView, v.FourDeckWaveform)
	}
	if l := conf(); !slices.Contains(l, "four_deck_waveform=1") || !slices.Contains(l, "paired_view=1") {
		t.Fatalf("not saved: %q", l)
	}
	srv2 := httptest.NewServer(New(cfg, nil))
	defer srv2.Close()
	if _, v := uiPost(t, srv2.URL, ""); !v.PairedView || !v.FourDeckWaveform {
		t.Fatalf("not reloaded: paired=%v fourDeck=%v", v.PairedView, v.FourDeckWaveform)
	}
	// Back to the 4-deck view: no restart, the layout stays.
	if code, v := uiPost(t, srv.URL, "pairedView=0"); code != 200 || v.PairedView || !v.FourDeckWaveform {
		t.Fatalf("pairedView=0: %d paired=%v fourDeck=%v", code, v.PairedView, v.FourDeckWaveform)
	}
	if l := conf(); slices.Contains(l, "paired_view=1") || !slices.Contains(l, "four_deck_waveform=1") {
		t.Fatalf("pairedView=0 saved as %q", l)
	}
	uiPost(t, srv.URL, "pairedView=1")
	if code, v := uiPost(t, srv.URL, "fourDeckWaveform=0"); code != 200 || v.PairedView || v.FourDeckWaveform {
		t.Fatalf("4-Deck Waveform off keeps the paired view: paired=%v fourDeck=%v", v.PairedView, v.FourDeckWaveform)
	}
	if l := conf(); slices.ContainsFunc(l, func(s string) bool {
		return strings.HasPrefix(s, "paired_view") || strings.HasPrefix(s, "four_deck_waveform")
	}) {
		t.Fatalf("stock again, yet saved %q", l)
	}
	// A stray key alone (hand edit, older writer) is not the paired view.
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("lang=en\npaired_view=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv3 := httptest.NewServer(New(cfg, nil))
	defer srv3.Close()
	if _, v := uiPost(t, srv3.URL, ""); v.PairedView || v.FourDeckWaveform {
		t.Fatalf("paired_view without four_deck_waveform: paired=%v fourDeck=%v", v.PairedView, v.FourDeckWaveform)
	}
	for _, bad := range []string{"pairedView=yes", "pairedView=2"} {
		if code, _ := uiPost(t, srv.URL, bad); code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}

// #201: the other player's active layer reaches this player's QML on every
// shadow path (Engine, auto, CDJ display), an empty active deck included, and a
// layer switch alone wakes the long poll (a new version).
func TestShadowPeerActiveLayer(t *testing.T) {
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = "/run/sc5000-assignments/role" }()
	os.WriteFile(fourDeckRolePath, []byte("13\n"), 0o644)
	for _, mode := range []string{"engine", "auto", "cdj"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), t.TempDir()
			srv := httptest.NewServer(s)
			defer srv.Close()
			uiPost(t, srv.URL, "fourDeckWaveform=1&shadowMode="+mode)
			now := time.Now()
			// The peer (role 24) on Layer B: deck 4 active and empty, deck 2 idle.
			s.shadow.mu.Lock()
			s.shadow.takePeer("peer-b", []deckState{{Deck: 2}, {Deck: 4, Active: true}}, now)
			ver := s.shadow.ver
			s.shadow.mu.Unlock()
			active := func() []int {
				role, decks := shadowGetJSON(t, srv.URL+"/v1/sc5000/shadow")
				if role != "13" {
					t.Fatalf("role %q", role)
				}
				var on []int
				for _, d := range decks {
					if d.Active {
						on = append(on, d.Deck)
					}
				}
				return on
			}
			if got := active(); !slices.Equal(got, []int{4}) {
				t.Fatalf("peer on Layer B: active %v, want [4]", got)
			}
			// The peer presses Layer: only the flag moves.
			s.shadow.mu.Lock()
			s.shadow.takePeer("peer-b", []deckState{{Deck: 2, Active: true}, {Deck: 4}}, now.Add(40*time.Millisecond))
			bumped := s.shadow.ver > ver
			s.shadow.mu.Unlock()
			if !bumped {
				t.Fatal("a layer switch alone did not wake the long poll")
			}
			if got := active(); !slices.Equal(got, []int{2}) {
				t.Fatalf("peer on Layer A: active %v, want [2]", got)
			}
		})
	}
}
