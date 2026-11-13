package deemixclient

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// An uploader's original may be WAV or AIFF. The TIDAL client that plays streams
// knows FLAC, so such a file is re-encoded to FLAC: the same samples, smaller.

var errNotPCM = errors.New("audio: not uncompressed PCM")

type pcmLayout struct {
	channels, rate, bits int
	bigEndian, unsigned  bool
	offset, length       int64 // where the sample data starts, and its size
}

func (p pcmLayout) frameBytes() int { return p.channels * p.bits / 8 }

// readPCMLayout reads a WAV or AIFF header. Formats FLAC cannot hold as they
// are (float, more than 24 bits) are refused, so the caller falls back.
func readPCMLayout(f *os.File) (pcmLayout, error) {
	st, err := f.Stat()
	if err != nil {
		return pcmLayout{}, err
	}
	var head [12]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return pcmLayout{}, errNotPCM
	}
	var p pcmLayout
	switch {
	case string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		p, err = wavLayout(f)
	case string(head[0:4]) == "FORM" && (string(head[8:12]) == "AIFF" || string(head[8:12]) == "AIFC"):
		p, err = aiffLayout(f, string(head[8:12]) == "AIFC")
	default:
		return pcmLayout{}, errNotPCM
	}
	if err != nil {
		return pcmLayout{}, err
	}
	if p.channels < 1 || p.channels > 8 || (p.bits != 8 && p.bits != 16 && p.bits != 24) || p.rate < 8000 || p.rate > 192000 {
		return pcmLayout{}, errNotPCM
	}
	// A header written before the size was known claims more than there is.
	p.length = min(p.length, st.Size()-p.offset)
	p.length -= p.length % int64(p.frameBytes())
	if p.length <= 0 {
		return pcmLayout{}, errors.New("audio: no samples")
	}
	return p, nil
}

func wavLayout(f *os.File) (pcmLayout, error) {
	var p pcmLayout
	haveFormat := false
	for at := int64(12); ; {
		var h [8]byte
		if _, err := f.ReadAt(h[:], at); err != nil {
			return p, errNotPCM
		}
		size := int64(binary.LittleEndian.Uint32(h[4:]))
		switch string(h[:4]) {
		case "fmt ":
			b := make([]byte, min(size, 40))
			if size < 16 {
				return p, errNotPCM
			}
			if _, err := f.ReadAt(b, at+8); err != nil {
				return p, errNotPCM
			}
			format := binary.LittleEndian.Uint16(b[0:])
			if format == 0xFFFE && len(b) >= 26 {
				format = binary.LittleEndian.Uint16(b[24:])
			}
			if format != 1 {
				return p, errNotPCM
			}
			p.channels = int(binary.LittleEndian.Uint16(b[2:]))
			p.rate = int(binary.LittleEndian.Uint32(b[4:]))
			p.bits = int(binary.LittleEndian.Uint16(b[14:]))
			p.unsigned = p.bits == 8
			haveFormat = true
		case "data":
			if !haveFormat {
				return p, errNotPCM
			}
			p.offset, p.length = at+8, size
			return p, nil
		}
		at += 8 + size + size%2
	}
}

func aiffLayout(f *os.File, compressed bool) (pcmLayout, error) {
	p := pcmLayout{bigEndian: true}
	haveFormat := false
	for at := int64(12); ; {
		var h [8]byte
		if _, err := f.ReadAt(h[:], at); err != nil {
			return p, errNotPCM
		}
		size := int64(binary.BigEndian.Uint32(h[4:]))
		switch string(h[:4]) {
		case "COMM":
			var b [22]byte
			if size < 18 {
				return p, errNotPCM
			}
			if _, err := f.ReadAt(b[:min(size, 22)], at+8); err != nil {
				return p, errNotPCM
			}
			if compressed {
				switch string(b[18:22]) {
				case "NONE", "twos":
				case "sowt":
					p.bigEndian = false
				default:
					return p, errNotPCM
				}
			}
			p.channels = int(binary.BigEndian.Uint16(b[0:]))
			p.bits = int(binary.BigEndian.Uint16(b[6:]))
			p.rate = int(math.Round(extended(b[8:18])))
			haveFormat = true
		case "SSND":
			if !haveFormat || size < 8 {
				return p, errNotPCM
			}
			var b [4]byte
			if _, err := f.ReadAt(b[:], at+8); err != nil {
				return p, errNotPCM
			}
			skip := int64(binary.BigEndian.Uint32(b[:]))
			p.offset, p.length = at+16+skip, size-8-skip
			return p, nil
		}
		at += 8 + size + size%2
	}
}

// extended decodes the 80-bit float AIFF stores its sample rate in.
func extended(b []byte) float64 {
	exp := int(binary.BigEndian.Uint16(b[0:]) & 0x7FFF)
	mant := binary.BigEndian.Uint64(b[2:])
	if exp == 0 && mant == 0 {
		return 0
	}
	return math.Ldexp(float64(mant), exp-16383-63)
}

const flacBlock = 4096

// seekingBuffer lets the encoder rewrite STREAMINFO at the end without a write
// per byte to the file.
type seekingBuffer struct {
	*bufio.Writer
	f *os.File
}

func (s seekingBuffer) Seek(offset int64, whence int) (int64, error) {
	if err := s.Flush(); err != nil {
		return 0, err
	}
	return s.f.Seek(offset, whence)
}

// pcmToFLAC writes the samples of a WAV or AIFF at src into a new FLAC file next
// to it and returns that file's path.
func pcmToFLAC(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	p, err := readPCMLayout(in)
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(filepath.Dir(src), ".part-*")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(out.Name())
		}
	}()
	w := seekingBuffer{bufio.NewWriterSize(out, 1<<20), out}
	frames := p.length / int64(p.frameBytes())
	info := &meta.StreamInfo{BlockSizeMin: flacBlock, BlockSizeMax: flacBlock, SampleRate: uint32(p.rate), NChannels: uint8(p.channels), BitsPerSample: uint8(p.bits), NSamples: uint64(frames)}
	enc, err := flac.NewEncoder(w, info)
	if err != nil {
		return "", err
	}
	// Disable mewkiz/flac's built-in analysis: its 32-bit int bit-cost accumulator
	// overflows on 24-bit and high-residual PCM on armhf, picking k=0 and inflating
	// unary residuals by >200,000x. analyzeSubframe64 performs 64-bit analysis instead.
	enc.EnablePredictionAnalysis(false)
	channels := frame.Channels(p.channels - 1) // independent channels, in order
	r := bufio.NewReaderSize(io.NewSectionReader(in, p.offset, p.length), 1<<20)
	raw := make([]byte, flacBlock*p.frameBytes())
	resBuf := make([]int32, flacBlock)
	for left := frames; left > 0; {
		n := int(min(left, flacBlock))
		if _, err := io.ReadFull(r, raw[:n*p.frameBytes()]); err != nil {
			return "", errors.New("audio: truncated PCM data")
		}
		f := &frame.Frame{Header: frame.Header{HasFixedBlockSize: true, BlockSize: uint16(n), SampleRate: uint32(p.rate), Channels: channels, BitsPerSample: uint8(p.bits)}}
		for c := 0; c < p.channels; c++ {
			f.Subframes = append(f.Subframes, &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: make([]int32, n), NSamples: n})
		}
		width := p.bits / 8
		for i := 0; i < n; i++ {
			for c := 0; c < p.channels; c++ {
				f.Subframes[c].Samples[i] = pcmSample(raw[(i*p.channels+c)*width:], width, p.bigEndian, p.unsigned)
			}
		}
		for c := 0; c < p.channels; c++ {
			analyzeSubframe64(f.Subframes[c], uint(p.bits), resBuf)
		}
		if err := enc.WriteFrame(f); err != nil {
			return "", err
		}
		left -= int64(n)
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	if err := w.Flush(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return out.Name(), nil
}

func pcmSample(b []byte, width int, bigEndian, unsigned bool) int32 {
	switch width {
	case 1:
		if unsigned {
			return int32(b[0]) - 128
		}
		return int32(int8(b[0]))
	case 2:
		if bigEndian {
			return int32(int16(binary.BigEndian.Uint16(b)))
		}
		return int32(int16(binary.LittleEndian.Uint16(b)))
	default:
		if bigEndian {
			return int32(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8) >> 8
		}
		return int32(uint32(b[2])<<24|uint32(b[1])<<16|uint32(b[0])<<8) >> 8
	}
}

// analyzeSubframe64 selects constant, fixed (orders 0..4 with Rice1/Rice2), or
// verbatim encoding using uint64 bit-cost accumulation so 24-bit and high-residual
// blocks never overflow 32-bit int on armhf.
func analyzeSubframe64(sf *frame.Subframe, bps uint, resBuf []int32) {
	samples := sf.Samples
	n := len(samples)
	if n < 2 {
		sf.Pred = frame.PredVerbatim
		sf.Order = 0
		sf.RiceSubframe = nil
		return
	}
	allEqual := true
	for i := 1; i < n; i++ {
		if samples[i] != samples[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		sf.Pred = frame.PredConstant
		sf.Order = 0
		sf.RiceSubframe = nil
		return
	}

	verbatimBits := uint64(8) + uint64(n)*uint64(bps)
	bestBits := verbatimBits
	bestOrder := 0
	bestK := uint(0)
	useFixed := false

	maxK := uint(14)
	if bps > 16 {
		maxK = 24
	}

	for order := 0; order <= 4 && order < n; order++ {
		res := resBuf[:n-order]
		overflow := false
		for i := order; i < n; i++ {
			var pred int64
			switch order {
			case 1:
				pred = int64(samples[i-1])
			case 2:
				pred = 2*int64(samples[i-1]) - int64(samples[i-2])
			case 3:
				pred = 3*int64(samples[i-1]) - 3*int64(samples[i-2]) + int64(samples[i-3])
			case 4:
				pred = 4*int64(samples[i-1]) - 6*int64(samples[i-2]) + 4*int64(samples[i-3]) - int64(samples[i-4])
			}
			d := int64(samples[i]) - pred
			if d < math.MinInt32 || d > math.MaxInt32 {
				overflow = true
				break
			}
			res[i-order] = int32(d)
		}
		if overflow {
			continue
		}
		for k := uint(0); k <= maxK; k++ {
			paramBits := uint64(4)
			if k > 14 {
				paramBits = 5
			}
			bits := uint64(8) + uint64(order)*uint64(bps) + 2 + 4 + paramBits
			for _, r := range res {
				folded := uint32((r << 1) ^ (r >> 31))
				bits += uint64(folded>>k) + 1 + uint64(k)
				if bits >= bestBits {
					break
				}
			}
			if bits < bestBits {
				bestBits = bits
				bestOrder = order
				bestK = k
				useFixed = true
			}
		}
	}

	if !useFixed {
		sf.Pred = frame.PredVerbatim
		sf.Order = 0
		sf.RiceSubframe = nil
		return
	}
	sf.Pred = frame.PredFixed
	sf.Order = bestOrder
	if bestK > 14 {
		sf.ResidualCodingMethod = frame.ResidualCodingMethodRice2
	} else {
		sf.ResidualCodingMethod = frame.ResidualCodingMethodRice1
	}
	sf.RiceSubframe = &frame.RiceSubframe{
		PartOrder:  0,
		Partitions: []frame.RicePartition{{Param: bestK}},
	}
}
