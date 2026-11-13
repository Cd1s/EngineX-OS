package ext4

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FuzzHostileImage feeds damaged filesystems to everything that reads a
// backup. Any result is fine except a panic or a hang.
func FuzzHostileImage(f *testing.F) {
	dir := f.TempDir()
	p := filepath.Join(dir, "seed.img")
	img, err := Create(context.Background(), p, CreateOptions{Size: 8 << 20, Label: "data", Now: fixedNow})
	if err != nil {
		f.Fatal(err)
	}
	cs := bundleChangeSet("enginex", "5.3.0.5", "99-enginex.conf")
	cs.Files = cs.Files[2:] // keep it small enough for 8 MiB
	if err := img.Apply(context.Background(), cs); err != nil {
		f.Fatal(err)
	}
	img.Close()
	seed, _ := os.ReadFile(p)
	// Damage to an inode is resealed with a valid checksum, so the fuzzer
	// reaches the mapping code instead of stopping at the checksum. The
	// seeds clear EXTENTS_FL (indirect maps) on the root directory, the
	// journal and a file, and plant block pointers in their i_block.
	sf, err := openFS(bytesSource(seed), int64(len(seed)), false)
	if err != nil {
		f.Fatal(err)
	}
	itStart := int64(sf.inodeTableBlock(0)) * sf.bs
	itEnd := itStart + int64(sf.itableBlocks())*sf.bs
	inodeOff := func(ino uint32) uint32 { return uint32(itStart) + (ino-1)*uint32(sf.inodeSize) }
	reseal := func(data []byte, off int) {
		if int64(off) < itStart || int64(off) >= itEnd {
			return
		}
		idx := (int64(off) - itStart) / int64(sf.inodeSize)
		raw := data[itStart+idx*int64(sf.inodeSize):][:sf.inodeSize]
		in := &inode{num: uint32(idx) + 1, raw: raw}
		c := sf.inodeCsum(in)
		le.PutUint16(raw[inChecksumLo:], uint16(c))
		if in.hasExtra(inChecksumHi + 2) {
			le.PutUint16(raw[inChecksumHi:], uint16(c>>16))
		}
	}
	f.Add(uint32(0), byte(0), uint32(0), byte(0))
	f.Add(uint32(1024+0x58), byte(0x80), uint32(4096*2), byte(1))
	for _, ino := range []uint32{rootIno, journalIno, firstGoodIno + 1, firstGoodIno + 5} {
		f.Add(inodeOff(ino)+inFlags+2, byte(0x08), inodeOff(ino)+inBlock+14*4, byte(0x20))
		f.Add(inodeOff(ino)+inFlags+2, byte(0x08), inodeOff(ino)+inBlock+13*4+1, byte(0x01))
		f.Add(inodeOff(ino)+inFlags+2, byte(0x08), inodeOff(ino)+inSizeHi, byte(0x40))
	}
	f.Fuzz(func(t *testing.T, off1 uint32, x1 byte, off2 uint32, x2 byte) {
		data := append([]byte(nil), seed...)
		// Keep damage inside the metadata-heavy first megabyte.
		o1, o2 := int(off1)%(1<<20), int(off2)%(1<<20)
		data[o1] ^= x1
		data[o2] ^= x2
		reseal(data, o1)
		reseal(data, o2)
		src := bytesSource(data)
		if _, err := Probe(src); err != nil {
			return
		}
		fsys, err := openFS(src, src.Size(), false)
		if err != nil {
			return
		}
		ro := &Image{fs: fsys, ro: true}
		_ = ro.Check(context.Background())
		_ = ro.Verify(context.Background(), cs)
		_, _, _ = ro.ChooseRuntimeDir()
		// Mutation of a probed-good but damaged copy must not panic either.
		mem := &memDev{b: data}
		w, err := openFS(mem, int64(len(data)), true)
		if err != nil {
			return
		}
		wi := &Image{fs: w}
		_ = wi.Apply(context.Background(), ChangeSet{Files: cs.Files[:3], Remove: []string{"enginex/version.txt"}})
	})
}

type memDev struct{ b []byte }

func (m *memDev) ReadAt(p []byte, off int64) (int, error) {
	return bytesSource(m.b).ReadAt(p, off)
}

func (m *memDev) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.b)) {
		return 0, os.ErrInvalid
	}
	return copy(m.b[off:], p), nil
}
