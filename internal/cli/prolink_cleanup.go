package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"golang.org/x/sys/unix"
)

const (
	prolinkCacheDirName     = ".prolink-cache"
	prolinkCleanupPollEvery = 3 * time.Second
	prolinkCleanupGraceWait = 3 * time.Second
	prolinkReaddirBatch     = 128
	stRdonlyFlag            = 0x1 // ST_RDONLY (mount(2) / statfs f_flags)
)

type prolinkCleanupOptions struct {
	isBusy            func() bool
	isEjecting        func() bool
	requireMountpoint bool
	statfs            func(int, *unix.Statfs_t) error
	logf              func(string, ...any)
}

type prolinkCleanupResult struct {
	Removed    bool
	Stopped    bool
	BytesFreed int64
}

type procMountEntry struct {
	seq    int
	source string
	target string
	fstype string
	opts   string
}

// decodeMountField decodes octal escapes (/proc/mounts encodes space as \040, etc.).
func decodeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b = append(b, byte(v))
				i += 3
				continue
			}
		}
		b = append(b, s[i])
	}
	return string(b)
}

func containsMountOpt(opts, want string) bool {
	for _, opt := range strings.Split(opts, ",") {
		if opt == want {
			return true
		}
	}
	return false
}

// parseWritableMediaMounts extracts topmost top-level writable mounts under mediaRoot from
// /proc/mounts, skipping read-only mounts, read-only HFS+ views (enginex-hfs-view), and
// virtual filesystems.
func parseWritableMediaMounts(r io.Reader, mediaRoot string) []string {
	if mediaRoot == "" {
		mediaRoot = "/media/"
	}
	if !strings.HasSuffix(mediaRoot, "/") {
		mediaRoot += "/"
	}
	var all []procMountEntry
	lastSeq := map[string]int{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		source := decodeMountField(fields[0])
		target := decodeMountField(fields[1])
		fstype := fields[2]
		opts := fields[3]
		seq := len(all)
		all = append(all, procMountEntry{seq: seq, source: source, target: target, fstype: fstype, opts: opts})
		lastSeq[target] = seq
	}

	var out []string
	for _, e := range all {
		if lastSeq[e.target] != e.seq {
			continue
		}
		rest, ok := strings.CutPrefix(e.target, mediaRoot)
		if !ok || rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if !containsMountOpt(e.opts, "rw") {
			continue
		}
		if e.source == "enginex-hfs-view" {
			continue
		}
		switch e.fstype {
		case "tmpfs", "devtmpfs", "proc", "sysfs", "overlay":
			continue
		}
		out = append(out, e.target)
	}
	return out
}

func writableMediaMounts() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseWritableMediaMounts(f, "/media/")
}

type cleanupWalk struct {
	ctx        context.Context
	mountDev   uint64
	isBusy     func() bool
	isEjecting func() bool
	statfs     func(int, *unix.Statfs_t) error
	bytesFreed int64
	stopped    bool
}

func (w *cleanupWalk) shouldStop() bool {
	if w.stopped {
		return true
	}
	if w.ctx.Err() != nil || w.isBusy() || w.isEjecting() {
		w.stopped = true
		return true
	}
	return false
}

// cleanDirChild opens parentFD/name with O_NOFOLLOW, clears it pass-by-pass so directory
// mutation never skips entries in FAT/ext4 readdir, then unlinks the directory itself.
func (w *cleanupWalk) cleanDirChild(parentFD int, name string) error {
	for pass := 0; ; pass++ {
		if w.shouldStop() {
			return nil
		}
		childFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return nil
			}
			return err
		}
		hadEntries, err := w.drainOnePass(childFD, name)
		if w.stopped || err != nil {
			return err
		}
		if !hadEntries || pass > 4096 {
			break
		}
	}
	if w.shouldStop() {
		return nil
	}
	err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			w.stopped = true
		}
		return nil
	}
	return err
}

// drainOnePass reads and unlinks entries from dirFD and always closes dirFD via os.File.
func (w *cleanupWalk) drainOnePass(dirFD int, name string) (hadEntries bool, err error) {
	f := os.NewFile(uintptr(dirFD), name)
	defer f.Close()

	var st unix.Stat_t
	if e := unix.Fstat(int(f.Fd()), &st); e != nil {
		return false, e
	}
	if uint64(st.Dev) != w.mountDev {
		w.stopped = true
		return false, nil
	}
	var fsStat unix.Statfs_t
	if e := w.statfs(int(f.Fd()), &fsStat); e != nil {
		return false, e
	}
	if uint64(fsStat.Flags)&stRdonlyFlag != 0 {
		w.stopped = true
		return false, nil
	}

	for {
		if w.shouldStop() {
			return hadEntries, nil
		}
		names, rerr := f.Readdirnames(prolinkReaddirBatch)
		for _, entry := range names {
			if entry == "." || entry == ".." {
				continue
			}
			hadEntries = true
			if w.shouldStop() {
				return hadEntries, nil
			}
			var est unix.Stat_t
			if e := unix.Fstatat(int(f.Fd()), entry, &est, unix.AT_SYMLINK_NOFOLLOW); e != nil {
				if errors.Is(e, unix.ENOENT) {
					continue
				}
				return hadEntries, e
			}
			if uint64(est.Dev) != w.mountDev {
				w.stopped = true
				return hadEntries, nil
			}
			if est.Mode&unix.S_IFMT == unix.S_IFDIR {
				if e := w.cleanDirChild(int(f.Fd()), entry); e != nil {
					return hadEntries, e
				}
				if w.stopped {
					return hadEntries, nil
				}
				continue
			}
			if e := unix.Unlinkat(int(f.Fd()), entry, 0); e != nil {
				if errors.Is(e, unix.ENOENT) {
					continue
				}
				return hadEntries, e
			}
			if est.Mode&unix.S_IFMT == unix.S_IFREG {
				w.bytesFreed += est.Size
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return hadEntries, rerr
		}
		if len(names) == 0 {
			break
		}
	}
	return hadEntries, nil
}

// cleanProlinkCache safely deletes the exact top-level <mount>/.prolink-cache/ directory on a
// mounted writable drive using O_DIRECTORY|O_NOFOLLOW directory fds, never crossing mount
// boundaries or following symlinks, and stopping immediately if a deck loads, plays, or ejects.
func cleanProlinkCache(ctx context.Context, mount string, opts prolinkCleanupOptions) (prolinkCleanupResult, error) {
	isBusy := opts.isBusy
	if isBusy == nil {
		isBusy = func() bool {
			return bridge.IsDeckActive() || bridge.IsDeckLoading() || bridge.IsEngineCopying()
		}
	}
	isEjecting := opts.isEjecting
	if isEjecting == nil {
		isEjecting = func() bool {
			if _, err := os.Lstat(prolink.EjectingMark(mount)); err == nil {
				return true
			}
			if opts.requireMountpoint {
				return !mounted(mount)
			}
			return false
		}
	}
	statfs := opts.statfs
	if statfs == nil {
		statfs = unix.Fstatfs
	}
	logf := opts.logf
	if logf == nil {
		logf = log.Printf
	}

	if ctx.Err() != nil || isBusy() || isEjecting() {
		return prolinkCleanupResult{Stopped: true}, nil
	}

	mountFD, err := unix.Open(mount, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return prolinkCleanupResult{}, nil
		}
		return prolinkCleanupResult{}, err
	}
	defer unix.Close(mountFD)

	var mountSt unix.Stat_t
	if err := unix.Fstat(mountFD, &mountSt); err != nil {
		return prolinkCleanupResult{}, err
	}
	if opts.requireMountpoint {
		var parentSt unix.Stat_t
		if err := unix.Lstat(filepath.Dir(mount), &parentSt); err != nil || parentSt.Dev == mountSt.Dev {
			return prolinkCleanupResult{}, nil
		}
	}
	var mountFS unix.Statfs_t
	if err := statfs(mountFD, &mountFS); err != nil {
		return prolinkCleanupResult{}, err
	}
	if uint64(mountFS.Flags)&stRdonlyFlag != 0 {
		return prolinkCleanupResult{}, nil
	}

	// Quick existence + symlink guard on <mount>/.prolink-cache before entering walk.
	var cacheSt unix.Stat_t
	if err := unix.Fstatat(mountFD, prolinkCacheDirName, &cacheSt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return prolinkCleanupResult{}, nil
		}
		return prolinkCleanupResult{}, err
	}
	if cacheSt.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(cacheSt.Dev) != uint64(mountSt.Dev) {
		// Never follow symlinks or touch non-directory or cross-mount filesystems.
		return prolinkCleanupResult{}, nil
	}

	w := &cleanupWalk{
		ctx:        ctx,
		mountDev:   uint64(mountSt.Dev),
		isBusy:     isBusy,
		isEjecting: isEjecting,
		statfs:     statfs,
	}
	if err := w.cleanDirChild(mountFD, prolinkCacheDirName); err != nil {
		return prolinkCleanupResult{Stopped: w.stopped, BytesFreed: w.bytesFreed}, err
	}

	var postSt unix.Stat_t
	removed := errors.Is(unix.Fstatat(mountFD, prolinkCacheDirName, &postSt, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT)
	res := prolinkCleanupResult{
		Removed:    removed && !w.stopped,
		Stopped:    w.stopped,
		BytesFreed: w.bytesFreed,
	}
	if res.Removed {
		logf("rb-bridge: removed leftover %s on %s (%d bytes freed)", prolinkCacheDirName, mount, res.BytesFreed)
	} else if res.Stopped && res.BytesFreed > 0 {
		logf("rb-bridge: paused removing leftover %s on %s (%d bytes freed; resumes on next insert)", prolinkCacheDirName, mount, res.BytesFreed)
	}
	return res, nil
}

type mountDevKey struct {
	mount string
	dev   uint64
}

type prolinkCleanupWatcher struct {
	mu        sync.Mutex
	handled   map[mountDevKey]bool
	firstSeen map[mountDevKey]time.Time
	list      func() []string
	clean     func(context.Context, string) (prolinkCleanupResult, error)
	poke      func()
	grace     time.Duration
	now       func() time.Time
}

func newProlinkCleanupWatcher() *prolinkCleanupWatcher {
	return &prolinkCleanupWatcher{
		handled:   map[mountDevKey]bool{},
		firstSeen: map[mountDevKey]time.Time{},
		list:      writableMediaMounts,
		clean: func(ctx context.Context, mount string) (prolinkCleanupResult, error) {
			return cleanProlinkCache(ctx, mount, prolinkCleanupOptions{requireMountpoint: true})
		},
		poke:  pokeStreamSyncUI,
		grace: prolinkCleanupGraceWait,
		now:   time.Now,
	}
}

func (cw *prolinkCleanupWatcher) step(ctx context.Context) {
	mounts := cw.list()
	present := make(map[mountDevKey]bool, len(mounts))
	var activeKeys []mountDevKey
	for _, m := range mounts {
		var st unix.Stat_t
		if unix.Lstat(m, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		k := mountDevKey{mount: m, dev: uint64(st.Dev)}
		present[k] = true
		activeKeys = append(activeKeys, k)
	}

	cw.mu.Lock()
	for k := range cw.firstSeen {
		if !present[k] {
			delete(cw.firstSeen, k)
			delete(cw.handled, k)
		}
	}
	for k := range cw.handled {
		if !present[k] {
			delete(cw.handled, k)
		}
	}
	now := cw.now()
	var ready []mountDevKey
	for _, k := range activeKeys {
		if cw.handled[k] {
			continue
		}
		t0, seen := cw.firstSeen[k]
		if !seen {
			cw.firstSeen[k] = now
			t0 = now
		}
		if now.Sub(t0) >= cw.grace {
			ready = append(ready, k)
		}
	}
	cw.mu.Unlock()

	for _, k := range ready {
		if ctx.Err() != nil {
			return
		}
		if cw.poke != nil {
			cw.poke()
		}
		_, _ = cw.clean(ctx, k.mount)
		// Once done (removed or absent) OR paused while busy/ejecting, stop touching the
		// drive for the remainder of this insert and resume automatically at the next insert.
		cw.mu.Lock()
		cw.handled[k] = true
		cw.mu.Unlock()
	}
}

func watchProlinkCacheCleanup(ctx context.Context) {
	if testing.Testing() {
		return
	}
	cw := newProlinkCleanupWatcher()
	tick := time.NewTicker(prolinkCleanupPollEvery)
	defer tick.Stop()
	for {
		cw.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
