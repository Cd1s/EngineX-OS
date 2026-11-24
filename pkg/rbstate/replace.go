package rbstate

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UnsafeToFree, when set, names a file on the stick whose clusters are not its own in the FAT
// (#321 review R3): freeing them would free another file's, and vfat would remount read-only.
var UnsafeToFree func(path string) bool

// ErrUnownedClusters: the file was left as it is because UnsafeToFree named it.
var ErrUnownedClusters = errors.New("rbstate: the file's clusters are not its own in the FAT")

// cut, in tests, is called between two writes a pull can separate.
var cut = func(step string) {}

const tempPattern = ".enginex-rb-*.tmp"

// ReplaceFile writes data to dst so that a pull at any moment leaves dst whole, old or new.
func ReplaceFile(dst string, data []byte) error {
	return writeAndRename(dst, func(f *os.File) error { _, err := f.Write(data); return err })
}

// InstallFile copies src, a file on another file system, to dst as ReplaceFile writes.
func InstallFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeAndRename(dst, func(f *os.File) error { _, err := io.Copy(f, in); return err })
}

// writeAndRename writes a temporary file beside dst and renames it over dst. The order is the
// one measured for cue write-back (#321, #253 review 3): the directory is synced before the
// temporary file (its allocation before its entry).
func writeAndRename(dst string, write func(*os.File) error) error {
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	if err = write(tmp); err == nil {
		_ = syncPath(dir)
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		cut("temp " + dst)
		err = RenameFile(tmp.Name(), dst)
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
