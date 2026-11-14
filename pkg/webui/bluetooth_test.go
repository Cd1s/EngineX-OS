package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func bluetoothCall(t *testing.T, s *Server, method, target, body string) (int, bluetoothStatus) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleBluetooth(w, httptest.NewRequest(method, target, strings.NewReader(body)))
	var st bluetoothStatus
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("bad response %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, st
}

func settingsConf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// Pair n of the master output is bt_out=n, and a model names only the pairs measured on a deck.
func TestBluetoothLayersByModel(t *testing.T) {
	for board, want := range map[string][]string{
		"SC5000": {"Layer A", "Layer B"}, "SC5000M": {"Layer A", "Layer B"}, "SC6000": {"Layer A", "Layer B"}, "SC6000M": {"Layer A", "Layer B"},
		"PRIME4":       {"Master", "Booth", "Headphones", "Zone"},
		"PRIME2":       {"Master", "Headphones"},
		"MIXSTREAMPRO": {"Master", "Headphones"},
		// not measured yet: Master only
		"PRIMEGO": {"Master"}, "SCLIVE4": {"Master"}, "SCLIVE2": {"Master"}, "PRIME4PLUS": {"Master"}, "PRIMEGOPLUS": {"Master"},
		"MIXSTREAMPROPLUS": {"Master"}, "MIXSTREAMPROGO": {"Master"}, "PRIME4G2": {"Master"}, "SYSTEMONE": {"Master"}, "": {"Master"},
	} {
		if got := bluetoothLayers(board); !slices.Equal(got, want) {
			t.Errorf("%q: layers %v, want %v", board, got, want)
		}
	}
}

func TestBluetoothAssignLayerIsSavedForTheHook(t *testing.T) {
	asDeck(t, "inmusic,jc11") // PRIME 4
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)

	if code, st := bluetoothCall(t, s, "GET", "/v1/sc5000/bluetooth", ""); code != 200 || st.AssignLayer != "Master" || !slices.Equal(st.Layers, []string{"Master", "Booth", "Headphones", "Zone"}) {
		t.Fatalf("default: %d %+v", code, st)
	}
	if conf := settingsConf(t, dir); strings.Contains(conf, "bt_out") {
		t.Fatalf("the default is written as a setting: %q", conf)
	}

	// JSON body and query string both pick a layer; the hook gets its index.
	for _, c := range []struct{ target, body, layer, line string }{
		{"/v1/sc5000/bluetooth", `{"assignLayer":"Headphones"}`, "Headphones", "bt_out=2\n"},
		{"/v1/sc5000/bluetooth?assignLayer=Zone", "", "Zone", "bt_out=3\n"},
		{"/api/bluetooth?assignLayer=Booth", "", "Booth", "bt_out=1\n"},
	} {
		if code, st := bluetoothCall(t, s, "POST", c.target, c.body); code != 200 || st.AssignLayer != c.layer {
			t.Fatalf("%s %s: %d %+v", c.target, c.body, code, st)
		}
		if conf := settingsConf(t, dir); !strings.Contains(conf, "\n"+c.line) && !strings.HasPrefix(conf, c.line) {
			t.Fatalf("%s: settings.conf %q lacks %q", c.layer, conf, c.line)
		}
	}

	// It survives a restart, and a save of any other setting (saveUI rebuilds the file from its struct).
	s = New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	w := httptest.NewRecorder()
	s.uiUpdate(w, httptest.NewRequest("POST", "/api/ui?waveform=rgb", nil))
	if w.Code != 200 {
		t.Fatalf("ui save: %d %s", w.Code, w.Body.String())
	}
	if conf := settingsConf(t, dir); !strings.Contains(conf, "bt_out=1\n") || !strings.Contains(conf, "waveform=rgb\n") {
		t.Fatalf("settings.conf after another save: %q", conf)
	}
	if code, st := bluetoothCall(t, s, "GET", "/v1/sc5000/bluetooth", ""); code != 200 || st.AssignLayer != "Booth" {
		t.Fatalf("after restart: %d %+v", code, st)
	}

	// The first pair is the default again: no line, as for every setting at its default.
	if code, st := bluetoothCall(t, s, "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"Master"}`); code != 200 || st.AssignLayer != "Master" {
		t.Fatalf("back to Master: %d %+v", code, st)
	}
	if conf := settingsConf(t, dir); strings.Contains(conf, "bt_out") {
		t.Fatalf("Master left a setting: %q", conf)
	}
}

func TestBluetoothPlayerPicksALayer(t *testing.T) {
	asDeck(t, "inmusic,jp07") // SC5000
	dir := t.TempDir()
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	if code, st := bluetoothCall(t, s, "GET", "/v1/sc5000/bluetooth", ""); code != 200 || st.AssignLayer != "Layer A" {
		t.Fatalf("default: %d %+v", code, st)
	}
	if code, st := bluetoothCall(t, s, "POST", "/v1/sc5000/bluetooth?assignLayer=Layer+B", ""); code != 200 || st.AssignLayer != "Layer B" {
		t.Fatalf("Layer B: %d %+v", code, st)
	}
	if conf := settingsConf(t, dir); !strings.Contains(conf, "bt_out=1\n") {
		t.Fatalf("settings.conf %q", conf)
	}
}

// A model offers only what was measured on it; a refused request changes nothing.
func TestBluetoothRefusesWhatTheModelDoesNotHave(t *testing.T) {
	for _, c := range []struct{ board, method, target, body string }{
		{"inmusic,jc16", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"Zone"}`},       // PRIME 2 has Master and Headphones
		{"inmusic,jc11", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"Layer A"}`},    // a player's name on a console
		{"inmusic,jp07", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"Master"}`},     // and the other way
		{"inmusic,jp11", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"Headphones"}`}, // PRIME GO: not measured
		{"inmusic,jc11", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"CH 1"}`},       // the old names
		{"inmusic,jc11", "POST", "/v1/sc5000/bluetooth", `{"assignLayer":"master"}`},     // exact names
		{"inmusic,jc11", "POST", "/v1/sc5000/bluetooth", `{"mode":"source"}`},            // the source mode is gone
		{"inmusic,jc11", "POST", "/v1/sc5000/bluetooth", `{invalid json`},
		{"inmusic,jc11", "DELETE", "/v1/sc5000/bluetooth", ""},
	} {
		asDeck(t, c.board)
		dir := t.TempDir()
		s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
		if code, _ := bluetoothCall(t, s, c.method, c.target, c.body); code < 400 || code >= 500 {
			t.Errorf("%s %s %s: status %d, want a client error", c.board, c.method, c.body, code)
		}
		if conf := settingsConf(t, dir); strings.Contains(conf, "bt_out") {
			t.Errorf("%s %s: refused request still wrote %q", c.board, c.body, conf)
		}
	}
}

// The setting is read back only within the pairs a model offers; anything else is the first pair, in the file the hook
// reads as well as in the answer: a pair this model has no name for is removed from settings.conf (the hook would send
// it), and a value the hook cannot use anyway (negative, not a number) never reaches Go.
func TestBluetoothIgnoresABadSavedPair(t *testing.T) {
	asDeck(t, "inmusic,jc16") // PRIME 2: two layers
	for _, c := range []struct {
		saved  string
		healed bool
	}{{"bt_out=9\n", false}, {"bt_out=-1\n", false}, {"bt_out=x\n", false}, {"bt_out=3\n", true}, {"bt_out=2\nled_brightness=50\n", true}} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(c.saved), 0o644); err != nil {
			t.Fatal(err)
		}
		s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
		if code, st := bluetoothCall(t, s, "GET", "/v1/sc5000/bluetooth", ""); code != 200 || st.AssignLayer != "Master" {
			t.Errorf("%q: %d %+v, want Master", c.saved, code, st)
		}
		conf := settingsConf(t, dir)
		if c.healed && strings.Contains(conf, "bt_out") {
			t.Errorf("%q: the pair this model does not have is still in settings.conf: %q", c.saved, conf)
		}
		if c.healed && strings.Contains(c.saved, "led_brightness=50") && !strings.Contains(conf, "led_brightness=50") {
			t.Errorf("%q: another setting was lost: %q", c.saved, conf)
		}
		if !c.healed && conf != c.saved {
			t.Errorf("%q: a GET changed settings.conf to %q", c.saved, conf)
		}
	}
}
