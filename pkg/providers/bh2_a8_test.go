package providers

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := int(m.pos) + len(p)
	if end > len(m.buf) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:end], p)
	m.pos = int64(end)
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = m.pos + offset
	case io.SeekEnd:
		next = int64(len(m.buf)) + offset
	}
	m.pos = next
	return next, nil
}

func TestBeatportAACWriter96kHzDoesNotOverflowMP4ABox(t *testing.T) {
	// ADTS frame with sampling_frequency_index = 0 (96000 Hz), stereo, length = 8 bytes.
	const frameLen = 8
	adts96k := []byte{
		0xff, 0xf1,
		(1 << 6) | (0 << 2),
		(2 << 6) | byte(frameLen>>11),
		byte(frameLen >> 3),
		byte((frameLen&7)<<5) | 0x1f,
		0xfc,
		0x42,
	}

	out := &memWriteSeeker{}
	w := &aacWriter{w: out}
	if err := w.start(); err != nil {
		t.Fatal(err)
	}
	if rem, err := w.addADTS(adts96k); err != nil || len(rem) != 0 {
		t.Fatalf("addADTS: rem=%d err=%v", len(rem), err)
	}
	if err := w.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	idx := bytes.Index(out.buf, []byte("mp4a"))
	if idx < 4 || idx+32 > len(out.buf) {
		t.Fatalf("mp4a box not found in %d bytes", len(out.buf))
	}
	// mp4a layout: size(4) + "mp4a"(4) + reserved(6) + data_ref_idx(2) + reserved(8) + channels(2) + samplesize(2) + pre_defined(2) + reserved(2) + samplerate_16_16(4)
	srFixed := binary.BigEndian.Uint32(out.buf[idx+4+6+2+8+8 : idx+4+6+2+8+8+4])
	if srFixed == uint32(96000<<16&0xffffffff) || srFixed != 0 {
		t.Fatalf("mp4a 16.16 samplerate overflowed to 0x%08x (%d Hz), want 0 for 96 kHz", srFixed, srFixed>>16)
	}
}
