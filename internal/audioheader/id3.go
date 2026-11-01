// Package audioheader shares container-prefix handling between inspection and
// decoding so that both pipelines see precisely the same first audio byte.
package audioheader

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

func SkipID3(r *bufio.Reader) error {
	prefix, err := r.Peek(3)
	if err != nil {
		return err
	}
	if string(prefix) != "ID3" {
		return nil
	}
	var h [10]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return err
	}
	if h[3] < 2 || h[3] > 4 {
		return errors.New("audio: unsupported ID3 version")
	}
	for _, b := range h[6:10] {
		if b&128 != 0 {
			return errors.New("audio: invalid ID3 size")
		}
	}
	n := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
	if n > 16<<20 {
		return errors.New("audio: ID3 too large")
	}
	if _, err = io.CopyN(io.Discard, r, n); err != nil {
		return err
	}
	if h[5]&16 != 0 {
		if h[3] != 4 {
			return errors.New("audio: invalid ID3 footer flag")
		}
		var footer [10]byte
		if _, err = io.ReadFull(r, footer[:]); err != nil {
			return err
		}
		if string(footer[:3]) != "3DI" || !bytes.Equal(footer[3:], h[3:]) {
			return errors.New("audio: invalid ID3 footer")
		}
	}
	return nil
}
