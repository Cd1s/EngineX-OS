package webui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real deck, an SC5000 behind a Wi-Fi repeater by cable and by Wi-Fi, a kit that set the
// ARP settings only before it put the cable first: at one boot of six Wi-Fi had its
// address 5 s before the route guard's next pass, eth0 and wlan0 asked and answered for
// each other's address until then, and 11 s after the pass Engine logged "online check
// failed Timeout was reached" and disconnected Wi-Fi. At the next boot that wait was
// 1.3 s and Wi-Fi stayed. stream-sync is up about 9 s before connman has an address on
// either interface, so the settings are there before any address is.
func TestEth0AndWiFiSpeakForTheirOwnAddressBeforeEitherHasOne(t *testing.T) {
	dir := t.TempDir()
	conf, table, calls := filepath.Join(dir, "conf"), filepath.Join(dir, "table"), filepath.Join(dir, "calls")
	settings := []string{"eth0/arp_announce", "wlan0/arp_announce", "eth0/arp_ignore", "wlan0/arp_ignore"}
	stock := func(root string, devs ...string) {
		for _, dev := range devs {
			os.MkdirAll(filepath.Join(root, dev), 0o755)
			for _, name := range []string{"arp_announce", "arp_ignore"} {
				os.WriteFile(filepath.Join(root, dev, name), []byte("0\n"), 0o644)
			}
		}
	}
	now := func(names ...string) string {
		var got []string
		for _, name := range names {
			b, _ := os.ReadFile(filepath.Join(conf, name))
			got = append(got, strings.TrimSpace(string(b)))
		}
		return strings.Join(got, " ")
	}
	// ip: prints the table, and records every change with what the two interfaces ask with
	// and answer for at that moment.
	script := "#!/bin/sh\nif [ \"$2\" = show ]; then cat '" + table + "'; exit; fi\ncd '" + conf + "'\n" +
		"echo \"$* | $(for f in " + strings.Join(settings, " ") + "; do printf '%s ' \"$(cat $f)\"; done)\" >> '" + calls + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := guardHostCommand("ip"); err != nil {
		t.Fatal(err)
	}
	start := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); guardRoutes(ctx, conf, 5*time.Millisecond) }()
		stop = func() { cancel(); <-done }
		t.Cleanup(stop) // after a failed check too: never left running once PATH is the host's again
		return stop
	}
	within := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(3 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				b, _ := os.ReadFile(calls)
				t.Fatalf("%s: arp_announce and arp_ignore of eth0 and wlan0 are %q, not 2 2 1 1\n%s", what, now(settings...), b)
			}
		}
	}

	// A boot: both interfaces are there, neither has an address or a route yet.
	stock(conf, "lo", "eth0", "wlan0")
	os.WriteFile(table, nil, 0o644)
	stop := start()
	within("before either interface has an address", func() bool { return now(settings...) == "2 2 1 1" })
	if b, _ := os.ReadFile(calls); len(b) != 0 || now("lo/arp_announce", "lo/arp_ignore") != "0 0" {
		t.Fatalf("a route or another interface was changed: %q %s", b, now("lo/arp_announce", "lo/arp_ignore"))
	}
	// connman gives both an address in one subnet: the cable goes first as it did.
	os.WriteFile(table, []byte("default via 192.168.1.6 dev wlan0 \n192.168.1.0/24 dev wlan0 scope link  src 192.168.1.187 \n192.168.1.0/24 dev eth0 scope link  src 192.168.1.238 \n"), 0o644)
	const first = "route add 192.168.1.0/24 dev wlan0 scope link src 192.168.1.187 metric 600 | 2 2 1 1 \n" +
		"route del 192.168.1.0/24 dev wlan0 scope link src 192.168.1.187 metric 0 | 2 2 1 1 \n"
	within("once both are in one subnet", func() bool { b, _ := os.ReadFile(calls); return len(b) >= len(first) })
	stop()
	if b, _ := os.ReadFile(calls); !strings.HasPrefix(string(b), first) {
		t.Fatalf("the cable does not go first as before:\n%s", b)
	}

	// The Wi-Fi interface appears only after stream-sync has started: set at the next pass.
	os.RemoveAll(conf)
	os.Remove(calls)
	stock(conf, "lo", "eth0")
	os.WriteFile(table, nil, 0o644)
	start()
	within("eth0 while wlan0 is not there", func() bool { return now("eth0/arp_announce", "eth0/arp_ignore") == "2 1" })
	stock(dir, "wlan0") // whole, then in place: the kernel's directory never appears half made
	if err := os.Rename(filepath.Join(dir, "wlan0"), filepath.Join(conf, "wlan0")); err != nil {
		t.Fatal(err)
	}
	within("an interface that came after the start", func() bool { return now(settings...) == "2 2 1 1" })
}

func TestWiFiPowerSaveWaitsForUpAndAppliesOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks, writes := 0, 0
	disableWiFiPowerSave(ctx, time.Millisecond, func() wifiInterfaceState {
		checks++
		if checks >= 6 {
			cancel()
		}
		return wifiInterfaceState{up: checks >= 3, index: 3}
	}, func() bool { return false }, func() error { writes++; return nil })
	if checks != 6 || writes != 1 {
		t.Fatalf("checks=%d writes=%d, want 6 checks and 1 steady-state write", checks, writes)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	disableWiFiPowerSave(ctx2, time.Millisecond, func() wifiInterfaceState {
		cancel2()
		return wifiInterfaceState{}
	}, func() bool { return false }, func() error { t.Fatal("set before up"); return nil })
}

func TestWiFiPowerSaveUsesStockIW(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "driver_error"}[fail], func(t *testing.T) {
			bin := t.TempDir()
			calls := filepath.Join(bin, "calls")
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$COMMAND_LOG\"\n"
			if fail {
				script += "echo 'driver refused power save' >&2\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "iw"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("COMMAND_LOG", calls)
			err := setWiFiPowerSaveOff(context.Background())
			if fail {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !strings.Contains(err.Error(), "driver refused") {
					t.Fatalf("lost stock iw error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if out, err := os.ReadFile(calls); err != nil || string(out) != "dev wlan0 set power_save off\n" {
				t.Fatalf("stock iw arguments: %q, %v", out, err)
			}
		})
	}
}

func TestWiFiPowerSaveRetriesAfterTransientError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts, checks := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		disableWiFiPowerSave(ctx, time.Millisecond, func() wifiInterfaceState {
			checks++
			if checks >= 5 {
				cancel()
			}
			return wifiInterfaceState{up: true, index: 3}
		}, func() bool { return false }, func() error {
			attempts++
			if attempts == 1 {
				return errors.New("driver busy")
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for retry")
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2 (retry once after transient error, then stay quiet)", attempts)
	}
}

func TestWiFiPowerSaveReappliesAfterDownUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Sequence: UP (apply #1), UP (steady), DOWN, DOWN, UP (apply #2), UP (steady -> cancel)
	seq := []bool{true, true, false, false, true, true}
	step, writes := 0, 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		disableWiFiPowerSave(ctx, time.Millisecond, func() wifiInterfaceState {
			if step >= len(seq) {
				cancel()
				return wifiInterfaceState{up: true, index: 3}
			}
			v := seq[step]
			step++
			return wifiInterfaceState{up: v, index: 3}
		}, func() bool { return false }, func() error {
			writes++
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for down->up reapply")
	}
	if writes != 2 {
		t.Fatalf("writes=%d after UP->DOWN->UP (step=%d), want 2", writes, step)
	}
}

func TestWiFiPowerSaveReappliesOnInterfaceIndexChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Simulates brcmf_fw_crashed -> brcmf_sdio_bus_reset recreating wlan0 within one tick.
	indices := []int{3, 3, 4, 4}
	step, writes := 0, 0
	disableWiFiPowerSave(ctx, time.Millisecond, func() wifiInterfaceState {
		if step >= len(indices) {
			cancel()
			return wifiInterfaceState{up: true, index: 4}
		}
		idx := indices[step]
		step++
		return wifiInterfaceState{up: true, index: idx}
	}, func() bool { return false }, func() error {
		writes++
		return nil
	})
	if writes != 2 {
		t.Fatalf("writes=%d across ifindex 3->4 reset, want 2", writes)
	}
}

func TestWiFiPowerSaveReappliesWhenKernelResetsPowerSaveOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	psStates := []bool{false, false, true, false, false}
	step, writes := 0, 0
	ps := false
	disableWiFiPowerSave(ctx, time.Millisecond, func() wifiInterfaceState {
		if step >= len(psStates) {
			cancel()
			return wifiInterfaceState{up: true, index: 3}
		}
		ps = psStates[step]
		step++
		return wifiInterfaceState{up: true, index: 3}
	}, func() bool { return ps }, func() error {
		writes++
		ps = false
		return nil
	})
	if writes != 2 {
		t.Fatalf("writes=%d after kernel wdev->ps flipped on once, want 2", writes)
	}
}

func TestGuardRoutesReappliesARPAfterInterfaceReset(t *testing.T) {
	dir := t.TempDir()
	conf, table := filepath.Join(dir, "conf"), filepath.Join(dir, "table")
	settings := []string{"eth0/arp_announce", "wlan0/arp_announce", "eth0/arp_ignore", "wlan0/arp_ignore"}
	writeStock := func(dev string) {
		os.MkdirAll(filepath.Join(conf, dev), 0o755)
		for _, name := range []string{"arp_announce", "arp_ignore"} {
			os.WriteFile(filepath.Join(conf, dev, name), []byte("0\n"), 0o644)
		}
	}
	now := func() string {
		var got []string
		for _, name := range settings {
			b, _ := os.ReadFile(filepath.Join(conf, name))
			got = append(got, strings.TrimSpace(string(b)))
		}
		return strings.Join(got, " ")
	}
	if err := os.WriteFile(filepath.Join(dir, "ip"), []byte("#!/bin/sh\nif [ \"$2\" = show ]; then cat '"+table+"'; fi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeStock("lo")
	writeStock("eth0")
	writeStock("wlan0")
	os.WriteFile(table, nil, 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); guardRoutes(ctx, conf, 5*time.Millisecond) }()
	t.Cleanup(func() { cancel(); <-done })

	within := func(what string, limit time.Duration) {
		t.Helper()
		for end := time.Now().Add(limit); now() != "2 2 1 1"; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%s: arp settings are %q, want \"2 2 1 1\"", what, now())
			}
		}
	}
	within("initial boot", 2*time.Second)

	// Case 1: fast in-place wlan0 reset within a single pass (defaults 0, 0 restored).
	writeStock("wlan0")
	within("after in-place wlan0 reset", 300*time.Millisecond)

	// Case 2: wlan0 removed during SDIO reset, then recreated with stock 0, 0.
	if err := os.RemoveAll(filepath.Join(conf, "wlan0")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	writeStock("wlan0")
	within("after remove-and-recreate wlan0 reset", 300*time.Millisecond)

	// Unrelated interface lo stays untouched.
	for _, name := range []string{"lo/arp_announce", "lo/arp_ignore"} {
		if b, _ := os.ReadFile(filepath.Join(conf, name)); strings.TrimSpace(string(b)) != "0" {
			t.Fatalf("%s changed to %q", name, b)
		}
	}
}
