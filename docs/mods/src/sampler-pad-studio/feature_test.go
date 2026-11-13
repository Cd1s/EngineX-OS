package sampler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"os"
	"path/filepath"
	"runtime"
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

func TestImportWithoutFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state, "stereo.wav")
	audio := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if err := (WAV{Channels: 2, SampleWidth: 2, Rate: 44100, Data: audio}).Write(source); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Action(map[string]any{"action": "import", "pad": json.Number("1"), "source": source}); err != nil {
		t.Fatal(err)
	}
	out, err := ReadWAV(app.slots[0].Path)
	if err != nil || out.Channels != 2 || !bytes.Equal(out.Data, audio) {
		t.Fatalf("import changed PCM: %+v %v", out, err)
	}
}

func TestSparseWAVSliceReadsOnlyRange(t *testing.T) {
	state := t.TempDir()
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(state, "large.wav")
	if err := (WAV{Channels: 2, SampleWidth: 2, Rate: 44100}).Write(source); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(source, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, size)
	if _, err = f.WriteAt(header, 40); err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(44 + size); err != nil {
		t.Fatal(err)
	}
	const start = size/4 - 2
	want := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if _, err = f.WriteAt(want, 44+start*4); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = app.Action(map[string]any{"action": "slice", "pad": json.Number("1"), "source": source,
		"inSample": json.Number("16777214"), "outSample": json.Number("16777216"), "sampleRate": json.Number("44100")})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Fatalf("tiny slice allocated %d bytes", n)
	}
	out, err := ReadWAV(app.slots[0].Path)
	if err != nil || !bytes.Equal(out.Data, want) {
		t.Fatalf("range: %+v %v", out, err)
	}
}

func TestCompressedAudioWithoutFFmpeg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	source := filepath.Join(state, "source.flac")
	var encoded bytes.Buffer
	samples := []int32{1, 2, 3, 4, 5, 6}
	if err := testaudio.FLAC(&encoded, samples); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Action(map[string]any{"action": "import", "pad": json.Number("1"), "source": source}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		rate int
	}{
		{source, testaudio.Rate},
		{"../../../../internal/audioheader/testdata/01-lame-cbr-info-44100.mp3", 44100},
		{"../../../../pkg/webui/testdata/spotify-native-44100.ogg", 44100},
	} {
		t.Run(filepath.Ext(tc.path), func(t *testing.T) {
			whole, err := ReadAudio(context.Background(), tc.path, 0, -1, 0)
			if err != nil {
				t.Fatal(err)
			}
			part, err := ReadAudio(context.Background(), tc.path, 1, 4, tc.rate)
			if err != nil || part.Frames() != 3 || !bytes.Equal(part.Data, whole.Data[whole.FrameSize():4*whole.FrameSize()]) {
				t.Fatalf("exact slice: %v", err)
			}
			if _, err = ReadAudio(context.Background(), tc.path, 1, 4, 8001); err == nil {
				t.Fatal("rate mismatch accepted")
			}
		})
	}
	previous := app.slots[0]
	if err = os.WriteFile(source, []byte("fLaC corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Action(map[string]any{"action": "import", "pad": json.Number("1"), "source": source}); err == nil {
		t.Fatal("corrupt source accepted")
	}
	if app.slots[0] != previous {
		t.Fatal("failed import replaced bank slot")
	}
}

func TestFLACInheritedFieldsAndTruncation(t *testing.T) {
	// A constant stereo frame whose rate and bit depth inherit STREAMINFO.
	b := make([]byte, 42)
	copy(b, "fLaC\x80\x00\x00\x22")
	binary.BigEndian.PutUint16(b[8:], 16)
	binary.BigEndian.PutUint16(b[10:], 16)
	packed := uint64(8000)<<44 | uint64(1)<<41 | uint64(15)<<36 | 16
	binary.BigEndian.PutUint64(b[18:], packed)
	frame := []byte{0xff, 0xf8, 0x60, 0x10, 0, 15}
	var crc8 byte
	for _, v := range frame {
		crc8 ^= v
		for range 8 {
			if crc8&0x80 != 0 {
				crc8 = crc8<<1 ^ 0x07
			} else {
				crc8 <<= 1
			}
		}
	}
	frame = append(frame, crc8, 0, 0x40, 0, 0, 0x20, 0)
	var crc16 uint16
	for _, v := range frame {
		crc16 ^= uint16(v) << 8
		for range 8 {
			if crc16&0x8000 != 0 {
				crc16 = crc16<<1 ^ 0x8005
			} else {
				crc16 <<= 1
			}
		}
	}
	b = append(b, append(frame, byte(crc16>>8), byte(crc16))...)
	source := filepath.Join(t.TempDir(), "inherit.flac")
	if err := os.WriteFile(source, b, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := ReadAudio(context.Background(), source, 2, 5, 8000)
	if err != nil || w.Frames() != 3 || w.Channels != 2 || !bytes.Equal(w.Data, bytes.Repeat([]byte{0, 0x40, 0, 0x20}, 3)) {
		t.Fatalf("inherited stereo frame: %+v %v", w, err)
	}
	binary.BigEndian.PutUint64(b[18:], packed+1)
	if err := os.WriteFile(source, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadAudio(context.Background(), source, 0, -1, 0); err == nil {
		t.Fatal("truncated FLAC import accepted")
	}
}
