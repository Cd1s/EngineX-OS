package webui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Checklist row 373 (#326): Bluetooth stored off is off again right after the boot-time unblock.

func writeEngineConf(t *testing.T, root, board, content string) {
	t.Helper()
	dir := filepath.Join(root, board+".user.settings", "AIR Music Technology")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Engine.conf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func engineConfGlobIn(root string) string {
	return filepath.Join(root, "*.user.settings", "AIR Music Technology", "Engine.conf")
}

func TestBluetoothStoredOff(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string // board -> Engine.conf
		want  bool
	}{
		{"no Engine.conf", nil, false},
		{"stored off, as Engine writes it", map[string]string{"jp07": "[Appearance]\nEnabled=true\n\n[Bluetooth]\nAutoConnectDeviceIDs=@Invalid()\nEnabled=false\nPreferredServices=@Invalid()\n\n[Network]\nEnabled=true\n"}, true},
		{"stored on", map[string]string{"jp07": "[Bluetooth]\nAutoConnectDeviceIDs=@Invalid()\nEnabled=true\n"}, false},
		{"off belongs to another section", map[string]string{"jp07": "[Recording]\nEnabled=false\n\n[Bluetooth]\nEnabled=true\n"}, false},
		{"no Enabled line", map[string]string{"jp07": "[Bluetooth]\nAutoConnectDeviceIDs=@Invalid()\n"}, false},
		{"CRLF line ends", map[string]string{"jp07": "[Bluetooth]\r\nEnabled=false\r\n"}, true},
		{"two boards, one stored on", map[string]string{"jp07": "[Bluetooth]\nEnabled=false\n", "jp08": "[Bluetooth]\nEnabled=true\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for board, content := range tc.files {
				writeEngineConf(t, root, board, content)
			}
			if got := bluetoothStoredOff(engineConfGlobIn(root)); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBluetoothOffOnceUp(t *testing.T) {
	cases := []struct {
		name     string
		reads    []string // "err", "on" or "off"; the last one repeats
		failOffs int      // the first power-offs that are refused
		want     bool
		wantOffs int
	}{
		{"comes up after the unblock, then stays off", []string{"err", "off", "on", "off"}, 0, true, 1},
		{"already up", []string{"on", "off"}, 0, true, 1},
		{"comes back once", []string{"on", "off", "on", "off"}, 0, true, 2},
		{"the first power-off is refused", []string{"on", "on", "off"}, 1, true, 2},
		{"never comes up", []string{"off"}, 0, false, 0},
		{"bluetoothd never answers", []string{"err"}, 0, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			i, offs := 0, 0
			powered := func() (bool, error) {
				r := tc.reads[min(i, len(tc.reads)-1)]
				i++
				if r == "err" {
					return false, errors.New("org.bluez is not there yet")
				}
				return r == "on", nil
			}
			off := func() error {
				offs++
				if offs <= tc.failOffs {
					return errors.New("refused")
				}
				return nil
			}
			if got := bluetoothOffOnceUp(ctx, time.Millisecond, powered, off); got != tc.want || offs != tc.wantOffs {
				t.Fatalf("got %v with %d power-offs, want %v with %d", got, offs, tc.want, tc.wantOffs)
			}
		})
	}
}

// fakeBusctl puts a busctl stand-in first on PATH. It records its arguments, reports the
// adapter powered until the first set-property call, and off after it.
func fakeBusctl(t *testing.T) (record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + record + "'\n" +
		"case \"$2\" in\n" +
		"get-property) if [ -e '" + record + ".off' ]; then echo 'b false'; else echo 'b true'; fi ;;\n" +
		"set-property) : > '" + record + ".off' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "busctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func TestBluetoothOffAsStored(t *testing.T) {
	const storedOff, storedOn = "[Bluetooth]\nEnabled=false\n", "[Bluetooth]\nEnabled=true\n"
	const powerOff = "--system set-property org.bluez /org/bluez/hci0 org.bluez.Adapter1 Powered b false"
	run := func(adapter, glob, mark string) {
		bluetoothOffAsStored(context.Background(), adapter, glob, mark, time.Millisecond, 2*time.Second)
	}
	calls := func(record string) string {
		data, _ := os.ReadFile(record)
		return string(data)
	}

	t.Run("stored off: switched off once the adapter is up, once per boot", func(t *testing.T) {
		record, conf := fakeBusctl(t), t.TempDir()
		writeEngineConf(t, conf, "jp07", storedOff)
		adapter, mark := t.TempDir(), filepath.Join(t.TempDir(), "mark")
		run(adapter, engineConfGlobIn(conf), mark)
		if got := calls(record); strings.Count(got, powerOff) != 1 {
			t.Fatalf("want exactly one power-off, got:\n%s", got)
		}
		// stream-sync restarts in the same boot, and the user has turned Bluetooth on
		// again in Engine meanwhile: the adapter is left alone.
		os.Remove(record)
		os.Remove(record + ".off")
		run(adapter, engineConfGlobIn(conf), mark)
		if got := calls(record); got != "" {
			t.Fatalf("a restart in the same boot called busctl:\n%s", got)
		}
	})
	t.Run("stored on: busctl is never called, also not by a restart after the user switched it off", func(t *testing.T) {
		record, conf := fakeBusctl(t), t.TempDir()
		writeEngineConf(t, conf, "jp07", storedOn)
		adapter, mark := t.TempDir(), filepath.Join(t.TempDir(), "mark")
		run(adapter, engineConfGlobIn(conf), mark)
		writeEngineConf(t, conf, "jp07", storedOff)
		run(adapter, engineConfGlobIn(conf), mark)
		if got := calls(record); got != "" {
			t.Fatalf("busctl was called:\n%s", got)
		}
	})
	t.Run("no Engine.conf: busctl is never called", func(t *testing.T) {
		record := fakeBusctl(t)
		run(t.TempDir(), engineConfGlobIn(t.TempDir()), filepath.Join(t.TempDir(), "mark"))
		if got := calls(record); got != "" {
			t.Fatalf("busctl was called:\n%s", got)
		}
	})
	t.Run("no adapter: busctl is never called and the boot mark is not taken", func(t *testing.T) {
		record, conf := fakeBusctl(t), t.TempDir()
		writeEngineConf(t, conf, "jp07", storedOff)
		mark := filepath.Join(t.TempDir(), "mark")
		run(filepath.Join(t.TempDir(), "no-hci0"), engineConfGlobIn(conf), mark)
		if got := calls(record); got != "" {
			t.Fatalf("busctl was called:\n%s", got)
		}
		if _, err := os.Stat(mark); !os.IsNotExist(err) {
			t.Fatal("the boot mark was taken without an adapter")
		}
	})
	t.Run("stored off but the adapter never powers on: only reads, then gives up", func(t *testing.T) {
		record, conf := fakeBusctl(t), t.TempDir()
		writeEngineConf(t, conf, "jp07", storedOff)
		if err := os.WriteFile(record+".off", nil, 0o644); err != nil { // the stand-in reports "b false" from the start
			t.Fatal(err)
		}
		bluetoothOffAsStored(context.Background(), t.TempDir(), engineConfGlobIn(conf), filepath.Join(t.TempDir(), "mark"), 20*time.Millisecond, 200*time.Millisecond)
		if got := calls(record); strings.Contains(got, "set-property") || !strings.Contains(got, "get-property") {
			t.Fatalf("want only reads, got:\n%s", got)
		}
	})
}
