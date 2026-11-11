package webui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type bootReportMount struct {
	path         string
	major, minor uint32
}

var bootDiskPartition = regexp.MustCompile(`^(sd[a-z]+)[0-9]*$`)
var bootSDPartition = regexp.MustCompile(`^(mmcblk[0-9]+)(?:p[0-9]+)?$`)

func removableBootDevice(source string) bool {
	return removableBootDeviceAt(source, "/sys/class/block")
}

func removableBootDeviceAt(source, sysClass string) bool {
	// Unknown devices fail closed. Internal SATA and eMMC are never candidates.
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return false
	}
	base := filepath.Base(source)
	disk := ""
	if m := bootDiskPartition.FindStringSubmatch(base); m != nil {
		disk = m[1]
	}
	if m := bootSDPartition.FindStringSubmatch(base); m != nil {
		b, err := os.ReadFile(filepath.Join(sysClass, m[1], "device/type"))
		return err == nil && strings.TrimSpace(string(b)) == "SD"
	}
	if disk == "" {
		return false
	}
	device, err := filepath.EvalSymlinks(filepath.Join(sysClass, disk))
	if err != nil {
		return false
	}
	// Some decks connect their internal SATA bay through a USB bridge.
	// USB topology alone cannot identify an external report destination.
	b, err := os.ReadFile(filepath.Join(sysClass, disk, "removable"))
	return strings.Contains(device, "/usb") && err == nil && strings.TrimSpace(string(b)) == "1"
}

func bootReportMounts(table []byte, root string, removable func(string) bool) []bootReportMount {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var mounts []bootReportMount
	rootDevice := ""
	records := strings.Split(string(table), "\n")
	for _, record := range records {
		fields := strings.Fields(record)
		if len(fields) > 5 && fields[4] == "/" {
			rootDevice = fields[2]
		}
	}
	seen := map[string]bool{}
	for _, record := range records {
		parts := strings.SplitN(record, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		f, tail := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(f) < 6 || len(tail) < 3 || f[2] == rootDevice || f[3] != "/" {
			continue
		} // no bind/subvolume exports
		path := unescape.Replace(f[4])
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsAbs(path) || strings.ContainsAny(path, "\n\x00") {
			continue
		}
		if !mountOption(f[5], "rw") || mountOption(f[5], "ro") || mountOption(tail[2], "ro") || !removable(unescape.Replace(tail[1])) || seen[path] {
			continue
		}
		dev := strings.Split(f[2], ":")
		if len(dev) != 2 {
			continue
		}
		major, e1 := strconv.ParseUint(dev[0], 10, 32)
		minor, e2 := strconv.ParseUint(dev[1], 10, 32)
		if e1 != nil || e2 != nil {
			continue
		}
		seen[path] = true
		mounts = append(mounts, bootReportMount{path, uint32(major), uint32(minor)})
	}
	return mounts
}
func mountOption(options, option string) bool {
	for _, v := range strings.Split(options, ",") {
		if v == option {
			return true
		}
	}
	return false
}

func exportBootReport(mount bootReportMount, name string, body []byte) error {
	fd, err := openBootDir(mount.path)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if unix.Major(uint64(st.Dev)) != mount.major || unix.Minor(uint64(st.Dev)) != mount.minor {
		return errors.New("mount changed")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return err
	}
	if fs.Flags&unix.ST_RDONLY != 0 {
		return errors.New("read-only mount")
	}
	// Existing user files are never replaced. A matching report is reused;
	// collisions receive a bounded alternate basename, with no symlink following.
	for n := 0; n < 100; n++ {
		candidate := name
		if n > 0 {
			candidate = fmt.Sprintf("%s-%d.txt", strings.TrimSuffix(name, ".txt"), n)
		}
		b, err := readBootFileAt(fd, candidate, bootReportLimit)
		if err == nil {
			if bytes.Equal(b, body) {
				return nil
			}
			continue
		}
		var existing unix.Stat_t
		if err := unix.Fstatat(fd, candidate, &existing, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			continue // includes symlinks, directories and oversized user files
		} else if !errors.Is(err, unix.ENOENT) {
			return err
		}
		if err := writeBootAtomicAtMode(fd, candidate, body, false, 0o644); err == nil {
			return nil
		} else if !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return errors.New("report filename collisions")
}
