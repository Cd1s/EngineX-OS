package rekordbox

import (
	"fmt"
	"math"
	"testing"
)

func newTestRecord(m map[string]string) sqlRecord {
	r := sqlRecord{}
	for k, v := range m {
		r[normalized(k)] = v
	}
	return r
}

func TestCueOverflow252(t *testing.T) {
	t.Run("inUsec Overflow and Range", func(t *testing.T) {
		tests := []struct {
			name   string
			inUsec int64
			wantOK bool
			wantMS uint32
		}{
			{
				name:   "MaxInt64",
				inUsec: math.MaxInt64,
				wantOK: false,
			},
			{
				name:   "MaxInt64-499",
				inUsec: math.MaxInt64 - 499,
				wantOK: false,
			},
			{
				name:   "MaxInt64-500",
				inUsec: math.MaxInt64 - 500,
				wantOK: false,
			},
			{
				name:   "MaxValidUint32MS",
				inUsec: 4294967295499, // (4294967295499 + 500) / 1000 = 4294967295 (MaxUint32)
				wantOK: true,
				wantMS: math.MaxUint32,
			},
			{
				name:   "OneAboveMaxValidUint32MS",
				inUsec: 4294967295500, // (4294967295500 + 500) / 1000 = 4294967296 (> MaxUint32)
				wantOK: false,
			},
			{
				name:   "RoundingDownBoundary_x499",
				inUsec: 1499, // (1499 + 500) / 1000 = 1
				wantOK: true,
				wantMS: 1,
			},
			{
				name:   "RoundingUpBoundary_x500",
				inUsec: 1500, // (1500 + 500) / 1000 = 2
				wantOK: true,
				wantMS: 2,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				rec := newTestRecord(map[string]string{
					"kind":   "1",
					"inUsec": fmt.Sprintf("%d", tc.inUsec),
				})
				cue, ok := parseSQLiteCue(rec)
				if ok != tc.wantOK {
					t.Fatalf("parseSQLiteCue(inUsec=%d) ok = %v, want %v", tc.inUsec, ok, tc.wantOK)
				}
				if ok && cue.TimeMS != tc.wantMS {
					t.Fatalf("parseSQLiteCue(inUsec=%d) TimeMS = %d, want %d", tc.inUsec, cue.TimeMS, tc.wantMS)
				}
			})
		}
	})

	t.Run("outUsec Overflow and Range", func(t *testing.T) {
		tests := []struct {
			name     string
			outUsec  int64
			wantLoop bool
			wantEnd  uint32
		}{
			{
				name:     "MaxInt64",
				outUsec:  math.MaxInt64,
				wantLoop: false,
			},
			{
				name:     "MaxInt64-499",
				outUsec:  math.MaxInt64 - 499,
				wantLoop: false,
			},
			{
				name:     "MaxInt64-500",
				outUsec:  math.MaxInt64 - 500,
				wantLoop: false,
			},
			{
				name:     "MaxValidUint32EndMS",
				outUsec:  4294967295499,
				wantLoop: true,
				wantEnd:  math.MaxUint32,
			},
			{
				name:     "OneAboveMaxValidUint32EndMS",
				outUsec:  4294967295500,
				wantLoop: false,
			},
			{
				name:     "RoundingDownBoundary_x499",
				outUsec:  3499, // (3499 + 500) / 1000 = 3
				wantLoop: true,
				wantEnd:  3,
			},
			{
				name:     "RoundingUpBoundary_x500",
				outUsec:  3500, // (3500 + 500) / 1000 = 4
				wantLoop: true,
				wantEnd:  4,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				rec := newTestRecord(map[string]string{
					"kind":    "1",
					"inUsec":  "1000", // 1 ms
					"outUsec": fmt.Sprintf("%d", tc.outUsec),
				})
				cue, ok := parseSQLiteCue(rec)
				if !ok {
					t.Fatalf("parseSQLiteCue(outUsec=%d) unexpectedly rejected cue", tc.outUsec)
				}
				if cue.Loop != tc.wantLoop {
					t.Fatalf("parseSQLiteCue(outUsec=%d) Loop = %v, want %v", tc.outUsec, cue.Loop, tc.wantLoop)
				}
				if cue.Loop && cue.EndMS != tc.wantEnd {
					t.Fatalf("parseSQLiteCue(outUsec=%d) EndMS = %d, want %d", tc.outUsec, cue.EndMS, tc.wantEnd)
				}
			})
		}
	})

	t.Run("Sentinels", func(t *testing.T) {
		sentinels := []int64{-1, 0x7fffffff, 0xffffffff}
		for _, s := range sentinels {
			t.Run(fmt.Sprintf("inUsec_sentinel_%d", s), func(t *testing.T) {
				rec := newTestRecord(map[string]string{
					"kind":   "1",
					"inUsec": fmt.Sprintf("%d", s),
				})
				_, ok := parseSQLiteCue(rec)
				if ok {
					t.Fatalf("parseSQLiteCue with inUsec sentinel %d was accepted, want rejected", s)
				}
			})

			t.Run(fmt.Sprintf("outUsec_sentinel_%d", s), func(t *testing.T) {
				rec := newTestRecord(map[string]string{
					"kind":    "1",
					"inUsec":  "1000",
					"outUsec": fmt.Sprintf("%d", s),
				})
				cue, ok := parseSQLiteCue(rec)
				if !ok {
					t.Fatalf("parseSQLiteCue with outUsec sentinel %d was rejected, want kept as normal cue", s)
				}
				if cue.Loop {
					t.Fatalf("parseSQLiteCue with outUsec sentinel %d treated as loop", s)
				}
			})
		}
	})

	t.Run("NormalCueAndLoop", func(t *testing.T) {
		// Normal cue
		recCue := newTestRecord(map[string]string{
			"kind":            "1",
			"inUsec":          "1000000",
			"colorTableIndex": "1",
			"cueComment":      "Verse Start",
		})
		cue, ok := parseSQLiteCue(recCue)
		if !ok {
			t.Fatalf("normal cue rejected")
		}
		if cue.HotCue != 1 || cue.TimeMS != 1000 || cue.Loop || cue.Name != "Verse Start" {
			t.Fatalf("unexpected normal cue: %+v", cue)
		}
		if cue.ColorCode != 1 || cue.Color[0] != 255 {
			t.Fatalf("unexpected palette color on normal cue: %+v", cue)
		}

		// Normal loop
		recLoop := newTestRecord(map[string]string{
			"kind":            "2",
			"inUsec":          "1000000",
			"outUsec":         "2000000",
			"isActiveLoop":    "1",
			"colorTableIndex": "2",
			"cueComment":      "Chorus Loop",
		})
		loopCue, ok := parseSQLiteCue(recLoop)
		if !ok {
			t.Fatalf("normal loop rejected")
		}
		if loopCue.HotCue != 2 || loopCue.TimeMS != 1000 || !loopCue.Loop || loopCue.EndMS != 2000 || !loopCue.Active || loopCue.Name != "Chorus Loop" {
			t.Fatalf("unexpected normal loop: %+v", loopCue)
		}
		if loopCue.ColorCode != 2 || loopCue.Color[0] != 255 {
			t.Fatalf("unexpected palette color on normal loop: %+v", loopCue)
		}
	})
}
