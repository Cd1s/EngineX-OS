package webui

import (
	"encoding/json"
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

// The players' Beat Jump pad mode (Shift + LOOP) is on by default on the four players only; Off survives a
// restart there as beat_jump_pads=0, and no other board gets a new line.
func TestBeatJumpPadsOnByDefaultOnTheFourPlayersOnly(t *testing.T) {
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
			if s.board != tc.board {
				t.Fatalf("board %q, want %q", s.board, tc.board)
			}
			s.uiMu.Lock()
			defer s.uiMu.Unlock()
			v := s.loadUI()
			if v.BeatJumpPads != tc.player {
				t.Fatalf("default beatJumpPads=%v, want %v", v.BeatJumpPads, tc.player)
			}
			conf := filepath.Join(s.uiDir(), "settings.conf")
			v.BeatJumpPads = false
			if err := s.saveUI(v); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(conf)
			if strings.Contains(string(b), "beat_jump_pads=0\n") != tc.player || strings.Contains(string(b), "beat_jump_pads=1") {
				t.Fatalf("Off saved as:\n%s", b)
			}
			if s.loadUI().BeatJumpPads {
				t.Fatal("Off came back On")
			}
			v.BeatJumpPads = true
			if err := s.saveUI(v); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(conf)
			if !strings.Contains(string(b), "beat_jump_pads=1\n") || strings.Contains(string(b), "beat_jump_pads=0") || !s.loadUI().BeatJumpPads {
				t.Fatalf("On saved as:\n%s", b)
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
	if s.board != "SC5000" || !v.BeatJumpPads {
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
	// The export of a player keeps its value (also the default On).
	if string(copyUIExport(uiSettings{BeatJumpPads: true})["beatJumpPads"]) != "true" {
		t.Fatal("beatJumpPads not exported")
	}
}
