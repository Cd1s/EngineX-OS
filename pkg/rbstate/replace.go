package rbstate

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UnsafeToFree, when set, reports a file on the stick whose clusters are not its own in the
// FAT (#321 review R3): replacing or removing such a file frees clusters another file owns,
// and vfat then remounts the stick read-only. pkg/bridge sets it to its FAT chain check.
var UnsafeToFree func(path string) bool

// ErrUnownedClusters: the file was left as it is because UnsafeToFree named it.
var ErrUnownedClusters = errors.New("rbstate: the file's clusters are not its own in the FAT")

// cut, in tests, is called between two writes a pull can separate.
var cut = func(step string) {}

const tempPattern = ".enginex-rb-*.tmp"

// ReplaceFile writes data to dst so that a pull at any moment leaves dst whole, old or new.
// The order is the one measured for cue write-back (#321, #253 review 3). The temporary
// file's directory is synced before the file (its allocation before its entry). The replaced
// file is held open across the rename until the directory and then dst are on the stick: on
// FAT a rename frees the replaced file's clusters at once, and the entry naming the new ones
// is written only later.
func ReplaceFile(dst string, data []byte) error {
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = RemoveFile(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	_ = syncPath(dir)
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	cut("temp " + dst)
	held, _ := os.Open(dst)
	if held != nil {
		defer held.Close()
	}
	if err = os.Rename(name, dst); err != nil {
		return err
	}
	ok = true
	cut("replacing " + dst)
	_ = syncPath(dir)
	_ = syncPath(dst)
	cut("renamed " + dst)
	return nil
}

// RenameFile renames src, already written and synced, over dst with the same order as ReplaceFile.
func RenameFile(src, dst string) error {
	if UnsafeToFree != nil && UnsafeToFree(dst) {
		return ErrUnownedClusters
	}
	held, _ := os.Open(dst)
	if held != nil {
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

// InstallFile copies src, a file on another file system, to dst so that a pull leaves dst whole,
// old or new: a temporary copy beside dst, synced, then RenameFile.
func InstallFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), tempPattern)
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = io.Copy(tmp, in)
	if err == nil {
		_ = syncPath(filepath.Dir(dst))
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = RenameFile(name, dst)
	}
	if err != nil {
		_ = RemoveFile(name)
	}
	return err
}

// RemoveFile removes path so that a pull leaves either the file or no entry for it: held open,
// the file keeps its clusters until the directory is synced.
func RemoveFile(path string) error {
	if UnsafeToFree != nil && UnsafeToFree(path) {
		return ErrUnownedClusters
	}
	held, _ := os.Open(path)
	if held != nil {
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(name, ".enginex-rb-") && strings.HasSuffix(name, ".tmp") {
			if RemoveFile(filepath.Join(dir, name)) == nil {
				n++
			}
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
