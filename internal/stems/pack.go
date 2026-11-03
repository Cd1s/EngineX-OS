package stems

// Packed sessions (X-Stems-Framing: 2) cross the network as int16: Engine's
// blocks on the way up, and three of the four stems on the way down. The deck
// rebuilds the fourth, melody, as Engine's own float32 input minus the other
// three, so the four Engine receives still sum to its input exactly. Uploads
// carry half the bytes and downloads 3/8.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// packScale maps ±2.0 to int16: a stem may peak a little above its input.
const packScale = 16384

func packSamples(dst, src []byte) {
	for i := 0; i*4 < len(src); i++ {
		f := float64(math.Float32frombits(binary.LittleEndian.Uint32(src[i*4:])))
		q := math.Round(f * packScale)
		q = math.Max(math.MinInt16, math.Min(math.MaxInt16, q))
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(q)))
	}
}

func unpackSample(b []byte) float32 { return float32(int16(binary.LittleEndian.Uint16(b))) / packScale }

func unpackSamples(dst, src []byte) []byte {
	for i := 0; i*2 < len(src); i++ {
		dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(unpackSample(src[i*2:])))
	}
	return dst
}

// unpackWriter restores the deck's int16 blocks to float32 for the worker.
type unpackWriter struct {
	w        io.Writer
	s        stream
	channels int
	cur      []byte
	want     int
	err      error
}

func newUnpackWriter(w io.Writer) *unpackWriter {
	u := &unpackWriter{w: w}
	u.s.keep = true
	u.s.data = func(b []byte) {
		if u.cur = append(u.cur, b...); len(u.cur) == u.want && u.err == nil {
			_, u.err = u.w.Write(unpackSamples(make([]byte, 0, u.want*2), u.cur))
		}
	}
	return u
}

func (u *unpackWriter) Write(b []byte) (int, error) {
	e := u.s.feed(b, func(f map[string]string) (int64, error) {
		if _, err := u.w.Write(u.s.raw); err != nil {
			return 0, err
		}
		if u.channels == 0 {
			c, err := count(f, "numChannels", 8)
			if err != nil || c == 0 {
				return 0, errors.New("stems pack: bad init header")
			}
			u.channels = int(c)
			return 0, nil
		}
		if f["action"] != "block" {
			return 0, nil
		}
		n, err := count(f, "numSamples", 1<<20)
		if err != nil {
			return 0, err
		}
		u.want, u.cur = int(n)*u.channels*2, u.cur[:0]
		return int64(u.want), nil
	})
	return len(b), cmpErr(e, u.err)
}

// packWriter rewrites the worker's replies before they leave the renderer.
type packWriter struct {
	w      io.Writer
	s      stream
	planes int // numReturnChannels: four stems per input channel
	cur    []byte
	want   int
	err    error
}

func newPackWriter(w io.Writer) *packWriter {
	p := &packWriter{w: w}
	p.s.keep = true
	p.s.data = func(b []byte) {
		p.cur = append(p.cur, b...)
		if len(p.cur) == p.want && p.err == nil {
			// Stems are stem-major planes: the first 3/4 are vocals, bass, drums.
			out := make([]byte, p.want*3/8)
			packSamples(out, p.cur[:p.want*3/4])
			_, p.err = p.w.Write(out)
		}
	}
	return p
}

func (p *packWriter) Write(b []byte) (int, error) {
	e := p.s.feed(b, func(f map[string]string) (int64, error) {
		if v, ok := f["numReturnChannels"]; ok {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n%4 != 0 {
				return 0, fmt.Errorf("stems pack: bad numReturnChannels %q", v)
			}
			p.planes = n
		}
		if _, err := p.w.Write(p.s.raw); err != nil {
			return 0, err
		}
		v, ok := f["returnFrames"]
		if !ok {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || p.planes == 0 {
			return 0, fmt.Errorf("stems pack: bad returnFrames %q", v)
		}
		p.want, p.cur = n*p.planes*4, p.cur[:0]
		return int64(p.want), nil
	})
	return len(b), cmpErr(e, p.err)
}
