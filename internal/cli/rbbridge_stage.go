package cli

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

const (
	stageEvery   = 10 * time.Minute
	stageReserve = 2 << 30  // always left free on the drive
	stageMax     = 32 << 30 // per player slot
)

// stager copies Pioneer players' libraries onto a local drive (#59), at most
// once per player slot every stageEvery and one copy at a time. The drive's
// next mount imports them (bridge.ReadLibrary).
type stager struct {
	mu      sync.Mutex
	last    map[string]time.Time
	running bool
	mounts  func() []string
}

func (s *stager) observe(ctx context.Context, d prolink.Device) {
	host := net.ParseIP(d.Address)
	if host == nil {
		return
	}
	s.mu.Lock()
	key := fmt.Sprintf("%s/%d", d.Address, d.Number)
	if s.running || time.Since(s.last[key]) < stageEvery {
		s.mu.Unlock()
		return
	}
	s.last[key], s.running = time.Now(), true
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
		mounts := s.mounts()
		if len(mounts) == 0 {
			return
		}
		for _, slot := range []byte{3, 2} {
			export, _ := prolink.SlotExport(slot)
			name := stageName(d.Name, d.Number, slot)
			dst := filepath.Join(mounts[0], prolink.StagedDir, name)
			var fs syscall.Statfs_t
			if syscall.Statfs(mounts[0], &fs) != nil {
				return
			}
			budget := min(int64(fs.Bavail)*int64(fs.Bsize)-stageReserve, stageMax)
			if budget <= 0 {
				log.Printf("rb-bridge: no room on %s to copy %s", mounts[0], name)
				return
			}
			n, err := prolink.Stage(ctx, &prolink.NFSClient{IP: host}, export, dst, budget)
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
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[1], "/media/") || !strings.Contains(","+fields[3]+",", ",rw,") {
			continue
		}
		mount := strings.ReplaceAll(fields[1], `\040`, " ")
		if _, err := os.Stat(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb")); err == nil {
			out = append(out, mount)
		}
	}
	return out
}
