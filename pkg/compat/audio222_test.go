package compat_test

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
)

func makeID3Tag(version, flags byte, payload []byte) []byte {
	n := len(payload)
	h := []byte{
		'I', 'D', '3', version, 0, flags,
		byte((n >> 21) & 0x7f), byte((n >> 14) & 0x7f), byte((n >> 7) & 0x7f), byte(n & 0x7f),
	}
	out := append(h, payload...)
	if flags&16 != 0 {
		out = append(out, append([]byte("3DI"), h[3:]...)...)
	}
	return out
}

func makeAIFFFixture() []byte {
	aiff := make([]byte, 12+26+16+96000*6)
	copy(aiff[0:4], "FORM")
	binary.BigEndian.PutUint32(aiff[4:8], uint32(len(aiff)-8))
	copy(aiff[8:12], "AIFF")
	copy(aiff[12:16], "COMM")
	binary.BigEndian.PutUint32(aiff[16:20], 18)
	binary.BigEndian.PutUint16(aiff[20:22], 2)     // channels
	binary.BigEndian.PutUint32(aiff[22:26], 96000) // frames
	binary.BigEndian.PutUint16(aiff[26:28], 24)    // bit depth
	binary.BigEndian.PutUint16(aiff[28:30], 0x400e)
	binary.BigEndian.PutUint64(aiff[30:38], 0xbb80000000000000)
	copy(aiff[38:42], "SSND")
	binary.BigEndian.PutUint32(aiff[42:46], uint32(8+96000*6))
	return aiff
}

func TestID3PrefixedFLACWAVAIFF(t *testing.T) {
	tag1 := makeID3Tag(3, 0, []byte("TIT2\x00\x00\x00\x07\x00\x00\x00Track1"))
	tag2 := makeID3Tag(4, 16, []byte("TPE1\x00\x00\x00\x08\x00\x00\x00Artist1"))
	pad := make([]byte, 512)

	prefix := append(append(append([]byte{}, tag1...), pad...), tag2...)

	flacBytes := append(prefix, testfixture.FLAC()...)
	wavPCMBytes := append(prefix, testfixture.WAV(false)...)
	wavExtBytes := append(prefix, testfixture.WAV(true)...)
	aiffBytes := append(prefix, makeAIFFFixture()...)

	cases := []struct {
		name   string
		data   []byte
		format string
	}{
		{"tagged.flac", flacBytes, "flac"},
		{"tagged_pcm.wav", wavPCMBytes, "wav"},
		{"tagged_ext.wav", wavExtBytes, "wav"},
		{"tagged.aiff", aiffBytes, "aiff"},
	}

	root := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.WriteFile(path, tc.data, 0644); err != nil {
				t.Fatal(err)
			}
			info, err := compat.AudioHeader(path)
			if err != nil {
				t.Fatalf("AudioHeader(%s) failed: %v", tc.name, err)
			}
			if info.Format != tc.format {
				t.Errorf("format = %q, want %q", info.Format, tc.format)
			}
			if info.SampleRate != 48000 || info.Channels != 2 || info.BitDepth != 24 || info.Samples != 96000 || info.Duration != 2 {
				t.Errorf("unexpected audio parameters: %+v", info)
			}
		})
	}
}

func TestID3PrefixedMalformedHeadersFailBounded(t *testing.T) {
	root := t.TempDir()
	validWav := testfixture.WAV(false)

	// Case 1: Bad ID3 version (e.g. ID3v2.5)
	badVer := append([]byte{'I', 'D', '3', 5, 0, 0, 0, 0, 0, 10, 0}, validWav...)
	p1 := filepath.Join(root, "bad_ver.wav")
	_ = os.WriteFile(p1, badVer, 0644)
	if _, err := compat.AudioHeader(p1); err == nil {
		t.Error("accepted bad ID3 version")
	}

	// Case 2: Truncated ID3 header
	trunc := []byte{'I', 'D', '3', 3, 0}
	p2 := filepath.Join(root, "trunc.wav")
	_ = os.WriteFile(p2, trunc, 0644)
	if _, err := compat.AudioHeader(p2); err == nil {
		t.Error("accepted truncated ID3 header")
	}

	// Case 3: Excessive ID3 tags (>8 tags)
	var nine []byte
	for i := 0; i < 9; i++ {
		nine = append(nine, makeID3Tag(3, 0, []byte("T"))...)
	}
	nine = append(nine, validWav...)
	p3 := filepath.Join(root, "too_many.wav")
	_ = os.WriteFile(p3, nine, 0644)
	if _, err := compat.AudioHeader(p3); err == nil {
		t.Error("accepted >8 ID3 tags")
	}
}

func TestVBRMP3AudioHeaderSuppliesDurationSamplesBitrate(t *testing.T) {
	src := "../../internal/audioheader/testdata/03-lavc-vbr-xing-44100.mp3"
	info, err := compat.AudioHeader(src)
	if err != nil {
		t.Fatalf("AudioHeader(%s): %v", src, err)
	}
	if info.Format != "mp3" {
		t.Fatalf("format = %q, want mp3", info.Format)
	}
	if info.SampleRate != 44100 || info.Channels != 2 {
		t.Fatalf("audio params mismatch: %+v", info)
	}
	if info.Samples == 0 || info.Duration <= 0 {
		t.Fatalf("VBR MP3 duration/samples omitted: Samples=%d, Duration=%f", info.Samples, info.Duration)
	}
	if info.Bitrate != 208 {
		t.Fatalf("VBR MP3 bitrate = %d, want 208 kbps", info.Bitrate)
	}
	if info.StartOffsetSamples != 2257 || info.OffsetClass != "lame:2257" {
		t.Fatalf("start offset = (%d, %q), want (2257, lame:2257)", info.StartOffsetSamples, info.OffsetClass)
	}
}
