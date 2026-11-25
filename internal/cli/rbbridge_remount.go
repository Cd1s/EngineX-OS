package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"golang.org/x/sys/unix"
)

// --remount publishes a library the background job finished, without a re-plug (#301). It sends
// the stick's partition a synthetic udev remove and then add: edisksd detaches it lazily and Engine
// closes its library, then the add mounts it again and the edisksd hook's pre-mount publishes the
// library before Engine sees the stick. It waits while any deck holds a track from the stick,
// loaded, playing or not: Engine could not save that track's play count or edits after the
// remount ("pool is disposed"). A stick the DJ ejected (the hook's mark) is never mounted again,
// and one ejected or pulled meanwhile publishes at its next insert. When the decks are clear and a
// next library waits, the rows the deck wrote meanwhile are taken into it first (design 1.11 item 6).
//
// EngineX peers report loaded tracks through /peer/decks. Silent peers cannot report an
// unload, so they do not block forever: publication also requires a quiet window with no
// observed outgoing audio read. Each observed serve restarts that window, including when
// stream-sync cannot establish which deck owns the read.

var (
	sysClassBlock = "/sys/class/block"
	mountinfoPath = "/proc/self/mountinfo"
	procDir       = "/proc"
	engineCopies  = "/data/inMusicTempFiles/"
	ejectMarkDir  = "/run"
	remountWait   = 500 * time.Millisecond
	removeLimit   = 15 * time.Second
	closeLimit    = 10 * time.Second
	remountLimit  = 60 * time.Second // a mount runs fsck and the publish
	serveQuiet    = 30 * time.Second // 60 nominal 500 ms watch intervals; short reads may escape sampling
	deckWait      = 5 * time.Second  // how often a remount a deck blocks looks again
	// ownDecks are the tracks this player's decks have loaded, Engine's TrackNetworkPath
	// ("net://<uuid>/<volume>/<path>"); ok is false when stream-sync does not answer.
	ownDecks = func() ([]string, bool) { return decksAt("127.0.0.1:8080") }
	// peerDecks asks an Engine Link peer's stream-sync the same: an EngineX deck answers.
	peerDecks = func(ip net.IP) ([]string, bool) { return decksAt(net.JoinHostPort(ip.String(), "8080")) }
	// takeDeckRows takes the rows the deck wrote since the job's last pass into a next library that
	// waits for this remount, and reports whether it had to look for them.
	takeDeckRows  = bridge.TakeDeckRows
	deckRowPasses = 3 // a library the deck goes on writing is left to the window after this many
)

// decksAt reads the decks an EngineX stream-sync at addr serves the other player
// (GET /peer/decks): each deck's Net; ok is false when it does not answer.
func decksAt(addr string) (nets []string, ok bool) {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/peer/decks")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var v struct {
		Decks []struct {
			Net string `json:"net"`
		} `json:"decks"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v) != nil {
		return nil, false
	}
	for _, d := range v.Decks {
		nets = append(nets, d.Net)
	}
	return nets, true
}

// onStick reports a TrackNetworkPath ("net://<uuid>/<volume>/<path>", the volume possibly
// "<name> (<slot>)") on a volume named as the stick at mount. A peer's own stick of the same
// name counts too: conservatively wait until that deck releases it.
func onStick(np, mount string) bool {
	parts := strings.SplitN(strings.TrimPrefix(np, "net://"), "/", 3)
	if !strings.HasPrefix(np, "net://") || len(parts) != 3 {
		return false
	}
	vol := parts[1]
	if i := strings.LastIndex(vol, " ("); i > 0 && strings.HasSuffix(vol, ")") {
		vol = vol[:i]
	}
	return strings.EqualFold(vol, filepath.Base(mount))
}

// peerBlocks reports an answering EngineX peer holding a track from this stick.
// Silent peers rely on the outgoing-serve quiet window instead of blocking forever.
func peerBlocks(mount string) bool {
	for _, ip := range enginePeers() {
		nets, ok := peerDecks(ip)
		if !ok {
			continue
		}
		for _, n := range nets {
			if onStick(n, mount) {
				return true
			}
		}
	}
	return false
}

// servedMark records the last observed outgoing read (mtime), or the start of observation.
func servedMark(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return filepath.Join(ejectMarkDir, fmt.Sprintf("enginex-rb-served-%016x", h.Sum64()))
}

// noteServed refreshes the last-serve timestamp while Engine sends audio from mount.
func noteServed(mount string) {
	var nets []string
	asked := false
	for _, pid := range engines() {
		fds, _ := os.ReadDir(filepath.Join(procDir, pid, "fd"))
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(procDir, pid, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, mount+"/") || !audioExt[strings.ToLower(filepath.Ext(target))] {
				continue
			}
			if !asked {
				nets, _ = ownDecks() // unanswered: no deck of ours is known to hold it
				asked = true
			}
			rel := strings.TrimPrefix(target, mount)
			local := false
			for _, n := range nets {
				local = local || strings.HasPrefix(n, "net://") && strings.HasSuffix(n, rel)
			}
			if !local {
				_ = os.WriteFile(servedMark(mount), nil, 0o600)
				return
			}
		}
	}
}

// enginePeers are the devices on the local network (private or link-local IPv4) Engine is
// connected to over TCP: its Engine Link peers. A variable for tests.
var enginePeers = func() []net.IP {
	inodes := map[string]bool{}
	for _, pid := range engines() {
		fds, _ := os.ReadDir(filepath.Join(procDir, pid, "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join(procDir, pid, "fd", fd.Name())); err == nil && strings.HasPrefix(t, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(t, "socket:["), "]")] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var peers []net.IP
	b, _ := os.ReadFile(filepath.Join(procDir, "net", "tcp"))
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		// sl local remote st ... inode: established, to a private IPv4 peer that is not us.
		if len(f) < 10 || f[3] != "01" || !inodes[f[9]] {
			continue
		}
		local, _, _ := strings.Cut(f[1], ":")
		remote, _, _ := strings.Cut(f[2], ":")
		// A deck cabled straight to this one, without DHCP, is on 169.254.0.0/16.
		if ip := hexIPv4(remote); ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast()) && !ip.IsLoopback() &&
			!ip.Equal(hexIPv4(local)) && !slices.ContainsFunc(peers, ip.Equal) {
			peers = append(peers, ip)
		}
	}
	return peers
}

// hexIPv4 reads /proc/net/tcp's little-endian hex address.
func hexIPv4(h string) net.IP {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil || len(h) != 8 {
		return nil
	}
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// watchServed notes Engine sending a peer a track of mount until ctx ends.
func watchServed(ctx context.Context, mount string) {
	for sleepCtx(ctx, remountWait) == nil {
		noteServed(mount)
	}
}

// ejectMark is the hook's mark of an Eject of mount (edisksd_hook.c mark_eject).
func ejectMark(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return filepath.Join(ejectMarkDir, fmt.Sprintf("enginex-rb-ejected-%016x", h.Sum64()))
}

func remountUnit(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return fmt.Sprintf("enginex-rb-remount-%016x.service", h.Sum64())
}

// publishMark says that the next mount of the volume is the remount the background job asked
// for: nobody waits for the stick, so that window spends all its time on staged data.
func publishMark(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return filepath.Join(ejectMarkDir, fmt.Sprintf("enginex-rb-publish-%016x", h.Sum64()))
}

// takePublishMark reports and removes a fresh publish mark of mount.
func takePublishMark(mount string) bool {
	p := publishMark(mount)
	st, err := os.Lstat(p)
	if err != nil {
		return false
	}
	_ = os.Remove(p)
	return st.Mode().IsRegular() && time.Since(st.ModTime()) < 2*remountLimit
}

// startRemount starts --remount in a unit of its own: the hook stops the import job at the remove.
var startRemount = func(mount string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--unit=" + remountUnit(mount), "--collect", "--no-block", "--quiet",
		"--slice=enginex-background.slice", "--nice=19",
		"--property=CPUSchedulingPolicy=idle", "--property=IOSchedulingClass=idle", "--property=TimeoutStopSec=5",
		"--setenv=GOMAXPROCS=1", "--setenv=GOMEMLIMIT=128MiB", exe}
	if filepath.Base(exe) != "rb-bridge" {
		args = append(args, "rb-bridge")
	}
	out, err := exec.Command("systemd-run", append(args, "--remount", mount)...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already") {
		return fmt.Errorf("remount not started: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runRemount(ctx context.Context, mount string) error {
	if !filepath.IsAbs(mount) {
		return fmt.Errorf("--remount must be absolute")
	}
	ctx, cancel := context.WithCancel(ctx)
	rep := newDriveReporter(mount)
	done := make(chan struct{})
	go func() { defer close(done); refresh(ctx, rep) }()
	defer func() { cancel(); <-done }()
	err := remount(ctx, mount, rep)
	if err == nil || ctx.Err() != nil {
		return nil
	}
	log.Printf("rb-bridge: %v", err)
	rep.FailPublication(err)

	return err
}

func remount(ctx context.Context, mount string, rep *usbLoadingReporter) error {
	dev, devno, err := mountDevice(mount)
	if err != nil {
		return err
	}
	uevent := filepath.Join(sysClassBlock, filepath.Base(dev), "uevent")
	ejected := func() bool { _, err := os.Lstat(ejectMark(mount)); return err == nil }
	// If the import watcher left no history, observe a whole quiet window here.
	quietSince := time.Now()
	if st, err := os.Stat(servedMark(mount)); err == nil {
		quietSince = st.ModTime()
	}
	waiting := false
	for clear, passes := 0, 0; clear < 2; {
		if !mounted(mount) || ejected() {
			return nil // ejected or pulled: the next mount publishes
		}
		peerHeld := peerBlocks(mount)
		// Sample after peer queries too: they may take a second per silent peer.
		noteServed(mount)
		if st, err := os.Stat(servedMark(mount)); err == nil && st.ModTime().After(quietSince) {
			quietSince = st.ModTime()
		}
		wait := remountWait
		if engineCopying() || deckHolds(mount) || peerHeld || time.Since(quietSince) < serveQuiet {
			clear, wait = 0, deckWait
			if !waiting {
				log.Printf("rb-bridge: %s waits for decks to release its tracks and outgoing reads to be quiet", mount)
				waiting = true
			}
			rep.Waiting()
		} else {
			clear++
		}
		// The decks are clear. What the deck wrote to the library while a next one waited is taken
		// into it now, with no window's limit, so that the remount's window only renames whatever
		// the library's size. Looking for it reads every row (seconds on a big library): the decks
		// are looked at again afterwards.
		if clear == 2 && passes < deckRowPasses && takeRowsWhileMounted(ctx, mount, ejected) {
			clear, wait = 0, 0
			passes++
		}
		if err = sleepCtx(ctx, wait); err != nil {
			return nil
		}
	}
	rep.Publishing()
	start := time.Now()
	_ = os.WriteFile(publishMark(mount), nil, 0o600)
	if err = os.WriteFile(uevent, []byte("remove"), 0); err != nil {
		return fmt.Errorf("remove %s: %w", dev, err)
	}
	if err = pollUntil(ctx, removeLimit, func() bool { return !mounted(mount) }); err != nil {
		// Nothing was unmounted (a busy read-write HFS+ stick): it still has its old library.
		return fmt.Errorf("%s not unmounted by the remove", mount)
	}
	// The publish renames the library: Engine's old handles must be closed first.
	if pollUntil(ctx, closeLimit, func() bool { return !engineHolds(devno) }) != nil {
		log.Printf("rb-bridge: Engine still holds files of %s; mounting it again anyway", dev)
	}
	if ejected() {
		return nil
	}
	if err = os.WriteFile(uevent, []byte("add"), 0); err != nil {
		return fmt.Errorf("add %s: %w", dev, err)
	}
	if err = pollUntil(ctx, remountLimit, func() bool { return mounted(mount) }); err != nil {
		return fmt.Errorf("%s not mounted again", dev)
	}
	log.Printf("rb-bridge: %s published by a remount in %s", mount, time.Since(start).Round(10*time.Millisecond))
	return nil
}

// takeRowsWhileMounted is takeDeckRows, stopped when the stick is ejected or gone: the pass holds
// files of the stick open and writes its next library, and after an Eject nothing may (the hook
// marks the eject before its own pass and the unmount, and stops only the import job's unit).
// It looks every 100 ms: the unmount follows the mark by the start of the hook's own pass.
func takeRowsWhileMounted(ctx context.Context, mount string, ejected func() bool) bool {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sleepCtx(ctx, min(remountWait, 100*time.Millisecond)) == nil {
			if ejected() || !mounted(mount) {
				cancel()
			}
		}
	}()
	looked := takeDeckRows(ctx, mount)
	cancel()
	<-done
	return looked
}

// mountID is the id of the mount at mount, "" when there is none.
func mountID(mount string) string {
	b, _ := os.ReadFile(mountinfoPath)
	id := ""
	for _, line := range strings.Split(string(b), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && strings.ReplaceAll(fields[4], `\040`, " ") == mount {
			id = fields[0] // the last one is on top
		}
	}
	return id
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// mountDevice is the device mount is mounted from and its number. A read-only HFS+ stick shows
// through the hook's view, whose entries are the stick's.
func mountDevice(mount string) (string, uint64, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	var dev string
	var devno uint64
	for sc.Scan() {
		// id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		point := strings.ReplaceAll(fields[4], `\040`, " ")
		if point != mount && !strings.HasPrefix(point, mount+"/") {
			continue
		}
		for i := 6; i < len(fields)-2; i++ {
			if fields[i] != "-" || !strings.HasPrefix(fields[i+2], "/dev/") {
				continue
			}
			maj, min, ok := strings.Cut(fields[2], ":")
			a, e1 := strconv.ParseUint(maj, 10, 32)
			b, e2 := strconv.ParseUint(min, 10, 32)
			if ok && e1 == nil && e2 == nil && (dev == "" || point == mount) {
				dev, devno = fields[i+2], unix.Mkdev(uint32(a), uint32(b))
			}
		}
	}
	if err = sc.Err(); err != nil {
		return "", 0, err
	}
	if dev == "" {
		return "", 0, fmt.Errorf("%s is not mounted from a device", mount)
	}
	return dev, devno, nil
}

// engines are the pids of Engine.
func engines() []string {
	entries, _ := os.ReadDir(procDir)
	var pids []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if comm, err := os.ReadFile(filepath.Join(procDir, e.Name(), "comm")); err == nil && strings.TrimSpace(string(comm)) == "Engine" {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

// engineCopying reports Engine writing a copy of a track it loads.
func engineCopying() bool {
	for _, pid := range engines() {
		fds, _ := os.ReadDir(filepath.Join(procDir, pid, "fd"))
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(procDir, pid, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(target, engineCopies) {
				continue
			}
			info, err := os.ReadFile(filepath.Join(procDir, pid, "fdinfo", fd.Name()))
			if err != nil {
				return true
			}
			for _, line := range strings.Split(string(info), "\n") {
				if v, ok := strings.CutPrefix(line, "flags:"); ok {
					if flags, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32); err != nil || flags&3 != 0 {
						return true // O_WRONLY or O_RDWR
					}
				}
			}
		}
	}
	return false
}

// audioExt are the files a deck plays; Engine also holds the stick's m.db and, for stock
// OneLibrary, PIONEER/rekordbox/exportLibrary.db.
var audioExt = map[string]bool{".mp3": true, ".m4a": true, ".mp4": true, ".aac": true, ".flac": true, ".wav": true,
	".aif": true, ".aiff": true, ".alac": true, ".ogg": true, ".opus": true}

// deckHolds reports a deck holding a track from the stick at mount: Engine has an audio file of
// it open (a track it could not copy), or its copy of one, under engineCopies as
// "<id>_<file name>", a name the stick's library has. Engine also keeps copies of its own sampler
// sounds open there, which no stick's library names.
func deckHolds(mount string) bool {
	for _, pid := range engines() {
		fds, _ := os.ReadDir(filepath.Join(procDir, pid, "fd"))
		for _, fd := range fds {
			link := filepath.Join(procDir, pid, "fd", fd.Name())
			target, err := os.Readlink(link)
			switch {
			case err != nil:
			case strings.HasPrefix(target, mount+"/") && audioExt[strings.ToLower(filepath.Ext(target))]:
				return true
			case strings.HasPrefix(target, engineCopies):
				if _, name, ok := strings.Cut(filepath.Base(target), "_"); ok && bridge.LibraryHasFile(mount, name) {
					return true
				}
			}
		}
	}
	return false
}

// engineHolds reports Engine holding a file of the filesystem devno.
func engineHolds(devno uint64) bool {
	for _, pid := range engines() {
		fds, _ := os.ReadDir(filepath.Join(procDir, pid, "fd"))
		for _, fd := range fds {
			var st syscall.Stat_t
			if syscall.Stat(filepath.Join(procDir, pid, "fd", fd.Name()), &st) == nil && uint64(st.Dev) == devno {
				return true
			}
		}
	}
	return false
}
