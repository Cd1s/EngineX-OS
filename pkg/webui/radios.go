package webui

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/wifi"
)

// storedFalse reports whether an INI-style file has key set to false in section (key
// with its "=", section with its brackets).
func storedFalse(path, section, key string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			in = line == section
			continue
		}
		if in && strings.HasPrefix(line, key) {
			return strings.TrimPrefix(line, key) == "false"
		}
	}
	return false
}

// wifiStoredOff reports whether the deck's stored choice is "Wi-Fi off": connman keeps it
// in its settings file when Engine's Wi-Fi switch is used ([WiFi] Enable=false).
func wifiStoredOff(settings string) bool { return storedFalse(settings, "[WiFi]", "Enable=") }

// bluetoothStoredOff reports whether Engine's own Bluetooth switch was off when the deck
// was last used: Engine keeps it in Engine.conf ([Bluetooth] Enabled=false), in a
// directory named after the board. Any doubt counts as "on".
func bluetoothStoredOff(confGlob string) bool {
	files, err := filepath.Glob(confGlob)
	if err != nil || len(files) == 0 {
		return false
	}
	for _, f := range files {
		if !storedFalse(f, "[Bluetooth]", "Enabled=") {
			return false
		}
	}
	return true
}

// UnblockRadios clears rfkill's soft block on Bluetooth and Wi-Fi. The kernel boots
// with rfkill.default_state=0. The Bluetooth radio is always unblocked (#60): Engine
// powers the adapter through BlueZ, which cannot clear a soft block, so without this
// the Bluetooth switch could never be turned on. Whether Bluetooth is on after a boot
// is Engine's own stored switch (Engine.conf, [Bluetooth] Enabled), which Engine
// applies about 20 s after it starts; BluetoothOffAsStored closes that gap when the
// switch is off. Wi-Fi follows connman's stored choice, off stays off (#326). This
// runs from the writable partition, so it also works on a deck whose read-only root
// still has the older legacy-debloat.
func UnblockRadios(root, connmanSettings string) {
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
		case "bluetooth":
			if err := os.WriteFile(filepath.Join(dir, "soft"), []byte("0"), 0o644); err != nil {
				log.Printf("stream-sync: could not unblock %s: %v", e.Name(), err)
			}
		case "wlan":
			if wifiStoredOff(connmanSettings) {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, "soft"), []byte("0"), 0o644); err != nil {
				log.Printf("stream-sync: could not unblock %s: %v", e.Name(), err)
			}
		}
	}
}

const (
	bluetoothAdapter  = "/sys/class/bluetooth/hci0"
	engineConfGlob    = "/data/.config/*.user.settings/AIR Music Technology/Engine.conf"
	bluetoothBootMark = "/run/enginex-bluetooth-boot"
)

// BluetoothOffAsStored closes the window in which Bluetooth was on after a boot although
// the user had switched it off. Clearing the soft block (UnblockRadios) makes BlueZ power
// the adapter on at once, and Engine applies its stored "off" only about 20 s after it
// starts (23 s on an SC5000). With the switch stored off, the adapter is powered off here
// as soon as it is up. The radio stays unblocked, so Engine's Bluetooth switch still turns
// it on. With the switch stored on, nothing is done. Once per boot; it ends by itself.
func BluetoothOffAsStored(ctx context.Context) {
	bluetoothOffAsStored(ctx, bluetoothAdapter, engineConfGlob, bluetoothBootMark, 200*time.Millisecond, 6*time.Second)
}

func bluetoothOffAsStored(ctx context.Context, adapter, confGlob, mark string, every, limit time.Duration) {
	if _, err := os.Stat(adapter); err != nil {
		return // no Bluetooth adapter
	}
	if guardHostCommand("busctl") != nil {
		return // never the Bluetooth of a machine that is not a deck
	}
	// Once per boot, whatever is stored (/run is empty after a reboot): a later restart of
	// stream-sync must not poll while a set plays, nor switch off what the user has just
	// turned on in Engine.
	f, err := os.OpenFile(mark, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return
	}
	f.Close()
	if !bluetoothStoredOff(confGlob) {
		return
	}
	c, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	if bluetoothOffOnceUp(c, every, func() (bool, error) { return bluetoothPowered(c) }, func() error { return bluetoothPowerOff(c) }) {
		log.Printf("stream-sync: Bluetooth is off as stored, %d ms after the check began", time.Since(start).Milliseconds())
	} else {
		log.Printf("stream-sync: Bluetooth is stored off and the adapter did not power on within %s: nothing done", limit)
	}
}

// bluetoothOffOnceUp polls until the adapter reports powered, switches it off, and returns
// true once it has then read "off" three times in a row. When ctx ends first it reports
// whether it switched the adapter off at all.
func bluetoothOffOnceUp(ctx context.Context, every time.Duration, powered func() (bool, error), off func() error) bool {
	tick := time.NewTicker(every)
	defer tick.Stop()
	switched, said, quiet := false, false, 0
	for {
		on, err := powered()
		switch {
		case err != nil: // bluetoothd does not answer yet
			quiet = 0
		case on:
			quiet = 0
			if err := off(); err == nil {
				switched = true
			} else if !said {
				said = true
				log.Printf("stream-sync: could not switch Bluetooth off as stored: %v", err)
			}
		case switched:
			if quiet++; quiet == 3 {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return switched
		case <-tick.C:
		}
	}
}

// busctlBluetooth reads or writes the adapter's Powered property the way Engine does:
// through BlueZ on the system bus.
func busctlBluetooth(ctx context.Context, verb string, value ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	args := append([]string{"--system", verb, "org.bluez", "/org/bluez/hci0", "org.bluez.Adapter1", "Powered"}, value...)
	out, err := exec.CommandContext(c, "busctl", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("busctl %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func bluetoothPowered(ctx context.Context) (bool, error) {
	out, err := busctlBluetooth(ctx, "get-property")
	return out == "b true", err
}

func bluetoothPowerOff(ctx context.Context) error {
	_, err := busctlBluetooth(ctx, "set-property", "b", "false")
	return err
}

type wifiInterfaceState struct {
	up    bool
	index int
}

func readWiFiInterfaceState() wifiInterfaceState {
	iface, err := net.InterfaceByName("wlan0")
	if err != nil {
		return wifiInterfaceState{}
	}
	return wifiInterfaceState{up: iface.Flags&net.FlagUp != 0, index: iface.Index}
}

func readWiFiPowerSaveOn() bool {
	on, err := wifi.GetPowerSave("wlan0")
	return err == nil && on
}

// DisableWiFiPowerSave keeps 802.11 power saving off on wlan0 until ctx ends.
// Stock kernel 6.18 initializes wdev->ps=1 on new wireless interfaces and a
// BCM4339 firmware/SDIO reset re-probes wlan0 with a new ifindex and power
// saving re-enabled; wpa_supplicant can also set power_save. Steady-state
// passes read only interface state and kernel wdev->ps (no driver I/O or iw
// fork) and write only on first UP, retry after error, DOWN->UP, ifindex
// change, or kernel power_save flipping back on.
func DisableWiFiPowerSave(ctx context.Context) {
	if guardHostCommand("iw") != nil {
		return
	}
	disableWiFiPowerSave(ctx, 5*time.Second, readWiFiInterfaceState, readWiFiPowerSaveOn, func() error {
		return setWiFiPowerSaveOff(ctx)
	})
}

func setWiFiPowerSaveOff(ctx context.Context) error {
	if stock, err := exec.LookPath("iw"); err == nil {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(c, stock, "dev", "wlan0", "set", "power_save", "off").CombinedOutput()
		if err != nil {
			return fmt.Errorf("iw: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return wifi.SetPowerSave("wlan0", false)
}

func disableWiFiPowerSave(ctx context.Context, every time.Duration, state func() wifiInterfaceState, psOn func() bool, disable func() error) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	applied, everApplied, errLogged := false, false, false
	prevIndex := 0
	for {
		if ctx.Err() != nil {
			return
		}
		st := state()
		if !st.up {
			applied = false
			errLogged = false
			if st.index == 0 {
				prevIndex = 0
			}
		} else {
			reason := ""
			switch {
			case applied && st.index != prevIndex:
				reason = "interface reset"
			case !applied:
				reason = "interface up"
			case psOn != nil && psOn():
				reason = "power_save re-enabled"
			}
			if reason != "" {
				if err := disable(); err != nil {
					if ctx.Err() != nil {
						return
					}
					applied = false
					if !errLogged {
						errLogged = true
						log.Printf("stream-sync: could not disable Wi-Fi power saving: %v", err)
					}
				} else {
					applied = true
					prevIndex = st.index
					errLogged = false
					if !everApplied {
						everApplied = true
						log.Print("stream-sync: Wi-Fi power saving disabled on wlan0")
					} else {
						log.Printf("stream-sync: Wi-Fi power saving re-disabled on wlan0 (%s)", reason)
					}
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

// ownARPReady reports whether eth0 and wlan0 already carry arp_announce=2 and
// arp_ignore=1 so steady-state guard passes stay read-only while an interface
// reset (which restores kernel defaults 0/0) is detected before DHCP completes.
func ownARPReady(conf string) bool {
	for _, dev := range []string{"eth0", "wlan0"} {
		for _, want := range [][2]string{{"arp_announce", "2"}, {"arp_ignore", "1"}} {
			b, err := os.ReadFile(filepath.Join(conf, dev, want[0]))
			if err != nil || strings.TrimSpace(string(b)) != want[1] {
				return false
			}
		}
	}
	return true
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
		// address, so neither ever does. Tried again until both interfaces are there,
		// and re-applied if an interface reset restores stock 0/0 defaults;
		// guardRoutesOnce still sets them before the cable goes first.
		if !own || !ownARPReady(conf) {
			err := ownARPAddress(conf)
			if err == nil {
				if !own {
					log.Printf("stream-sync: eth0 and wlan0 ask and answer by ARP for their own address only")
				} else {
					log.Printf("stream-sync: restored ARP settings on eth0 and wlan0 after interface reset")
				}
				own, said = true, false
			} else {
				own = false
				if !said {
					said = true
					log.Printf("stream-sync: eth0 and wlan0 keep the stock ARP settings for now: %v", err)
				}
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
