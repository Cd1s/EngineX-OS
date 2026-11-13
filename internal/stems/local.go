package stems

// LocalRender answers Engine's Stems child protocol on the deck itself, for a
// deck with no renderer set up. It is a coarse frequency split, not a
// separator: bass below 250 Hz, vocals the 250-4000 Hz band of the mid
// signal, drums above 4 kHz, and melody the exact remainder, so the four
// stems sum to the input. The filters are causal: each block is answered at
// once with the same number of frames, and nothing is left to drain.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// biquad is an RBJ cookbook second-order section, transposed direct form II.
type biquad struct{ b0, b1, b2, a1, a2, z1, z2 float64 }

func newBiquad(highPass bool, f0, rate float64) *biquad {
	w := 2 * math.Pi * f0 / rate
	alpha := math.Sin(w) / math.Sqrt2 // sin(w)/(2Q) with Q = 1/sqrt(2)
	cos := math.Cos(w)
	a0 := 1 + alpha
	b := (1 - cos) / 2
	b1 := 1 - cos
	if highPass {
		b, b1 = (1+cos)/2, -(1 + cos)
	}
	return &biquad{b0: b / a0, b1: b1 / a0, b2: b / a0, a1: -2 * cos / a0, a2: (1 - alpha) / a0}
}

func (f *biquad) step(x float64) float64 {
	y := f.b0*x + f.z1
	f.z1 = f.b1*x - f.a1*y + f.z2
	f.z2 = f.b2*x - f.a2*y
	return y
}

func readHeader(r *bufio.Reader) (map[string]string, error) {
	f, used := map[string]string{}, 0
	for {
		line, err := r.ReadString('\n')
		if used += len(line); used > maxHeader {
			return nil, errors.New("stems: header too large")
		}
		if err != nil {
			if err == io.EOF && len(f) == 0 && line == "" {
				return nil, io.EOF
			}
			return nil, errors.New("stems: truncated header")
		}
		if line = strings.TrimRight(line, "\r\n"); line == "" {
			if len(f) == 0 {
				continue
			}
			return f, nil
		}
		k, v, ok := strings.Cut(line, "=")
		if _, dup := f[k]; !ok || k == "" || dup {
			return nil, fmt.Errorf("stems: bad header line %q", line)
		}
		f[k] = v
	}
}

func LocalRender(in io.Reader, out io.Writer) error {
	r, w := bufio.NewReaderSize(in, 64<<10), bufio.NewWriterSize(out, 64<<10)
	send := func(s string) error {
		w.WriteString(s)
		return w.Flush()
	}
	if err := send("session=FFFFFFFFFFFFFFFF\n\n"); err != nil {
		return err
	}
	init, err := readHeader(r)
	if err == io.EOF {
		return nil // a presence check: greeting only
	}
	if err != nil {
		return err
	}
	ch, _ := strconv.Atoi(init["numChannels"])
	rate, _ := strconv.ParseFloat(init["sampleRate"], 64)
	maxBlock, _ := strconv.Atoi(init["maxBlockSize"])
	if ch < 1 || ch > 2 || !(rate >= 8000 && rate <= 192000) || maxBlock < 1 || maxBlock > 65536 {
		return fmt.Errorf("stems: unsupported init %v", init)
	}
	if err = send(fmt.Sprintf("preparing=ready\n\npreparing=ready\nnumReturnChannels=%d\n\n", 4*ch)); err != nil {
		return err
	}
	type chain struct{ low, high, vocalHigh, vocalLow *biquad }
	filters := make([]chain, ch)
	for c := range filters {
		filters[c] = chain{newBiquad(false, 250, rate), newBiquad(true, 4000, rate), newBiquad(true, 250, rate), newBiquad(false, 4000, rate)}
	}
	pcm, stems := make([]byte, maxBlock*ch*4), make([]byte, maxBlock*ch*16)
	for {
		f, err := readHeader(r)
		if err != nil {
			return err
		}
		switch f["action"] {
		case "done":
			return send("returnFrames=0\n\n")
		case "block":
		default:
			return fmt.Errorf("stems: unsupported action %q", f["action"])
		}
		n, err := strconv.Atoi(f["numSamples"])
		if err != nil || n < 0 || n > maxBlock {
			return fmt.Errorf("stems: bad numSamples %q", f["numSamples"])
		}
		x := pcm[:n*ch*4]
		if _, err = io.ReadFull(r, x); err != nil {
			return errors.New("stems: truncated PCM block")
		}
		sample := func(c, k int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(x[(c*n+k)*4:])) }
		put := func(stem, c, k int, v float32) {
			binary.LittleEndian.PutUint32(stems[((stem*ch+c)*n+k)*4:], math.Float32bits(v))
		}
		for k := 0; k < n; k++ {
			mid := 0.0
			for c := 0; c < ch; c++ {
				mid += float64(sample(c, k))
			}
			mid /= float64(ch)
			// mid is mono, so every channel's vocal chain yields the same value.
			vocals := float32(filters[0].vocalLow.step(filters[0].vocalHigh.step(mid)))
			for c := 0; c < ch; c++ {
				fl := filters[c]
				v := sample(c, k)
				bass := float32(fl.low.step(float64(v)))
				drums := float32(fl.high.step(float64(v)))
				put(0, c, k, vocals)
				put(1, c, k, bass)
				put(2, c, k, drums)
				put(3, c, k, v-vocals-bass-drums)
			}
		}
		w.WriteString("returnFrames=" + strconv.Itoa(n) + "\n\n")
		w.Write(stems[:n*ch*16])
		if err = w.Flush(); err != nil {
			return err
		}
	}
}
