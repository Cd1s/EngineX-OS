package enginedb_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"reflect"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestQCompressAndAbsentCues(t *testing.T) {
	raw := []byte("Engine data")
	b, e := enginedb.QCompress(raw)
	if e != nil {
		t.Fatal(e)
	}
	if binary.BigEndian.Uint32(b) != uint32(len(raw)) {
		t.Fatal("missing Qt length prefix")
	}
	got, e := enginedb.QUncompress(b, 100)
	if e != nil || !bytes.Equal(raw, got) {
		t.Fatal(got, e)
	}
	for _, bad := range [][]byte{b[:3], append(append([]byte(nil), b...), 0), append([]byte{0, 0, 0, 1}, b[4:]...)} {
		if _, e = enginedb.QUncompress(bad, 100); e == nil {
			t.Fatal("accepted invalid qCompress")
		}
	}
	if _, e = enginedb.QUncompress(b, 1); e == nil {
		t.Fatal("unbounded decompression")
	}
	p, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 44100, Duration: 10}, nil)
	if e != nil {
		t.Fatal(e)
	}
	q, e := enginedb.QUncompress(p.QuickCues, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if len(q) != 129 {
		t.Fatalf("empty eight-slot layout: %d", len(q))
	}
	for i := 0; i < 8; i++ {
		if binary.BigEndian.Uint64(q[9+i*13:]) != 0xbff0000000000000 {
			t.Fatal("cue synthesized")
		}
	}
	if p.Complete {
		t.Fatal("empty analysis marked complete")
	}
}

func TestSmartMatchWheel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{{"8A", "9A", true}, {"8A", "7A", true}, {"8A", "8B", true}, {"12A", "1A", true}, {"8A", "9B", false}, {"8A", "10A", false}, {"?", "8A", false}, {"Am", "C", true}, {"A minor", "G major", false}} {
		if got := enginedb.CompatibleKeys(tc.a, tc.b); got != tc.want {
			t.Errorf("%s/%s = %v", tc.a, tc.b, got)
		}
	}
	xs := []rekordbox.Track{{ID: 1, BPM: 120, Key: "8A"}, {ID: 2, BPM: 124, Key: "9A"}, {ID: 3, BPM: 124.01, Key: "8A"}, {ID: 4, BPM: 116, Key: "8B"}, {ID: 5, BPM: 120, Key: "bad"}}
	if got := enginedb.SmartMatch(xs, 1); !reflect.DeepEqual(got, []uint32{2, 4}) {
		t.Fatal(got)
	}
}

func TestExactContainerSamplesAndLegacyCueColor(t *testing.T) {
	a := &rekordbox.Analysis{Cues: []rekordbox.Cue{{HotCue: 2, TimeMS: 500}}}
	p, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 48000, Duration: 1, SampleCount: 48001}, a)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := enginedb.QUncompress(p.TrackData, 100)
	if e != nil {
		t.Fatal(e)
	}
	if got := binary.BigEndian.Uint64(raw[8:]); got != 48001 {
		t.Fatal("container count lost", got)
	}
	q, e := enginedb.QUncompress(p.QuickCues, 1000)
	if e != nil {
		t.Fatal(e)
	}
	const emptySlot = 13
	slotB := q[8+emptySlot:]
	if !bytes.Equal(slotB[9:13], []byte{255, 0, 255, 0}) {
		t.Fatal("legacy cue is transparent", slotB[:13])
	}
	if !bytes.Equal(q[8+9:8+13], []byte{0, 0, 0, 0}) {
		t.Fatal("empty A was painted")
	}
	// GL-013 (Seabass #56): Explicit RGB cue/loop color with Alpha=0 is forced opaque (Alpha=255).
	pOpaque, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 48000, Duration: 2}, &rekordbox.Analysis{
		Cues: []rekordbox.Cue{{HotCue: 1, TimeMS: 100, EndMS: 500, Loop: true, Color: [4]byte{0, 10, 20, 30}}},
	})
	if e != nil {
		t.Fatal(e)
	}
	qOpaque, e := enginedb.QUncompress(pOpaque.QuickCues, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(qOpaque[8+9:8+13], []byte{255, 10, 20, 30}) {
		t.Fatalf("expected opaque Alpha=255 on hot cue, got %v", qOpaque[8+9:8+13])
	}
	if !bytes.Equal(pOpaque.Loops[8+1+18:8+1+22], []byte{255, 10, 20, 30}) {
		t.Fatalf("expected opaque Alpha=255 on loop, got %v", pOpaque.Loops[8+1+18:8+1+22])
	}
}

func TestWaveformIndexAndShortAnalysisBoundaries(t *testing.T) {
	for _, entry := range []uint32{0, 0x80000000, 0xffffffff} {
		_, err := enginedb.EncodePerformance(rekordbox.Track{}, &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWAV", EntryBytes: entry, Data: []byte{1}}}})
		if err == nil {
			t.Fatalf("accepted entry size %d", entry)
		}
	}
	data := make([]byte, 3_000_001) // i*n exceeds a signed 32-bit int in later bins.
	data[len(data)-1] = 31
	cases := []struct {
		name    string
		a       rekordbox.Analysis
		minimum uint64
	}{
		{"large", rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWAV", EntryBytes: 1, Data: data}}}, uint64(len(data))},
		{"short", rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWAV", EntryBytes: 1, Data: []byte{31}}}}, 1024},
		{"beat", rekordbox.Analysis{Beats: []rekordbox.Beat{{TimeMS: 2000}}}, 88201},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := enginedb.EncodePerformance(rekordbox.Track{}, &tc.a)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := enginedb.QUncompress(p.TrackData, 1000)
			if err != nil || binary.BigEndian.Uint64(raw[8:]) < tc.minimum {
				t.Fatalf("missing positive sample fallback: %x %v", raw, err)
			}
			if len(p.Overview) > 0 {
				raw, err = enginedb.QUncompress(p.Overview, 10000)
				if err != nil {
					t.Fatal(err)
				}
				step := math.Float64frombits(binary.BigEndian.Uint64(raw[16:]))
				if step <= 0 {
					t.Fatal("nonpositive samples per bin", step)
				}
				if !bytes.Equal(raw[24+1023*3:24+1024*3], []byte{255, 255, 255}) {
					t.Fatal("last waveform peak lost")
				}
			}
		})
	}
}

// #101: Engine 5.3.0 ignores all-zero waveform peaks, so imported tracks drew
// lower than Engine-analysed ones.
func TestTrackDataWaveformPeaks(t *testing.T) {
	peaks := func(a *rekordbox.Analysis) []float64 {
		t.Helper()
		p, e := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 44100, Duration: 10}, a)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := enginedb.QUncompress(p.TrackData, 100)
		if e != nil || len(raw) != 68 {
			t.Fatalf("trackData %d bytes: %v", len(raw), e)
		}
		var v [6]float64
		for i := range v {
			v[i] = math.Float64frombits(binary.BigEndian.Uint64(raw[20+8*i:]))
		}
		if v[3] != 0 || v[4] != 0 || v[5] != 0 {
			t.Fatal("stem peaks written", v)
		}
		return v[:3]
	}
	if got := peaks(nil); !reflect.DeepEqual(got, []float64{0.68, 0.40, 0.18}) {
		t.Fatal("default peaks", got)
	}
	pwv7 := rekordbox.Waveform{Tag: "PWV7", EntryBytes: 3, Data: []byte{0, 0, 0, 102, 123, 255}}
	got := peaks(&rekordbox.Analysis{Waveforms: []rekordbox.Waveform{pwv7}})
	if math.Abs(got[0]-0.6732) > 1e-9 || math.Abs(got[1]-0.4305) > 1e-9 || math.Abs(got[2]-0.408) > 1e-9 {
		t.Fatal("PWV7 peaks", got)
	}
	silent := rekordbox.Waveform{Tag: "PWV7", EntryBytes: 3, Data: []byte{0, 0, 0}}
	if got := peaks(&rekordbox.Analysis{Waveforms: []rekordbox.Waveform{silent}}); got[0] != 0.68 {
		t.Fatal("silent band must keep the default", got)
	}
}

func TestTrackDataPeaksUpgrade(t *testing.T) {
	old := bytes.Repeat([]byte{7}, 20)
	old = append(old, bytes.Repeat([]byte{0}, 24)...) // 5.0.4: 44 bytes, zero peaks
	overview := append(bytes.Repeat([]byte{0}, 24), 100, 50, 25, 10, 60, 5, 0, 0, 0)
	got := enginedb.TrackDataPeaks(old, overview)
	if len(got) != 68 || !bytes.Equal(got[:20], old[:20]) || !bytes.Equal(got[44:], bytes.Repeat([]byte{0}, 24)) {
		t.Fatalf("upgrade %x", got)
	}
	for c, want := range []float64{100 * 0.0066, 60 * 0.0035, 25 * 0.0016} {
		if v := math.Float64frombits(binary.BigEndian.Uint64(got[20+8*c:])); math.Abs(v-want) > 1e-12 {
			t.Fatal("band", c, v, want)
		}
	}
	// Engine's own peaks are kept as they are.
	if again := enginedb.TrackDataPeaks(got, nil); !bytes.Equal(again, got) {
		t.Fatal("rewrote existing peaks")
	}
}

// #178: Rekordbox plays Xing/Info frames as audio and ignores LAME gapless delay,
// while Engine drops the Xing/Info frame and skips LAME start_pad + 529.
func TestEncodePerformanceMP3StartOffsetAndClamping(t *testing.T) {
	if _, err := enginedb.EncodePerformance(rekordbox.Track{StartOffsetSamples: 2257}, nil); err == nil {
		t.Fatal("accepted start offset without real sample rate")
	}
	for _, sr := range []int{44100, 48000} {
		firstBeatMS := uint32(50)
		if sr == 48000 {
			firstBeatMS = 46
		}
		for _, tc := range []struct {
			name   string
			offset int
		}{
			{"lame", 2257},
			{"xing", 1152},
			{"headerless", 0},
		} {
			a := &rekordbox.Analysis{
				Beats: []rekordbox.Beat{
					{Number: 4, BPM: 120, TimeMS: firstBeatMS},
					{Number: 1, BPM: 120, TimeMS: firstBeatMS + 500},
					{Number: 2, BPM: 120, TimeMS: firstBeatMS + 1000},
				},
				Cues: []rekordbox.Cue{
					{HotCue: 1, TimeMS: 20, Name: "EarlyHot"},
					{HotCue: 2, TimeMS: 550, Name: "BeatHot"},
					{HotCue: 3, TimeMS: 550, EndMS: 1550, Loop: true, Name: "HotLoop"},
					{HotCue: 0, TimeMS: 10, Name: "EarlyMain"},
					{HotCue: 0, TimeMS: 20, EndMS: 520, Loop: true, Name: "EarlyMemLoop"},
				},
				Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 80, 60}}},
			}
			p, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: sr, Duration: 8, StartOffsetSamples: tc.offset}, a)
			if err != nil {
				t.Fatalf("sr=%d %s: %v", sr, tc.name, err)
			}
			beats, err := enginedb.QUncompress(p.Beats, 4096)
			if err != nil {
				t.Fatal(err)
			}
			wantB0 := float64(firstBeatMS)*float64(sr)/1000 - float64(tc.offset)
			wantStep := 0.5 * float64(sr)
			for g := 0; g < 2; g++ {
				base := 17 + g*(8+3*24) + 8
				b0 := math.Float64frombits(binary.LittleEndian.Uint64(beats[base:]))
				b1 := math.Float64frombits(binary.LittleEndian.Uint64(beats[base+24:]))
				b2 := math.Float64frombits(binary.LittleEndian.Uint64(beats[base+48:]))
				if math.Abs(b0-wantB0) > 1e-6 || math.Abs((b1-b0)-wantStep) > 1e-6 || math.Abs((b2-b1)-wantStep) > 1e-6 {
					t.Fatalf("sr=%d %s grid %d: got (%v, %v, %v), want b0=%v step=%v", sr, tc.name, g, b0, b1, b2, wantB0, wantStep)
				}
			}
			// Negative first anchor (e.g. 50ms*44.1 - 2257 = -52 samples) must pass Engine display validation.
			if _, err := enginedb.ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
				t.Fatalf("sr=%d %s display validation rejected shifted performance: %v", sr, tc.name, err)
			}

			q, err := enginedb.QUncompress(p.QuickCues, 4096)
			if err != nil {
				t.Fatal(err)
			}
			r := bytes.NewReader(q[8:])
			var hotPos [8]float64
			for i := 0; i < 8; i++ {
				n, _ := r.ReadByte()
				_, _ = r.Seek(int64(n), 1)
				_ = binary.Read(r, binary.BigEndian, &hotPos[i])
				_, _ = r.Seek(4, 1)
			}
			var mainCue, defaultCue float64
			_ = binary.Read(r, binary.BigEndian, &mainCue)
			_, _ = r.ReadByte()
			_ = binary.Read(r, binary.BigEndian, &defaultCue)

			wantEarlyHot := max(0.0, 20.0*float64(sr)/1000-float64(tc.offset))
			wantBeatHot := 550.0*float64(sr)/1000 - float64(tc.offset)
			wantMain := max(0.0, 10.0*float64(sr)/1000-float64(tc.offset))
			if math.Abs(hotPos[0]-wantEarlyHot) > 1e-6 || math.Abs(hotPos[1]-wantBeatHot) > 1e-6 || hotPos[3] != -1 {
				t.Fatalf("sr=%d %s hot cues: %v", sr, tc.name, hotPos)
			}
			if math.Abs(mainCue-wantMain) > 1e-6 || defaultCue != mainCue {
				t.Fatalf("sr=%d %s main cue: %v / %v, want %v", sr, tc.name, mainCue, defaultCue, wantMain)
			}
		}
	}
}
