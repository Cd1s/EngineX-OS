package analyzer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
	"github.com/hajimehoshi/go-mp3"
	"github.com/mewkiz/flac"
)

// ShadowEnvelope contains interleaved low/mid/high peak bytes, with no terminal
// sentinel. Bin i starts at floor(i*SamplesPerBin); the last bin may be incomplete.
// It contains display data only: no tempo, grid, key, or persistent analysis.
type ShadowEnvelope struct {
	SampleRate    int
	Bands         []byte
	SamplesPerBin float64
}

const (
	shadowEnvelopeSeconds = 2 * 60 * 60
	shadowEnvelopeBins    = 1_000_000
	shadowEnvelopeChunk   = 4096
)

// ShadowWaveformFile streams WAV, native FLAC or Layer III MP3 into a 105-bin/s
// three-band envelope. The source is opened read-only, independent of its suffix.
// Only the current decoder frame/chunk and byte envelope are retained.
//
// peaks are source trackData's normalized PCM peaks, in low/mid/high order.
// Invalid peaks share the largest valid source peak, or PCM full scale (1) if
// none is usable. No independently measured band maximum boosts filter leakage.
// ticker is optional and is called on I/O, before decoder blocks and at most
// every 4096 filtered mono samples. It owns cancellation and scheduling policy.
func ShadowWaveformFile(path string, peaks [3]float64, ticker Ticker) (*ShadowEnvelope, error) {
	if ticker != nil {
		if err := ticker.Tick(); err != nil {
			return nil, fmt.Errorf("analyzer: shadow waveform: %w", err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return shadowWaveformReader(f, peaks, ticker)
}

// A non-seekable reader also prevents go-mp3 from scanning/indexing the whole
// file in NewDecoder. Limit I/O quanta when skipping tags and FLAC metadata.
type shadowEnvelopeReader struct {
	r      io.Reader
	ticker Ticker
	err    error
}

func (r *shadowEnvelopeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.ticker != nil {
		if err := r.ticker.Tick(); err != nil {
			r.err = err
			return 0, err
		}
	}
	if len(p) > shadowEnvelopeChunk {
		p = p[:shadowEnvelopeChunk]
	}
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func shadowWaveformReader(r io.Reader, peaks [3]float64, ticker Ticker) (out *ShadowEnvelope, err error) {
	src := &shadowEnvelopeReader{r: r, ticker: ticker}
	// The decoders are third-party parsers. Malformed bitstreams must fail this
	// display-only operation rather than take down the serving process.
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("analyzer: shadow waveform: malformed audio: %v", p)
		}
		// go-mp3 can translate a decoder UnexpectedEOF into EOF. Never allow that
		// behavior to hide cancellation or a source I/O error.
		if src.err != nil {
			out, err = nil, fmt.Errorf("analyzer: shadow waveform: %w", src.err)
		}
	}()
	br := bufio.NewReaderSize(src, 16<<10)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, fmt.Errorf("analyzer: shadow waveform header: %w", err)
	}
	head, _ := br.Peek(12)
	switch {
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return shadowEnvelopeWAV(br, peaks, ticker)
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		return shadowEnvelopeFLAC(br, peaks, ticker)
	case len(head) >= 2 && head[0] == 0xff && head[1]&0xe0 == 0xe0:
		return shadowEnvelopeMP3(br, peaks, ticker)
	default:
		return nil, errors.New("analyzer: shadow waveform: unsupported or truncated audio header")
	}
}

type shadowEnvelopeBuilder struct {
	rate, binLimit int
	samples, limit int64
	bin, nextBin   int64
	peak, scale    [3]float64
	lp, mh, ml, hp [2]*biquad
	bands          []byte
	ticker         Ticker
}

func newShadowEnvelopeBuilder(rate int, peaks [3]float64, ticker Ticker) (*shadowEnvelopeBuilder, error) {
	if rate < 8000 || rate > 384000 {
		return nil, errors.New("analyzer: shadow waveform: invalid sample rate (8000..384000)")
	}
	common := 0.0
	valid := func(v float64) bool { return v > 1e-6 && v <= 1 && !math.IsNaN(v) && !math.IsInf(v, 0) }
	for _, p := range peaks {
		if valid(p) {
			common = math.Max(common, p)
		}
	}
	if common == 0 {
		common = 1
	}
	for i, p := range peaks {
		if !valid(p) {
			peaks[i] = common
		}
	}
	sr := float64(rate)
	return &shadowEnvelopeBuilder{
		rate: rate, binLimit: shadowEnvelopeBins, limit: int64(rate) * shadowEnvelopeSeconds,
		nextBin: int64(rate) / WaveformEntriesPerSecond,
		scale:   peaks, ticker: ticker, bands: make([]byte, 0, 3*1024),
		lp: [2]*biquad{newLowpass(sr, 200, butterworthQ), newLowpass(sr, 200, butterworthQ)},
		mh: [2]*biquad{newHighpass(sr, 200, butterworthQ), newHighpass(sr, 200, butterworthQ)},
		ml: [2]*biquad{newLowpass(sr, 2500, butterworthQ), newLowpass(sr, 2500, butterworthQ)},
		hp: [2]*biquad{newHighpass(sr, 2500, butterworthQ), newHighpass(sr, 2500, butterworthQ)},
	}, nil
}

func (e *shadowEnvelopeBuilder) tick() error {
	if e.ticker != nil {
		return e.ticker.Tick()
	}
	return nil
}

func (e *shadowEnvelopeBuilder) add(x float64) error {
	if e.samples%shadowEnvelopeChunk == 0 {
		if err := e.tick(); err != nil {
			return err
		}
	}
	if e.samples >= e.limit {
		return errors.New("analyzer: shadow waveform exceeds two hours")
	}
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) > math.MaxFloat32 {
		return errors.New("analyzer: shadow waveform: invalid PCM sample")
	}
	if e.samples == e.nextBin {
		if err := e.flush(); err != nil {
			return err
		}
		e.bin++
		if e.bin >= int64(e.binLimit) {
			return errors.New("analyzer: shadow waveform exceeds bin limit")
		}
		e.nextBin = (e.bin + 1) * int64(e.rate) / WaveformEntriesPerSecond
	}
	values := [3]float64{
		e.lp[1].process(e.lp[0].process(x)),
		e.ml[1].process(e.ml[0].process(e.mh[1].process(e.mh[0].process(x)))),
		e.hp[1].process(e.hp[0].process(x)),
	}
	for b, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("analyzer: shadow waveform: non-finite filter output")
		}
		e.peak[b] = math.Max(e.peak[b], math.Abs(v))
	}
	e.samples++
	return nil
}

func (e *shadowEnvelopeBuilder) flush() error {
	if len(e.bands)/3 >= e.binLimit {
		return errors.New("analyzer: shadow waveform exceeds bin limit")
	}
	for b, p := range e.peak {
		v := math.Min(1, p/e.scale[b])
		e.bands = append(e.bands, byte(math.Round(v*255)))
	}
	e.peak = [3]float64{}
	return nil
}

func (e *shadowEnvelopeBuilder) finish() (*ShadowEnvelope, error) {
	if err := e.tick(); err != nil {
		return nil, err
	}
	if e.samples == 0 {
		return nil, errors.New("analyzer: shadow waveform: empty audio")
	}
	if err := e.flush(); err != nil {
		return nil, err
	}
	return &ShadowEnvelope{SampleRate: e.rate, Bands: e.bands,
		SamplesPerBin: float64(e.rate) / WaveformEntriesPerSecond}, nil
}

func shadowEnvelopeFLAC(r io.Reader, peaks [3]float64, ticker Ticker) (*ShadowEnvelope, error) {
	stream, err := flac.New(r) // STREAMINFO only; other metadata is skipped, not retained.
	if err != nil {
		return nil, fmt.Errorf("analyzer: shadow FLAC: %w", err)
	}
	info := stream.Info
	if info == nil || info.BitsPerSample < 8 || info.BitsPerSample > 32 || info.NChannels < 1 || info.NChannels > 8 || info.BlockSizeMin < 16 || info.BlockSizeMax < info.BlockSizeMin {
		return nil, errors.New("analyzer: shadow FLAC: invalid STREAMINFO format")
	}
	e, err := newShadowEnvelopeBuilder(int(info.SampleRate), peaks, ticker)
	if err != nil {
		return nil, err
	}
	if info.NSamples > uint64(e.limit) {
		return nil, errors.New("analyzer: shadow FLAC exceeds two hours")
	}
	scale := 1 / (float64(int64(1)<<(info.BitsPerSample-1)) * float64(info.NChannels))
	for {
		if err := e.tick(); err != nil {
			return nil, err
		}
		fr, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("analyzer: shadow FLAC frame: %w", err)
		}
		if fr.BitsPerSample == 0 {
			fr.BitsPerSample = info.BitsPerSample
		}
		if fr.SampleRate == 0 {
			fr.SampleRate = info.SampleRate
		}
		n := int64(fr.BlockSize)
		if fr.BitsPerSample != info.BitsPerSample || fr.SampleRate != info.SampleRate || fr.Channels.Count() != int(info.NChannels) || n == 0 || fr.BlockSize > info.BlockSizeMax {
			return nil, errors.New("analyzer: shadow FLAC: frame differs from STREAMINFO")
		}
		if n > e.limit-e.samples || (info.NSamples != 0 && uint64(e.samples+n) > info.NSamples) {
			return nil, errors.New("analyzer: shadow FLAC: excessive frame samples")
		}
		// Parse only one bounded native frame. No full-track Audio is constructed.
		if err := fr.Parse(); err != nil {
			return nil, fmt.Errorf("analyzer: shadow FLAC samples: %w", err)
		}
		if len(fr.Subframes) != int(info.NChannels) {
			return nil, errors.New("analyzer: shadow FLAC: missing channels")
		}
		for _, sf := range fr.Subframes {
			if sf == nil || int64(sf.NSamples) != n || int64(len(sf.Samples)) != n {
				return nil, errors.New("analyzer: shadow FLAC: inconsistent subframe size")
			}
		}
		for i := 0; i < int(n); i++ {
			var sum int64
			for _, sf := range fr.Subframes {
				sum += int64(sf.Samples[i])
			}
			if err := e.add(float64(sum) * scale); err != nil {
				return nil, err
			}
		}
	}
	if info.NSamples != 0 && uint64(e.samples) != info.NSamples {
		return nil, errors.New("analyzer: shadow FLAC: truncated sample count")
	}
	return e.finish()
}

// go-mp3 hides some partial-frame errors as EOF. Feed it only complete Layer III
// frames, validating the compressed boundary before exposing any frame bytes.
// This is a framing reader, not another audio decoder or an entire-file index.
type shadowMP3Frames struct {
	r        *bufio.Reader
	buf      [4096]byte
	off, n   int
	rate     int
	mono     bool
	samples  int64
	terminal error
}

func (r *shadowMP3Frames) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.off == r.n {
		if r.terminal != nil {
			return 0, r.terminal
		}
		if err := r.frame(); err != nil {
			r.terminal = err
			return 0, err
		}
	}
	n := copy(p, r.buf[r.off:r.n])
	r.off += n
	return n, nil
}

func (r *shadowMP3Frames) frame() error {
	prefix, err := r.r.Peek(3)
	if err != nil {
		if err == io.EOF && len(prefix) != 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if string(prefix) == "TAG" { // Optional trailing ID3v1, exactly 128 bytes.
		var tag [128]byte
		if _, err := io.ReadFull(r.r, tag[:]); err != nil {
			return err
		}
		if _, err := r.r.Peek(1); err != io.EOF {
			if err != nil {
				return err
			}
			return errors.New("analyzer: shadow MP3: data after ID3v1")
		}
		return io.EOF
	}
	if _, err := io.ReadFull(r.r, r.buf[:4]); err != nil {
		return err
	}
	h := r.buf[:4]
	version, layer := (h[1]>>3)&3, (h[1]>>1)&3
	bi, si := int(h[2]>>4), int((h[2]>>2)&3)
	if h[0] != 0xff || h[1]&0xe0 != 0xe0 || version == 1 || layer != 1 || bi == 0 || bi == 15 || si == 3 || h[3]&3 == 2 {
		return errors.New("analyzer: shadow MP3: invalid or unsupported frame header")
	}
	rate := [3]int{44100, 48000, 32000}[si]
	bitrates := [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	factor, samples := 144000, int64(1152)
	if version != 3 {
		bitrates = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
		factor, samples = 72000, 576
		rate /= 2
		if version == 0 {
			rate /= 2
		}
	}
	mono := h[3]>>6 == 3
	if r.rate != 0 && (rate != r.rate || mono != r.mono) {
		return errors.New("analyzer: shadow MP3: frame format changed")
	}
	r.rate, r.mono = rate, mono
	if r.samples+samples > int64(rate)*shadowEnvelopeSeconds {
		return errors.New("analyzer: shadow MP3 exceeds two hours")
	}
	n := factor*bitrates[bi]/rate + int((h[2]>>1)&1)
	if n < 4 || n > len(r.buf) {
		return errors.New("analyzer: shadow MP3: invalid frame length")
	}
	if _, err := io.ReadFull(r.r, r.buf[4:n]); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	r.samples += samples
	r.off, r.n = 0, n
	return nil
}

func shadowEnvelopeMP3(r *bufio.Reader, peaks [3]float64, ticker Ticker) (*ShadowEnvelope, error) {
	frames := &shadowMP3Frames{r: r}
	dec, err := mp3.NewDecoder(frames)
	if frames.terminal != nil && frames.terminal != io.EOF {
		return nil, fmt.Errorf("analyzer: shadow MP3: %w", frames.terminal)
	}
	if err != nil {
		return nil, fmt.Errorf("analyzer: shadow MP3: %w", err)
	}
	e, err := newShadowEnvelopeBuilder(dec.SampleRate(), peaks, ticker)
	if err != nil {
		return nil, err
	}
	if dec.SampleRate() != frames.rate {
		return nil, errors.New("analyzer: shadow MP3: decoder rate differs from frame header")
	}
	var buf [4 * shadowEnvelopeChunk]byte // Decoder output is always stereo s16le.
	for {
		if err := e.tick(); err != nil {
			return nil, err
		}
		n, err := io.ReadFull(dec, buf[:])
		if frames.terminal != nil && frames.terminal != io.EOF {
			return nil, fmt.Errorf("analyzer: shadow MP3: %w", frames.terminal)
		}
		if n%4 != 0 {
			return nil, errors.New("analyzer: shadow MP3: truncated PCM frame")
		}
		for i := 0; i < n; i += 4 {
			l := int16(binary.LittleEndian.Uint16(buf[i:]))
			rr := int16(binary.LittleEndian.Uint16(buf[i+2:]))
			if err := e.add((float64(l) + float64(rr)) / 65536); err != nil {
				return nil, err
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break // A short final PCM read is normal only at a complete compressed boundary.
		}
		if err != nil {
			return nil, fmt.Errorf("analyzer: shadow MP3: %w", err)
		}
	}
	if e.samples != frames.samples || frames.off != frames.n {
		return nil, errors.New("analyzer: shadow MP3: incomplete decoded frame")
	}
	return e.finish()
}

type shadowWAVFormat struct {
	channels, rate, bits, align int
	convert                     func([]byte) float64
}

func shadowWAVFormatFrom(b []byte) (*shadowWAVFormat, error) {
	if len(b) < 16 {
		return nil, errors.New("analyzer: shadow WAV: short fmt chunk")
	}
	format := int(binary.LittleEndian.Uint16(b))
	f := &shadowWAVFormat{channels: int(binary.LittleEndian.Uint16(b[2:])),
		rate: int(binary.LittleEndian.Uint32(b[4:])), align: int(binary.LittleEndian.Uint16(b[12:])),
		bits: int(binary.LittleEndian.Uint16(b[14:]))}
	if format == wavFormatExtensible {
		guidTail := []byte{0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
		if len(b) < 40 || binary.LittleEndian.Uint16(b[16:]) < 22 || !bytes.Equal(b[28:40], guidTail) || binary.LittleEndian.Uint16(b[18:]) > uint16(f.bits) {
			return nil, errors.New("analyzer: shadow WAV: invalid extensible fmt")
		}
		format = int(binary.LittleEndian.Uint32(b[24:]))
	}
	if f.channels < 1 || f.channels > 8 || f.rate < 8000 || f.rate > 384000 || f.bits%8 != 0 || f.align != f.channels*(f.bits/8) || uint64(binary.LittleEndian.Uint32(b[8:])) != uint64(f.rate)*uint64(f.align) {
		return nil, errors.New("analyzer: shadow WAV: invalid sample format")
	}
	switch {
	case format == wavFormatPCM && f.bits == 8:
		f.convert = func(b []byte) float64 { return (float64(b[0]) - 128) / 128 }
	case format == wavFormatPCM && f.bits == 16:
		f.convert = func(b []byte) float64 { return float64(int16(binary.LittleEndian.Uint16(b))) / 32768 }
	case format == wavFormatPCM && f.bits == 24:
		f.convert = func(b []byte) float64 {
			v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
			return float64(v) / 8388608
		}
	case format == wavFormatPCM && f.bits == 32:
		f.convert = func(b []byte) float64 { return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648 }
	case format == wavFormatFloat && f.bits == 32:
		f.convert = func(b []byte) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) }
	case format == wavFormatFloat && f.bits == 64:
		f.convert = func(b []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }
	default:
		return nil, errors.New("analyzer: shadow WAV: unsupported PCM encoding")
	}
	return f, nil
}

func shadowEnvelopeWAV(r io.Reader, peaks [3]float64, ticker Ticker) (*ShadowEnvelope, error) {
	var head [12]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	if string(head[:4]) != "RIFF" || string(head[8:]) != "WAVE" {
		return nil, errors.New("analyzer: shadow WAV: invalid RIFF header")
	}
	var riff *io.LimitedReader
	if size := binary.LittleEndian.Uint32(head[4:]); size != 0 && size != 0xffffffff {
		if size < 4 {
			return nil, errors.New("analyzer: shadow WAV: invalid RIFF length")
		}
		riff = &io.LimitedReader{R: r, N: int64(size) - 4}
		r = riff
	}
	var format *shadowWAVFormat
	for {
		var h [8]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, fmt.Errorf("analyzer: shadow WAV: missing data chunk: %w", err)
		}
		size := int64(binary.LittleEndian.Uint32(h[4:]))
		switch string(h[:4]) {
		case "fmt ":
			if format != nil || size < 16 || size > 65536 {
				return nil, errors.New("analyzer: shadow WAV: invalid fmt length or duplicate fmt")
			}
			b := make([]byte, int(size))
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, err
			}
			var err error
			format, err = shadowWAVFormatFrom(b)
			if err != nil {
				return nil, err
			}
		case "data":
			if format == nil {
				return nil, errors.New("analyzer: shadow WAV: data before fmt")
			}
			e, err := newShadowEnvelopeBuilder(format.rate, peaks, ticker)
			if err != nil {
				return nil, err
			}
			unknown := size == 0xffffffff || (size == 0 && riff == nil)
			var src io.Reader = r
			var data *io.LimitedReader
			if !unknown {
				if size%int64(format.align) != 0 || size/int64(format.align) > e.limit {
					return nil, errors.New("analyzer: shadow WAV: invalid or excessive data length")
				}
				data = &io.LimitedReader{R: r, N: size}
				src = data
			}
			if err := shadowEnvelopePCM(src, format, e); err != nil {
				return nil, err
			}
			if data != nil && data.N != 0 {
				return nil, errors.New("analyzer: shadow WAV: truncated data chunk")
			}
			if !unknown && size%2 != 0 {
				if _, err := io.CopyN(io.Discard, r, 1); err != nil {
					return nil, err
				}
			}
			if riff != nil {
				if _, err := io.Copy(io.Discard, riff); err != nil {
					return nil, err
				}
				if riff.N != 0 {
					return nil, errors.New("analyzer: shadow WAV: truncated RIFF container")
				}
			}
			return e.finish()
		default:
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return nil, err
			}
		}
		if size%2 != 0 {
			if _, err := io.CopyN(io.Discard, r, 1); err != nil {
				return nil, err
			}
		}
	}
}

func shadowEnvelopePCM(r io.Reader, f *shadowWAVFormat, e *shadowEnvelopeBuilder) error {
	buf := make([]byte, f.align*shadowEnvelopeChunk)
	for {
		if err := e.tick(); err != nil {
			return err
		}
		n, err := io.ReadFull(r, buf)
		if n%f.align != 0 {
			return errors.New("analyzer: shadow WAV: truncated PCM frame")
		}
		for off := 0; off < n; off += f.align {
			sum := 0.0
			for c := 0; c < f.channels; c++ {
				v := f.convert(buf[off+c*(f.bits/8):])
				if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32 {
					return errors.New("analyzer: shadow WAV: invalid PCM sample")
				}
				sum += v / float64(f.channels)
			}
			if err := e.add(sum); err != nil {
				return err
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
