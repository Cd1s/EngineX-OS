package ext4

// Regression tests for the review of a2f488d, built on the reviewer's
// reproductions. Each fails on that commit.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fsckIfInstalled requires e2fsck -fn exit 0 when e2fsprogs is present.
func fsckIfInstalled(t *testing.T, p string) {
	t.Helper()
	if _, err := exec.LookPath("e2fsck"); err != nil {
		return
	}
	if out, err := exec.Command("e2fsck", "-fn", p).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck -fn: %v\n%s", err, out)
	}
}

func r3ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func r3setMap(t *testing.T, f *fsys, in *inode, exts []extent, size uint64) {
	t.Helper()
	meta, err := f.writeExtentTree(in, exts, 0)
	r3ok(t, err)
	blocks := uint64(len(meta))
	for _, e := range exts {
		blocks += uint64(e.length)
	}
	in.setSize(size)
	r3ok(t, in.setBlocks512(f, blocks*uint64(f.bs/512)))
	r3ok(t, f.writeInode(in))
}

// r3fillDir fills each directory block with fifteen 255-byte names; each
// block keeps less than 264 bytes of slack, so one more such name needs a
// new block.
func r3fillDir(t *testing.T, f *fsys, dir, target *inode, exts []extent) {
	t.Helper()
	entries := 0
	for b, e := range exts {
		data := make([]byte, f.bs)
		off := 0
		if b == 0 {
			data = f.newDirBlock(dir, rootIno)
			f.putRecLen(data[16:], 12)
			off = 24
		}
		for j := 0; j < 15; j++ {
			name := fmt.Sprintf("%08d", entries) + strings.Repeat("n", 247)
			rl := direntLen(len(name))
			if j == 14 {
				rl = f.dirEnd() - off
			}
			le.PutUint32(data[off:], target.num)
			f.putRecLen(data[off+4:], rl)
			data[off+6], data[off+7] = byte(len(name)), ftReg
			copy(data[off+8:], name)
			off += rl
			entries++
		}
		f.sealDirBlock(dir, data)
		r3ok(t, f.writeBlock(e.phys, data))
	}
	target.setLinks(uint16(entries + 1))
	r3ok(t, f.writeInode(target))
}

// A directory of 1360 single-block extents (a full depth-1 tree): one more
// block needs a depth-2 tree, 3 blocks net. With 2 free it must be refused
// before any write; with 3 free it must succeed.
func TestReview3DirectoryPeak(t *testing.T) {
	for _, free := range []uint64{2, 3} {
		t.Run(fmt.Sprintf("free=%d", free), func(t *testing.T) {
			img, p := newImage(t, 64<<20, "data")
			f := img.fs
			r3ok(t, img.Apply(context.Background(), ChangeSet{Directories: []string{"crowded"},
				Files: []Entry{memEntry("sentinel", 0o644, nil), memEntry("spacer", 0o644, nil), memEntry("fill", 0o644, nil)}}))
			dir, err := f.lstat("crowded")
			r3ok(t, err)
			old, _, err := f.fileMap(dir)
			r3ok(t, err)
			for _, e := range old {
				r3ok(t, f.freeBlocks(e.phys, uint64(e.length)))
			}
			const n = 1360
			blocks, err := f.allocMeta(0, n*2)
			r3ok(t, err)
			dirs, spaces := make([]extent, n), make([]extent, n)
			for i := 0; i < n; i++ {
				dirs[i] = extent{logical: uint32(i), phys: blocks[2*i], length: 1}
				spaces[i] = extent{logical: uint32(i), phys: blocks[2*i+1], length: 1}
			}
			r3setMap(t, f, dir, dirs, n*uint64(f.bs))
			spacer, _ := f.lstat("spacer")
			r3setMap(t, f, spacer, spaces, n*uint64(f.bs))
			sentinel, _ := f.lstat("sentinel")
			r3fillDir(t, f, dir, sentinel, dirs)
			fill, _ := f.lstat("fill")
			padding, err := f.allocData(0, f.sbFreeBlocks()-free, 0)
			r3ok(t, err)
			var used uint64
			for _, e := range padding {
				used += uint64(e.length)
			}
			r3setMap(t, f, fill, padding, used*uint64(f.bs))
			if f.sbFreeBlocks() != free {
				t.Fatalf("fixture has %d free blocks, want %d", f.sbFreeBlocks(), free)
			}
			r3ok(t, f.flush())
			mustCheck(t, img)
			fsckIfInstalled(t, p)
			before := fileHash(t, p)
			err = img.Apply(context.Background(), ChangeSet{Files: []Entry{memEntry("crowded/"+strings.Repeat("z", 255), 0o644, nil)}})
			r3ok(t, img.Close())
			if free == 2 {
				if !errors.Is(err, ErrNoSpace) || fileHash(t, p) != before {
					t.Fatalf("Apply = %v, changed = %v; want ErrNoSpace and no change", err, fileHash(t, p) != before)
				}
				return
			}
			r3ok(t, err)
			fsckIfInstalled(t, p)
			ro, err := Open(p)
			r3ok(t, err)
			defer ro.Close()
			mustCheck(t, ro)
		})
	}
}

type r3countReader struct {
	io.ReaderAt
	counts map[int64]int
}

func (r *r3countReader) ReadAt(p []byte, off int64) (int, error) {
	r.counts[off]++
	return r.ReaderAt.ReadAt(p, off)
}

// A damaged tree shared by 40 inodes is walked once: the first failed
// mapping stops the check. Empty non-root leaves are refused outright.
func TestReview3SharedFailedMapping(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	defer img.Close()
	f := img.fs
	// Without metadata_csum the same tree can hang off different inodes.
	le.PutUint32(f.sb[sbFeatureROCompat:], f.roCompat()&^roCompatMetadataCsum)
	const victims, leaves = 40, 256
	var cs ChangeSet
	for i := 0; i < victims; i++ {
		cs.Files = append(cs.Files, memEntry(fmt.Sprintf("v%02d", i), 0o644, nil))
	}
	r3ok(t, img.Apply(context.Background(), cs))
	blocks, err := f.allocMeta(0, leaves+1)
	r3ok(t, err)
	header := func(entries, depth int) []byte {
		b := make([]byte, f.bs)
		le.PutUint16(b, extentMagic)
		le.PutUint16(b[2:], uint16(entries))
		le.PutUint16(b[4:], uint16((f.bs-12)/12))
		le.PutUint16(b[6:], uint16(depth))
		return b
	}
	index := header(leaves, 1)
	for i := 0; i < leaves; i++ {
		le.PutUint32(index[12+12*i:], uint32(i*340))
		le.PutUint32(index[16+12*i:], uint32(blocks[i+1]))
		leaf := header(0, 0)
		if i == leaves-1 {
			le.PutUint16(leaf, 0) // bad magic, reached only after every other leaf
		}
		r3ok(t, f.writeBlock(blocks[i+1], leaf))
	}
	r3ok(t, f.writeBlock(blocks[0], index))
	for i := 0; i < victims; i++ {
		in, err := f.lstat(fmt.Sprintf("v%02d", i))
		r3ok(t, err)
		ib := in.iblock()
		clear(ib)
		le.PutUint16(ib, extentMagic)
		le.PutUint16(ib[2:], 1)
		le.PutUint16(ib[4:], 4)
		le.PutUint16(ib[6:], 2)
		le.PutUint32(ib[16:], uint32(blocks[0]))
		r3ok(t, f.writeInode(in))
	}
	r3ok(t, f.flush())
	r := &r3countReader{ReaderAt: f.dev, counts: map[int64]int{}}
	f.dev = r
	c := &checker{f: f}
	r3ok(t, c.run(context.Background()))
	if n := r.counts[int64(blocks[0])*f.bs]; n > 1 || len(c.problems) == 0 {
		t.Fatalf("shared index read %d times, %d problems; want one read and a failed check", n, len(c.problems))
	}
}

func TestReview3JournalCorruptionRefusedBeforeOutput(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	j, err := img.fs.readInode(journalIno)
	r3ok(t, err)
	le.PutUint16(j.iblock()[16:], 0) // zero-length first extent, inode resealed
	r3ok(t, img.fs.writeInode(j))
	r3ok(t, img.Close())
	src, closer, err := OpenFileSource(p)
	r3ok(t, err)
	defer closer.Close()
	if _, err := Probe(src); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Probe = %v, want ErrCorrupt", err)
	}
	out := filepath.Join(t.TempDir(), "copy.img")
	if _, err := OpenCopy(context.Background(), src, out); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenCopy = %v, want ErrCorrupt", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("an output copy exists for a backup with a corrupt journal")
	}
}

// A full block-mapped directory cannot grow: refused before any write. A
// name that fits its slack is still installed.
func TestReview3BlockMappedDirectory(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	f := img.fs
	r3ok(t, img.Apply(context.Background(), ChangeSet{Directories: []string{"d"}, Files: []Entry{memEntry("sentinel", 0o644, nil)}}))
	dir, err := f.lstat("d")
	r3ok(t, err)
	exts, _, err := f.fileMap(dir)
	r3ok(t, err)
	sentinel, _ := f.lstat("sentinel")
	r3fillDir(t, f, dir, sentinel, exts)
	dir.setFlags(dir.flags() &^ flagExtents)
	clear(dir.iblock())
	le.PutUint32(dir.iblock(), uint32(exts[0].phys))
	r3ok(t, f.writeInode(dir))
	r3ok(t, f.flush())
	mustCheck(t, img)
	fsckIfInstalled(t, p)
	before := fileHash(t, p)
	long := ChangeSet{Files: []Entry{memEntry("d/"+strings.Repeat("z", 255), 0o644, []byte("payload"))}}
	if err := img.Apply(context.Background(), long); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Apply = %v, want ErrUnsupported", err)
	}
	r3ok(t, img.Close())
	if fileHash(t, p) != before {
		t.Fatal("the image changed although the change was refused")
	}
	img = openRW(t, p)
	short := ChangeSet{Files: []Entry{memEntry("d/fits", 0o644, []byte("payload"))}}
	r3ok(t, img.Apply(context.Background(), short))
	r3ok(t, img.Close())
	r3ok(t, VerifyFile(context.Background(), p, short))
	fsckIfInstalled(t, p)
}
