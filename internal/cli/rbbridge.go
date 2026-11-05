// Package cli holds the command lines of rb-bridge, engine-guardian and
// stream-sync. The standalone commands and the sc5000-suite multi-call binary
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
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
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
	smart := fs.Uint("smart-match", 0, "optional recommendation playlist relative to this Rekordbox track ID")
	defaultStateFile := "/tmp/sc5000-prolink.json"
	if env := os.Getenv("SC5000_PROLINK_STATE"); env != "" {
		defaultStateFile = env
	}
	stateFile := fs.String("state-file", defaultStateFile, "path to write Pro DJ Link snapshot state JSON")
	anlzRoot := fs.String("anlz-root", "/media", "root directory to scan for Rekordbox ANLZ analysis files")
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
	if *mount != "" {
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
	if *anlzRoot != "" {
		discovery.SetANLZRoot(*anlzRoot)
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
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return
		}
		if err := tmp.Close(); err != nil {
			return
		}
		_ = os.Chmod(tmp.Name(), 0644)
		_ = os.Rename(tmp.Name(), path)
	}

	err := discovery.Listen(ctx, ip, func(d prolink.Device) {
		emit(d)
		if *stateFile != "" {
			writeState(*stateFile, prolink.BuildSnapshot(discovery.Devices(time.Now())))
		}
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
