package analyzer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

func TestFoldBPMRejectsInvalidValues(t *testing.T) {
	for _, bpm := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -120} {
		if got := foldBPM(bpm, 88, 175); got != 0 {
			t.Fatalf("foldBPM(%v) = %v, want 0", bpm, got)
		}
	}
	for _, bounds := range [][2]float64{{0, 175}, {-1, 175}, {175, 88}, {88, 0}, {math.NaN(), 175}, {88, math.NaN()}, {math.Inf(1), 175}, {88, math.Inf(1)}} {
		if got := foldBPM(120, bounds[0], bounds[1]); got != 0 {
			t.Fatalf("invalid bounds %v produced %v", bounds, got)
		}
	}
	if got := foldBPM(1, math.MaxFloat64, math.MaxFloat64); got != 0 {
		t.Fatalf("overflowing octave fold = %v", got)
	}
}

func TestFoldBPMFiniteOctaves(t *testing.T) {
	for _, tc := range []struct{ bpm, want float64 }{
		{30, 120}, {120, 120}, {240, 120}, {352, 88},
		{math.SmallestNonzeroFloat64, 128}, {math.MaxFloat64, 128},
	} {
		if got := foldBPM(tc.bpm, 88, 175); math.Abs(got-tc.want) > 1e-10 {
			t.Fatalf("foldBPM(%v) = %v, want %v", tc.bpm, got, tc.want)
		}
	}
}

func TestOnsetAutocorrelationLastSampleBoundary(t *testing.T) {
	ac, err := onsetAutocorrelation([]float64{1, 2, 3, 4, 5}, 1, 2, tickerFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 10, 26.0 / 3, 7, 5}
	if len(ac) != len(want) {
		t.Fatalf("correlation contains lags without overlapping samples: %v", ac)
	}
	for lag, v := range ac {
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v-want[lag]) > 1e-12 {
			t.Fatalf("correlation at lag %d = %v, want %v", lag, v, want[lag])
		}
	}
	if _, err := onsetAutocorrelation([]float64{1, 2, 3, 4}, 1, 2, tickerFunc(nil)); !errors.Is(err, ErrTooShort) {
		t.Fatalf("too-short correlation: %v", err)
	}
	canceled := errors.New("stop autocorrelation")
	if _, err := onsetAutocorrelation([]float64{1, 2, 3, 4, 5}, 1, 2, func(int) error { return canceled }); !errors.Is(err, canceled) {
		t.Fatalf("autocorrelation cancellation: %v", err)
	}
}

func TestDetectTempoShortestAutocorrelationEnvelope(t *testing.T) {
	const sr = 8000
	fps := float64(sr) / onsetHop
	maxLag := int(math.Ceil(60 * fps / 88))
	for _, frames := range []int{2 * maxLag, 2*maxLag + 1, 2*maxLag + 2} {
		t.Run(fmt.Sprint(frames), func(t *testing.T) {
			x := make([]float32, (frames-1)*onsetHop+1)
			for i := range x {
				seconds := float64(i) / sr
				phase := math.Mod(seconds-.2, .5)
				if seconds >= .2 && phase < .05 {
					x[i] = float32(math.Exp(-phase*100) * math.Sin(2*math.Pi*80*phase))
				}
			}
			bpm, first, err := detectTempo(x, sr, 88, 175, tickerFunc(nil))
			if frames == 2*maxLag {
				if !errors.Is(err, ErrTooShort) {
					t.Fatalf("short envelope: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if math.IsNaN(bpm) || math.IsInf(bpm, 0) || bpm < 88 || bpm > 175 || math.IsNaN(first) || math.IsInf(first, 0) || first < 0 || first >= 60/bpm {
				t.Fatalf("non-finite or out-of-range tempo: bpm=%v first=%v", bpm, first)
			}
		})
	}
}

func TestCombSearchRejectsInvalidRangeBeforeIteration(t *testing.T) {
	for _, center := range []float64{0, -1, math.NaN(), math.Inf(1), math.SmallestNonzeroFloat64, math.MaxFloat64} {
		ticks := 0
		_, _, ok, err := combSearch([]float64{1, 1}, 100, center, 0, 0.05, 0.5, func(int) error { ticks++; return context.Canceled })
		if err == nil || ok || ticks != 0 {
			t.Fatalf("invalid center %v entered search: ok=%v err=%v ticks=%d", center, ok, err, ticks)
		}
	}
}

func TestAnalyzeFirstBeatAlignsWithKickTransients(t *testing.T) {
	for _, sr := range []int{44100, 48000} {
		x := make([]float32, sr*8)
		for i := range x {
			sec := float64(i) / float64(sr)
			if sec < 0.25 {
				continue
			}
			phase := math.Mod(sec-0.25, 0.5)
			if phase < 0.06 {
				x[i] = float32(0.8 * math.Exp(-phase*90) * math.Sin(2*math.Pi*70*phase))
			}
		}
		res, err := Analyze(&Audio{SampleRate: sr, Channels: 1, BitsPerSample: 16, Samples: x}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if delta := math.Abs(res.FirstBeat - 0.25); delta > 0.015 {
			t.Fatalf("sr=%d: FirstBeat=%0.4fs (delta=%0.1fms from 0.2500s kick, want <= 15ms)", sr, res.FirstBeat, delta*1000)
		}
	}
}

func TestFoldBPMStaysInsideANarrowWindow(t *testing.T) {
	for _, c := range []struct{ bpm, lo, hi, want float64 }{
		{87.9, 88, 175, 88},    // no octave of 87.9 lies in [88, 175]; 88 is nearer
		{87.593, 88, 175, 175}, // 175.186 is nearer to 175 than 87.593 to 88
		{90, 100, 150, 100},    // 90 is nearer to 100 than 180 is to 150
		{140, 100, 150, 140},
		{70, 100, 150, 140},
		{148, 100, 150, 148},
		{280, 100, 150, 140},
	} {
		if got := foldBPM(c.bpm, c.lo, c.hi); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("foldBPM(%v, %v, %v) = %v, want %v", c.bpm, c.lo, c.hi, got, c.want)
		}
	}
}
