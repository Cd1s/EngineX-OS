package analyzer

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/jfreymuth/oggvorbis"
)

// DecodeOggVorbis decodes Vorbis with no native libraries, averaging channels
// into mono PCM. The output grows only as frames arrive, up to MaxDecodedSamples.
func DecodeOggVorbis(r io.Reader) (*Audio, error) {
	return decodeOggVorbis(r, nil, MaxDecodedSamples)
}

func decodeOggVorbis(r io.Reader, ticker Ticker, maxSamples int) (audio *Audio, err error) {
	// The upstream decoder can panic on malformed page tables or granule positions.
	// Keep damaged downloads from crashing the analysis worker.
	defer func() {
		if recover() != nil {
			audio, err = nil, errors.New("analyzer: ogg: malformed Vorbis stream")
		}
	}()
	// Hide io.Seeker so initialization never scans the whole file for its length.
	dec, err := oggvorbis.NewReader(tickReader{r, ticker})
	if err != nil {
		return nil, fmt.Errorf("analyzer: ogg: %w", err)
	}
	sr, ch := dec.SampleRate(), dec.Channels()
	if sr <= 0 || ch <= 0 {
		return nil, errors.New("analyzer: ogg: invalid sample format")
	}
	a := &Audio{SampleRate: sr, Channels: ch, BitsPerSample: 16, Format: "ogg"}
	buf := make([]float32, 4096*ch)
	for {
		if ticker != nil {
			if err := ticker.Tick(); err != nil {
				return nil, err
			}
		}
		n, err := dec.Read(buf)
		if n/ch > maxSamples-len(a.Samples) {
			return nil, errors.New("analyzer: PCM memory limit exceeded")
		}
		if needed := len(a.Samples) + n/ch; needed > cap(a.Samples) {
			// Cap the backing array too; append's geometric growth can overshoot.
			grown := make([]float32, len(a.Samples), min(maxSamples, max(needed, 2*cap(a.Samples))))
			copy(grown, a.Samples)
			a.Samples = grown
		}
		for i := 0; i < n; i += ch {
			var sum float64
			for _, v := range buf[i : i+ch] {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					return nil, errors.New("analyzer: ogg: non-finite sample")
				}
				sum += float64(v)
			}
			mono := float32(sum / float64(ch))
			a.Samples = append(a.Samples, max(-1, min(1, mono)))
		}
		if err == io.EOF {
			// With a non-seekable source Length is set only at the EOS packet.
			if dec.Length() <= 0 {
				return nil, errors.New("analyzer: ogg: missing end of stream")
			}
			if len(a.Samples) == 0 {
				return nil, errors.New("analyzer: ogg: no audio samples")
			}
			return a, nil
		}
		if err != nil {
			return nil, fmt.Errorf("analyzer: ogg: %w", err)
		}
	}
}
