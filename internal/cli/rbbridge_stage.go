package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
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
	stageEvery   = 10 * time.Minute
	stageReserve = 2 << 30  // always left free on the drive
	stageMax     = 32 << 30 // per player slot
)

// stager copies Pioneer players' libraries onto a local drive (#59), at most
// once per host/slot every stageEvery and one copy at a time. The drive's
// next mount imports them (bridge.ReadLibrary).
type stager struct {
	mu      sync.Mutex
	copyMu  sync.Mutex
	last    map[string]time.Time
	running map[string]bool
	mounts  func() []string
	statfs  func(string, *syscall.Statfs_t) error
	copy    func(context.Context, *prolink.NFSClient, string, string, int64) (int, error)
}

func (s *stager) observe(ctx context.Context, d prolink.Device) {
	host := net.ParseIP(d.Address)
	if host == nil {
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
				delete(s.running, fmt.Sprintf("%s/%d", host.String(), slot))
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
				budget = min(int64(fs.Bavail)*int64(fs.Bsize)-stageReserve, stageMax)
				if budget > 0 {
					dst = filepath.Join(mount, prolink.StagedDir, name)
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
