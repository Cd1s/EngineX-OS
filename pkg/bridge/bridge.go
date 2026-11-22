// Package bridge coordinates read-only export discovery and Engine imports.
package bridge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ST_RDONLY from statvfs(3); syscall has no portable name for it.
const unix_ST_RDONLY = 0x1

var errNoLocalExport = fmt.Errorf("no local Rekordbox export: %w", os.ErrNotExist)
var errUnreadableExport = errors.New("rekordbox export is unreadable; existing library kept")

type unreadableExport struct {
	candidate exportCandidate
	err       error
}

// ReadLibrary reads the drive's own export plus the libraries staged from
// Pioneer players on the LAN (#59).
func ReadLibrary(ctx context.Context, mount string) (*rekordbox.Library, error) {
	l, _, err := readLibrary(ctx, mount)
	return l, err
}
func readLibrary(ctx context.Context, mount string) (*rekordbox.Library, bool, error) {
	l, staged, _, err := readLibraryDetailed(ctx, mount)
	return l, staged, err
}

func readLibraryDetailed(ctx context.Context, mount string) (*rekordbox.Library, bool, []string, error) {
	r, err := compat.NewResolver(mount)
	if err != nil {
		return nil, false, nil, err
	}
	l, readable, unreadable, err := readExportWithResolver(ctx, mount, r)
	stagedOnly := errors.Is(err, errNoLocalExport)
	if len(unreadable) > 0 && hasLibrary(filepath.Join(mount, "Engine Library", "Database2")) {
		dir := filepath.Join(mount, "Engine Library", "Database2")
		stamp := importedStamp(dir)
		readMarkValue := readMark(dir, readName)
		for _, bad := range unreadable {
			builtFrom := readLibrariesStamp(readMarkValue) == stamp && readLibrariesContains(readMarkValue, bad.candidate.rel)
			if readMarkValue == "" && primaryOfStamp(stamp) == bad.candidate.rel {
				builtFrom = true
			}
			if builtFrom {
				log.Printf("rb-bridge: existing library kept: %s unreadable", bad.candidate.rel)
				if errors.Is(bad.err, rekordbox.ErrNotCheckpointed) {
					return nil, false, nil, errUnreadableExport
				}
				return nil, false, nil, fmt.Errorf("existing library kept: %s unreadable: %w", bad.candidate.rel, bad.err)
			}
		}
	}
	if stagedOnly {
		l = &rekordbox.Library{}
		err = nil
	}
	if err != nil {
		return nil, false, nil, err
	}
	if l != nil {
		mergeStaged(mount, l)
	}
	if stagedOnly && len(l.Tracks) == 0 && len(l.Playlists) == 0 {
		return nil, false, nil, os.ErrNotExist
	}
	rels := make([]string, 0, len(readable))
	for _, read := range readable {
		rels = append(rels, read.candidate.rel)
	}
	return l, stagedOnly, rels, nil
}

const exportNewerMargin = 5 * time.Second

type exportCandidate struct {
	rel, full string
	size      int64
	mtime     time.Time
	isPDB     bool
}

func exportCandidates(r *compat.Resolver, mount string) ([]exportCandidate, error) {
	resolve := func(p string) (exportCandidate, bool, error) {
		rel, err := r.Resolve(p)
		if errors.Is(err, os.ErrNotExist) {
			return exportCandidate{}, false, nil
		}
		if err != nil {
			return exportCandidate{}, false, err
		}
		full := r.FullPath(rel)
		st, err := os.Stat(full)
		if errors.Is(err, os.ErrNotExist) {
			return exportCandidate{}, false, nil
		}
		if err != nil {
			return exportCandidate{}, false, err
		}
		return exportCandidate{
			rel:   rel,
			full:  full,
			size:  st.Size(),
			mtime: st.ModTime(),
			isPDB: strings.EqualFold(filepath.Ext(rel), ".pdb"),
		}, true, nil
	}

	var oneLib []exportCandidate
	seen := map[string]bool{}
	for _, p := range []string{"PIONEER/rekordbox/exportLibrary.db", "PIONEER/rekordbox/exportOneLibrary.db"} {
		c, ok, err := resolve(p)
		if err != nil {
			return nil, err
		}
		if ok && !seen[c.rel] {
			seen[c.rel] = true
			oneLib = append(oneLib, c)
		}
	}
	if len(oneLib) == 2 && exportTimeKnown(oneLib[0].mtime) && exportTimeKnown(oneLib[1].mtime) && oneLib[1].mtime.Sub(oneLib[0].mtime) > exportNewerMargin {
		oneLib[0], oneLib[1] = oneLib[1], oneLib[0]
	}

	var pdb *exportCandidate
	if !prolink.GeneratedExport(mount) {
		c, ok, err := resolve("PIONEER/rekordbox/export.pdb")
		if err != nil {
			return nil, err
		}
		if ok && !seen[c.rel] {
			pdb = &c
		}
	}

	switch {
	case len(oneLib) == 0 && pdb == nil:
		return nil, nil
	case len(oneLib) == 0:
		return []exportCandidate{*pdb}, nil
	case pdb == nil:
		return oneLib, nil
	case exportTimeKnown(pdb.mtime) && exportTimeKnown(oneLib[0].mtime) && pdb.mtime.Sub(oneLib[0].mtime) > exportNewerMargin:
		return append([]exportCandidate{*pdb}, oneLib...), nil
	case exportTimeKnown(pdb.mtime) && exportTimeKnown(oneLib[0].mtime) && oneLib[0].mtime.Sub(pdb.mtime) > exportNewerMargin:
		return append(oneLib, *pdb), nil
	default:
		// On ties, retain the recorded source to preserve deck edits and lists.
		dir := filepath.Join(mount, "Engine Library", "Database2")
		mark := readMark(dir, pendingName)
		if mark == "" {
			mark = readMark(dir, stampName)
		}
		for _, v := range []string{stampVersion, "v3", "v2"} {
			mark = strings.TrimPrefix(mark, v+" ")
		}
		for i, c := range oneLib {
			if strings.HasPrefix(mark, c.rel+" ") {
				oneLib[0], oneLib[i] = oneLib[i], oneLib[0]
				return append(oneLib, *pdb), nil
			}
		}
		for _, name := range []string{stampName, pendingName, stateName} {
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				return append([]exportCandidate{*pdb}, oneLib...), nil
			}
		}
		// Only a fresh stick adopts the richer format on a timestamp tie.
		return append(oneLib, *pdb), nil
	}
}

func exportTimeKnown(t time.Time) bool { return t.Year() >= 2000 }

type readableExport struct {
	candidate exportCandidate
	library   *rekordbox.Library
}

// exportReads counts reads of a drive's own export (each may decrypt a OneLibrary).
var exportReads atomic.Int64

// afterExportRead runs after each export read; tests use it to stretch the read.
var afterExportRead = func() {}

func readExport(ctx context.Context, mount string) (*rekordbox.Library, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return nil, e
	}
	l, _, _, e := readExportWithResolver(ctx, mount, r)
	return l, e
}

func readExportWithResolver(ctx context.Context, mount string, r *compat.Resolver) (*rekordbox.Library, []readableExport, []unreadableExport, error) {
	exportReads.Add(1)
	defer afterExportRead()
	candidates, e := exportCandidates(r, mount)
	if e != nil {
		return nil, nil, nil, e
	}
	if len(candidates) == 0 {
		return nil, nil, nil, errNoLocalExport
	}
	var errs []error
	readable := make([]readableExport, 0, len(candidates))
	unreadable := make([]unreadableExport, 0, len(candidates))
	for _, c := range candidates {
		var l *rekordbox.Library
		var err error
		if c.isPDB {
			l, err = rekordbox.ReadPDB(c.full)
		} else {
			if st, statErr := os.Stat(c.full + "-wal"); statErr == nil && st.Size() > 0 {
				err = fmt.Errorf("%w: source sidecar -wal is present", rekordbox.ErrNotCheckpointed)
			} else {
				l, err = rekordbox.ReadSQLite(ctx, c.full, nil)
			}
		}
		if err == nil {
			readable = append(readable, readableExport{candidate: c, library: l})
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, unreadable, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.rel, err))
		unreadable = append(unreadable, unreadableExport{candidate: c, err: err})
		log.Printf("rb-bridge: %s unreadable (%v)", c.rel, err)
	}
	if len(readable) == 0 {
		return nil, nil, unreadable, errors.Join(errs...)
	}
	merged := mergeReadableExports(readable)
	parts := make([]string, 0, len(readable))
	for _, read := range readable {
		parts = append(parts, fmt.Sprintf("%s tracks=%d playlists=%d", read.candidate.rel, len(read.library.Tracks), len(read.library.Playlists)))
	}
	log.Printf("rb-bridge: libraries read: %s", strings.Join(parts, ", "))
	return merged, readable, unreadable, nil
}

func mergeReadableExports(readable []readableExport) *rekordbox.Library {
	if len(readable) == 1 {
		return readable[0].library
	}
	out := &rekordbox.Library{Artwork: map[uint32]string{}, Keys: map[uint32]string{}}
	usedTrackIDs := map[uint32]bool{}
	byPath := map[string]uint32{}
	usedPlaylistIDs := map[uint32]bool{}
	playlistByKey := map[string]int{}

	for sourceIndex, read := range readable {
		sourceLabel := exportSourceLabel(read.candidate)
		trackIDs := map[uint32]uint32{}
		for _, original := range read.library.Tracks {
			t := original
			key := foldPath(t.Path)
			if existing, ok := byPath[key]; ok {
				trackIDs[original.ID] = existing
				continue
			}
			id := original.ID
			if sourceIndex > 0 {
				id = stableExportID("track", key, usedTrackIDs)
			} else if usedTrackIDs[id] {
				id = stableExportID("track", key, usedTrackIDs)
			}
			t.ID = id
			if id != original.ID {
				t.RecordHash = rekordbox.RecordHash(t)
			}
			usedTrackIDs[id] = true
			byPath[key] = id
			trackIDs[original.ID] = id
			out.Tracks = append(out.Tracks, t)
		}
		for id, value := range read.library.Artwork {
			if mapped, ok := trackIDs[id]; ok {
				out.Artwork[mapped] = value
			}
		}
		for id, value := range read.library.Keys {
			if mapped, ok := trackIDs[id]; ok {
				out.Keys[mapped] = value
			}
		}
		if sourceIndex == 0 {
			out.Columns = append(out.Columns, read.library.Columns...)
		}

		playlistIDs := map[uint32]uint32{0: 0}
		pending := append([]rekordbox.Playlist(nil), read.library.Playlists...)
		for len(pending) > 0 {
			progress := false
			remaining := make([]rekordbox.Playlist, 0, len(pending))
			for _, original := range pending {
				if original.ParentID != 0 {
					if _, ok := playlistIDs[original.ParentID]; !ok {
						remaining = append(remaining, original)
						continue
					}
				}
				p := original
				p.ParentID = playlistIDs[original.ParentID]
				var tracks []uint32
				for _, id := range original.Tracks {
					if mapped, ok := trackIDs[id]; ok {
						tracks = append(tracks, mapped)
					}
				}
				p.Tracks = tracks
				key := playlistPathKey(read.library.Playlists, original.ID)
				if existingIndex, ok := playlistByKey[key]; ok && samePlaylistTracks(out.Playlists[existingIndex].Tracks, p.Tracks) {
					playlistIDs[original.ID] = out.Playlists[existingIndex].ID
					progress = true
					continue
				}
				if existingIndex, ok := playlistByKey[key]; ok {
					p.Name += " (" + sourceLabel + ")"
					_ = existingIndex
				}
				if sourceIndex > 0 {
					p.ID = stableExportID("playlist", sourceLabel+"\x00"+key, usedPlaylistIDs)
				} else if usedPlaylistIDs[p.ID] {
					p.ID = stableExportID("playlist", key, usedPlaylistIDs)
				}
				usedPlaylistIDs[p.ID] = true
				playlistIDs[original.ID] = p.ID
				if _, ok := playlistByKey[key]; !ok {
					playlistByKey[key] = len(out.Playlists)
				}
				out.Playlists = append(out.Playlists, p)
				progress = true
			}
			if !progress {
				// A malformed parent cycle cannot be merged. Keep the readable tracks
				// and let the library validator report the broken playlist tree.
				break
			}
			pending = remaining
		}
	}
	return out
}

func exportSourceLabel(c exportCandidate) string {
	if c.isPDB {
		return "Device Library"
	}
	return "OneLibrary"
}

func stableExportID(kind, key string, used map[uint32]bool) uint32 {
	sum := sha256.Sum256([]byte("rb-bridge:" + kind + ":" + key))
	id := binary.LittleEndian.Uint32(sum[:]) | 0x80000000
	for id == 0 || used[id] {
		id++
	}
	return id
}

func playlistPathKey(playlists []rekordbox.Playlist, id uint32) string {
	byID := make(map[uint32]rekordbox.Playlist, len(playlists))
	for _, p := range playlists {
		byID[p.ID] = p
	}
	var names []string
	for {
		p, ok := byID[id]
		if !ok {
			break
		}
		names = append(names, p.Name)
		if p.ParentID == 0 {
			break
		}
		id = p.ParentID
	}
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	return strings.Join(names, "/")
}

func samePlaylistTracks(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ensureWritableEngineLibrary mounts an ext4-backed overlay onto "<mount>/Engine Library"
// when the underlying volume (such as macOS HFS+) is mounted read-only.
func ensureWritableEngineLibrary(mount string) bool {
	libDir := filepath.Join(mount, "Engine Library")
	var libSt syscall.Statfs_t
	if syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0 {
		return true
	}
	info, err := os.Lstat(libDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if st, err := os.Stat("/data/enginex"); err != nil || !st.IsDir() {
		return false
	}
	base := filepath.Base(filepath.Clean(mount))
	if base == "" || base == "." || base == "/" {
		return false
	}
	lower := "/run/enginex-empty"
	upper := filepath.Join("/data/enginex/hfs-lib", base, "upper")
	work := filepath.Join("/data/enginex/hfs-lib", base, "work")
	for _, d := range []string{lower, upper, work} {
		if os.MkdirAll(d, 0755) != nil {
			return false
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if overlayMount(libDir, opts) != nil {
		return false
	}
	return syscall.Statfs(libDir, &libSt) == nil && libSt.Flags&unix_ST_RDONLY == 0
}

func writable(mount string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(mount, &st) != nil || st.Flags&unix_ST_RDONLY == 0 || ensureWritableEngineLibrary(mount)
}

func Convert(ctx context.Context, mount string, opts enginedb.Options) (enginedb.Result, error) {
	return convert(ctx, mount, &opts, nil, 0, time.Time{})
}

// preread is a library ConvertAtMount has already read (and, for OneLibrary,
// decrypted), so the same mount does not read it twice.
type preread struct {
	l          *rekordbox.Library
	stagedOnly bool
	readRels   []string
	resolver   *compat.Resolver
	// reconciled: its tracks the library holds have their rows' paths, and only the others are
	// looked up, by the import itself (#253). Reconcile looked up every folder of the export.
	reconciled bool
}

// convert is Convert with an optional preread library. budget > 0 sets the
// file-reading deadline after reconciliation and leaves it in opts for backfill.
func convert(ctx context.Context, mount string, opts *enginedb.Options, pre *preread, budget time.Duration, mountStart time.Time) (enginedb.Result, error) {
	r, e := compat.NewResolver(mount)
	if e != nil {
		return enginedb.Result{}, e
	}
	_, e = r.Resolve("PIONEER")
	if errors.Is(e, os.ErrNotExist) {
		if len(stagedExports(mount)) == 0 {
			return enginedb.Result{Bypassed: true}, nil
		}
		e = nil // a pure Engine/empty drive can still receive staged CDJ libraries
	}
	// Read-only volumes (HFS+ on Engine OS 5.0.4: rw writes crash hfsplus.ko)
	// cannot hold an Engine Library unless Engine Library itself is a writable mount.
	if e == nil && !writable(mount) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if e != nil {
		return enginedb.Result{}, e
	}
	var l *rekordbox.Library
	var stagedOnly bool
	if pre != nil {
		l, stagedOnly = pre.l, pre.stagedOnly
		if pre.resolver != nil {
			r = pre.resolver
		}
	} else {
		l, stagedOnly, e = readLibrary(ctx, mount)
		if errors.Is(e, os.ErrNotExist) {
			return enginedb.Result{Bypassed: true}, nil
		}
		if errors.Is(e, errUnreadableExport) {
			return enginedb.Result{Bypassed: true}, nil
		}
		if e != nil {
			return enginedb.Result{}, e
		}
	}
	opts.StagedOnly = stagedOnly
	opts.MatchByPath = true
	if pre != nil && pre.reconciled {
		mergeByPath(l)
	} else if e := ReconcileContext(ctx, r, l); e != nil {
		return enginedb.Result{}, e
	}
	if budget > 0 && opts.Deadline.IsZero() {
		if !mountStart.IsZero() {
			if remaining := maxMountHookBudget - time.Since(mountStart); remaining < budget {
				if remaining > time.Second {
					budget = remaining
				} else {
					budget = time.Second
				}
			}
		}
		opts.Deadline = time.Now().Add(budget)
	}
	return enginedb.SyncResolved(ctx, r, l, *opts)
}

// Reconcile fits a real export to what a library can hold, so that one odd entry
// cannot lose thousands of tracks. rekordbox can list one file under two records,
// and files get deleted from a stick after export. The first record of a file
// stands for it in playlists; a track whose file is gone is left out. Files that are
// still on the drive under names the export does not have are not gone: see
// ErrFilesUnreadable.
func Reconcile(r *compat.Resolver, l *rekordbox.Library) {
	_ = ReconcileContext(context.Background(), r, l)
}

// ReconcileContext is Reconcile with cancellation between path lookups.
func ReconcileContext(ctx context.Context, r *compat.Resolver, l *rekordbox.Library) error {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{} // 0 means left out
	kept := l.Tracks[:0:0]
	var merged, missing int
	var gone []string
	for _, t := range l.Tracks {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := r.ResolveContext(ctx, t.Path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, os.ErrNotExist) {
			replaced[t.ID] = 0
			missing++
			gone = append(gone, t.Path)
			continue
		}
		if err != nil {
			kept = append(kept, t) // Sync reports anything else
			continue
		}
		if first, ok := byPath[rel]; ok {
			replaced[t.ID] = first
			merged++
			continue
		}
		byPath[rel] = t.ID
		kept = append(kept, t)
	}
	if len(replaced) == 0 {
		return nil
	}
	if hidden := hiddenTracks(r, byPath, gone); hidden > unreadableLimit(len(l.Tracks)) {
		log.Printf("rb-bridge: NOT IMPORTED: the files of %d of the export's %d tracks are on the drive under names that cannot be read as the export wrote them (the first: %q). Nothing is removed, the library stays as it is; the next mount tries again", hidden, len(l.Tracks), gone[0])
		return fmt.Errorf("%w: %d of %d tracks", ErrFilesUnreadable, hidden, len(l.Tracks))
	}
	l.Tracks = kept
	replacePlaylistTracks(l, replaced)
	log.Printf("rb-bridge: %d duplicate records merged, %d tracks without a file left out", merged, missing)
	return nil
}

// ErrFilesUnreadable: many tracks of the export have no file under their name, and files
// no track of the export leads to lie where those names should be. The library is left as
// it is and a later mount tries again. Real stick, 2026-11-19: during one update of a
// stick of 6013 tracks no file name with a non-ASCII character could be read as the export
// wrote it (a mount without UTF-8 names); the update took those 707 tracks for deleted
// and built a library without them, their playlist entries and what the deck had saved.
var ErrFilesUnreadable = errors.New("files of the export are on the drive under names that cannot be read")

// unreadableLimit is how many such tracks an import leaves out without refusing: 2 % of
// the export's tracks, at most 50 and at least 5.
func unreadableLimit(tracks int) int { return min(50, max(5, tracks/50)) }

// hiddenTracks counts the tracks in gone (export paths without a file) whose file is
// still on the drive under another name. A deleted file or folder leaves nothing behind.
// A name the volume does not give as the export wrote it leaves, where the lookup
// stopped, an entry no track of the export leads to and of the same kind: a folder with a
// file somewhere in it for a folder (deleting an album's files by hand leaves empty folders),
// a file with the same extension for a file. kept has the found tracks' paths.
func hiddenTracks(r *compat.Resolver, kept map[string]uint32, gone []string) int {
	claimed := map[string]map[string]bool{}
	for rel := range kept {
		dir := ""
		for _, name := range strings.Split(rel, "/") {
			if claimed[dir] == nil {
				claimed[dir] = map[string]bool{}
			}
			claimed[dir][name] = true
			dir = filepath.Join(dir, name)
		}
	}
	type place struct{ dir, kind string } // kind: "/" for a folder, else the file's extension
	lost := map[place]map[string]int{}    // the names not found there, and their tracks
	for _, p := range gone {
		stop, ok := r.Stopped(p)
		if !ok {
			continue
		}
		at := place{stop.Dir, "/"}
		if stop.File {
			at.kind = strings.ToLower(filepath.Ext(stop.Name))
		}
		if lost[at] == nil {
			lost[at] = map[string]int{}
		}
		lost[at][stop.Name]++
	}
	hidden := 0
	for at, names := range lost {
		free, tracks := 0, 0
		for _, name := range r.Entries(at.dir, at.kind == "/") {
			if free >= len(names) || claimed[at.dir][name] || strings.HasPrefix(name, ".") {
				continue
			}
			if at.kind == "/" {
				if !holdsFile(r.FullPath(filepath.Join(at.dir, name))) {
					continue
				}
			} else if strings.ToLower(filepath.Ext(name)) != at.kind {
				continue
			}
			free++
		}
		for _, n := range names {
			tracks += n
		}
		hidden += tracks * min(free, len(names)) / len(names)
	}
	return hidden
}

// holdsFile reports whether a file, hidden ones aside, lies anywhere under dir.
func holdsFile(dir string) bool {
	found := errors.New("found")
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if err == nil && !d.IsDir() {
			return found
		}
		return nil
	}) == found
}

// exportStamp names the export a library is imported from: the export files' sizes and
// times and the CDJ libraries staged on the volume. "" when the volume has neither.
func exportStamp(r *compat.Resolver, mount string) (string, error) {
	candidates, err := exportCandidates(r, mount)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		if staged := strings.TrimSpace(stagedStamp(mount)); staged != "" {
			return stampVersion + " " + staged, nil
		}
		return "", nil
	}
	parts := make([]string, len(candidates))
	for i, c := range candidates {
		parts[i] = fmt.Sprintf("%s %d %d", c.rel, c.size, c.mtime.UnixNano())
	}
	return stampVersion + " " + strings.Join(parts, " | ") + stagedStamp(mount), nil
}

const (
	stampName = ".rb-bridge-stamp"
	readName  = ".rb-bridge-read"
	// pendingName holds the stamp of an export whose import left tracks without
	// their files read; the next mount of the same export resumes it.
	pendingName  = ".rb-bridge-pending"
	stampVersion = "v4" // v2 aligned MP3 (#178); v3 also checks AAC priming; v4: one rekordbox offset, loaded rows moved to it.
	stateName    = ".rb-bridge-state.db"
)

func legacyGridOptions(stamp string) (mp3, aac bool) {
	// v4 reads every track again, whatever the library was before.
	return stamp != "" && !strings.HasPrefix(stamp, stampVersion+" "), false
}

// The edisksd hook kills rb-bridge 20 s after the mount, which rolled back the
// whole import, playlists included (#48). Stop reading files after this; the
// rest of the tracks finish on later mounts. A variable only for tests.
var (
	mountReadBudget    = 8 * time.Second
	maxMountHookBudget = 14 * time.Second
	// An update in place still running this long after the mount stops before the hook's kill
	// and goes on in the background (#301): the kill threw it away, at every insert.
	mountGiveUp = 17 * time.Second
)

// ConvertAtMount is Convert for when a volume is mounted, while Engine is waiting
// for it. An export unchanged since the last import is skipped at once. A first
// import is built in a stage on the volume and published complete; one that does
// not fit in the mount's time returns Result.Background and is finished by
// ImportInBackground (#301). full forces a complete import, with cues and waveforms.
// Otherwise files are read for mountReadBudget at most, and the export counts as
// imported only once no track was left without its cues.
func ConvertAtMount(ctx context.Context, mount string, opts enginedb.Options, full bool) (enginedb.Result, error) {
	r, err := convertAtMount(ctx, mount, &opts, full)
	if r.Background {
		// The library is updated aside (#301): its upkeep and queued edits wait for the publish.
		return r, err
	}
	// A bypass without file reads has not started the shared read budget yet.
	if opts.Deadline.IsZero() && !full {
		opts.Deadline = time.Now().Add(mountReadBudget)
	}
	if !opts.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, opts.Deadline)
		defer cancel()
	}
	applyPendingAtMount(ctx, mount)
	fixMP3GridsAtMount(ctx, mount, opts.Deadline)
	if err == nil {
		shrinkBridgeAnalysisAtMount(ctx, mount, opts)
	}
	return r, err
}

func shrinkBridgeAnalysisAtMount(ctx context.Context, mount string, opts enginedb.Options) {
	if ctx.Err() != nil || !writable(mount) {
		return
	}
	if !opts.Deadline.IsZero() && !time.Now().Before(opts.Deadline) {
		return
	}
	r, err := enginedb.ShrinkBridgeAnalysis(ctx, mount, enginedb.ShrinkOptions{
		Deadline:            opts.Deadline,
		MaxBridgeStateBytes: opts.MaxBridgeStateBytes,
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("rb-bridge: bridge analysis shrink not finished: %v", err)
		return
	}
	if r.Rewritten > 0 || r.Compacted || r.Marked > 0 {
		log.Printf("rb-bridge: bridge analysis shrink: rewritten=%d compacted=%v marked=%d done=%v", r.Rewritten, r.Compacted, r.Marked, r.Done)
	}
}

func fixMP3GridsAtMount(ctx context.Context, mount string, deadline time.Time) {
	if ctx.Err() != nil || !writable(mount) {
		return
	}
	var budget time.Duration
	if !deadline.IsZero() {
		budget = time.Until(deadline)
		if budget <= 0 {
			return
		}
	}
	if r, err := enginedb.FixMP3Grids(ctx, mount, enginedb.MP3GridOptions{Budget: budget}); err != nil {
		log.Printf("rb-bridge: mp3 grid fix not finished: %v", err)
	} else if r.Corrected > 0 || r.Skipped > 0 {
		log.Printf("rb-bridge: mp3 grid fix: corrected=%d unchanged=%d skipped=%d interrupted=%v", r.Corrected, r.Unchanged, r.Skipped, r.Interrupted)
	}
}

// applyPendingAtMount applies the library edits the Web Console queued while Engine had the
// library open (Hot Cues, Relocate: enginedb.QueuePending). engine-guardian --heal-mount
// applies them on other sticks; the mount hook does not run it on a stick with a rekordbox
// export, so rb-bridge does, after its own import and on the stamp bypass as well.
func applyPendingAtMount(ctx context.Context, mount string) {
	if ctx.Err() != nil || !writable(mount) {
		return
	}
	if n, err := enginedb.ApplyPending(ctx, mount); err != nil {
		log.Printf("rb-bridge: queued library edits not applied: %v", err)
	} else if n > 0 {
		log.Printf("rb-bridge: applied %d queued library edits", n)
	}
}

func convertAtMount(ctx context.Context, mount string, opts *enginedb.Options, full bool) (enginedb.Result, error) {
	mountStart := time.Now()
	opts.MatchByPath = true
	// The file-reading budget starts once the library is read: decrypting a
	// OneLibrary must not use up the time meant for cues and waveforms.
	var budget time.Duration
	if !full && opts.Deadline.IsZero() {
		budget = mountReadBudget
	}
	r, err := compat.NewResolver(mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	stamp, err := exportStamp(r, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	if stamp == "" {
		return convert(ctx, mount, opts, nil, budget, mountStart)
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	// Prepare a read-only drive's writable library view before reading its stamp.
	canWrite := !full && writable(mount)
	if canWrite {
		// A publish an earlier mount did not finish, before anything looks at the library.
		if err := finishPublish(dir, stateName, stampName, readName, "m.db", pendingName); err != nil {
			return enginedb.Result{}, err
		}
		pruneRGBOverviews(ctx, dir, filepath.Join(dir, "m.db"), "")
	}
	if !full && !opts.SmartMatch && firstImport(dir) {
		if !canWrite {
			return enginedb.Result{Bypassed: true}, nil
		}
		return firstImportAtMount(ctx, mount, dir, stamp, opts, r, mountStart)
	}
	stage := filepath.Join(dir, importStageName)
	if canWrite {
		switch {
		case isUpdateStage(stage) && readMark(stage, stampName) == stamp:
			return publishUpdate(ctx, mount, dir)
		case isUpdateStage(stage) && readMark(stage, pendingName) == stamp && !opts.SmartMatch:
			return enginedb.Result{Background: true}, nil
		}
		dropStage(stage) // a stage of another export, or a first import's: the library is the volume's own now
	}
	oldStamp := readMark(dir, stampName)
	opts.LegacyOffsets, opts.LegacyAACOffsets = legacyGridOptions(oldStamp)
	// The few tracks a background update left to the mounts to read, in place (#253).
	var reread map[string]bool
	if !full && !opts.SmartMatch && canWrite {
		reread = readReread(dir, stamp)
	}
	readLibrariesMarkValue := readMark(dir, readName)
	bypass := !full && !opts.SmartMatch && importedStamp(dir) == stamp &&
		((readLibrariesMarkValue == "" && !stampHasMultipleExports(stamp)) || readLibrariesStamp(readLibrariesMarkValue) == stamp)
	// An earlier export-only import still has tracks without their files read.
	resume := bypass && hasFastTracks(dir)
	bypass = bypass && !resume
	var result enginedb.Result
	var pre *preread
	var readErr error
	if !full && opts.SmartMatch && !canWrite {
		return enginedb.Result{Bypassed: true}, nil
	}
	// An update a large export needs cannot finish while the mount waits: resolving and reading
	// the files of a cold stick takes minutes (#301). It goes on in the background on a copy.
	// Its playlists and new tracks come in now, from the export alone (#253).
	if !full && !bypass && !opts.SmartMatch && canWrite && opts.Deadline.IsZero() && reread == nil && exportBytes(r, mount) > attemptExportBytes {
		var res enginedb.Result
		quick := false
		// An export whose list is in the library already (.rb-bridge-pending) has only its files left.
		if readMark(dir, pendingName) != stamp {
			var err error
			if res, err = quickUpdate(ctx, mount, dir, r, mountStart.Add(maxMountHookBudget), stamp); err != nil {
				if errors.Is(err, errUnreadableExport) {
					return enginedb.Result{Bypassed: true}, nil
				}
				log.Printf("rb-bridge: the re-export's playlists wait for the background job: %v", err)
				res, quick = enginedb.Result{}, errors.Is(err, errQuickTooLong)
			} else {
				log.Printf("rb-bridge: re-export's playlists and %d new tracks in at the mount; grids and waveforms follow", res.Added)
				cut("quick update committed")
			}
		}
		if err := startUpdate(stage, stamp); err != nil {
			return enginedb.Result{}, err
		}
		if quick {
			if err := writeMark(stage, quickName, stamp); err != nil {
				return enginedb.Result{}, err
			}
		}
		log.Printf("rb-bridge: update of a large export goes on after the mount")
		res.Background = true
		return res, nil
	}
	// The overviews before anything is read from the export. On a card that also carries
	// the staged exports of thousands of tracks that read alone outlasts the mount's time:
	// the hook kills rb-bridge in it at every mount, and after it no overview was ever
	// completed. At most half of the time for reading files and never past the hook's
	// own limit; what it takes comes off the time the import and the MP3 grids share.
	backfill := canWrite && hasLibrary(dir)
	if backfill {
		start := time.Now()
		limit := opts.Deadline
		if limit.IsZero() {
			limit = start.Add(min(budget, max(maxMountHookBudget-time.Since(mountStart), time.Second)))
		}
		completeOverviews(ctx, mount, start.Add(limit.Sub(start)/2))
		if budget > 0 {
			budget = max(budget-time.Since(start), 1)
		}
	}
	giveUp := mountStart.Add(mountGiveUp)
	if opts.Deadline.After(giveUp) {
		giveUp = opts.Deadline // a caller's own, longer budget
	}
	inPlace, stop := context.WithDeadline(ctx, giveUp)
	defer stop()
	if !full && !bypass {
		// Parsing/decrypting the export does not spend the file-reading budget.
		l, stagedOnly, readRels, err := readLibraryDetailed(inPlace, mount)
		readErr = err
		if err == nil {
			pre = &preread{l: l, stagedOnly: stagedOnly, readRels: readRels, resolver: r}
			opts.PathFirst = pathFirstFor(oldStamp, readLibrariesMarkValue, readRels, stamp)
		}
		// What a background update left to this mount is a few tracks of a large export: the
		// others keep their rows' paths, unread (#253).
		if err == nil && reread != nil {
			var known map[string]string
			if known, err = ownedSpellings(dir); err == nil {
				_, err = keepSpellings(l, known)
			}
			readErr, pre.reconciled = err, err == nil
		}
	}
	// Start the shared absolute deadline once the library is read. Import and MP3
	// grids use what the backfill left, even if reading the export fails or the
	// import bypasses its unchanged stamp.
	if backfill && opts.Deadline.IsZero() {
		remaining := max(maxMountHookBudget-time.Since(mountStart), time.Second)
		opts.Deadline = time.Now().Add(min(budget, remaining))
		budget = 0
	}
	overviews := enginedb.ReadOverviewProgress(mount) // before this mount's import writes anything
	if bypass {
		if canWrite {
			// Marks of this import that an interruption left after its stamp (#253 review).
			if readMark(dir, pendingName) == stamp {
				os.Remove(filepath.Join(dir, pendingName))
			}
			if reread != nil {
				os.Remove(filepath.Join(dir, rereadName))
			}
			os.Remove(filepath.Join(dir, stateName+".compact"))
		}
		if writable(mount) { // drives imported by an earlier build still get their covers
			if opts.Deadline.IsZero() {
				opts.Deadline = time.Now().Add(mountReadBudget)
			}
			_, _ = enginedb.MigrateArtworkBefore(ctx, mount, opts.Deadline)
			completeOverviews(ctx, mount, opts.Deadline)
		}
		return enginedb.Result{Bypassed: true}, nil
	}
	// Out of the mount's time: what was done is not committed, the rest goes on in the background.
	late := func(err error) bool {
		return err != nil && inPlace.Err() != nil && ctx.Err() == nil && canWrite && !full && !opts.SmartMatch
	}
	background := func(err error) (enginedb.Result, error) {
		log.Printf("rb-bridge: update goes on after the mount: %v", err)
		if err = startUpdate(stage, stamp); err != nil {
			return enginedb.Result{}, err
		}
		return enginedb.Result{Background: true}, nil
	}
	if late(readErr) {
		return background(readErr)
	}
	if errors.Is(readErr, errUnreadableExport) {
		return enginedb.Result{Bypassed: true}, nil
	}
	if readErr != nil {
		if opts.SmartMatch && errors.Is(readErr, os.ErrNotExist) {
			return enginedb.Result{Bypassed: true}, nil
		}
		return result, readErr
	}
	// The same export as a mount that left tracks without their files: read
	// only those, the others are known to be imported with theirs.
	opts.Resume = resume || reread != nil || (!full && !opts.SmartMatch && readMark(dir, pendingName) == stamp)
	opts.Reread = reread
	result, err = convert(inPlace, mount, opts, pre, budget, mountStart)
	if late(err) {
		return background(err)
	}
	if err == nil && reread != nil {
		cut("in place committed")
		compactState(inPlace, dir, pre.l, rereadRefs(dir, stamp))
	}
	if err == nil && !result.Bypassed {
		// Only this build has written since the progress was read, and it leaves no overview
		// without band maxima: the backfill goes on where it was, also with no time left now.
		if err := overviews.Keep(mount); err != nil {
			log.Printf("rb-bridge: overview progress not kept: %v", err)
		}
		completeOverviews(ctx, mount, opts.Deadline) // the tracks this import had no reason to read again
	}
	if err == nil && result.Deferred > 0 {
		writeMark(dir, pendingName, stamp)
		cut("in place deferred")
		log.Printf("rb-bridge: %d tracks imported without cues; the next mount reads them", result.Deferred)
		if reread != nil {
			// Left by a background update: it goes on, and its remount reads the rest.
			if err = startUpdate(stage, stamp); err == nil {
				result.Background = true
			}
		}
	} else if err == nil && !result.Bypassed {
		if pre != nil {
			_ = writeImportState(dir, stamp, pre.readRels)
		} else {
			writeStamp(dir, stamp)
		}
		cut("in place stamped")
		os.Remove(filepath.Join(dir, pendingName))
		cut("in place pending removed")
		os.Remove(filepath.Join(dir, rereadName))
	}
	return result, err
}

// completeOverviews gives the overviews an earlier build imported the scale Engine
// expects, in the time until deadline; the next mount goes on from where this one stopped.
// A library it has been through is not read again until an import by another build.
func completeOverviews(ctx context.Context, mount string, deadline time.Time) {
	if _, err := enginedb.FillOverviewMaxima(ctx, mount, deadline); err != nil {
		log.Printf("rb-bridge: overviews not completed: %v", err)
	}
}

func cleanEmptyEngineLibrary(dir string) {
	if enginedb.OpenElsewhere(filepath.Join(dir, "m.db")) != 0 {
		return
	}
	// Leave no Engine Library for Engine to mistake for one; Remove spares non-empty dirs.
	// An empty m.db is what an earlier build left behind on a failed import.
	for _, name := range []string{"m.db", stateName} {
		if st, e := os.Stat(filepath.Join(dir, name)); e == nil && st.Size() == 0 {
			os.Remove(filepath.Join(dir, name))
		}
	}
	// The lock file belongs to a running rb-bridge while it is locked: removing
	// it would let a third one lock a fresh file and run beside it.
	lock := filepath.Join(dir, ".rb-bridge.lock")
	if f, err := os.OpenFile(lock, os.O_RDWR|syscall.O_NOFOLLOW, 0); err == nil {
		busy := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
		if !busy {
			os.Remove(lock)
		}
		f.Close()
		if busy {
			return
		}
	}
	os.Remove(dir)
	os.Remove(filepath.Dir(dir))
}

// hasFastTracks reports whether the bridge state holds tracks imported without
// their files read.
func hasFastTracks(dir string) bool {
	p := filepath.Join(dir, stateName)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return false
	}
	defer db.Close()
	var fast int
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM BridgeState WHERE fingerprint LIKE 'fast:%')`).Scan(&fast)
	return err == nil && fast == 1
}

func importedStamp(dir string) string {
	if !hasLibrary(dir) {
		return ""
	}
	b, _ := os.ReadFile(filepath.Join(dir, stampName))
	return strings.TrimSpace(string(b))
}

func primaryOfStamp(stamp string) string {
	f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(stamp), stampVersion+" "))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func pathFirstFor(oldStamp, oldMark string, currentRels []string, currentStamp string) bool {
	if len(currentRels) == 0 {
		return false
	}
	if oldMark == "" {
		return stampHasMultipleExports(oldStamp) || primaryOfStamp(oldStamp) != currentRels[0]
	}
	lines := strings.Split(strings.TrimSpace(oldMark), "\n")
	oldFirst := ""
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			oldFirst = strings.TrimSpace(line)
			break
		}
	}
	if oldFirst == "" {
		oldFirst = primaryOfStamp(oldStamp)
	}
	return oldFirst != currentRels[0] || primaryOfStamp(oldStamp) != currentRels[0] && oldStamp != currentStamp
}

func readLibrariesMark(stamp string, rels []string) string {
	return stamp + "\n" + strings.Join(rels, "\n") + "\n"
}

func readLibrariesStamp(mark string) string {
	if i := strings.IndexByte(mark, '\n'); i >= 0 {
		return strings.TrimSpace(mark[:i])
	}
	return strings.TrimSpace(mark)
}

func readLibrariesContains(mark, rel string) bool {
	lines := strings.Split(mark, "\n")
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == rel {
			return true
		}
	}
	return false
}

func stampHasMultipleExports(stamp string) bool {
	body := strings.TrimPrefix(strings.TrimSpace(stamp), stampVersion+" ")
	return strings.Contains(body, " | ")
}

func stampContainsExport(stamp, rel string) bool {
	body := strings.TrimPrefix(strings.TrimSpace(stamp), stampVersion+" ")
	for _, part := range strings.Split(body, " | ") {
		fields := strings.Fields(part)
		if len(fields) > 0 && fields[0] == rel {
			return true
		}
	}
	return false
}

// hasLibrary reports an m.db with tracks or playlists, whatever wrote it. Stock Engine leaves a
// ~80 KB skeleton with an empty Track table when it saw a rekordbox stick without
// the unpatched import, and that is not a library. An m.db that cannot be read
// counts as one, so it is never replaced blindly.
func hasLibrary(dir string) bool {
	p := filepath.Join(dir, "m.db")
	if st, err := os.Stat(p); err != nil || st.Size() == 0 {
		return false
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return true
	}
	defer db.Close()
	var n int
	return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Track) OR EXISTS(SELECT 1 FROM Playlist)`).Scan(&n) != nil || n > 0
}

// HasPopulatedLibrary reports whether mount carries a readable Engine Library
// database (Engine Library/Database2/m.db) with a valid Information row, at
// least one Track row, and playlists or the bridge's state beside it. Stock
// Engine creates an empty ~80–160 KB m.db skeleton on writable media before
// asking QML to choose a database, and adds a track to it for each one the DJ
// loads from the OneLibrary export: a skeleton with a few tracks is still not a
// library to show instead of the export (#301). Unlike hasLibrary (which treats
// unreadable files as present so ConvertAtMount never overwrites them), any
// missing, symlinked, unreadable, or skeleton database returns false here so the
// UI falls back to stock OneLibrary reading.
func HasPopulatedLibrary(mount string) bool {
	if mount == "" || !filepath.IsAbs(mount) {
		return false
	}
	engDir := filepath.Join(mount, "Engine Library")
	dbDir := filepath.Join(engDir, "Database2")
	p := filepath.Join(dbDir, "m.db")
	for _, sub := range []string{engDir, dbDir, p} {
		lst, err := os.Lstat(sub)
		if err != nil || lst.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return false
	}
	u := url.URL{
		Scheme:   "file",
		Path:     p,
		RawQuery: "mode=ro&_pragma=busy_timeout(1500)&_pragma=query_only(1)",
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var ok int
	err = db.QueryRow(
		`SELECT CASE WHEN EXISTS(SELECT 1 FROM Information WHERE id=1 AND trim(COALESCE(uuid,''))<>'')` +
			` AND EXISTS(SELECT 1 FROM Track) THEN 1 ELSE 0 END`,
	).Scan(&ok)
	if err != nil || ok != 1 {
		return false
	}
	if st, err := os.Lstat(filepath.Join(dbDir, stateName)); err == nil && st.Mode().IsRegular() {
		return true
	}
	var playlists int
	return db.QueryRow(`SELECT EXISTS(SELECT 1 FROM Playlist)`).Scan(&playlists) == nil && playlists == 1
}

func writeStamp(dir, stamp string) { _ = writeMark(dir, stampName, stamp) }

func writeImportState(dir, stamp string, rels []string) error {
	if err := writeMark(dir, readName, readLibrariesMark(stamp, rels)); err != nil {
		return err
	}
	return writeMark(dir, stampName, stamp)
}

func writeMark(dir, name, stamp string) error {
	err := os.WriteFile(filepath.Join(dir, name), []byte(stamp+"\n"), 0o644)
	if err != nil {
		log.Printf("rb-bridge: could not record the import: %v", err)
	}
	return err
}

func readMark(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// foldPath is the export-merge equivalence of paths: case folding and NFC (#6).
// enginedb keeps its own bridgePathKey because it also cleans database-relative paths.
func foldPath(p string) string { return norm.NFC.String(cases.Fold().String(norm.NFC.String(p))) }

// mergeByPath is Reconcile without touching the disk.
func mergeByPath(l *rekordbox.Library) {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{}
	kept := l.Tracks[:0:0]
	for _, t := range l.Tracks {
		key := foldPath(t.Path)
		if first, ok := byPath[key]; ok {
			replaced[t.ID] = first
			continue
		}
		byPath[key] = t.ID
		kept = append(kept, t)
	}
	l.Tracks = kept
	replacePlaylistTracks(l, replaced)
}

func replacePlaylistTracks(l *rekordbox.Library, replaced map[uint32]uint32) {
	if len(replaced) == 0 {
		return
	}
	for i := range l.Playlists {
		var tracks []uint32
		for _, id := range l.Playlists[i].Tracks {
			first, changed := replaced[id]
			if !changed {
				tracks = append(tracks, id)
			} else if first != 0 {
				tracks = append(tracks, first)
			}
		}
		l.Playlists[i].Tracks = tracks
	}
}
