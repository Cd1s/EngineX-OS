package compat

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

type AudioInfo struct {
	SampleRate, Channels, BitDepth, Bitrate int
	Samples                                 uint64
	Duration                                float64
	Format                                  string
}

// AudioHeader visits only container headers. Large FLAC pictures and WAV data
// chunks are skipped with Seek; neither decoded audio nor pictures enter memory.
func AudioHeader(path string) (AudioInfo, error) {
	f, e := os.Open(path)
	if e != nil {
		return AudioInfo{}, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return AudioInfo{}, e
	}
	var b [12]byte
	if _, e = io.ReadFull(f, b[:]); e != nil {
		return AudioInfo{}, e
	}
	switch {
	case string(b[:4]) == "fLaC":
		if _, e = f.Seek(4, 0); e != nil {
			return AudioInfo{}, e
		}
		return flacHeader(f, st.Size())
	case (string(b[:4]) == "RIFF" || string(b[:4]) == "RF64") && string(b[8:]) == "WAVE":
		return wavHeader(f, st.Size())
	default:
		lower := strings.ToLower(path)
		if strings.HasSuffix(lower, ".wav") || strings.HasSuffix(lower, ".wave") || strings.HasSuffix(lower, ".flac") {
			return AudioInfo{}, fmt.Errorf("invalid WAV/FLAC magic")
		}
		return AudioInfo{}, nil // exported metadata remains authoritative for other codecs
	}
}

func wavHeader(f *os.File, size int64) (AudioInfo, error) {
	a := AudioInfo{Format: "wav"}
	var data uint64
	var rfData uint64
	off := int64(12)
	gotFmt, gotData := false, false
	var blockAlign uint64
	for off+8 <= size {
		var h [8]byte
		if _, e := f.ReadAt(h[:], off); e != nil {
			return a, e
		}
		n := uint64(binary.LittleEndian.Uint32(h[4:]))
		name := string(h[:4])
		if name == "data" && n == 0xffffffff && rfData > 0 {
			n = rfData
		}
		if n > uint64(size-off-8) {
			return a, fmt.Errorf("wav: chunk %s exceeds file", name)
		}
		switch name {
		case "ds64":
			if n < 28 {
				return a, fmt.Errorf("wav: short ds64")
			}
			var b [28]byte
			if _, e := f.ReadAt(b[:], off+8); e != nil {
				return a, e
			}
			rfData = binary.LittleEndian.Uint64(b[8:])
		case "fmt ":
			if n < 16 {
				return a, fmt.Errorf("wav: short fmt")
			}
			b := make([]byte, min(n, 64))
			if _, e := f.ReadAt(b, off+8); e != nil {
				return a, e
			}
			tag := binary.LittleEndian.Uint16(b)
			a.Channels = int(binary.LittleEndian.Uint16(b[2:]))
			a.SampleRate = int(binary.LittleEndian.Uint32(b[4:]))
			a.Bitrate = int(uint64(binary.LittleEndian.Uint32(b[8:])) * 8 / 1000)
			a.BitDepth = int(binary.LittleEndian.Uint16(b[14:]))
			blockAlign = uint64(binary.LittleEndian.Uint16(b[12:]))
			if tag == 0xfffe {
				if n < 40 || binary.LittleEndian.Uint16(b[16:]) < 22 {
					return a, fmt.Errorf("wav: short extensible format")
				}
				tag = binary.LittleEndian.Uint16(b[24:])
				if v := int(binary.LittleEndian.Uint16(b[18:])); v > 0 {
					a.BitDepth = v
				}
			}
			if tag != 1 && tag != 3 {
				return a, fmt.Errorf("wav: unsupported format %#x", tag)
			}
			gotFmt = true
		case "data":
			data = n
			gotData = true
		}
		if gotFmt && gotData {
			break
		}
		off += 8 + int64(n) + (int64(n) & 1)
	}
	if !gotFmt || !gotData || a.Channels == 0 || a.SampleRate <= 0 || a.SampleRate > 768000 || a.BitDepth == 0 || blockAlign == 0 {
		return a, fmt.Errorf("wav: missing/invalid fmt or data")
	}
	a.Samples = data / blockAlign
	a.Duration = float64(a.Samples) / float64(a.SampleRate)
	return a, nil
}

func flacHeader(f *os.File, size int64) (AudioInfo, error) {
	a := AudioInfo{Format: "flac"}
	off := int64(4)
	found := false
	for off+4 <= size {
		var h [4]byte
		if _, e := f.ReadAt(h[:], off); e != nil {
			return a, e
		}
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if n > size-off-4 {
			return a, fmt.Errorf("flac: metadata exceeds file")
		}
		if h[0]&0x7f == 0 {
			if n != 34 {
				return a, fmt.Errorf("flac: invalid STREAMINFO")
			}
			var b [34]byte
			if _, e := f.ReadAt(b[:], off+4); e != nil {
				return a, e
			}
			v := binary.BigEndian.Uint64(b[10:])
			a.SampleRate = int(v >> 44)
			a.Channels = int(v>>41&7) + 1
			a.BitDepth = int(v>>36&31) + 1
			a.Samples = v & 0xfffffffff
			if a.SampleRate == 0 {
				return a, fmt.Errorf("flac: zero sample rate")
			}
			a.Duration = float64(a.Samples) / float64(a.SampleRate)
			found = true
		}
		off += 4 + n
		if h[0]&0x80 != 0 {
			if !found {
				return a, fmt.Errorf("flac: missing STREAMINFO")
			}
			if a.Duration > 0 {
				a.Bitrate = int(float64(size-off) * 8 / a.Duration / 1000)
			}
			return a, nil
		}
	}
	return a, fmt.Errorf("flac: truncated metadata %s", strings.TrimSpace(f.Name()))
}
