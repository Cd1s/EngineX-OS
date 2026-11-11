// Package testaudio synthesizes deterministic fixtures without ffmpeg/CGO.
package testaudio

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

const Rate = 22050

// Song combines a C major triad with 120-BPM percussive onsets at 0.25s.
func Song(seconds int) []int32 {
	s := make([]int32, Rate*seconds)
	for i := range s {
		t := float64(i) / Rate
		v := 0.0
		for _, f := range []float64{261.625565, 329.627557, 391.995436} {
			v += .12 * math.Sin(2*math.Pi*f*t)
		}
		phase := math.Mod(t-.25, .5)
		if t >= .25 && phase < .04 {
			v += .55 * math.Exp(-phase*100) * math.Sin(2*math.Pi*80*phase)
		}
		s[i] = int32(v * 32767)
	}
	return s
}
func WAV(samples []int32) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+2*len(samples)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint32(Rate))
	binary.Write(&b, binary.LittleEndian, uint32(Rate*2))
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(2*len(samples)))
	for _, s := range samples {
		binary.Write(&b, binary.LittleEndian, int16(s))
	}
	return b.Bytes()
}
func FLAC(w io.Writer, samples []int32) error {
	enc, err := flac.NewEncoder(w, &meta.StreamInfo{BlockSizeMin: 4096, BlockSizeMax: 4096, SampleRate: Rate, NChannels: 1, BitsPerSample: 16, NSamples: uint64(len(samples))})
	if err != nil {
		return err
	}
	for i := 0; i < len(samples); i += 4096 {
		s := samples[i:min(i+4096, len(samples))]
		f := &frame.Frame{Header: frame.Header{BlockSize: uint16(len(s)), SampleRate: Rate, Channels: frame.ChannelsMono, BitsPerSample: 16}, Subframes: []*frame.Subframe{{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s, NSamples: len(s)}}}
		if err := enc.WriteFrame(f); err != nil {
			return err
		}
	}
	return enc.Close()
}

// SpotifyOggMetadataPage builds a CRC-valid Ogg seek/replaygain page whose first
// packet is 0x81. A 139-byte payload gives Spotify's common 167-byte prefix.
func SpotifyOggMetadataPage(payload int) []byte {
	if payload < 1 || payload > 65024 {
		panic("invalid Spotify test metadata length")
	}
	segments := payload/255 + 1
	page := make([]byte, 27+segments+payload)
	copy(page, "OggS")
	page[5] = 2
	binary.LittleEndian.PutUint32(page[14:], 1)
	page[26] = byte(segments)
	left := payload
	for i := 0; i < segments; i++ {
		size := min(left, 255)
		page[27+i] = byte(size)
		left -= size
	}
	page[27+segments] = 0x81
	var crc uint32
	for _, b := range page {
		crc ^= uint32(b) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	binary.LittleEndian.PutUint32(page[22:], crc)
	return page
}
