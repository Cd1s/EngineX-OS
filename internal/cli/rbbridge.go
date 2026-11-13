// Package cli holds the command lines of rb-bridge, engine-guardian and
// stream-sync. The standalone commands and the enginex-suite multi-call binary
// that the player runs share them, so the two cannot drift apart.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/djay"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// RBBridge converts one mounted export, or listens for Pro DJ Link.
//
// --watch is retired. It rescanned every mount and wrote m.db while Engine had
// it open (SQLITE_BUSY, and on real media corruption). The edisksd mount hook
// runs --mount-path before Engine sees the volume. The units baked into the
// player's read-only /etc still pass --watch, so it exits at once instead of
// failing, and systemd does not restart it.
func RBBridge(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("rb-bridge", flag.ContinueOnError)
	mount := fs.String("mount-path", "", "mounted USB/export root")
	link := fs.Bool("prolink", false, "listen for Rekordbox / Pro DJ Link announcements and player status")
	bind := fs.String("bind", "0.0.0.0", "IPv4 address for Pro DJ Link listeners")
	full := fs.Bool("full", false, "with --mount-path: read every file for cues and waveforms, even if unchanged")
	migrateArt := fs.Bool("migrate-artwork", false, "with --mount-path: only give an existing library the cover files Engine Link needs; Engine must not have the volume open")
	smart := fs.Uint("smart-match", 0, "optional recommendation playlist relative to this Rekordbox track ID")
	defaultStateFile := "/tmp/enginex-prolink.json"
	if env := os.Getenv("ENGINEX_PROLINK_STATE"); env != "" {
		defaultStateFile = env
	}
	stateFile := fs.String("state-file", defaultStateFile, "path to write Pro DJ Link snapshot state JSON")
	anlzRoot := fs.String("anlz-root", "/media", "root directory to scan for Rekordbox ANLZ analysis files")
	mediaCache := fs.String("media-cache", prolink.SharedMediaCacheDir(), "where tracks read from Pioneer players over the network are kept (#59)")
	uiSettings := fs.String("ui-settings", "/data/enginex/ui/settings.conf", "deck settings; prolink_share=1 enables reading players' media")
	watch := fs.Bool("watch", false, "retired; exits at once")
	fs.String("watch-roots", "", "retired")
	fs.Duration("poll-interval", 0, "retired")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *watch {
		log.Print("rb-bridge: --watch is retired; the mount hook imports each volume before Engine opens it")
		return nil
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *smart > 0xffffffff {
		return fmt.Errorf("smart-match track ID exceeds uint32")
	}
	if !*link && *mount == "" {
		fs.Usage()
		return fmt.Errorf("specify --mount-path or --prolink")
	}
	var ip net.IP
	if *link {
		if ip = net.ParseIP(*bind); ip == nil || ip.To4() == nil {
			return fmt.Errorf("bind must be IPv4")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var mu sync.Mutex
	enc := json.NewEncoder(stdout)
	emit := func(v any) { mu.Lock(); defer mu.Unlock(); _ = enc.Encode(v) }
	if *mount != "" && *migrateArt {
		n, err := enginedb.MigrateArtwork(ctx, *mount)
		emit(map[string]int{"ArtworkMigrated": n})
		return err
	}
	if *mount != "" {
		_ = syncDjayLibrary(*mount)
		opts := enginedb.Options{SmartMatch: *smart != 0, CurrentTrack: uint32(*smart)}
		r, err := bridge.ConvertAtMount(ctx, *mount, opts, *full)
		if err != nil {
			return err
		}
		emit(r)
	}
	if !*link {
		return nil
	}

	discovery := prolink.NewDiscovery()
	discovery.SetIgnoreLocal(true) // never stage or cache the deck's own export
	if *anlzRoot != "" {
		discovery.SetANLZRoot(*anlzRoot)
	}
	if *mediaCache != "" {
		discovery.SetMediaCache(&prolink.MediaCache{Dir: *mediaCache, Enabled: func() bool { return settingOn(*uiSettings, "prolink_share") }})
	}

	var stateMu sync.Mutex
	writeState := func(path string, snap prolink.Snapshot) {
		stateMu.Lock()
		defer stateMu.Unlock()
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return
		}
		tmp, err := os.CreateTemp(dir, ".prolink-*.tmp")
		if err != nil {
			return
		}
		defer os.Remove(tmp.Name())
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			tmp.Close()
			return
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return
		}
		if err := tmp.Close(); err != nil {
			return
		}
		_ = os.Chmod(tmp.Name(), 0644)
		_ = os.Rename(tmp.Name(), path)
	}

	_, _ = resourceguard.New(resourceguard.DefaultConfig()).ApplyToProcess(0)
	st := &stager{mounts: localExports, last: map[string]time.Time{}}
	// Packets arrive many times a second; snapshot at most every 500 ms and only when something changed.
	var dirty atomic.Bool
	if *stateFile != "" {
		go func() {
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					if dirty.Swap(false) {
						writeState(*stateFile, prolink.BuildSnapshot(discovery.Devices(time.Now())))
					}
					return
				case <-tick.C:
					if dirty.Swap(false) {
						writeState(*stateFile, prolink.BuildSnapshot(discovery.Devices(time.Now())))
					}
				}
			}
		}()
	}
	err := discovery.Listen(ctx, ip, func(d prolink.Device) {
		emit(d)
		if d.PacketType == 6 && d.Number >= 1 && d.Number <= 6 && settingOn(*uiSettings, "prolink_share") {
			st.observe(ctx, d)
		}
		dirty.Store(true)
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// settingOn reports whether key=1 is in a key=value settings file.
func settingOn(path, key string) bool {
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == key+"=1" {
			return true
		}
	}
	return false
}

// syncDjayLibrary discovers and logs Algoriddim djay libraries on a mounted volume.
func syncDjayLibrary(mountPoint string) error {
	libs, err := djay.Discover(mountPoint)
	if err != nil {
		return err
	}
	if len(libs) == 0 {
		return nil
	}
	totalTracks := 0
	totalPlaylists := 0
	for _, lib := range libs {
		totalTracks += len(lib.Tracks)
		totalPlaylists += len(lib.Playlists)
	}
	log.Printf("rb-bridge: discovered %d djay libraries with %d playlists and %d tracks on %s", len(libs), totalPlaylists, totalTracks, mountPoint)
	return nil
}
