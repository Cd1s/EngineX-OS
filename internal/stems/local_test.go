package stems

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
	"testing"
)

func TestLocalRenderGreetsAndExitsOnEOF(t *testing.T) {
	var out bytes.Buffer
	if err := LocalRender(strings.NewReader(""), &out); err != nil || out.String() != "session=FFFFFFFFFFFFFFFF\n\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}

// engineSession is what Engine writes for the planar channels x: the init
// header, blocks of at most block frames, done.
func engineSession(rate, block int, x [][]float32) [][]byte {
	msgs := [][]byte{[]byte(fmt.Sprintf("numChannels=%d\nsampleRate=%d\nmaxBlockSize=%d\n\n", len(x), rate, block))}
	for at := 0; at < len(x[0]); at += block {
		n := min(block, len(x[0])-at)
		m := []byte(fmt.Sprintf("action=block\nnumSamples=%d\n\n", n))
		for _, c := range x {
			m = append(m, f32s(c[at:at+n])...)
		}
		msgs = append(msgs, m)
	}
	return append(msgs, []byte("action=done\n\n"))
}

// lockstep hands LocalRender one Engine message at a time and fails when the
// next one is asked for before the last was answered: Engine sends a block
// and waits for its reply.
type lockstep struct {
	t    *testing.T
	msgs [][]byte
	at   int // bytes of msgs[0] already read
	out  bytes.Buffer
	seen int // out.Len() when msgs[0] was started
}

func (l *lockstep) Read(p []byte) (int, error) {
	if len(l.msgs) == 0 {
		return 0, io.EOF
	}
	if l.at == 0 {
		if l.out.Len() == l.seen {
			l.t.Fatal("LocalRender asked for more input before answering")
		}
		l.seen = l.out.Len()
	}
	n := copy(p, l.msgs[0][l.at:])
	if l.at += n; l.at == len(l.msgs[0]) {
		l.msgs, l.at = l.msgs[1:], 0
	}
	return n, nil
}

// localStems renders x as Engine would and returns the 4*channels planes it
// gets back, with the number of replies.
func localStems(t *testing.T, rate, block int, x [][]float32) ([][]float32, int) {
	t.Helper()
	l := &lockstep{t: t, msgs: engineSession(rate, block, x), seen: -1}
	if err := LocalRender(l, &l.out); err != nil {
		t.Fatal(err)
	}
	head := fmt.Sprintf("session=FFFFFFFFFFFFFFFF\n\npreparing=ready\n\npreparing=ready\nnumReturnChannels=%d\n\n", 4*len(x))
	if !strings.HasPrefix(l.out.String(), head) {
		t.Fatalf("greeting %q", l.out.String()[:min(l.out.Len(), 90)])
	}
	got, replies, last := engineReplies(t, l.out.Bytes()[len(head):], 4*len(x), block)
	if last != 0 || len(got[0]) != len(x[0]) {
		t.Fatalf("last reply %d, %d of %d frames", last, len(got[0]), len(x[0]))
	}
	for c := range x {
		for i, want := range x[c] {
			sum := got[c][i] + got[len(x)+c][i] + got[2*len(x)+c][i] + got[3*len(x)+c][i]
			if d := math.Abs(float64(sum - want)); d > 1e-6 {
				t.Fatalf("stems sum off by %g at frame %d", d, i)
			}
		}
	}
	return got, replies
}

// Every block is answered before the next; the stems sum to the input; a
// 100 Hz tone lands in bass, not drums.
func TestLocalRenderSplitsAndSumsToInput(t *testing.T) {
	const ch, block, blocks, rate = 2, 1024, 64, 44100
	x := make([][]float32, ch)
	for c := range x {
		x[c] = make([]float32, blocks*block)
		for i := range x[c] {
			x[c][i] = float32(0.5 * math.Sin(2*math.Pi*100*float64(i)/rate))
		}
	}
	got, replies := localStems(t, rate, block, x)
	if replies < blocks+1 {
		t.Fatalf("%d replies for %d blocks", replies, blocks)
	}
	var bass, drums float64
	for c := 0; c < ch; c++ {
		for i := 16 * block; i < 48*block; i++ { // away from the tone's start and end
			bass += math.Abs(float64(got[ch+c][i]))
			drums += math.Abs(float64(got[2*ch+c][i]))
		}
	}
	if drums > bass/100 {
		t.Fatalf("100 Hz tone: bass %g, drums %g", bass, drums)
	}
}

// testMix builds known stems (vocals, bass, drums, melody; two channels each)
// and their sum: a centred voice with vibrato, a centred bass note, a centred
// kick with noise hits between, and two chords panned hard left and right.
func testMix(rate, n int) (stems [4][2][]float32, mix [][]float32) {
	rnd := rand.New(rand.NewSource(1))
	mix = [][]float32{make([]float32, n), make([]float32, n)}
	for s := range stems {
		stems[s] = [2][]float32{make([]float32, n), make([]float32, n)}
	}
	phase := 0.0
	for i := 0; i < n; i++ {
		t := float64(i) / float64(rate)
		phase += 2 * math.Pi * 330 * (1 + 0.03*math.Sin(2*math.Pi*5.5*t)) / float64(rate)
		voice := 0.0
		for h := 1.0; h <= 8; h++ {
			voice += 0.12 / h * math.Sin(h*phase)
		}
		bass := 0.25*math.Sin(2*math.Pi*55*t) + 0.12*math.Sin(2*math.Pi*110*t)
		kick, hit := math.Mod(t, 0.5), math.Mod(t+0.25, 0.5)
		drums := 0.5*math.Sin(2*math.Pi*60*kick)*math.Exp(-kick/0.04) + 0.3*(2*rnd.Float64()-1)*math.Exp(-hit/0.02)
		left := 0.1 * (math.Sin(2*math.Pi*440*t) + math.Sin(2*math.Pi*554*t))
		right := 0.1 * (math.Sin(2*math.Pi*660*t) + math.Sin(2*math.Pi*880*t))
		for c, v := range [2][4]float64{{voice, bass, drums, left}, {voice, bass, drums, right}} {
			for s := range stems {
				stems[s][c][i] = float32(v[s])
				mix[c][i] += float32(v[s])
			}
		}
	}
	return stems, mix
}

// The band split this replaced put most of every source in the wrong stem:
// each stem of a mix with known stems must come back much closer to its own
// source (signal-to-distortion ratio, in dB) than the split brought it.
func TestLocalRenderSeparatesKnownStems(t *testing.T) {
	const rate = 44100
	want, mix := testMix(rate, 6*rate)
	got, _ := localStems(t, rate, 1024, mix)
	floor := [4]float64{10, 9, 3, 10} // the band split: -0.5, 4.6, 0.2, -1.7
	for s, name := range []string{"vocals", "bass", "drums", "melody"} {
		var signal, dist float64
		for c := 0; c < 2; c++ {
			for i := rate; i < 5*rate; i++ { // away from the edges of the audio
				w := float64(want[s][c][i])
				d := w - float64(got[2*s+c][i])
				signal, dist = signal+w*w, dist+d*d
			}
		}
		if sdr := 10 * math.Log10(signal/dist); sdr < floor[s] {
			t.Errorf("%s: SDR %.1f dB, want at least %g", name, sdr, floor[s])
		}
	}
}

// One or two channels, any rate, blocks of any size, audio shorter than the
// separator's lookahead: localStems checks the replies and the sum.
func TestLocalRenderAnyShape(t *testing.T) {
	_, mix := testMix(8000, 30000)
	for _, tc := range []struct{ ch, rate, block, frames int }{{1, 8000, 100, 5000}, {2, 96000, 4096, 30000}, {2, 44100, 1024, 10}, {1, 192000, 65536, 1}} {
		x := [][]float32{mix[0][:tc.frames], mix[1][:tc.frames]}[:tc.ch]
		localStems(t, tc.rate, tc.block, x)
	}
}

// PCM comes from another process: a NaN or infinite sample is refused, not
// spread through the transform into every stem.
func TestLocalRenderRejectsNonFinitePCM(t *testing.T) {
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(-1))} {
		in := bytes.Join(engineSession(44100, 4, [][]float32{{0, bad, 0, 0}}), nil)
		if err := LocalRender(bytes.NewReader(in), io.Discard); err == nil {
			t.Fatalf("accepted a %v sample", bad)
		}
	}
}

// BenchmarkLocalRender renders ten seconds of stereo 44.1 kHz audio; the
// x-realtime metric is seconds of audio per second of work.
func BenchmarkLocalRender(b *testing.B) {
	const rate, seconds = 44100, 10
	_, mix := testMix(rate, seconds*rate)
	in := bytes.Join(engineSession(rate, 1024, mix), nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := LocalRender(bytes.NewReader(in), io.Discard); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(seconds*b.N)/b.Elapsed().Seconds(), "x-realtime")
}

// The real FFT against the definition of the transform, and back again, with
// an odd and an even number of butterfly sizes.
func TestRFFTMatchesDFT(t *testing.T) {
	rnd := rand.New(rand.NewSource(2))
	for _, n := range []int{64, 128} {
		f := newRFFT(n)
		x, back := make([]float32, n), make([]float32, n)
		re, im := make([]float32, n/2+1), make([]float32, n/2+1)
		for i := range x {
			x[i] = rnd.Float32() - 0.5
		}
		f.forward(x, re, im)
		for k := range re {
			var wr, wi float64
			for i, v := range x {
				sin, cos := math.Sincos(2 * math.Pi * float64(k*i) / float64(n))
				wr, wi = wr+float64(v)*cos, wi-float64(v)*sin
			}
			if math.Abs(wr-float64(re[k])) > 1e-4 || math.Abs(wi-float64(im[k])) > 1e-4 {
				t.Fatalf("n %d bin %d: got %g%+gi, want %g%+gi", n, k, re[k], im[k], wr, wi)
			}
		}
		f.inverse(re, im, back)
		for i := range x {
			if got := back[i] / float32(n/2); math.Abs(float64(got-x[i])) > 1e-5 {
				t.Fatalf("n %d sample %d: got %g back, want %g", n, i, got, x[i])
			}
		}
	}
}
