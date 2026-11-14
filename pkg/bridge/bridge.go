// Package bridge coordinates read-only export discovery and Engine imports.
package bridge

import (
	"context"
	"database/sql"
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

// ReadLibrary reads the drive's own export plus the libraries staged from
// Pioneer players on the LAN (#59).
func ReadLibrary(ctx context.Context, mount string) (*rekordbox.Library, error) {
	l, _, err := readLibrary(ctx, mount)
	return l, err
}
func readLibrary(ctx context.Context, mount string) (*rekordbox.Library, bool, error) {
	l, err := readExport(ctx, mount)
	stagedOnly := errors.Is(err, errNoLocalExport)
	if stagedOnly {
		l = &rekordbox.Library{}
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	if l != nil {
		mergeStaged(mount, l)
	}
	if stagedOnly && len(l.Tracks) == 0 && len(l.Playlists) == 0 {
		return nil, false, os.ErrNotExist
	}
	return l, stagedOnly, nil
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
	if len(oneLib) == 2 && oneLib[1].mtime.Sub(oneLib[0].mtime) > exportNewerMargin {
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
	case pdb.mtime.Sub(oneLib[0].mtime) > exportNewerMargin:
		return append([]exportCandidate{*pdb}, oneLib...), nil
	case oneLib[0].mtime.Sub(pdb.mtime) > exportNewerMargin:
		return append(oneLib, *pdb), nil
	default:
		// On ties, retain the recorded source to preserve deck edits and lists.
		dir := filepath.Join(mount, "Engine Library", "Database2")
		mark := readMark(dir, pendingName)
		if mark == "" {
			mark = readMark(dir, stampName)
		}
		mark = strings.TrimPrefix(mark, stampVersion+" ")
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

// exportReads counts reads of a drive's own export (each may decrypt a OneLibrary).
var exportReads atomic.Int64

// afterExportRead runs after each export read; tests use it to stretch the read.
var afterExportRead = func() {}

func readExport(ctx context.Context, mount string) (*rekordbox.Library, error) {
	exportReads.Add(1)
	defer afterExportRead()
	r, e := compat.NewResolver(mount)
	if e != nil {
		return nil, e
	}
	candidates, e := exportCandidates(r, mount)
	if e != nil {
		return nil, e
	}
	if len(candidates) == 0 {
		return nil, errNoLocalExport
	}
	var errs []error
	for i, c := range candidates {
		var l *rekordbox.Library
		var err error
		if c.isPDB {
			l, err = rekordbox.ReadPDB(c.full)
		} else {
			l, err = rekordbox.ReadSQLite(ctx, c.full, nil)
		}
		if err == nil {
			return l, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.rel, err))
		if i+1 < len(candidates) {
			log.Printf("rb-bridge: %s unreadable (%v); falling back to %s", c.rel, err, candidates[i+1].rel)
		}
	}
	return nil, errors.Join(errs...)
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
	if st, err := os.Stat("/data/sc5000-custom"); err != nil || !st.IsDir() {
		return false
	}
	base := filepath.Base(filepath.Clean(mount))
	if base == "" || base == "." || base == "/" {
		return false
	}
	lower := "/run/sc5000-empty"
	upper := filepath.Join("/data/sc5000-custom/hfs-lib", base, "upper")
	work := filepath.Join("/data/sc5000-custom/hfs-lib", base, "work")
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
	resolver   *compat.Resolver
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
		if e != nil {
			return enginedb.Result{}, e
		}
	}
	opts.StagedOnly = stagedOnly
	if e := ReconcileContext(ctx, r, l); e != nil {
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
// stands for it in playlists; a track whose file is gone is left out.
func Reconcile(r *compat.Resolver, l *rekordbox.Library) {
	_ = ReconcileContext(context.Background(), r, l)
}

// ReconcileContext is Reconcile with cancellation between path lookups.
func ReconcileContext(ctx context.Context, r *compat.Resolver, l *rekordbox.Library) error {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{} // 0 means left out
	kept := l.Tracks[:0:0]
	var merged, missing int
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
	l.Tracks = kept
	replacePlaylistTracks(l, replaced)
	log.Printf("rb-bridge: %d duplicate records merged, %d tracks without a file left out", merged, missing)
	return nil
}

const (
	stampName = ".rb-bridge-stamp"
	// pendingName holds the stamp of an export whose import left tracks without
	// their files read; the next mount of the same export resumes it.
	pendingName  = ".rb-bridge-pending"
	stampVersion = "v2" // v2 checks per-track MP3 start-offset classes (#178)
	stateName    = ".rb-bridge-state.db"
	// Past this, a first import cannot finish while the mount waits: reading every
	// file took 8 minutes for 6000 tracks on a USB stick. Later mounts read the
	// files a budget at a time.
	fastImportTracks = 500
)

// The edisksd hook kills rb-bridge 20 s after the mount, which rolled back the
// whole import, playlists included (#48). Stop reading files after this; the
// rest of the tracks finish on later mounts. A variable only for tests.
var (
	mountReadBudget    = 8 * time.Second
	maxMountHookBudget = 14 * time.Second
)

// ConvertAtMount is Convert for when a volume is mounted, while Engine is waiting
// for it. An export unchanged since the last import is skipped at once. A large
// library's first import takes only what the export holds, built on tmpfs and
// copied over in one go. full forces a complete import, with cues and waveforms.
// Otherwise files are read for mountReadBudget at most, and the export counts as
// imported only once no track was left without its cues.
func ConvertAtMount(ctx context.Context, mount string, opts enginedb.Options, full bool) (enginedb.Result, error) {
	r, err := convertAtMount(ctx, mount, &opts, full)
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
	return r, err
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
	candidates, err := exportCandidates(r, mount)
	if err != nil {
		return enginedb.Result{}, err
	}
	var stamp string
	if len(candidates) > 0 {
		parts := make([]string, len(candidates))
		for i, c := range candidates {
			parts[i] = fmt.Sprintf("%s %d %d", c.rel, c.size, c.mtime.UnixNano())
		}
		stamp = stampVersion + " " + strings.Join(parts, " | ") + stagedStamp(mount)
	} else {
		staged := strings.TrimSpace(stagedStamp(mount))
		if staged == "" {
			return convert(ctx, mount, &opts, nil, budget, mountStart)
		}
		stamp = stampVersion + " " + staged
	}
	dir := filepath.Join(mount, "Engine Library", "Database2")
	// The overviews first, in at most half of the time for reading files, and the import
	// reads files in what is left. They came after the import, and an import that does not
	// finish in the mount's time is killed at every mount: on a card that also carried
	// staged exports of thousands of tracks no overview was ever completed.
	if budget > 0 && writable(mount) && hasLibrary(dir) {
		start := time.Now()
		completeOverviews(ctx, mount, start.Add(budget/2))
		budget = max(budget-time.Since(start), 1)
	}
	overviews := enginedb.ReadOverviewProgress(mount) // before this mount's import writes anything
	oldStamp := readMark(dir, stampName)
	opts.LegacyOffsets = oldStamp != "" && !strings.HasPrefix(oldStamp, stampVersion+" ")
	resume := false
	if !full && !opts.SmartMatch && importedStamp(dir) == stamp {
		// Earlier builds stamped a large library imported from the export alone
		// (BH-GC1-2); it still has tracks without their files read.
		if resume = hasFastTracks(dir); !resume {
			if writable(mount) { // drives imported by an earlier build still get their covers
				if opts.Deadline.IsZero() {
					opts.Deadline = time.Now().Add(mountReadBudget)
				}
				_, _ = enginedb.MigrateArtworkBefore(ctx, mount, opts.Deadline)
				completeOverviews(ctx, mount, opts.Deadline)
			}
			return enginedb.Result{Bypassed: true}, nil
		}
	}
	var result enginedb.Result
	var pre *preread
	if !full && !opts.SmartMatch {
		l, stagedOnly, err := readLibrary(ctx, mount)
		if err != nil {
			if !hasLibrary(dir) {
				cleanEmptyEngineLibrary(dir)
			}
			return result, err
		}
		pre = &preread{l, stagedOnly, r}
		opts.StagedOnly = stagedOnly
		// A large library's first import takes only what the export holds, built
		// aside and published in one go, so its playlists are there when Engine
		// first sees the stick (#182). Its cues, loops, beat grids, waveforms and
		// MP3 offsets are read on the next inserts, a read budget at a time, in
		// the in-place import below: no stamp until no track is left without them.
		// Before, the stamp was written here and they were never read.
		if !hasLibrary(dir) {
			if !writable(mount) {
				return enginedb.Result{Bypassed: true}, nil
			}
			if err := ReconcileContext(ctx, r, l); err != nil {
				cleanEmptyEngineLibrary(dir)
				return result, err
			}
			if len(l.Tracks) > fastImportTracks {
				opts.Fast = true
				result, err = stagedImport(ctx, dir, l, *opts, r)
				if err != nil {
					cleanEmptyEngineLibrary(dir)
				} else {
					writeMark(dir, pendingName, stamp)
					log.Printf("rb-bridge: %d tracks imported without cues; the next mounts read them", len(l.Tracks))
				}
				return result, err
			}
			if budget > 0 && opts.Deadline.IsZero() {
				opts.Deadline = time.Now().Add(budget)
				budget = 0
			}
			result, err = stagedImport(ctx, dir, l, *opts, r)
			if err != nil {
				cleanEmptyEngineLibrary(dir)
			} else if result.Deferred > 0 {
				writeMark(dir, pendingName, stamp)
				log.Printf("rb-bridge: %d tracks imported without cues; the next mount reads them", result.Deferred)
			} else {
				writeStamp(dir, stamp)
			}
			return result, err
		}
	}
	// The same export as a mount that left tracks without their files: read
	// only those, the others are known to be imported with theirs.
	opts.Resume = resume || (!full && !opts.SmartMatch && readMark(dir, pendingName) == stamp)
	result, err = convert(ctx, mount, &opts, pre, budget, mountStart)
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
		log.Printf("rb-bridge: %d tracks imported without cues; the next mount reads them", result.Deferred)
	} else if err == nil && !result.Bypassed {
		writeStamp(dir, stamp)
		os.Remove(filepath.Join(dir, pendingName))
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
// database (Engine Library/Database2/m.db) with a valid Information row and at
// least one Track row. Stock Engine creates an empty ~80–160 KB m.db skeleton on
// writable media before asking QML to choose a database; unlike hasLibrary
// (which treats unreadable files as present so ConvertAtMount never overwrites
// them), any missing, symlinked, unreadable, or 0-track database returns false
// here so the UI falls back to stock OneLibrary reading.
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
	return err == nil && ok == 1
}

func writeStamp(dir, stamp string) { writeMark(dir, stampName, stamp) }

func writeMark(dir, name, stamp string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(stamp+"\n"), 0o644); err != nil {
		log.Printf("rb-bridge: could not record the import: %v", err)
	}
}

func readMark(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// stagedImport syncs a copy of the volume's database in a temporary directory
// and publishes it back only if the whole import succeeded.
func stagedImport(ctx context.Context, dir string, l *rekordbox.Library, opts enginedb.Options, res *compat.Resolver) (enginedb.Result, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return enginedb.Result{}, err
	}
	unlock, err := lockVolume(dir)
	if err != nil {
		return enginedb.Result{}, err
	}
	defer unlock()
	if enginedb.LibraryInUse(res.FullPath("")) {
		return enginedb.Result{}, enginedb.ErrLibraryInUse
	}
	// An earlier run may have died between the two renames.
	if err = finishPublish(dir, stateName, "m.db"); err != nil {
		return enginedb.Result{}, err
	}
	work, err := os.MkdirTemp("", "rb-bridge-*")
	if err != nil {
		return enginedb.Result{}, err
	}
	defer os.RemoveAll(work)
	// Keep one read-only connection so data_version detects commits to m.db
	// or its WAL while we import the snapshot. File identity also catches an
	// Engine-created/replaced database, including an initially absent m.db.
	p := filepath.Join(dir, "m.db")
	before, err := os.Stat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return enginedb.Result{}, err
	}
	var source *sql.DB
	var version int
	if before != nil {
		source, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
		if err != nil {
			return enginedb.Result{}, err
		}
		defer source.Close()
		source.SetMaxOpenConns(1)
		if err = source.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&version); err != nil {
			return enginedb.Result{}, err
		}
	}
	// Tracks and bridge state already held on this volume stay in the library for incremental sync.
	for _, name := range []string{"m.db", stateName} {
		if err = snapshot(filepath.Join(dir, name), filepath.Join(work, name)); err != nil {
			return enginedb.Result{}, err
		}
	}
	opts.DatabaseDir = work
	mergeByPath(l)
	result, err := enginedb.SyncResolved(ctx, res, l, opts)
	if err != nil {
		return result, err
	}
	return result, publish(work, dir, func() error {
		after, err := os.Stat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		changed := (before == nil) != (after == nil)
		if before != nil && after != nil {
			changed = !os.SameFile(before, after)
			var current int
			if err = source.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&current); err != nil {
				return err
			}
			changed = changed || current != version
		}
		if changed {
			return errors.New("Engine library changed during import; retry on next mount")
		}
		if enginedb.LibraryInUse(res.FullPath("")) {
			return enginedb.ErrLibraryInUse
		}
		if source != nil {
			return source.Close()
		}
		return nil
	}, stateName, "m.db")
}

// mergeByPath is Reconcile without touching the disk.
func mergeByPath(l *rekordbox.Library) {
	byPath := map[string]uint32{}
	replaced := map[uint32]uint32{}
	kept := l.Tracks[:0:0]
	for _, t := range l.Tracks {
		// Same equivalence as compat's resolver: case folding and NFC (#6).
		key := norm.NFC.String(cases.Fold().String(norm.NFC.String(t.Path)))
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
