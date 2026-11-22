package enginedb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Four minutes at rekordbox's 150 detail entries/second, with the redundant
// PWV5/PWV3/overview and unknown sections found on the 21k-track export.
func realisticBridgeAnalysis() *rekordbox.Analysis {
	rng := rand.New(rand.NewSource(272))
	a := &rekordbox.Analysis{Unknown: map[string][][]byte{"ZZZZ": {make([]byte, 10<<10)}}}
	for _, w := range []struct {
		tag      string
		width, n int
	}{{"PWV7", 3, 36000}, {"PWV5", 2, 36000}, {"PWV3", 1, 36000}, {"PWV4", 6, 1200}} {
		data := make([]byte, w.width*w.n)
		for i := range data {
			data[i] = byte(rng.Intn(128))
		}
		a.Waveforms = append(a.Waveforms, rekordbox.Waveform{Tag: w.tag, EntryBytes: uint32(w.width), Data: data})
	}
	for i := 0; i < 500; i++ {
		a.Beats = append(a.Beats, rekordbox.Beat{Number: uint16(i%4 + 1), BPM: 125, TimeMS: uint32(i * 480)})
	}
	return a
}

func compactTestRow(t *testing.T) ([]byte, []byte, *rekordbox.Analysis, string) {
	t.Helper()
	root := t.TempDir()
	testfixture.Export(t, root)
	a := realisticBridgeAnalysis()
	var sections [][]byte
	for _, w := range a.Waveforms {
		payload := make([]byte, 12+len(w.Data))
		binary.BigEndian.PutUint32(payload, w.EntryBytes)
		binary.BigEndian.PutUint32(payload[4:], uint32(len(w.Data))/w.EntryBytes)
		copy(payload[12:], w.Data)
		sections = append(sections, testfixture.Section(w.Tag, 24, payload))
	}
	sections = append(sections, testfixture.Section("ZZZZ", 12, a.Unknown["ZZZZ"][0]))
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(sections...))
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2", stateFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var source, audio []byte
	if err = db.QueryRow(`SELECT sourceJSON,audioJSON FROM BridgeAnalysis WHERE trackId=1`).Scan(&source, &audio); err != nil {
		t.Fatal(err)
	}
	return source, audio, a, root
}

func TestIssue272CompactRowSize(t *testing.T) {
	source, audio, _, _ := compactTestRow(t)
	if size := len(source) + len(audio); size > 112<<10 {
		t.Fatalf("realistic row is %d bytes; maximum 112 KiB", size)
	}
}

func TestIssue272CompactLibraryFitsCap(t *testing.T) {
	source, audio, _, _ := compactTestRow(t)
	// Round up the BLOB row to SQLite pages and reserve another page per track
	// for BridgeState/path/index overhead, plus initial schema/index pages.
	perTrack := int64((len(source)+len(audio)+128+4095)/4096+1) * 4096
	projected := perTrack*21352 + (1 << 20)
	if projected >= defaultMaxBridgeStateBytes {
		t.Fatalf("21,352-track projection %d exceeds cap %d", projected, defaultMaxBridgeStateBytes)
	}
	t.Logf("row=%d bytes; SQLite projection=%d bytes for 21,352 tracks", len(source)+len(audio), projected)
}

// Since #253 the row is the source ID and the track's ANLZ files; the display consumer reads the
// same selected raw waveform from those files.
func TestIssue272CompactConsumerFields(t *testing.T) {
	packed, _, a, root := compactTestRow(t)
	plain, err := UnpackAnalysis(packed, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Track    rekordbox.Track
		Detail   []string
		Analysis *rekordbox.Analysis
	}
	if err = json.Unmarshal(plain, &got); err != nil {
		t.Fatal(err)
	}
	if got.Track.ID != 1 || got.Analysis != nil || len(got.Detail) != 2 {
		t.Fatalf("compact consumer schema: %+v", got)
	}
	detail := BridgeDetail(packed, root, 64<<20)
	wave := displayAnalysisWaveform(detail)
	if wave == nil || wave.Tag != "PWV7" || wave.EntryBytes != 3 || !bytes.Equal(wave.Data, a.Waveforms[0].Data) {
		t.Fatal("selected raw waveform changed")
	}
	expected := DisplayWaveFromRekordbox(a, 44100)
	actual := DisplayWaveFromRekordbox(detail, 44100)
	if actual == nil || actual.SamplesPerBin != expected.SamplesPerBin || !bytes.Equal(actual.Bands, expected.Bands) {
		t.Fatal("display consumer changed")
	}
	if got.Track.Title != "" || len(packed) > 128 {
		t.Fatalf("unused source archive retained: %d bytes", len(packed))
	}
}

func TestIssue272CompactPWV5FallbackAndEmpty(t *testing.T) {
	for _, tag := range []string{"PWV7", "PWV5", "none"} {
		t.Run(tag, func(t *testing.T) {
			a := realisticBridgeAnalysis()
			if tag == "PWV5" {
				a.Waveforms[0].EntryBytes = 1
			} // invalid PWV7 must not displace valid PWV5
			if tag == "none" {
				a = nil
			}
			packed, err := packBridgeSource(0xf1234567, a)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := UnpackAnalysis(packed, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Track    struct{ ID uint32 }
				Analysis *rekordbox.Analysis
			}
			if err = json.Unmarshal(plain, &got); err != nil {
				t.Fatal(err)
			}
			if got.Track.ID != 0xf1234567 {
				t.Fatal("source ID truncated")
			}
			if tag == "none" {
				if got.Analysis != nil {
					t.Fatal("invented waveform")
				}
				return
			}
			if got.Analysis == nil || len(got.Analysis.Waveforms) != 1 || got.Analysis.Waveforms[0].Tag != tag {
				t.Fatalf("fallback failed: %+v", got.Analysis)
			}
			want, actual := DisplayWaveFromRekordbox(a, 48000), DisplayWaveFromRekordbox(got.Analysis, 48000)
			if actual.SamplesPerBin != want.SamplesPerBin || !bytes.Equal(actual.Bands, want.Bands) {
				t.Fatal("fallback display changed")
			}
		})
	}
}

func TestIssue272CompactLegacyAndBounds(t *testing.T) {
	a := realisticBridgeAnalysis()
	legacy, err := json.Marshal(struct {
		Track    rekordbox.Track
		Analysis *rekordbox.Analysis
	}{rekordbox.Track{ID: 123, Title: "legacy", Path: "Contents/track.flac"}, a})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := packAnalysis(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][]byte{legacy, gz} {
		got, err := UnpackAnalysis(row, len(legacy))
		if err != nil || !bytes.Equal(got, legacy) {
			t.Fatalf("legacy changed: %v", err)
		}
		if id, err := analysisSourceID(row[:min(4096, len(row))]); err != nil || id != 123 {
			t.Fatalf("legacy identity prefix: %d %v", id, err)
		}
	}
	packed, err := packBridgeSource(123, a)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := UnpackAnalysis(packed, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := analysisSourceID(packed[:min(4096, len(packed))]); err != nil || id != 123 {
		t.Fatalf("compact identity prefix: %d %v", id, err)
	}
	if _, err := UnpackAnalysis(packed, len(plain)); err != nil {
		t.Fatal("exact unpack limit rejected", err)
	}
	if _, err := UnpackAnalysis(packed, len(plain)-1); err == nil {
		t.Fatal("reconstructed JSON exceeded limit")
	}
	if _, err := UnpackAnalysis(packed, 100); err == nil {
		t.Fatal("decompressed binary exceeded limit")
	}
	// Compact records must reject truncated/mismatched lengths and invalid widths.
	r, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{raw[:10], raw[:len(raw)-1], append(append([]byte(nil), raw...), 0)} {
		gz, err := packAnalysis(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UnpackAnalysis(gz, 64<<20); err == nil {
			t.Fatal("invalid compact length accepted")
		}
	}
	binary.LittleEndian.PutUint32(raw[12:], 0)
	gz, err = packAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnpackAnalysis(gz, 64<<20); err == nil {
		t.Fatal("invalid compact waveform width accepted")
	}
}

func BenchmarkBridgeAnalysisEncode(b *testing.B) {
	a := realisticBridgeAnalysis()
	source := struct {
		Track    rekordbox.Track
		Analysis *rekordbox.Analysis
	}{rekordbox.Track{ID: 123, Title: "Synthetic four-minute recording", Path: "Contents/recording.flac"}, a}
	for _, mode := range []string{"GzipJSONBaseline", "CompactRaw"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var packed []byte
				var err error
				if mode == "GzipJSONBaseline" {
					var plain []byte
					plain, err = json.Marshal(source)
					if err == nil {
						packed, err = packAnalysis(plain)
					}
				} else {
					packed, err = packBridgeSource(source.Track.ID, a)
				}
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(packed)), "bytes/track")
			}
		})
	}
}
