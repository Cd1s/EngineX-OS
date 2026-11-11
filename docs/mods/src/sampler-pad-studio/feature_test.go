package sampler

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

func TestExactSliceAndReferenceOnlyClear(t *testing.T) {
	state := t.TempDir()
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	var audio bytes.Buffer
	for i := 0; i < 100; i++ {
		binary.Write(&audio, binary.LittleEndian, int16(i))
	}
	source := filepath.Join(state, "source.wav")
	if err := (WAV{Channels: 1, SampleWidth: 2, Rate: 44100, Data: audio.Bytes()}).Write(source); err != nil {
		t.Fatal(err)
	}
	num := func(s string) json.Number { return json.Number(s) }
	res, err := app.Action(map[string]any{"action": "slice", "pad": num("8"), "source": source, "inSample": num("10"), "outSample": num("30"), "sampleRate": num("44100")})
	if err != nil {
		t.Fatal(err)
	}
	exported := res.(map[string]any)["slots"].([]slot)[7].Path
	w, err := ReadWAV(exported)
	if err != nil || w.Frames() != 20 || !bytes.Equal(w.Data, audio.Bytes()[20:60]) {
		t.Fatalf("slice: %v %d", err, w.Frames())
	}
	if _, err := app.Action(map[string]any{"action": "rename", "pad": num("8"), "name": "Snare"}); err != nil || app.slots[7].Name != "Snare" {
		t.Fatalf("rename: %v", err)
	}
	if _, err := app.Action(map[string]any{"action": "clear", "pad": num("8")}); err != nil || app.slots[7].Path != "" {
		t.Fatalf("clear: %v", err)
	}
	for _, p := range []string{exported, source} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("clear must not delete %s", p)
		}
	}
	if _, err := app.Action(map[string]any{"action": "slice", "pad": num("1"), "source": source, "inSample": num("10"), "outSample": num("300"), "sampleRate": num("44100")}); !modkit.IsBad(err) {
		t.Fatalf("out-of-bounds slice: %v", err)
	}
	for _, bad := range []map[string]any{{"action": "rename", "pad": num("9"), "name": "x"}, {"action": "rename", "pad": num("1.0"), "name": "x"},
		{"action": "rename", "pad": true, "name": "x"}, {"action": "rename", "pad": num("1"), "name": " "}, {"action": "explode", "pad": num("1")}} {
		if _, err := app.Action(bad); !modkit.IsBad(err) {
			t.Errorf("%v: expected 400, got %v", bad, err)
		}
	}
	again, err := newFeature(state)
	if err != nil || again.slots[7].Name != "Pad 8" {
		t.Fatalf("bank reload: %v", err)
	}
}
