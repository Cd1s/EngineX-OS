package webui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// bjBoard makes this process one board (a lower-case device-tree name such as "jp07") for the
// next New, which reads the board once; the previous device tree comes back after the test.
func bjBoard(t *testing.T, dt string) {
	t.Helper()
	old := deviceCompatiblePath
	deviceCompatiblePath = filepath.Join(t.TempDir(), "compatible")
	t.Cleanup(func() { deviceCompatiblePath = old })
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,"+dt+"\x00rockchip,rk3288\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The Beat Jump pad mode (Shift + LOOP) of the four players is off by default, like Mixstream's on-screen bank (the
// players have physical Beat Jump buttons): no board starts with it on and none writes a line for the default.
// Only beat_jump_pads=1 turns it on, and that comes back after a restart.
func TestBeatJumpPadsOffByDefaultOnEveryBoard(t *testing.T) {
	for _, tc := range []struct {
		dt, board string
		player    bool
	}{
		{"jp07", "SC5000", true}, {"jp08", "SC5000M", true}, {"jp13", "SC6000", true}, {"jp14", "SC6000M", true},
		{"jp11", "PRIMEGO", false}, {"jc11", "PRIME4", false}, {"nh08", "MIXSTREAMPRO", false}, {"jp21", "SCLIVE4", false},
	} {
		t.Run(tc.board, func(t *testing.T) {
			bjBoard(t, tc.dt)
			s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
			t.Cleanup(s.Close)
			if s.board != tc.board || playerBoards[s.board] != tc.player {
				t.Fatalf("board %q (player %v), want %q (player %v)", s.board, playerBoards[s.board], tc.board, tc.player)
			}
			s.uiMu.Lock()
			defer s.uiMu.Unlock()
			conf := filepath.Join(s.uiDir(), "settings.conf")
			// No settings.conf at all: off.
			v := s.loadUI()
			if v.BeatJumpPads {
				t.Fatal("beatJumpPads is on by default")
			}
			// Saving the default writes no beat_jump_pads line (a missing key is off), and it loads off again.
			if err := s.saveUI(v); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(conf)
			if strings.Contains(string(b), "beat_jump_pads") || s.loadUI().BeatJumpPads {
				t.Fatalf("the default saved as:\n%s", b)
			}
			// On is saved as beat_jump_pads=1 and survives a reload; Off removes the line again.
			v.BeatJumpPads = true
			if err := s.saveUI(v); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(conf)
			if !strings.Contains(string(b), "beat_jump_pads=1\n") || strings.Contains(string(b), "beat_jump_pads=0") || !s.loadUI().BeatJumpPads {
				t.Fatalf("On saved as:\n%s", b)
			}
			v.BeatJumpPads = false
			if err := s.saveUI(v); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(conf)
			if strings.Contains(string(b), "beat_jump_pads") || s.loadUI().BeatJumpPads {
				t.Fatalf("Off saved as:\n%s", b)
			}
		})
	}
}

// A player with a saved beat_jump_pads=1 starts with the mode on; no value, 0 and anything else stay off. The same
// file means the same on every board (the key has no board test), so Mixstream's bank is unchanged.
func TestBeatJumpPadsSavedValueTurnsItOn(t *testing.T) {
	for _, tc := range []struct {
		dt, board string
	}{
		{"jp07", "SC5000"}, {"jp08", "SC5000M"}, {"jp13", "SC6000"}, {"jp14", "SC6000M"}, {"nh08", "MIXSTREAMPRO"},
	} {
		t.Run(tc.board, func(t *testing.T) {
			bjBoard(t, tc.dt)
			s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
			t.Cleanup(s.Close)
			s.uiMu.Lock()
			defer s.uiMu.Unlock()
			conf := filepath.Join(s.uiDir(), "settings.conf")
			if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
				t.Fatal(err)
			}
			for text, want := range map[string]bool{
				"beat_jump_pads=1\n": true, "led_brightness=60\nbeat_jump_pads=1\n": true, "beat_jump_pads=true\n": true,
				"beat_jump_pads=0\n": false, "beat_jump_pads=\n": false, "led_brightness=60\n": false, "": false,
			} {
				if err := os.WriteFile(conf, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				if got := s.loadUI().BeatJumpPads; got != want {
					t.Errorf("settings.conf %q: beatJumpPads=%v, want %v", text, got, want)
				}
			}
		})
	}
}

// Turning the mode on from the console or the deck's Settings (POST beatJumpPads=1) on a player is saved as
// beat_jump_pads=1, shows in the settings the controller polls, and is still on in a new process; turning it off again
// leaves the file without the key.
func TestBeatJumpPadsTurnedOnOnAPlayer(t *testing.T) {
	for _, dt := range []string{"jp07", "jp08", "jp13", "jp14"} {
		t.Run(dt, func(t *testing.T) {
			bjBoard(t, dt)
			cfg := Config{UIDir: t.TempDir(), StateDir: t.TempDir()}
			srv := httptest.NewServer(New(cfg, nil))
			defer srv.Close()
			if code, v := uiPost(t, srv.URL, "ledBrightness=100"); code != 200 || v.BeatJumpPads || !v.Player {
				t.Fatalf("default: code=%d player=%v beatJumpPads=%v", code, v.Player, v.BeatJumpPads)
			}
			if code, v := uiPost(t, srv.URL, "beatJumpPads=1"); code != 200 || !v.BeatJumpPads {
				t.Fatalf("turned on: code=%d beatJumpPads=%v", code, v.BeatJumpPads)
			}
			b, _ := os.ReadFile(filepath.Join(cfg.UIDir, "settings.conf"))
			if !strings.Contains(string(b), "beat_jump_pads=1\n") {
				t.Fatalf("On saved as:\n%s", b)
			}
			again := New(cfg, nil)
			t.Cleanup(again.Close)
			again.uiMu.Lock()
			on := again.loadUI().BeatJumpPads
			again.uiMu.Unlock()
			if !on {
				t.Fatal("On did not survive a restart")
			}
			if code, v := uiPost(t, srv.URL, "beatJumpPads=0"); code != 200 || v.BeatJumpPads {
				t.Fatalf("turned off: code=%d beatJumpPads=%v", code, v.BeatJumpPads)
			}
			b, _ = os.ReadFile(filepath.Join(cfg.UIDir, "settings.conf"))
			if strings.Contains(string(b), "beat_jump_pads") {
				t.Fatalf("Off saved as:\n%s", b)
			}
		})
	}
}

// loadUI also runs on background paths (retryExport -> tryExport). It must use the board New read, never the
// device tree again: a second read raced with tests that point deviceCompatiblePath elsewhere (82 races under
// -race on the first Beat Jump pads branch).
func TestLoadUIUsesTheBoardReadAtStart(t *testing.T) {
	bjBoard(t, "jp07")
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,jc11\x00rockchip,rk3288\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	if s.board != "SC5000" || v.BeatJumpPads {
		t.Fatalf("board %q beatJumpPads %v: loadUI read the device tree again", s.board, v.BeatJumpPads)
	}
}

// Settings Copy carries beatJumpPads between the players (and between Mixstream boards); a board without
// either keeps its own value.
func TestSettingsCopyBeatJumpPadsBetweenPlayers(t *testing.T) {
	payload := map[string]json.RawMessage{"beatJumpPads": json.RawMessage("false")}
	for _, board := range []string{"SC5000", "SC5000M", "SC6000", "SC6000M", "MIXSTREAMPRO"} {
		got, skipped, err := copyUIMerge(uiSettings{BeatJumpPads: true}, payload, board)
		if err != nil || got.BeatJumpPads || slices.Contains(skipped, "beatJumpPads") {
			t.Fatalf("%s: got %v skipped %v err %v", board, got.BeatJumpPads, skipped, err)
		}
	}
	for _, board := range []string{"PRIME4", "PRIMEGO", "SCLIVE4", "PRIME4G2"} {
		got, skipped, err := copyUIMerge(uiSettings{BeatJumpPads: true}, payload, board)
		if err != nil || !got.BeatJumpPads || !slices.Contains(skipped, "beatJumpPads") {
			t.Fatalf("%s: got %v skipped %v err %v", board, got.BeatJumpPads, skipped, err)
		}
	}
	// The export of a player keeps its value, On and Off.
	if string(copyUIExport(uiSettings{BeatJumpPads: true})["beatJumpPads"]) != "true" ||
		string(copyUIExport(uiSettings{})["beatJumpPads"]) != "false" {
		t.Fatal("beatJumpPads not exported")
	}
}
