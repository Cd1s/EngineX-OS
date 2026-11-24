// Package bridge turns the rekordbox export of a stick into the Engine library the deck shows
// (docs/design/rekordbox-bridge-v2.md). One pipeline serves every insert: the exports are read
// into a view, the view is compared with the snapshot of the last import, and the difference
// is written to the live m.db in place before Engine sees the stick.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The export files of a stick. A stick can hold both libraries; neither is a superset of the
// other (a DJ edits them separately in rekordbox 7), so both are read and joined by path.
const (
	devicePDB      = "PIONEER/rekordbox/export.pdb"
	oneLibraryDB   = "PIONEER/rekordbox/exportLibrary.db"
	oneLibraryDB2  = "PIONEER/rekordbox/exportOneLibrary.db" // read when the stick has no exportLibrary.db
	databaseSubdir = "Engine Library/Database2"
)

// KindOf says which library an export file is.
func KindOf(rel string) rbstate.LibraryKind {
	if strings.EqualFold(path.Ext(rel), ".pdb") {
		return rbstate.DeviceLibrary
	}
	return rbstate.OneLibrary
}

// FindSources stats the export files of the volume r resolves. No file is opened. An
// export.pdb the deck generated itself for Pro DJ Link is not a source. Device Library comes
// first, then OneLibrary; file times play no part.
func FindSources(r *compat.Resolver, mount string) ([]rbstate.FileSig, error) {
	stat := func(p string) (rbstate.FileSig, bool, error) {
		rel, err := r.Resolve(p)
		if errors.Is(err, os.ErrNotExist) {
			return rbstate.FileSig{}, false, nil
		}
		if err != nil {
			return rbstate.FileSig{}, false, err
		}
		st, err := os.Stat(r.FullPath(rel))
		if errors.Is(err, os.ErrNotExist) {
			return rbstate.FileSig{}, false, nil
		}
		if err != nil {
			return rbstate.FileSig{}, false, err
		}
		if !st.Mode().IsRegular() {
			return rbstate.FileSig{}, false, nil
		}
		return rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()}, true, nil
	}
	var files []rbstate.FileSig
	if !prolink.GeneratedExport(mount) {
		f, ok, err := stat(devicePDB)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, f)
		}
	}
	for _, p := range []string{oneLibraryDB, oneLibraryDB2} {
		f, ok, err := stat(p)
		if err != nil {
			return nil, err
		}
		if ok {
			files = append(files, f)
			break
		}
	}
	return files, nil
}

// SourceLibrary is one library as pkg/rekordbox read it.
type SourceLibrary struct {
	Kind   rbstate.LibraryKind
	Source string // the staged library's name for a LinkLibrary
	File   rbstate.FileSig
	Lib    *rekordbox.Library
}

// keyCacheEnv names pkg/rekordbox's directory of derived OneLibrary keys.
const keyCacheEnv = "ENGINEX_RB_SHARED_KEY_CACHE"

// keyDataDir is the deck's own data directory.
var keyDataDir = "/data/enginex"

// persistentKeyDir is where the keys are kept across power-ons, "" when dataDir is not there.
// pkg/rekordbox's own default is in /run: every first insert after a power-on then derived the
// key again, 5.5 s at deck speed. The key follows from the export file's salt and rekordbox's
// public passphrase alone, so it is no secret of the DJ's, and a cached key is checked against
// the file's first page before it is used.
func persistentKeyDir(dataDir string) string {
	if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(dataDir, "rb-keys")
}

// newKeyCache is a key cache of pkg/rekordbox that reads and writes the persistent directory.
// A test, a caller that named a directory itself and a process that is not root keep
// pkg/rekordbox's choice.
func newKeyCache() *rekordbox.KeyCache {
	if !testing.Testing() && os.Getenv(keyCacheEnv) == "" && os.Geteuid() == 0 {
		if dir := persistentKeyDir(keyDataDir); dir != "" {
			os.Setenv(keyCacheEnv, dir)
		}
	}
	return rekordbox.NewKeyCache()
}

// exportKeys keeps the SQLCipher keys of OneLibrary exports for this process.
var exportKeys = sync.OnceValue(newKeyCache)

// ReadSources reads each export once. A library that cannot be read is reported in failed and
// left out, so one broken file does not hide the other; with none readable err is set.
func ReadSources(ctx context.Context, r *compat.Resolver, mount string, files []rbstate.FileSig, keys *rekordbox.KeyCache) (libs []SourceLibrary, failed []error, err error) {
	if keys == nil {
		keys = exportKeys()
	}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return nil, failed, err
		}
		var l *rekordbox.Library
		var e error
		kind := KindOf(f.Rel)
		if kind == rbstate.DeviceLibrary {
			l, e = rekordbox.ReadPDB(r.FullPath(f.Rel))
		} else {
			l, e = rekordbox.ReadSQLiteKeys(ctx, r.FullPath(f.Rel), nil, keys)
		}
		if e != nil {
			if ctx.Err() != nil {
				return nil, failed, ctx.Err()
			}
			failed = append(failed, fmt.Errorf("%s: %w", f.Rel, e))
			continue
		}
		libs = append(libs, SourceLibrary{Kind: kind, File: f, Lib: l})
	}
	if len(libs) == 0 && len(failed) > 0 {
		return nil, failed, errors.Join(failed...)
	}
	return libs, failed, nil
}

// stagedNames lists the libraries staged from Pioneer players into
// <mount>/.prolink-cache/<name>/ (#59), in name order.
func stagedNames(mount string) []string {
	dirs, _ := os.ReadDir(filepath.Join(mount, prolink.StagedDir))
	var names []string
	for _, d := range dirs {
		if d.IsDir() && d.Type()&os.ModeSymlink == 0 && stagedPDB(filepath.Join(mount, prolink.StagedDir, d.Name())) != "" {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	return names
}

// stagedPDB uses the same hidden-directory and case rules as a stick's own export.
func stagedPDB(root string) string {
	r, err := compat.NewResolver(root)
	if err != nil {
		return ""
	}
	rel, err := r.Resolve(devicePDB)
	if err != nil {
		return ""
	}
	return r.FullPath(rel)
}

// stagedStamp changes whenever a staged library is written again.
func stagedStamp(mount string) string {
	var parts []string
	for _, name := range stagedNames(mount) {
		if st, err := os.Stat(stagedPDB(filepath.Join(mount, prolink.StagedDir, name))); err == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", name, st.Size(), st.ModTime().UnixNano()))
		}
	}
	return strings.Join(parts, " ")
}

// readStaged reads the staged CDJ libraries, their paths pointing into the staged copies. A
// broken one is left out with a log line: it must never fail the stick's own import.
func readStaged(mount string) []SourceLibrary {
	var out []SourceLibrary
	for _, name := range stagedNames(mount) {
		root := filepath.Join(mount, prolink.StagedDir, name)
		staged, err := rekordbox.ReadPDB(stagedPDB(root))
		if err != nil {
			log.Printf("rb-bridge: staged %s: %v", name, err)
			continue
		}
		resolver, err := compat.NewResolver(root)
		if err != nil {
			continue
		}
		into := func(p string) string {
			rel, ok := prolink.SafeRel(p)
			if !ok {
				return ""
			}
			rel, err := resolver.Resolve(rel)
			if err != nil {
				return ""
			}
			return "/" + path.Join(prolink.StagedDir, name, filepath.ToSlash(rel))
		}
		kept := staged.Tracks[:0:0]
		for _, t := range staged.Tracks {
			if t.ID == 0 {
				continue
			}
			if t.Path = into(t.Path); t.Path == "" {
				continue // its audio has not been copied yet
			}
			t.AnalysisPath, t.ArtworkPath = into(t.AnalysisPath), into(t.ArtworkPath)
			t.RecordHash = ""
			kept = append(kept, t)
		}
		staged.Tracks = kept
		if err := staged.Validate(); err != nil {
			log.Printf("rb-bridge: staged %s left out: %v", name, err)
			continue
		}
		out = append(out, SourceLibrary{Kind: rbstate.LinkLibrary, Source: name, Lib: staged})
	}
	return out
}

// ReadLayout is the setting rb_library_view of the deck's settings file: how a stick with two
// libraries shows them. Anything else than the three fixed choices is the three folders.
func ReadLayout(settingsPath string) string {
	switch readSetting(settingsPath, "rb_library_view") {
	case rbstate.LayoutMerged:
		return rbstate.LayoutMerged
	case rbstate.LayoutDevice:
		return rbstate.LayoutDevice
	case rbstate.LayoutOneLibrary:
		return rbstate.LayoutOneLibrary
	}
	return rbstate.LayoutFolders
}

// readSetting returns the value of key in a key=value settings file, "" when it has none.
func readSetting(settingsPath, key string) string {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
