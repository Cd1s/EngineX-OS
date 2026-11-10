package analyzer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
)

type shadowTestTicker func() error

func (f shadowTestTicker) Tick() error { return f() }

func shadowTestEnvelope(t *testing.T, data []byte, peaks [3]float64) *ShadowEnvelope {
	t.Helper()
	e, err := shadowWaveformReader(bytes.NewReader(data), peaks, nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func shadowTestTone(rate, samples int, hz, amplitude float64) []byte {
	values := make([]float64, samples)
	for i := range values {
		values[i] = amplitude * math.Sin(2*math.Pi*hz*float64(i)/float64(rate))
	}
	b := floatWAV(64, 1, values)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*8))
	return b
}

func shadowTestMP3(rate int) ([]byte, int) {
	var header [4]byte
	var frameSize, samples int
	switch rate {
	case 16000:
		header, frameSize, samples = [4]byte{0xff, 0xf3, 0x88, 0}, 288, 576
	case 32000:
		header, frameSize, samples = [4]byte{0xff, 0xfb, 0x98, 0}, 576, 1152
	case 44100:
		header, frameSize, samples = [4]byte{0xff, 0xfb, 0x90, 0}, 417, 1152
	case 48000:
		header, frameSize, samples = [4]byte{0xff, 0xfb, 0x94, 0}, 384, 1152
	default:
		panic("unsupported test rate")
	}
	b := make([]byte, 2*frameSize)
	copy(b, header[:])
	copy(b[frameSize:], header[:])
	return b, 2 * samples
}

func TestShadowWaveformRationalBinsAndShortTail(t *testing.T) {
	for _, rate := range []int{8000, 22050, 44100, 48000, 384000} {
		for _, n := range []int{1, rate / 105, rate/105 + 1, 4095, 4096, 4097, rate, rate + 1} {
			t.Run(fmt.Sprintf("%d/%d", rate, n), func(t *testing.T) {
				e := shadowTestEnvelope(t, shadowTestTone(rate, n, 100, .4), [3]float64{1, 1, 1})
				want := (int64(n)*105 + int64(rate) - 1) / int64(rate)
				if e.SampleRate != rate || e.SamplesPerBin != float64(rate)/105 || len(e.Bands) != 3*int(want) {
					t.Fatalf("rate=%d bin width=%v bytes=%d, want %d bins", e.SampleRate, e.SamplesPerBin, len(e.Bands), want)
				}
			})
		}
	}
}

func TestShadowWaveformBandSeparationAndFallback(t *testing.T) {
	for band, hz := range []float64{100, 1000, 6000} {
		t.Run(fmt.Sprint(hz), func(t *testing.T) {
			data := shadowTestTone(48000, 2*48000, hz, .7)
			e := shadowTestEnvelope(t, data, [3]float64{})
			var total [3]int
			for i := 16; i < len(e.Bands)/3; i++ { // Ignore startup transients.
				for b := range total {
					total[b] += int(e.Bands[3*i+b])
				}
			}
			if total[band] == 0 {
				t.Fatal("dominant band is silent")
			}
			for b := range total {
				if b != band && total[b]*5 >= total[band] {
					t.Fatalf("filter leakage amplified: band sums %v", total)
				}
			}
			full := shadowTestEnvelope(t, data, [3]float64{1, 1, 1})
			if !bytes.Equal(e.Bands, full.Bands) {
				t.Fatal("missing peaks must share full scale, not independently normalize each band")
			}
		})
	}
	data := shadowTestTone(48000, 48000, 100, .3)
	shared := shadowTestEnvelope(t, data, [3]float64{.5, .5, .5})
	partial := shadowTestEnvelope(t, data, [3]float64{.5, math.NaN(), math.Inf(1)})
	if !bytes.Equal(shared.Bands, partial.Bands) {
		t.Fatal("invalid peaks did not share the valid source scale")
	}
	invalid := shadowTestEnvelope(t, data, [3]float64{-1, 1e-30, 2})
	full := shadowTestEnvelope(t, data, [3]float64{1, 1, 1})
	if !bytes.Equal(invalid.Bands, full.Bands) {
		t.Fatal("invalid trackData peaks were used")
	}
}

func TestShadowWaveformUsesSourcePeaksAndClamps(t *testing.T) {
	data := shadowTestTone(48000, 48000, 100, .6)
	normal := shadowTestEnvelope(t, data, [3]float64{1, 1, 1})
	scaled := shadowTestEnvelope(t, data, [3]float64{.5, 1, 1})
	clamped := shadowTestEnvelope(t, data, [3]float64{.01, 1, 1})
	for i := 16; i < len(normal.Bands)/3; i++ {
		if scaled.Bands[i*3] <= normal.Bands[i*3] || clamped.Bands[i*3] != 255 {
			t.Fatal("source peak normalization/clamping failed")
		}
		if scaled.Bands[i*3+1] != normal.Bands[i*3+1] || scaled.Bands[i*3+2] != normal.Bands[i*3+2] {
			t.Fatal("valid source band scales were not kept independently")
		}
	}
}

type shadowFragmentReader struct {
	r   io.Reader
	max int
}

func (r shadowFragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.r.Read(p)
}

func TestShadowWaveformStreamsAcrossFLACAndReadBoundaries(t *testing.T) {
	song := testaudio.Song(2)
	wav := testaudio.WAV(song)
	var encoded bytes.Buffer
	if err := testaudio.FLAC(&encoded, song); err != nil {
		t.Fatal(err)
	}
	want := shadowTestEnvelope(t, wav, [3]float64{.8, .5, .3})
	for name, data := range map[string][]byte{"wav": wav, "flac": encoded.Bytes()} {
		for _, quantum := range []int{1, 7, 4096} {
			t.Run(fmt.Sprintf("%s/read%d", name, quantum), func(t *testing.T) {
				got, err := shadowWaveformReader(shadowFragmentReader{bytes.NewReader(data), quantum}, [3]float64{.8, .5, .3}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got.SampleRate != want.SampleRate || got.SamplesPerBin != want.SamplesPerBin || !bytes.Equal(got.Bands, want.Bands) {
					t.Fatal("codec/chunk boundary changed the continuous filter envelope")
				}
			})
		}
	}
}

func TestShadowWaveformFLACFormatsAndMalformedFrames(t *testing.T) {
	for _, bits := range []int{8, 16, 24, 32} {
		for _, channels := range []int{1, 8} {
			for _, inherit := range []bool{false, true} {
				if bits == 32 && !inherit {
					continue
				}
				b := constantFLAC(bits, channels, inherit)
				e := shadowTestEnvelope(t, b, [3]float64{1, 1, 1})
				if e.SampleRate != 8000 || len(e.Bands) != 3 {
					t.Fatal("FLAC format or short final bin lost")
				}
			}
		}
	}
	base := constantFLAC(16, 1, false)
	badCRC := bytes.Clone(base)
	badCRC[len(badCRC)-1] ^= 1
	badCount := bytes.Clone(base)
	packed := binary.BigEndian.Uint64(badCount[18:])
	binary.BigEndian.PutUint64(badCount[18:], packed&^((1<<36)-1)|15)
	tooLong := bytes.Clone(base)
	binary.BigEndian.PutUint64(tooLong[18:], packed&^((1<<36)-1)|uint64(8000*shadowEnvelopeSeconds+1))
	badRate := bytes.Clone(base)
	binary.BigEndian.PutUint64(badRate[18:], packed&^(((uint64(1)<<20)-1)<<44)|uint64(48000)<<44)
	for name, b := range map[string][]byte{"crc": badCRC, "count": badCount, "duration": tooLong, "frame-rate": badRate} {
		t.Run(name, func(t *testing.T) {
			if got, err := shadowWaveformReader(bytes.NewReader(b), [3]float64{}, nil); err == nil || got != nil {
				t.Fatal("malformed FLAC accepted")
			}
		})
	}
	for _, n := range []int{0, 3, 4, 41, 42, len(base) - 1} {
		if got, err := shadowWaveformReader(bytes.NewReader(base[:n]), [3]float64{}, nil); err == nil || got != nil {
			t.Fatalf("FLAC truncated at %d accepted", n)
		}
	}
	unknown := bytes.Clone(base)
	binary.BigEndian.PutUint64(unknown[18:], packed&^((1<<36)-1))
	if got := shadowTestEnvelope(t, unknown, [3]float64{}); len(got.Bands) != 3 {
		t.Fatal("unknown FLAC total sample count rejected")
	}
}

func TestShadowWaveformMP3RatesTrailersAndTruncation(t *testing.T) {
	for _, rate := range []int{16000, 32000, 44100, 48000} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			b, samples := shadowTestMP3(rate)
			for _, tagged := range []bool{false, true} {
				data := bytes.Clone(b)
				if tagged {
					tag := make([]byte, 128)
					copy(tag, "TAG")
					data = append(data, tag...)
				}
				e := shadowTestEnvelope(t, data, [3]float64{})
				want := (samples*105 + rate - 1) / rate
				if e.SampleRate != rate || len(e.Bands) != want*3 || !bytes.Equal(e.Bands, make([]byte, want*3)) {
					t.Fatal("silent MP3 rate, frame count or envelope incorrect")
				}
			}
			for _, n := range []int{4, len(b)/2 + 4, len(b) - 1} {
				if got, err := shadowWaveformReader(bytes.NewReader(b[:n]), [3]float64{}, nil); err == nil || got != nil {
					t.Fatalf("MP3 truncated at %d accepted", n)
				}
			}
			junk := append(bytes.Clone(b), 1, 2)
			if got, err := shadowWaveformReader(bytes.NewReader(junk), [3]float64{}, nil); err == nil || got != nil {
				t.Fatal("MP3 incomplete trailing header accepted")
			}
		})
	}
	b, _ := shadowTestMP3(44100)
	other, _ := shadowTestMP3(48000)
	mixed := append(bytes.Clone(b[:len(b)/2]), other...)
	if got, err := shadowWaveformReader(bytes.NewReader(mixed), [3]float64{}, nil); err == nil || got != nil {
		t.Fatal("changing MP3 sample rate accepted")
	}
}

func TestShadowWaveformWAVRejectsMalformedAndNonFinite(t *testing.T) {
	base := floatWAV(32, 1, []float64{.5, .1, -.2})
	badAlign := bytes.Clone(base)
	binary.LittleEndian.PutUint16(badAlign[32:], 0)
	badSize := bytes.Clone(base)
	binary.LittleEndian.PutUint32(badSize[40:], 11)
	long := bytes.Clone(base)
	binary.LittleEndian.PutUint32(long[40:], uint32((48000*shadowEnvelopeSeconds+1)*4))
	for name, data := range map[string][]byte{
		"align": badAlign, "partial-frame": badSize, "duration": long,
		"truncated": base[:len(base)-4], "empty": floatWAV(32, 1, nil),
		"nan": floatWAV(32, 1, []float64{math.NaN()}), "inf": floatWAV(64, 1, []float64{math.Inf(1)}),
		"overflow": floatWAV(64, 1, []float64{math.MaxFloat64}),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := shadowWaveformReader(bytes.NewReader(data), [3]float64{}, nil); err == nil || got != nil {
				t.Fatal("malformed/invalid WAV accepted")
			}
		})
	}
	stereo := floatWAV(32, 2, []float64{.5, -.5, .25, -.25})
	if e := shadowTestEnvelope(t, stereo, [3]float64{}); !bytes.Equal(e.Bands, []byte{0, 0, 0}) {
		t.Fatal("stereo was not mixed as source mono PCM")
	}
}

func TestShadowWaveformWAVPCMWidthsAndExtensible(t *testing.T) {
	for _, bits := range []int{8, 24, 32} {
		values := []float64{0, .5, -.5, .25, -.25, 0}
		b := make([]byte, 44+len(values)*bits/8)
		copy(b, floatWAV(64, 1, nil)[:44])
		binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
		binary.LittleEndian.PutUint16(b[20:], wavFormatPCM)
		binary.LittleEndian.PutUint32(b[28:], uint32(48000*bits/8))
		binary.LittleEndian.PutUint16(b[32:], uint16(bits/8))
		binary.LittleEndian.PutUint16(b[34:], uint16(bits))
		binary.LittleEndian.PutUint32(b[40:], uint32(len(b)-44))
		for i, v := range values {
			x := int64(v * math.Exp2(float64(bits-1)))
			if bits == 8 {
				x += 128
			}
			for j := 0; j < bits/8; j++ {
				b[44+i*(bits/8)+j] = byte(x >> (8 * j))
			}
		}
		got := shadowTestEnvelope(t, b, [3]float64{})
		want := shadowTestEnvelope(t, floatWAV(64, 1, values), [3]float64{})
		if !bytes.Equal(got.Bands, want.Bands) {
			t.Fatalf("PCM %d-bit conversion differs from equivalent float PCM", bits)
		}
	}
	base := floatWAV(64, 1, []float64{0, .5, -.5})
	ext := make([]byte, len(base)+24)
	copy(ext[:36], base[:36])
	copy(ext[60:], base[36:])
	binary.LittleEndian.PutUint32(ext[4:], uint32(len(ext)-8))
	binary.LittleEndian.PutUint32(ext[16:], 40)
	binary.LittleEndian.PutUint16(ext[20:], wavFormatExtensible)
	binary.LittleEndian.PutUint16(ext[36:], 22)
	binary.LittleEndian.PutUint16(ext[38:], 64)
	binary.LittleEndian.PutUint32(ext[44:], wavFormatFloat)
	copy(ext[48:60], []byte{0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	if got, want := shadowTestEnvelope(t, ext, [3]float64{}), shadowTestEnvelope(t, base, [3]float64{}); !bytes.Equal(got.Bands, want.Bands) {
		t.Fatal("extensible float PCM changed envelope")
	}
	ext[59] ^= 1
	if got, err := shadowWaveformReader(bytes.NewReader(ext), [3]float64{}, nil); got != nil || err == nil {
		t.Fatal("invalid extensible GUID accepted")
	}
	for _, size := range []uint32{0, 0xffffffff} {
		streamed := bytes.Clone(base)
		binary.LittleEndian.PutUint32(streamed[4:], 0xffffffff)
		binary.LittleEndian.PutUint32(streamed[40:], size)
		if got, want := shadowTestEnvelope(t, streamed, [3]float64{}), shadowTestEnvelope(t, base, [3]float64{}); !bytes.Equal(got.Bands, want.Bands) {
			t.Fatal("streaming WAV length changed envelope")
		}
	}
}

type shadowTestProgress struct {
	shadowTestTicker
	read []int64
}

func (p *shadowTestProgress) Progress(n int64) { p.read = append(p.read, n) }

// A ticker that asks for it hears the bytes read so far: they only grow, and
// end at the file's size.
func TestShadowWaveformReportsBytesRead(t *testing.T) {
	data := shadowTestTone(48000, 48000, 100, 0.5)
	p := &shadowTestProgress{shadowTestTicker: func() error { return nil }}
	if _, err := shadowWaveformReader(bytes.NewReader(data), [3]float64{}, p); err != nil {
		t.Fatal(err)
	}
	if len(p.read) < 2 || p.read[len(p.read)-1] != int64(len(data)) {
		t.Fatalf("%d reports, the last %v, file is %d bytes", len(p.read), p.read[max(0, len(p.read)-1):], len(data))
	}
	for i := 1; i < len(p.read); i++ {
		if p.read[i] < p.read[i-1] || p.read[i]-p.read[i-1] > shadowEnvelopeChunk {
			t.Fatalf("report %d: %d after %d", i, p.read[i], p.read[i-1])
		}
	}
}

func TestShadowWaveformCancellationAndSampleQuantum(t *testing.T) {
	cancel := errors.New("cancel shadow envelope")
	var e *shadowEnvelopeBuilder
	var last int64
	calls := 0
	ticker := shadowTestTicker(func() error {
		calls++
		if gap := e.samples - last; gap > shadowEnvelopeChunk {
			t.Fatalf("ticker gap %d samples exceeds 4096", gap)
		}
		last = e.samples
		if e.samples >= 8192 {
			return cancel
		}
		return nil
	})
	var err error
	e, err = newShadowEnvelopeBuilder(48000, [3]float64{}, ticker)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		err = e.add(0)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, cancel) || calls != 3 || e.samples != 8192 {
		t.Fatal("filtering did not cancel at its bounded sample quantum")
	}
	mp3data, _ := shadowTestMP3(44100)
	for name, data := range map[string][]byte{"wav": shadowTestTone(48000, 48000, 100, .2), "flac": constantFLAC(16, 1, false), "mp3": mp3data} {
		t.Run(name, func(t *testing.T) {
			n := 0
			ticker := shadowTestTicker(func() error {
				n++
				if n >= 3 {
					return context.Canceled
				}
				return nil
			})
			got, err := shadowWaveformReader(bytes.NewReader(data), [3]float64{}, ticker)
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: got=%v err=%v", got, err)
			}
		})
	}
}

func TestShadowWaveformBoundsWithoutAllocatingLongPCM(t *testing.T) {
	e, err := newShadowEnvelopeBuilder(384000, [3]float64{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.samples = e.limit
	if err := e.add(0); err == nil {
		t.Fatal("two-hour limit not enforced")
	}
	e, _ = newShadowEnvelopeBuilder(8000, [3]float64{}, nil)
	e.binLimit = 2
	for i := 0; i < 200; i++ {
		if err = e.add(0); err != nil {
			break
		}
	}
	if err == nil || len(e.bands) > 6 {
		t.Fatal("bin limit not enforced before growing output")
	}
}

func TestShadowWaveformFileSniffsID3FLACAndDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "actually-flac.mp3")
	b := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}, constantFLAC(16, 1, false)...)
	if err := os.WriteFile(path, b, 0400); err != nil {
		t.Fatal(err)
	}
	e, err := ShadowWaveformFile(path, [3]float64{}, nil)
	if err != nil || e == nil || e.SampleRate != 8000 {
		t.Fatalf("header sniff failed: envelope=%v err=%v", e, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, b) {
		t.Fatal("source changed")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("envelope created a sidecar")
	}
	if got, err := ShadowWaveformFile(filepath.Join(dir, "missing"), [3]float64{}, nil); got != nil || err == nil {
		t.Fatal("missing input accepted")
	}
}

func FuzzShadowWaveformReader(f *testing.F) {
	f.Add([]byte("not an audio file"))
	f.Add(floatWAV(32, 1, []float64{0, .5, -.5}))
	f.Add(constantFLAC(16, 1, false))
	mp3data, _ := shadowTestMP3(44100)
	f.Add(mp3data)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			t.Skip()
		}
		ticks := 0
		ticker := shadowTestTicker(func() error {
			ticks++
			if ticks > 128 {
				return context.Canceled
			}
			return nil
		})
		got, err := shadowWaveformReader(bytes.NewReader(data), [3]float64{}, ticker)
		if err != nil {
			if got != nil {
				t.Fatal("partial envelope returned with failure")
			}
			return
		}
		if got == nil || got.SampleRate < 8000 || got.SampleRate > 384000 || len(got.Bands) == 0 || len(got.Bands)%3 != 0 || len(got.Bands)/3 > shadowEnvelopeBins {
			t.Fatal("invalid successful envelope")
		}
	})
}
