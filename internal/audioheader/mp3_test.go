package audioheader

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"
)

func buildMPEGFrame(ver, srIdx, bitrateIdx, padding, mode int) ([]byte, int) {
	var verBits uint32
	switch ver {
	case 1:
		verBits = 3
	case 2:
		verBits = 2
	case 25:
		verBits = 0
	}
	h := uint32(0xffe00000) | (verBits << 19) | (1 << 17) | (1 << 16) |
		(uint32(bitrateIdx) << 12) | (uint32(srIdx) << 10) | (uint32(padding) << 9) |
		(uint32(mode) << 6)
	dec, ok := decodeMPEGHeader(h)
	if !ok {
		panic("invalid synthetic MPEG header")
	}
	frame := make([]byte, dec.frameSize)
	binary.BigEndian.PutUint32(frame, h)
	return frame, dec.sideInfo
}

func buildXingLAMEStream(ver, srIdx, bitrateIdx, mode int, tag, encoder string, delay, pad int) []byte {
	f0, side := buildMPEGFrame(ver, srIdx, bitrateIdx, 0, mode)
	p := 4 + side
	copy(f0[p:p+4], tag)
	binary.BigEndian.PutUint32(f0[p+4:p+8], 0x03) // FRAMES | SIZE
	binary.BigEndian.PutUint32(f0[p+8:p+12], 3)
	binary.BigEndian.PutUint32(f0[p+12:p+16], uint32(len(f0)*3))
	p += 16
	if encoder != "" {
		copy(f0[p:p+9], encoder)
		v := uint32(delay&0xfff)<<12 | uint32(pad&0xfff)
		f0[p+21] = byte(v >> 16)
		f0[p+22] = byte(v >> 8)
		f0[p+23] = byte(v)
	}
	f1, _ := buildMPEGFrame(ver, srIdx, bitrateIdx, 0, mode)
	f2, _ := buildMPEGFrame(ver, srIdx, bitrateIdx, 0, mode)
	return append(append(f0, f1...), f2...)
}

func makeID3v2(version, flags byte, payload []byte) []byte {
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

func TestReadMP3RealFixtures(t *testing.T) {
	cases := []struct {
		file       string
		sr         int
		hasXing    bool
		tag        string
		lame       bool
		delay      int
		wantOffset int
		wantClass  string
	}{
		{"01-lame-cbr-info-44100.mp3", 44100, true, "Info", true, 576, 2257, "lame:2257"},
		{"01-lame-cbr-info-48000.mp3", 48000, true, "Info", true, 576, 2257, "lame:2257"},
		{"03-lavc-vbr-xing-44100.mp3", 44100, true, "Xing", true, 576, 2257, "lame:2257"},
		{"04-xing-only-44100.mp3", 44100, true, "Xing", false, 0, 1152, "xing:1152"},
		{"04-xing-only-48000.mp3", 48000, true, "Xing", false, 0, 1152, "xing:1152"},
		{"05-headerless-lame-44100.mp3", 44100, false, "", false, 0, 0, ""},
		{"05-headerless-lame-48000.mp3", 48000, false, "", false, 0, 0, ""},
		{"06-headerless-shine-44100.mp3", 44100, false, "", false, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			got, err := OpenMP3(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("OpenMP3(%s): %v", tc.file, err)
			}
			if got.SampleRate != tc.sr || got.Channels != 2 || got.Layer != 3 || got.SamplesPerFrame != 1152 {
				t.Fatalf("unexpected audio params: %+v", got)
			}
			if got.HasXing != tc.hasXing || got.Tag != tc.tag || got.LAMEApplied != tc.lame || got.EncoderDelay != tc.delay {
				t.Fatalf("unexpected tag/LAME parse: %+v", got)
			}
			if got.StartOffsetSamples != tc.wantOffset || got.OffsetClass != tc.wantClass {
				t.Fatalf("offset = (%d, %q), want (%d, %q)", got.StartOffsetSamples, got.OffsetClass, tc.wantOffset, tc.wantClass)
			}
		})
	}
}

func TestReadMP3SyntheticVersionsEncodersAndEdgeCases(t *testing.T) {
	// MPEG-1 mono (side-info 17) with Lavf encoder string and custom 12-bit delay 672:
	// 1152 + 672 + 529 = 2353 samples.
	lavfMono := buildXingLAMEStream(1, 0, 9, 3, "Xing", "Lavf61.7.", 672, 1000)
	got, err := ReadMP3(bytes.NewReader(lavfMono), int64(len(lavfMono)))
	if err != nil {
		t.Fatal(err)
	}
	if got.SampleRate != 44100 || got.Channels != 1 || got.StartOffsetSamples != 2353 || got.EndPadding != 1000 || got.OffsetClass != "lame:2353" {
		t.Fatalf("MPEG-1 mono Lavf: %+v", got)
	}

	// MPEG-2 22050 Hz stereo (side-info 17, spf 576): 576 + 576 + 529 = 1681; Xing-only = 576.
	mpeg2Lame := buildXingLAMEStream(2, 0, 8, 0, "Xing", "LAME3.100", 576, 576)
	got, err = ReadMP3(bytes.NewReader(mpeg2Lame), int64(len(mpeg2Lame)))
	if err != nil || got.SampleRate != 22050 || got.SamplesPerFrame != 576 || got.StartOffsetSamples != 1681 || got.OffsetClass != "lame:1681" {
		t.Fatalf("MPEG-2 LAME: %+v, err=%v", got, err)
	}
	mpeg2Xing := buildXingLAMEStream(2, 0, 8, 0, "Xing", "", 0, 0)
	got, err = ReadMP3(bytes.NewReader(mpeg2Xing), int64(len(mpeg2Xing)))
	if err != nil || got.SampleRate != 22050 || got.SamplesPerFrame != 576 || got.StartOffsetSamples != 576 || got.OffsetClass != "xing:576" {
		t.Fatalf("MPEG-2 Xing-only: %+v, err=%v", got, err)
	}

	// MPEG-2.5 11025 Hz mono (side-info 9, spf 576).
	mpeg25Lame := buildXingLAMEStream(25, 0, 8, 3, "Info", "Lavc61.19", 576, 288)
	got, err = ReadMP3(bytes.NewReader(mpeg25Lame), int64(len(mpeg25Lame)))
	if err != nil || got.SampleRate != 11025 || got.Channels != 1 || got.SamplesPerFrame != 576 || got.StartOffsetSamples != 1681 {
		t.Fatalf("MPEG-2.5 mono Lavc: %+v, err=%v", got, err)
	}

	// Non-FFmpeg encoder prefix in Xing extension must NOT apply delay+529 (only Xing frame = 1152).
	gogo := buildXingLAMEStream(1, 0, 9, 0, "Xing", "GoGo3.13 ", 576, 576)
	got, err = ReadMP3(bytes.NewReader(gogo), int64(len(gogo)))
	if err != nil || got.LAMEApplied || got.StartOffsetSamples != 1152 || got.OffsetClass != "xing:1152" {
		t.Fatalf("GoGo encoder must be treated as Xing-only: %+v, err=%v", got, err)
	}

	// Never trust encoder name alone: headerless MP3 with LAME3.100 in ID3v2 and frame ancillary bytes -> 0.
	f0, side := buildMPEGFrame(1, 0, 11, 0, 0)
	copy(f0[4+side+16:], "LAME3.100UUUUUUUU")
	f1, _ := buildMPEGFrame(1, 0, 11, 0, 0)
	hdrless := append(makeID3v2(3, 0, []byte("TSSE\x00\x00\x00\x0a\x00\x00\x00LAME3.100")), append(f0, f1...)...)
	got, err = ReadMP3(bytes.NewReader(hdrless), int64(len(hdrless)))
	if err != nil || got.HasXing || got.LAMEApplied || got.StartOffsetSamples != 0 || got.OffsetClass != "" {
		t.Fatalf("headerless MP3 with LAME strings must get 0 offset: %+v, err=%v", got, err)
	}

	// VBRI tag at 4+32 with non-zero delay: FFmpeg mp3_parse_vbri_tag skips delay without setting start_skip_samples -> 0.
	vbri0, _ := buildMPEGFrame(1, 1, 11, 0, 0) // 48000 Hz
	copy(vbri0[36:40], "VBRI")
	binary.BigEndian.PutUint16(vbri0[40:42], 1)    // version 1
	binary.BigEndian.PutUint16(vbri0[42:44], 1152) // VBRI delay field (ignored by FFmpeg)
	binary.BigEndian.PutUint32(vbri0[46:50], uint32(len(vbri0)*2))
	binary.BigEndian.PutUint32(vbri0[50:54], 2)
	vbri1, _ := buildMPEGFrame(1, 1, 11, 0, 0)
	vbri2, _ := buildMPEGFrame(1, 1, 11, 0, 0)
	vbriStream := append(append(vbri0, vbri1...), vbri2...)
	got, err = ReadMP3(bytes.NewReader(vbriStream), int64(len(vbriStream)))
	if err != nil || got.SampleRate != 48000 || got.StartOffsetSamples != 0 || got.OffsetClass != "" {
		t.Fatalf("VBRI must decode sample rate with 0 offset: %+v, err=%v", got, err)
	}

	// Multiple back-to-back ID3v2 tags (v2.3 + v2.4 with 3DI footer) before tagged stream.
	tagged := buildXingLAMEStream(1, 1, 9, 0, "Info", "LAME3.100", 576, 1344)
	multiID3 := append(append(makeID3v2(3, 0, []byte("tag-one")), makeID3v2(4, 16, []byte("tag-two"))...), tagged...)
	got, err = ReadMP3(bytes.NewReader(multiID3), int64(len(multiID3)))
	if err != nil || got.SampleRate != 48000 || got.StartOffsetSamples != 2257 || got.OffsetClass != "lame:2257" {
		t.Fatalf("multiple ID3v2 tags failed: %+v, err=%v", got, err)
	}

	// Invalid inputs must fail closed.
	badFooter := append([]byte(nil), multiID3...)
	badFooter[len(makeID3v2(3, 0, []byte("tag-one")))+10+7] = 'X'
	for _, bad := range [][]byte{
		nil,
		[]byte("not an mp3 file"),
		append([]byte{'I', 'D', '3', 5, 0, 0, 0, 0, 0, 0}, tagged...),
		append([]byte{'I', 'D', '3', 3, 0, 0, 0x80, 0, 0, 0}, tagged...),
		badFooter,
		bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64}, 50), // misaligned 417-byte frame size
	} {
		if _, err := ReadMP3(bytes.NewReader(bad), int64(len(bad))); err == nil {
			t.Fatalf("accepted invalid MP3 input of len %d", len(bad))
		}
	}
}

func TestPeekMP3StartOffsetMatchesReadMP3OnZeroFlagLAMETag(t *testing.T) {
	f0, side := buildMPEGFrame(1, 0, 9, 0, 0)
	p := 4 + side
	copy(f0[p:p+4], "Xing")
	binary.BigEndian.PutUint32(f0[p+4:p+8], 0) // neither FRAMES nor BYTES flag set
	p += 8
	copy(f0[p:p+9], "LAME3.100")
	v := uint32(576)<<12 | 576
	f0[p+21] = byte(v >> 16)
	f0[p+22] = byte(v >> 8)
	f0[p+23] = byte(v)
	f1, _ := buildMPEGFrame(1, 0, 9, 0, 0)
	stream := append(f0, f1...)

	info, err := ReadMP3(bytes.NewReader(stream), int64(len(stream)))
	if err != nil {
		t.Fatal(err)
	}
	peeked := PeekMP3StartOffset(bufio.NewReader(bytes.NewReader(stream)))
	if peeked != info.StartOffsetSamples || peeked != 576+529 {
		t.Fatalf("PeekMP3StartOffset = %d, ReadMP3 = %d, want %d", peeked, info.StartOffsetSamples, 576+529)
	}
}
