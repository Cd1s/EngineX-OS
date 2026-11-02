// Package bridge coordinates read-only export discovery and Engine imports.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

// ST_RDONLY from statvfs(3); syscall has no portable name for it.
const unix_ST_RDONLY = 0x1

func ReadLibrary(ctx context.Context, mount string) (*rekordbox.Library, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return nil, e
	}
	for _, p := range []string{"PIONEER/rekordbox/export.pdb", "PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/exportOneLibrary.db"} {
		rel, e := r.Resolve(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if strings.EqualFold(filepath.Ext(rel), ".pdb") {
			return rekordbox.ReadPDB(r.FullPath(rel))
		}
		return rekordbox.ReadSQLite(ctx, r.FullPath(rel), nil)
	}
	return nil, os.ErrNotExist
}

// ensureWritableEngineLibrary mounts an ext4-backed overlay onto "<mount>/Engine Library"
// when the underlying volume (such as macOS HFS+) is mounted read-only.
func ensureWritableEngineLibrary(mount string) bool {
	libDir := filepath.Join(mount, "Engine Library")
	var libSt syscall.Statfs_t
	if syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0 {
		return true
	}
	info, err := os.Lstat(libDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if st, err := os.Stat("/data/sc5000-custom"); err != nil || !st.IsDir() {
		return false
	}
	base := filepath.Base(filepath.Clean(mount))
	if base == "" || base == "." || base == "/" {
		return false
	}
	lower := "/run/sc5000-empty"
	upper := filepath.Join("/data/sc5000-custom/hfs-lib", base, "upper")
	work := filepath.Join("/data/sc5000-custom/hfs-lib", base, "work")
	for _, d := range []string{lower, upper, work} {
		if os.MkdirAll(d, 0755) != nil {
			return false
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if syscall.Mount("overlay", libDir, "overlay", 0, opts) != nil {
		return false
	}
	return syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0
}

func Convert(ctx context.Context, mount string, opts enginedb.Options) (enginedb.Result, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return enginedb.Result{}, e
	}
	_, e = r.Resolve("PIONEER")
	if errors.Is(e, os.ErrNotExist) {
		return enginedb.Result{Bypassed: true}, nil
	}
	// Read-only volumes (HFS+ on Engine OS 5.0.4: rw writes crash hfsplus.ko)
	// cannot hold an Engine Library unless Engine Library itself is a writable mount.
	var st syscall.Statfs_t
	if e == nil && syscall.Statfs(mount, &st) == nil && st.Flags&unix_ST_RDONLY != 0 {
		if !ensureWritableEngineLibrary(mount) {
			return enginedb.Result{Bypassed: true}, nil
		}
	}
	if e != nil {
		return enginedb.Result{}, e
	}
	l, e := ReadLibrary(ctx, mount)
	if errors.Is(e, os.ErrNotExist) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if e != nil {
		return enginedb.Result{}, e
	}
	return enginedb.Sync(ctx, mount, l, opts)
}

// Candidates examines at most three directory levels (Linux mount conventions),
// stopping at PIONEER (.PIONEER on macOS exports). It never recursively scans music or follows symlinks.
func Candidates(root string) ([]string, error) {
	resolved, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	root = resolved
	var out []string
	var visit func(string, int) error
	visit = func(dir string, depth int) error {
		st, e := os.Lstat(dir)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			return e
		}
		engineOnly := false
		for _, ent := range entries {
			if ent.IsDir() && (strings.EqualFold(ent.Name(), "PIONEER") || strings.EqualFold(ent.Name(), ".PIONEER")) {
				out = append(out, dir)
				return nil
			}
			if ent.IsDir() && strings.EqualFold(ent.Name(), "Engine Library") {
				engineOnly = true
			}
		}
		if engineOnly {
			return nil
		}
		if depth == 0 {
			return nil
		}
		for _, ent := range entries {
			if !ent.IsDir() || ent.Name() == "Engine Library" {
				continue
			}
			if e := visit(filepath.Join(dir, ent.Name()), depth-1); e != nil && !errors.Is(e, os.ErrNotExist) && !errors.Is(e, os.ErrPermission) {
				return e
			}
		}
		return nil
	}
	e = visit(root, 3)
	return out, e
}

type WatchEvent struct {
	Mount  string
	Result enginedb.Result
	Err    error
}

// Watch imports a mount once per attachment. Failed imports retry; successful
// imports are not repeated while a player may be editing its performance data.
func Watch(ctx context.Context, roots []string, opts enginedb.Options, interval time.Duration, notify func(WatchEvent)) error {
	if interval <= 0 {
		return fmt.Errorf("watch interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	done := map[string]bool{}
	retry := map[string]time.Time{}
	for {
		present := map[string]bool{}
		for _, root := range roots {
			xs, e := Candidates(root)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				if notify != nil {
					notify(WatchEvent{Mount: root, Err: e})
				}
				continue
			}
			for _, mount := range xs {
				present[mount] = true
				if done[mount] || time.Now().Before(retry[mount]) {
					continue
				}
				result, e := Convert(ctx, mount, opts)
				if notify != nil {
					notify(WatchEvent{mount, result, e})
				}
				if e == nil {
					done[mount] = true
				} else {
					retry[mount] = time.Now().Add(10 * time.Second)
				}
			}
		}
		for mount := range done {
			if !present[mount] {
				delete(done, mount)
				delete(retry, mount)
			}
		}
		for mount := range retry {
			if !present[mount] {
				delete(retry, mount)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
