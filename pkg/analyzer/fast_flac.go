package analyzer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"

	"github.com/Cd1s/EngineX-OS/internal/audioheader"
)

var errFastFLACFallback = errors.New("analyzer: fast flac fallback")

var (
	flacBlockSizes = [16]int{0, 192, 576, 1152, 2304, 4608, 0, 0, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}
	flacSampleRate = [16]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000, 0, 0, 0, 0}
	flacBitDepth   = [8]int{0, 8, 12, 0, 16, 20, 24, 0}
)

type flacBitReader struct {
	r   io.Reader
	buf []byte
	pos int
	end int
	acc uint64
	nb  int
	eof bool
}

func newFLACBitReader(r io.Reader) *flacBitReader {
	return &flacBitReader{r: r, buf: make([]byte, 1<<16)}
}

func (b *flacBitReader) refill() error {
	if b.pos < b.end {
		n := copy(b.buf, b.buf[b.pos:b.end])
		b.pos = 0
		b.end = n
	} else {
		b.pos = 0
		b.end = 0
	}
	for b.end < len(b.buf) {
		n, err := b.r.Read(b.buf[b.end:])
		b.end += n
		if err != nil {
			if err == io.EOF {
				b.eof = true
				break
			}
			return err
		}
		if b.end >= 4096 {
			break
		}
	}
	return nil
}

func (b *flacBitReader) ensure(n int) error {
	if b.nb >= n {
		return nil
	}
	if b.end-b.pos < 8 && !b.eof {
		if err := b.refill(); err != nil {
			return err
		}
	}
	for b.nb <= 56 && b.pos < b.end {
		b.acc = (b.acc << 8) | uint64(b.buf[b.pos])
		b.pos++
		b.nb += 8
	}
	if b.nb < n {
		return io.EOF
	}
	return nil
}

func (b *flacBitReader) readBits(n int) (uint32, error) {
	if n == 0 {
		return 0, nil
	}
	if b.nb < n {
		if err := b.ensure(n); err != nil {
			return 0, err
		}
	}
	b.nb -= n
	return uint32((b.acc >> uint(b.nb)) & ((uint64(1) << uint(n)) - 1)), nil
}

func (b *flacBitReader) readSigned(n int) (int32, error) {
	u, err := b.readBits(n)
	if err != nil {
		return 0, err
	}
	shift := uint(32 - n)
	return int32(u<<shift) >> shift, nil
}

func (b *flacBitReader) alignByte() {
	b.nb &= ^7
}

func (b *flacBitReader) readBytes(dst []byte) error {
	b.alignByte()
	i := 0
	for i < len(dst) && b.nb >= 8 {
		b.nb -= 8
		dst[i] = byte(b.acc >> uint(b.nb))
		i++
	}
	for i < len(dst) {
		if b.pos >= b.end {
			if b.eof {
				return io.ErrUnexpectedEOF
			}
			if err := b.refill(); err != nil {
				return err
			}
			if b.pos >= b.end {
				return io.ErrUnexpectedEOF
			}
		}
		n := copy(dst[i:], b.buf[b.pos:b.end])
		b.pos += n
		i += n
	}
	return nil
}

func (b *flacBitReader) skipBytes(n int) error {
	b.alignByte()
	for n > 0 && b.nb >= 8 {
		b.nb -= 8
		n--
	}
	for n > 0 {
		if b.pos >= b.end {
			if b.eof {
				return io.ErrUnexpectedEOF
			}
			if err := b.refill(); err != nil {
				return err
			}
			if b.pos >= b.end {
				return io.ErrUnexpectedEOF
			}
		}
		k := b.end - b.pos
		if k > n {
			k = n
		}
		b.pos += k
		n -= k
	}
	return nil
}

func (b *flacBitReader) readUTF8() (uint64, error) {
	first, err := b.readBits(8)
	if err != nil {
		return 0, err
	}
	if first < 0x80 {
		return uint64(first), nil
	}
	ones := bits.LeadingZeros8(^uint8(first))
	if ones < 2 || ones > 7 {
		return 0, errors.New("analyzer: flac: invalid frame number")
	}
	val := uint64(first & ((1 << uint(7-ones)) - 1))
	for i := 1; i < ones; i++ {
		c, err := b.readBits(8)
		if err != nil {
			return 0, err
		}
		if c&0xc0 != 0x80 {
			return 0, errors.New("analyzer: flac: invalid utf8 frame number")
		}
		val = (val << 6) | uint64(c&0x3f)
	}
	return val, nil
}

func (b *flacBitReader) decodeResidual(out []int32, order, blockSize int) error {
	method, err := b.readBits(2)
	if err != nil {
		return err
	}
	if method > 1 {
		return errFastFLACFallback
	}
	pOrder, err := b.readBits(4)
	if err != nil {
		return err
	}
	paramBits := 4
	esc := uint32(15)
	if method == 1 {
		paramBits = 5
		esc = 31
	}
	nPart := 1 << pOrder
	if nPart > blockSize || blockSize&(nPart-1) != 0 {
		return errors.New("analyzer: flac: invalid residual partition")
	}
	partLen := blockSize >> pOrder
	idx := order
	for p := 0; p < nPart; p++ {
		nSamp := partLen
		if p == 0 {
			nSamp -= order
			if nSamp < 0 {
				return errors.New("analyzer: flac: negative partition length")
			}
		}
		k, err := b.readBits(paramBits)
		if err != nil {
			return err
		}
		if k == esc {
			rawBits, err := b.readBits(5)
			if err != nil {
				return err
			}
			for j := 0; j < nSamp; j++ {
				if rawBits == 0 {
					out[idx] = 0
				} else {
					v, err := b.readSigned(int(rawBits))
					if err != nil {
						return err
					}
					out[idx] = v
				}
				idx++
			}
			continue
		}
		ku := uint(k)
		if ku > 30 {
			return errFastFLACFallback
		}
		mask := (uint64(1) << ku) - 1
		nb := b.nb
		acc := b.acc
		pos := b.pos
		end := b.end
		buf := b.buf
		for j := 0; j < nSamp; j++ {
			var q uint32
			for {
				if nb < 32 && pos+4 <= end {
					acc = (acc << 32) | uint64(binary.BigEndian.Uint32(buf[pos:]))
					pos += 4
					nb += 32
				} else if nb == 0 {
					b.nb, b.acc, b.pos = nb, acc, pos
					if err := b.ensure(1); err != nil {
						return err
					}
					nb, acc, pos, end = b.nb, b.acc, b.pos, b.end
				}
				top := acc << uint(64-nb)
				lz := bits.LeadingZeros64(top)
				if lz < nb {
					q += uint32(lz)
					nb -= lz + 1
					break
				}
				q += uint32(nb)
				nb = 0
			}
			if ku > 0 {
				if nb < int(ku) {
					if nb < 32 && pos+4 <= end {
						acc = (acc << 32) | uint64(binary.BigEndian.Uint32(buf[pos:]))
						pos += 4
						nb += 32
					}
					if nb < int(ku) {
						b.nb, b.acc, b.pos = nb, acc, pos
						if err := b.ensure(int(ku)); err != nil {
							return err
						}
						nb, acc, pos, end = b.nb, b.acc, b.pos, b.end
					}
				}
				nb -= int(ku)
			}
			u := (q << ku) | uint32((acc>>uint(nb))&mask)
			out[idx] = int32(u>>1) ^ -int32(u&1)
			idx++
		}
		b.nb, b.acc, b.pos = nb, acc, pos
	}
	return nil
}

// computeFLACPeaksFast decodes a FLAC stream directly into a single-pass peak
// bank with O(1) per-frame allocations. Returns errFastFLACFallback for rare
// streams outside the fast subset (e.g. unknown NSamples or >24-bit side channels)
// so callers can fall back to mewkiz/flac.
func computeFLACPeaksFast(r io.Reader, ticker Ticker) (*OverviewPeaks, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	b := newFLACBitReader(br)
	var magic [4]byte
	if err := b.readBytes(magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, errors.New("analyzer: flac: missing fLaC magic")
	}

	var (
		sr, ch, bps int
		total       int64
		haveInfo    bool
	)
	for {
		var hdr [4]byte
		if err := b.readBytes(hdr[:]); err != nil {
			return nil, err
		}
		last := (hdr[0] & 0x80) != 0
		typ := hdr[0] & 0x7f
		blen := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if typ == 0 {
			if blen != 34 {
				return nil, errors.New("analyzer: flac: bad STREAMINFO size")
			}
			var si [34]byte
			if err := b.readBytes(si[:]); err != nil {
				return nil, err
			}
			sr = int(si[10])<<12 | int(si[11])<<4 | int(si[12]>>4)
			ch = int((si[12]>>1)&7) + 1
			bps = int((si[12]&1)<<4|(si[13]>>4)) + 1
			total = int64(si[13]&0x0f)<<32 | int64(binary.BigEndian.Uint32(si[14:18]))
			haveInfo = true
		} else if err := b.skipBytes(blen); err != nil {
			return nil, err
		}
		if last {
			break
		}
	}
	if !haveInfo || ch < 1 || ch > 2 || bps < 8 || bps > 31 || sr < 8000 || sr > 384000 {
		return nil, errFastFLACFallback
	}
	if total <= 0 {
		return nil, errFastFLACFallback
	}

	bank, err := newPeakFilterBank(sr, ch, bps, total)
	if err != nil {
		return nil, err
	}
	var sub [2][]int32
	for c := 0; c < ch; c++ {
		sub[c] = make([]int32, 65536)
	}
	var coef [32]int32

	for {
		if ticker != nil {
			if err := ticker.Tick(); err != nil {
				return nil, err
			}
		}
		b.alignByte()
		sync, err := b.readBits(16)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if sync&0xfffe != 0xfff8 {
			return nil, errFastFLACFallback
		}
		b2, err := b.readBits(8)
		if err != nil {
			return nil, err
		}
		b3, err := b.readBits(8)
		if err != nil {
			return nil, err
		}
		if b3&1 != 0 {
			return nil, errFastFLACFallback
		}
		bsCode := b2 >> 4
		srCode := b2 & 0xf
		chAss := b3 >> 4
		ssCode := (b3 >> 1) & 7
		if bsCode == 0 || srCode == 15 || chAss >= 11 {
			return nil, errFastFLACFallback
		}
		if _, err := b.readUTF8(); err != nil {
			return nil, err
		}
		bs := flacBlockSizes[bsCode]
		if bsCode == 6 {
			v, err := b.readBits(8)
			if err != nil {
				return nil, err
			}
			bs = int(v) + 1
		} else if bsCode == 7 {
			v, err := b.readBits(16)
			if err != nil {
				return nil, err
			}
			bs = int(v) + 1
		}
		if bs <= 0 || bs > 65536 {
			return nil, errFastFLACFallback
		}
		frameSR := sr
		switch {
		case srCode >= 1 && srCode <= 11:
			frameSR = flacSampleRate[srCode]
		case srCode == 12:
			v, err := b.readBits(8)
			if err != nil {
				return nil, err
			}
			frameSR = int(v) * 1000
		case srCode == 13:
			v, err := b.readBits(16)
			if err != nil {
				return nil, err
			}
			frameSR = int(v)
		case srCode == 14:
			v, err := b.readBits(16)
			if err != nil {
				return nil, err
			}
			frameSR = int(v) * 10
		}
		if frameSR != sr {
			return nil, errors.New("analyzer: flac: sample rate changed mid-stream")
		}
		if _, err := b.readBits(8); err != nil { // frame CRC-8
			return nil, err
		}
		fBPS := flacBitDepth[ssCode]
		if ssCode == 0 {
			fBPS = bps
		}
		if fBPS != bps {
			return nil, errors.New("analyzer: flac: bit depth changed mid-stream")
		}
		nCh := int(chAss) + 1
		if chAss >= 8 {
			nCh = 2
		}
		if nCh != ch {
			return nil, errors.New("analyzer: flac: channel count changed mid-stream")
		}

		for c := 0; c < nCh; c++ {
			sbps := fBPS
			if (chAss == 8 && c == 1) || (chAss == 9 && c == 0) || (chAss == 10 && c == 1) {
				sbps++
			}
			if sbps > 31 {
				return nil, errFastFLACFallback
			}
			sh, err := b.readBits(8)
			if err != nil {
				return nil, err
			}
			wasted := 0
			if sh&1 != 0 {
				wasted = 1
				for {
					bit, err := b.readBits(1)
					if err != nil {
						return nil, err
					}
					if bit == 1 {
						break
					}
					wasted++
					if wasted >= sbps {
						return nil, errFastFLACFallback
					}
				}
				sbps -= wasted
			}
			styp := (sh >> 1) & 0x3f
			out := sub[c][:bs]
			switch {
			case styp == 0: // CONSTANT
				v, err := b.readSigned(sbps)
				if err != nil {
					return nil, err
				}
				for i := range out {
					out[i] = v
				}
			case styp == 1: // VERBATIM
				for i := range out {
					v, err := b.readSigned(sbps)
					if err != nil {
						return nil, err
					}
					out[i] = v
				}
			case styp >= 8 && styp <= 12: // FIXED
				ord := int(styp - 8)
				if ord > bs {
					return nil, errors.New("analyzer: flac: fixed order exceeds block")
				}
				for i := 0; i < ord; i++ {
					v, err := b.readSigned(sbps)
					if err != nil {
						return nil, err
					}
					out[i] = v
				}
				if err := b.decodeResidual(out, ord, bs); err != nil {
					return nil, err
				}
				switch ord {
				case 1:
					for i := 1; i < bs; i++ {
						out[i] += out[i-1]
					}
				case 2:
					for i := 2; i < bs; i++ {
						out[i] += 2*out[i-1] - out[i-2]
					}
				case 3:
					for i := 3; i < bs; i++ {
						out[i] += 3*out[i-1] - 3*out[i-2] + out[i-3]
					}
				case 4:
					for i := 4; i < bs; i++ {
						out[i] += 4*out[i-1] - 6*out[i-2] + 4*out[i-3] - out[i-4]
					}
				}
			case styp >= 32: // LPC
				ord := int(styp-32) + 1
				if ord > bs || ord > 32 {
					return nil, errors.New("analyzer: flac: lpc order exceeds block")
				}
				for i := 0; i < ord; i++ {
					v, err := b.readSigned(sbps)
					if err != nil {
						return nil, err
					}
					out[i] = v
				}
				precBits, err := b.readBits(4)
				if err != nil {
					return nil, err
				}
				if precBits == 15 {
					return nil, errors.New("analyzer: flac: invalid lpc precision")
				}
				prec := int(precBits) + 1
				shift, err := b.readSigned(5)
				if err != nil {
					return nil, err
				}
				if shift < 0 {
					return nil, errFastFLACFallback
				}
				for j := 0; j < ord; j++ {
					v, err := b.readSigned(prec)
					if err != nil {
						return nil, err
					}
					coef[j] = v
				}
				if err := b.decodeResidual(out, ord, bs); err != nil {
					return nil, err
				}
				shU := uint(shift)
				if sbps+prec+bits.Len(uint(ord)) <= 31 {
					for i := ord; i < bs; i++ {
						var sum int32
						base := out[i-ord : i]
						for j := 0; j < ord; j++ {
							sum += coef[j] * base[ord-1-j]
						}
						out[i] += sum >> shU
					}
				} else {
					for i := ord; i < bs; i++ {
						var sum int64
						for j := 0; j < ord; j++ {
							sum += int64(coef[j]) * int64(out[i-1-j])
						}
						out[i] += int32(sum >> shU)
					}
				}
			default:
				return nil, errFastFLACFallback
			}
			if wasted > 0 {
				wu := uint(wasted)
				for i := range out {
					out[i] <<= wu
				}
			}
		}

		if nCh == 2 {
			s0, s1 := sub[0][:bs], sub[1][:bs]
			switch chAss {
			case 8: // L/S
				for i := 0; i < bs; i++ {
					s1[i] = s0[i] - s1[i]
				}
			case 9: // S/R
				for i := 0; i < bs; i++ {
					s0[i] = s0[i] + s1[i]
				}
			case 10: // M/S
				for i := 0; i < bs; i++ {
					side := s1[i]
					mid := (s0[i] << 1) | (side & 1)
					s0[i] = (mid + side) >> 1
					s1[i] = (mid - side) >> 1
				}
			}
			bank.feedInt32(s0, s1)
		} else {
			bank.feedInt32(sub[0][:bs], nil)
		}

		b.alignByte()
		if _, err := b.readBits(16); err != nil { // frame CRC-16
			return nil, err
		}
	}

	if bank.sampleIdx != total {
		return nil, errFastFLACFallback
	}
	return bank.finish("flac")
}
