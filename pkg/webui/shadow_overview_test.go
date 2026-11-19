package webui

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestNormalizeShadowOverviewFillsDecoderScaleWithoutChangingGoodRows(t *testing.T) {
	low := enginedb.DisplayWave{Bands: bytes.Repeat([]byte{2, 1, 1}, 4), SamplesPerBin: 44100}
	got := normalizeShadowOverview(low, [3]float64{0.68, 0.40, 0.18})
	var top [3]byte
	for i, v := range got.Bands {
		top[i%3] = max(top[i%3], v)
	}
	if top != [3]byte{103, 114, 112} {
		t.Fatalf("normalized overview maxima = %v, want decoder-scaled maxima", top)
	}

	good := enginedb.DisplayWave{Bands: []byte{103, 114, 112, 40, 50, 60}, SamplesPerBin: 44100}
	unchanged := normalizeShadowOverview(good, [3]float64{0.68, 0.40, 0.18})
	if !bytes.Equal(unchanged.Bands, good.Bands) {
		t.Fatalf("correctly scaled overview changed: %v -> %v", good.Bands, unchanged.Bands)
	}
}

func TestPerfFromBlobsFillsZeroOverviewMaxima(t *testing.T) {
	// 44-byte Engine 5.0.4 trackData without peaks.
	rawTrack := make([]byte, 44)
	binary.BigEndian.PutUint64(rawTrack[0:], 0x40e5888000000000) // 44100.0
	binary.BigEndian.PutUint64(rawTrack[8:], 44100*10)

	rawBeats := make([]byte, 33)
	binary.BigEndian.PutUint64(rawBeats[0:], 0x40e5888000000000)

	// Overview with 2 bins [180, 120, 60] and [40, 80, 30] and a ZERO 3-byte trailer.
	rawOverview := make([]byte, 24+6+3)
	copy(rawOverview[24:], []byte{180, 120, 60, 40, 80, 30, 0, 0, 0})

	trackBlob, err := enginedb.QCompress(rawTrack)
	if err != nil {
		t.Fatal(err)
	}
	beatsBlob, err := enginedb.QCompress(rawBeats)
	if err != nil {
		t.Fatal(err)
	}
	ovBlob, err := enginedb.QCompress(rawOverview)
	if err != nil {
		t.Fatal(err)
	}

	expf, err := perfFromBlobs(128, trackBlob, beatsBlob, ovBlob)
	if err != nil {
		t.Fatalf("perfFromBlobs: %v", err)
	}
	// EXPF v1: "EXPF" (4) + u32 version (4) + f64 bpm (8) + 3*(u32 len + bytes).
	off := 16
	trackLen := int(binary.LittleEndian.Uint32(expf[off:]))
	off += 4
	gotTrack := expf[off : off+trackLen]
	off += trackLen
	beatsLen := int(binary.LittleEndian.Uint32(expf[off:]))
	off += 4 + beatsLen
	ovLen := int(binary.LittleEndian.Uint32(expf[off:]))
	off += 4
	gotOv := expf[off : off+ovLen]

	if len(gotTrack) != 68 || binary.BigEndian.Uint64(gotTrack[20:]) == 0 {
		t.Fatalf("expected 68-byte trackData with non-zero peaks, got len=%d", len(gotTrack))
	}
	wantTrailer := []byte{180, 120, 60}
	if !bytes.Equal(gotOv[len(gotOv)-3:], wantTrailer) {
		t.Fatalf("overview trailer = %v, want %v", gotOv[len(gotOv)-3:], wantTrailer)
	}
}
