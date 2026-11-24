package rbstate

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The stage holds what the background job has built from a stick's ANLZ files and audio
// headers, until a mount window writes it to m.db. It lives on the deck, one directory per
// library (Information.uuid), so the job never writes the stick:
//
//	<root>/<uuid>/index.jsonl     one line per Put (the last line of a key wins), and marks
//	<root>/<uuid>/<name>.bin      the track's performance data, in the UI hook's RBXA1 layout
//	<root>/<uuid>/<name>.rgb      the body of the track's OverviewData file, when it has one
//	<root>/<uuid>/<name>.jpg      a cover taken from the audio file itself
//
// <name> is the hex SHA-1 of the track's key.

const (
	// StageEnv overrides the stage root; tests must set it.
	StageEnv         = "ENGINEX_RB_STAGE_DIR"
	defaultStageRoot = "/data/enginex/rb-stage"
	stageIndex       = "index.jsonl"
	perfMagic        = "RBXA1"
	// stageFloor is the free space /data keeps: Put refuses below it.
	stageFloor = 256 << 20
	stageIdle  = 30 * 24 * time.Hour
	maxBlob    = 64 << 20
)

// ErrStageFull: the stage's file system is at its floor; nothing was written.
var ErrStageFull = errors.New("rbstate: the stage's disk is at its free-space floor")

// Perf is a track's performance data as Engine stores it (enginedb.Performance).
type Perf struct {
	TrackData, Overview, Beats, QuickCues, Loops []byte
	ActiveLoops                                  uint32
	Complete                                     bool
}

// Facts is what the audio file's header says, where it differs from or adds to the export.
type Facts struct {
	Duration           float64 `json:"dur,omitempty"`
	SampleRate         int     `json:"rate,omitempty"`
	SampleCount        uint64  `json:"samples,omitempty"`
	BitDepth           int     `json:"bits,omitempty"`
	Bitrate            int     `json:"kbps,omitempty"`
	StartOffsetSamples int     `json:"start,omitempty"`
	BPM                float64 `json:"bpm,omitempty"` // the grid's first tempo when the export has none
	FileBytes          int64   `json:"bytes,omitempty"`
}

// Staged is one track as the job built it from the volume.
type Staged struct {
	Key      string   `json:"k"`
	Sig      TrackSig `json:"sig"`
	Rel      string   `json:"rel,omitempty"` // the audio file's spelling on the volume
	Missing  bool     `json:"missing,omitempty"`
	Facts    Facts    `json:"facts"`
	Perf     Perf     `json:"-"`
	RGB      []byte   `json:"-"`
	Art      []byte   `json:"-"` // only a cover from the audio file; an export's cover is read from Sig.Artwork
	Detail   []string `json:"det,omitempty"`
	Degraded string   `json:"deg,omitempty"` // why there is no performance data, "" otherwise

	// In the index line only: what lies beside it.
	HasPerf     bool   `json:"perf,omitempty"`
	HasRGB      bool   `json:"rgb,omitempty"`
	HasArt      bool   `json:"art,omitempty"`
	ActiveLoops uint32 `json:"active,omitempty"`
}

type indexLine struct {
	*Staged
	Drop     string `json:"drop,omitempty"`     // a key a window applied
	Verified string `json:"verified,omitempty"` // a stamp whose every track was compared
}

// Stage is one library's stage directory.
type Stage struct {
	dir   string
	index *os.File
}

// StageRoot is where the stages are.
func StageRoot() (string, error) {
	if root := os.Getenv(StageEnv); root != "" {
		return root, nil
	}
	if testing.Testing() {
		return "", fmt.Errorf("rbstate: a test must set %s", StageEnv)
	}
	return defaultStageRoot, nil
}

// OpenStage opens the stage of the library uuid, creating it, and removes the stages of other
// libraries that were not touched for 30 days.
func OpenStage(libraryUUID string) (*Stage, error) {
	if libraryUUID == "" || libraryUUID != filepath.Base(libraryUUID) || libraryUUID[0] == '.' {
		return nil, fmt.Errorf("rbstate: bad library uuid %q", libraryUUID)
	}
	root, err := StageRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, libraryUUID)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("rbstate: stage %s is not a directory", dir)
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.Name() == libraryUUID || !e.IsDir() {
				continue
			}
			if st, err := os.Stat(filepath.Join(root, e.Name(), stageIndex)); err == nil && time.Since(st.ModTime()) > stageIdle {
				_ = os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
	}
	return &Stage{dir: dir}, nil
}

// Dir is the stage's directory.
func (s *Stage) Dir() string { return s.dir }

func stageName(key string) string {
	h := sha1.Sum([]byte(key))
	return hex.EncodeToString(h[:])
}

func (s *Stage) appendLine(l indexLine) error {
	if s.index == nil {
		f, err := os.OpenFile(filepath.Join(s.dir, stageIndex), os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		// A line a power cut tore has no newline: end it, or this line would join it and be lost.
		if st, err := f.Stat(); err == nil && st.Size() > 0 {
			last := make([]byte, 1)
			if r, err := os.Open(f.Name()); err == nil {
				_, _ = r.ReadAt(last, st.Size()-1)
				_ = r.Close()
			}
			if last[0] != '\n' {
				if _, err = f.Write([]byte{'\n'}); err != nil {
					_ = f.Close()
					return err
				}
			}
		}
		s.index = f
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	_, err = s.index.Write(append(b, '\n'))
	return err
}

// Put stages a track: its files first, each written whole and synced, then its index line.
func (s *Stage) Put(t *Staged) error {
	if t.Key == "" {
		return errors.New("rbstate: a staged track needs a key")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &fs); err == nil && fs.Bavail*uint64(fs.Bsize) < stageFloor {
		return ErrStageFull
	}
	name := filepath.Join(s.dir, stageName(t.Key))
	line := *t
	line.HasPerf = len(t.Perf.TrackData) > 0 || len(t.Perf.Beats) > 0 || len(t.Perf.QuickCues) > 0 || len(t.Perf.Loops) > 0 || len(t.Perf.Overview) > 0
	line.HasRGB, line.HasArt, line.ActiveLoops = len(t.RGB) > 0, len(t.Art) > 0, t.Perf.ActiveLoops
	for _, f := range []struct {
		ext  string
		data []byte
		keep bool
	}{{".bin", EncodePerf(t.Perf), line.HasPerf}, {".rgb", t.RGB, line.HasRGB}, {".jpg", t.Art, line.HasArt}} {
		if !f.keep {
			_ = os.Remove(name + f.ext)
			continue
		}
		if err := writeWhole(name+f.ext, f.data); err != nil {
			return err
		}
	}
	return s.appendLine(indexLine{Staged: &line})
}

func writeWhole(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// EncodePerf is the RBXA1 file of p: the magic, then trackData, overviewWaveFormData, beatData,
// quickCues and loops, each a little-endian uint32 length and its bytes, then one byte that
// says whether the track counts as analysed. tools/ui-mod/rekordbox_analysis.h reads the same.
func EncodePerf(p Perf) []byte {
	var b bytes.Buffer
	b.WriteString(perfMagic)
	for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops} {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(blob)))
		b.Write(blob)
	}
	if p.Complete {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// DecodePerf reads an RBXA1 file. ActiveLoops is not part of it.
func DecodePerf(b []byte) (Perf, error) {
	var p Perf
	bad := errors.New("rbstate: not a staged performance file")
	if len(b) < len(perfMagic) || string(b[:len(perfMagic)]) != perfMagic {
		return p, bad
	}
	at := len(perfMagic)
	for _, dst := range []*[]byte{&p.TrackData, &p.Overview, &p.Beats, &p.QuickCues, &p.Loops} {
		if len(b)-at < 4 {
			return Perf{}, bad
		}
		n := int(binary.LittleEndian.Uint32(b[at:]))
		at += 4
		if n < 0 || n > len(b)-at {
			return Perf{}, bad
		}
		if n > 0 {
			*dst = append([]byte(nil), b[at:at+n]...)
		}
		at += n
	}
	if len(b)-at != 1 || b[at] > 1 {
		return Perf{}, bad
	}
	p.Complete = b[at] == 1
	return p, nil
}

// readIndex returns the last line of every key that was not dropped since, and the last
// verified stamp. A torn last line is ignored.
func (s *Stage) readIndex() (map[string]*Staged, string, error) {
	out := map[string]*Staged{}
	f, err := os.Open(filepath.Join(s.dir, stageIndex))
	if errors.Is(err, os.ErrNotExist) {
		return out, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	verified := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l indexLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch {
		case l.Drop != "":
			delete(out, l.Drop)
		case l.Verified != "":
			verified = l.Verified
		case l.Staged != nil && l.Staged.Key != "":
			out[l.Staged.Key] = l.Staged
		}
	}
	return out, verified, sc.Err()
}

// Index is what is staged and from what, without reading the blobs. A line whose files are
// gone is left out, so its track is staged again.
func (s *Stage) Index() (map[string]TrackSig, error) {
	lines, _, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	out := make(map[string]TrackSig, len(lines))
	for k, t := range lines {
		if t.HasPerf {
			if _, err := os.Stat(filepath.Join(s.dir, stageName(k)+".bin")); err != nil {
				continue
			}
		}
		out[k] = t.Sig
	}
	return out, nil
}

// Get reads a staged track with its blobs. os.ErrNotExist: not staged, or its files are not whole.
func (s *Stage) Get(key string) (*Staged, error) {
	lines, _, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	return s.load(lines[key])
}

// Entries is every staged track as its index line has it, without the blobs, and the last
// verified stamp. A window reads it once, picks what it applies and calls Read for each.
func (s *Stage) Entries() (entries map[string]*Staged, verified string, err error) {
	return s.readIndex()
}

// Read returns an entry of Entries with its blobs. os.ErrNotExist: its files are not whole.
func (s *Stage) Read(entry *Staged) (*Staged, error) { return s.load(entry) }

func (s *Stage) load(line *Staged) (*Staged, error) {
	if line == nil {
		return nil, os.ErrNotExist
	}
	t := *line
	name := filepath.Join(s.dir, stageName(t.Key))
	if t.HasPerf {
		b, err := readBlob(name + ".bin")
		if err != nil {
			return nil, err
		}
		if t.Perf, err = DecodePerf(b); err != nil {
			return nil, os.ErrNotExist
		}
		t.Perf.ActiveLoops = t.ActiveLoops
	}
	var err error
	if t.HasRGB {
		if t.RGB, err = readBlob(name + ".rgb"); err != nil {
			return nil, err
		}
	}
	if t.HasArt {
		if t.Art, err = readBlob(name + ".jpg"); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

func readBlob(p string) ([]byte, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxBlob {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(p)
}

// MarkVerified records that every track of stamp's export was compared with the snapshot.
func (s *Stage) MarkVerified(stamp string) error {
	if err := s.appendLine(indexLine{Verified: stamp}); err != nil {
		return err
	}
	return s.Sync()
}

// Verified is the last stamp MarkVerified recorded.
func (s *Stage) Verified() string {
	_, v, _ := s.readIndex()
	return v
}

// Drop forgets staged tracks, after a window applied them.
func (s *Stage) Drop(keys ...string) error {
	for _, k := range keys {
		if err := s.appendLine(indexLine{Drop: k}); err != nil {
			return err
		}
		name := filepath.Join(s.dir, stageName(k))
		for _, ext := range []string{".bin", ".rgb", ".jpg"} {
			_ = os.Remove(name + ext)
		}
	}
	return nil
}

// Sync puts the index on the disk. Put syncs each track's files itself.
func (s *Stage) Sync() error {
	if s.index == nil {
		return nil
	}
	return s.index.Sync()
}

// Close syncs and closes the index.
func (s *Stage) Close() error {
	if s.index == nil {
		return nil
	}
	err := s.index.Sync()
	if cerr := s.index.Close(); err == nil {
		err = cerr
	}
	s.index = nil
	return err
}

// Remove deletes the whole stage.
func (s *Stage) Remove() error {
	_ = s.Close()
	return os.RemoveAll(s.dir)
}
