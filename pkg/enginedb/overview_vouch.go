package enginedb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// The overview vouch is a file next to a library whose imported overviews rb-bridge has
// completed (FillOverviewMaxima): the tracks it had to leave without band maxima, and the
// state of rb-bridge's own database at that moment. stream-sync answers the deck's
// overview from it and never opens m.db, which belongs to Engine while the stick is in.
const overviewVouchFile = ".rb-bridge-overview"

type overviewVouch struct {
	State string   `json:"state"`
	Raw   []string `json:"raw,omitempty"`
}

// readRegular reads at most limit bytes of a regular file, never through a symlink.
func readRegular(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// stateKey changes with every import, an earlier build's too: each one writes rb-bridge's
// own database (since 5.3.0.2), and SQLite counts the writes in the file's header. Engine
// never opens that file, so reading it disturbs nothing.
func stateKey(dir string) string {
	header, err := readRegular(filepath.Join(dir, stateFile), 100)
	if err != nil || len(header) < 100 {
		return ""
	}
	sum := sha256.Sum256(header)
	return hex.EncodeToString(sum[:])
}

// OverviewVouch reads the vouch in a library's Database2 directory: the tracks (Track.path)
// whose overview has no band maxima. ok is false without a vouch or when an import has run
// since it was written; nothing is known about the library's overviews then.
func OverviewVouch(dir string) (raw []string, ok bool) {
	b, err := readRegular(filepath.Join(dir, overviewVouchFile), 1<<20)
	var v overviewVouch
	if err != nil || json.Unmarshal(b, &v) != nil || v.State == "" || v.State != stateKey(dir) {
		return nil, false
	}
	return v.Raw, true
}

// VouchOverviews records that every imported overview in the library has its band maxima,
// except those of raw.
func VouchOverviews(dir string, raw []string) error {
	state := stateKey(dir)
	if state == "" {
		return fmt.Errorf("no %s in %s", stateFile, dir)
	}
	b, err := json.Marshal(overviewVouch{state, raw})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, overviewVouchFile)
	if old, err := readRegular(path, 1<<20); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := safeFile(path); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
