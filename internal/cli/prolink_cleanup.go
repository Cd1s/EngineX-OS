package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
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
	prolinkCacheDirName         = ".prolink-cache"
	prolinkCleanupPollEvery     = 3 * time.Second
	prolinkCleanupGraceWait     = 18 * time.Second
	prolinkCleanupKeepaliveEach = 5 * time.Second
	prolinkPreparingMaxAge      = 25 * time.Second
	prolinkReaddirBatch         = 128
	stRdonlyFlag                = 0x1 // ST_RDONLY (mount(2) / statfs f_flags)
)

type prolinkCleanupOptions struct {
	isBusy            func() bool
	isEjecting        func() bool
	keepalive         func()
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

type mountIdentity struct {
	dev      uint64
	mntID    uint64
	hasMntID bool
}

func statxMountNode(dirFD int, name string, flags int) (unix.Statx_t, mountIdentity, error) {
	var stx unix.Statx_t
	err := unix.Statx(dirFD, name, flags, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stx)
	if err != nil {
		return stx, mountIdentity{}, err
	}
	id := mountIdentity{
		dev:      unix.Mkdev(stx.Dev_major, stx.Dev_minor),
		mntID:    stx.Mnt_id,
		hasMntID: stx.Mask&unix.STATX_MNT_ID != 0,
	}
	return stx, id, nil
}

func sameMountIdentity(a, b mountIdentity) bool {
	if a.dev != b.dev {
		return false
	}
	if a.hasMntID && b.hasMntID && a.mntID != b.mntID {
		return false
	}
	return true
}

func usbPreparingActive() bool {
	info, err := os.Lstat(usbLoadingStatusPath())
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	age := time.Since(info.ModTime())
	return age >= -2*time.Second && age < prolinkPreparingMaxAge
}

var pokeDeckClock = func() {
	if testing.Testing() {
		return
	}
	c := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := c.Get("http://127.0.0.1:8080/v1/enginex/clock")
	if err == nil {
		resp.Body.Close()
	}
	pokeStreamSyncUI()
}

type cleanupWalk struct {
	ctx           context.Context
	mountID       mountIdentity
	isBusy        func() bool
	isEjecting    func() bool
	keepalive     func()
	lastKeepalive time.Time
	statfs        func(int, *unix.Statfs_t) error
	seenIno       map[uint64]struct{}
	bytesFreed    int64
	stopped       bool
}

func (w *cleanupWalk) shouldStop() bool {
	if w.stopped {
		return true
	}
	if w.keepalive != nil && time.Since(w.lastKeepalive) >= prolinkCleanupKeepaliveEach {
		w.keepalive()
		w.lastKeepalive = time.Now()
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

	_, dirID, e := statxMountNode(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
	if e != nil {
		return false, e
	}
	if !sameMountIdentity(dirID, w.mountID) {
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
			estx, eid, e := statxMountNode(int(f.Fd()), entry, unix.AT_SYMLINK_NOFOLLOW)
			if e != nil {
				if errors.Is(e, unix.ENOENT) {
					continue
				}
				return hadEntries, e
			}
			if !sameMountIdentity(eid, w.mountID) {
				w.stopped = true
				return hadEntries, nil
			}
			if estx.Mode&unix.S_IFMT == unix.S_IFDIR {
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
			if estx.Mode&unix.S_IFMT == unix.S_IFREG {
				if estx.Nlink > 1 && w.seenIno == nil {
					w.seenIno = make(map[uint64]struct{})
				}
				counted := true
				if w.seenIno != nil {
					if _, dup := w.seenIno[estx.Ino]; dup {
						counted = false
					} else if estx.Nlink > 1 {
						w.seenIno[estx.Ino] = struct{}{}
					}
				}
				if counted {
					sz := int64(estx.Size)
					if estx.Mask&unix.STATX_BLOCKS != 0 {
						alloc := int64(estx.Blocks) * 512
						if sz > 4096 && alloc < sz {
							sz = alloc
						}
					}
					w.bytesFreed += sz
				}
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
			active, loading, fresh := bridge.DeckActivityState()
			return active || loading || !fresh || bridge.IsEngineCopying() || usbPreparingActive()
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
	keepalive := opts.keepalive
	if keepalive == nil && opts.isBusy == nil {
		keepalive = pokeDeckClock
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

	_, mountID, err := statxMountNode(mountFD, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return prolinkCleanupResult{}, err
	}
	if opts.requireMountpoint {
		_, parentID, perr := statxMountNode(unix.AT_FDCWD, filepath.Dir(mount), unix.AT_SYMLINK_NOFOLLOW)
		if perr != nil || sameMountIdentity(parentID, mountID) {
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

	// Quick existence + symlink + mount-boundary guard on <mount>/.prolink-cache before entering walk.
	cacheStx, cacheID, err := statxMountNode(mountFD, prolinkCacheDirName, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return prolinkCleanupResult{}, nil
		}
		return prolinkCleanupResult{}, err
	}
	if cacheStx.Mode&unix.S_IFMT != unix.S_IFDIR || !sameMountIdentity(cacheID, mountID) {
		// Never follow symlinks or touch non-directory or cross-mount filesystems.
		return prolinkCleanupResult{}, nil
	}

	if keepalive != nil {
		keepalive()
	}
	w := &cleanupWalk{
		ctx:           ctx,
		mountID:       mountID,
		isBusy:        isBusy,
		isEjecting:    isEjecting,
		keepalive:     keepalive,
		lastKeepalive: time.Now(),
		statfs:        statfs,
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
	mntID uint64
}

type prolinkCleanupWatcher struct {
	mu            sync.Mutex
	handled       map[mountDevKey]bool
	firstSeen     map[mountDevKey]time.Time
	list          func() []string
	statMount     func(string) (mountDevKey, bool)
	isPreparing   func() bool
	activityFresh func() bool
	clean         func(context.Context, string) (prolinkCleanupResult, error)
	poke          func()
	grace         time.Duration
	now           func() time.Time
}

func defaultStatMountKey(mount string) (mountDevKey, bool) {
	stx, id, err := statxMountNode(unix.AT_FDCWD, mount, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil || stx.Mode&unix.S_IFMT != unix.S_IFDIR {
		return mountDevKey{}, false
	}
	return mountDevKey{mount: mount, dev: id.dev, mntID: id.mntID}, true
}

func newProlinkCleanupWatcher() *prolinkCleanupWatcher {
	return &prolinkCleanupWatcher{
		handled:     map[mountDevKey]bool{},
		firstSeen:   map[mountDevKey]time.Time{},
		list:        writableMediaMounts,
		statMount:   defaultStatMountKey,
		isPreparing: usbPreparingActive,
		activityFresh: func() bool {
			_, _, fresh := bridge.DeckActivityState()
			return fresh
		},
		clean: func(ctx context.Context, mount string) (prolinkCleanupResult, error) {
			return cleanProlinkCache(ctx, mount, prolinkCleanupOptions{requireMountpoint: true})
		},
		poke:  pokeDeckClock,
		grace: prolinkCleanupGraceWait,
		now:   time.Now,
	}
}

func (cw *prolinkCleanupWatcher) step(ctx context.Context) {
	mounts := cw.list()
	statFn := cw.statMount
	if statFn == nil {
		statFn = defaultStatMountKey
	}
	present := make(map[mountDevKey]bool, len(mounts))
	var activeKeys []mountDevKey
	for _, m := range mounts {
		k, ok := statFn(m)
		if !ok {
			continue
		}
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
		info, lerr := os.Lstat(filepath.Join(k.mount, prolinkCacheDirName))
		if lerr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			cw.mu.Lock()
			cw.handled[k] = true
			cw.mu.Unlock()
			continue
		}
		if cw.isPreparing != nil && cw.isPreparing() {
			continue
		}
		if cw.poke != nil {
			cw.poke()
		}
		if cw.activityFresh != nil && !cw.activityFresh() {
			continue
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
