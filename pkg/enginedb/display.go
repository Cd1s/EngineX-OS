package enginedb

// Read-only, bounded decoders for Engine's display data. These functions never
// open a library, repair data, analyze audio, or synthesize a replacement grid.

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"
)

type DisplayBeat struct {
	MS     float64 `json:"ms"`
	Number int64   `json:"number"` // Engine's zero-based beat index, not a bar number
}

type DisplayCue struct {
	I  int     `json:"i"`
	MS float64 `json:"ms"`
	N  string  `json:"n,omitempty"`
	C  string  `json:"c,omitempty"`
}

type DisplayLoop struct {
	StartMS float64 `json:"startMs"`
	EndMS   float64 `json:"endMs"`
	N       string  `json:"n,omitempty"`
	C       string  `json:"c,omitempty"`
}

type DisplayWave struct {
	Bands         []byte  `json:"bands"` // low/mid/high amplitude bytes, base64 on wire
	SamplesPerBin float64 `json:"samplesPerBin"`
}

type DisplayPerformance struct {
	SampleRate float64       `json:"sampleRate"`
	DurationMS float64       `json:"durationMs"`
	Overview   DisplayWave   `json:"overview"`
	Waveform   *DisplayWave  `json:"waveform,omitempty"`
	Beats      []DisplayBeat `json:"beats"`
	Cues       []DisplayCue  `json:"cues"`
	Loops      []DisplayLoop `json:"loops"`
}

func displayFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func displayFloat(b []byte, order binary.ByteOrder) float64 {
	return math.Float64frombits(order.Uint64(b))
}

// ParseDisplayWave accepts the measured qCompress waveform envelope. The final
// triple is a sentinel, not another bin. Unknown formats fail closed.
func ParseDisplayWave(blob []byte) (DisplayWave, error) {
	var out DisplayWave
	raw, err := QUncompress(blob, 8<<20)
	if err != nil {
		return out, err
	}
	if len(raw) < 27 {
		return out, fmt.Errorf("display: truncated waveform")
	}
	logical, n := big.Uint64(raw), big.Uint64(raw[8:])
	step := displayFloat(raw[16:], big)
	if n == 0 || n > 2<<20 || logical < n || !displayFinite(step) || step <= 0 || n*3+27 != uint64(len(raw)) {
		return out, fmt.Errorf("display: invalid waveform envelope")
	}
	out.Bands = append([]byte(nil), raw[24:24+int(n)*3]...)
	out.SamplesPerBin = step
	return out, nil
}

// ParseDisplayPerformance decodes the saved source grid, including sparse
// marker grids. Only the adjusted grid is used when it contains two markers.
func ParseDisplayPerformance(track, overview, beats, cues, loops []byte) (DisplayPerformance, error) {
	out := DisplayPerformance{Beats: []DisplayBeat{}, Cues: []DisplayCue{}, Loops: []DisplayLoop{}}
	raw, err := QUncompress(track, 4096)
	if err != nil {
		return out, err
	}
	if len(raw) != 44 && len(raw) != 68 {
		return out, fmt.Errorf("display: unknown trackData length")
	}
	out.SampleRate = displayFloat(raw, big)
	samples := big.Uint64(raw[8:])
	if !displayFinite(out.SampleRate) || out.SampleRate < 8000 || out.SampleRate > 384000 || samples == 0 || samples > uint64(out.SampleRate*86400) {
		return out, fmt.Errorf("display: invalid trackData")
	}
	out.DurationMS = float64(samples) * 1000 / out.SampleRate
	if out.Overview, err = ParseDisplayWave(overview); err != nil {
		return out, err
	}
	if len(beats) != 0 {
		if out.Beats, err = parseDisplayBeats(beats, out.SampleRate, float64(samples)); err != nil {
			return out, err
		}
	}
	if len(cues) != 0 {
		if out.Cues, err = parseDisplayCues(cues, out.SampleRate, float64(samples)); err != nil {
			return out, err
		}
	}
	if len(loops) != 0 {
		if out.Loops, err = parseDisplayLoops(loops, out.SampleRate, float64(samples)); err != nil {
			return out, err
		}
	}
	return out, nil
}

func parseDisplayBeats(blob []byte, rate, samples float64) ([]DisplayBeat, error) {
	raw, err := QUncompress(blob, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(raw) < 33 || raw[16] > 1 || displayFloat(raw, big) != rate || math.Abs(displayFloat(raw[8:], big)-samples) > 1 {
		return nil, fmt.Errorf("display: invalid beatData header")
	}
	type marker struct {
		p float64
		n int64
	}
	var grids [2][]marker
	off := 17
	for g := range grids {
		if len(raw)-off < 8 {
			return nil, fmt.Errorf("display: truncated grid")
		}
		n := big.Uint64(raw[off:])
		off += 8
		if n > 65536 || n > uint64((len(raw)-off)/24) {
			return nil, fmt.Errorf("display: invalid grid count")
		}
		for range n {
			m := marker{displayFloat(raw[off:], little), int64(little.Uint64(raw[off+8:]))}
			off += 24
			if !displayFinite(m.p) || math.Abs(m.p) > 1e12 || m.n < -1000000 || m.n > 1000000 {
				return nil, fmt.Errorf("display: invalid grid marker")
			}
			if a := grids[g]; len(a) != 0 && (a[len(a)-1].p >= m.p || a[len(a)-1].n >= m.n) {
				return nil, fmt.Errorf("display: unordered grid")
			}
			grids[g] = append(grids[g], m)
		}
	}
	m := grids[1]
	if len(m) < 2 {
		m = grids[0]
	}
	out := []DisplayBeat{}
	if raw[16] == 0 || len(m) < 2 {
		return out, nil
	}
	if m[len(m)-1].n-m[0].n > 65536 {
		return nil, fmt.Errorf("display: excessive expanded grid")
	}
	for i := 0; i+1 < len(m); i++ {
		for n := m[i].n; n < m[i+1].n; n++ {
			p := m[i].p + float64(n-m[i].n)*(m[i+1].p-m[i].p)/float64(m[i+1].n-m[i].n)
			if p >= 0 && p <= samples {
				out = append(out, DisplayBeat{MS: p * 1000 / rate, Number: n})
			}
		}
	}
	last := m[len(m)-1]
	if last.p >= 0 && last.p <= samples {
		out = append(out, DisplayBeat{MS: last.p * 1000 / rate, Number: last.n})
	}
	return out, nil
}

func displayLabel(raw []byte, off *int, tail int) (string, error) {
	if *off >= len(raw) {
		return "", fmt.Errorf("display: missing label")
	}
	n := int(raw[*off])
	*off++
	if len(raw)-*off < n+tail || !utf8.Valid(raw[*off:*off+n]) {
		return "", fmt.Errorf("display: invalid label")
	}
	s := string(raw[*off : *off+n])
	*off += n
	return s, nil
}

func displayColor(b []byte) string { return fmt.Sprintf("#%02x%02x%02x", b[1], b[2], b[3]) }

func parseDisplayCues(blob []byte, rate, samples float64) ([]DisplayCue, error) {
	raw, err := QUncompress(blob, 64<<10)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 || big.Uint64(raw) > 8 {
		return nil, fmt.Errorf("display: invalid cue count")
	}
	off, out := 8, []DisplayCue{}
	for i := 1; i <= int(big.Uint64(raw)); i++ {
		name, err := displayLabel(raw, &off, 12)
		if err != nil {
			return nil, err
		}
		p := displayFloat(raw[off:], big)
		if !displayFinite(p) || p < -1 || p > samples {
			return nil, fmt.Errorf("display: invalid cue position")
		}
		if p >= 0 {
			out = append(out, DisplayCue{I: i, MS: p * 1000 / rate, N: name, C: displayColor(raw[off+8:])})
		}
		off += 12
	}
	if len(raw)-off < 17 {
		return nil, fmt.Errorf("display: missing main cue")
	}
	p := displayFloat(raw[off:], big)
	if !displayFinite(p) || p < -1 || p > samples {
		return nil, fmt.Errorf("display: invalid main cue")
	}
	if p >= 0 {
		out = append(out, DisplayCue{I: 0, MS: p * 1000 / rate, N: "CUE", C: "#ffffff"})
	}
	return out, nil
}

func parseDisplayLoops(raw []byte, rate, samples float64) ([]DisplayLoop, error) {
	// Engine's loops column is not qCompress (unlike quickCues).
	if len(raw) < 8 || raw[0] > 8 {
		return nil, fmt.Errorf("display: invalid loops")
	}
	for _, b := range raw[1:8] {
		if b != 0 {
			return nil, fmt.Errorf("display: unknown loop version")
		}
	}
	off, out := 8, []DisplayLoop{}
	for range raw[0] {
		name, err := displayLabel(raw, &off, 22)
		if err != nil {
			return nil, err
		}
		a, b := displayFloat(raw[off:], little), displayFloat(raw[off+8:], little)
		if raw[off+16] > 1 || raw[off+17] > 1 || !displayFinite(a) || !displayFinite(b) {
			return nil, fmt.Errorf("display: invalid loop flags/position")
		}
		if raw[off+16] == 1 && raw[off+17] == 1 {
			if a < 0 || b <= a || b > samples {
				return nil, fmt.Errorf("display: invalid loop range")
			}
			out = append(out, DisplayLoop{StartMS: a * 1000 / rate, EndMS: b * 1000 / rate, N: name, C: displayColor(raw[off+18:])})
		}
		off += 22
	}
	return out, nil
}
