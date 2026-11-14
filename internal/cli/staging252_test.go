package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

type staging252Copy struct {
	host, export, dst string
	budget            int64
}

func staging252Next(t *testing.T, calls <-chan staging252Copy) staging252Copy {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("staging copy did not start")
		return staging252Copy{}
	}
}

func TestStaging252HostSlotExclusion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := make(chan staging252Copy, 64)
	release := make(chan struct{})
	root := t.TempDir()
	s := &stager{last: map[string]time.Time{}, mounts: func() []string { return []string{root} },
		statfs: func(_ string, fs *syscall.Statfs_t) error { fs.Bavail = stageReserve + 4096; fs.Bsize = 1; return nil },
		copy: func(ctx context.Context, c *prolink.NFSClient, export, dst string, budget int64) (int, error) {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			calls <- staging252Copy{c.IP.String(), export, dst, budget}
			select {
			case <-release:
				return 0, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}}
	a := prolink.Device{Address: "192.0.2.1", Number: 1, Name: "CDJ"}
	b := prolink.Device{Address: "192.0.2.2", Number: 1, Name: "CDJ"}
	s.observe(ctx, a)
	if c := staging252Next(t, calls); c.host != a.Address || c.export != "/C/" {
		t.Fatal(c)
	}
	// A copy outliving the cooldown still excludes this host and slot.
	s.mu.Lock()
	for key := range s.last {
		s.last[key] = time.Now().Add(-2 * stageEvery)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); d := a; d.Number = byte(i%2 + 1); s.observe(ctx, d) }()
	}
	wg.Wait()
	s.observe(ctx, b)
	close(release)
	seen := map[string]bool{a.Address + "/C/": true}
	for range 3 {
		c := staging252Next(t, calls)
		key := c.host + c.export
		if seen[key] || (c.host != a.Address && c.host != b.Address) {
			t.Fatalf("duplicate copy: %+v", c)
		}
		seen[key] = true
	}
	// Wait for both queued hosts to finish, then check the cooldown as well.
	s.copyMu.Lock()
	s.copyMu.Unlock()
	a.Number = 2
	s.observe(ctx, a)
	select {
	case c := <-calls:
		t.Fatalf("duplicate copy: %+v", c)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStaging252ChoosesVolumeWithReserve(t *testing.T) {
	for _, first := range []string{"full", "unavailable", "readonly"} {
		t.Run(first, func(t *testing.T) {
			calls := make(chan staging252Copy, 2)
			mounts := []string{filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")}
			s := &stager{last: map[string]time.Time{}, mounts: func() []string { return mounts },
				statfs: func(p string, fs *syscall.Statfs_t) error {
					fs.Bsize = 1
					fs.Bavail = stageReserve + 4096
					if p == mounts[0] {
						switch first {
						case "full":
							fs.Bavail = stageReserve
						case "unavailable":
							return os.ErrNotExist
						case "readonly":
							fs.Flags = 1
						}
					}
					return nil
				},
				copy: func(_ context.Context, c *prolink.NFSClient, export, dst string, budget int64) (int, error) {
					calls <- staging252Copy{c.IP.String(), export, dst, budget}
					return 0, nil
				},
			}
			s.observe(t.Context(), prolink.Device{Address: "192.0.2.1", Number: 1})
			for range 2 {
				c := staging252Next(t, calls)
				if !strings.HasPrefix(c.dst, mounts[1]+string(os.PathSeparator)) || c.budget != 4096 {
					t.Fatalf("wrong destination/reserve: %+v", c)
				}
			}
		})
	}
}

func TestStaging252HiddenWritableExport(t *testing.T) {
	// The production mount filter only accepts /media paths.
	root, err := os.MkdirTemp("/media", "staging252 space-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	testfixture.Export(t, root)
	if err := os.Rename(filepath.Join(root, "PIONEER"), filepath.Join(root, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(root, " ", `\040`)
	for _, mode := range []string{"rw", "ro"} {
		mounts := localExportsFrom(strings.NewReader(fmt.Sprintf("/dev/test %s ext4 %s 0 0\n", escaped, mode)))
		if mode == "rw" && (len(mounts) != 1 || mounts[0] != root) {
			t.Fatalf("hidden export excluded: %v", mounts)
		}
		if mode == "ro" && len(mounts) != 0 {
			t.Fatal("read-only mount selected")
		}
	}
}

func TestStaging252HiddenNFSAndRepeat(t *testing.T) {
	source := t.TempDir()
	testfixture.Export(t, source)
	if err := os.Rename(filepath.Join(source, "PIONEER"), filepath.Join(source, ".PIONEER")); err != nil {
		t.Fatal(err)
	}
	server := &prolink.NFSServer{Root: source}
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client := &prolink.NFSClient{IP: []byte{127, 0, 0, 1}, Portmap: server.PortmapPort()}
	dst := t.TempDir()
	if n, err := prolink.Stage(t.Context(), client, "/C/", dst, 1<<30); err != nil || n == 0 {
		t.Fatalf("stage hidden source: %d %v", n, err)
	}
	if n, err := prolink.Stage(t.Context(), client, "/C/", dst, 1<<30); err != nil || n != 0 {
		t.Fatalf("repeat copied files: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "Engine Library")); !os.IsNotExist(err) {
		t.Fatalf("staging wrote an Engine library: %v", err)
	}
}
