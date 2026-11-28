package analyzer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

func TestOverviewFileFingerprint(t *testing.T) {
	dir := t.TempDir()

	// Empty file
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gotEmpty, err := OverviewFileFingerprint(empty)
	if err != nil {
		t.Fatal(err)
	}
	var z [8]byte
	wantEmpty := sha256.Sum256(z[:])
	if gotEmpty != hex.EncodeToString(wantEmpty[:]) {
		t.Fatalf("empty fingerprint: got %s want %x", gotEmpty, wantEmpty)
	}

	// Exact 64 KiB boundary (single head read, no duplicate tail read)
	exact := filepath.Join(dir, "exact.bin")
	buf64k := bytes.Repeat([]byte{0x5a, 0xa5}, 32<<10)
	if err := os.WriteFile(exact, buf64k, 0o644); err != nil {
		t.Fatal(err)
	}
	got64, err := OverviewFileFingerprint(exact)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	binary.LittleEndian.PutUint64(z[:], uint64(len(buf64k)))
	h.Write(z[:])
	h.Write(buf64k)
	if got64 != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("64k fingerprint mismatch: %s", got64)
	}

	// >128 KiB file: changing middle bytes stays identical, changing tail changes fingerprint
	big := filepath.Join(dir, "big.bin")
	data := make([]byte, 200<<10)
	for i := range data {
		data[i] = byte(i * 31)
	}
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fp1, err := OverviewFileFingerprint(big)
	if err != nil {
		t.Fatal(err)
	}
	data[100<<10] ^= 0xff // middle
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fp2, err := OverviewFileFingerprint(big)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("middle byte change altered fingerprint: %s != %s", fp1, fp2)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}
	fp3, err := OverviewFileFingerprint(big)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp3 {
		t.Fatalf("tail byte change did not alter fingerprint")
	}
}

func writeWAVHeader(w *bytes.Buffer, sr, ch, bps, nSamples int) {
	dataBytes := nSamples * ch * (bps / 8)
	w.WriteString("RIFF")
	_ = binary.Write(w, binary.LittleEndian, uint32(36+dataBytes))
	w.WriteString("WAVE")
	w.WriteString("fmt ")
	_ = binary.Write(w, binary.LittleEndian, uint32(16))
	_ = binary.Write(w, binary.LittleEndian, uint16(1))
	_ = binary.Write(w, binary.LittleEndian, uint16(ch))
	_ = binary.Write(w, binary.LittleEndian, uint32(sr))
	_ = binary.Write(w, binary.LittleEndian, uint32(sr*ch*(bps/8)))
	_ = binary.Write(w, binary.LittleEndian, uint16(ch*(bps/8)))
	_ = binary.Write(w, binary.LittleEndian, uint16(bps))
	w.WriteString("data")
	_ = binary.Write(w, binary.LittleEndian, uint32(dataBytes))
}

func TestOverviewSidecarRoundTripAndRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "tone.wav")
	sr, n := 44100, 44100*6
	var rawWAV bytes.Buffer
	writeWAVHeader(&rawWAV, sr, 2, 16, n)
	for i := 0; i < n; i++ {
		s := int16(20000 * math.Sin(2*math.Pi*120*float64(i)/float64(sr)))
		_ = binary.Write(&rawWAV, binary.LittleEndian, s)
		_ = binary.Write(&rawWAV, binary.LittleEndian, s)
	}
	if err := os.WriteFile(wav, rawWAV.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	ov, err := ComputeOverviewPeaks(context.Background(), wav, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := OverviewFileFingerprint(wav)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := WriteOverviewSidecar(filepath.Join(dir, "ov"), fp, ov, 3)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	raw, z, err := ParseOverviewSidecar(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, ov.Raw[:]) || !bytes.Equal(z, ov.Compressed) {
		t.Fatalf("roundtrip bytes differ")
	}

	// Reject corrupt magic, CRC bit flip, truncation, bad header, and out-of-range byte
	badMagic := append([]byte(nil), blob...)
	badMagic[0] = 'X'
	if _, _, err := ParseOverviewSidecar(badMagic); err == nil {
		t.Fatal("accepted bad magic")
	}
	badCRC := append([]byte(nil), blob...)
	badCRC[30] ^= 1
	if _, _, err := ParseOverviewSidecar(badCRC); err == nil {
		t.Fatal("accepted bad CRC")
	}
	if _, _, err := ParseOverviewSidecar(blob[:len(blob)-1]); err == nil {
		t.Fatal("accepted truncated sidecar")
	}

	// Re-encode with corrupt raw value (> 127) must be rejected by EncodeOverviewSidecar
	badRaw := ov.Raw
	badRaw[24] = 128
	if _, err := EncodeOverviewSidecar(badRaw[:], ov.Compressed); err == nil {
		t.Fatal("accepted band byte > 127")
	}

	// Verify LRU pruning cap (maxEntries = 3)
	ovDir := filepath.Join(dir, "ov")
	for i := 1; i <= 4; i++ {
		time.Sleep(5 * time.Millisecond)
		f2 := make([]byte, 64)
		for k := range f2 {
			f2[k] = byte('a' + i)
		}
		if _, err := WriteOverviewSidecar(ovDir, string(f2), ov, 3); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(ovDir)
	ovCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ov" {
			ovCount++
		}
	}
	if ovCount != 3 {
		t.Fatalf("LRU cap failed: got %d .ov files want 3", ovCount)
	}
}

func TestBandPeakSeparationAndFastFLACParity(t *testing.T) {
	dir := t.TempDir()
	sr := 44100
	n := sr * 6 // 2s 90 Hz (low), 2s 1000 Hz (mid), 2s 6000 Hz (high)
	flacPath := filepath.Join(dir, "bands.flac")

	// Write multi-band 16-bit stereo FLAC using mewkiz/flac (variable LPC & FIXED frames)
	f, err := os.Create(flacPath)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := flac.NewEncoder(f, &meta.StreamInfo{
		BlockSizeMin:  4096,
		BlockSizeMax:  4096,
		SampleRate:    uint32(sr),
		NChannels:     2,
		BitsPerSample: 16,
		NSamples:      uint64(n),
	})
	if err != nil {
		t.Fatal(err)
	}
	const bs = 4096
	for off := 0; off < n; off += bs {
		cur := bs
		if off+cur > n {
			cur = n - off
		}
		s0 := make([]int32, cur)
		s1 := make([]int32, cur)
		for i := 0; i < cur; i++ {
			pos := off + i
			tSec := float64(pos) / float64(sr)
			var freq float64
			switch {
			case pos < sr*2:
				freq = 90
			case pos < sr*4:
				freq = 1000
			default:
				freq = 6000
			}
			v := int32(28000 * math.Sin(2*math.Pi*freq*tSec))
			s0[i] = v
			s1[i] = -v / 2
		}
		fr := &frame.Frame{
			Header: frame.Header{
				BlockSize:     uint16(cur),
				SampleRate:    uint32(sr),
				Channels:      frame.ChannelsLR,
				BitsPerSample: 16,
			},
			Subframes: []*frame.Subframe{
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s0, NSamples: cur},
				{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s1, NSamples: cur},
			},
		}
		if err := enc.WriteFrame(fr); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Fast FLAC path vs mewkiz/flac slow fallback path must agree
	fastOv, err := ComputeOverviewPeaks(context.Background(), flacPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	rawF, err := os.Open(flacPath)
	if err != nil {
		t.Fatal(err)
	}
	slowOv, err := computeFLACPeaksSlow(rawF, nil)
	rawF.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fastOv.Raw[:], slowOv.Raw[:]) {
		t.Fatalf("fast FLAC and slow FLAC differed")
	}

	// Verify band dominance in each third (skipping boundary bins where filters ring)
	lowBin := 24 + 170*3
	midBin := 24 + 512*3
	highBin := 24 + 850*3
	if fastOv.Raw[lowBin] <= fastOv.Raw[midBin]*2 || fastOv.Raw[lowBin] < 80 {
		t.Fatalf("expected Low dominance at 90Hz, got bin=%v", fastOv.Raw[lowBin:lowBin+3])
	}
	if fastOv.Raw[midBin+1] <= fastOv.Raw[midBin]*2 || fastOv.Raw[midBin+1] < 80 {
		t.Fatalf("expected Mid dominance at 1kHz, got bin=%v", fastOv.Raw[midBin:midBin+3])
	}
	if fastOv.Raw[highBin+2] <= fastOv.Raw[highBin]*2 || fastOv.Raw[highBin+2] < 80 {
		t.Fatalf("expected High dominance at 6kHz, got bin=%v", fastOv.Raw[highBin:highBin+3])
	}
}

func TestMP4ReturnsUnsupportedOverviewFormat(t *testing.T) {
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "track.mp4")
	if err := os.WriteFile(mp4, []byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A isom"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ComputeOverviewPeaks(context.Background(), mp4, nil)
	if !errors.Is(err, ErrUnsupportedOverviewFormat) {
		t.Fatalf("got %v, want ErrUnsupportedOverviewFormat", err)
	}
}

func TestComputeOverviewPeaksRespectsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "c.wav")
	_ = os.WriteFile(wav, []byte("RIFF\x24\x00\x00\x00WAVE"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ComputeOverviewPeaks(ctx, wav, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v want context.Canceled", err)
	}
}
