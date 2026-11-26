// Package bridge turns the rekordbox export of a stick into the Engine library the deck shows
// (docs/design/rekordbox-bridge-v2.md): the exports are read into a view, the view is compared
// with the snapshot of the last import, and the difference is written to the live m.db in place
// before Engine sees the stick.
package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	Source string
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
	return readSourcesPaced(ctx, r, mount, files, keys, nil)
}

// cleanStaleSourceCopies removes any .rb-src-* temp files left in StageRoot by a killed background job.
func cleanStaleSourceCopies() {
	dir, err := rbstate.StageRoot()
	if err != nil || dir == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".rb-src-*"))
	for _, m := range matches {
		if st, err := os.Lstat(m); err == nil && st.Mode().IsRegular() {
			_ = os.Remove(m)
		}
	}
}

// checkExportSQLiteSidecars enforces the same clean-export check as pkg/rekordbox.ReadSQLite
// directly against the source on the stick so uncheckpointed -journal or -wal sidecars are
// always refused before any copy or read.
func checkExportSQLiteSidecars(full string) error {
	for _, suffix := range []string{"-wal", "-journal"} {
		st, err := os.Lstat(full + suffix)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!st.Mode().IsRegular() || st.Size() > 0) {
			return fmt.Errorf("OneLibrary: requires checkpointed export; source sidecar %s is present", suffix)
		}
	}
	return nil
}

type pacedReaderAt struct {
	ctx  context.Context
	f    *os.File
	pace *pacer
}

func (r *pacedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		if err := r.pace.waitNotLoading(); err != nil {
			return total, err
		}
		if err := r.ctx.Err(); err != nil {
			return total, err
		}
		end := min(total+playingReadChunk, len(p))
		playing := r.pace != nil && r.pace.active()
		t0 := time.Now()
		n, err := r.f.ReadAt(p[total:end], off+int64(total))
		took := time.Since(t0)
		total += n
		if n > 0 && playing {
			if perr := r.pace.pause(n, took); perr != nil {
				return total, perr
			}
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// readPDBPaced parses a PDB file directly in place on the stick without copying it to /data,
// halting on SongLoading and pacing chunk reads while a deck plays.
func readPDBPaced(ctx context.Context, full string, pace *pacer) (*rekordbox.Library, error) {
	if err := pace.waitNotLoading(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return rekordbox.ParsePDB(&pacedReaderAt{ctx: ctx, f: f, pace: pace}, st.Size())
}

var sourceCopyHook func(src string)

func copyStickFilePaced(ctx context.Context, src string, pace *pacer) (dst *os.File, size int64, err error) {
	if sourceCopyHook != nil {
		sourceCopyHook(src)
	}
	if err = pace.waitNotLoading(); err != nil {
		return nil, 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return nil, 0, err
	}
	defer in.Close()
	dir, derr := rbstate.StageRoot()
	if derr != nil || (dir != "" && os.MkdirAll(dir, 0o755) != nil) {
		dir = ""
	}
	out, err := os.CreateTemp(dir, ".rb-src-*")
	if err != nil {
		return nil, 0, err
	}
	cleanup := func() {
		name := out.Name()
		_ = out.Close()
		_ = os.Remove(name)
	}
	buf := make([]byte, playingReadChunk)
	for {
		if err = pace.waitNotLoading(); err != nil {
			cleanup()
			return nil, 0, err
		}
		if err = ctx.Err(); err != nil {
			cleanup()
			return nil, 0, err
		}
		playing := pace != nil && pace.active()
		t0 := time.Now()
		n, rerr := in.Read(buf)
		took := time.Since(t0)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				cleanup()
				return nil, 0, werr
			}
			size += int64(n)
			if playing {
				if perr := pace.pause(n, took); perr != nil {
					cleanup()
					return nil, 0, perr
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			cleanup()
			return nil, 0, rerr
		}
	}
	return out, size, nil
}

type pacedReader struct {
	ctx  context.Context
	r    io.Reader
	pace *pacer
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if err := r.pace.waitNotLoading(); err != nil {
		return 0, err
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > playingReadChunk {
		p = p[:playingReadChunk]
	}
	t0 := time.Now()
	n, err := r.r.Read(p)
	took := time.Since(t0)
	if n > 0 {
		if perr := r.pace.chunkPause(n, took); perr != nil {
			return n, perr
		}
	}
	return n, err
}

// readSQLitePaced reads a OneLibrary export database without continuing stick I/O across a
// SongLoading edge: encrypted SQLCipher exports stream through a loading-aware chunk reader
// directly into decryption (zero /data temp copy), while plaintext SQLite exports watch for
// loading edges and fall back to paced staging while busy.
func readSQLitePaced(ctx context.Context, full string, keys *rekordbox.KeyCache, pace *pacer) (*rekordbox.Library, error) {
	if err := pace.waitNotLoading(); err != nil {
		return nil, err
	}
	enc, err := rekordbox.IsEncryptedSQLite(full)
	if err != nil {
		return nil, err
	}
	if enc {
		return rekordbox.ReadSQLiteKeysPaced(ctx, full, nil, keys, func(r io.Reader) io.Reader {
			return bufio.NewReaderSize(&pacedReader{ctx: ctx, r: r, pace: pace}, playingReadChunk)
		})
	}
	for {
		if err := pace.waitNotLoading(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e := checkExportSQLiteSidecars(full); e != nil {
			return nil, e
		}
		if pace.isBusy() {
			tmp, _, e := copyStickFilePaced(ctx, full, pace)
			if e != nil {
				return nil, e
			}
			tmpName := tmp.Name()
			_ = tmp.Close()
			cleanup := func() {
				_ = os.Remove(tmpName)
				_ = os.Remove(tmpName + "-wal")
				_ = os.Remove(tmpName + "-shm")
				_ = os.Remove(tmpName + "-journal")
			}
			if e = checkExportSQLiteSidecars(full); e != nil {
				cleanup()
				return nil, e
			}
			l, e := rekordbox.ReadSQLiteKeys(ctx, tmpName, nil, keys)
			cleanup()
			return l, e
		}
		// Unencrypted export while idle: monitor loading edges so direct SQLite reads abort
		// immediately when SongLoading starts instead of racing into stick I/O.
		rctx, rcancel := context.WithCancel(ctx)
		var loadingHit atomic.Bool
		monDone := make(chan struct{})
		if pace != nil && pace.loading != nil {
			poll := min(max(loadingPollGap/4, 2*time.Millisecond), 15*time.Millisecond)
			go func() {
				defer close(monDone)
				t := time.NewTicker(poll)
				defer t.Stop()
				for {
					select {
					case <-rctx.Done():
						return
					case <-t.C:
						if pace.loading() {
							loadingHit.Store(true)
							rcancel()
							return
						}
					}
				}
			}()
		} else {
			close(monDone)
		}
		l, e := rekordbox.ReadSQLiteKeys(rctx, full, nil, keys)
		rcancel()
		<-monDone
		if loadingHit.Load() && ctx.Err() == nil {
			if werr := pace.waitNotLoading(); werr != nil {
				return nil, werr
			}
			continue
		}
		return l, e
	}
}

func readSourcesPaced(ctx context.Context, r *compat.Resolver, mount string, files []rbstate.FileSig, keys *rekordbox.KeyCache, pace *pacer) (libs []SourceLibrary, failed []error, err error) {
	if keys == nil {
		keys = exportKeys()
	}
	for _, f := range files {
		if err = pace.waitNotLoading(); err != nil {
			return nil, failed, err
		}
		if err = ctx.Err(); err != nil {
			return nil, failed, err
		}
		var l *rekordbox.Library
		var e error
		kind := rbstate.OneLibrary
		full := r.FullPath(f.Rel)
		if strings.EqualFold(path.Ext(f.Rel), ".pdb") {
			kind = rbstate.DeviceLibrary
			if pace == nil {
				l, e = rekordbox.ReadPDB(full)
			} else {
				l, e = readPDBPaced(ctx, full, pace)
			}
		} else if e = checkExportSQLiteSidecars(full); e != nil {
			// Dirty -journal or -wal on the stick: refuse before any read or copy.
		} else if pace == nil {
			l, e = rekordbox.ReadSQLiteKeys(ctx, full, nil, keys)
		} else {
			l, e = readSQLitePaced(ctx, full, keys, pace)
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
