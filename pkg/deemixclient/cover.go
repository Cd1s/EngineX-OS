package deemixclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxCoverBytes = 2 << 20

// EmbedCover fetches artwork and writes it into a downloaded FLAC or MP3, unless the
// file already has a picture. The file is replaced in one step.
func (c *Client) EmbedCover(ctx context.Context, path, address string) error {
	// By content: a file's name may not say what it holds.
	isFLAC, err := startsWithFLAC(path)
	if err != nil {
		return err
	}
	isMP3 := !isFLAC && strings.HasSuffix(strings.ToLower(path), ".mp3")
	if !isFLAC && !isMP3 {
		return nil
	}
	if has, err := hasPicture(path); err != nil || has {
		return err
	}
	cover, mime, err := c.fetchCover(ctx, address)
	if err != nil {
		return err
	}
	if isFLAC {
		return replaceFile(path, func(in io.Reader, out io.Writer) error { return insertFLACPicture(in, out, cover, mime) })
	}
	return replaceFile(path, func(in io.Reader, out io.Writer) error { return insertAPIC(in, out, cover, mime) })
}

func startsWithFLAC(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false, nil
	}
	return string(head[:]) == "fLaC", nil
}

func (c *Client) fetchCover(ctx context.Context, address string) ([]byte, string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" {
		return nil, "", errors.New("cover: invalid address")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, "", errors.New("cover: invalid address")
	}
	resp, err := c.media().Do(req)
	if err != nil {
		return nil, "", errors.New("cover: transfer failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("cover: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", errors.New("cover: transfer interrupted")
	}
	if len(data) > maxCoverBytes {
		return nil, "", errors.New("cover: too large")
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return data, "image/jpeg", nil
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return data, "image/png", nil
	}
	return nil, "", errors.New("cover: not a JPEG or PNG")
}

func replaceFile(path string, rewrite func(io.Reader, io.Writer) error) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<16)
	err = rewrite(bufio.NewReaderSize(in, 1<<16), w)
	if err == nil {
		err = w.Flush()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), info.Mode().Perm())
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// hasPicture reads only the headers. A file it cannot read counts as having one, so
// nothing it does not understand gets rewritten.
func hasPicture(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	head, err := r.Peek(10)
	if err != nil {
		return true, nil
	}
	if string(head[:4]) == "fLaC" {
		r.Discard(4)
		for {
			var h [4]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return true, nil
			}
			if h[0]&0x7f == 6 {
				return true, nil
			}
			if h[0]&0x80 != 0 {
				return false, nil
			}
			size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
			if _, err := r.Discard(size); err != nil {
				return true, nil
			}
		}
	}
	if string(head[:3]) == "ID3" {
		tag, _, err := readID3(r)
		if err != nil {
			return true, nil
		}
		return bytes.Contains(tag.frames, []byte("APIC")), nil
	}
	return false, nil
}

func pictureBlock(cover []byte, mime string) []byte {
	var b bytes.Buffer
	put := func(v uint32) { binary.Write(&b, binary.BigEndian, v) }
	put(3) // front cover
	put(uint32(len(mime)))
	b.WriteString(mime)
	put(0)        // no description
	for range 4 { // width, height, depth, colours: unknown
		put(0)
	}
	put(uint32(len(cover)))
	b.Write(cover)
	return b.Bytes()
}

// insertFLACPicture adds the picture right after STREAMINFO, which must stay first.
func insertFLACPicture(in io.Reader, out io.Writer, cover []byte, mime string) error {
	var head [8]byte
	if _, err := io.ReadFull(in, head[:]); err != nil {
		return err
	}
	if string(head[:4]) != "fLaC" || head[4]&0x7f != 0 {
		return errors.New("cover: unexpected FLAC layout")
	}
	last := head[4]&0x80 != 0
	info := make([]byte, int(head[5])<<16|int(head[6])<<8|int(head[7]))
	if _, err := io.ReadFull(in, info); err != nil {
		return err
	}
	block := pictureBlock(cover, mime)
	if len(block) >= 1<<24 {
		return errors.New("cover: too large")
	}
	flags := byte(6)
	if last {
		flags |= 0x80
	}
	head[4] &= 0x7f
	for _, part := range [][]byte{head[:], info, {flags, byte(len(block) >> 16), byte(len(block) >> 8), byte(len(block))}, block} {
		if _, err := out.Write(part); err != nil {
			return err
		}
	}
	_, err := io.Copy(out, in)
	return err
}

type id3Tag struct {
	version byte
	frames  []byte
}

func readID3(r io.Reader) (id3Tag, []byte, error) {
	var h [10]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return id3Tag{}, nil, err
	}
	if string(h[:3]) != "ID3" || (h[3] != 3 && h[3] != 4) || h[5] != 0 {
		return id3Tag{}, nil, errors.New("cover: unsupported ID3 tag")
	}
	size := int(h[6]&0x7f)<<21 | int(h[7]&0x7f)<<14 | int(h[8]&0x7f)<<7 | int(h[9]&0x7f)
	frames := make([]byte, size)
	if _, err := io.ReadFull(r, frames); err != nil {
		return id3Tag{}, nil, err
	}
	return id3Tag{h[3], frames}, h[:], nil
}

func synchsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
}

// insertAPIC adds an APIC frame to the file's ID3v2 tag, or gives it a new tag.
func insertAPIC(in io.Reader, out io.Writer, cover []byte, mime string) error {
	r := bufio.NewReader(in)
	tag := id3Tag{version: 3}
	if head, err := r.Peek(3); err == nil && string(head) == "ID3" {
		if tag, _, err = readID3(r); err != nil {
			return err
		}
	}
	var body bytes.Buffer
	body.WriteByte(0) // text encoding of the description
	body.WriteString(mime)
	body.WriteByte(0)
	body.WriteByte(3) // front cover
	body.WriteByte(0) // no description
	body.Write(cover)
	frame := []byte("APIC")
	if tag.version == 4 {
		frame = append(frame, synchsafe(body.Len())...)
	} else {
		frame = binary.BigEndian.AppendUint32(frame, uint32(body.Len()))
	}
	frame = append(frame, 0, 0)
	// Padding after the old frames would swallow a frame added behind it.
	frames := bytes.TrimRight(tag.frames, "\x00")
	total := len(frames) + len(frame) + body.Len()
	if total >= 1<<28 {
		return errors.New("cover: tag too large")
	}
	for _, part := range [][]byte{{'I', 'D', '3', tag.version, 0, 0}, synchsafe(total), frames, frame, body.Bytes()} {
		if _, err := out.Write(part); err != nil {
			return err
		}
	}
	_, err := io.Copy(out, r)
	return err
}
