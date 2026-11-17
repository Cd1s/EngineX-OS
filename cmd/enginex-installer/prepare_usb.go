package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

var errUSBExportChanging = errors.New("rekordbox 文件仍在变化：请完成导出后重试 / export files are still changing; finish the rekordbox export and try again")

func prepareUSB(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 1 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(out, "Prepare USB / 预处理 U 盘: enginex-installer prepare-usb [mount path]")
		if len(args) > 1 {
			return errors.New("specify one USB mount path")
		}
		return nil
	}
	var root string
	if len(args) == 1 {
		var err error
		root, err = filepath.Abs(args[0])
		if err != nil {
			return err
		}
	} else {
		mounts, err := usbMountPaths()
		if err != nil {
			return err
		}
		exports := usbExports(mounts)
		if len(exports) != 1 {
			return fmt.Errorf("找到 %d 个 rekordbox U 盘，请指定路径 / found %d rekordbox volumes; specify a mount path: %s", len(exports), len(exports), strings.Join(exports, ", "))
		}
		root = exports[0]
	}
	if len(usbExports([]string{root})) == 0 {
		return fmt.Errorf("%s: no rekordbox export.pdb or exportLibrary.db; nothing prepared", root)
	}
	before, err := usbSourceFiles(ctx, root)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "检查导出是否已完成 / Checking export is idle: %s\n", root)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	stable, err := usbSourceFiles(ctx, root)
	if err != nil {
		return err
	}
	if !maps.Equal(before, stable) {
		return errUSBExportChanging
	}
	fmt.Fprintln(out, "Prepare USB / 预处理 U 盘：读取播放列表、提示点与节拍网格 / Reading playlists, cues and beat grids...")
	percent := -1
	opts := enginedb.Options{OnProgress: func(done, total int) {
		if total > 0 && done*100/total != percent {
			percent = done * 100 / total
			fmt.Fprintf(out, "\r%d/%d (%d%%)", done, total, percent)
		}
	}}
	result, err := bridge.ConvertAtMount(ctx, root, opts, true)
	if err != nil {
		return err
	}
	after, err := usbSourceFiles(ctx, root)
	if err != nil || !maps.Equal(stable, after) {
		markErr := os.Remove(filepath.Join(root, "Engine Library", "Database2", ".rb-bridge-stamp"))
		if errors.Is(markErr, os.ErrNotExist) {
			markErr = nil
		}
		return errors.Join(errUSBExportChanging, err, markErr)
	}
	if result.Bypassed || result.Deferred != 0 {
		return errors.New("USB preparation did not complete; no importable rekordbox library")
	}
	fmt.Fprintf(out, "\n已完成 / Prepared: %d tracks. 安全弹出后插入碟机 / Safely eject the USB stick, then insert it into the deck.\n", result.Added+result.Updated+result.Unchanged)
	if result.Degraded > 0 {
		fmt.Fprintf(out, "部分文件无法读取 / Tracks with unavailable files: %d\n", result.Degraded)
	}
	return nil
}

func usbExports(roots []string) []string {
	var found []string
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		r, err := compat.NewResolver(root)
		if err != nil {
			continue
		}
		for _, name := range []string{"export.pdb", "exportLibrary.db", "exportOneLibrary.db"} {
			rel, err := r.Resolve("PIONEER/rekordbox/" + name)
			if err != nil {
				continue
			}
			if st, err := os.Stat(r.FullPath(rel)); err == nil && st.Mode().IsRegular() {
				found = append(found, root)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

type usbSourceFile struct{ size, modified int64 }

func usbSourceFiles(ctx context.Context, root string) (map[string]usbSourceFile, error) {
	files := map[string]usbSourceFile{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() && path != root && strings.EqualFold(d.Name(), "Engine Library") {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			st, err := d.Info()
			if err != nil {
				return err
			}
			files[path] = usbSourceFile{st.Size(), st.ModTime().UnixNano()}
		}
		return nil
	})
	return files, err
}
