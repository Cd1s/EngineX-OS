package webui

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// File identity invalidates the cached resolution after an audio edit.
type shadowFileIdentity struct{ size, modified int64 }

func shadowIdentity(st os.FileInfo) shadowFileIdentity {
	return shadowFileIdentity{st.Size(), st.ModTime().UnixNano()}
}

type shadowResolvedFile struct {
	path, key, libraryPath    string
	identity, libraryIdentity shadowFileIdentity
}

// The fingerprint reads at most 128 KiB, once per loaded file identity. Mtime
// is included in unresolved keys, so edited audio cannot reuse stale analysis.
func shadowFingerprint(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(size))
	_, _ = h.Write(n[:])
	if _, err := io.CopyN(h, f, min(size, 64<<10)); err != nil {
		return "", err
	}
	if size > 64<<10 {
		if _, err := f.Seek(max(64<<10, size-(64<<10)), io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyN(h, f, min(size-(64<<10), 64<<10)); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
