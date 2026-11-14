package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

const (
	stageEvery = 10 * time.Minute
	// A drive keeps a tenth of itself free, and at least stageReserve: the 2 GiB an
	// earlier build left were less than Engine wants for one track's Stems.
	stageReserve = 4 << 30
	// All staged libraries on a drive together take at most a tenth of it, and never more
	// than stageMax. It was 32 GiB for each slot of each player, and nothing was ever
	// removed: an SD card of 58 GiB ended with 50 GiB of them and 2 GiB free.
	stageMax = 8 << 30
	// A player not copied for this long gives up its staged library when room is needed.
	stageKeep = 14 * 24 * time.Hour
)

// stager copies Pioneer players' libraries onto a local drive (#59), at most
// once per host/slot every stageEvery and one copy at a time. The drive's
// next mount imports them (bridge.ReadLibrary).
type stager struct {
	mu      sync.Mutex
	copyMu  sync.Mutex
	last    map[string]time.Time
	running map[string]bool
	staging map[string]int // folder names with a copy running or waiting
	mounts  func() []string
	inUse   func(string) bool
	statfs  func(string, *syscall.Statfs_t) error
	copy    func(context.Context, *prolink.NFSClient, string, string, int64) (int, error)
}

func (s *stager) observe(ctx context.Context, d prolink.Device) {
	host := net.ParseIP(d.Address)
	if host == nil || prolink.OwnExport(d) {
		return
	}
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	var slots []byte
	for _, slot := range []byte{3, 2} {
		key := fmt.Sprintf("%s/%d", host.String(), slot)
		if s.running[key] || time.Since(s.last[key]) < stageEvery {
			continue
		}
		s.last[key], s.running[key] = time.Now(), true
		slots = append(slots, slot)
		if s.staging == nil {
			s.staging = map[string]int{}
		}
		s.staging[stageName(d.Name, d.Number, slot)]++
	}
	s.mu.Unlock()
	if len(slots) == 0 {
		return
	}
	statfs, copy := s.statfs, s.copy
	if statfs == nil {
		statfs = syscall.Statfs
	}
	if copy == nil {
		copy = prolink.Stage
	}
	go func() {
		s.copyMu.Lock()
		defer s.copyMu.Unlock()
		defer func() {
			s.mu.Lock()
			for _, slot := range slots {
				key := fmt.Sprintf("%s/%d", host.String(), slot)
				s.last[key] = time.Now()
				delete(s.running, key)
				if name := stageName(d.Name, d.Number, slot); s.staging[name] > 1 {
					s.staging[name]--
				} else {
					delete(s.staging, name)
				}
			}
			s.mu.Unlock()
		}()
		mounts := s.mounts()
		if len(mounts) == 0 {
			return
		}
		for _, slot := range slots {
			export, _ := prolink.SlotExport(slot)
			name := stageName(d.Name, d.Number, slot)
			var dst string
			var budget int64
			// ST_RDONLY is 1 on Linux; syscall has no named constant.
			for _, mount := range mounts {
				var fs syscall.Statfs_t
				if statfs(mount, &fs) != nil || fs.Flags&1 != 0 {
					continue
				}
				cache := filepath.Join(mount, prolink.StagedDir)
				s.tidy(cache)
				room := func() int64 {
					if statfs(mount, &fs) != nil {
						return 0
					}
					return stageRoom(&fs, dirSize(cache))
				}
				budget = room()
				for budget <= 0 && s.expire(cache) {
					budget = room()
				}
				if budget > 0 {
					dst = filepath.Join(cache, name)
					budget += dirSize(dst) // Stage counts what the folder already holds
					// shortcut: Stage writes export.pdb last and outside the budget: one file of at most 64 MiB, against a reserve of 4 GiB
					break
				}
			}
			if dst == "" {
				log.Printf("rb-bridge: no room to copy %s", name)
				continue
			}
			n, err := copy(ctx, &prolink.NFSClient{IP: host}, export, dst, budget)
			if err == nil {
				log.Printf("rb-bridge: copied %d files of %s to %s; it shows after the drive is mounted again", n, name, dst)
			}
		}
	}()
}

// stageRoom is how many more bytes may be staged on a drive that holds staged bytes of
// them already. A drive whose size statfs does not tell keeps the two fixed limits.
func stageRoom(fs *syscall.Statfs_t, staged int64) int64 {
	free, size := int64(fs.Bavail)*int64(fs.Bsize), int64(fs.Blocks)*int64(fs.Bsize)
	reserve, limit := int64(stageReserve), int64(stageMax)
	if size > 0 {
		reserve, limit = max(reserve, size/10), min(limit, size/10)
	}
	return min(free-reserve, limit-staged)
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// waiting reports whether a copy into the folder name is running or queued.
func (s *stager) waiting(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staging[name] > 0
}

// tidy removes from a drive's staged libraries what no import can use: downloads cut
// short, and a library that never got its export.pdb (bridge imports only one that has
// it), unless a copy into it is running or waiting and goes on from what is there.
func (s *stager) tidy(cache string) {
	if !realDir(cache) {
		return
	}
	dirs, _ := os.ReadDir(cache)
	for _, d := range dirs {
		dir := filepath.Join(cache, d.Name())
		if !d.IsDir() {
			continue
		}
		// ENOTDIR: a damaged library where PIONEER or rekordbox is a file has none either.
		if _, err := os.Stat(prolink.StagedExport(dir)); (os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)) && !s.waiting(d.Name()) {
			if os.RemoveAll(dir) == nil {
				log.Printf("rb-bridge: removed %s: its copy was never finished", dir)
			}
			continue
		}
		// One copy runs at a time and it has not started: every .fetch- file is a leftover.
		_ = filepath.WalkDir(dir, func(p string, f fs.DirEntry, err error) error {
			if err == nil && f.Type().IsRegular() && strings.HasPrefix(f.Name(), ".fetch-") {
				os.Remove(p)
			}
			return nil
		})
	}
}

// expire removes the staged library whose player was copied longest ago, if that is more
// than stageKeep ago, no copy into it is running or waiting and no file of it is open (a
// deck may be playing one). It reports whether it removed one.
func (s *stager) expire(cache string) bool {
	inUse := s.inUse
	if inUse == nil {
		inUse = stagedInUse
	}
	if !realDir(cache) {
		return false
	}
	var oldest string
	var at time.Time
	dirs, _ := os.ReadDir(cache)
	for _, d := range dirs {
		dir := filepath.Join(cache, d.Name())
		st, err := os.Stat(prolink.StagedExport(dir))
		if !d.IsDir() || err != nil || time.Since(st.ModTime()) < stageKeep || s.waiting(d.Name()) || inUse(dir) {
			continue
		}
		if oldest == "" || st.ModTime().Before(at) {
			oldest, at = dir, st.ModTime()
		}
	}
	if oldest == "" || os.RemoveAll(oldest) != nil {
		return false
	}
	log.Printf("rb-bridge: removed %s to make room: its player was last copied %s", oldest, at.Format("2006-01-02"))
	return true
}

// realDir reports whether path is a directory itself. A symbolic link in the cache's place
// could point anywhere: nothing is ever removed through one.
func realDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir()
}

// stagedInUse reports whether any process has a file under dir open.
func stagedInUse(dir string) bool {
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		if p.Name()[0] < '0' || p.Name()[0] > '9' {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join("/proc", p.Name(), "fd"))
		for _, fd := range fds {
			if open, err := os.Readlink(filepath.Join("/proc", p.Name(), "fd", fd.Name())); err == nil && strings.HasPrefix(open, dir+string(os.PathSeparator)) {
				return true
			}
		}
	}
	return false
}

// stageName is the folder (and playlist) name of a player slot: printable
// ASCII only, since it comes from the network.
func stageName(player string, number, slot byte) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return ' '
	}, player)
	clean = strings.Join(strings.Fields(clean), " ")
	if clean == "" {
		clean = "Player"
	}
	if len(clean) > 24 {
		clean = strings.TrimSpace(clean[:24])
	}
	media := "USB"
	if slot == 2 {
		media = "SD"
	}
	return fmt.Sprintf("%s %d %s", clean, number, media)
}

// localExports lists writable drives under /media that already carry a
// Rekordbox export: only those are imported by rb-bridge, so only those get
// players' libraries.
func localExports() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	return localExportsFrom(f)
}

func localExportsFrom(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[1], "/media/") || !strings.Contains(","+fields[3]+",", ",rw,") {
			continue
		}
		mount := strings.ReplaceAll(fields[1], `\040`, " ")
		if r, err := compat.NewResolver(mount); err == nil {
			if _, err := r.Resolve("PIONEER/rekordbox/export.pdb"); err == nil {
				out = append(out, mount)
			}
		}
	}
	return out
}
