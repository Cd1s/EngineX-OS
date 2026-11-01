// Package bridge coordinates read-only export discovery and Engine imports.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
)

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

func Convert(ctx context.Context, mount string, opts enginedb.Options) (enginedb.Result, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return enginedb.Result{}, e
	}
	_, e = r.Resolve("PIONEER")
	if errors.Is(e, os.ErrNotExist) {
		return enginedb.Result{Bypassed: true}, nil
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
// stopping at PIONEER. It never recursively scans music or follows symlinks.
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
			if ent.IsDir() && strings.EqualFold(ent.Name(), "PIONEER") {
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
