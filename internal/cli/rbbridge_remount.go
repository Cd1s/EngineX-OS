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
// and one ejected or pulled meanwhile publishes at its next insert.
//
// A peer deck that loaded a track from the stick over Engine Link holds it on the peer, where
// this deck cannot see it. A wrong remount breaks a track a peer plays; a wrong mark costs one
// re-insert. So the stick is not remounted, and its library opens at the next insert as it did
// before the remount existed, when:
//   - Engine was seen sending a peer a track: an audio file of the stick read while Engine writes
//     no copy, unless stream-sync confirms a deck of this player has that track loaded (seen while
//     the job ran or the remount waited, kept as a mark until the next insert);
//   - Engine is connected to another device on the network now (enginePeers): a peer that copied
//     a track between two looks, or before the job, leaves no other trace here.
// A peer's unload is not observable, so there is no window after which it is safe.

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
	readyEvery    = 2 * time.Second
	deckWait      = 5 * time.Second // how often a remount a deck blocks looks again
	// ownDecks are the tracks this player's decks have loaded, Engine's TrackNetworkPath
	// ("net://<uuid>/<volume>/<path>"); ok is false when stream-sync does not answer.
	ownDecks = func() (nets []string, ok bool) {
		c := http.Client{Timeout: time.Second}
		resp, err := c.Get("http://127.0.0.1:8080/peer/decks")
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
)

// servedMark marks a stick from which Engine sent a peer a track since the import began.
func servedMark(mount string) string {
	h := fnv.New64a()
	h.Write([]byte(mount))
	return filepath.Join(ejectMarkDir, fmt.Sprintf("enginex-rb-served-%016x", h.Sum64()))
}

// noteServed marks mount when Engine sends a peer an audio file of it now (see above).
func noteServed(mount string) {
	if engineCopying() {
		return // a load of this player reads the stick to copy it
	}
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
				log.Printf("rb-bridge: Engine sends %s to a peer deck; %s opens at its next insert", filepath.Base(target), mount)
				_ = os.WriteFile(servedMark(mount), nil, 0o600)
				return
			}
		}
	}
}

// enginePeers reports Engine connected over TCP to another device on the local network:
// Engine Link. A variable for tests.
var enginePeers = func() bool {
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
		return false
	}
	b, _ := os.ReadFile(filepath.Join(procDir, "net", "tcp"))
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		// sl local remote st ... inode: established, to a private IPv4 peer that is not us.
		if len(f) < 10 || f[3] != "01" || !inodes[f[9]] {
			continue
		}
		local, _, _ := strings.Cut(f[1], ":")
		remote, _, _ := strings.Cut(f[2], ":")
		if ip := hexIPv4(remote); ip != nil && ip.IsPrivate() && !ip.Equal(hexIPv4(local)) {
			return true
		}
	}
	return false
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

// startRemount starts --remount in a unit of its own: the hook stops the import job at the remove.
var startRemount = func(mount string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--unit=" + remountUnit(mount), "--collect", "--no-block", "--quiet", exe}
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
	rep := newUSBLoadingReporter(usbLoadingStatusPath(), detectMediaKind(mount, ""))
	err := remount(ctx, mount, rep)
	if err == nil || ctx.Err() != nil {
		return nil
	}
	// The DJ inserts the stick again; the banner says so until it is mounted anew.
	log.Printf("rb-bridge: %v", err)
	old := mountID(mount)
	for i := 0; i < 600 && ctx.Err() == nil && (!mounted(mount) || mountID(mount) == old); i++ {
		rep.Ready()
		if sleepCtx(ctx, readyEvery) != nil {
			break
		}
	}
	return err
}

func remount(ctx context.Context, mount string, rep *usbLoadingReporter) error {
	dev, devno, err := mountDevice(mount)
	if err != nil {
		return err
	}
	uevent := filepath.Join(sysClassBlock, filepath.Base(dev), "uevent")
	ejected := func() bool { _, err := os.Lstat(ejectMark(mount)); return err == nil }
	// Engine copies a track it loads from the stick; a remove meanwhile could cut that load short.
	waiting := false
	served := func() bool { _, err := os.Lstat(servedMark(mount)); return err == nil }
	for clear := 0; clear < 2; {
		if !mounted(mount) || ejected() {
			return nil // ejected or pulled: the next mount publishes
		}
		if noteServed(mount); served() || enginePeers() {
			return fmt.Errorf("a peer deck may hold a track from %s; eject it and insert it again to load the library", mount)
		}
		wait := remountWait
		switch {
		case engineCopying():
			clear = 0
		case deckHolds(mount):
			clear, wait = 0, deckWait
			if !waiting {
				log.Printf("rb-bridge: a deck holds a track from %s; it opens when none does, or at the next insert", mount)
				waiting = true
				rep.Ready()
			}
		default:
			clear++
		}
		if err = sleepCtx(ctx, wait); err != nil {
			return nil
		}
	}
	if waiting {
		rep.Publishing()
	}
	start := time.Now()
	if err = os.WriteFile(uevent, []byte("remove"), 0); err != nil {
		return fmt.Errorf("remove %s: %w", dev, err)
	}
	if err = pollUntil(ctx, removeLimit, func() bool { return !mounted(mount) }); err != nil {
		// Nothing was unmounted (a busy read-write HFS+ stick): it still has its old library.
		return fmt.Errorf("%s not unmounted by the remove; eject it and insert it again to load the library", mount)
	}
	// The publish renames the library: Engine's old handles must be closed first.
	if pollUntil(ctx, closeLimit, func() bool { return !engineHolds(devno) }) != nil {
		log.Printf("rb-bridge: Engine still holds files of %s; mounting it again anyway", dev)
	}
	if ejected() {
		return nil
	}
	if err = os.WriteFile(uevent, []byte("add"), 0); err != nil {
		return fmt.Errorf("add %s: %w; insert it again", dev, err)
	}
	if err = pollUntil(ctx, remountLimit, func() bool { return mounted(mount) }); err != nil {
		return fmt.Errorf("%s not mounted again; insert it again", dev)
	}
	log.Printf("rb-bridge: %s published by a remount in %s", mount, time.Since(start).Round(10*time.Millisecond))
	return nil
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
