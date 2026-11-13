package audioheader

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

func TestReadMP3VBRDurationSamplesAndBitrate(t *testing.T) {
	path := filepath.Join("testdata", "03-lavc-vbr-xing-44100.mp3")
	got, err := OpenMP3(path)
	if err != nil {
		t.Fatalf("OpenMP3(%s): %v", path, err)
	}
	if got.SampleRate != 44100 || got.Channels != 2 {
		t.Fatalf("unexpected audio params: %+v", got)
	}
	// 3 frames * 1152 samples = 3456 samples.
	if got.Samples != 3456 {
		t.Errorf("got.Samples = %d, want 3456", got.Samples)
	}
	wantDur := float64(3456) / 44100.0
	if got.Duration != wantDur {
		t.Errorf("got.Duration = %f, want %f", got.Duration, wantDur)
	}
	if got.Bitrate != 208 {
		t.Errorf("got.Bitrate = %d, want 208", got.Bitrate)
	}
	if got.StartOffsetSamples != 2257 || got.OffsetClass != "lame:2257" {
		t.Errorf("offset = (%d, %q), want (2257, lame:2257)", got.StartOffsetSamples, got.OffsetClass)
	}
}

func TestReadMP3VBRWithoutBytesFlagDerivesBitrate(t *testing.T) {
	// Build a 4-frame MPEG-1 44100 Hz Xing stream where flags has FRAMES (0x01) but NOT BYTES (0x02).
	f0, side := buildMPEGFrame(1, 0, 9, 0, 0) // 128 kbps template
	p := 4 + side
	copy(f0[p:p+4], "Xing")
	binary.BigEndian.PutUint32(f0[p+4:p+8], 0x01) // FRAMES only
	binary.BigEndian.PutUint32(f0[p+8:p+12], 4)    // 4 frames
	f1, _ := buildMPEGFrame(1, 0, 9, 0, 0)
	f2, _ := buildMPEGFrame(1, 0, 9, 0, 0)
	f3, _ := buildMPEGFrame(1, 0, 9, 0, 0)
	stream := append(append(append(f0, f1...), f2...), f3...)

	got, err := ReadMP3(bytes.NewReader(stream), int64(len(stream)))
	if err != nil {
		t.Fatalf("ReadMP3: %v", err)
	}
	if got.Samples != 4*1152 {
		t.Errorf("got.Samples = %d, want %d", got.Samples, 4*1152)
	}
	wantDur := float64(4*1152) / 44100.0
	if got.Duration != wantDur {
		t.Errorf("got.Duration = %f, want %f", got.Duration, wantDur)
	}
	if got.Bitrate <= 0 {
		t.Errorf("expected derived Bitrate > 0, got %d", got.Bitrate)
	}
	if got.StartOffsetSamples != 1152 || got.OffsetClass != "xing:1152" {
		t.Errorf("offset = (%d, %q), want (1152, xing:1152)", got.StartOffsetSamples, got.OffsetClass)
	}
}

func TestReadMP3VBRIDurationSamplesAndBitrate(t *testing.T) {
	// VBRI header at offset 36 in stereo MPEG-1 48000 Hz frame.
	vbri0, _ := buildMPEGFrame(1, 1, 11, 0, 0) // 48000 Hz, 160 kbps, frame size 480 bytes
	copy(vbri0[36:40], "VBRI")
	binary.BigEndian.PutUint16(vbri0[40:42], 1)                   // version 1
	binary.BigEndian.PutUint16(vbri0[42:44], 1152)                // delay field (ignored by FFmpeg)
	binary.BigEndian.PutUint32(vbri0[46:50], uint32(len(vbri0)*3)) // bytes (3 frames total)
	binary.BigEndian.PutUint32(vbri0[50:54], 3)                   // 3 frames
	vbri1, _ := buildMPEGFrame(1, 1, 11, 0, 0)
	vbri2, _ := buildMPEGFrame(1, 1, 11, 0, 0)
	stream := append(append(vbri0, vbri1...), vbri2...)

	got, err := ReadMP3(bytes.NewReader(stream), int64(len(stream)))
	if err != nil {
		t.Fatalf("ReadMP3: %v", err)
	}
	if got.Tag != "VBRI" {
		t.Errorf("got.Tag = %q, want VBRI", got.Tag)
	}
	if got.Samples != 3*1152 {
		t.Errorf("got.Samples = %d, want %d", got.Samples, 3*1152)
	}
	wantDur := float64(3*1152) / 48000.0
	if got.Duration != wantDur {
		t.Errorf("got.Duration = %f, want %f", got.Duration, wantDur)
	}
	if got.Bitrate != 192 {
		t.Errorf("got.Bitrate = %d, want 192", got.Bitrate)
	}
	if got.StartOffsetSamples != 0 || got.OffsetClass != "" {
		t.Errorf("VBRI must have 0 start offset, got (%d, %q)", got.StartOffsetSamples, got.OffsetClass)
	}
}

func TestReadMP3MalformedFramesRemainBounded(t *testing.T) {
	f0, side := buildMPEGFrame(1, 0, 9, 0, 0)
	p := 4 + side
	copy(f0[p:p+4], "Xing")
	binary.BigEndian.PutUint32(f0[p+4:p+8], 0x01)
	// Absurdly large frame count (100,000,000 frames) in a 2-frame stream
	binary.BigEndian.PutUint32(f0[p+8:p+12], 100_000_000)
	f1, _ := buildMPEGFrame(1, 0, 9, 0, 0)
	stream := append(f0, f1...)

	got, err := ReadMP3(bytes.NewReader(stream), int64(len(stream)))
	if err != nil {
		t.Fatalf("ReadMP3 should not fail on bounded malformed header: %v", err)
	}
	// Bounded check should reset implausible frame count to 0 rather than creating invalid duration
	if got.Duration > 1e7 || got.Samples > uint64(len(stream))*1152 {
		t.Errorf("malformed frames not bounded: Samples=%d, Duration=%f", got.Samples, got.Duration)
	}
	// Start offset remains preserved
	if got.StartOffsetSamples != 1152 {
		t.Errorf("StartOffsetSamples = %d, want 1152", got.StartOffsetSamples)
	}
}
