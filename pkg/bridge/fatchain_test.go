package bridge

import (
	"encoding/binary"
	"testing"
)

// fatVolume is a FAT volume's boot sector and first FAT, enough for fatChainOwned.
type fatVolume struct {
	boot  [512]byte
	fat   map[int64]uint32
	width int64
	rsvd  int64
}

func (v *fatVolume) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		return copy(p, v.boot[:]), nil
	}
	c := (off - v.rsvd*512) / v.width
	var e [4]byte
	binary.LittleEndian.PutUint32(e[:], v.fat[c])
	return copy(p, e[:v.width]), nil
}

// newFATVolume: 512-byte sectors, spc sectors a cluster; fat32 as mkfs -F 32 writes it
// (FATSz16 zero), else FAT16.
func newFATVolume(fat32 bool, spc byte, totalSectors uint32) *fatVolume {
	v := &fatVolume{fat: map[int64]uint32{}, width: 2, rsvd: 4}
	le := binary.LittleEndian
	le.PutUint16(v.boot[11:], 512)
	v.boot[13] = spc
	le.PutUint16(v.boot[14:], uint16(v.rsvd))
	v.boot[16] = 2
	le.PutUint32(v.boot[32:], totalSectors)
	if fat32 {
		v.width, v.rsvd = 4, 32
		le.PutUint16(v.boot[14:], uint16(v.rsvd))
		le.PutUint32(v.boot[36:], 256)
	} else {
		le.PutUint16(v.boot[17:], 512) // root entries
		le.PutUint16(v.boot[22:], 256)
	}
	v.boot[510], v.boot[511] = 0x55, 0xaa
	return v
}

// firstSector is the volume sector of cluster c.
func (v *fatVolume) firstSector(c int64) int64 {
	data := v.rsvd + 2*256
	if v.width == 2 {
		data += 512 * 32 / 512
	}
	return data + (c-2)*int64(v.boot[13])
}

// #321: a leftover or a replaced file is freed only when the FAT gives it all its clusters;
// vfat calls freeing a free cluster corruption and remounts the stick read-only.
func TestIssue321FATChainOwned(t *testing.T) {
	t.Parallel()
	const eoc32, eoc16 = 0x0fffffff, 0xffff
	cases := []struct {
		name  string
		fat32 bool
		spc   byte
		total uint32
		chain map[int64]uint32
		start int64
		size  int64
		owned bool
	}{
		{"FAT32, three clusters", true, 8, 1 << 20, map[int64]uint32{15: 16, 16: 40, 40: eoc32}, 15, 3*4096 - 100, true},
		// mkfs -F 32 on a small volume: FAT32 with fewer than 65525 clusters (Linux goes by the
		// BPB, not the count). Read as FAT16 it looked unowned: a healthy stick would not be written.
		{"FAT32 with few clusters", true, 64, 1 << 20, map[int64]uint32{15: 16, 16: 17, 17: eoc32}, 15, 86386, true},
		{"a free cluster in the chain", true, 8, 1 << 20, map[int64]uint32{15: 16, 16: 0}, 15, 3 * 4096, false},
		{"its first cluster free (an entry ahead of its chain)", true, 64, 1 << 20, map[int64]uint32{}, 15, 5000, false},
		{"a chain longer than the file", true, 8, 1 << 20, map[int64]uint32{15: 16, 16: eoc32}, 15, 100, false},
		{"a chain shorter than the file", true, 8, 1 << 20, map[int64]uint32{15: eoc32}, 15, 5000, false},
		{"FAT16, two clusters", false, 64, 1 << 20, map[int64]uint32{15: 16, 16: eoc16}, 15, 40000, true},
		{"FAT16, freed", false, 64, 1 << 20, map[int64]uint32{}, 15, 40000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newFATVolume(c.fat32, c.spc, c.total)
			for k, e := range c.chain {
				v.fat[k] = e
			}
			owned, ok := fatChainOwned(v, v.firstSector(c.start), c.size)
			if !ok || owned != c.owned {
				t.Fatalf("owned=%v ok=%v, want owned=%v", owned, ok, c.owned)
			}
		})
	}
	// FAT12 is not read: the caller frees as before.
	if _, ok := fatChainOwned(newFATVolume(false, 64, 8000), 600, 100); ok {
		t.Fatal("FAT12 read")
	}
}
