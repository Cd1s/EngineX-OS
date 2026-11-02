package webui

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UnblockRadios clears rfkill's soft block on Bluetooth and Wi-Fi. The kernel boots
// with rfkill.default_state=0. This runs from the writable partition, so it also
// works on a deck whose read-only root still has the older sc5000-debloat.
func UnblockRadios(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		kind, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(kind)) {
		case "bluetooth", "wlan":
			if err := os.WriteFile(filepath.Join(dir, "soft"), []byte("0"), 0o644); err != nil {
				log.Printf("stream-sync: could not unblock %s: %v", e.Name(), err)
			}
		}
	}
}

// strayEthernetDefault finds a default route on eth0 with no gateway, while wlan0 has
// a real one. connman adds it when a cable to a computer or another deck has no DHCP
// server, and it would take all internet traffic away from Wi-Fi.
func strayEthernetDefault(routes string) (string, bool) {
	var stray string
	wifi := false
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || (f[0] != "default" && f[0] != "0.0.0.0") {
			continue
		}
		if f[1] == "dev" && f[2] == "eth0" {
			stray = f[0]
		}
		if f[1] == "via" && strings.Contains(line, "dev wlan0") {
			wifi = true
		}
	}
	return stray, stray != "" && wifi
}

// GuardRoutes removes that route whenever it appears, until ctx ends. The cable keeps
// its link-local address, so Link and direct connections are unaffected.
func GuardRoutes(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		if out, err := exec.CommandContext(ctx, "ip", "route", "show").Output(); err == nil {
			if target, ok := strayEthernetDefault(string(out)); ok {
				if err := exec.CommandContext(ctx, "ip", "route", "del", target, "dev", "eth0", "scope", "link").Run(); err != nil {
					log.Printf("stream-sync: could not remove the stray eth0 route: %v", err)
				} else {
					log.Printf("stream-sync: removed a default route on eth0 that took traffic from Wi-Fi")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
