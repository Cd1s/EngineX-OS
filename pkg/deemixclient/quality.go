package deemixclient

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/Cd1s/sc5000-custom-os/internal/audioheader"
	"io"
	"os"
)

type Quality struct {
	Label      string `json:"label"`
	Format     string `json:"format"`
	Bits       int    `json:"bits"`
	SampleRate int    `json:"sampleRate"`
	Bitrate    int    `json:"bitrate"`
	Lossless   bool   `json:"lossless"`
}

func (q Quality) Comment() string {
	if q.Lossless {
		return fmt.Sprintf("[FLAC] %d-bit/%d Hz", q.Bits, q.SampleRate)
	}
	return fmt.Sprintf("[320k] MP3 %d Hz; FLAC unavailable", q.SampleRate)
}

// Inspect reads the actual container/frame headers, never the extension or
// queue's advertised quality. Non-320k lossy audio is rejected rather than
// receiving a dishonest [320k] label. Full decoding validates frame integrity.
func Inspect(path string) (Quality, error) {
	f, err := os.Open(path)
	if err != nil {
		return Quality{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := audioheader.SkipID3(r); err != nil {
		return Quality{}, err
	}
	head, err := r.Peek(4)
	if err != nil {
		return Quality{}, err
	}
	if string(head) == "fLaC" {
		var b [42]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return Quality{}, err
		}
		if b[4]&127 != 0 || b[5] != 0 || b[6] != 0 || b[7] != 34 {
			return Quality{}, errors.New("quality: invalid FLAC STREAMINFO")
		}
		v := binary.BigEndian.Uint64(b[18:26])
		sr := int(v >> 44)
		bits := int((v>>36)&31) + 1
		if sr < 8000 || sr > 192000 || bits < 8 || bits > 32 {
			return Quality{}, errors.New("quality: invalid FLAC sample format")
		}
		return Quality{Label: "[FLAC]", Format: "flac", Bits: bits, SampleRate: sr, Lossless: true}, nil
	}
	count := 0
	sr := 0
	for {
		h, err := r.Peek(4)
		if err == io.EOF {
			break
		}
		if err != nil {
			return Quality{}, err
		}
		if string(h[:3]) == "TAG" {
			tail, err := io.ReadAll(io.LimitReader(r, 129))
			if err == nil && len(tail) == 128 {
				break
			}
			return Quality{}, errors.New("quality: invalid MP3 tail")
		}
		v := binary.BigEndian.Uint32(h)
		// MPEG1 Layer III, bitrate index 14 = 320 kbit/s.
		if v&0xfffe0000 != 0xfffa0000 || ((v>>12)&15) != 14 {
			return Quality{}, errors.New("quality: expected MPEG1 Layer III MP3 at 320k; refusing downgrade")
		}
		rate := []int{44100, 48000, 32000, 0}[(v>>10)&3]
		if rate == 0 {
			return Quality{}, errors.New("quality: invalid MP3 sample rate")
		}
		if sr != 0 && sr != rate {
			return Quality{}, errors.New("quality: changing MP3 sample rate")
		}
		sr = rate
		size := 144*320000/rate + int((v>>9)&1)
		if _, err = io.CopyN(io.Discard, r, int64(size)); err != nil {
			return Quality{}, errors.New("quality: truncated MP3 frame")
		}
		count++
	}
	if count < 2 {
		return Quality{}, errors.New("quality: no complete audio frames")
	}
	return Quality{Label: "[320k]", Format: "mp3", SampleRate: sr, Bitrate: 320000}, nil
}
