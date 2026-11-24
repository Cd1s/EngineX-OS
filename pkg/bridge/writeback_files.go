package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const (
	writeBackMarkName  = ".rb-bridge-writeback"
	writeBackRetryName = ".rb-bridge-writeback-retry"
)

func init() {
	rbstate.UnsafeToFree = unsafeToFree
}

type writePlan struct {
	dirs      []string
	targets   [][2]string
	stagedAs  map[string]string
	anlzByKey map[string][]string
}

type stagedWrite struct {
	dst        string
	tmp        string
	exportKind rbstate.LibraryKind
}

func prepareWritePlan(mount string, resolver *compat.Resolver, changes []trackChange, libs map[rbstate.LibraryKind]SourceLibrary, dateDir string) (writePlan, error) {
	plan := writePlan{stagedAs: map[string]string{}, anlzByKey: map[string][]string{}}
	seenTarget := map[string]bool{}
	seenDir := map[string]bool{}
	addDir := func(rel string) {
		d := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
		if seenDir[d] {
			return
		}
		seenDir[d] = true
		plan.dirs = append(plan.dirs, d)
	}
	addTarget := func(rel, kind string, exportKind rbstate.LibraryKind) error {
		if seenTarget[rel] {
			return nil
		}
		seenTarget[rel] = true
		full := resolver.FullPath(rel)
		if _, err := os.Stat(full); err != nil {
			return err
		}
		if unsafeToFree(full) {
			return fmt.Errorf("%w: %s; the drive needs a disk check on a computer", errUnownedClusters, rel)
		}
		name := newStagedName(filepath.Dir(full), kind)
		plan.stagedAs[full] = name
		plan.targets = append(plan.targets, [2]string{rel, name})
		addDir(rel)
		_ = exportKind
		return nil
	}
	for _, ch := range changes {
		if ch.cuesDiff && ch.analysis != "" {
			clean, err := safeANLZRelPath(ch.analysis)
			if err != nil {
				continue
			}
			base := strings.TrimSuffix(clean, filepath.Ext(clean))
			for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
				rel, ok := resolveANLZVariant(resolver, base, ext)
				if !ok {
					continue
				}
				if err := addTarget(rel, "wb", 0); err != nil {
					return writePlan{}, err
				}
				plan.anlzByKey[ch.candidate.key] = append(plan.anlzByKey[ch.candidate.key], rel)
			}
		}
		if ch.metadata[rbstate.DeviceLibrary] {
			if src, ok := libs[rbstate.DeviceLibrary]; ok {
				if err := addTarget(src.File.Rel, "pdb", rbstate.DeviceLibrary); err != nil {
					return writePlan{}, err
				}
			}
		}
		if ch.refs[rbstate.OneLibrary].ID != 0 && (ch.metadata[rbstate.OneLibrary] || ch.cuesDiff) {
			if src, ok := libs[rbstate.OneLibrary]; ok {
				if err := addTarget(src.File.Rel, "onelib", rbstate.OneLibrary); err != nil {
					return writePlan{}, err
				}
			}
		}
	}
	backupRoot := filepath.Join(mount, ".enginex-backup")
	var flush *os.File
	defer func() {
		if flush != nil {
			_ = flush.Close()
		}
	}()
	for _, t := range plan.targets {
		rel := t[0]
		if !hasFirstBackup(backupRoot, rel) {
			if flush == nil {
				if f, err := os.Open(resolver.FullPath(rel)); err == nil {
					flush = f
				}
			}
			if err := mkdirOrdered(mount, filepath.Join(backupRoot, dateDir, filepath.FromSlash(filepath.Dir(rel))), flush); err != nil {
				return writePlan{}, err
			}
		}
	}
	return plan, nil
}

func stageChanges(ctx context.Context, mount string, resolver *compat.Resolver, backupRoot, dateDir string, changes []trackChange, libs map[rbstate.LibraryKind]SourceLibrary, plan writePlan, keys *rekordbox.KeyCache, tm *passTimes) ([]stagedWrite, map[string][]rbstate.FileSig, error) {
	var staged []stagedWrite
	written := map[string][]rbstate.FileSig{}
	stageBytes := func(dst string, data []byte, kind rbstate.LibraryKind) error {
		tmp, err := stageWriteFile(dst, plan.stagedAs[dst], data)
		if err != nil {
			return err
		}
		writeBackCut("temp " + dst)
		staged = append(staged, stagedWrite{dst: dst, tmp: tmp, exportKind: kind})
		return nil
	}
	oneCues := map[uint32][]rekordbox.Cue{}
	oneMeta := map[uint32]rekordbox.Metadata{}
	pdbMeta := map[uint32]rekordbox.Metadata{}
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		for kind, tr := range ch.refs {
			if ch.metadata[kind] {
				md := ch.candidate.metadata
				if kind == rbstate.OneLibrary {
					oneMeta[tr.ID] = md
				} else if kind == rbstate.DeviceLibrary {
					pdbMeta[tr.ID] = md
				}
			}
			if kind == rbstate.OneLibrary && ch.cuesDiff {
				oneCues[tr.ID] = ch.cues
			}
		}
		if !ch.cuesDiff || ch.analysis == "" {
			continue
		}
		clean, err := safeANLZRelPath(ch.analysis)
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(clean, filepath.Ext(clean))
		for _, ext := range []struct {
			suffix   string
			extended bool
			onlyCue  bool
		}{
			{suffix: ".DAT"},
			{suffix: ".EXT", extended: true},
			{suffix: ".2EX", extended: true, onlyCue: true},
		} {
			rel, ok := resolveANLZVariant(resolver, base, ext.suffix)
			if !ok {
				continue
			}
			full := resolver.FullPath(rel)
			raw, err := os.ReadFile(full)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, nil, err
			}
			updated, changed, err := rekordbox.RewriteANLZCues(raw, ch.cues, ext.extended, ext.onlyCue)
			if err != nil {
				return nil, nil, fmt.Errorf("writeback: %s: %w", rel, err)
			}
			if !changed {
				continue
			}
			n, err := ensureFirstOriginalBackup(mount, backupRoot, dateDir, rel)
			if err != nil {
				return nil, nil, err
			}
			tm.bytes += n + int64(len(updated))
			if err := stageBytes(full, updated, 0); err != nil {
				return nil, nil, err
			}
			written[ch.candidate.key] = append(written[ch.candidate.key], rbstate.FileSig{Rel: rel})
		}
	}
	if src, ok := libs[rbstate.OneLibrary]; ok && (len(oneCues) > 0 || len(oneMeta) > 0) {
		rel := src.File.Rel
		full := resolver.FullPath(rel)
		n, err := ensureFirstOriginalBackup(mount, backupRoot, dateDir, rel)
		if err != nil {
			return nil, nil, err
		}
		tm.bytes += n
		tmpPath := filepath.Join(filepath.Dir(full), plan.stagedAs[full])
		out, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, nil, err
		}
		_ = out.Close()
		started := time.Now()
		upd, err := rekordbox.UpdateOneLibraryEditsKeys(ctx, full, oneCues, oneMeta, tmpPath, keys)
		tm.decrypt += upd.Decrypt
		tm.edit += upd.Edit
		tm.encrypt += upd.Encrypt
		tm.encrypt += time.Since(started) - upd.Decrypt - upd.Edit - upd.Encrypt
		if err != nil {
			_ = rbstate.RemoveFile(tmpPath)
			return nil, nil, err
		}
		if st, err := os.Stat(tmpPath); err == nil {
			tm.bytes += st.Size()
		}
		writeBackCut("temp " + full)
		staged = append(staged, stagedWrite{dst: full, tmp: tmpPath, exportKind: rbstate.OneLibrary})
	}
	if src, ok := libs[rbstate.DeviceLibrary]; ok && len(pdbMeta) > 0 {
		rel := src.File.Rel
		full := resolver.FullPath(rel)
		n, err := ensureFirstOriginalBackup(mount, backupRoot, dateDir, rel)
		if err != nil {
			return nil, nil, err
		}
		tm.bytes += n
		data, err := pdbMetadataBytes(ctx, full, pdbMeta)
		if err != nil {
			return nil, nil, err
		}
		if err := stageBytes(full, data, rbstate.DeviceLibrary); err != nil {
			return nil, nil, err
		}
		tm.bytes += int64(len(data))
	}
	return staged, written, nil
}

func newStagedName(dir, kind string) string {
	for {
		name := fmt.Sprintf(".enginex-%s-%d.tmp", kind, rand.Uint32())
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			return name
		}
	}
}

func stagedName(name string) bool {
	return strings.HasPrefix(name, ".enginex-") && strings.HasSuffix(name, ".tmp") && !strings.ContainsAny(name, `/\\`)
}

func markWriteBack(engineDBDir, dateDir string, dirs []string, targets [][2]string) error {
	seen := map[string]bool{}
	var b strings.Builder
	for _, d := range dirs {
		for _, x := range []string{d, filepath.Join(".enginex-backup", dateDir, d)} {
			x = filepath.ToSlash(filepath.Clean(x))
			if !seen[x] {
				seen[x] = true
				b.WriteString(x + "\n")
			}
		}
	}
	for _, target := range targets {
		b.WriteString("= " + target[1] + " " + filepath.ToSlash(target[0]) + "\n")
	}
	return rbstate.ReplaceFile(filepath.Join(engineDBDir, writeBackMarkName), []byte(b.String()))
}

func markPath(mount, line string, missing bool) (string, bool) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(line)))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	p := mount
	for i, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return rel, missing && i == len(strings.Split(rel, string(filepath.Separator)))-1 && errors.Is(err, os.ErrNotExist)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return rel, true
}

func stageWriteFile(dst, name string, data []byte) (string, error) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(dst), name), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	path := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = rbstate.RemoveFile(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func replaceFile(tmp, dst string) error {
	if unsafeToFree(dst) {
		return errUnownedClusters
	}
	held, _ := os.Open(dst)
	if held != nil {
		defer held.Close()
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	writeBackCut("replacing " + dst)
	_ = syncPath(filepath.Dir(dst))
	_ = syncPath(dst)
	writeBackCut("renamed " + dst)
	return nil
}

func removeFile(path string) error {
	if unsafeToFree(path) {
		return errUnownedClusters
	}
	return rbstate.RemoveFile(path)
}

func mkdirOrdered(root, dir string, flush *os.File) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		p = filepath.Join(p, part)
		if err := os.Mkdir(p, 0o755); err == nil {
			if flush != nil {
				_ = flush.Sync()
			}
			_ = syncPath(filepath.Dir(p))
		} else if st, statErr := os.Stat(p); statErr != nil || !st.IsDir() {
			return err
		}
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func stagedWhole(tmp, dst string) bool {
	switch strings.ToLower(filepath.Base(dst)) {
	case "export.pdb":
		_, err := rekordbox.ReadPDB(tmp)
		return err == nil && rekordbox.CheckPDBExtent(tmp) == nil
	case "exportlibrary.db":
		_, err := rekordbox.ReadSQLiteWhole(context.Background(), tmp)
		return err == nil
	default:
		b, err := os.ReadFile(tmp)
		if err != nil {
			return false
		}
		_, err = rekordbox.ParseAnalysis(b)
		return err == nil
	}
}

func putBack(mount, rel, name string) (bool, bool) {
	dst := filepath.Join(mount, rel)
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		return true, false
	}
	copy := filepath.Join(filepath.Dir(dst), name)
	if st, err := os.Lstat(copy); err == nil && st.Mode().IsRegular() && stagedWhole(copy, dst) && replaceFile(copy, dst) == nil {
		log.Printf("rb-bridge: %s was missing after a cut write-back; its new copy put back", dst)
		return true, false
	}
	entries, _ := os.ReadDir(filepath.Join(mount, ".enginex-backup"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", entry.Name(), filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		tmp, err := stageWriteFile(dst, newStagedName(filepath.Dir(dst), "restore"), data)
		if err == nil && replaceFile(tmp, dst) == nil {
			log.Printf("rb-bridge: %s was missing after a cut write-back; its original put back", dst)
			return true, true
		}
	}
	return false, false
}

func CleanWriteBackLeftovers(mount string) int {
	dir := filepath.Join(mount, "Engine Library", "Database2")
	n := rbstate.CleanTemps(dir)
	mark := filepath.Join(dir, writeBackMarkName)
	data, err := os.ReadFile(mark)
	if err != nil {
		return n
	}
	whole := true
	var dirs []string
	for _, line := range strings.Split(string(data), "\n") {
		rest, target := strings.CutPrefix(line, "= ")
		name := ""
		if target {
			var ok bool
			name, rest, ok = strings.Cut(rest, " ")
			if !ok || !stagedName(name) {
				continue
			}
		}
		rel, ok := markPath(mount, rest, target)
		if !ok {
			continue
		}
		if !target {
			dirs = append(dirs, rel)
			continue
		}
		if put, _ := putBack(mount, rel, name); !put {
			whole = false
		}
	}
	if !whole {
		return n
	}
	for _, rel := range dirs {
		for _, pattern := range []string{".enginex-*.tmp", ".bak-*.tmp"} {
			matches, _ := filepath.Glob(filepath.Join(mount, rel, pattern))
			for _, path := range matches {
				if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && removeFile(path) == nil {
					n++
				}
			}
		}
	}
	_ = removeFile(mark)
	_ = os.WriteFile(writeBackRetryPath(mount), []byte("replay\n"), 0o600)
	refreshRecoveredSnapshotStamp(mount)
	return n
}
