package rekordbox

import "strconv"

type sqlRecord map[string]string

func (r sqlRecord) get(names ...string) string {
	for _, n := range names {
		if v := r[normalized(n)]; v != "" {
			return v
		}
	}
	return ""
}

func (r sqlRecord) num(names ...string) uint32 {
	v, _ := strconv.ParseUint(r.get(names...), 10, 32)
	return uint32(v)
}

func (r sqlRecord) sint(names ...string) (int64, bool) {
	s := r.get(names...)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (r sqlRecord) decimal(names ...string) float64 {
	v, _ := strconv.ParseFloat(r.get(names...), 64)
	return v
}

func parseSQLiteBPM(r sqlRecordLike) float64 {
	if rawStr := r.get("bpmx100"); rawStr != "" {
		if iv, err := strconv.ParseInt(rawStr, 10, 64); err == nil {
			if iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff {
				return 0
			}
			return float64(iv) / 100
		}
		fv := r.decimal("bpmx100")
		if fv <= 0 || fv == 0x7fffffff || fv == 0xffffffff {
			return 0
		}
		return fv / 100
	}
	if iv, ok := r.sint("bpm", "tempo"); ok && (iv <= 0 || iv == 0x7fffffff || iv == 0xffffffff) {
		return 0
	}
	bpm := r.decimal("bpm", "tempo")
	if bpm > 1000 {
		bpm /= 100
	}
	return bpm
}
