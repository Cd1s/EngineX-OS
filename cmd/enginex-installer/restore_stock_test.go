package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/installer/usb"
)

func stockFixture(t *testing.T, prefix string, image []byte) {
	t.Helper()
	original := stockFirmware[prefix]
	stockFirmware[prefix] = struct{ URL, SHA256 string }{original.URL, hexSHA(image)}
	t.Cleanup(func() { stockFirmware[prefix] = original })
}

func TestRestoreStockImages(t *testing.T) {
	official := gen1Container(t, "SC6000M", []byte("official rootfs"))
	stockFixture(t, "SC6000M", official)
	stockFixture(t, "SC6000", official)
	for _, tc := range []struct {
		name  string
		image []byte
		board string
		want  string
	}{
		{"official arbitrary filename", official, "jp14", ""},
		{"renamed EngineX", gen1Container(t, "SC6000M", []byte("EngineX rootfs")), "jp14", "EngineX or modified images are refused"},
		{"wrong board", official, "jp13", "does not support this deck"},
		{"not AZ01", []byte("not a firmware image"), "jp14", "invalid or unrecognized image format"},
		{"truncated header", official[:16], "jp14", "corrupt container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "not AZ01" || tc.name == "truncated header" {
				stockFixture(t, "SC6000M", tc.image)
			}
			fb := newFastboot(t, tc.board, true)
			o := testOpts(t)
			o.RestoreStock, o.USB = true, fb.Bin
			o.Bundle = filepath.Join(t.TempDir(), "firmware.img")
			if err := os.WriteFile(o.Bundle, tc.image, 0o600); err != nil {
				t.Fatal(err)
			}
			o.defaults()
			_, err := tryUSB(ctxT(t), o, "")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(fb.Dir, "flashed-rootfs.img"))
				if err != nil || string(got) != "official rootfs" {
					t.Fatalf("flashed %q, %v", got, err)
				}
				if calls := fb.Calls(); calls[len(calls)-1] != "reboot" {
					t.Fatal(calls)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err=%v", err)
				}
				for _, call := range fb.Calls() {
					if strings.HasPrefix(call, "oem ") || strings.HasPrefix(call, "flash ") || call == "reboot" {
						t.Fatalf("write before validation: %s", call)
					}
				}
			}
		})
	}
}

func TestRestoreStockOfficialDownload(t *testing.T) {
	image := gen1Container(t, "SC6000M", []byte("official rootfs"))
	stockFixture(t, "SC6000M", image)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write(image) }))
	t.Cleanup(server.Close)
	firmware := stockFirmware["SC6000M"]
	firmware.URL = server.URL + "/SC6000M-5.3.0-Update.img"
	stockFirmware["SC6000M"] = firmware
	o := testOpts(t)
	o.RestoreStock = true
	o.defaults()
	path, err := gen1Image(ctxT(t), o, "SC6000M")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, image) || requests.Load() != 1 {
		t.Fatalf("download requests=%d, err=%v", requests.Load(), err)
	}
	o.Bundle = path
	if _, err := gen1Image(ctxT(t), o, "SC6000M"); err != nil || requests.Load() != 1 {
		t.Fatalf("explicit image downloaded again: %v", err)
	}
}

func TestRestoreStockCleanupCommand(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	for _, name := range []string{"systemctl", "rm", "sync"} {
		script := "#!/bin/sh\nprintf '%s' '" + name + "' >> \"$RESTORE_COMMAND_LOG\"\nfor arg in \"$@\"; do printf ' <%s>' \"$arg\" >> \"$RESTORE_COMMAND_LOG\"; done\nprintf '\\n' >> \"$RESTORE_COMMAND_LOG\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("/bin/sh", "-c", stockCleanupCmd())
	command.Env = append(os.Environ(), "PATH="+dir, "RESTORE_COMMAND_LOG="+log)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	got, err := os.ReadFile(log)
	want := "systemctl <stop> <engine.service>\nrm <-rf> <--> </data/AppDataUser/AIR Music Technology/Engine/.cache/qml>\nsync\nsystemctl <poweroff>\n"
	if err != nil || string(got) != want {
		t.Fatalf("cleanup commands %q, %v", got, err)
	}
}

func TestRestoreStockPreparesBeforeUSB(t *testing.T) {
	image := gen1Container(t, "SC6000M", []byte("official rootfs"))
	stockFixture(t, "SC6000M", image)
	o := testOpts(t)
	o.RestoreStock = true
	o.Bundle = filepath.Join(t.TempDir(), "official.img")
	if err := os.WriteFile(o.Bundle, image, 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := startSSHDeck(t, o.SSHPort, "inmusic,jp14\x00")
	oldReadTimeout := sshReadTimeout
	sshReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sshReadTimeout = oldReadTimeout })
	if err := os.WriteFile(filepath.Join(ssh.Shim, "systemctl"), []byte("#!/bin/sh\n[ \"$1\" != stop ] || sleep 0.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fb := newFastboot(t, "jp14", true)
	fb.Bin.enumerate = func() []usb.DeviceInfo {
		if !strings.Contains(strings.Join(ssh.Commands(), "\n"), "systemctl poweroff") {
			t.Fatal("USB reached before cache preparation")
		}
		return []usb.DeviceInfo{fb.Bin.info}
	}
	o.USB = fb.Bin
	o.defaults()
	if err := modeA(ctxT(t), o, deck{IP: "127.0.0.1", Name: "SC6000M", Board: "jp14", Known: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ssh.Commands(), "\n"), stockCleanupCmd()) || len(fb.Flashed()) != 1 {
		t.Fatalf("SSH=%v USB=%v", ssh.Commands(), fb.Calls())
	}
}

func TestRestoreStockUnreachableShowsRescue(t *testing.T) {
	o := testOpts(t)
	o.RestoreStock = true
	err := run(ctxT(t), o)
	if err == nil || !strings.Contains(err.Error(), "first flash EngineX") {
		t.Fatalf("err=%v", err)
	}
	if out := o.Out.(*bytes.Buffer).String(); !strings.Contains(out, "51337") || !strings.Contains(out, "11224") {
		t.Fatalf("missing LAN firewall guidance: %s", out)
	}
}

func TestRestoreStockRefusesADeckWithoutEngineX(t *testing.T) {
	o := testOpts(t)
	o.RestoreStock, o.Bundle = true, "must-not-open.img"
	ssh := startSSHDeck(t, o.SSHPort, "inmusic,jp14\x00")
	if err := os.Remove(filepath.Join(ssh.Shim, "rootfs-version.txt")); err != nil {
		t.Fatal(err)
	}
	err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"})
	if err == nil || !strings.Contains(err.Error(), "first flash EngineX") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(strings.Join(ssh.Commands(), "\n"), "systemctl stop") {
		t.Fatalf("stock deck was stopped: %v", ssh.Commands())
	}
}

func TestRestoreStockSecondGenUsesUninstall(t *testing.T) {
	bundle := buildBundle(t, "PRIME4PLUS", "5.3.0.5")
	o := testOpts(t)
	o.defaults()
	ssh := startSSHDeck(t, o.SSHPort, "inmusic,jc11s\x00")
	useRelease(o, releaseServer(t, bundle, "", nil))
	if err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	o.RestoreStock = true
	if err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(ssh.Commands(), "\n"), "uninstall.sh") {
		t.Fatal(ssh.Commands())
	}
}
