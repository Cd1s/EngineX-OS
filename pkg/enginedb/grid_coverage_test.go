package enginedb

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

type coverageMarker struct {
	pos   float64
	index int64
	next  uint32
}

func decodeCoverageGrids(t *testing.T, blob []byte) (float64, [2][]coverageMarker) {
	t.Helper()
	raw, err := QUncompress(blob, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	samples := math.Float64frombits(binary.BigEndian.Uint64(raw[8:]))
	var grids [2][]coverageMarker
	off := 17
	for g := range grids {
		n := int(binary.BigEndian.Uint64(raw[off:]))
		off += 8
		for range n {
			grids[g] = append(grids[g], coverageMarker{
				math.Float64frombits(binary.LittleEndian.Uint64(raw[off:])),
				int64(binary.LittleEndian.Uint64(raw[off+8:])),
				binary.LittleEndian.Uint32(raw[off+16:]),
			})
			off += 24
		}
	}
	return samples, grids
}

// Shadow grid/CUE: a CUE set before rekordbox's first beat played with no grid under it,
// and the shadow copy's waveform went off the main deck's; cued inside the grid both lined
// up. Engine's own analysis grids the whole track (first marker before sample 0, beat -4):
// the grid we write for a rekordbox track (USB rb-bridge and Link fill share this encoder)
// must run from before sample 0, on rekordbox's own beats and bar phase.
func TestRekordboxGridRunsBackToSampleZero(t *testing.T) {
	const sr = 44100.0
	for _, tc := range []struct {
		name   string
		offset int
	}{{"plain", 0}, {"aac-priming", 2112}} {
		t.Run(tc.name, func(t *testing.T) {
			var beats []rekordbox.Beat
			for i := range 13 { // 120 BPM from 2 s to 8 s, the first beat is beat 3 of its bar
				beats = append(beats, rekordbox.Beat{Number: uint16((i+2)%4 + 1), BPM: 120, TimeMS: uint32(2000 + 500*i)})
			}
			tr := rekordbox.Track{ID: 1, SampleRate: sr, SampleCount: 10 * sr, Duration: 10, StartOffsetSamples: tc.offset}
			p, err := EncodePerformance(tr, &rekordbox.Analysis{Beats: beats})
			if err != nil {
				t.Fatal(err)
			}
			samples, grids := decodeCoverageGrids(t, p.Beats)
			const step = 0.5 * sr
			for g, m := range grids {
				if len(m) < len(beats) {
					t.Fatalf("grid %d: %d markers", g, len(m))
				}
				if m[0].pos > 0 || m[0].pos <= -step {
					t.Fatalf("grid %d starts at sample %v: the intro before rekordbox's first beat has no grid", g, m[0].pos)
				}
				if last := m[len(m)-1]; last.pos != 8*sr-float64(tc.offset) || last.index != 14 || last.next != 0 {
					t.Fatalf("grid %d ends at %+v: rekordbox's last beat moved", g, last)
				}
				found := false
				for i, x := range m {
					if i > 0 && (math.Abs(x.pos-m[i-1].pos-step) > 1e-6 || x.index != m[i-1].index+1 || m[i-1].next != 1) {
						t.Fatalf("grid %d marker %d: %+v after %+v", g, i, x, m[i-1])
					}
					// rekordbox's first beat stays where it was (start offset applied once) with its bar phase.
					if x.index == 2 {
						found = true
						if want := 2*sr - float64(tc.offset); math.Abs(x.pos-want) > 1e-6 {
							t.Fatalf("grid %d: rekordbox's first beat at %v, want %v", g, x.pos, want)
						}
					}
				}
				if !found {
					t.Fatalf("grid %d lost rekordbox's first beat index", g)
				}
			}
			// Still a grid the deck display and the Pro DJ Link export read.
			if _, err := parseDisplayBeats(p.Beats, sr, samples); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A grid of one beat has no step to extend by, and one from sample 0 needs none: both stay.
func TestRekordboxSingleBeatGridUnchanged(t *testing.T) {
	tr := rekordbox.Track{ID: 1, SampleRate: 44100, SampleCount: 441000, Duration: 10}
	p, err := EncodePerformance(tr, &rekordbox.Analysis{Beats: []rekordbox.Beat{{Number: 1, BPM: 120, TimeMS: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, grids := decodeCoverageGrids(t, p.Beats); len(grids[0]) != 1 || grids[0][0].pos != 88200 || grids[0][0].index != 0 {
		t.Fatalf("one-beat grid changed: %+v", grids[0])
	}
	p, err = EncodePerformance(tr, &rekordbox.Analysis{Beats: []rekordbox.Beat{{Number: 1, BPM: 120, TimeMS: 0}, {Number: 2, BPM: 120, TimeMS: 500}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, grids := decodeCoverageGrids(t, p.Beats); len(grids[1]) != 2 || grids[1][0].pos != 0 || grids[1][0].index != 0 {
		t.Fatalf("grid from sample 0 changed: %+v", grids[1])
	}
}
