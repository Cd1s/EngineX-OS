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
	f.Add(uint32(0), byte(0), uint32(0), byte(0))
	f.Add(uint32(1024+0x58), byte(0x80), uint32(4096*2), byte(1))
	f.Fuzz(func(t *testing.T, off1 uint32, x1 byte, off2 uint32, x2 byte) {
		data := append([]byte(nil), seed...)
		// Keep damage inside the metadata-heavy first megabyte.
		data[int(off1)%(1<<20)] ^= x1
		data[int(off2)%(1<<20)] ^= x2
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
