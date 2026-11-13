package webui

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// File identity invalidates the cold-path lookup even after an in-place write
// which restored mtime, or an atomic replacement with the same name and size.
type shadowFileIdentity struct {
	size, modified, changed int64
	device, inode           uint64
}

func shadowIdentity(st os.FileInfo) shadowFileIdentity {
	id := shadowFileIdentity{size: st.Size(), modified: st.ModTime().UnixNano()}
	if v, ok := st.Sys().(*syscall.Stat_t); ok {
		id.device, id.inode = uint64(v.Dev), v.Ino
		id.changed = v.Ctim.Sec*1e9 + v.Ctim.Nsec
	}
	return id
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
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (sh *shadowState) resolvedTrackKey(ctx context.Context, deck int, path string, st os.FileInfo) string {
	sh.resolveMu.Lock()
	defer sh.resolveMu.Unlock()
	id := shadowIdentity(st)
	c := sh.resolved[deck]
	if c.path == path && c.identity == id {
		if c.libraryPath == "" {
			return c.key
		}
		if live, err := os.Stat(c.libraryPath); err == nil && shadowIdentity(live) == c.libraryIdentity {
			return c.key
		}
	}
	c = shadowResolvedFile{path: path, identity: id}
	if orig, ok := inMusicTempOrig(path, sh.dataRoot); ok {
		fingerprint, err := shadowFingerprint(path, st.Size())
		if err != nil {
			return ""
		}
		if realPath := resolveInMusicTempPath(ctx, orig, st.Size(), sh.dataRoot); realPath != "" {
			if rst, err := os.Stat(realPath); err == nil {
				if other, err := shadowFingerprint(realPath, rst.Size()); err == nil && fingerprint == other {
					c.libraryPath, c.libraryIdentity = realPath, shadowIdentity(rst)
					c.key = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s %d %d %s", realPath, rst.Size(), rst.ModTime().UnixNano(), fingerprint))))
				}
			}
		}
		if c.key == "" {
			c.key = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("temp:%s %d %s", orig, id.modified, fingerprint))))
		}
	} else {
		c.key = fmt.Sprintf("%x", fnvSum(fmt.Sprintf("%s %d %d", path, id.size, id.modified)))
	}
	sh.resolved[deck] = c
	return c.key
}

// Filename fallback is allowed only for one physical file of the expected size.
// Include unanalysed rows when checking ambiguity.
func dbPathByFilename(ctx context.Context, mdb, filename string, size int64) (string, error) {
	if _, err := os.Stat(mdb); err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT path FROM Track WHERE filename = ?`, filename)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var match string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return "", err
		}
		if rel == "" {
			continue
		}
		candidate := filepath.Join(filepath.Dir(filepath.Dir(mdb)), filepath.FromSlash(rel))
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && st.Size() == size {
			if match != "" {
				return "", fmt.Errorf("shadow: ambiguous filename")
			}
			match = rel
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if match == "" {
		return "", sql.ErrNoRows
	}
	return match, nil
}
