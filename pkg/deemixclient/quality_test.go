package deemixclient

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The complete first page of an original libvorbis-encoded stereo test signal.
func oggIdentificationPage(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString("T2dnUwACAAAAAAAAAAB0abtWAAAAAAVZKaUBHgF2b3JiaXMAAAAAAoA+AAAAAAAAwNoAAAAAAACpAQ==")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func inspectOggBytes(t *testing.T, b []byte) (Quality, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio.bin")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return Inspect(path)
}

func TestInspectOggVorbis(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nominal int32
		label   string
		bitrate int
	}{
		{"320k", 320000, "[320k]", 320000},
		{"160k", 160000, "[160k]", 160000},
		{"unknown_zero", 0, "[OGG]", 0},
		{"unknown_negative", -1, "[OGG]", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := oggIdentificationPage(t)
			binary.LittleEndian.PutUint32(b[28+20:], uint32(tc.nominal))
			// Signed unspecified maximum/minimum must not wrap into huge rates.
			binary.LittleEndian.PutUint32(b[28+16:], ^uint32(0))
			binary.LittleEndian.PutUint32(b[28+24:], ^uint32(0))
			q, err := inspectOggBytes(t, b)
			want := Quality{Label: tc.label, Format: "ogg", SampleRate: 16000, Bitrate: tc.bitrate}
			if err != nil || q != want {
				t.Fatalf("got %+v, %v; want %+v", q, err, want)
			}
		})
	}
	t.Run("real_page", func(t *testing.T) {
		b := oggIdentificationPage(t)
		q, err := inspectOggBytes(t, b)
		nominal := int(binary.LittleEndian.Uint32(b[48:52]))
		if err != nil || q.Format != "ogg" || q.SampleRate != 16000 || q.Bitrate != nominal || q.Lossless {
			t.Fatalf("real page: %+v, %v", q, err)
		}
	})
	t.Run("segment_table_offset", func(t *testing.T) {
		b := oggIdentificationPage(t)
		b[26] = 2
		b = append(b[:28:28], append([]byte{3}, append(b[28:], 0, 0, 0)...)...)
		if q, err := inspectOggBytes(t, b); err != nil || q.Format != "ogg" {
			t.Fatalf("two segments: %+v, %v", q, err)
		}
	})
	for _, sr := range []uint32{8000, 192000} {
		t.Run(fmt.Sprintf("rate_%d", sr), func(t *testing.T) {
			b := oggIdentificationPage(t)
			binary.LittleEndian.PutUint32(b[40:], sr)
			if q, err := inspectOggBytes(t, b); err != nil || q.SampleRate != int(sr) {
				t.Fatalf("%+v, %v", q, err)
			}
		})
	}
}

func TestInspectOggRejectsMalformedHeaders(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func([]byte) []byte
	}{
		{"truncated_page", func(b []byte) []byte { return b[:26] }},
		{"truncated_segments", func(b []byte) []byte { return b[:27] }},
		{"truncated_packet", func(b []byte) []byte { return b[:len(b)-1] }},
		{"ogg_version", func(b []byte) []byte { b[4] = 1; return b }},
		{"not_first_page", func(b []byte) []byte { b[5] = 0; return b }},
		{"continued_packet", func(b []byte) []byte { b[5] = 3; return b }},
		{"empty_segments", func(b []byte) []byte { b[26] = 0; return b }},
		{"short_id", func(b []byte) []byte { b[27] = 29; return b }},
		{"continued_id", func(b []byte) []byte { b[27] = 255; return b }},
		{"other_codec", func(b []byte) []byte { copy(b[28:], "OpusHead"); return b }},
		{"wrong_packet_type", func(b []byte) []byte { b[28] = 3; return b }},
		{"vorbis_version", func(b []byte) []byte { b[35] = 1; return b }},
		{"zero_channels", func(b []byte) []byte { b[39] = 0; return b }},
		{"zero_rate", func(b []byte) []byte { clear(b[40:44]); return b }},
		{"low_rate", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:], 7999); return b }},
		{"high_rate", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:], 192001); return b }},
		{"signed_rate", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:], ^uint32(0)); return b }},
		{"missing_framing", func(b []byte) []byte { b[57] = 0; return b }},
		{"bad_blocks", func(b []byte) []byte { b[56] = 0xff; return b }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if q, err := inspectOggBytes(t, tc.corrupt(oggIdentificationPage(t))); err == nil {
				t.Fatalf("malformed header accepted: %+v", q)
			}
		})
	}
}

func TestQualityCommentOgg(t *testing.T) {
	for _, label := range []string{"[320k]", "[OGG]"} {
		q := Quality{Label: label, Format: "ogg", SampleRate: 44100}
		if got, want := q.Comment(), label+" OGG 44100 Hz"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
