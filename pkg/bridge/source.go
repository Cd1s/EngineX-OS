// Package bridge turns the rekordbox export of a stick into the Engine library the deck shows
// (docs/design/rekordbox-bridge-v2.md): the exports are read into a view, the view is compared
// with the snapshot of the last import, and the difference is written to the live m.db in place
// before Engine sees the stick.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The export files of a stick. Neither library is a superset of the other (#327): both are read
// and joined by path.
const (
	devicePDB      = "PIONEER/rekordbox/export.pdb"
	oneLibraryDB   = "PIONEER/rekordbox/exportLibrary.db"
	oneLibraryDB2  = "PIONEER/rekordbox/exportOneLibrary.db" // read when the stick has no exportLibrary.db
	databaseSubdir = "Engine Library/Database2"
)

// FindSources stats the export files of the volume r resolves, Device Library first; no file is
// opened and file times play no part. An export.pdb the deck made for Pro DJ Link is no source.
func FindSources(r *compat.Resolver, mount string) ([]rbstate.FileSig, error) {
	var files []rbstate.FileSig
	for _, names := range [][]string{{devicePDB}, {oneLibraryDB, oneLibraryDB2}} {
		if names[0] == devicePDB && prolink.GeneratedExport(mount) {
			continue
		}
		for _, p := range names {
			rel, err := r.Resolve(p)
			var st os.FileInfo
			if err == nil {
				st, err = os.Stat(r.FullPath(rel))
			}
			if errors.Is(err, os.ErrNotExist) || err == nil && !st.Mode().IsRegular() {
				continue
			} else if err != nil {
				return nil, err
			}
			files = append(files, rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()})
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

const keyCacheEnv = "ENGINEX_RB_SHARED_KEY_CACHE" // pkg/rekordbox's directory of derived OneLibrary keys

var keyDataDir = "/data/enginex" // the deck's own data directory

// persistentKeyDir keeps OneLibrary keys across power-ons, "" when dataDir is not there:
// pkg/rekordbox's default in /run cost a 5.5 s key derivation at the first insert after each
// power-on. A key follows from the file's salt and rekordbox's public passphrase, so it is no
// secret of the DJ's, and a cached key is checked against the file before it is used.
func persistentKeyDir(dataDir string) string {
	if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
		return ""
	}
	return filepath.Join(dataDir, "rb-keys")
}

// newKeyCache is a key cache in the persistent directory without mutating the process environment.
func newKeyCache() *rekordbox.KeyCache {
	if !testing.Testing() && os.Getenv(keyCacheEnv) == "" && os.Geteuid() == 0 {
		if dir := persistentKeyDir(keyDataDir); dir != "" {
			return rekordbox.NewKeyCacheAt(dir)
		}
	}
	return rekordbox.NewKeyCache()
}

var exportKeys = sync.OnceValue(newKeyCache) // this process's OneLibrary keys

// ReadSources reads each export once. A library that cannot be read is reported in failed and
// left out, so one broken file does not hide the other; with none readable err is set. keys nil
// uses the process's key cache.
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
		kind := rbstate.OneLibrary
		if strings.EqualFold(path.Ext(f.Rel), ".pdb") {
			kind = rbstate.DeviceLibrary
			l, e = rekordbox.ReadPDB(r.FullPath(f.Rel))
		} else {
			l, e = rekordbox.ReadSQLiteKeys(ctx, r.FullPath(f.Rel), nil, keys)
		}
		if e != nil && ctx.Err() != nil {
			return nil, failed, ctx.Err()
		} else if e != nil {
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

// stagedLibraries lists the libraries staged from Pioneer players into
// <mount>/.prolink-cache/<name>/ (#59) in name order, each with its export.pdb, found by the same
// hidden-directory and case rules as a stick's own.
func stagedLibraries(mount string) (names, pdbs []string) {
	dirs, _ := os.ReadDir(filepath.Join(mount, prolink.StagedDir))
	for _, d := range dirs { // os.ReadDir sorts by name
		if !d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			continue
		}
		r, err := compat.NewResolver(filepath.Join(mount, prolink.StagedDir, d.Name()))
		if err != nil {
			continue
		}
		if rel, err := r.Resolve(devicePDB); err == nil {
			names, pdbs = append(names, d.Name()), append(pdbs, r.FullPath(rel))
		}
	}
	return names, pdbs
}

// stagedStamp changes whenever a staged library is written again.
func stagedStamp(mount string) string {
	var parts []string
	names, pdbs := stagedLibraries(mount)
	for i, name := range names {
		if st, err := os.Stat(pdbs[i]); err == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", name, st.Size(), st.ModTime().UnixNano()))
		}
	}
	return strings.Join(parts, " ")
}

// readStaged reads the staged CDJ libraries, their paths pointing into the staged copies. A broken
// one is left out with a log line: it never fails the stick's own import.
func readStaged(mount string) []SourceLibrary {
	var out []SourceLibrary
	names, pdbs := stagedLibraries(mount)
	for i, name := range names {
		staged, err := rekordbox.ReadPDB(pdbs[i])
		if err != nil {
			log.Printf("rb-bridge: staged %s: %v", name, err)
			continue
		}
		resolver, err := compat.NewResolver(filepath.Join(mount, prolink.StagedDir, name))
		if err != nil {
			continue
		}
		into := func(p string) string {
			rel, ok := prolink.SafeRel(p)
			if !ok {
				return ""
			}
			if rel, e := resolver.Resolve(rel); e == nil {
				return "/" + path.Join(prolink.StagedDir, name, filepath.ToSlash(rel))
			}
			return ""
		}
		kept := staged.Tracks[:0]
		for _, t := range staged.Tracks {
			if t.Path = into(t.Path); t.ID != 0 && t.Path != "" { // "": its audio has not been copied yet
				t.AnalysisPath, t.ArtworkPath, t.RecordHash = into(t.AnalysisPath), into(t.ArtworkPath), ""
				kept = append(kept, t)
			}
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

// ReadLayout is the setting rb_library_view: how a stick with two libraries shows them. Anything
// but the three fixed choices is the three folders.
func ReadLayout(settingsPath string) string {
	if v := readSetting(settingsPath, "rb_library_view"); slices.Contains([]string{rbstate.LayoutMerged, rbstate.LayoutDevice, rbstate.LayoutOneLibrary}, v) {
		return v
	}
	return rbstate.LayoutFolders
}

// readSetting returns the value of key in a key=value settings file, "" when it has none.
func readSetting(settingsPath, key string) string {
	b, _ := os.ReadFile(settingsPath)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
