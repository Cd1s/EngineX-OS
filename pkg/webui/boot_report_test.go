package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBootReportStockVersionMatchesHookBuilds(t *testing.T) {
	source, err := os.ReadFile("../../tools/ui-mod/engine_ui_hook.c")
	if err != nil {
		t.Fatal(err)
	}
	builds := regexp.MustCompile(`(?s)(?:\{\s*"|\.name\s*=\s*")([0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9]+)?)",\s*(?:\.id\s*=\s*)?"((?:\\x[0-9a-f]{2}){20})"`).FindAllSubmatch(source, -1)
	if len(builds) != len(bootStockBuilds) {
		t.Fatalf("stock version map must cover every hook build: %d vs %d", len(builds), len(bootStockBuilds))
	}
	for _, build := range builds {
		id, err := hex.DecodeString(strings.ReplaceAll(string(build[2]), `\x`, ""))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Split(string(build[1]), "-")[0]
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			note := make([]byte, 36)
			order.PutUint32(note, 4)
			order.PutUint32(note[4:], 20)
			order.PutUint32(note[8:], 3)
			copy(note[12:], "GNU\x00")
			copy(note[16:], id)
			if got := bootStockNoteVersion(note, order); got != want {
				t.Fatalf("%s %v: got %q want %q", build[1], order, got, want)
			}
			if got := bootStockNoteVersion(note[:35], order); got != "unknown" {
				t.Fatalf("truncated build ID accepted: %q", got)
			}
			note[12] = 'X'
			if got := bootStockNoteVersion(note, order); got != "unknown" {
				t.Fatal("non-GNU note accepted")
			}
			note[12] = 'G'
			note[16] ^= 0xff
			if got := bootStockNoteVersion(note, order); got != "unknown" {
				t.Fatal("unknown build accepted")
			}
		}
	}
}

func TestBootReportStockVersionNeverExecutesFile(t *testing.T) {
	dir := t.TempDir()
	path, marker := filepath.Join(dir, "Engine"), filepath.Join(dir, "executed")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := bootStockVersion(path); got != "unknown" {
		t.Fatal(got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("version collection executed the file")
	}
}

func reportTestInputs(t *testing.T) (bootReportInputs, *[]byte, *int) {
	t.Helper()
	table := new([]byte)
	calls := new(int)
	return bootReportInputs{
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			*calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("capture has no deadline")
			}
			if name == "journalctl" {
				if !slicesHave(args, "--boot") || !slicesHave(args, "--unit=engine.service") {
					t.Error("missing boot/unit restriction")
				}
				var out bytes.Buffer
				for _, entry := range []map[string]string{
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "splash waiting for scene"},
					{"_SYSTEMD_UNIT": "engine-guardian.service", "MESSAGE": "completed database pass"},
					{"_SYSTEMD_UNIT": "unrelated.service", "MESSAGE": "DO-NOT-EXPORT"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "Authorization:\nopaque-secret\nmore-secret"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "visit https://private.invalid/path at 192.0.2.8 or [2001:db8::42]"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "TEST-CODE"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "hostname: personal-unit"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "serial: private-serial"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "usb 1-1: SerialNumber: usb-device-identity"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "serial_number=another-device-identity"},
					{"_SYSTEMD_UNIT": "engine.service", "MESSAGE": "iSerial 3 third-device-identity"},
				} {
					if err := json.NewEncoder(&out).Encode(entry); err != nil {
						t.Fatal(err)
					}
				}
				return out.Bytes(), nil
			}
			if name != "dmesg" {
				t.Fatalf("unexpected command %s", name)
			}
			return []byte("[ 1.000] kernel notice\n[ 2.000] 192.0.2.100 2001:db8::99 device.private.invalid\n"), nil
		},
		mounts: func() ([]byte, error) { return *table, nil }, removable: func(source string) bool { return source == "/dev/sdb1" || source == "/dev/mmcblk1p1" },
		model: "PRIME4", engineX: "5.3.0.3", stock: "5.3.0", codes: []string{"TEST-CODE"}, now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 1, time.UTC) },
	}, table, calls
}
func slicesHave(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}
func reportMountLine(t *testing.T, path, source, options string) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	escaped := strings.NewReplacer(`\`, `\134`, " ", `\040`).Replace(path)
	return fmt.Sprintf("30 1 %d:%d / %s %s - vfat %s %s\n", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), escaped, options, source, options)
}
func reportNoLeaks(t *testing.T, body []byte) {
	t.Helper()
	for _, secret := range []string{"opaque-secret", "more-secret", "private.invalid", "192.0.2.", "2001:db8", "TEST-CODE", "personal-unit", "private-serial", "usb-device-identity", "another-device-identity", "third-device-identity", "DO-NOT-EXPORT"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("report leaked %s", secret)
		}
	}
}

func TestBootReportRealDeviceClassification(t *testing.T) {
	root := t.TempDir()
	sysClass := filepath.Join(root, "sys/class/block")
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(sysClass, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dev, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		disk, node, topology, removable, kind string
		want                                  bool
	}{
		{"sda", "sda1", "usb-sata-internal", "0", "", false},
		{"sdb", "sdb1", "usb-external", "1", "", true},
		{"sdd", "sdd1", "usb-external-fixed", "0", "", false},
		{"sdc", "sdc1", "ata-internal", "0", "", false},
		{"mmcblk0", "mmcblk0p1", "mmc-internal", "0", "MMC", false},
		{"mmcblk1", "mmcblk1p1", "mmc-card", "1", "SD", true},
	} {
		path := filepath.Join(root, "sys/devices", tc.topology, tc.disk)
		if err := os.MkdirAll(filepath.Join(path, "device"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(sysClass, tc.disk)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "removable"), []byte(tc.removable), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "device/type"), []byte(tc.kind), 0600); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(dev, tc.node)
		if err := os.WriteFile(source, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if got := removableBootDeviceAt(source, sysClass); got != tc.want {
			t.Errorf("%s classification=%v want %v", tc.topology, got, tc.want)
		}
	}
}

func TestBootReportUnsupportedRenameKeepsUserFiles(t *testing.T) {
	old := bootRenameNoReplace
	bootRenameNoReplace = func(int, string, int, string, uint) error { return unix.ENOSYS }
	defer func() { bootRenameNoReplace = old }()
	dir := t.TempDir()
	body := []byte("sanitized diagnostic fixture")
	if err := writeBootAtomic(dir, "report.txt", body, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "report.txt"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("fallback publication: %q %v", got, err)
	}
	if err := writeBootAtomic(dir, "report.txt", []byte("replacement"), false); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("existing user name was not protected: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "report.txt"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("fallback overwrote the existing file")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, ".enginex-boot-*")); len(files) != 0 {
		t.Fatal("temporary report leaked")
	}
}

func TestBootKernelRecordsDiscardSecretsAndLeadingFragments(t *testing.T) {
	body := bootKernelRecords("truncated-sensitive-value\n[ 1.000] usb SerialNumber: device-identity\ncontinuation-identity\n[ 2.000] safe kernel event\n", nil)
	for _, value := range []string{"truncated-sensitive-value", "device-identity", "continuation-identity"} {
		if strings.Contains(body, value) {
			t.Fatalf("kernel record leaked %s", value)
		}
	}
	if !strings.Contains(body, "safe kernel event") {
		t.Fatal("unrelated kernel event was lost")
	}
	if strings.Contains(bootKernelRecords("unknown-format opaque-value", nil), "opaque-value") {
		t.Fatal("unknown kernel format was exported")
	}
}

func TestBootReportPrivacyHotInsertionAndFallbackRetry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ui")
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	in, table, calls := reportTestInputs(t)
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	name, body, err := existingBootReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	reportNoLeaks(t, body)
	if !strings.Contains(string(body), "splash waiting for scene") || !strings.Contains(string(body), "completed database pass") || !strings.Contains(string(body), "EngineX: 5.3.0.3") {
		t.Fatalf("missing useful evidence: %s", body)
	}
	if *calls != 2 {
		t.Fatal(*calls)
	}
	// An unknown directory is never treated as a mounted drive.
	unknown := filepath.Join(media, "not-mounted")
	usb := filepath.Join(media, "USB with spaces")
	for _, p := range []string{unknown, usb} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	*table = []byte(reportMountLine(t, usb, "/dev/sdb1", "rw"))
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(filepath.Join(usb, name))
	if err != nil {
		t.Fatal(err)
	}
	reportNoLeaks(t, exported)
	if entries, err := os.ReadDir(unknown); err != nil || len(entries) != 0 {
		t.Fatal("wrote to unmounted directory")
	}
	// Repeated fallback invocations reuse the report and do not rewrite the USB.
	stamp := time.Unix(100, 0)
	if err := os.Chtimes(filepath.Join(usb, name), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(usb, name))
	if err != nil || !st.ModTime().Equal(stamp) {
		t.Fatal("export was rewritten")
	}
	sd := filepath.Join(media, "SD")
	if err := os.Mkdir(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	*table = append(*table, []byte(reportMountLine(t, sd, "/dev/mmcblk1p1", "rw"))...)
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(sd, name))
	if err != nil || !bytes.Equal(again, exported) || *calls != 2 {
		t.Fatal("hot insertion regenerated report")
	}
	// Loss of all drives removes the saved-to-drive indication.
	*table = nil
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	if bootMarker(dir, "boot-report-usb") {
		t.Fatal("stale drive availability")
	}
}

func TestBootReportNoOverwriteSymlinksAndMountFilters(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ui")
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	in, table, _ := reportTestInputs(t)
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	name, _, err := existingBootReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	usb := filepath.Join(media, "usb")
	if err := os.Mkdir(usb, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	putBootTestFile(t, outside, "user-file", "precious")
	if err := os.Symlink(filepath.Join(outside, "user-file"), filepath.Join(usb, name)); err != nil {
		t.Fatal(err)
	}
	*table = []byte(reportMountLine(t, usb, "/dev/sdb1", "rw"))
	if err := writeBootReport(dir, media, in); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "user-file")); err != nil || string(b) != "precious" {
		t.Fatal("overwrote user file")
	}
	if _, err := os.ReadFile(filepath.Join(usb, strings.TrimSuffix(name, ".txt")+"-1.txt")); err != nil {
		t.Fatal("missing collision-safe report")
	}
	// Symlink mount directories and symlink ancestors fail before writing.
	link := filepath.Join(media, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	*table = []byte(reportMountLine(t, link, "/dev/sdb1", "rw"))
	if err := writeBootReport(dir, media, in); err == nil {
		t.Fatal("accepted symlink mount")
	}
	nested := filepath.Join(link, "nested")
	if err := os.Mkdir(filepath.Join(outside, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	*table = []byte(reportMountLine(t, nested, "/dev/sdb1", "rw"))
	if err := writeBootReport(dir, media, in); err == nil {
		t.Fatal("accepted symlink ancestor")
	}
	for _, tc := range []struct{ name, source, options string }{
		{"readonly", "/dev/sdb1", "ro"}, {"emmc", "/dev/mmcblk0p2", "rw"}, {"internal SATA", "/dev/sda1", "rw"}, {"unknown device", "/dev/unknown", "rw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if mounts := bootReportMounts([]byte(reportMountLine(t, usb, tc.source, tc.options)), media, in.removable); len(mounts) != 0 {
				t.Fatal(mounts)
			}
		})
	}
	// A table record for another device cannot authorize writes to a plain dir.
	mount := bootReportMount{path: usb, major: 999, minor: 999}
	if err := exportBootReport(mount, name, []byte("report")); err == nil {
		t.Fatal("accepted stale mount")
	}
	if mounts := bootReportMounts([]byte(reportMountLine(t, outside, "/dev/sdb1", "rw")), media, in.removable); len(mounts) != 0 {
		t.Fatal("accepted outside media root")
	}
	if mounts := bootReportMounts([]byte(reportMountLine(t, usb, "/dev/sdb1", "rw")), usb, in.removable); len(mounts) != 0 {
		t.Fatal("accepted media root itself")
	}
	rootRecord := strings.Replace(reportMountLine(t, usb, "/dev/sdb1", "rw"), strings.NewReplacer(" ", `\040`).Replace(usb), "/", 1)
	if mounts := bootReportMounts([]byte(rootRecord+reportMountLine(t, usb, "/dev/sdb1", "rw")), media, in.removable); len(mounts) != 0 {
		t.Fatal("accepted root device")
	}
}

func TestBootReportInvalidMarkerCaptureFailureAndDownload(t *testing.T) {
	s, dir, _ := bootTestServer(t)
	in, _, calls := reportTestInputs(t)
	outside := filepath.Join(filepath.Dir(dir), "private.txt")
	putBootTestFile(t, filepath.Dir(dir), "private.txt", "private payload")
	putBootTestFile(t, dir, "boot-report-latest", "../private.txt\n")
	if _, _, err := existingBootReport(dir); err == nil {
		t.Fatal("accepted traversal marker")
	}
	in.run = func(context.Context, string, ...string) ([]byte, error) {
		*calls++
		return []byte("secret captured on failure"), errors.New("private error details")
	}
	if err := writeBootReport(dir, t.TempDir(), in); err != nil {
		t.Fatal(err)
	}
	name, body, err := existingBootReport(dir)
	if err != nil || strings.Contains(string(body), "secret captured") || *calls != 2 {
		t.Fatalf("%q %v", body, err)
	}
	// Reuse has no command dependency, even if collection was unavailable.
	if err := writeBootReport(dir, t.TempDir(), in); err != nil || *calls != 2 {
		t.Fatalf("%v calls=%d", err, *calls)
	}
	if w := bootRequest(s, "GET", "/api/boot-report", "", "192.0.2.9:7", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := bootRequest(s, "GET", "/api/boot-report", "", "192.0.2.9:7", "TEST-CODE")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), name) || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("download %d %s", w.Code, w.Body)
	}
	// A replaced report must not follow a symlink, nor read an oversized file.
	path := filepath.Join(filepath.Dir(dir), "logs", name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if w := bootRequest(s, "GET", "/api/boot-report", "", "192.0.2.9:7", "TEST-CODE"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	putBootTestFile(t, filepath.Dir(path), name, bootReportHeader+strings.Repeat("x", bootReportLimit))
	if _, _, err := existingBootReport(dir); err == nil {
		t.Fatal("accepted oversized report")
	}
}

func TestBootReportMultilinePrivacy(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"multiline credentials", "Authorization:\nopaque-secret\nmore-secret"},
		{"access code without label", "test code"},
		{"multiline access code", "test-\ncode"},
		{"IPv4 URL and IPv6", "https://private.invalid/x 192.0.2.8 [2001:db8::42]"},
		{"escaped credentials", `password=opaque-secret\nmore-secret`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeBootText(tc.text, []string{"TEST-CODE"})
			reportNoLeaks(t, []byte(got))
			if strings.Contains(bootCodeText(got), "TESTCODE") {
				t.Fatal("access code leaked")
			}
		})
	}
}

func TestBootReportKernelFilesystemIdentifiers(t *testing.T) {
	got := bootKernelRecords("[ 1.000] EXT4-fs: mounted filesystem 12345678-abcd-4567-89ab-123456789abc\n"+
		"[ 2.000] ntfs3: volume name\nPrivate music collection\n"+
		"[ 3.000] FAT-fs: label=PrivateUSB\n"+
		"[ 4.000] Engine startup ready\n", nil)
	for _, private := range []string{"12345678", "Private music", "PrivateUSB"} {
		if strings.Contains(got, private) {
			t.Fatalf("kernel report leaked %q: %s", private, got)
		}
	}
	if !strings.Contains(got, "Engine startup ready") {
		t.Fatal("unrelated kernel record was lost")
	}
}
