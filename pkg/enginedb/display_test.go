package enginedb

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func displayFixture(t *testing.T) Performance {
	t.Helper()
	p, err := EncodePerformance(rekordbox.Track{SampleRate: 44100, Duration: 4}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 0, BPM: 120, Number: 1}, {TimeMS: 500, BPM: 120, Number: 2}, {TimeMS: 1000, BPM: 120, Number: 3}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{20, 40, 60, 80, 100, 120}}},
		Cues:      []rekordbox.Cue{{HotCue: 1, TimeMS: 500, Name: "drop"}, {TimeMS: 1000}, {HotCue: 2, TimeMS: 1500, EndMS: 2500, Loop: true, Name: "loop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDisplayPerformance(t *testing.T) {
	p := displayFixture(t)
	d, err := ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops)
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleRate != 44100 || d.DurationMS != 4000 || len(d.Overview.Bands) != 3072 || len(d.Beats) != 3 || d.Beats[1].MS != 500 || d.Beats[1].Number != 1 {
		t.Fatalf("track/wave/grid: %+v", d)
	}
	if len(d.Cues) != 3 || d.Cues[0].MS != 500 || d.Cues[0].N != "drop" || d.Cues[2].I != 0 || d.Cues[2].MS != 1000 || len(d.Loops) != 1 || d.Loops[0].StartMS != 1500 || d.Loops[0].EndMS != 2500 {
		t.Fatalf("cues/loops: %+v %+v", d.Cues, d.Loops)
	}
}

func TestDisplayAdjustedSparseGrid(t *testing.T) {
	p := displayFixture(t)
	var b bytes.Buffer
	put(&b, big, float64(44100))
	put(&b, big, float64(176400))
	b.WriteByte(1)
	for _, shift := range []float64{0, 11025} {
		put(&b, big, uint64(2))
		for _, m := range []struct {
			p    float64
			n    int64
			span uint32
		}{{-22050 + shift, -1, 8}, {154350 + shift, 7, 0}} {
			put(&b, little, m.p)
			put(&b, little, m.n)
			put(&b, little, m.span)
			put(&b, little, uint32(0))
		}
	}
	q, _ := QCompress(b.Bytes())
	d, err := ParseDisplayPerformance(p.TrackData, p.Overview, q, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Beats) != 8 || d.Beats[0].MS != 250 || d.Beats[0].Number != 0 || d.Beats[7].MS != 3750 {
		t.Fatalf("adjusted sparse grid was replaced: %+v", d.Beats)
	}
}

func TestDisplayStockRoundedBeatDuration(t *testing.T) {
	p := displayFixture(t)
	for _, tc := range []struct {
		name    string
		samples uint64
		grid    float64
		wantErr bool
	}{
		{"stock m4a", 17321984, 17287200, false},
		{"stock mp3", 11487744, 11466000, false},
		{"short track", 22050, 0, false},
		{"exact", 176400, 176400, false},
		// Rows Engine had loaded on the test decks (2026-11-19): the track's count is the
		// decoder's, the grid's the import's. The source's shadow refused them all.
		{"m4a Engine has loaded: priming off the track's count", 21433344, 21435392, false},
		{"mp3 Engine has loaded: delay and padding off the track's count", 14903808, 14905728, false},
		{"more than a decoder trims", 176400, 176400 + 16385, true},
		{"unrelated length", 176400, 176300, true},
		{"previous second", 176400, 132300, true},
		{"future second", 176401, 220500, true},
		{"nan", 176400, math.NaN(), true},
		{"infinite", 176400, math.Inf(1), true},
		{"negative", 176400, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track, err := QUncompress(p.TrackData, 4096)
			if err != nil {
				t.Fatal(err)
			}
			beats, err := QUncompress(p.Beats, 8<<20)
			if err != nil {
				t.Fatal(err)
			}
			big.PutUint64(track[8:], tc.samples)
			big.PutUint64(beats[8:], math.Float64bits(tc.grid))
			trackBlob, err := QCompress(track)
			if err != nil {
				t.Fatal(err)
			}
			beatBlob, err := QCompress(beats)
			if err != nil {
				t.Fatal(err)
			}
			d, err := ParseDisplayPerformance(trackBlob, p.Overview, beatBlob, nil, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid beat duration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.DurationMS != float64(tc.samples)*1000/44100 || len(d.Beats) == 0 || d.Beats[0].MS != 0 ||
				(tc.samples >= 44100 && (len(d.Beats) < 2 || d.Beats[1].MS != 500 || d.Beats[1].Number != 1)) {
				t.Fatalf("source duration or grid changed: %+v", d)
			}
		})
	}
}

func TestDisplayStockPrerollCue(t *testing.T) {
	p := displayFixture(t)
	for _, tc := range []struct {
		name    string
		cue     float64
		wantErr bool
	}{
		{"stock preroll", -23423.4, false},
		{"unset", -1, false},
		{"nan", math.NaN(), true},
		{"infinite", math.Inf(-1), true},
		{"past end", 176401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cues, err := QUncompress(p.QuickCues, 64<<10)
			if err != nil {
				t.Fatal(err)
			}
			big.PutUint64(cues[len(cues)-17:], math.Float64bits(tc.cue))
			blob, err := QCompress(cues)
			if err != nil {
				t.Fatal(err)
			}
			d, err := ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, blob, p.Loops)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid main CUE accepted")
				}
				return
			}
			if err != nil || len(d.Beats) != 3 || len(d.Overview.Bands) != 3072 || len(d.Cues) != 2 || d.Cues[0].N != "drop" || len(d.Loops) != 1 {
				t.Fatalf("pre-roll discarded source data: %+v, %v", d, err)
			}
		})
	}
}

func TestDisplayMalformedPerformance(t *testing.T) {
	p := displayFixture(t)
	unpack := func(b []byte) []byte {
		r, e := QUncompress(b, 8<<20)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	pack := func(b []byte) []byte {
		q, e := QCompress(b)
		if e != nil {
			t.Fatal(e)
		}
		return q
	}
	badTrack := unpack(p.TrackData)
	binary.BigEndian.PutUint64(badTrack, math.Float64bits(math.NaN()))
	badWave := unpack(p.Overview)
	binary.BigEndian.PutUint64(badWave[8:], math.MaxUint64)
	badGrid := unpack(p.Beats)
	binary.BigEndian.PutUint64(badGrid[17:], math.MaxUint64)
	badCue := unpack(p.QuickCues)
	badCue[8] = 255
	badLoops := bytes.Clone(p.Loops)
	badLoops[0] = 255
	for _, tc := range []struct {
		name                            string
		track, wave, beats, cues, loops []byte
	}{
		{"track", pack(badTrack), p.Overview, p.Beats, p.QuickCues, p.Loops},
		{"wave", p.TrackData, pack(badWave), p.Beats, p.QuickCues, p.Loops},
		{"grid", p.TrackData, p.Overview, pack(badGrid), p.QuickCues, p.Loops},
		{"cues", p.TrackData, p.Overview, p.Beats, pack(badCue), p.Loops},
		{"loops", p.TrackData, p.Overview, p.Beats, p.QuickCues, badLoops},
		{"trailing", p.TrackData, append(bytes.Clone(p.Overview), 1), p.Beats, p.QuickCues, p.Loops},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := ParseDisplayPerformance(tc.track, tc.wave, tc.beats, tc.cues, tc.loops); e == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
}

// BH-GC1-4: stock Engine 5.3.0 stores Hot Cues and Saved Loops before sample
// zero (7 of the 13 DemoDevice tracks: -1024, -1103.21, -927.17, -590.19). The
// parser rejected the whole track, so the shadow display showed nothing for it.
func TestDisplayStockPrerollHotCueAndLoop(t *testing.T) {
	p := displayFixture(t)
	cues, err := QUncompress(p.QuickCues, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	// Hot Cue 1 ("drop") moves into the pre-roll.
	off := 8 + 1 + int(cues[8])
	big.PutUint64(cues[off:], math.Float64bits(-1103.21))
	blob, err := QCompress(cues)
	if err != nil {
		t.Fatal(err)
	}
	loops := bytes.Clone(p.Loops)
	loff := 8 + 1 + int(loops[8]) + 22 // slot 2 holds the fixture's loop
	loff += 1 + int(loops[loff])
	little.PutUint64(loops[loff:], math.Float64bits(-590.19))
	d, err := ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, blob, loops)
	if err != nil {
		t.Fatalf("stock pre-roll rejected: %v", err)
	}
	if len(d.Cues) != 3 || d.Cues[0].N != "drop" || d.Cues[0].MS != 0 || len(d.Loops) != 1 || d.Loops[0].StartMS != 0 || d.Loops[0].EndMS != 2500 {
		t.Fatalf("pre-roll cue/loop: %+v %+v", d.Cues, d.Loops)
	}
	// A loop that ends before the track starts is not shown; an inverted one is still invalid.
	little.PutUint64(loops[loff+8:], math.Float64bits(-100))
	if d, err = ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, blob, loops); err != nil || len(d.Loops) != 0 {
		t.Fatalf("pre-roll-only loop: %+v %v", d.Loops, err)
	}
	little.PutUint64(loops[loff+8:], math.Float64bits(-1000))
	if _, err = ParseDisplayPerformance(p.TrackData, p.Overview, p.Beats, blob, loops); err == nil {
		t.Fatal("inverted loop accepted")
	}
}

// Item 4 regression (content 76 / Engine track 3222): quickCues slots carrying negative
// positions (e.g. -420.0 across Cue 1..Cue 8, or pre-roll values != -1) must be treated
// as unset by ParseDeckCuesAndLoops so write-back never clamps them to sample 0 + offset
// and emits phantom hot cues at 0.023 s, while ParseDisplayPerformance (#BH-GC1-4) keeps
// showing pre-roll cues on the web shadow display.
func TestParseDeckCuesAndLoopsIgnoresNegativeSlots(t *testing.T) {
	// 1. Exact content 76 / Engine track 3222 shape: 48 kHz MP3, all 8 slots Cue 1..Cue 8
	// at -420.0 with Engine DJ default pad colors, main = -420.0, flag = 1, def = -420.0.
	td := makeTrackDataBlob(t, 48000, 20882304, 0, [3]float64{0.68, 0.40, 0.18}, true)
	colors := [8][4]byte{
		{0xff, 0xf4, 0xd3, 0x38},
		{0xff, 0xef, 0x81, 0x30},
		{0xff, 0xaa, 0x55, 0xc4},
		{0xff, 0xce, 0x32, 0x39},
		{0xff, 0x86, 0xc6, 0x4b},
		{0xff, 0x20, 0xc6, 0x70},
		{0xff, 0x00, 0xa8, 0xa9},
		{0xff, 0x15, 0x71, 0xe2},
	}
	var buf bytes.Buffer
	put(&buf, big, uint64(8))
	for i := range 8 {
		name := "Cue " + string(rune('1'+i))
		buf.WriteByte(byte(len(name)))
		buf.WriteString(name)
		put(&buf, big, float64(-420.0))
		buf.Write(colors[i][:])
	}
	put(&buf, big, float64(-420.0))
	buf.WriteByte(1)
	put(&buf, big, float64(-420.0))
	qcAllNeg, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	deck, err := ParseDeckCuesAndLoops(td, qcAllNeg, nil, 0)
	if err != nil {
		t.Fatalf("ParseDeckCuesAndLoops(-420.0): %v", err)
	}
	for slot, hc := range deck.HotCues {
		if hc != nil {
			t.Fatalf("slot %d with position -420.0 must be unset (nil), got %+v", slot, hc)
		}
	}
	if deck.MainCue != -1 {
		t.Fatalf("negative main cue = %v, want -1", deck.MainCue)
	}

	// 2. Mixed slots: -420.0, -1.0, and -1103.21 are ignored; 0.0 and positive positions stay valid.
	buf.Reset()
	put(&buf, big, uint64(8))
	mixed := [8]float64{-420.0, -1.0, 0.0, 48000.0, -1103.21, -0.001, 96000.0, -1.0}
	for i, pos := range mixed {
		name := "Cue " + string(rune('1'+i))
		buf.WriteByte(byte(len(name)))
		buf.WriteString(name)
		put(&buf, big, pos)
		buf.Write(colors[i][:])
	}
	put(&buf, big, float64(12000.0))
	buf.WriteByte(1)
	put(&buf, big, float64(12000.0))
	qcMixed, err := QCompress(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	deck, err = ParseDeckCuesAndLoops(td, qcMixed, nil, 0)
	if err != nil {
		t.Fatalf("ParseDeckCuesAndLoops(mixed): %v", err)
	}
	for _, slot := range []int{0, 1, 4, 5, 7} {
		if deck.HotCues[slot] != nil {
			t.Fatalf("negative slot %d (%v) must be nil, got %+v", slot, mixed[slot], deck.HotCues[slot])
		}
	}
	if deck.HotCues[2] == nil || deck.HotCues[2].Samples != 0 {
		t.Fatalf("slot 2 at 0.0 must stay valid: %+v", deck.HotCues[2])
	}
	if deck.HotCues[3] == nil || deck.HotCues[3].Samples != 48000.0 {
		t.Fatalf("slot 3 at 48000.0 must stay valid: %+v", deck.HotCues[3])
	}
	if deck.HotCues[6] == nil || deck.HotCues[6].Samples != 96000.0 {
		t.Fatalf("slot 6 at 96000.0 must stay valid: %+v", deck.HotCues[6])
	}
	if deck.MainCue != 12000.0 {
		t.Fatalf("main cue = %v, want 12000.0", deck.MainCue)
	}
}
