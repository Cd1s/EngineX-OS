package rbstate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UnsafeToFree, when set, names a file on the stick whose clusters are not its own in the FAT
// (#321 review R3): freeing them would free another file's, and vfat would remount read-only.
var UnsafeToFree func(path string) bool

// ErrUnownedClusters: the file was left as it is because UnsafeToFree named it.
var ErrUnownedClusters = errors.New("rbstate: the file's clusters are not its own in the FAT")

// cut, in tests, is called between two writes a pull can separate.
// cutWrite is only used by Ctx replacement paths for write/sync/dirsync cancellation testing.
var (
	cut      = func(step string) {}
	cutWrite = func(step string) {}
)

// SetCutHooks installs test cut hooks and returns a restore function.
func SetCutHooks(onTemp, onWrite func(string)) func() {
	prevCut, prevWrite := cut, cutWrite
	if onTemp != nil {
		cut = onTemp
	} else {
		cut = func(string) {}
	}
	if onWrite != nil {
		cutWrite = onWrite
	} else {
		cutWrite = func(string) {}
	}
	return func() {
		cut, cutWrite = prevCut, prevWrite
	}
}

const tempPattern = ".enginex-rb-*.tmp"

// ReplaceFile writes data to dst so that a pull at any moment leaves dst whole, old or new.
func ReplaceFile(dst string, data []byte) error {
	return ReplaceFileCtx(context.Background(), dst, data)
}

// ReplaceFileCtx is ReplaceFile honoring ctx cancellation across chunks, fsyncs, and before rename.
func ReplaceFileCtx(ctx context.Context, dst string, data []byte) error {
	return writeAndRenameCtx(ctx, dst, func(f *os.File) error {
		const chunk = 64 << 10
		for off := 0; off < len(data); off += chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(off+chunk, len(data))
			if _, err := f.Write(data[off:end]); err != nil {
				return err
			}
			if off == 0 {
				cutWrite("write " + dst)
			}
		}
		return nil
	})
}

// InstallFile copies src, a file on another file system, to dst as ReplaceFile writes.
func InstallFile(src, dst string) error { return InstallFileChecked(src, dst, nil) }

// InstallFileChecked is InstallFile with a last check: it runs when the copy is whole and
// synced, right before the rename over dst, and its error leaves dst as it is. The copy can
// take seconds; what the rename may replace is decided after it, not before.
func InstallFileChecked(src, dst string, last func() error) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeAndRenameLast(context.Background(), dst, func(f *os.File) error { _, err := io.Copy(f, in); return err }, last)
}

// InstallFilePaced copies src to dst in bounded chunks, calling pace before each chunk,
// before every incremental flushBytes sync, and before the final sync/rename. If pace returns
// an error (e.g. a deck became active), the temp file is closed and removed without fsync.
func InstallFilePaced(src, dst string, chunk int, flushBytes int64, pace func(n int, took time.Duration) error) error {
	return InstallFilePacedChecked(src, dst, chunk, flushBytes, pace, nil)
}

// InstallFilePacedChecked is InstallFilePaced with the last check of InstallFileChecked.
func InstallFilePacedChecked(src, dst string, chunk int, flushBytes int64, pace func(n int, took time.Duration) error, last func() error) error {
	if pace != nil {
		if err := pace(0, 0); err != nil {
			return err
		}
	}
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	if chunk <= 0 {
		chunk = 64 << 10
	}
	buf := make([]byte, chunk)
	var sinceSync int64
	for {
		if pace != nil {
			if err = pace(0, 0); err != nil {
				break
			}
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			t0 := time.Now()
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				err = werr
				break
			}
			took := time.Since(t0)
			sinceSync += int64(n)
			if pace != nil {
				if err = pace(n, took); err != nil {
					break
				}
			}
			if flushBytes > 0 && sinceSync >= flushBytes {
				if pace != nil {
					if err = pace(0, 0); err != nil {
						break
					}
				}
				s0 := time.Now()
				if err = tmp.Sync(); err != nil {
					break
				}
				sinceSync = 0
				if pace != nil {
					if err = pace(0, time.Since(s0)); err != nil {
						break
					}
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	if err == nil && pace != nil {
		err = pace(0, 0)
	}
	if err == nil {
		_ = syncPath(dir)
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && pace != nil {
		err = pace(0, 0)
	}
	if err == nil {
		cut("temp " + dst)
		if last != nil {
			err = last()
		}
	}
	if err == nil {
		err = RenameFile(tmp.Name(), dst)
	}
	if err != nil {
		_ = RemoveFile(tmp.Name())
	}
	return err
}

// writeAndRename writes a temporary file beside dst and renames it over dst. The order is the
// one measured for cue write-back (#321, #253 review 3): the directory is synced before the
// temporary file (its allocation before its entry).
func writeAndRename(dst string, write func(*os.File) error) error {
	return writeAndRenameCtx(context.Background(), dst, write)
}

func writeAndRenameCtx(ctx context.Context, dst string, write func(*os.File) error) error {
	return writeAndRenameLast(ctx, dst, write, nil)
}

func writeAndRenameLast(ctx context.Context, dst string, write func(*os.File) error, last func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	if err = write(tmp); err == nil {
		cutWrite("dirsync " + dst)
		if err = ctx.Err(); err == nil {
			_ = syncPath(dir)
			cutWrite("filesync " + dst)
			if err = ctx.Err(); err == nil {
				err = tmp.Sync()
			}
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		cut("temp " + dst)
		if last != nil {
			err = last()
		}
	}
	if err == nil {
		err = RenameFileCtx(ctx, tmp.Name(), dst)
	}
	if err != nil {
		_ = RemoveFile(tmp.Name())
	}
	return err
}

// RenameFile renames src, written and synced, over dst. The replaced file is held open until the
// directory and then dst are synced: on FAT a rename frees its clusters at once, and the entry
// naming the new ones is written only later.
func RenameFile(src, dst string) error {
	return RenameFileCtx(context.Background(), src, dst)
}

// RenameFileCtx checks ctx cancellation before os.Rename and completes post-rename syncs once renamed.
func RenameFileCtx(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	if held, _ := os.Open(dst); held != nil {
		defer held.Close()
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	cut("replacing " + dst)
	_ = syncPath(filepath.Dir(dst))
	_ = syncPath(dst)
	cut("renamed " + dst)
	return nil
}

// RemoveFile removes path so that a pull leaves the file or no entry: held open, the file keeps
// its clusters until the directory is synced.
func RemoveFile(path string) error {
	if UnsafeToFree != nil && UnsafeToFree(path) {
		return ErrUnownedClusters
	}
	if held, _ := os.Open(path); held != nil {
		defer held.Close()
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	_ = syncPath(filepath.Dir(path))
	cut("removed " + path)
	return nil
}

// CleanTemps removes what a cut ReplaceFile left in dir and reports how many files went.
func CleanTemps(dir string) int {
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(name, ".enginex-rb-") && strings.HasSuffix(name, ".tmp") && RemoveFile(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n
}

func syncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
