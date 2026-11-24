package rbstate

import (
	"bufio"
	"bytes"
	"encoding/gob"
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

// The snapshot is one file:
//
//	RBS2 {header}\n       JSON: all an unchanged insert reads
//	body                  the lists and the tracks, by key, as encoding/gob writes them
//	\nE <records> <crc32 of every byte above, hex>\n
//
// The body is binary because every window that finds a changed export reads it all and every
// commit writes it all: as JSON lines, 2,390 tracks took 0.4 s of a deck's CPU for one read and
// one write (replica, 2026-11-24). The file is only ever replaced whole (Save), so a reader sees
// an old one or a new one. A file without the E line is no snapshot.

const (
	snapshotMagic = "RBS2 "
	maxSnapshot   = 256 << 20
)

type snapshotBody struct {
	Lists  []*List
	Tracks []*Track
}

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
func Load(dbDir string) (*Snapshot, error) { return load(filepath.Join(dbDir, SnapshotName)) }

func load(file string) (*Snapshot, error) {
	f, err := os.Open(file)
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
	head, rest, _ := bytes.Cut(body[:len(body)-1], []byte{'\n'})
	if err = json.Unmarshal(head[len(snapshotMagic):], &s.Header); err != nil || s.Format != Format {
		return nil, ErrNoSnapshot
	}
	var rec snapshotBody
	if err = gob.NewDecoder(bytes.NewReader(rest)).Decode(&rec); err != nil || len(rec.Lists)+len(rec.Tracks) != records {
		return nil, ErrNoSnapshot
	}
	for _, l := range rec.Lists {
		s.Lists[l.Key] = l
	}
	for _, t := range rec.Tracks {
		s.Tracks[t.Key] = t
	}
	if len(s.Lists)+len(s.Tracks) != records {
		return nil, ErrNoSnapshot // an empty or repeated key
	}
	return s, nil
}

// Encode is the snapshot as Save writes it.
func (s *Snapshot) Encode() ([]byte, error) {
	s.Format, s.Rules = Format, Rules
	s.Header.Tracks, s.Header.Lists = len(s.Tracks), len(s.Lists)
	head, err := json.Marshal(&s.Header)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Grow(len(head) + 256 + 64*len(s.Lists) + 512*len(s.Tracks))
	b.WriteString(snapshotMagic)
	b.Write(head)
	b.WriteByte('\n')
	rec := snapshotBody{Lists: make([]*List, 0, len(s.Lists)), Tracks: make([]*Track, 0, len(s.Tracks))}
	for _, k := range sortedKeys(s.Lists) {
		rec.Lists = append(rec.Lists, s.Lists[k])
	}
	for _, k := range sortedKeys(s.Tracks) {
		rec.Tracks = append(rec.Tracks, s.Tracks[k])
	}
	if err = gob.NewEncoder(&b).Encode(&rec); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
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

// A window writes m.db and the snapshot so that a cut between the two loses nothing. After its
// statements ran, and before the transaction commits, it saves the snapshot the commit leads to
// as the next snapshot (SaveNext): by then every new row's ID is known. After the commit it
// makes that file the snapshot (Promote). A next snapshot found at the start of a window was cut
// off on either side of its commit; LoadNext returns it as claims, and the window takes over a
// claimed row only if m.db really holds it (the row's ID with its path, the list's ID with its
// title and folder). So no row is taken for the bridge's on the word of a commit that never
// happened, whatever Engine wrote in between.
const nextName = SnapshotName + ".next"

// SaveNext writes the snapshot the transaction under way leads to.
func (s *Snapshot) SaveNext(dbDir string) error {
	b, err := s.Encode()
	if err != nil {
		return err
	}
	return ReplaceFile(filepath.Join(dbDir, nextName), b)
}

// Promote makes the next snapshot the snapshot, pull-safe.
func Promote(dbDir string) error {
	return RenameFile(filepath.Join(dbDir, nextName), filepath.Join(dbDir, SnapshotName))
}

// LoadNext returns the next snapshot a cut left, or ErrNoSnapshot.
func LoadNext(dbDir string) (*Snapshot, error) { return load(filepath.Join(dbDir, nextName)) }

// DropNext removes the next snapshot once its claims are settled.
func DropNext(dbDir string) error {
	err := RemoveFile(filepath.Join(dbDir, nextName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
