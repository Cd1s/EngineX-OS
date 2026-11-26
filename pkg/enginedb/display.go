package enginedb

// Read-only, bounded decoders for Engine's display data. These functions never
// open a library, repair data, analyze audio, or synthesize a replacement grid.

import (
	"encoding/binary"
	"fmt"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
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
	Peaks      [3]float64    `json:"peaks"`
	Overview   DisplayWave   `json:"overview"`
	Waveform   *DisplayWave  `json:"waveform,omitempty"`
	Beats      []DisplayBeat `json:"beats"`
	Cues       []DisplayCue  `json:"cues"`
	Loops      []DisplayLoop `json:"loops"`
}

// DisplayWaveFromRekordbox preserves persisted 150 Hz PWV7/PWV5 detail. It
// never decodes the song and never mistakes a 1024-bin overview for detail.
func DisplayWaveFromRekordbox(a *rekordbox.Analysis, sampleRate float64) *DisplayWave {
	best := displayAnalysisWaveform(a)
	if best == nil {
		return nil
	}
	n := len(best.Data) / int(best.EntryBytes)
	w := &DisplayWave{Bands: make([]byte, n*3), SamplesPerBin: sampleRate / 150}
	for i := 0; i < n; i++ {
		v := WaveRGB(*best, i)
		copy(w.Bands[i*3:], v[:])
	}
	return w
}

// Storage and rendering must choose the same valid detail section.
func displayAnalysisWaveform(a *rekordbox.Analysis) *rekordbox.Waveform {
	if a == nil {
		return nil
	}
	var best *rekordbox.Waveform
	for i := range a.Waveforms {
		w := &a.Waveforms[i]
		if w.EntryBytes == 0 || len(w.Data)%int(w.EntryBytes) != 0 || !displayWaveFits(w.Tag, w.EntryBytes, uint64(len(w.Data)/int(w.EntryBytes))) {
			continue
		}
		if best == nil || w.Tag == "PWV7" && best.Tag != "PWV7" {
			best = w
		}
	}
	return best
}

// displayWaveFits is the waveform the display draws: a PWV7 of 3 bytes an entry or a PWV5 of 2,
// with 1 to 2<<20 entries. DetailServes decides with it from a section's header (#253 review 3).
func displayWaveFits(tag string, width uint32, count uint64) bool {
	return (tag == "PWV7" && width == 3 || tag == "PWV5" && width == 2) && count > 0 && count <= 2<<20
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
	for i := range out.Peaks {
		v := displayFloat(raw[20+8*i:], big)
		if displayFinite(v) && v > 0 && v <= 1 {
			out.Peaks[i] = v
		}
	}
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

// DeckHotCue holds one Engine Hot Cue pad (Slot 0..7 -> Rekordbox Hot Cue A..H = 1..8)
// in decoded samples along with its label and RGBA color.
type DeckHotCue struct {
	Slot    int
	Samples float64
	Name    string
	Color   [4]byte
}

// DeckLoop holds one Engine Saved Loop slot (Slot 0..7) in decoded samples.
type DeckLoop struct {
	Slot         int
	StartSamples float64
	EndSamples   float64
	Name         string
	Color        [4]byte
	Active       bool
}

// DeckCues is the exact slot-indexed cue/loop state parsed from a PerformanceData row.
type DeckCues struct {
	SampleRate  float64
	SampleCount float64
	HotCues     [8]*DeckHotCue
	Loops       [8]*DeckLoop
	MainCue     float64 // >= 0 when set, -1 when unset
}

// ParseDeckCuesAndLoops parses exact slot-indexed Hot Cues, Saved Loops, and active-loop
// bits from Engine PerformanceData blobs, preserving decoded sample positions and RGBA colors.
func ParseDeckCuesAndLoops(track, cues, loops []byte, activeOnLoadLoops int64) (DeckCues, error) {
	var out DeckCues
	out.MainCue = -1
	raw, err := QUncompress(track, 4096)
	if err != nil {
		return out, err
	}
	if len(raw) != 44 && len(raw) != 68 {
		return out, fmt.Errorf("display: unknown trackData length")
	}
	rate := displayFloat(raw, big)
	samples := big.Uint64(raw[8:])
	if !displayFinite(rate) || rate < 8000 || rate > 384000 || samples == 0 || samples > uint64(rate*86400) {
		return out, fmt.Errorf("display: invalid trackData")
	}
	out.SampleRate = rate
	out.SampleCount = float64(samples)

	if len(cues) != 0 {
		qraw, err := QUncompress(cues, 64<<10)
		if err != nil {
			return out, err
		}
		if len(qraw) < 8 || big.Uint64(qraw) > 8 {
			return out, fmt.Errorf("display: invalid cue count")
		}
		off := 8
		count := int(big.Uint64(qraw))
		for slot := 0; slot < count; slot++ {
			name, err := displayLabel(qraw, &off, 12)
			if err != nil {
				return out, err
			}
			p := displayFloat(qraw[off:], big)
			if !displayFinite(p) || p > out.SampleCount {
				return out, fmt.Errorf("display: invalid cue position")
			}
			// Treat any negative position (-1 unset, or negative pre-roll/sentinel values
			// such as -420.0) as unset so deck cue write-back never clamps them to sample 0
			// and fabricates phantom hot cues at 0 + RekordboxOffset().
			if p >= 0 {
				if p == 0 {
					p = 0 // normalize -0.0
				}
				col := [4]byte{qraw[off+8], qraw[off+9], qraw[off+10], qraw[off+11]}
				if col != ([4]byte{}) && col[0] == 0 {
					col[0] = 255
				}
				out.HotCues[slot] = &DeckHotCue{
					Slot:    slot,
					Samples: p,
					Name:    name,
					Color:   col,
				}
			}
			off += 12
		}
		if len(qraw)-off < 17 {
			return out, fmt.Errorf("display: missing main cue")
		}
		p := displayFloat(qraw[off:], big)
		if !displayFinite(p) || p > out.SampleCount {
			return out, fmt.Errorf("display: invalid main cue")
		}
		if p >= 0 {
			out.MainCue = p
		}
	}

	if len(loops) != 0 {
		if len(loops) < 8 || loops[0] > 8 {
			return out, fmt.Errorf("display: invalid loops")
		}
		for _, b := range loops[1:8] {
			if b != 0 {
				return out, fmt.Errorf("display: unknown loop version")
			}
		}
		off := 8
		for slot := 0; slot < int(loops[0]); slot++ {
			name, err := displayLabel(loops, &off, 22)
			if err != nil {
				return out, err
			}
			a, b := displayFloat(loops[off:], little), displayFloat(loops[off+8:], little)
			if loops[off+16] > 1 || loops[off+17] > 1 || !displayFinite(a) || !displayFinite(b) {
				return out, fmt.Errorf("display: invalid loop flags/position")
			}
			if loops[off+16] == 1 && loops[off+17] == 1 {
				if b <= a || b > out.SampleCount {
					return out, fmt.Errorf("display: invalid loop range")
				}
				if b > 0 {
					col := [4]byte{loops[off+18], loops[off+19], loops[off+20], loops[off+21]}
					if col != ([4]byte{}) && col[0] == 0 {
						col[0] = 255
					}
					out.Loops[slot] = &DeckLoop{
						Slot:         slot,
						StartSamples: math.Max(a, 0),
						EndSamples:   b,
						Name:         name,
						Color:        col,
						Active:       activeOnLoadLoops > 0 && (activeOnLoadLoops&(1<<uint(slot))) != 0,
					}
				}
			}
			off += 22
		}
	}
	return out, nil
}

// displayDecoderTrim is more than a decoder takes off a track: an M4A's priming is under
// 16384 samples (compat), an MP3's delay and padding under 4096 each beside one frame.
const displayDecoderTrim = 16384

func parseDisplayBeats(blob []byte, rate, samples float64) ([]DisplayBeat, error) {
	raw, err := QUncompress(blob, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(raw) < 33 || raw[16] > 1 || displayFloat(raw, big) != rate {
		return nil, fmt.Errorf("display: invalid beatData header")
	}
	// Stock beatData can store a duration truncated to whole seconds, while
	// trackData keeps the decoded sample count. Keep the saved grid unchanged.
	// Once Engine has loaded an imported track it writes its decoder's count into
	// trackData, without what the decoder trims (an M4A's priming, an MP3's delay and
	// padding), and leaves the import's in beatData: the grid's is that much more.
	gridSamples := displayFloat(raw[8:], big)
	if !displayFinite(gridSamples) || gridSamples < 0 ||
		(math.Abs(gridSamples-samples) > 1 && gridSamples != math.Floor(samples/rate)*rate &&
			!(gridSamples > samples && gridSamples-samples <= displayDecoderTrim)) {
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
		if !displayFinite(p) || p > samples {
			return nil, fmt.Errorf("display: invalid cue position")
		}
		// Exactly -1 is an unset pad. Stock Engine also stores Hot Cues before
		// sample zero (AAC/MP3 pre-roll, e.g. -1024): shown at the track start
		// instead of failing the whole track (BH-GC1-4).
		if p != -1 {
			out = append(out, DisplayCue{I: i, MS: math.Max(p, 0) * 1000 / rate, N: name, C: displayColor(raw[off+8:])})
		}
		off += 12
	}
	if len(raw)-off < 17 {
		return nil, fmt.Errorf("display: missing main cue")
	}
	p := displayFloat(raw[off:], big)
	// The stock main CUE can precede sample zero. Its pre-roll position has no
	// in-track marker, but must not discard the track's waveform and saved grid.
	if !displayFinite(p) || p > samples {
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
			if b <= a || b > samples {
				return nil, fmt.Errorf("display: invalid loop range")
			}
			// A stock Saved Loop may start in the pre-roll (BH-GC1-4); one
			// that ends there has nothing to show.
			if b > 0 {
				out = append(out, DisplayLoop{StartMS: math.Max(a, 0) * 1000 / rate, EndMS: b * 1000 / rate, N: name, C: displayColor(raw[off+18:])})
			}
		}
		off += 22
	}
	return out, nil
}
