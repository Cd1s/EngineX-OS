package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/pkg/bridge"
	"github.com/Cd1s/sc5000-custom-os/pkg/enginedb"
	"github.com/Cd1s/sc5000-custom-os/pkg/prolink"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "rb-bridge:", e)
		os.Exit(1)
	}
}
func run() error {
	mount := flag.String("mount-path", "", "mounted USB/export root")
	watch := flag.Bool("watch", false, "watch attached media (imports once per attachment)")
	roots := flag.String("watch-roots", "/media,/run/media,/mnt", "comma-separated mount parent directories")
	link := flag.Bool("prolink", false, "listen for Rekordbox / Pro DJ Link announcements and player status")
	bind := flag.String("bind", "0.0.0.0", "IPv4 address for Pro DJ Link listeners")
	smart := flag.Uint("smart-match", 0, "optional recommendation playlist relative to this Rekordbox track ID")
	poll := flag.Duration("poll-interval", 2*time.Second, "mount polling interval")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *smart > 0xffffffff {
		return fmt.Errorf("smart-match track ID exceeds uint32")
	}
	opts := enginedb.Options{SmartMatch: *smart != 0, CurrentTrack: uint32(*smart)}
	if !*watch && !*link && *mount == "" {
		flag.Usage()
		return fmt.Errorf("specify --mount-path, --watch or --prolink")
	}
	var ip net.IP
	if *link {
		ip = net.ParseIP(*bind)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("bind must be IPv4")
		}
	}
	var rs []string
	if *watch {
		rs = strings.Split(*roots, ",")
		if *mount != "" {
			rs = []string{*mount}
		}
		for i, root := range rs {
			rs[i] = strings.TrimSpace(root)
			if rs[i] == "" {
				return fmt.Errorf("watch roots must not be empty")
			}
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(v any) { mu.Lock(); defer mu.Unlock(); _ = enc.Encode(v) }
	if *mount != "" && !*watch {
		r, e := bridge.Convert(ctx, *mount, opts)
		if e != nil {
			return e
		}
		emit(r)
	}
	if !*watch && !*link {
		return nil
	}
	errs := make(chan error, 2)
	workers := 0
	if *watch {
		workers++
		go func() {
			errs <- bridge.Watch(ctx, rs, opts, *poll, func(e bridge.WatchEvent) {
				if e.Err != nil {
					emit(map[string]any{"mount": e.Mount, "error": e.Err.Error()})
				} else {
					emit(e)
				}
			})
		}()
	}
	if *link {
		workers++
		go func() { errs <- prolink.NewDiscovery().Listen(ctx, ip, func(d prolink.Device) { emit(d) }) }()
	}
	var first error
	for i := 0; i < workers; i++ {
		e := <-errs
		if e != nil && !errors.Is(e, context.Canceled) && first == nil {
			first = e
		}
		cancel()
	}
	return first
}
