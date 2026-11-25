package datagrow

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The PRIME 4 G2 of #291: /data is mmcblk0p12 (179:12), 21812719 KiB, with a filesystem of
// 235431 KiB as statfs counts it (df); a mode-C install (#244) has a 64 MiB one there instead.
const (
	g2Sectors  = 21812719 * 2
	g2Blocks   = g2Sectors * 512 / 4096
	modeCAvail = 14336   // a 64 MiB fresh image after its journal and inode table, in 4 KiB blocks
	report291  = 58857   // 235431 KiB
	fullStatfs = 5333000 // a filesystem that fills p12: statfs leaves out ~2% metadata
)

type fake struct {
	t       *testing.T
	env     Env
	blocks  uint64
	resized []uint64
	err     error
}

func newFake(t *testing.T, compatible, mountinfo string, sectors int64, partition bool, blocks uint64) *fake {
	t.Helper()
	dir := t.TempDir()
	f := &fake{t: t, blocks: blocks}
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.env.Compatible = filepath.Join(dir, "compatible")
	if compatible != "" {
		write(f.env.Compatible, compatible)
	}
	f.env.MountInfo = filepath.Join(dir, "mountinfo")
	write(f.env.MountInfo, mountinfo)
	f.env.SysDevBlock = filepath.Join(dir, "dev-block")
	write(filepath.Join(f.env.SysDevBlock, "179:12", "size"), itoa(sectors)+"\n")
	if partition {
		write(filepath.Join(f.env.SysDevBlock, "179:12", "partition"), "12\n")
	}
	f.env.Statfs = func(string) (uint64, uint64, error) { return 4096, f.blocks, nil }
	f.env.Resize = func(_ string, to uint64) error {
		if f.err != nil {
			return f.err
		}
		f.resized = append(f.resized, to)
		f.blocks = to - to/50
		return nil
	}
	return f
}

func itoa(v int64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			return string(b[i:])
		}
	}
}

const g2Compatible = "inmusic,jp24\x00rockchip,rk3588\x00"

// mountinfo as AZ04 shows it: /data mounted from PARTUUID, /var/lib bound from /data/system/var-lib.
func g2MountInfo(fstype, opts, superOpts string) string {
	return strings.Join([]string{
		"22 1 254:0 / / ro,relatime shared:1 - ext4 /dev/dm-0 ro",
		"30 22 179:12 / /data " + opts + " shared:9 - " + fstype + " /dev/mmcblk0p12 " + superOpts,
		"31 22 179:12 /system/var-lib /var/lib rw,relatime shared:10 - ext4 /dev/mmcblk0p12 rw",
	}, "\n") + "\n"
}

func TestGrowsModeCDataToItsPartition(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks uint64
	}{{"mode C 64 MiB (#244)", modeCAvail}, {"230 MiB of #291", report291}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, g2Compatible, g2MountInfo("ext4", "rw,relatime", "rw"), g2Sectors, true, tc.blocks)
			r, err := Grow(f.env, "/data")
			if err != nil {
				t.Fatal(err)
			}
			if !r.Grown || len(f.resized) != 1 || f.resized[0] != g2Blocks {
				t.Fatalf("want one resize to %d blocks, got %v (result %+v)", g2Blocks, f.resized, r)
			}
			if r.FromBlocks != tc.blocks || r.ToBlocks <= r.FromBlocks || r.PartitionBytes != g2Sectors*512 {
				t.Fatalf("result %+v", r)
			}
			// The second boot finds the filesystem filling its partition and asks nothing.
			r, err = Grow(f.env, "/data")
			if err != nil || r.Grown || len(f.resized) != 1 || !strings.Contains(r.Skipped, "already uses its partition") {
				t.Fatalf("second run: %+v, %v, resizes %v", r, err, f.resized)
			}
		})
	}
}

func TestLeavesAloneWhatItMustNotTouch(t *testing.T) {
	rw := g2MountInfo("ext4", "rw,relatime", "rw")
	for _, tc := range []struct {
		name, compatible, mountinfo, dir, why string
		sectors                               int64
		partition                             bool
		blocks                                uint64
	}{
		{"full partition", g2Compatible, rw, "/data", "already uses its partition", g2Sectors, true, fullStatfs},
		{"build host", "linux,dummy-virt\x00", rw, "/data", "not a deck", g2Sectors, true, modeCAvail},
		{"no device tree", "", rw, "/data", "not a deck", g2Sectors, true, modeCAvail},
		{"not mounted", g2Compatible, rw, "/media/sda1", "not a mount point", g2Sectors, true, modeCAvail},
		{"vfat", g2Compatible, g2MountInfo("vfat", "rw,relatime", "rw"), "/data", "not ext4", g2Sectors, true, modeCAvail},
		{"read-only mount", g2Compatible, g2MountInfo("ext4", "ro,relatime", "rw"), "/data", "read-only", g2Sectors, true, modeCAvail},
		{"read-only after errors", g2Compatible, g2MountInfo("ext4", "rw,relatime", "ro,errors=remount-ro"), "/data", "read-only", g2Sectors, true, modeCAvail},
		{"bind mount", g2Compatible, rw, "/var/lib", "bind mount", g2Sectors, true, modeCAvail},
		{"whole disk", g2Compatible, rw, "/data", "not on a partition", g2Sectors, false, modeCAvail},
		// 56 MiB on a 100 MiB partition: less than 64 MiB to win; 859 MiB on 1 GiB: 164 MiB to
		// win, but not a quarter.
		{"small win", g2Compatible, rw, "/data", "already uses its partition", 100 << 11, true, modeCAvail},
		{"near full", g2Compatible, rw, "/data", "already uses its partition", 1 << 21, true, 220000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.compatible, tc.mountinfo, tc.sectors, tc.partition, tc.blocks)
			r, err := Grow(f.env, tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			if r.Grown || len(f.resized) != 0 || !strings.Contains(r.Skipped, tc.why) {
				t.Fatalf("want skipped for %q, got %+v, resizes %v", tc.why, r, f.resized)
			}
		})
	}
}

func TestTopmostMountWinsAndEscapesAreRead(t *testing.T) {
	mi := "30 22 179:12 / /data ro,relatime - ext4 /dev/mmcblk0p12 ro\n" +
		"41 30 179:12 / /data rw,relatime - ext4 /dev/mmcblk0p12 rw\n" +
		"42 22 179:12 / /my\\040data rw - ext4 /dev/mmcblk0p12 rw\n"
	for _, dir := range []string{"/data", "/my data"} {
		f := newFake(t, g2Compatible, mi, g2Sectors, true, modeCAvail)
		if r, err := Grow(f.env, dir); err != nil || !r.Grown {
			t.Fatalf("%s: %+v, %v", dir, r, err)
		}
	}
}

func TestKernelRefusalIsReported(t *testing.T) {
	f := newFake(t, g2Compatible, g2MountInfo("ext4", "rw", "rw"), g2Sectors, true, modeCAvail)
	f.err = errors.New("operation not permitted")
	r, err := Grow(f.env, "/data")
	if err == nil || r.Grown || !strings.Contains(err.Error(), "grow /data") {
		t.Fatalf("want the refusal, got %+v, %v", r, err)
	}
}

func TestMainUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main([]string{"/data", "/x"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}
