package enginedb

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEncodeRGBOverviewPWV6AndPWV4FormatAndPruneSafe(t *testing.T) {
	track := rekordbox.Track{
		ID:          1,
		SampleRate:  44100,
		SampleCount: 44100 * 240,
		Duration:    240,
	}

	pwv7 := make([]byte, 4096*3)
	for i := 0; i < 4096; i++ {
		pwv7[i*3] = byte(10 + i%100)
		pwv7[i*3+1] = byte(20 + (i*3)%90)
		pwv7[i*3+2] = byte(30 + (i*5)%80)
	}
	pwv6 := make([]byte, 1200*3)
	for i := 0; i < 1200; i++ {
		pwv6[i*3] = byte(5 + i%50)
		pwv6[i*3+1] = byte(15 + (i*2)%45)
		pwv6[i*3+2] = byte(25 + (i*4)%55)
	}
	pwv4 := make([]byte, 1200*6)
	for i := 0; i < 1200; i++ {
		pwv4[i*6+3] = byte(40 + i%60)
		pwv4[i*6+4] = byte(10 + (i*3)%70)
		pwv4[i*6+5] = byte(20 + (i*5)%80)
	}

	cases := []struct {
		name      string
		waveforms []rekordbox.Waveform
	}{
		{
			name: "PWV6_over_PWV7",
			waveforms: []rekordbox.Waveform{
				{Tag: "PWV7", EntryBytes: 3, Data: pwv7},
				{Tag: "PWV6", EntryBytes: 3, Data: pwv6},
				{Tag: "PWV4", EntryBytes: 6, Data: pwv4},
			},
		},
		{
			name: "PWV4_fallback_without_PWV6",
			waveforms: []rekordbox.Waveform{
				{Tag: "PWV7", EntryBytes: 3, Data: pwv7},
				{Tag: "PWV4", EntryBytes: 6, Data: pwv4},
			},
		},
		{
			name: "PWV4_only_distinct_from_stored_overview",
			waveforms: []rekordbox.Waveform{
				{Tag: "PWV4", EntryBytes: 6, Data: pwv4},
			},
		},
		{
			name: "monochrome_PWAV_only",
			waveforms: []rekordbox.Waveform{
				{Tag: "PWAV", EntryBytes: 1, Data: bytes.Repeat([]byte{0x18, 0x0c, 0x1f}, 200)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &rekordbox.Analysis{
				Beats:     []rekordbox.Beat{{Number: 1, BPM: 128, TimeMS: 100}},
				Waveforms: tc.waveforms,
			}
			perf, err := EncodePerformance(track, a)
			if err != nil {
				t.Fatalf("EncodePerformance: %v", err)
			}
			rgbBlob, err := EncodeRGBOverview(track, a, perf.Overview)
			if err != nil {
				t.Fatalf("EncodeRGBOverview: %v", err)
			}
			raw, err := QUncompress(rgbBlob, 1<<20)
			if err != nil {
				t.Fatalf("QUncompress: %v", err)
			}
			if len(raw) != 3099 || !ValidRGBOverview(raw) {
				t.Fatalf("invalid raw RGB overview len=%d valid=%v", len(raw), ValidRGBOverview(raw))
			}
			n1 := binary.BigEndian.Uint64(raw[0:8])
			n2 := binary.BigEndian.Uint64(raw[8:16])
			spb := math.Float64frombits(binary.BigEndian.Uint64(raw[16:24]))
			wantSPB := float64(track.SampleCount) / 1024
			if n1 != 1024 || n2 != 1024 || math.Abs(spb-wantSPB) > 1e-6 {
				t.Fatalf("header n1=%d n2=%d spb=%f want %f", n1, n2, spb, wantSPB)
			}
			// Verify trailer matches per-channel maxima.
			var maxes [3]byte
			for i := 0; i < 1024; i++ {
				for c := 0; c < 3; c++ {
					maxes[c] = max(maxes[c], raw[24+i*3+c])
				}
			}
			if raw[3096] != maxes[0] || raw[3097] != maxes[1] || raw[3098] != maxes[2] {
				t.Fatalf("trailer=%v want maxes=%v", raw[3096:3099], maxes)
			}

			// Verify PruneRGBOverviews keeps this generated file.
			dir := t.TempDir()
			libPath := filepath.Join(dir, "m.db")
			rgbTestLibrary(t, libPath, map[int64]string{1: "../Track.mp3"}, map[int64][]byte{1: perf.Overview})
			if err := WriteRGBOverviewAtomic(dir, "lib-uuid", 1, rgbBlob); err != nil {
				t.Fatalf("WriteRGBOverviewAtomic: %v", err)
			}
			removed, err := PruneRGBOverviews(context.Background(), dir, libPath)
			if err != nil || removed != 0 {
				t.Fatalf("PruneRGBOverviews removed=%d err=%v, want 0", removed, err)
			}
			rgbPath := filepath.Join(dir, "OverviewData", "lib-uuid", "1.rgb")
			if st, err := os.Stat(rgbPath); err != nil || st.Size() <= 8 {
				t.Fatalf("generated RGB overview missing or empty: st=%v err=%v", st, err)
			}
		})
	}
}
