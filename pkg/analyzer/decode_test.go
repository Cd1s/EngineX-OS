package analyzer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
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
