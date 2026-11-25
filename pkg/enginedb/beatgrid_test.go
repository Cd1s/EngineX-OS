package enginedb

import (
	"math"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestBeatPositionsHoldConstantBPM(t *testing.T) {
	const sr = 44100.0
	const n = 1000
	exact := func(bpm, anchorMS float64, i int) uint32 { return uint32(math.Round(anchorMS + float64(i)*60000/bpm)) }
	beats := make([]rekordbox.Beat, n)
	for i := range beats {
		beats[i] = rekordbox.Beat{BPM: 130, TimeMS: exact(130, 100, i)}
	}
	p := beatPositionsWithOffset(beats, sr, 0)
	want := 60 / 130.0 * sr
	for i := 0; i < n-1; i++ {
		if math.Abs((p[i+1]-p[i])-want) > 1e-6 {
			t.Fatalf("beat %d spacing %v, want %v", i, p[i+1]-p[i], want)
		}
	}
	for i := range beats {
		beats[i] = rekordbox.Beat{BPM: 130, TimeMS: exact(130.004, 100, i)}
	}
	p = beatPositionsWithOffset(beats, sr, 0)
	for i := range beats {
		if drift := math.Abs(p[i]/sr*1000 - float64(beats[i].TimeMS)); drift > 1.5 {
			t.Fatalf("beat %d drifted %v ms", i, drift)
		}
	}
}
