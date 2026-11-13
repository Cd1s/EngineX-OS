package dbhealer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, path, text string) {
	t.Helper()
	dest := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, root, path string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(blob)
}
func TestDebloatScriptCompleteOfflineAndIdempotent(t *testing.T) {
	script, err := filepath.Abs("../../rootfs-overlay/usr/local/bin/enginex-debloat")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "offline image with spaces")
	os.MkdirAll(root, 0755)
	put(t, root, "usr/Engine/Scripts/engine", "#!/bin/sh\n/usr/bin/az01-ethernet-speed 100\n/usr/bin/az01-ethernet-speed 1000\n")
	put(t, root, "usr/Engine/Scripts/setup-prerequisites.sh", `#!/bin/sh
/usr/Engine/Scripts/set-irq-affinity.sh ffa30000 1
/usr/Engine/Scripts/set-irq-affinity.sh ffb20000 3
/usr/Engine/Scripts/set-irq-affinity.sh eth0 1
/usr/Engine/Scripts/set-irq-affinity.sh ehci_hcd 1
/usr/Engine/Scripts/set-irq-affinity.sh ff540000 1
`)
	put(t, root, "usr/Engine/Scripts/cleanup-JP07.sh", "#!/bin/sh\nexit 0\n")
	put(t, root, "usr/Engine/Scripts/setup-usb-gadget.sh", "#!/bin/sh\nshift1\n")
	put(t, root, "usr/Engine/Scripts/encrypt-fs.sh", "#!/bin/sh\nif false; then\n/usr/bin/az01-update --erase-data\nfi\n")
	put(t, root, "etc/hosts", "127.0.0.1 localhost\n192.0.2.1 user-host\n")
	put(t, root, "data/downloads/keep.mp3", "do not remove")
	run := func() {
		t.Helper()
		cmd := exec.Command("sh", script, "--root", root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run()
	engine := read(t, root, "usr/Engine/Scripts/engine")
	if strings.Contains(engine, "speed 100\n") || strings.Count(engine, "speed 1000") != 2 {
		t.Fatal(engine)
	}
	prereq := read(t, root, "usr/Engine/Scripts/setup-prerequisites.sh")
	for _, line := range []string{"ffa30000 1", "ffb20000 0", "eth0 2", "ehci_hcd 2", "ff540000 2"} {
		if !strings.Contains(prereq, line) {
			t.Fatal(prereq)
		}
	}
	target, err := os.Readlink(filepath.Join(root, "usr/Engine/Scripts/cleanup"))
	if err != nil || target != "cleanup-JP07.sh" {
		t.Fatalf("cleanup: %q %v", target, err)
	}
	conf := read(t, root, "etc/systemd/system/engine.service.d/override.conf")
	for _, line := range []string{"LimitCORE=0", "ExecStopPost=\n", "exec /usr/Engine/Scripts/cleanup-JP07.sh'", "ExecStopPost=-/etc/enginex/engine-ui-guard stop", "Environment=HOME=/tmp/sc5000-engine-home", "Environment=QML_DISABLE_DISK_CACHE=1", "ExecStartPre=/etc/enginex/engine-ui-guard runtime"} {
		if !strings.Contains(conf, line) {
			t.Fatal(conf)
		}
	}
	if !strings.Contains(read(t, root, "etc/systemd/journald.conf.d/enginex.conf"), "RuntimeMaxUse=16M") {
		t.Fatal("journal unlimited")
	}
	hosts := read(t, root, "etc/hosts")
	for _, name := range []string{"localhost", "user-host", "inmusicbrands.com", "logstash.inmusicbrands.com", "posthog.com", "us.i.posthog.com"} {
		if !strings.Contains(hosts, name) {
			t.Fatal(hosts)
		}
	}
	if strings.Contains(hosts, "0.0.0.0 inmusicbrands.com\n") || strings.Contains(read(t, root, "etc/dnsmasq.d/enginex-telemetry.conf"), "/inmusicbrands.com/") {
		t.Fatal("connman online check domain blocked")
	}
	// Engine shows a Wi-Fi "!" unless connman's stock online check reaches ONLINE.
	if conf := read(t, root, "etc/connman/main.conf"); !strings.Contains(conf, "EnableOnlineCheck = true") || !strings.Contains(conf, "OnlineCheckIPv4URL = http://ipv4.connman.inmusicbrands.com/") {
		t.Fatal("connman online check disabled")
	}
	// hfsplus.ko oopses on write; HFS+ must never be force-mounted read-write.
	if strings.Contains(read(t, root, "etc/edisksd/main.conf"), "[hfsplus]") {
		t.Fatal("HFS+ force mount retained")
	}
	if strings.Contains(read(t, root, "usr/Engine/Scripts/encrypt-fs.sh"), "az01-update --erase-data") {
		t.Fatal("dangerous recovery retained")
	}
	if read(t, root, "data/downloads/keep.mp3") != "do not remove" {
		t.Fatal("downloads deleted")
	}
	snapshot := map[string][]byte{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e == nil && d.Type().IsRegular() {
			snapshot[path], _ = os.ReadFile(path)
		}
		return e
	})
	run()
	for path, before := range snapshot {
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatalf("not idempotent: %s", path)
		}
	}
	for _, name := range []string{"engine", "setup-prerequisites.sh", "setup-usb-gadget.sh", "encrypt-fs.sh"} {
		if out, err := exec.Command("sh", "-n", filepath.Join(root, "usr/Engine/Scripts", name)).CombinedOutput(); err != nil {
			t.Fatalf("%s: %s %v", name, out, err)
		}
	}
	if err := exec.Command("sh", script, "--root", root, "--apply-runtime").Run(); err == nil {
		t.Fatal("offline staging permitted runtime changes")
	}
}
func TestOverlayUnitsAndRuntimePolicy(t *testing.T) {
	root := "../../rootfs-overlay"
	script := read(t, root, "usr/local/bin/enginex-debloat")
	for _, text := range []string{"iw dev wlan0 set power_save off", "az01-ethernet-speed 1000 || az01-ethernet-speed 100", "chrt -f -p 99", "ffb20000:0 ffa30000:1 ehci_hcd:2 ff540000:2 eth0:2"} {
		if !strings.Contains(script, text) {
			t.Fatal(text)
		}
	}
	unit := read(t, root, "etc/systemd/system/enginex-debloat.service")
	if !strings.Contains(unit, "After=engine.service") {
		t.Fatal("vendor affinity would win")
	}
	guardian := read(t, root, "etc/systemd/system/engine-guardian.service")
	// The retired database watch is now the boot watchdog. It must get a fair
	// CPU slice even while ordinary background work saturates the service core.
	for _, text := range []string{"CPUAffinity=3", "CPUSchedulingPolicy=other", "Nice=10", "IOSchedulingClass=best-effort", "LimitCORE=0", "ExecStart=/etc/enginex/engine-ui-guard watch"} {
		if !strings.Contains(guardian, text) {
			t.Fatal(text)
		}
	}
	target, err := os.Readlink(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/enginex-debloat.service"))
	if err != nil || target != "../enginex-debloat.service" {
		t.Fatalf("enginex-debloat %q %v", target, err)
	}
	// Database repair must still never run alongside Engine. Only the shell
	// boot watchdog is enabled, and its reporting branch has no heal operation.
	target, err = os.Readlink(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/engine-guardian.service"))
	if err != nil || target != "../engine-guardian.service" || strings.Contains(guardian, "--heal-mount") || strings.Contains(guardian, "--watch") {
		t.Fatalf("boot guardian must not enable database repair: %q %v", target, err)
	}
	if out, err := exec.Command("bash", "-n", filepath.Join(root, "usr/local/bin/enginex-debloat")).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
}

func TestDebloatRuntimeIRQWiFiAndEthernetFallback(t *testing.T) {
	// Execute the production runtime block with /proc redirected into a fixture
	// and hardware commands replaced by logging stubs. Never touch host IRQs.
	script := read(t, "../../rootfs-overlay", "usr/local/bin/enginex-debloat")
	start := strings.LastIndex(script, "if [ \"$runtime\" = 1 ]; then")
	if start < 0 {
		t.Fatal("runtime block missing")
	}
	root := t.TempDir()
	block := strings.ReplaceAll(script[start:], "/proc/", filepath.Join(root, "proc")+"/")
	put(t, root, "runtime.sh", "#!/bin/sh\nset -eu\nruntime=1\n"+block)
	put(t, root, "proc/interrupts", ` 32: 1 0 0 0 GIC ffb20000
 33: 0 1 0 0 GIC ffa30000
 34: 0 1 0 0 GIC ehci_hcd
 35: 0 1 0 0 GIC ff540000
 36: 0 1 0 0 GIC eth0
`)
	for i, irq := range []string{"32", "33", "34", "35", "36"} {
		leaf := "smp_affinity_list"
		if irq == "36" {
			leaf = "smp_affinity"
		}
		put(t, root, "proc/irq/"+irq+"/"+leaf, "old\n")
		put(t, root, fmt.Sprintf("proc/%d/comm", 201+i), "irq/"+irq+"-fixture\n")
	}
	for _, name := range []string{"az01-ethernet-speed", "iw", "taskset", "chrt"} {
		put(t, root, "mock-bin/"+name, `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >> "$COMMAND_LOG"
case "${0##*/}:$*" in
az01-ethernet-speed:1000|iw:*) exit 1;;
esac
`)
		if err := os.Chmod(filepath.Join(root, "mock-bin", name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", filepath.Join(root, "runtime.sh"))
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "mock-bin")+":/usr/bin:/bin", "COMMAND_LOG="+filepath.Join(root, "calls.log"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, spec := range []struct{ irq, leaf, want string }{{"32", "smp_affinity_list", "0\n"}, {"33", "smp_affinity_list", "1\n"}, {"34", "smp_affinity_list", "2\n"}, {"35", "smp_affinity_list", "2\n"}, {"36", "smp_affinity", "4\n"}} {
		if got := read(t, root, "proc/irq/"+spec.irq+"/"+spec.leaf); got != spec.want {
			t.Fatalf("IRQ %s: %q", spec.irq, got)
		}
	}
	calls := read(t, root, "calls.log")
	for _, want := range []string{"az01-ethernet-speed 1000\naz01-ethernet-speed 100\n", "iw dev wlan0 set power_save off", "taskset -pc 0 201", "taskset -pc 1 202", "taskset -pc 2 203", "taskset -pc 2 204", "taskset -pc 2 205", "chrt -f -p 99 201"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %q in %s", want, calls)
		}
	}
	if strings.Count(calls, "chrt ") != 1 {
		t.Fatal("non-audio IRQ got realtime priority")
	}
}
