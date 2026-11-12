package stems

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

const (
	nativeMaxBlock = 65536
	nativeMaxRate  = 192000.0
	nativeBatch    = 64
)

// StreamSeparator separates planar [channels][frames] float32 blocks into
// planar [4][channels][frames] float32 stems (vocals, bass, drums, melody).
type StreamSeparator interface {
	Feed(ctx context.Context, x [][]float32) ([][][]float32, error)
	Drain(ctx context.Context) ([][][]float32, error)
}

// ServeNative serves one Engine 5.0.4 child-protocol session in-process using factory.
func ServeNative(ctx context.Context, in io.Reader, out io.Writer, factory func(channels int, sampleRate float64) (StreamSeparator, error)) error {
	r := bufio.NewReaderSize(in, 256<<10)
	w := bufio.NewWriterSize(out, 256<<10)
	writeHeader := func(lines ...string) error {
		for _, l := range lines {
			if _, err := w.WriteString(l + "\n"); err != nil {
				return err
			}
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
		return w.Flush()
	}
	if err := writeHeader("session=FFFFFFFFFFFFFFFF"); err != nil {
		return err
	}
	init, err := readNativeHeader(r, true)
	if err == io.EOF {
		return nil // presence check
	}
	if err != nil {
		return err
	}
	for _, reqKey := range []string{"numChannels", "sampleRate", "maxBlockSize"} {
		if _, ok := init[reqKey]; !ok {
			return fmt.Errorf("stems native: init header lacks %s", reqKey)
		}
	}
	channels, err := strconv.Atoi(init["numChannels"])
	if err != nil || (channels != 1 && channels != 2) {
		return fmt.Errorf("stems native: unsupported numChannels %q", init["numChannels"])
	}
	sampleRate, err := strconv.ParseFloat(init["sampleRate"], 64)
	if err != nil || !(sampleRate >= 8000 && sampleRate <= nativeMaxRate) {
		return fmt.Errorf("stems native: unsupported sampleRate %q", init["sampleRate"])
	}
	maxBlock, err := strconv.Atoi(init["maxBlockSize"])
	if err != nil || maxBlock < 1 || maxBlock > nativeMaxBlock {
		return fmt.Errorf("stems native: unsupported maxBlockSize %q", init["maxBlockSize"])
	}
	sep, err := factory(channels, sampleRate)
	if err != nil {
		return err
	}
	if err := writeHeader("preparing=ready"); err != nil {
		return err
	}
	if err := writeHeader("preparing=ready", fmt.Sprintf("numReturnChannels=%d", channels*4)); err != nil {
		return err
	}

	// Queued output per stem & channel: [4][channels][]float32
	queued := make([][][]float32, 4)
	for s := 0; s < 4; s++ {
		queued[s] = make([][]float32, channels)
	}
	appendOut := func(y [][][]float32) {
		if len(y) == 0 || len(y[0]) == 0 || len(y[0][0]) == 0 {
			return
		}
		for s := 0; s < 4; s++ {
			for c := 0; c < channels; c++ {
				queued[s][c] = append(queued[s][c], y[s][c]...)
			}
		}
	}
	queuedLen := func() int {
		return len(queued[0][0])
	}
	var outBuf []byte
	sendReply := func(limit int) error {
		n := min(limit, queuedLen())
		if _, err := w.WriteString("returnFrames=" + strconv.Itoa(n) + "\n\n"); err != nil {
			return err
		}
		if n > 0 {
			need := 4 * channels * n * 4
			if cap(outBuf) < need {
				outBuf = make([]byte, need)
			}
			outBuf = outBuf[:need]
			off := 0
			for s := 0; s < 4; s++ {
				for c := 0; c < channels; c++ {
					plane := queued[s][c][:n]
					for k := 0; k < n; k++ {
						binary.LittleEndian.PutUint32(outBuf[off:], math.Float32bits(plane[k]))
						off += 4
					}
					queued[s][c] = queued[s][c][n:]
				}
			}
			if _, err := w.Write(outBuf); err != nil {
				return err
			}
		}
		return w.Flush()
	}

	var rawPCM []byte
	fields, err := readNativeHeader(r, false)
	for err == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		blocks := 0
		batch := make([][]float32, channels)
		hitDone := false
		for {
			action := fields["action"]
			if action == "done" {
				hitDone = true
				break
			}
			if action != "block" {
				return fmt.Errorf("stems native: unsupported action %q", action)
			}
			ns, err := strconv.Atoi(fields["numSamples"])
			if err != nil || ns < 0 || ns > maxBlock {
				return fmt.Errorf("stems native: invalid numSamples %q", fields["numSamples"])
			}
			if ns > 0 {
				need := ns * channels * 4
				if cap(rawPCM) < need {
					rawPCM = make([]byte, need)
				}
				rawPCM = rawPCM[:need]
				if _, err := io.ReadFull(r, rawPCM); err != nil {
					return errors.New("stems native: truncated PCM payload")
				}
				for c := 0; c < channels; c++ {
					base := c * ns * 4
					for k := 0; k < ns; k++ {
						v := math.Float32frombits(binary.LittleEndian.Uint32(rawPCM[base+k*4:]))
						if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
							return errors.New("stems native: non-finite PCM")
						}
						batch[c] = append(batch[c], v)
					}
				}
			}
			blocks++
			if blocks >= nativeBatch || r.Buffered() == 0 {
				fields = nil
				break
			}
			fields, err = readNativeHeader(r, false)
			if err != nil {
				if err == io.EOF {
					fields = nil
					err = nil
				}
				break
			}
		}
		if err != nil {
			return err
		}
		if len(batch[0]) > 0 {
			y, err := sep.Feed(ctx, batch)
			if err != nil {
				return err
			}
			appendOut(y)
		}
		for i := 0; i < blocks; i++ {
			if err := sendReply(maxBlock); err != nil {
				return err
			}
		}
		if hitDone {
			y, err := sep.Drain(ctx)
			if err != nil {
				return err
			}
			appendOut(y)
			for queuedLen() > 0 {
				if err := sendReply(maxBlock); err != nil {
					return err
				}
			}
			return sendReply(0)
		}
		if fields == nil {
			fields, err = readNativeHeader(r, false)
		}
	}
	if err == io.EOF {
		return nil
	}
	return err
}

func readNativeHeader(r *bufio.Reader, skipLeadingBlanks bool) (map[string]string, error) {
	for {
		fields := map[string]string{}
		used := 0
		for {
			line, err := r.ReadString('\n')
			used += len(line)
			if used > maxHeader {
				return nil, errors.New("stems native: header too large")
			}
			if err != nil {
				if err == io.EOF && len(fields) == 0 && line == "" {
					return nil, io.EOF
				}
				return nil, errors.New("stems native: truncated header")
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if len(fields) == 0 && skipLeadingBlanks {
					break
				}
				return fields, nil
			}
			k, v, ok := strings.Cut(trimmed, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("stems native: invalid header line %q", trimmed)
			}
			if _, dup := fields[k]; dup {
				return nil, fmt.Errorf("stems native: duplicate header key %q", k)
			}
			fields[k] = v
		}
	}
}

// HPSSSeparator is a pure-Go causal WOLA spectral harmonic-percussive separator
// (2048-point Hann window, 512 hop, 17-frame harmonic median, 31-bin percussive median).
type HPSSSeparator struct {
	channels  int
	fft       *radix2FFT
	window    []float32
	pending   [][]float32
	ola       [][][]float32 // [4][channels][size]
	norm      []float32
	history   [][]float32 // [<=17][bins]
	low       []float32
	vocalBand []float32
	skip      int
	received  int
	returned  int
	finished  bool
}

const (
	hpssSize    = 2048
	hpssHop     = 512
	hpssBins    = hpssSize/2 + 1
	hpssHistory = 17
)

func NewHPSSSeparator(channels int, sampleRate float64) (*HPSSSeparator, error) {
	if channels < 1 || channels > 2 || !(sampleRate >= 8000 && sampleRate <= nativeMaxRate) {
		return nil, errors.New("hpss: invalid channels or sample rate")
	}
	s := &HPSSSeparator{
		channels:  channels,
		fft:       newRadix2FFT(hpssSize),
		window:    hannSymmetric(hpssSize),
		pending:   make([][]float32, channels),
		ola:       make([][][]float32, 4),
		norm:      make([]float32, hpssSize),
		low:       make([]float32, hpssBins),
		vocalBand: make([]float32, hpssBins),
		skip:      hpssSize - hpssHop,
	}
	for c := 0; c < channels; c++ {
		s.pending[c] = make([]float32, hpssSize-hpssHop)
	}
	for st := 0; st < 4; st++ {
		s.ola[st] = make([][]float32, channels)
		for c := 0; c < channels; c++ {
			s.ola[st][c] = make([]float32, hpssSize)
		}
	}
	for b := 0; b < hpssBins; b++ {
		f := float64(b) * sampleRate / float64(hpssSize)
		s.low[b] = float32(1.0 / (1.0 + math.Pow(f/220.0, 6)))
		fmax := math.Max(f, 1.0)
		s.vocalBand[b] = float32((1.0 / (1.0 + math.Pow(180.0/fmax, 6))) * (1.0 / (1.0 + math.Pow(f/5000.0, 6))))
	}
	return s, nil
}

func (s *HPSSSeparator) Feed(_ context.Context, x [][]float32) ([][][]float32, error) {
	if s.finished {
		return nil, errors.New("hpss: PCM after drain")
	}
	if len(x) != s.channels {
		return nil, errors.New("hpss: channel mismatch")
	}
	n := len(x[0])
	for c := 0; c < s.channels; c++ {
		for _, v := range x[c] {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, errors.New("hpss: non-finite PCM")
			}
		}
	}
	s.received += n
	out := s.process(x)
	s.returned += len(out[0][0])
	return out, nil
}

func (s *HPSSSeparator) Drain(_ context.Context) ([][][]float32, error) {
	if s.finished {
		return emptyStems(s.channels), nil
	}
	left := s.received - s.returned
	zeros := make([][]float32, s.channels)
	for c := 0; c < s.channels; c++ {
		zeros[c] = make([]float32, hpssSize+hpssHop)
	}
	out := s.process(zeros)
	for st := 0; st < 4; st++ {
		for c := 0; c < s.channels; c++ {
			if len(out[st][c]) > left {
				out[st][c] = out[st][c][:left]
			}
		}
	}
	s.returned += len(out[0][0])
	s.finished = true
	if s.returned != s.received {
		return nil, fmt.Errorf("hpss: drain frame mismatch (%d != %d)", s.returned, s.received)
	}
	return out, nil
}

func emptyStems(channels int) [][][]float32 {
	out := make([][][]float32, 4)
	for st := 0; st < 4; st++ {
		out[st] = make([][]float32, channels)
	}
	return out
}

func medianSlice(buf []float32) float32 {
	slices.Sort(buf)
	m := len(buf) / 2
	if len(buf)%2 == 1 {
		return buf[m]
	}
	return 0.5 * (buf[m-1] + buf[m])
}

func (s *HPSSSeparator) process(x [][]float32) [][][]float32 {
	for c := 0; c < s.channels; c++ {
		s.pending[c] = append(s.pending[c], x[c]...)
	}
	plen := len(s.pending[0])
	if plen < hpssSize {
		return emptyStems(s.channels)
	}
	count := (plen-hpssSize)/hpssHop + 1
	out := emptyStems(s.channels)

	winFrame := make([]float32, hpssSize)
	zRe := make([][]float32, s.channels)
	zIm := make([][]float32, s.channels)
	for c := 0; c < s.channels; c++ {
		zRe[c] = make([]float32, hpssBins)
		zIm[c] = make([]float32, hpssBins)
	}
	scratch := make([]float64, hpssSize)
	mag := make([]float32, hpssBins)
	harmonic := make([]float32, hpssBins)
	percussive := make([]float32, hpssBins)
	medScratch := make([]float32, 31)
	sRe := make([]float32, hpssBins)
	sIm := make([]float32, hpssBins)
	wave := make([]float32, hpssSize)

	for i := 0; i < count; i++ {
		off := i * hpssHop
		for b := 0; b < hpssBins; b++ {
			mag[b] = 0
		}
		for c := 0; c < s.channels; c++ {
			src := s.pending[c][off : off+hpssSize]
			for k := 0; k < hpssSize; k++ {
				winFrame[k] = src[k] * s.window[k]
			}
			s.fft.rfft(winFrame, zRe[c], zIm[c], scratch)
			for b := 0; b < hpssBins; b++ {
				re, im := float64(zRe[c][b]), float64(zIm[c][b])
				mag[b] += float32(math.Hypot(re, im))
			}
		}
		if s.channels == 2 {
			for b := 0; b < hpssBins; b++ {
				mag[b] *= 0.5
			}
		}
		histRow := make([]float32, hpssBins)
		copy(histRow, mag)
		s.history = append(s.history, histRow)
		if len(s.history) > hpssHistory {
			s.history = s.history[len(s.history)-hpssHistory:]
		}
		hlen := len(s.history)
		hbuf := medScratch[:hlen]
		for b := 0; b < hpssBins; b++ {
			for t := 0; t < hlen; t++ {
				hbuf[t] = s.history[t][b]
			}
			harmonic[b] = medianSlice(hbuf)
		}
		pbuf := medScratch[:31]
		for b := 0; b < hpssBins; b++ {
			for d := -15; d <= 15; d++ {
				idx := min(max(b+d, 0), hpssBins-1)
				pbuf[d+15] = mag[idx]
			}
			percussive[b] = medianSlice(pbuf)
		}

		var masks [4][hpssBins]float32
		for b := 0; b < hpssBins; b++ {
			hh := float64(harmonic[b]) * float64(harmonic[b])
			pp := float64(percussive[b]) * float64(percussive[b])
			pMask := float32(pp / (hh + pp + 1e-20))
			hMask := 1.0 - pMask
			bassMask := hMask * s.low[b]
			center := float32(1.0)
			if s.channels == 2 {
				mr := 0.5 * (zRe[0][b] + zRe[1][b])
				mi := 0.5 * (zIm[0][b] + zIm[1][b])
				sr := 0.5 * (zRe[0][b] - zRe[1][b])
				si := 0.5 * (zIm[0][b] - zIm[1][b])
				mid := math.Hypot(float64(mr), float64(mi))
				side := math.Hypot(float64(sr), float64(si))
				cval := (mid - side) / (mid + side + 1e-10)
				center = float32(min(max(cval, 0.0), 1.0))
			}
			vocalMask := hMask * (1.0 - s.low[b]) * s.vocalBand[b] * center * 0.8
			melodyMask := 1.0 - pMask - bassMask - vocalMask
			masks[0][b] = vocalMask
			masks[1][b] = bassMask
			masks[2][b] = pMask
			masks[3][b] = melodyMask
		}

		for st := 0; st < 4; st++ {
			for c := 0; c < s.channels; c++ {
				for b := 0; b < hpssBins; b++ {
					m := masks[st][b]
					sRe[b] = zRe[c][b] * m
					sIm[b] = zIm[c][b] * m
				}
				s.fft.irfft(sRe, sIm, wave, scratch)
				ola := s.ola[st][c]
				for k := 0; k < hpssSize; k++ {
					ola[k] += wave[k] * s.window[k]
				}
			}
		}
		for k := 0; k < hpssSize; k++ {
			s.norm[k] += s.window[k] * s.window[k]
		}
		take := 0
		if s.skip > 0 {
			take = min(s.skip, hpssHop)
			s.skip -= take
		}
		for st := 0; st < 4; st++ {
			for c := 0; c < s.channels; c++ {
				ola := s.ola[st][c]
				for k := take; k < hpssHop; k++ {
					denom := max(s.norm[k], float32(1e-12))
					out[st][c] = append(out[st][c], ola[k]/denom)
				}
				copy(ola[:hpssSize-hpssHop], ola[hpssHop:])
				clear(ola[hpssSize-hpssHop:])
			}
		}
		copy(s.norm[:hpssSize-hpssHop], s.norm[hpssHop:])
		clear(s.norm[hpssSize-hpssHop:])
	}
	for c := 0; c < s.channels; c++ {
		s.pending[c] = s.pending[c][count*hpssHop:]
	}
	return out
}
