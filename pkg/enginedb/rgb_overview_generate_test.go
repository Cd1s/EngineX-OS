package enginedb

import (
	"bytes"
	"context"
	"database/sql"
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
			removed, err := PruneRGBOverviews(context.Background(), dir, libPath, "")
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

func TestRGBVerifiedCacheAndCountMissing(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "m.db")
	db, err := sql.Open("sqlite", libPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE Information (uuid TEXT)`, `INSERT INTO Information VALUES ('lib-uuid')`,
		`CREATE TABLE Track (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT, isAnalyzed INTEGER)`,
		`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, overviewWaveFormData BLOB)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ov := rgbTestOverview(t, 17803)
	for id, p := range map[int64]string{1: "../A.mp3", 2: "../B.mp3"} {
		if _, err := db.Exec(`INSERT INTO Track(id, path, isAnalyzed) VALUES (?, ?, 1)`, id, p); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO PerformanceData VALUES (?, ?)`, id, ov); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ctx := context.Background()
	if missing, err := CountMissingRGBOverviews(ctx, dir); err != nil || missing != 2 {
		t.Fatalf("initial missing=%d err=%v, want 2", missing, err)
	}
	valid := rgbTestOverviewBins(t, 17803, 0x20)
	if err := WriteRGBOverviewAtomic(dir, "lib-uuid", 1, valid); err != nil {
		t.Fatal(err)
	}
	if err := WriteRGBOverviewAtomic(dir, "lib-uuid", 2, valid); err != nil {
		t.Fatal(err)
	}
	if missing, err := CountMissingRGBOverviews(ctx, dir); err != nil || missing != 0 {
		t.Fatalf("after write missing=%d err=%v, want 0", missing, err)
	}
	RecordVerifiedRGBOverviews(ctx, dir, libPath, []int64{1, 2})
	if _, err := os.Stat(filepath.Join(dir, rgbVerifiedName)); err != nil {
		t.Fatalf("verified cache not written: %v", err)
	}
	// Prune must keep both cached files, then immediately remove track 2 if it turns empty.
	if removed, err := PruneRGBOverviews(ctx, dir, libPath, ""); err != nil || removed != 0 {
		t.Fatalf("cached prune removed=%d err=%v, want 0", removed, err)
	}
	p2 := filepath.Join(dir, "OverviewData", "lib-uuid", "2.rgb")
	if err := os.WriteFile(p2, []byte{0, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	if missing, err := CountMissingRGBOverviews(ctx, dir); err != nil || missing != 1 {
		t.Fatalf("with 4-byte file missing=%d err=%v, want 1", missing, err)
	}
	if removed, err := PruneRGBOverviews(ctx, dir, libPath, ""); err != nil || removed != 1 {
		t.Fatalf("empty file prune removed=%d err=%v, want 1", removed, err)
	}
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Fatal("empty file survived cached prune")
	}
	// Verify MDBSize/MDBModTime fast path: even if m.db contents are corrupted in-place
	// with preserved size and mtime, pruneRGBVerifiedFast verifies directory metadata
	// without opening SQLite or reading PerformanceData pages.
	st, err := os.Stat(libPath)
	if err != nil {
		t.Fatal(err)
	}
	origMDB, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libPath, bytes.Repeat([]byte("X"), len(origMDB)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(libPath, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneRGBOverviews(ctx, dir, libPath, ""); err != nil || removed != 0 {
		t.Fatalf("fast stat-matched prune opened m.db or failed: removed=%d err=%v", removed, err)
	}
	if err := os.WriteFile(libPath, origMDB, 0o644); err != nil {
		t.Fatal(err)
	}
}
