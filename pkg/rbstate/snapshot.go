package rbstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The snapshot is text, one record a line:
//
//	RBS1 {header}
//	L {list}        one per list, by key
//	T {track}       one per track, by key
//	E <records> <crc32 of every byte above, hex>
//
// It is only ever replaced whole (Save), so a reader sees an old one or a new one. A file
// without the E line is no snapshot.

const (
	snapshotMagic = "RBS1 "
	maxSnapshot   = 256 << 20
)

// ErrNoSnapshot wraps os.ErrNotExist: the directory has no snapshot a reader can use.
var ErrNoSnapshot = fmt.Errorf("rbstate: no snapshot: %w", os.ErrNotExist)

// ReadHeader reads the snapshot's first line and checks that the file ends with its E line.
// It reads two small pieces of the file whatever its size.
func ReadHeader(dbDir string) (Header, error) {
	var h Header
	f, err := os.Open(filepath.Join(dbDir, SnapshotName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return h, ErrNoSnapshot
		}
		return h, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return h, err
	}
	line, err := bufio.NewReaderSize(f, 8<<10).ReadBytes('\n')
	if err != nil || !bytes.HasPrefix(line, []byte(snapshotMagic)) {
		return h, ErrNoSnapshot
	}
	if err = json.Unmarshal(line[len(snapshotMagic):], &h); err != nil || h.Format != Format {
		return Header{}, ErrNoSnapshot
	}
	tail := make([]byte, min(st.Size(), 64))
	if _, err = f.ReadAt(tail, st.Size()-int64(len(tail))); err != nil {
		return Header{}, err
	}
	if _, _, ok := parseEnd(lastLine(tail)); !ok {
		return Header{}, ErrNoSnapshot
	}
	return h, nil
}

// lastLine is the last line of b, which ends in a newline, without it.
func lastLine(b []byte) []byte {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil
	}
	b = b[:len(b)-1]
	return b[bytes.LastIndexByte(b, '\n')+1:]
}

func parseEnd(line []byte) (records int, sum uint32, ok bool) {
	f := strings.Fields(string(line))
	if len(f) != 3 || f[0] != "E" {
		return 0, 0, false
	}
	n, err := strconv.Atoi(f[1])
	s, err2 := strconv.ParseUint(f[2], 16, 32)
	return n, uint32(s), err == nil && err2 == nil
}

// Load reads and checks the whole snapshot.
func Load(dbDir string) (*Snapshot, error) {
	f, err := os.Open(filepath.Join(dbDir, SnapshotName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoSnapshot
		}
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSnapshot+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSnapshot {
		return nil, fmt.Errorf("rbstate: snapshot larger than %d bytes", maxSnapshot)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, ErrNoSnapshot
	}
	end := lastLine(b)
	records, sum, ok := parseEnd(end)
	body := b[:len(b)-len(end)-1]
	if !ok || len(end) == 0 || crc32.ChecksumIEEE(body) != sum || !bytes.HasPrefix(body, []byte(snapshotMagic)) {
		return nil, ErrNoSnapshot
	}
	s := &Snapshot{Tracks: map[string]*Track{}, Lists: map[string]*List{}}
	lines := bytes.Split(body[:len(body)-1], []byte{'\n'})
	if err = json.Unmarshal(lines[0][len(snapshotMagic):], &s.Header); err != nil || s.Format != Format {
		return nil, ErrNoSnapshot
	}
	if len(lines)-1 != records {
		return nil, ErrNoSnapshot
	}
	for _, line := range lines[1:] {
		if len(line) < 2 || line[1] != ' ' {
			return nil, ErrNoSnapshot
		}
		switch line[0] {
		case 'T':
			t := new(Track)
			if err = json.Unmarshal(line[2:], t); err != nil || t.Key == "" {
				return nil, ErrNoSnapshot
			}
			s.Tracks[t.Key] = t
		case 'L':
			l := new(List)
			if err = json.Unmarshal(line[2:], l); err != nil || l.Key == "" {
				return nil, ErrNoSnapshot
			}
			s.Lists[l.Key] = l
		default:
			return nil, ErrNoSnapshot
		}
	}
	return s, nil
}

// Encode is the snapshot as Save writes it.
func (s *Snapshot) Encode() ([]byte, error) {
	s.Format, s.Rules = Format, Rules
	s.Header.Tracks, s.Header.Lists = len(s.Tracks), len(s.Lists)
	var b bytes.Buffer
	b.Grow(256 + 64*len(s.Lists) + 256*len(s.Tracks))
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	b.WriteString(snapshotMagic)
	if err := enc.Encode(&s.Header); err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(s.Lists) {
		b.WriteString("L ")
		if err := enc.Encode(s.Lists[k]); err != nil {
			return nil, err
		}
	}
	for _, k := range sortedKeys(s.Tracks) {
		b.WriteString("T ")
		if err := enc.Encode(s.Tracks[k]); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(&b, "E %d %08x\n", len(s.Lists)+len(s.Tracks), crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Save replaces the snapshot in dbDir, pull-safe (ReplaceFile). The caller holds LockVolume.
func (s *Snapshot) Save(dbDir string) error {
	b, err := s.Encode()
	if err != nil {
		return err
	}
	return ReplaceFile(filepath.Join(dbDir, SnapshotName), b)
}

// Remove deletes the snapshot of dbDir, pull-safe.
func Remove(dbDir string) error {
	err := RemoveFile(filepath.Join(dbDir, SnapshotName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
