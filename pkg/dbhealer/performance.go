package dbhealer

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const maxBlobSize = 32 << 20

// DecodeQCompress validates the Qt length prefix, zlib checksum, exact end of
// stream, and a fixed decompression budget (including malicious length headers).
func DecodeQCompress(blob []byte) ([]byte, error) {
	if len(blob) < 5 || len(blob) > maxBlobSize {
		return nil, fmt.Errorf("invalid qCompress size")
	}
	size := binary.BigEndian.Uint32(blob[:4])
	if size == 0 || size > maxBlobSize {
		return nil, fmt.Errorf("invalid qCompress length %d", size)
	}
	input := bytes.NewReader(blob[4:])
	z, err := zlib.NewReader(input)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(z, int64(size)+1))
	closeErr := z.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(raw) != int(size) || input.Len() != 0 {
		return nil, fmt.Errorf("qCompress length/trailing-data mismatch")
	}
	return raw, nil
}

func EncodeQCompress(raw []byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(len(raw)))
	z := zlib.NewWriter(&out)
	_, _ = z.Write(raw)
	_ = z.Close()
	return out.Bytes()
}
func beFloat(b []byte) float64       { return math.Float64frombits(binary.BigEndian.Uint64(b)) }
func leFloat(b []byte) float64       { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }
func finite(v float64) bool          { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func putBEFloat(b []byte, v float64) { binary.BigEndian.PutUint64(b, math.Float64bits(v)) }
func putLEFloat(b []byte, v float64) { binary.LittleEndian.PutUint64(b, math.Float64bits(v)) }

func validOverview(raw []byte) bool {
	return len(raw) == 3099 && binary.BigEndian.Uint64(raw[:8]) == 1024 && binary.BigEndian.Uint64(raw[8:16]) == 1024 && finite(beFloat(raw[16:24])) && beFloat(raw[16:24]) > 0
}
func placeholder(length float64) []byte {
	raw := make([]byte, 3099)
	binary.BigEndian.PutUint64(raw[:8], 1024)
	binary.BigEndian.PutUint64(raw[8:16], 1024)
	if !finite(length) || length <= 0 {
		length = 1024
	}
	putBEFloat(raw[16:24], length/1024)
	// A minimal three-channel placeholder, not a synthesized representation of the
	// audio. A separate queue marks it for actual device-side reanalysis.
	for i := 24; i < len(raw); i++ {
		raw[i] = 1
	}
	return EncodeQCompress(raw)
}

type marker struct{ sample, beat float64 }
type beatGrid struct {
	rate, length float64
	markers      []marker
}

func parseBeats(blob []byte) (beatGrid, error) {
	var grid beatGrid
	raw, err := DecodeQCompress(blob)
	if err != nil {
		return grid, err
	}
	if len(raw) < 33 {
		return grid, fmt.Errorf("truncated beatData")
	}
	grid.rate, grid.length = beFloat(raw[:8]), beFloat(raw[8:16])
	if !finite(grid.rate) || grid.rate < 8000 || grid.rate > 384000 || !finite(grid.length) || grid.length <= 0 || raw[16] > 1 {
		return grid, fmt.Errorf("invalid beatData header")
	}
	offset := 17
	var grids [2][]marker
	for g := 0; g < 2; g++ {
		if len(raw)-offset < 8 {
			return grid, fmt.Errorf("truncated grid count")
		}
		count := binary.BigEndian.Uint64(raw[offset : offset+8])
		offset += 8
		if count > uint64((len(raw)-offset)/24) {
			return grid, fmt.Errorf("truncated grid")
		}
		for i := uint64(0); i < count; i++ {
			m := marker{leFloat(raw[offset : offset+8]), float64(int64(binary.LittleEndian.Uint64(raw[offset+8 : offset+16])))}
			offset += 24
			if !finite(m.sample) || math.Abs(m.sample) > 1e12 || math.Abs(m.beat) > 1e9 {
				return grid, fmt.Errorf("invalid marker")
			}
			if len(grids[g]) > 0 {
				p := grids[g][len(grids[g])-1]
				if m.sample <= p.sample || m.beat <= p.beat {
					return grid, fmt.Errorf("unordered beatgrid")
				}
			}
			grids[g] = append(grids[g], m)
		}
	}
	// The adjusted grid takes precedence. Unknown trailing extension bytes remain
	// untouched; only the two documented grids are interpreted.
	grid.markers = grids[1]
	if len(grid.markers) < 2 {
		grid.markers = grids[0]
	}
	if len(grid.markers) < 2 {
		return grid, fmt.Errorf("insufficient beat markers")
	}
	return grid, nil
}
func (g beatGrid) sampleAt(beat float64) float64 {
	m := g.markers
	i := 0
	for i+2 < len(m) && beat > m[i+1].beat {
		i++
	}
	return m[i].sample + (beat-m[i].beat)*(m[i+1].sample-m[i].sample)/(m[i+1].beat-m[i].beat)
}
func (g beatGrid) beatAt(sample float64) float64 {
	m := g.markers
	i := 0
	for i+2 < len(m) && sample > m[i+1].sample {
		i++
	}
	return m[i].beat + (sample-m[i].sample)*(m[i+1].beat-m[i].beat)/(m[i+1].sample-m[i].sample)
}

// alignLoops changes only the two LE sample doubles in documented, set slots.
// The format has no explicit beat count: infer it only within a 1ms rounding
// window of a beat boundary, and lengths near a 1/128-beat grid.
// Deliberate free loops outside that window survive.
func alignLoops(blob []byte, g beatGrid) ([]byte, int, error) {
	if len(blob) == 0 {
		return blob, 0, nil
	}
	if len(blob) < 8 || blob[0] > 64 {
		return nil, 0, fmt.Errorf("invalid loops header")
	}
	for _, b := range blob[1:8] {
		if b != 0 {
			return nil, 0, fmt.Errorf("unknown loops version")
		}
	}
	out := append([]byte(nil), blob...)
	offset := 8
	changes := 0
	for i := 0; i < int(blob[0]); i++ {
		if offset >= len(blob) {
			return nil, 0, fmt.Errorf("truncated loop label")
		}
		labelLen := int(blob[offset])
		offset++
		if len(blob)-offset < labelLen+22 {
			return nil, 0, fmt.Errorf("truncated loop slot")
		}
		offset += labelLen
		start, end := leFloat(blob[offset:offset+8]), leFloat(blob[offset+8:offset+16])
		startSet, endSet := blob[offset+16], blob[offset+17]
		if startSet > 1 || endSet > 1 {
			return nil, 0, fmt.Errorf("unknown loop flags")
		}
		if startSet == 1 && endSet == 1 && finite(start) && finite(end) && start >= 0 && end > start && end <= g.length {
			startBeat := math.Round(g.beatAt(start))
			beats := math.Round((g.beatAt(end)-g.beatAt(start))*128) / 128
			anchoredStart := g.sampleAt(startBeat)
			anchoredEnd := g.sampleAt(startBeat + beats)
			// Bound the window to less than half a grid step to avoid ambiguity.
			spb := g.sampleAt(startBeat+1) - g.sampleAt(startBeat)
			tolerance := math.Min(g.rate*0.0011, spb/512)
			if beats > 0 && anchoredStart >= 0 && anchoredEnd <= g.length && math.Abs(anchoredStart-start) <= tolerance && math.Abs(anchoredEnd-end) <= tolerance && (math.Abs(anchoredStart-start) > 1e-7 || math.Abs(anchoredEnd-end) > 1e-7) {
				putLEFloat(out[offset:offset+8], anchoredStart)
				putLEFloat(out[offset+8:offset+16], anchoredEnd)
				changes++
			}
		}
		offset += 22
	}
	if offset != len(blob) {
		return nil, 0, fmt.Errorf("unknown loops tail")
	}
	return out, changes, nil
}

func overviewReason(blob []byte) string {
	return fmt.Sprintf("placeholder sha256=%x; rebuild waveform from audio", sha256.Sum256(blob))
}

func healPerformance(tx *sql.Tx, r *Report) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS GuardianReanalysis(trackId INTEGER NOT NULL,field TEXT NOT NULL,reason TEXT NOT NULL,PRIMARY KEY(trackId,field))`); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM GuardianReanalysis WHERE trackId NOT IN (SELECT trackId FROM PerformanceData)"); err != nil {
		return err
	}
	// Page by primary key to bound memory even for large libraries. Close each
	// cursor before issuing writes on the same SQLite connection.
	var last int64 = math.MinInt64
	for {
		rows, err := tx.Query("SELECT trackId,substr(trackData,1,33554433),substr(overviewWaveFormData,1,33554433),substr(beatData,1,33554433),substr(loops,1,33554433) FROM PerformanceData WHERE trackId>? ORDER BY trackId LIMIT 1", last)
		if err != nil {
			return err
		}
		type perf struct {
			id                           int64
			track, overview, beat, loops []byte
		}
		var page []perf
		for rows.Next() {
			var p perf
			if err = rows.Scan(&p.id, &p.track, &p.overview, &p.beat, &p.loops); err != nil {
				rows.Close()
				return err
			}
			page = append(page, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, p := range page {
			last = p.id
			mark := func(field string, e error) error {
				_, err := tx.Exec("INSERT INTO GuardianReanalysis VALUES(?,?,?) ON CONFLICT(trackId,field) DO UPDATE SET reason=excluded.reason", p.id, field, e.Error())
				return err
			}
			clear := func(field string) error {
				_, err := tx.Exec("DELETE FROM GuardianReanalysis WHERE trackId=? AND field=?", p.id, field)
				return err
			}
			var length float64
			raw, trackErr := DecodeQCompress(p.track)
			if trackErr == nil {
				if len(raw) != 44 || !finite(beFloat(raw[:8])) || beFloat(raw[:8]) < 8000 || beFloat(raw[:8]) > 384000 {
					trackErr = fmt.Errorf("unsupported trackData layout")
				} else {
					length = float64(binary.BigEndian.Uint64(raw[8:16]))
					if length <= 0 || length > 1e12 {
						trackErr = fmt.Errorf("invalid track sample length")
					}
				}
			}
			if trackErr != nil {
				if err = mark("trackData", trackErr); err != nil {
					return err
				}
			} else if err = clear("trackData"); err != nil {
				return err
			}
			grid, beatErr := parseBeats(p.beat)
			if beatErr != nil {
				if err = mark("beatData", beatErr); err != nil {
					return err
				}
			} else {
				if err = clear("beatData"); err != nil {
					return err
				}
				if length <= 0 {
					length = grid.length
				}
			}
			overview, e := DecodeQCompress(p.overview)
			if e != nil || !validOverview(overview) {
				replacement := placeholder(length)
				if _, err = tx.Exec("UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=?", replacement, p.id); err != nil {
					return err
				}
				if err = mark("overviewWaveFormData", fmt.Errorf("%s", overviewReason(replacement))); err != nil {
					return err
				}
				r.Waveforms++
			} else {
				// A new valid waveform written by Engine completes the request;
				// our own unchanged placeholder remains queued.
				if _, err = tx.Exec("DELETE FROM GuardianReanalysis WHERE trackId=? AND field='overviewWaveFormData' AND reason<>?", p.id, overviewReason(p.overview)); err != nil {
					return err
				}
			}
			if beatErr == nil {
				loops, n, e := alignLoops(p.loops, grid)
				if e != nil {
					if err = mark("loops", e); err != nil {
						return err
					}
				} else {
					if err = clear("loops"); err != nil {
						return err
					}
					if n > 0 {
						if _, err = tx.Exec("UPDATE PerformanceData SET loops=? WHERE trackId=?", loops, p.id); err != nil {
							return err
						}
						r.Loops += n
					}
				}
			}
		}
	}
	return nil
}
