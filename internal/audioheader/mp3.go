package audioheader

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// mp3Mask matches FFmpeg's MP3_MASK (0xFFFE0CCF) in mpegaudiodecheader.h when
// verifying two consecutive MPEG audio frames.
const (
	mp3Mask         = 0xfffe0ccf
	mp3ScanWindow   = 64 * 1024
	mpaMaxFrameSize = 2884
	lameDecoderPad  = 528 + 1 // FFmpeg libavformat/mp3dec.c: start_pad + 528 + 1
)

var mpaBitrateTab = [2][3][15]int{
	{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
	},
	{
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	},
}

var mpaFreqTab = [3]int{44100, 48000, 32000}

// MP3Info describes the first audio frame of an MP3 stream and the start-offset
// difference between Rekordbox and Engine.
//
// Rekordbox decodes a leading Xing/Info frame as audio and never trims LAME
// gapless delay. Engine uses FFmpeg's MP3 demuxer (libavformat/mp3dec.c), which
// drops a valid Xing/Info frame and, when mp3_parse_info_tag sees a "LAME",
// "Lavf", or "Lavc" extension, also skips start_pad + 529 samples.
//
// VBRI frames receive StartOffsetSamples = 0: FFmpeg's mp3_parse_vbri_tag skips
// the VBRI delay field without setting start_skip_samples, and Rekordbox VBRI
// timing has no measured shift.
type MP3Info struct {
	SampleRate         int
	Channels           int
	Bitrate            int // kbps
	Samples            uint64
	Duration           float64
	MPEGVersion        int // 1, 2, or 25 (MPEG 2.5)
	Layer              int // 1, 2, or 3
	SamplesPerFrame    int
	HasXing            bool
	Tag                string // "Xing", "Info", "VBRI", or ""
	Encoder            string // 9-byte LAME extension encoder field (trimmed of trailing NULs)
	LAMEApplied        bool   // true when Encoder starts with "LAME", "Lavf", or "Lavc"
	EncoderDelay       int    // 12-bit start_pad
	EndPadding         int    // 12-bit end_pad
	StartOffsetSamples int    // Rekordbox sample - Engine sample (>= 0)
	OffsetClass        string // "", "xing:<samples>", or "lame:<samples>"
}

type mpegHeader struct {
	version    int
	lsf        int
	layer      int
	sampleRate int
	bitrate    int // kbps
	channels   int
	frameSize  int
	spf        int
	sideInfo   int
}

func decodeMPEGHeader(h uint32) (mpegHeader, bool) {
	if h&0xffe00000 != 0xffe00000 {
		return mpegHeader{}, false
	}
	verBits := (h >> 19) & 3
	if verBits == 1 {
		return mpegHeader{}, false
	}
	layerBits := (h >> 17) & 3
	if layerBits == 0 {
		return mpegHeader{}, false
	}
	bitrateIdx := (h >> 12) & 0xf
	if bitrateIdx == 0 || bitrateIdx == 0xf {
		return mpegHeader{}, false
	}
	srIdx := (h >> 10) & 3
	if srIdx == 3 {
		return mpegHeader{}, false
	}

	var m mpegHeader
	var mpeg25 int
	switch verBits {
	case 3:
		m.version, m.lsf, mpeg25 = 1, 0, 0
	case 2:
		m.version, m.lsf, mpeg25 = 2, 1, 0
	default:
		m.version, m.lsf, mpeg25 = 25, 1, 1
	}
	m.layer = int(4 - layerBits)
	m.sampleRate = mpaFreqTab[srIdx] >> (m.lsf + mpeg25)
	if m.sampleRate <= 0 {
		return mpegHeader{}, false
	}
	padding := int((h >> 9) & 1)
	if (h>>6)&3 == 3 {
		m.channels = 1
	} else {
		m.channels = 2
	}
	m.bitrate = mpaBitrateTab[m.lsf][m.layer-1][bitrateIdx]
	if m.bitrate == 0 {
		return mpegHeader{}, false
	}
	switch m.layer {
	case 1:
		m.frameSize = ((m.bitrate*12000)/m.sampleRate + padding) * 4
		m.spf = 384
	case 2:
		m.frameSize = (m.bitrate*144000)/m.sampleRate + padding
		m.spf = 1152
	default:
		m.frameSize = (m.bitrate*144000)/(m.sampleRate<<m.lsf) + padding
		if m.lsf == 0 {
			m.spf = 1152
		} else {
			m.spf = 576
		}
	}
	if m.frameSize < 4 {
		return mpegHeader{}, false
	}
	if m.lsf == 0 {
		if m.channels == 1 {
			m.sideInfo = 17
		} else {
			m.sideInfo = 32
		}
	} else {
		if m.channels == 1 {
			m.sideInfo = 9
		} else {
			m.sideInfo = 17
		}
	}
	return m, true
}

// OpenMP3 inspects the MP3 header at path without decoding audio frames.
func OpenMP3(path string) (MP3Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return MP3Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return MP3Info{}, err
	}
	return ReadMP3(f, st.Size())
}

// PeekMP3StartOffset inspects the first post-ID3 MPEG frame in br without
// consuming bytes and returns the number of leading decoded PCM samples
// FFmpeg/Engine trims (Xing/Info frame samples plus LAME/Lavf/Lavc start_pad + 529).
func PeekMP3StartOffset(br *bufio.Reader) int {
	hdr, _ := br.Peek(4)
	if len(hdr) < 4 {
		return 0
	}
	f, ok := decodeMPEGHeader(binary.BigEndian.Uint32(hdr))
	if !ok || f.layer != 3 {
		return 0
	}
	frame, _ := br.Peek(f.frameSize)
	if len(frame) < f.frameSize {
		return 0
	}
	p := 4 + f.sideInfo
	if p+8 > len(frame) {
		return 0
	}
	tag := string(frame[p : p+4])
	if tag != "Xing" && tag != "Info" {
		return 0
	}
	flags := binary.BigEndian.Uint32(frame[p+4 : p+8])
	p += 8
	var frames, headerFilesize uint32
	fieldsOK := true
	if flags&0x01 != 0 {
		if p+4 <= len(frame) {
			frames = binary.BigEndian.Uint32(frame[p : p+4])
			p += 4
		} else {
			fieldsOK = false
		}
	}
	if fieldsOK && flags&0x02 != 0 {
		if p+4 <= len(frame) {
			headerFilesize = binary.BigEndian.Uint32(frame[p : p+4])
			p += 4
		} else {
			fieldsOK = false
		}
	}
	if fieldsOK && flags&0x04 != 0 {
		if p+100 <= len(frame) {
			p += 100
		} else {
			fieldsOK = false
		}
	}
	if fieldsOK && flags&0x08 != 0 {
		if p+4 <= len(frame) {
			p += 4
		} else {
			fieldsOK = false
		}
	}
	lameSkip := 0
	if fieldsOK && p+24 <= len(frame) {
		prefix := string(frame[p : p+4])
		if prefix == "LAME" || prefix == "Lavf" || prefix == "Lavc" {
			v := uint32(frame[p+21])<<16 | uint32(frame[p+22])<<8 | uint32(frame[p+23])
			lameSkip = int(v>>12) + lameDecoderPad
		}
	}
	if frames == 0 && headerFilesize == 0 {
		return lameSkip
	}
	return f.spf + lameSkip
}

// ReadMP3 skips any leading ID3v2 tags, parses a leading Layer III Xing/Info +
// LAME tag frame using FFmpeg 7.1's mp3_parse_info_tag rules, and validates the
// audio stream header. Encoder names in ID3 tags or audio ancillary data are
// ignored when no Xing/Info tag frame is present.
func ReadMP3(r io.ReadSeeker, size int64) (MP3Info, error) {
	if size < 4 {
		return MP3Info{}, errors.New("mp3: file too short")
	}
	off, err := SkipID3Seeker(r, size)
	if err != nil {
		return MP3Info{}, err
	}
	if size-off < 4 {
		return MP3Info{}, errors.New("mp3: missing MPEG frame")
	}

	var (
		info     MP3Info
		vbrFrame mpegHeader
		xingSkip int
		lameSkip int
		vbrKbps  int
		frames   uint32
	)

	// Match FFmpeg's mp3_read_header -> mp3_parse_vbr_tags at exact post-ID3 offset.
	var firstHdr [4]byte
	if _, err := io.ReadFull(r, firstHdr[:]); err != nil {
		return MP3Info{}, err
	}
	if f, ok := decodeMPEGHeader(binary.BigEndian.Uint32(firstHdr[:])); ok && f.layer == 3 && off+int64(f.frameSize) <= size {
		frame := make([]byte, f.frameSize)
		copy(frame, firstHdr[:])
		if _, err := io.ReadFull(r, frame[4:]); err != nil {
			return MP3Info{}, err
		}
		p := 4 + f.sideInfo
		if p+8 <= len(frame) && (string(frame[p:p+4]) == "Xing" || string(frame[p:p+4]) == "Info") {
			tag := string(frame[p : p+4])
			isCBR := tag == "Info"
			flags := binary.BigEndian.Uint32(frame[p+4 : p+8])
			p += 8
			var headerFilesize uint32
			fieldsOK := true
			if flags&0x01 != 0 {
				if p+4 <= len(frame) {
					frames = binary.BigEndian.Uint32(frame[p : p+4])
					p += 4
				} else {
					fieldsOK = false
				}
			}
			if fieldsOK && flags&0x02 != 0 {
				if p+4 <= len(frame) {
					headerFilesize = binary.BigEndian.Uint32(frame[p : p+4])
					p += 4
				} else {
					fieldsOK = false
				}
			}
			fsize := uint64(size - off)
			if fsize > 0 && headerFilesize > 0 {
				lo := min(fsize, uint64(headerFilesize))
				delta := max(fsize, uint64(headerFilesize)) - lo
				if fsize > uint64(headerFilesize) && delta > lo>>4 {
					frames = 0
				}
			}
			if fieldsOK && flags&0x04 != 0 {
				if p+100 <= len(frame) {
					p += 100
				} else {
					fieldsOK = false
				}
			}
			if fieldsOK && flags&0x08 != 0 {
				if p+4 <= len(frame) {
					p += 4
				} else {
					fieldsOK = false
				}
			}
			if fieldsOK && p+24 <= len(frame) {
				ver := frame[p : p+9]
				info.Encoder = strings.TrimRight(string(ver), "\x00")
				prefix := string(ver[:4])
				if prefix == "LAME" || prefix == "Lavf" || prefix == "Lavc" {
					v := uint32(frame[p+21])<<16 | uint32(frame[p+22])<<8 | uint32(frame[p+23])
					info.EncoderDelay = int(v >> 12)
					info.EndPadding = int(v & 0xfff)
					info.LAMEApplied = true
					lameSkip = info.EncoderDelay + lameDecoderPad
				}
			}
			if frames != 0 || headerFilesize != 0 {
				info.HasXing = true
				info.Tag = tag
				vbrFrame = f
				xingSkip = f.spf
				off += int64(f.frameSize)
				if !isCBR && frames != 0 {
					audioBytes := uint64(headerFilesize)
					if audioBytes == 0 && size > off {
						audioBytes = uint64(size - off)
					}
					if audioBytes > 0 {
						denom := uint64(frames) * uint64(f.spf) * 1000
						vbrKbps = int((audioBytes*8*uint64(f.sampleRate) + denom/2) / denom)
					}
				}
			}
		} else if len(frame) >= 36+26 && string(frame[36:40]) == "VBRI" {
			info.Tag = "VBRI"
			vbrFrame = f
			vbriBytes := binary.BigEndian.Uint32(frame[46:50])
			frames = binary.BigEndian.Uint32(frame[50:54])
			off += int64(f.frameSize)
			audioBytes := uint64(vbriBytes)
			if audioBytes == 0 && size > off {
				audioBytes = uint64(size - off)
			}
			if frames != 0 && audioBytes > 0 {
				denom := uint64(frames) * uint64(f.spf) * 1000
				vbrKbps = int((audioBytes*8*uint64(f.sampleRate) + denom/2) / denom)
			}
		}
	}

	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return MP3Info{}, err
	}
	rem := size - off
	if rem == 0 && (info.HasXing || info.Tag == "VBRI") {
		return finalizeMP3Info(info, vbrFrame, vbrKbps, xingSkip, lameSkip, frames, size), nil
	}
	winLen := int(min(rem, int64(mp3ScanWindow+mpaMaxFrameSize+4)))
	if winLen < 4 {
		return MP3Info{}, errors.New("mp3: no valid MPEG audio frame")
	}
	win := make([]byte, winLen)
	if _, err := io.ReadFull(r, win); err != nil {
		return MP3Info{}, err
	}

	for i := 0; i+4 <= len(win) && i < mp3ScanWindow; i++ {
		h1 := binary.BigEndian.Uint32(win[i : i+4])
		f1, ok := decodeMPEGHeader(h1)
		if !ok {
			continue
		}
		next := i + f1.frameSize
		if next+4 <= len(win) {
			if i == 0 && off+int64(next)+128 == size && string(win[next:next+3]) == "TAG" {
				return finalizeMP3Info(info, f1, vbrKbps, xingSkip, lameSkip, frames, size), nil
			}
			h2 := binary.BigEndian.Uint32(win[next : next+4])
			if _, ok2 := decodeMPEGHeader(h2); ok2 && (h1&mp3Mask) == (h2&mp3Mask) {
				return finalizeMP3Info(info, f1, vbrKbps, xingSkip, lameSkip, frames, size), nil
			}
		} else if i == 0 && off+int64(next) == size {
			return finalizeMP3Info(info, f1, vbrKbps, xingSkip, lameSkip, frames, size), nil
		}
	}
	return MP3Info{}, errors.New("mp3: failed to find valid MPEG audio frames")
}

func finalizeMP3Info(info MP3Info, audio mpegHeader, vbrKbps, xingSkip, lameSkip int, frames uint32, size int64) MP3Info {
	info.SampleRate = audio.sampleRate
	info.Channels = audio.channels
	info.Bitrate = audio.bitrate
	if vbrKbps > 0 {
		info.Bitrate = vbrKbps
	}
	info.MPEGVersion = audio.version
	info.Layer = audio.layer
	info.SamplesPerFrame = audio.spf
	if frames > 0 && audio.sampleRate > 0 {
		if (size > 0 && uint64(frames) > uint64(size)) || uint64(frames)*uint64(audio.spf)/uint64(audio.sampleRate) > 1e7 {
			frames = 0
		} else {
			info.Samples = uint64(frames) * uint64(audio.spf)
			info.Duration = float64(info.Samples) / float64(audio.sampleRate)
		}
	}
	info.StartOffsetSamples = xingSkip + lameSkip
	if info.StartOffsetSamples > 0 {
		if info.LAMEApplied {
			info.OffsetClass = fmt.Sprintf("lame:%d", info.StartOffsetSamples)
		} else if info.HasXing {
			info.OffsetClass = fmt.Sprintf("xing:%d", info.StartOffsetSamples)
		}
	}
	return info
}
