package analyzer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func floatWAV(bits, channels int, samples []float64) []byte {
	b := make([]byte, 44+len(samples)*bits/8)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], wavFormatFloat)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], 48000)
	binary.LittleEndian.PutUint32(b[28:], uint32(48000*channels*bits/8))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*bits/8))
	binary.LittleEndian.PutUint16(b[34:], uint16(bits))
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(b)-44))
	for i, v := range samples {
		if bits == 32 {
			binary.LittleEndian.PutUint32(b[44+i*4:], math.Float32bits(float32(v)))
		} else {
			binary.LittleEndian.PutUint64(b[44+i*8:], math.Float64bits(v))
		}
	}
	return b
}

func TestDecodeFloatWAVRejectsNonFiniteSamples(t *testing.T) {
	for _, bits := range []int{32, 64} {
		for _, channels := range []int{1, 2} {
			for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
				t.Run(fmt.Sprintf("%dbit/%dch/%v", bits, channels, v), func(t *testing.T) {
					samples := make([]float64, channels*2)
					samples[len(samples)-1] = v
					if a, err := DecodeWAV(bytes.NewReader(floatWAV(bits, channels, samples))); err == nil || a != nil {
						t.Fatalf("non-finite PCM accepted: audio=%v err=%v", a, err)
					}
				})
			}
		}
	}
}

func TestDecodeFloatWAVPreservesFinitePCM(t *testing.T) {
	for _, bits := range []int{32, 64} {
		for _, channels := range []int{1, 2} {
			t.Run(fmt.Sprintf("%dbit/%dch", bits, channels), func(t *testing.T) {
				samples := []float64{-.75, .25, 1.5, -.5}
				a, err := DecodeWAV(bytes.NewReader(floatWAV(bits, channels, samples)))
				if err != nil {
					t.Fatal(err)
				}
				if len(a.Samples) != len(samples)/channels || a.SampleRate != 48000 || a.BitsPerSample != bits || a.Channels != channels {
					t.Fatal("incorrect floating PCM metadata or sample count")
				}
				for i, got := range a.Samples {
					want := 0.0
					for _, v := range samples[i*channels : (i+1)*channels] {
						want += v / float64(channels)
					}
					if got != float32(want) {
						t.Fatalf("sample %d = %v, want %v", i, got, want)
					}
				}
			})
		}
	}
}

func TestDecodeFloat64WAVRejectsFloat32Overflow(t *testing.T) {
	for _, v := range []float64{math.MaxFloat64, -math.MaxFloat64, 2 * math.MaxFloat32} {
		if a, err := DecodeWAV(bytes.NewReader(floatWAV(64, 2, []float64{v, v}))); err == nil || a != nil {
			t.Fatalf("unrepresentable PCM %v accepted: err=%v", v, err)
		}
	}
}

// constantFLAC encodes 16 constant samples per channel, with a real frame CRC.
// Inherited frame fields exercise formats (including 32 bits) absent from the
// frame header's explicit sample-size table.
func constantFLAC(bits, channels int, inherit bool) []byte {
	b := make([]byte, 42)
	copy(b, "fLaC\x80\x00\x00\x22")
	binary.BigEndian.PutUint16(b[8:], 16)
	binary.BigEndian.PutUint16(b[10:], 16)
	packed := uint64(8000)<<44 | uint64(channels-1)<<41 | uint64(bits-1)<<36 | 16
	binary.BigEndian.PutUint64(b[18:], packed)
	rateCode, bitsCode := byte(4), map[int]byte{8: 1, 16: 4, 24: 6}[bits]
	if inherit {
		rateCode, bitsCode = 0, 0
	}
	f := []byte{0xff, 0xf8, 0x60 | rateCode, byte(channels-1)<<4 | bitsCode<<1, 0, 15}
	var crc8 byte
	for _, v := range f {
		crc8 ^= v
		for range 8 {
			if crc8&0x80 != 0 {
				crc8 = crc8<<1 ^ 0x07
			} else {
				crc8 <<= 1
			}
		}
	}
	f = append(f, crc8)
	for range channels {
		f = append(f, 0) // constant subframe, no wasted bits
		value := uint32(1) << (bits - 2)
		for shift := bits - 8; shift >= 0; shift -= 8 {
			f = append(f, byte(value>>shift))
		}
	}
	var crc16 uint16
	for _, v := range f {
		crc16 ^= uint16(v) << 8
		for range 8 {
			if crc16&0x8000 != 0 {
				crc16 = crc16<<1 ^ 0x8005
			} else {
				crc16 <<= 1
			}
		}
	}
	f = append(f, byte(crc16>>8), byte(crc16))
	return append(b, f...)
}

func TestDecodeFLACFormatBoundsAndInheritedFields(t *testing.T) {
	for _, bits := range []int{8, 16, 24, 32} {
		for _, channels := range []int{1, 8} {
			for _, inherit := range []bool{false, true} {
				if bits == 32 && !inherit {
					continue
				}
				t.Run(fmt.Sprintf("%dbit/%dch/inherit=%v", bits, channels, inherit), func(t *testing.T) {
					a, err := DecodeFLAC(bytes.NewReader(constantFLAC(bits, channels, inherit)))
					if err != nil {
						t.Fatal(err)
					}
					if a.BitsPerSample != bits || a.Channels != channels || a.SampleRate != 8000 || len(a.Samples) != 16 {
						t.Fatal("wrong FLAC format or sample count")
					}
					for i, v := range a.Samples {
						if v != .5 {
							t.Fatalf("sample %d = %v, want .5", i, v)
						}
					}
				})
			}
		}
	}
}

func TestDecodeFLACRejectsInvalidOrInconsistentStreamInfo(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mask, value uint64
	}{
		{"one_bit", 31 << 36, 0},
		{"seven_bits", 31 << 36, 6 << 36},
		{"low_rate", 0xfffff << 44, 7999 << 44},
		{"high_rate", 0xfffff << 44, 192001 << 44},
		{"too_many_samples", 0xfffffffff, MaxDecodedSamples + 1},
		{"different_channels", 7 << 41, 1 << 41},
		{"different_bits", 31 << 36, 23 << 36},
		{"different_rate", 0xfffff << 44, 16000 << 44},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := constantFLAC(16, 1, false)
			packed := binary.BigEndian.Uint64(b[18:26])
			binary.BigEndian.PutUint64(b[18:26], packed&^tc.mask|tc.value)
			if a, err := DecodeFLAC(bytes.NewReader(b)); err == nil || a != nil {
				t.Fatalf("invalid STREAMINFO accepted: err=%v", err)
			}
		})
	}
}

func TestDecodeMP3SampleRates(t *testing.T) {
	for _, tc := range []struct {
		rate, frameSize int
		header          [4]byte
	}{
		{16000, 288, [4]byte{0xff, 0xf3, 0x88, 0}},
		{32000, 576, [4]byte{0xff, 0xfb, 0x98, 0}},
		{44100, 417, [4]byte{0xff, 0xfb, 0x90, 0}},
		{48000, 384, [4]byte{0xff, 0xfb, 0x94, 0}},
	} {
		t.Run(fmt.Sprint(tc.rate), func(t *testing.T) {
			// Zero side information and main data encode silence without a bit reservoir.
			b := make([]byte, tc.frameSize*2)
			copy(b, tc.header[:])
			copy(b[tc.frameSize:], tc.header[:])
			a, err := DecodeMP3(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if a.SampleRate != tc.rate || len(a.Samples) == 0 {
				t.Fatal("wrong MP3 sample rate or empty PCM")
			}
			for _, v := range a.Samples {
				if v != 0 {
					t.Fatalf("silent MP3 decoded as %v", v)
				}
			}
		})
	}
	if _, err := DecodeMP3(bytes.NewReader([]byte{0xff, 0xfb, 0x9c, 0})); err == nil {
		t.Fatal("reserved MP3 sample-rate index accepted")
	}
}

// oggStereoFixture is an original 8-second, 16-kHz stereo signal encoded once
// with libvorbis (ffmpeg -q:a 2). Left: .5*sin(440 Hz); right: .25*sin(660 Hz).
// Embedded here to keep tests offline and free of ffmpeg/native dependencies.
const oggStereoFixture = `
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
IQgppRBSSiGklEJIKYWQAACAAQcAgAATykChISsBgFQAAABCCCGEEEIIIYQQQgghhBBCCCGEEEIIIYQQQgghhBBCCCGEEEII
IYQQQgghhBBCCCGE0DnnnHPOOeecc84555xzzjnnnHPOOeecc84555xzzjnnnHPOOeecc84555xzzjnnnHPOOeecc04AIHaF
A8BOhA2rI5wUjQUWGrISAAgHAACMMcY5i7XWWmullFISaq211lozhZSS0GKMMcYYMwYhpRZjjDHGmDHnqMUYY4wxxtZKiS3G
GGOMMbZWSowxxhhjjDHG2GKLMcYYY4wxxhZjjDHGGGOMMcYYY4wxxhhjjDHGFmOMMcYYY4wxxhhjjDHGGGOMMcYYY4wxxhhj
jC3GGGOMMcYYY4wxxhhjjAUAmDw4AEAl2DjDStJZ4WhwoSErAYDcAADAGKUYc8w5CCGUUkIpqbXOOQchhFJKKSmVllJMGXPO
OQihlFJCKSm1lDrnHIRSSkkppZRSS611DkIIoZRSSkkppZRaCiGEUkpJJaWUUmqttRRCCKWUklJKKaWUWmsxlBJSKaWklFIq
qaXWUksllJJKSimllFJqLbXWSimppJRSSimllFpsLYVSUioppZZSSqm1GFsspZWUUkoppdRaiq21FltKKaXUUksppdZiS62l
lFJqKaXUUmqpxdhaaymllFpKLbWUUoqttRZTSq2llFpqraUWW0qtpZZSai21lFJrrcUWW2stpZZSSim11mJLMbaWWkkppZZa
S63F1mJrrbXUWkstpdRiizHG2GJsLaaUUmoptVQAANCBAwBAgBGVFmKnGVcegSMKGSagQkNWAgBkAAAEMk0yJyk1wiSnGJTS
nHNKKaWUhsiSDFIMqiOTMScpZ4g0hhSkninymFIMYghJhU4xh60mH0voINagjBEupRgAAABBAICAkAAAAwQFMwDA4ABh5ECg
I4DAoQ0AMBAhM4FBITQ4yASAB4gIqQAgMUFRutAFIUSQLoIsHrhw4sYTN5zQoQ0EAAAAAAAEAD4AABIKICAimrkKiwuMDI0N
jg6PDxABAAAAAAACgA8AgCQECIiIZq7C4gIjQ2ODo8PjAyQAABBAAAAAAAAEEICAgAAAAAAAQAAAAICAT2dnUwAAgD4AAAAA
AAB0abtWAgAAAAbCgR4hLS4xLy8vLi4wMC8uLjAvLDAvLi8wLzAvLi4uLi8vLy4uVLV+a/r41GofwPv2MyuAwEf/9a8fAgAA
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
`

func oggStereo(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(oggStereoFixture)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeOggVorbisStereo(t *testing.T) {
	a, err := DecodeOggVorbis(bytes.NewReader(oggStereo(t)))
	if err != nil {
		t.Fatal(err)
	}
	if a.Format != "ogg" || a.SampleRate != 16000 || a.Channels != 2 || a.BitsPerSample != 16 {
		t.Fatalf("wrong format: %+v", *a)
	}
	if len(a.Samples) != 128000 || a.Duration() != 8 {
		t.Fatalf("samples=%d duration=%g", len(a.Samples), a.Duration())
	}
	// Compare with an independent analytic average, allowing Vorbis quantization.
	var squaredError float64
	for i, v := range a.Samples {
		if math.IsNaN(float64(v)) || v < -1 || v > 1 {
			t.Fatalf("invalid sample %d: %v", i, v)
		}
		tm := float64(i) / 16000
		want := .25*math.Sin(2*math.Pi*440*tm) + .125*math.Sin(2*math.Pi*660*tm)
		squaredError += math.Pow(float64(v)-want, 2)
	}
	if rmse := math.Sqrt(squaredError / float64(len(a.Samples))); rmse > .015 {
		t.Fatalf("stereo average differs from source: RMSE=%g", rmse)
	}
	path := filepath.Join(t.TempDir(), "spotify-track.bin")
	if err := os.WriteFile(path, oggStereo(t), 0600); err != nil {
		t.Fatal(err)
	}
	byMagic, err := DecodeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if byMagic.Format != "ogg" || len(byMagic.Samples) != len(a.Samples) {
		t.Fatal("magic detection failed")
	}
	for i := range a.Samples {
		if a.Samples[i] != byMagic.Samples[i] {
			t.Fatalf("DecodeFile sample %d differs", i)
		}
	}
}

func TestDecodeOggVorbisRejectsCorruption(t *testing.T) {
	b := oggStereo(t)
	for _, tc := range []struct {
		name    string
		corrupt func([]byte) []byte
	}{
		{"empty", func(b []byte) []byte { return nil }},
		{"not_ogg", func(b []byte) []byte { return []byte("not an Ogg file") }},
		{"only_magic", func(b []byte) []byte { return b[:4] }},
		{"headers_only", func(b []byte) []byte { return b[:58] }},
		{"truncated_tail", func(b []byte) []byte { return b[:len(b)-1] }},
		{"bad_crc", func(b []byte) []byte { b[100] ^= 1; return b }},
		{"empty_segment_table", func(b []byte) []byte { b[26] = 0; return b }},
		{"zero_channels", func(b []byte) []byte { b[39] = 0; oggPageCRC(b[:58]); return b }},
		{"zero_rate", func(b []byte) []byte { clear(b[40:44]); oggPageCRC(b[:58]); return b }},
		{"missing_eos_page", func(b []byte) []byte {
			off := 0
			for off < len(b) {
				if b[off+5]&4 != 0 {
					return b[:off]
				}
				size := 27 + int(b[off+26])
				for _, n := range b[off+27 : off+size] {
					size += int(n)
				}
				off += size
			}
			t.Fatal("fixture has no EOS page")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if a, err := DecodeOggVorbis(bytes.NewReader(tc.corrupt(bytes.Clone(b)))); err == nil || a != nil {
				t.Fatalf("corrupt Ogg accepted: err=%v", err)
			}
		})
	}
}

func oggPageCRC(page []byte) {
	clear(page[22:26])
	var crc uint32
	for _, v := range page {
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(page[22:26], crc)
}

func TestDecodeOggVorbisSampleLimit(t *testing.T) {
	// Exercise the production streaming guard with a smaller budget, avoiding a
	// 128-MiB fixture and allocation on the player's 32-bit test/build hosts.
	for _, limit := range []int{0, 1024, 127999, 128000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			a, err := decodeOggVorbis(bytes.NewReader(oggStereo(t)), nil, limit)
			if limit < 128000 {
				if a != nil || err == nil || !strings.Contains(err.Error(), "PCM memory limit") {
					t.Fatalf("guard failed: %v", err)
				}
			} else if err != nil || len(a.Samples) != limit || cap(a.Samples) > limit {
				t.Fatalf("exact limit rejected or overallocated: %v", err)
			}
		})
	}
}

func TestDecodeOggVorbisTickerAndReaderErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spotify-track.ogg")
	if err := os.WriteFile(path, oggStereo(t), 0600); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("test: analysis canceled")
	calls := 0
	ticker := shadowTestTicker(func() error {
		calls++
		if calls == 8 {
			return stop
		}
		return nil
	})
	if a, err := decodeFile(path, ticker); a != nil || !errors.Is(err, stop) || calls != 8 {
		t.Fatalf("ticker ignored: calls=%d err=%v", calls, err)
	}
	// The small fixture fits in DecodeFile's byte buffer. Eight ticks still
	// cancel mid-decode, proving checks occur between PCM chunks too.
	failing := io.MultiReader(bytes.NewReader(oggStereo(t)[:58]), oggErrorReader{stop})
	if a, err := DecodeOggVorbis(failing); a != nil || !errors.Is(err, stop) {
		t.Fatalf("read error lost: %v", err)
	}
}

type oggErrorReader struct{ err error }

func (r oggErrorReader) Read([]byte) (int, error) { return 0, r.err }
