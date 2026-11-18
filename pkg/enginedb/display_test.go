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

func gridBlob(t *testing.T, flag byte, grids ...[][2]float64) []byte {
	t.Helper()
	var b bytes.Buffer
	put(&b, big, float64(44100))
	put(&b, big, float64(176400))
	b.WriteByte(flag)
	for i := 0; i < 2; i++ {
		var g [][2]float64
		if i < len(grids) {
			g = grids[i]
		}
		put(&b, big, uint64(len(g)))
		for _, m := range g {
			put(&b, little, m[0])
			put(&b, little, int64(m[1]))
			put(&b, little, uint32(0))
			put(&b, little, uint32(0))
		}
	}
	q, err := QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The countdown to a hot cue maps samples to beats on the saved markers: a tempo change is a marker, so the map is exact across it.
func TestParseDisplayGridKeepsEveryMarkerOfATempoChange(t *testing.T) {
	p := displayFixture(t)
	two := [][2]float64{{0, 0}, {88200, 4}, {163800, 8}} // 120 BPM for 4 beats, then 140 BPM
	rate, m, err := ParseDisplayGrid(p.TrackData, gridBlob(t, 1, [][2]float64{{0, 0}, {176400, 8}}, two))
	if err != nil || rate != 44100 || len(m) != 3 || m[1] != (GridMarker{88200, 4}) || m[2] != (GridMarker{163800, 8}) {
		t.Fatalf("the adjusted grid's markers: %v %v %+v", rate, err, m)
	}
	// Only one adjusted marker: the original grid is used, as the display does.
	_, m, err = ParseDisplayGrid(p.TrackData, gridBlob(t, 1, [][2]float64{{0, 0}, {176400, 8}}, [][2]float64{{0, 0}}))
	if err != nil || len(m) != 2 || m[1].Beat != 8 {
		t.Fatalf("original grid fallback: %v %+v", err, m)
	}
	// The grid flag off: no grid, so the pack keeps counting by tempo.
	_, m, err = ParseDisplayGrid(p.TrackData, gridBlob(t, 0, two))
	if err != nil || len(m) != 0 {
		t.Fatalf("no grid: %v %+v", err, m)
	}
	// What the display shows from the same blob is unchanged by the shared reader.
	d, err := ParseDisplayPerformance(p.TrackData, p.Overview, gridBlob(t, 1, two), nil, nil)
	if err != nil || len(d.Beats) != 9 || d.Beats[4].MS != 2000 || d.Beats[8].Number != 8 {
		t.Fatalf("display beats: %v %+v", err, d.Beats)
	}
	// Corrupt and unordered markers fail closed.
	if _, _, err = ParseDisplayGrid(p.TrackData, gridBlob(t, 1, [][2]float64{{0, 0}, {0, 4}})); err == nil {
		t.Fatal("unordered markers accepted")
	}
}
