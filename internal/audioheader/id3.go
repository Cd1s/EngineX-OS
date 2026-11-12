// Package audioheader shares container-prefix handling between inspection and
// decoding so that both pipelines see precisely the same first audio byte.
package audioheader

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

func parseID3Header(h [10]byte) (int64, bool, error) {
	if h[3] < 2 || h[3] > 4 {
		return 0, false, errors.New("audio: unsupported ID3 version")
	}
	for _, b := range h[6:10] {
		if b&128 != 0 {
			return 0, false, errors.New("audio: invalid ID3 size")
		}
	}
	n := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
	if n > 16<<20 {
		return 0, false, errors.New("audio: ID3 too large")
	}
	if h[5]&16 != 0 {
		if h[3] != 4 {
			return 0, false, errors.New("audio: invalid ID3 footer flag")
		}
		return n, true, nil
	}
	return n, false, nil
}

func checkID3Footer(r io.Reader, h [10]byte) error {
	var footer [10]byte
	if _, err := io.ReadFull(r, footer[:]); err != nil {
		return err
	}
	if string(footer[:3]) != "3DI" || !bytes.Equal(footer[3:], h[3:]) {
		return errors.New("audio: invalid ID3 footer")
	}
	return nil
}

const (
	maxID3Tags    = 8
	maxID3Padding = 64 << 10
)

func SkipID3(r *bufio.Reader) error {
	var padded int
	for count := 0; ; count++ {
		if count > 0 {
			for {
				b, err := r.Peek(1)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if b[0] != 0 {
					break
				}
				if padded >= maxID3Padding {
					return errors.New("audio: excessive post-ID3 padding")
				}
				_, _ = r.ReadByte()
				padded++
			}
		}
		prefix, err := r.Peek(3)
		if err != nil {
			if count == 0 {
				return err
			}
			if err == io.EOF {
				if len(prefix) > 0 && bytes.HasPrefix([]byte("ID3"), prefix) {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
		if string(prefix) != "ID3" {
			return nil
		}
		if count >= maxID3Tags {
			return errors.New("audio: too many ID3 headers")
		}
		var h [10]byte
		if _, err = io.ReadFull(r, h[:]); err != nil {
			return err
		}
		n, footer, err := parseID3Header(h)
		if err != nil {
			return err
		}
		if _, err = io.CopyN(io.Discard, r, n); err != nil {
			return err
		}
		if footer {
			if err := checkID3Footer(r, h); err != nil {
				return err
			}
		}
	}
}

// skipID3Seeker skips zero or more consecutive validated ID3v2 tags (plus
// bounded post-tag NUL padding) at the start of r using Seek so large embedded
// pictures are never read into memory.
func skipID3Seeker(r io.ReadSeeker, fileSize int64) (int64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	var off int64
	var padded int
	var zbuf [512]byte
	for count := 0; ; count++ {
		if count > 0 && off < fileSize {
			for off < fileSize {
				toRead := int(min(int64(len(zbuf)), fileSize-off))
				if _, err := r.Seek(off, io.SeekStart); err != nil {
					return 0, err
				}
				if _, err := io.ReadFull(r, zbuf[:toRead]); err != nil {
					return 0, err
				}
				nz := 0
				for nz < toRead && zbuf[nz] == 0 {
					nz++
				}
				if padded+nz > maxID3Padding {
					return 0, errors.New("audio: excessive post-ID3 padding")
				}
				padded += nz
				off += int64(nz)
				if nz < toRead {
					break
				}
			}
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return 0, err
			}
		}
		if fileSize-off < 3 {
			break
		}
		var h [10]byte
		readLen := int(min(int64(len(h)), fileSize-off))
		if _, err := io.ReadFull(r, h[:readLen]); err != nil {
			return 0, err
		}
		if string(h[:3]) != "ID3" {
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return 0, err
			}
			break
		}
		if count >= maxID3Tags {
			return 0, errors.New("audio: too many ID3 headers")
		}
		if readLen < 10 {
			return 0, io.ErrUnexpectedEOF
		}
		n, footer, err := parseID3Header(h)
		if err != nil {
			return 0, err
		}
		off += 10 + n
		if off > fileSize {
			return 0, io.ErrUnexpectedEOF
		}
		if _, err = r.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		if footer {
			if fileSize-off < 10 {
				return 0, io.ErrUnexpectedEOF
			}
			if err = checkID3Footer(r, h); err != nil {
				return 0, err
			}
			off += 10
		}
	}
	return off, nil
}
