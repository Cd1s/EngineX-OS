package compat

import (
	"encoding/binary"
	"io"
	"math/bits"
	"strconv"
	"strings"
)

type mp4AudioBox struct {
	name      string
	data, end int64
}

func IsMP4AudioPath(path string) bool {
	path = strings.ToLower(path)
	return strings.HasSuffix(path, ".m4a") || strings.HasSuffix(path, ".mp4")
}

func MP4AudioHeaderPrefix(r io.ReaderAt, size int64) (AudioInfo, error) {
	return mp4AudioHeader(r, 0, size)
}

// Walk only the requested children. Payloads (notably mdat and cover art) are
// skipped, and declared box bounds never extend outside the parent.
func mp4AudioFind(r io.ReaderAt, parent mp4AudioBox, name string) mp4AudioBox {
	for pos, n := parent.data, 0; pos <= parent.end-8 && n < 4096; n++ {
		var b [16]byte
		if _, err := r.ReadAt(b[:8], pos); err != nil {
			break
		}
		size, head := uint64(binary.BigEndian.Uint32(b[:4])), int64(8)
		switch size {
		case 0:
			size = uint64(parent.end - pos)
		case 1:
			if pos > parent.end-16 {
				return mp4AudioBox{}
			}
			if _, err := r.ReadAt(b[8:], pos+8); err != nil {
				return mp4AudioBox{}
			}
			size, head = binary.BigEndian.Uint64(b[8:]), 16
		}
		if size < uint64(head) || size > uint64(parent.end-pos) {
			break
		}
		end := pos + int64(size)
		if string(b[4:8]) == name {
			return mp4AudioBox{name, pos + head, end}
		}
		pos = end
	}
	return mp4AudioBox{}
}

func mp4AudioRead(r io.ReaderAt, box mp4AudioBox, off int64, b []byte) bool {
	if off < 0 || off > box.end-box.data || int64(len(b)) > box.end-box.data-off {
		return false
	}
	_, err := r.ReadAt(b, box.data+off)
	return err == nil
}

func mp4AudioHeader(r io.ReaderAt, start, end int64) (AudioInfo, error) {
	moov := mp4AudioFind(r, mp4AudioBox{data: start, end: end}, "moov")
	// A partial moov can contain the sample rate but not the priming tag.
	var last [1]byte
	if moov.name == "" || !mp4AudioRead(r, moov, moov.end-moov.data-1, last[:]) {
		return AudioInfo{}, io.ErrUnexpectedEOF
	}
	for next, n := moov.data, 0; next < moov.end && n < 32; n++ {
		trak := mp4AudioFind(r, mp4AudioBox{data: next, end: moov.end}, "trak")
		if trak.name == "" {
			break
		}
		next = trak.end
		mdia := mp4AudioFind(r, trak, "mdia")
		var typ [4]byte
		if !mp4AudioRead(r, mp4AudioFind(r, mdia, "hdlr"), 8, typ[:]) || string(typ[:]) != "soun" {
			continue
		}
		header := mp4AudioFind(r, mdia, "mdhd")
		var h [32]byte
		if !mp4AudioRead(r, header, 0, h[:20]) {
			continue
		}
		var scale, units uint64
		switch h[0] {
		case 0:
			scale, units = uint64(binary.BigEndian.Uint32(h[12:])), uint64(binary.BigEndian.Uint32(h[16:]))
			if units == 0xffffffff {
				continue
			}
		case 1:
			if !mp4AudioRead(r, header, 0, h[:32]) {
				continue
			}
			scale, units = uint64(binary.BigEndian.Uint32(h[20:])), binary.BigEndian.Uint64(h[24:])
		default:
			continue
		}
		stsd := mp4AudioFind(r, mp4AudioFind(r, mp4AudioFind(r, mdia, "minf"), "stbl"), "stsd")
		if stsd.end-stsd.data < 8 {
			continue
		}
		entry := mp4AudioFind(r, mp4AudioBox{data: stsd.data + 8, end: stsd.end}, "mp4a")
		var a [28]byte
		if !mp4AudioRead(r, entry, 0, a[:]) {
			continue
		}
		channels := int(binary.BigEndian.Uint16(a[16:]))
		rate := uint64(binary.BigEndian.Uint32(a[24:]) >> 16)
		if rate < 8000 || channels == 0 || channels > 16 || scale == 0 || units == 0 {
			continue
		}
		hi, lo := bits.Mul64(units, rate)
		if hi >= scale {
			continue
		}
		samples, rem := bits.Div64(hi, lo, scale)
		if rem != 0 || samples/rate > 1e7 {
			continue
		}
		// iTunes AAC priming is skipped at playback; Engine still counts the
		// trailing AAC frame padding. The grid uses the same decoded timebase.
		prime := mp4AudioPriming(r, moov)
		if samples <= uint64(prime) {
			continue
		}
		samples -= uint64(prime)
		info := AudioInfo{Format: "aac", SampleRate: int(rate), Channels: channels, Samples: samples, Duration: float64(samples) / float64(rate), StartOffsetSamples: prime}
		return info, nil
	}
	return AudioInfo{}, nil
}

func mp4AudioPriming(r io.ReaderAt, moov mp4AudioBox) int {
	meta := mp4AudioFind(r, mp4AudioFind(r, moov, "udta"), "meta")
	if meta.end-meta.data < 4 {
		return 0
	}
	meta.data += 4 // FullBox version and flags.
	ilst := mp4AudioFind(r, meta, "ilst")
	for next, n := ilst.data, 0; next < ilst.end && n < 256; n++ {
		item := mp4AudioFind(r, mp4AudioBox{data: next, end: ilst.end}, "----")
		if item.name == "" {
			break
		}
		next = item.end
		mean, name := mp4AudioFind(r, item, "mean"), mp4AudioFind(r, item, "name")
		var vendor [16]byte
		var tag [8]byte
		if !mp4AudioRead(r, mean, 4, vendor[:]) || string(vendor[:]) != "com.apple.iTunes" ||
			!mp4AudioRead(r, name, 4, tag[:]) || string(tag[:]) != "iTunSMPB" {
			continue
		}
		data := mp4AudioFind(r, item, "data")
		var value [256]byte
		n := data.end - data.data - 8 // type and locale
		if n <= 0 || n > int64(len(value)) || !mp4AudioRead(r, data, 8, value[:n]) {
			continue
		}
		words := strings.Fields(string(value[:n]))
		if len(words) >= 4 {
			if priming, err := strconv.ParseUint(words[1], 16, 16); err == nil && priming < 16384 {
				return int(priming)
			}
		}
	}
	return 0
}
