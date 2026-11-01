package analyzer

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/sc5000-custom-os/internal/testaudio"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

func TestSyntheticWAVFLACAnalysis(t *testing.T) {
	song := testaudio.Song(16)
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, song); err != nil {
		t.Fatal(err)
	}
	for name, decode := range map[string]func() (*Audio, error){"wav": func() (*Audio, error) { return DecodeWAV(bytes.NewReader(testaudio.WAV(song))) }, "flac": func() (*Audio, error) { return DecodeFLAC(bytes.NewReader(flac.Bytes())) }} {
		t.Run(name, func(t *testing.T) {
			a, err := decode()
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Samples) != len(song) || a.SampleRate != testaudio.Rate || a.BitsPerSample != 16 {
				t.Fatalf("bad decode %+v", a)
			}
			r, err := Analyze(a, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(r.BPM-120) > .6 {
				t.Fatalf("BPM = %v", r.BPM)
			}
			// Windowed onset timing may precede the exact transient by one FFT frame.
			if math.Abs(r.FirstBeat-.25) > .09 {
				t.Fatalf("first beat=%v", r.FirstBeat)
			}
			if r.Key.Name != "C" || r.Key.Camelot != "8B" {
				t.Fatalf("key=%+v confidence=%v", r.Key, r.KeyConfidence)
			}
			if len(r.Overview) != 1024 || len(r.Waveform) != 16*105 || len(r.Beatgrid) != 2 {
				t.Fatalf("wrong waveform/grid sizes")
			}
			var sums [3]int
			for _, p := range r.Waveform {
				sums[0] += int(p.LowValue)
				sums[1] += int(p.MidValue)
				sums[2] += int(p.HighValue)
			}
			for _, s := range sums {
				if s == 0 {
					t.Fatal("missing frequency band")
				}
			}
			spb := (r.Beatgrid[1].SampleOffset - r.Beatgrid[0].SampleOffset) / float64(r.Beatgrid[1].BeatNumber-r.Beatgrid[0].BeatNumber)
			if math.Abs(spb-float64(a.SampleRate)/2) > 100 {
				t.Fatal("grid samples inconsistent")
			}
		})
	}
}
func TestAnalyzerCancellationAndMalformedAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Analyze(&Audio{SampleRate: testaudio.Rate, Samples: make([]float32, 5*testaudio.Rate)}, Options{Ticker: resourceguard.NewThrottle(ctx, 0, 0)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	b := testaudio.WAV(testaudio.Song(4))
	if _, err = DecodeWAV(bytes.NewReader(b[:len(b)-1])); err == nil {
		t.Fatal("truncated WAV accepted")
	}
	if _, err = DecodeFLAC(bytes.NewReader([]byte("fLaC\x80\x00"))); err == nil {
		t.Fatal("truncated FLAC accepted")
	}
	if _, err = Analyze(&Audio{SampleRate: 44100, Samples: []float32{0}}, Options{}); !errors.Is(err, ErrTooShort) {
		t.Fatal(err)
	}
}

func TestRGBBandSeparation(t *testing.T) {
	for band, freq := range []float64{80, 800, 6000} {
		a := &Audio{SampleRate: testaudio.Rate, Samples: make([]float32, testaudio.Rate*4)}
		for i := range a.Samples {
			a.Samples[i] = float32(.5 * math.Sin(2*math.Pi*freq*float64(i)/float64(a.SampleRate)))
		}
		r := &Result{}
		if err := computeWaveforms(a, r, tickerFunc(nil)); err != nil {
			t.Fatal(err)
		}
		var sum [3]int
		for _, p := range r.Waveform[10:] {
			sum[0] += int(p.LowValue)
			sum[1] += int(p.MidValue)
			sum[2] += int(p.HighValue)
		}
		for b, s := range sum {
			if b != band && sum[band] < s*3 {
				t.Fatalf("%vHz bands=%v", freq, sum)
			}
		}
		for _, p := range r.Overview {
			if p.LowValue == 0 && p.MidValue == 0 && p.HighValue == 0 {
				t.Fatal("overview contains binning holes")
			}
		}
	}
}
func TestSilenceAndInvalidOptions(t *testing.T) {
	a := &Audio{SampleRate: testaudio.Rate, Samples: make([]float32, testaudio.Rate*4)}
	if _, err := Analyze(a, Options{}); err == nil {
		t.Fatal("invented BPM for silence")
	}
	if _, err := Analyze(a, Options{MinBPM: math.SmallestNonzeroFloat64, MaxBPM: 175}); err == nil {
		t.Fatal("unbounded BPM range")
	}
	if _, err := Analyze(nil, Options{}); err == nil {
		t.Fatal("nil audio accepted")
	}
}

func TestDecodeID3PrefixedFLACRegardlessOfExtension(t *testing.T) {
	samples := testaudio.Song(4)
	var encoded bytes.Buffer
	if err := testaudio.FLAC(&encoded, samples); err != nil {
		t.Fatal(err)
	}
	h := []byte{'I', 'D', '3', 4, 0, 16, 0, 0, 0, 0}
	footer := append([]byte("3DI"), h[3:]...)
	data := append(append(h, footer...), encoded.Bytes()...)
	file := filepath.Join(t.TempDir(), "misnamed.mp3")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	audio, err := DecodeFile(file)
	if err != nil || audio.Format != "flac" || len(audio.Samples) != len(samples) {
		t.Fatalf("tagged decode %v", err)
	}
}

func TestComputeWaveformsRejectsEmptyAudio(t *testing.T) {
	for _, a := range []*Audio{nil, {SampleRate: 44100}} {
		if err := computeWaveforms(a, &Result{}, tickerFunc(nil)); !errors.Is(err, ErrTooShort) {
			t.Fatalf("empty waveform input: %v", err)
		}
	}
}

func TestComputeWaveformsSingleSample(t *testing.T) {
	for _, sr := range []int{8000, 44100, 384000} {
		a := &Audio{SampleRate: sr, Samples: []float32{.5}}
		r := &Result{}
		if err := computeWaveforms(a, r, tickerFunc(nil)); err != nil {
			t.Fatal(err)
		}
		if len(r.Waveform) != 1 || len(r.Overview) != OverviewEntries || r.OverviewSamplesPerEntry != 1.0/OverviewEntries || math.IsNaN(r.Loudness) {
			t.Fatalf("invalid one-sample waveform at %d Hz", sr)
		}
		if r.Waveform[0] == (WavePoint{}) {
			t.Fatal("non-silent sample lost")
		}
		for i, got := range r.Overview {
			if got != r.Waveform[0] {
				t.Fatalf("overview[%d] at %d Hz = %+v, want %+v", i, sr, got, r.Waveform[0])
			}
		}
	}
}

func TestAnalyzerRejectsUnsupportedSampleRates(t *testing.T) {
	for _, sr := range []int{1, 7999, 384001} {
		a := &Audio{SampleRate: sr, Samples: make([]float32, sr*4)}
		if _, err := Analyze(a, Options{}); err == nil {
			t.Fatalf("analysis accepted %d Hz", sr)
		}
		if err := computeWaveforms(a, &Result{}, tickerFunc(nil)); err == nil {
			t.Fatalf("waveform accepted %d Hz", sr)
		}
	}
}
