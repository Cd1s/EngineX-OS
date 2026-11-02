package enginedb

import (
	"math"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

func TestBeatPositionsHoldConstantBPM(t *testing.T) {
	const sr = 44100.0
	const n = 1000
	exact := func(bpm, anchorMS float64, i int) uint32 { return uint32(math.Round(anchorMS + float64(i)*60000/bpm)) }
	spacing := func(p []float64, i int) float64 { return p[i+1] - p[i] }

	// Exactly 130.00 BPM, with times rounded to whole milliseconds.
	beats := make([]rekordbox.Beat, n)
	for i := range beats {
		beats[i] = rekordbox.Beat{BPM: 130, TimeMS: exact(130, 100, i)}
	}
	p := beatPositions(beats, sr)
	want := 60 / 130.0 * sr
	for i := 0; i < n-1; i++ {
		if math.Abs(spacing(p, i)-want) > 1e-6 {
			t.Fatalf("beat %d spacing %v, want %v", i, spacing(p, i), want)
		}
	}

	// Really 130.004 BPM, exported as 130.00: the grid must stay on the audio.
	for i := range beats {
		beats[i] = rekordbox.Beat{BPM: 130, TimeMS: exact(130.004, 100, i)}
	}
	p = beatPositions(beats, sr)
	for i := range beats {
		if drift := math.Abs(p[i]/sr*1000 - float64(beats[i].TimeMS)); drift > 1.5 {
			t.Fatalf("beat %d drifted %v ms", i, drift)
		}
	}
	for i := 0; i < n-2; i++ {
		if math.Abs(spacing(p, i)-spacing(p, i+1)) > 1e-6 {
			t.Fatalf("beat %d spacing varies", i)
		}
	}

	// A tempo change re-anchors on the exported time; unknown BPM keeps it.
	beats = []rekordbox.Beat{{BPM: 120, TimeMS: 0}, {BPM: 120, TimeMS: 500}, {BPM: 140, TimeMS: 1000}, {BPM: 140, TimeMS: 1429}, {BPM: 0, TimeMS: 1900}, {BPM: 0, TimeMS: 2400}}
	p = beatPositions(beats, sr)
	if p[2] != 44100 || p[4] != 1.9*sr || p[5] != 2.4*sr {
		t.Fatalf("anchors %v", p)
	}
	if math.Abs(spacing(p, 2)-60/140.0*sr) > 1e-6 {
		t.Fatalf("second segment spacing %v", spacing(p, 2))
	}
}
