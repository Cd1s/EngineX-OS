package bridge

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const writeBackMarkName = ".rb-bridge-writeback"

// target is a file a pass replaces: a track's ANLZ file (kind 0) or a library.
type target struct {
	rel, full, tmp string // tmp: the name of its staged copy, beside it
	kind           rbstate.LibraryKind
	key            string          // ANLZ: the track
	cues           []rekordbox.Cue // ANLZ
	ext            int             // ANLZ: the index in anlzExts
}

var (
	anlzExts   = []string{".DAT", ".EXT", ".2EX"}
	stagedKind = map[rbstate.LibraryKind]string{0: "wb", rbstate.DeviceLibrary: "pdb", rbstate.OneLibrary: "onelib"}
)

// write replaces the files of changes in the order of #321: first-original backups and their
// folders, the mark naming every folder and staged copy, every staged copy synced, then the
// renames. It returns the ANLZ files written, by track.
func (p *pass) write(ctx context.Context, libs map[rbstate.LibraryKind]SourceLibrary, changes []trackChange) (map[string][]rbstate.FileSig, recordHashes, error) {
	var targets []*target
	seen := map[string]bool{}
	add := func(t *target) error {
		if seen[t.rel] {
			return nil
		}
		seen[t.rel] = true
		t.full = p.r.FullPath(t.rel)
		if _, err := os.Stat(t.full); err != nil {
			return err
		}
		// Decided before anything is allocated on the volume (#321 review R3).
		if unsafeToFree(t.full) {
			return fmt.Errorf("%w: %s; the drive needs a disk check on a computer", errUnownedClusters, t.rel)
		}
		t.tmp = newStagedName(stagedKind[t.kind])
		targets = append(targets, t)
		return nil
	}
	oneCues, oneMeta, pdbMeta := map[uint32][]rekordbox.Cue{}, map[uint32]rekordbox.Metadata{}, map[uint32]rekordbox.Metadata{}
	for _, ch := range changes {
		for kind, tr := range ch.refs {
			if kind == rbstate.OneLibrary && ch.metadata[kind] {
				oneMeta[tr.ID] = ch.candidate.metadata
			} else if kind == rbstate.DeviceLibrary && ch.metadata[kind] {
				pdbMeta[tr.ID] = ch.candidate.metadata
			}
			if kind == rbstate.OneLibrary && ch.cuesDiff {
				oneCues[tr.ID] = ch.cues
			}
		}
		if base, ok := anlzBase(ch.analysis); ch.cuesDiff && ok {
			for i, ext := range anlzExts {
				if rel, ok := resolveANLZVariant(p.r, base, ext); ok {
					if err := add(&target{rel: rel, key: ch.candidate.key, cues: ch.cues, ext: i}); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}
	for _, kind := range []rbstate.LibraryKind{rbstate.OneLibrary, rbstate.DeviceLibrary} {
		edits := len(pdbMeta)
		if kind == rbstate.OneLibrary {
			edits = len(oneCues) + len(oneMeta)
		}
		if src, ok := libs[kind]; ok && edits > 0 {
			if err := add(&target{rel: src.File.Rel, kind: kind}); err != nil {
				return nil, nil, err
			}
		}
	}

	backupRoot := filepath.Join(p.mount, ".enginex-backup")
	dateDir := cmp.Or(p.opts.DateDir, time.Now().Format("2006-01-02"))
	var flush *os.File
	defer func() { _ = flush.Close() }()
	for _, t := range targets {
		if hasFirstBackup(backupRoot, t.rel) {
			continue
		}
		if flush == nil {
			flush, _ = os.Open(t.full)
		}
		if err := mkdirOrdered(p.mount, filepath.Join(backupRoot, dateDir, filepath.Dir(t.rel)), flush); err != nil {
			return nil, nil, err
		}
	}
	if err := markWriteBack(p.mount, dateDir, targets); err != nil {
		return nil, nil, err
	}

	var staged []*target
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		var data []byte
		if t.kind != rbstate.OneLibrary {
			raw, err := os.ReadFile(t.full)
			if t.kind == 0 && errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, nil, err
			}
			changed := true
			if t.kind == 0 {
				if data, changed, err = rekordbox.RewriteANLZCues(raw, t.cues, t.ext > 0, t.ext == 2); err != nil {
					err = fmt.Errorf("writeback: %s: %w", t.rel, err)
				}
			} else {
				data, err = rekordbox.RewritePDBMetadata(raw, pdbMeta)
			}
			if err != nil {
				return nil, nil, err
			} else if !changed {
				continue
			}
		}
		n, err := ensureFirstOriginalBackup(p.mount, backupRoot, dateDir, t.rel)
		if err != nil {
			return nil, nil, err
		}
		tmp := filepath.Join(filepath.Dir(t.full), t.tmp)
		f, err := createStaged(tmp)
		if err != nil {
			return nil, nil, err
		}
		size := int64(len(data))
		if t.kind == rbstate.OneLibrary {
			_ = f.Close()
			started := time.Now()
			upd, err := rekordbox.UpdateOneLibraryEditsKeys(ctx, t.full, oneCues, oneMeta, tmp, p.keys)
			p.phase["decrypt"] += upd.Decrypt
			p.phase["edit"] += upd.Edit
			p.phase["encrypt"] += time.Since(started) - upd.Decrypt - upd.Edit
			if err != nil {
				_ = rbstate.RemoveFile(tmp)
				return nil, nil, err
			}
			if st, err := os.Stat(tmp); err == nil {
				size = st.Size()
			}
		} else if _, err := writeSynced(f, bytes.NewReader(data)); err != nil {
			return nil, nil, err
		}
		p.res.BytesWritten += n + size
		writeBackCut("temp " + t.full)
		staged = append(staged, t)
	}

	for _, t := range staged {
		if p.opts.BeforeRename != nil {
			if err := p.opts.BeforeRename(t.full); err != nil {
				return nil, nil, err
			}
		}
	}
	started := time.Now()
	written := map[string][]rbstate.FileSig{}
	for _, t := range staged {
		if err := replaceFile(filepath.Join(filepath.Dir(t.full), t.tmp), t.full); err != nil {
			return nil, nil, err
		}
		p.res.FilesUpdated++
		p.res.OneLibraryUpdated = p.res.OneLibraryUpdated || t.kind == rbstate.OneLibrary
		if t.kind == 0 {
			sig := rbstate.FileSig{Rel: t.rel}
			if st, err := os.Stat(t.full); err == nil {
				sig.Size, sig.MTimeNS = st.Size(), st.ModTime().UnixNano()
			}
			written[t.key] = append(written[t.key], sig)
		}
	}
	p.phase["write"] = time.Since(started)
	if p.res.FilesUpdated > 0 {
		p.res.BackupDir = filepath.Join(".enginex-backup", dateDir)
	}
	hashes := make(recordHashes, len(changes))
	rows := make([]dbCandidate, len(changes))
	for i, change := range changes {
		rows[i] = change.candidate
	}
	refreshed, err := p.readLibraries(ctx, rows)
	if err != nil {
		return nil, nil, err
	}
	for _, change := range changes {
		for _, ref := range change.candidate.track.Refs {
			for libraryKind, library := range refreshed {
				if library.Lib == nil {
					continue
				}
				for _, track := range library.Lib.Tracks {
					if track.ID == ref.ID && rbstate.FoldPath(track.Path) == change.candidate.key {
						if hashes[change.candidate.key] == nil {
							hashes[change.candidate.key] = map[rbstate.LibraryKind]string{}
						}
						hashes[change.candidate.key][libraryKind] = track.RecordHash
						break
					}
				}
			}
		}
	}
	return written, hashes, nil
}

// ensureFirstOriginalBackup copies rel to the backup folder of dateDir unless any backup folder
// has it already: the first original is kept. It returns the bytes copied.
func ensureFirstOriginalBackup(mount, backupRoot, dateDir, rel string) (int64, error) {
	if hasFirstBackup(backupRoot, rel) {
		return 0, nil
	}
	dst := filepath.Join(backupRoot, dateDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(filepath.Join(mount, filepath.FromSlash(rel)))
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bak-*.tmp")
	if err != nil {
		return 0, err
	}
	n, err := writeSynced(tmp, in)
	if err != nil {
		return 0, err
	}
	writeBackCut("backup temp " + dst)
	if err := replaceFile(tmp.Name(), dst); err != nil {
		_ = rbstate.RemoveFile(tmp.Name())
		return 0, err
	}
	writeBackCut("backup " + dst)
	return n, nil
}

func hasFirstBackup(root, rel string) bool {
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if _, err := os.Stat(filepath.Join(root, entry.Name(), filepath.FromSlash(rel))); entry.IsDir() && err == nil {
			return true
		}
	}
	return false
}

// newStagedName is a new staged copy's name; createStaged refuses one that exists.
func newStagedName(kind string) string { return fmt.Sprintf(".enginex-%s-%d.tmp", kind, rand.Uint64()) }

func stagedName(name string) bool {
	return strings.HasPrefix(name, ".enginex-") && strings.HasSuffix(name, ".tmp") && !strings.ContainsAny(name, `/\\`)
}

func createStaged(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// writeSynced writes r to f, a new file, then syncs and closes it; on failure f is removed.
func writeSynced(f *os.File, r io.Reader) (int64, error) {
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = rbstate.RemoveFile(f.Name())
	}
	return n, err
}

// markWriteBack names, before a pass writes beside them, every folder it may leave a temporary
// file in, and after "= " each target with the name of its staged copy: "= <name> <file>".
func markWriteBack(mount, dateDir string, targets []*target) error {
	seen := map[string]bool{}
	var b strings.Builder
	for _, t := range targets {
		d := path.Dir(filepath.ToSlash(t.rel))
		for _, x := range []string{d, path.Join(".enginex-backup", dateDir, d)} {
			if !seen[x] {
				seen[x] = true
				b.WriteString(x + "\n")
			}
		}
	}
	for _, t := range targets {
		b.WriteString("= " + t.tmp + " " + filepath.ToSlash(t.rel) + "\n")
	}
	mark := filepath.Join(mount, "Engine Library", "Database2", writeBackMarkName)
	writeBackCut("temp " + mark)
	return rbstate.ReplaceFile(mark, []byte(b.String()))
}

// markPath is a mark line's path inside mount; refused when it leaves mount, passes a symlink or
// is missing (unless missing allows its last part to be missing).
func markPath(mount, line string, missing bool) (string, bool) {
	rel := filepath.Clean(filepath.FromSlash(strings.TrimSpace(line)))
	if rel == "." || !filepath.IsLocal(rel) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	p := mount
	for i, part := range parts {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return rel, missing && i == len(parts)-1 && errors.Is(err, os.ErrNotExist)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return rel, true
}

// replaceFile renames tmp, written and synced, over dst in rbstate.RenameFile's order (#321: dst
// held open until the directory and then dst are synced), with this package's cut steps.
func replaceFile(tmp, dst string) error {
	if unsafeToFree(dst) {
		return errUnownedClusters
	}
	if held, _ := os.Open(dst); held != nil {
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

// mkdirOrdered makes dir and its missing parents under root one at a time. On vfat an fsync
// writes a file's own entry before the FAT, so the FAT that gives a new folder its cluster goes
// first with the fsync of flush, then the parent with the entry naming it (#321 review R4).
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

// stagedWhole reports whether a staged copy is a whole file of dst's kind.
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

// putBack restores rel when a cut pass left it missing: from its staged copy when that is
// whole, else from its first-original backup. False when rel stays missing.
func putBack(mount, rel, name string) bool {
	dst := filepath.Join(mount, rel)
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		return true
	}
	staged := filepath.Join(filepath.Dir(dst), name)
	if st, err := os.Lstat(staged); err == nil && st.Mode().IsRegular() && stagedWhole(staged, dst) && replaceFile(staged, dst) == nil {
		log.Printf("rb-bridge: %s was missing after a cut write-back; its new copy put back", dst)
		return true
	}
	entries, _ := os.ReadDir(filepath.Join(mount, ".enginex-backup"))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(mount, ".enginex-backup", entry.Name(), rel))
		if !entry.IsDir() || err != nil {
			continue
		}
		f, err := createStaged(filepath.Join(filepath.Dir(dst), newStagedName("restore")))
		if err == nil {
			_, err = writeSynced(f, bytes.NewReader(data))
		}
		if err == nil && replaceFile(f.Name(), dst) == nil {
			log.Printf("rb-bridge: %s was missing after a cut write-back; its original put back", dst)
			return true
		}
	}
	return false
}

// CleanWriteBackLeftovers, at an insert, finishes what a cut pass left (#321): every file the
// mark names that is missing is put back; then, when none is missing, the temporary files in
// the marked folders and the mark go. It returns how many temporary files it removed.
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
		rest, isTarget := strings.CutPrefix(line, "= ")
		name := ""
		if isTarget {
			var ok bool
			if name, rest, ok = strings.Cut(rest, " "); !ok || !stagedName(name) {
				continue
			}
		}
		rel, ok := markPath(mount, rest, isTarget)
		switch {
		case !ok:
		case !isTarget:
			dirs = append(dirs, rel)
		case !putBack(mount, rel, name):
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
				if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && rbstate.RemoveFile(path) == nil {
					n++
				}
			}
		}
	}
	_ = rbstate.RemoveFile(mark)
	return n
}
