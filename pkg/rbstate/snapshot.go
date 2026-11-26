package rbstate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
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
// The body is gob, not JSON lines: 2,390 tracks as JSON cost 0.4 s of a deck's CPU for one read
// and one write (replica, 2026-11-24). A file without its E line is no snapshot.

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
func ReadHeader(dbDir string) (Header, error) {
	return ReadHeaderFile(filepath.Join(dbDir, SnapshotName))
}

// ReadHeaderFile is ReadHeader of a snapshot under another name (SwapName).
func ReadHeaderFile(file string) (Header, error) {
	var h Header
	f, err := openSnapshot(file)
	if err != nil {
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
func Load(dbDir string) (*Snapshot, error) { return LoadFile(filepath.Join(dbDir, SnapshotName)) }

func openSnapshot(file string) (*os.File, error) {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSnapshot
	}
	return f, err
}

// LoadFile is Load of a snapshot under another name.
func LoadFile(file string) (*Snapshot, error) {
	f, err := openSnapshot(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadReader(f)
}

// LoadReader reads and validates an encoded snapshot from r.
func LoadReader(r io.Reader) (*Snapshot, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSnapshot+1))
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
	for _, k := range slices.Sorted(maps.Keys(s.Lists)) {
		rec.Lists = append(rec.Lists, s.Lists[k])
	}
	for _, k := range slices.Sorted(maps.Keys(s.Tracks)) {
		rec.Tracks = append(rec.Tracks, s.Tracks[k])
	}
	if err = gob.NewEncoder(&b).Encode(&rec); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "E %d %08x\n", len(s.Lists)+len(s.Tracks), crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes(), nil
}

// Save replaces the snapshot in dbDir, pull-safe (ReplaceFile). The caller holds LockVolume.
func (s *Snapshot) Save(dbDir string) error { return s.SaveCtx(context.Background(), dbDir) }

// SaveCtx replaces the snapshot in dbDir while honoring ctx cancellation.
func (s *Snapshot) SaveCtx(ctx context.Context, dbDir string) error {
	return s.SaveAsCtx(ctx, filepath.Join(dbDir, SnapshotName))
}

// A window saves the snapshot its transaction leads to as the next snapshot (SaveNext) before it
// commits, then makes it the snapshot (Promote). A window that finds a next snapshot reads the
// library's commit ID: equal to Header.Commit, the commit happened and the file is promoted;
// otherwise it is dropped (design section 5). Readers other than the mount path ignore it.
const nextName = SnapshotName + ".next"

// SaveNext writes the snapshot the transaction under way leads to.
func (s *Snapshot) SaveNext(dbDir string) error {
	return s.SaveNextCtx(context.Background(), dbDir)
}

// SaveNextCtx writes the next snapshot while honoring ctx cancellation.
func (s *Snapshot) SaveNextCtx(ctx context.Context, dbDir string) error {
	return s.SaveAsCtx(ctx, filepath.Join(dbDir, nextName))
}

// SwapName is, beside the snapshot, the snapshot of a next library that is ready to take the
// stick's library's place (pkg/bridge next.go). Its Header.Base names the library it replaces.
const SwapName = SnapshotName + ".swap"

// SaveAs is Save under another name, pull-safe.
func (s *Snapshot) SaveAs(file string) error { return s.SaveAsCtx(context.Background(), file) }

// SaveAsCtx encodes and writes the snapshot under another name while honoring ctx cancellation.
func (s *Snapshot) SaveAsCtx(ctx context.Context, file string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := s.Encode()
	if err != nil {
		return err
	}
	return ReplaceFileCtx(ctx, file, b)
}

// Promote makes the next snapshot the snapshot, pull-safe.
func Promote(dbDir string) error { return PromoteCtx(context.Background(), dbDir) }

// PromoteCtx promotes the next snapshot while checking ctx cancellation before rename.
func PromoteCtx(ctx context.Context, dbDir string) error {
	return RenameFileCtx(ctx, filepath.Join(dbDir, nextName), filepath.Join(dbDir, SnapshotName))
}

// LoadNext returns the next snapshot a cut left, or ErrNoSnapshot.
func LoadNext(dbDir string) (*Snapshot, error) { return LoadFile(filepath.Join(dbDir, nextName)) }

// DropNext removes the next snapshot.
func DropNext(dbDir string) error {
	if err := RemoveFile(filepath.Join(dbDir, nextName)); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
