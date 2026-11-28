package deemixclient

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// CopyTagged writes dst as a copy of the download src with title, artist and
// album tags, when src is a FLAC or MP3 that has no title of its own. Services
// send audio without tags and the cache names files by id, so a file Engine
// imports (streaming Stems) showed up as "<digits>.flac". The download itself
// is not touched: the import job may be embedding its cover at the same time.
// It reports false and writes nothing when src has a title already or is in a
// format it does not tag.
func CopyTagged(src, dst, title, artist, album string) (bool, error) {
	if title == "" {
		return false, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return false, err
	}
	r := bufio.NewReader(f)
	head, _ := r.Peek(4)
	isFLAC := string(head) == "fLaC"
	isMP3 := !isFLAC && string(head) != "OggS" && strings.EqualFold(filepath.Ext(src), ".mp3")
	has := true
	switch {
	case isFLAC:
		r.Discard(4)
		if blocks, err := readFLACBlocks(r); err == nil {
			has = false
			for _, b := range blocks {
				has = has || (b.kind == 4 && vorbisHasField(b.data, "TITLE"))
			}
		}
	case isMP3 && len(head) >= 3 && string(head[:3]) == "ID3":
		if tag, _, err := readID3(r); err == nil {
			has = id3HasFrame(tag, "TIT2")
		}
	case isMP3:
		has = false
	}
	f.Close()
	if has {
		return false, nil
	}
	fields := [][2]string{{"TITLE", title}, {"ARTIST", artist}, {"ALBUM", album}}
	if isFLAC {
		return true, rewriteFile(src, dst, func(in io.Reader, out io.Writer) error { return tagFLAC(in, out, fields) })
	}
	return true, rewriteFile(src, dst, func(in io.Reader, out io.Writer) error {
		return appendID3(in, out, func(tag id3Tag) []byte {
			var frames []byte
			for i, id := range []string{"TIT2", "TPE1", "TALB"} {
				// A frame the file has is kept: a tag holds one of each.
				if v := fields[i][1]; v != "" && !id3HasFrame(tag, id) {
					frames = append(frames, id3Frame(tag.version, id, id3Text(tag.version, v))...)
				}
			}
			return frames
		})
	})
}

type flacMeta struct {
	kind byte
	data []byte
}

// readFLACBlocks reads the metadata blocks that follow the "fLaC" marker.
func readFLACBlocks(r io.Reader) ([]flacMeta, error) {
	var blocks []flacMeta
	for {
		var h [4]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil, err
		}
		data := make([]byte, int(h[1])<<16|int(h[2])<<8|int(h[3]))
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		blocks = append(blocks, flacMeta{h[0] & 0x7f, data})
		if h[0]&0x80 != 0 {
			break
		}
		if len(blocks) >= 128 {
			return nil, errors.New("tags: too many FLAC metadata blocks")
		}
	}
	if blocks[0].kind != 0 {
		return nil, errors.New("tags: unexpected FLAC layout")
	}
	return blocks, nil
}

// vorbisHasField reports whether a VORBIS_COMMENT block has a non-empty name= field.
func vorbisHasField(b []byte, name string) bool {
	next := func() ([]byte, bool) {
		if len(b) < 4 {
			return nil, false
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 0 || n > len(b)-4 {
			return nil, false
		}
		v := b[4 : 4+n]
		b = b[4+n:]
		return v, true
	}
	if _, ok := next(); !ok || len(b) < 4 { // vendor
		return false
	}
	count := binary.LittleEndian.Uint32(b)
	b = b[4:]
	for ; count > 0; count-- {
		v, ok := next()
		if !ok {
			return false
		}
		if k, val, found := bytes.Cut(v, []byte("=")); found && len(val) > 0 && strings.EqualFold(string(k), name) {
			return true
		}
	}
	return false
}

// tagFLAC writes the file with one VORBIS_COMMENT block holding fields, right
// after STREAMINFO, which must stay first. The other blocks are kept in order.
func tagFLAC(in io.Reader, out io.Writer, fields [][2]string) error {
	var magic [4]byte
	if _, err := io.ReadFull(in, magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != "fLaC" {
		return errors.New("tags: not a FLAC file")
	}
	blocks, err := readFLACBlocks(in)
	if err != nil {
		return err
	}
	var comment []byte
	put := func(s string) {
		comment = binary.LittleEndian.AppendUint32(comment, uint32(len(s)))
		comment = append(comment, s...)
	}
	put("EngineX")
	var kept [][2]string
	for _, f := range fields {
		if f[1] != "" {
			kept = append(kept, f)
		}
	}
	comment = binary.LittleEndian.AppendUint32(comment, uint32(len(kept)))
	for _, f := range kept {
		put(f[0] + "=" + f[1])
	}
	result := []flacMeta{blocks[0], {4, comment}}
	for _, b := range blocks[1:] {
		if b.kind != 4 {
			result = append(result, b)
		}
	}
	if _, err = out.Write(magic[:]); err != nil {
		return err
	}
	for i, b := range result {
		if len(b.data) >= 1<<24 {
			return errors.New("tags: FLAC block too large")
		}
		kind := b.kind
		if i == len(result)-1 {
			kind |= 0x80
		}
		if _, err = out.Write([]byte{kind, byte(len(b.data) >> 16), byte(len(b.data) >> 8), byte(len(b.data))}); err != nil {
			return err
		}
		if _, err = out.Write(b.data); err != nil {
			return err
		}
	}
	_, err = io.Copy(out, in)
	return err
}

// id3Text is a text frame's body: UTF-8 in ID3v2.4, UTF-16 with a byte order
// mark in ID3v2.3, which has no UTF-8.
func id3Text(version byte, s string) []byte {
	if version == 4 {
		return append([]byte{3}, s...)
	}
	b := []byte{1, 0xff, 0xfe}
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// id3Frame is one frame of an ID3v2.3 or v2.4 tag.
func id3Frame(version byte, id string, body []byte) []byte {
	frame := []byte(id)
	if version == 4 {
		frame = append(frame, synchsafe(len(body))...)
	} else {
		frame = binary.BigEndian.AppendUint32(frame, uint32(len(body)))
	}
	return append(append(frame, 0, 0), body...)
}

// id3FramesEnd is where the tag's frames end and its padding starts, found by
// walking whole frames: a frame's own last bytes may be zero (UTF-16 text ends
// in one), so zeros cut from the end of the tag are not all padding.
func id3FramesEnd(tag id3Tag) (int, error) {
	f, end := tag.frames, 0
	for end < len(f) && f[end] != 0 {
		if len(f)-end < 10 {
			return 0, errors.New("tags: a broken ID3 frame header")
		}
		size := int(binary.BigEndian.Uint32(f[end+4 : end+8]))
		if tag.version == 4 {
			size = int(f[end+4]&0x7f)<<21 | int(f[end+5]&0x7f)<<14 | int(f[end+6]&0x7f)<<7 | int(f[end+7]&0x7f)
		}
		if size < 0 || size > len(f)-end-10 {
			return 0, errors.New("tags: an ID3 frame runs past its tag")
		}
		end += 10 + size
	}
	return end, nil
}

// appendID3 adds frames to the file's ID3v2 tag, or gives it a new tag.
func appendID3(in io.Reader, out io.Writer, frames func(tag id3Tag) []byte) error {
	r := bufio.NewReader(in)
	tag := id3Tag{version: 3}
	if head, err := r.Peek(3); err == nil && string(head) == "ID3" {
		if tag, _, err = readID3(r); err != nil {
			return err
		}
	}
	// Padding after the old frames would swallow a frame added behind it.
	end, err := id3FramesEnd(tag)
	if err != nil {
		return err
	}
	old, added := tag.frames[:end], frames(tag)
	if len(old)+len(added) >= 1<<28 {
		return errors.New("tags: tag too large")
	}
	for _, part := range [][]byte{{'I', 'D', '3', tag.version, 0, 0}, synchsafe(len(old) + len(added)), old, added} {
		if _, err := out.Write(part); err != nil {
			return err
		}
	}
	_, err = io.Copy(out, r)
	return err
}

// SameAudio reports whether the files a and b hold the same audio: one file, or the
// same bytes after the tags CopyTagged and EmbedCover write (a FLAC's metadata
// blocks, an MP3's ID3v2 tag). A Stems import file made from a download, linked or
// tagged, holds that download's audio; a new download of other audio does not match.
func SameAudio(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	sa, err := fa.Stat()
	if err != nil {
		return false, err
	}
	sb, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if os.SameFile(sa, sb) {
		return true, nil
	}
	startA, startB := audioStart(fa), audioStart(fb)
	if sa.Size()-startA != sb.Size()-startB {
		return false, nil
	}
	if _, err := fa.Seek(startA, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := fb.Seek(startB, io.SeekStart); err != nil {
		return false, err
	}
	bufA, bufB := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		n, errA := io.ReadFull(fa, bufA)
		m, errB := io.ReadFull(fb, bufB)
		if n != m || !bytes.Equal(bufA[:n], bufB[:m]) {
			return false, nil
		}
		if errA == io.EOF || errA == io.ErrUnexpectedEOF {
			return errB == io.EOF || errB == io.ErrUnexpectedEOF, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}

// audioStart is where f's audio begins: after a FLAC's metadata blocks or an MP3's
// ID3v2 tag, as CopyTagged and EmbedCover write them; 0 for anything else.
func audioStart(f *os.File) int64 {
	r := bufio.NewReader(f)
	head, _ := r.Peek(4)
	switch {
	case string(head) == "fLaC":
		r.Discard(4)
		if blocks, err := readFLACBlocks(r); err == nil {
			n := int64(4)
			for _, b := range blocks {
				n += 4 + int64(len(b.data))
			}
			return n
		}
	case len(head) >= 3 && string(head[:3]) == "ID3":
		if tag, _, err := readID3(r); err == nil {
			return 10 + int64(len(tag.frames))
		}
	}
	return 0
}
