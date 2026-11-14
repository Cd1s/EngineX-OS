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

// wifiMetric is what a wlan0 subnet route gets when the cable reaches the same subnet.
const wifiMetric = "600"

// wifiTwinRoutes finds the subnets eth0 and wlan0 are both connected to. Both routes have
// the same metric and the first one added wins: with Wi-Fi up first, Engine Link served the
// other deck its files over Wi-Fi (1.3-1.9 MB/s; the cable beside it carries 14-17 MB/s,
// measured on two SC5000). For each it returns the route wlan0 must get (add, nil when it
// is already there) and the one to take away after it (del).
func wifiTwinRoutes(routes string) (add, del [][]string) {
	type route struct{ dev, src, metric string }
	subnets := map[string][]route{}
	var order []string
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.Contains(f[0], "/") || !strings.Contains(line, "scope link") {
			continue // a default route, the gateway's host route, a route through a gateway
		}
		r := route{metric: "0"}
		for i := 1; i+1 < len(f); i += 2 {
			switch f[i] {
			case "dev":
				r.dev = f[i+1]
			case "src":
				r.src = f[i+1]
			case "metric":
				r.metric = f[i+1]
			}
		}
		if _, seen := subnets[f[0]]; !seen {
			order = append(order, f[0])
		}
		subnets[f[0]] = append(subnets[f[0]], r)
	}
	for _, subnet := range order {
		cable, demoted, wifi := false, false, route{}
		for _, r := range subnets[subnet] {
			switch {
			case r.dev == "eth0" && r.metric == "0":
				cable = true
			case r.dev == "wlan0" && r.metric == "0":
				wifi = r
			case r.dev == "wlan0" && r.metric == wifiMetric:
				demoted = true
			}
		}
		if !cable || wifi.dev == "" {
			continue
		}
		args := []string{subnet, "dev", "wlan0", "scope", "link"}
		if wifi.src != "" {
			args = append(args, "src", wifi.src)
		}
		if !demoted {
			add = append(add, append(append([]string{"route", "add"}, args...), "metric", wifiMetric))
		} else {
			add = append(add, nil)
		}
		del = append(del, append(append([]string{"route", "del"}, args...), "metric", "0"))
	}
	return add, del
}

// arpConf is where the kernel keeps each interface's ARP settings.
const arpConf = "/proc/sys/net/ipv4/conf"

// ownARPAddress makes eth0 and wlan0 speak by ARP for their own address only: they ask
// for a neighbour's address with their own (arp_announce 2; stock is 0, the address of the
// packet that waits) and answer only a request for an address they hold themselves
// (arp_ignore 1; stock is 0, any address of the deck). With the cable first, a packet from
// Wi-Fi's address to the subnet leaves by the cable, and its ARP request said there that
// Wi-Fi's address is the sender; and a request for Wi-Fi's address that reaches the cable
// was answered there, with the cable's hardware address. Either way the router then sent
// that address's traffic to the cable, where a socket bound to wlan0 never gets it:
// Engine's Wi-Fi online check is one, it timed out and Engine disconnected Wi-Fi.
func ownARPAddress(conf string) error {
	for _, dev := range []string{"eth0", "wlan0"} {
		for _, set := range [][2]string{{"arp_announce", "2"}, {"arp_ignore", "1"}} {
			if err := os.WriteFile(filepath.Join(conf, dev, set[0]), []byte(set[1]), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// GuardRoutes keeps two things right until ctx ends, whenever connman sets them up again:
// no gateway-less default route on eth0 (the cable keeps its link-local address, so Link
// and direct connections are unaffected), and the cable before Wi-Fi where both reach the
// same subnet. The default route stays where connman put it, and Wi-Fi keeps its own
// route for when the cable is out.
func GuardRoutes(ctx context.Context) {
	if guardHostCommand("ip") != nil {
		return // never the routes of a machine that is not a deck
	}
	guardRoutes(ctx, arpConf, 5*time.Second)
}

func guardRoutes(ctx context.Context, conf string, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	own, said := false, false
	for {
		// From the start, not from the first shared subnet on: a pass comes up to 5 s
		// after the second address, and until then either interface asked and answered
		// for the other's. At boot stream-sync is up seconds before connman has an
		// address, so neither ever does. Tried again until both interfaces are there;
		// guardRoutesOnce still sets them before the cable goes first, for an interface
		// that came back later.
		if !own {
			err := ownARPAddress(conf)
			if own = err == nil; own {
				log.Printf("stream-sync: eth0 and wlan0 ask and answer by ARP for their own address only")
			} else if !said {
				said = true
				log.Printf("stream-sync: eth0 and wlan0 keep the stock ARP settings for now: %v", err)
			}
		}
		guardRoutesOnce(ctx, conf)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func guardRoutesOnce(ctx context.Context, conf string) {
	out, err := exec.CommandContext(ctx, "ip", "route", "show").Output()
	if err != nil {
		return
	}
	if target, ok := strayEthernetDefault(string(out)); ok {
		if err := exec.CommandContext(ctx, "ip", "route", "del", target, "dev", "eth0", "scope", "link").Run(); err != nil {
			log.Printf("stream-sync: could not remove the stray eth0 route: %v", err)
		} else {
			log.Printf("stream-sync: removed a default route on eth0 that took traffic from Wi-Fi")
		}
	}
	add, del := wifiTwinRoutes(string(out))
	if len(del) > 0 {
		// Before the cable goes first, never after: see ownARPAddress.
		if err := ownARPAddress(conf); err != nil {
			log.Printf("stream-sync: the cable does not go before Wi-Fi: %v", err)
			return
		}
	}
	for i := range del {
		// Its own route first: Wi-Fi is never left without one.
		if add[i] != nil {
			if err := exec.CommandContext(ctx, "ip", add[i]...).Run(); err != nil {
				log.Printf("stream-sync: could not put the cable before Wi-Fi on %s: %v", del[i][2], err)
				continue
			}
		}
		if err := exec.CommandContext(ctx, "ip", del[i]...).Run(); err != nil {
			log.Printf("stream-sync: could not put the cable before Wi-Fi on %s: %v", del[i][2], err)
		} else {
			log.Printf("stream-sync: %s is on the cable and on Wi-Fi: the cable goes first", del[i][2])
		}
	}
}
