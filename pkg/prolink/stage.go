package prolink

import (
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
)

const maxAudio = 1 << 30

// A drive keeps a tenth of itself free, and at least StageReserve: the 2 GiB an earlier
// build left were less than Engine wants for one track's Stems. On-demand playback caches
// (.rekordbox-cache) take at most a tenth of the drive, and never more than StageMax.
const (
	StageReserve = 4 << 30
	StageMax     = 8 << 30
)

// StageRoom is how many more bytes an on-demand playback cache may use on a drive that
// already holds cached bytes of them. A drive whose size statfs does not tell keeps the
// two fixed limits.
func StageRoom(fs *syscall.Statfs_t, cached int64) int64 {
	free, size := int64(fs.Bavail)*int64(fs.Bsize), int64(fs.Blocks)*int64(fs.Bsize)
	reserve, limit := int64(StageReserve), int64(StageMax)
	if size > 0 {
		reserve, limit = max(reserve, size/10), min(limit, size/10)
	}
	return min(free-reserve, limit-cached)
}

// DirSize is the bytes of the regular files under dir.
func DirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// exportName is what a deck of ours calls itself on the link when it shares its library
// as a player (export.go).
const exportName = "EngineX Export Beta"

// SafeRel turns a path from a remote export into one that stays inside its root.
func SafeRel(p string) (string, bool) {
	rel := filepath.Clean(strings.TrimPrefix(p, "/"))
	return rel, p != "" && filepath.IsLocal(rel)
}
