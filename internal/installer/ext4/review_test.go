package ext4

// Regression tests for the cross-family review of 192cfdd. Each one fails
// (hangs past its deadline, or accepts what it must refuse) on that commit.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// within runs fn and fails the test if it does not return in d: hostile
// metadata must be refused in bounded time, not walked.
func within(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v (unbounded walk)", what, d)
		return nil
	}
}

// fanOut makes in a block-mapped inode whose triple-indirect block repeats
// two double-indirect blocks, which repeat one indirect block, which
// repeats one data block: ~1024^3 pointers if walked naively.
func fanOut(t *testing.T, f *fsys, in *inode, size uint64) {
	t.Helper()
	blks, err := f.allocMeta(f.groupFirstBlock(0), 5)
	if err != nil {
		t.Fatal(err)
	}
	tind, d1, d2, ind, data := blks[0], blks[1], blks[2], blks[3], blks[4]
	fill := func(b uint64, ptr func(i int) uint64) {
		buf := make([]byte, f.bs)
		for i := 0; i < int(f.bs/4); i++ {
			le.PutUint32(buf[4*i:], uint32(ptr(i)))
		}
		if err := f.writeBlock(b, buf); err != nil {
			t.Fatal(err)
		}
	}
	fill(tind, func(i int) uint64 {
		if i%2 == 0 {
			return d1
		}
		return d2
	})
	fill(d1, func(int) uint64 { return ind })
	fill(d2, func(int) uint64 { return ind })
	fill(ind, func(int) uint64 { return data })
	in.setFlags(in.flags() &^ flagExtents)
	ib := in.iblock()
	clear(ib)
	le.PutUint32(ib[14*4:], uint32(tind))
	in.setSize(size)
	if err := f.writeInode(in); err != nil {
		t.Fatal(err)
	}
}

func TestHostileIndirectMapIsBounded(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	if err := img.Apply(context.Background(), ChangeSet{Files: []Entry{memEntry("victim", 0o644, []byte("x"))}}); err != nil {
		t.Fatal(err)
	}
	in, err := img.fs.lstat("victim")
	if err != nil {
		t.Fatal(err)
	}
	fanOut(t, img.fs, in, 1<<42)
	err = within(t, 10*time.Second, "fileMap", func() error { _, _, err := img.fs.fileMap(in); return err })
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("fileMap = %v, want ErrCorrupt", err)
	}
	err = within(t, 10*time.Second, "Check", func() error { return img.Check(context.Background()) })
	if err == nil {
		t.Fatal("Check accepted a fan-out indirect map")
	}
	// A block-mapped file may not map past its size.
	in.setFlags(in.flags() &^ flagExtents)
	clear(in.iblock())
	le.PutUint32(in.iblock()[5*4:], uint32(img.fs.groupFirstBlock(0)+100))
	in.setSize(4096)
	if err := img.fs.writeInode(in); err != nil {
		t.Fatal(err)
	}
	if _, _, err := img.fs.fileMap(in); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "size") {
		t.Fatalf("block past i_size: %v", err)
	}
}

func TestHostileRootAndJournalAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		ino  uint32
		size uint64
	}{
		{"root-huge-size", rootIno, 1 << 42},
		{"root-fanout", rootIno, 64 << 20},
		{"journal-fanout", journalIno, 4 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, p := newImage(t, 64<<20, "data")
			f := img.fs
			in, err := f.readInode(tc.ino)
			if err != nil {
				t.Fatal(err)
			}
			first, err := f.readBlock(firstMapped(t, f, in))
			if err != nil {
				t.Fatal(err)
			}
			fanOut(t, f, in, tc.size)
			// Keep logical block 0 readable (the journal superblock) so
			// only the fan-out is hostile.
			copyTo, err := f.allocMeta(f.groupFirstBlock(0), 1)
			if err != nil {
				t.Fatal(err)
			}
			f.writeBlock(copyTo[0], first)
			le.PutUint32(in.iblock(), uint32(copyTo[0]))
			f.writeInode(in)
			img.Close()
			src, closer, err := OpenFileSource(p)
			if err != nil {
				t.Fatal(err)
			}
			defer closer.Close()
			within(t, 10*time.Second, "Probe", func() error { _, err := Probe(src); return err })
			ro, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			if err := within(t, 10*time.Second, "Check", func() error { return ro.Check(context.Background()) }); err == nil {
				t.Fatal("Check accepted a hostile inode")
			}
		})
	}
}

func firstMapped(t *testing.T, f *fsys, in *inode) uint64 {
	t.Helper()
	exts, _, err := f.fileMap(in)
	if err != nil || len(exts) == 0 {
		t.Fatalf("map inode %d: %v", in.num, err)
	}
	return exts[0].phys
}

func TestSharedMappingBlocksStopTheCheck(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	var cs ChangeSet
	for i := 0; i < 40; i++ {
		cs.Files = append(cs.Files, memEntry(fmt.Sprintf("f%02d", i), 0o644, []byte("x")))
	}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	// Every file shares one indirect block that maps 1024 data blocks.
	f := img.fs
	blks, _ := f.allocMeta(f.groupFirstBlock(0), 2)
	buf := make([]byte, f.bs)
	for i := 0; i < int(f.bs/4); i++ {
		le.PutUint32(buf[4*i:], uint32(blks[1]))
	}
	f.writeBlock(blks[0], buf)
	for i := 0; i < 40; i++ {
		in, _ := f.lstat(fmt.Sprintf("f%02d", i))
		in.setFlags(in.flags() &^ flagExtents)
		clear(in.iblock())
		le.PutUint32(in.iblock()[12*4:], uint32(blks[0]))
		in.setSize(uint64(12+f.bs/4) * uint64(f.bs))
		f.writeInode(in)
	}
	err := within(t, 10*time.Second, "Check", func() error { return img.Check(context.Background()) })
	var ce *CheckError
	if !errors.As(err, &ce) || len(ce.Problems) > 300 {
		t.Fatalf("Check = %v", err)
	}
}

// Peak use in execution order: a new file listed before the replacement
// that would make room for it must be refused up front, not halfway.
func TestPreflightTracksPeakInExecutionOrder(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	free := img.Profile().FreeBlocks * 4096
	old := int64(free) * 6 / 10
	if err := img.Apply(context.Background(), ChangeSet{Files: []Entry{memEntry("old/big.bin", 0o644, make([]byte, old))}}); err != nil {
		t.Fatal(err)
	}
	img.Close()
	before := fileHash(t, p)
	newer := old * 9 / 10 // fits only once old/big.bin is gone
	order := ChangeSet{Files: []Entry{
		memEntry("new/a.bin", 0o644, pattern(int(newer), 1)),
		memEntry("old/big.bin", 0o644, []byte("small now")),
	}}
	img2 := openRW(t, p)
	err := img2.Apply(context.Background(), order)
	img2.Close()
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Apply = %v, want ErrNoSpace from the preflight", err)
	}
	if fileHash(t, p) != before {
		t.Fatal("the image changed although the change was refused")
	}
	// The same files in the order that frees first fit and are applied.
	img3 := openRW(t, p)
	defer img3.Close()
	reordered := ChangeSet{Files: []Entry{order.Files[1], order.Files[0]}}
	if err := img3.Apply(context.Background(), reordered); err != nil {
		t.Fatalf("freeing first: %v", err)
	}
	mustCheck(t, img3)
}

func TestPlanRejectsParentChildConflicts(t *testing.T) {
	cases := map[string]ChangeSet{
		"file-and-dir-below": {Files: []Entry{memEntry("a", 0o644, []byte("x"))}, Directories: []string{"a/b"}},
		"file-and-file-below": {Files: []Entry{memEntry("a", 0o644, []byte("x")),
			memEntry("a/b", 0o644, []byte("y"))}},
		"link-and-file-below": {Links: map[string]string{"l": "x"}, Files: []Entry{memEntry("l/f", 0o644, []byte("y"))}},
		"remove-and-file-below": {Remove: []string{"d"}, Files: []Entry{memEntry("d/f", 0o644, []byte("y"))}},
	}
	for name, cs := range cases {
		t.Run(name, func(t *testing.T) {
			img, p := newImage(t, 64<<20, "data")
			img.Close()
			before := fileHash(t, p)
			img = openRW(t, p)
			err := img.Apply(context.Background(), cs)
			img.Close()
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Apply = %v, want ErrUnsafePath", err)
			}
			if fileHash(t, p) != before {
				t.Fatal("the image changed")
			}
		})
	}
}

func TestLargeFileWithoutFeatureRefusedBeforeCapacity(t *testing.T) {
	img, p := newImage(t, 64<<20, "data")
	img.Close()
	data, _ := os.ReadFile(p)
	sb := data[superblockOffset : superblockOffset+superblockSize]
	le.PutUint32(sb[sbFeatureROCompat:], le.Uint32(sb[sbFeatureROCompat:])&^roCompatLargeFile)
	le.PutUint32(sb[sbChecksum:], crc32c(^uint32(0), sb[:sbChecksum]))
	q := filepath.Join(t.TempDir(), "nolarge.img")
	os.WriteFile(q, data, 0o600)
	img = openRW(t, q)
	defer img.Close()
	e := Entry{Path: "big/file.bin", Mode: 0o644, Size: 3 << 30, Open: memEntry("x", 0, nil).Open}
	err := img.Apply(context.Background(), ChangeSet{Files: []Entry{e}})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "large_file") {
		t.Fatalf("Apply = %v, want the large_file refusal", err)
	}
}

// appendBlock when the directory's old tree had more tree blocks than the
// new one: i_blocks must drop by exactly the freed tree blocks.
func TestAppendBlockShrinkingTreeAccounting(t *testing.T) {
	img, _ := newImage(t, 64<<20, "data")
	f := img.fs
	if err := img.Apply(context.Background(), ChangeSet{Directories: []string{"d"}}); err != nil {
		t.Fatal(err)
	}
	dir, _ := f.lstat("d")
	exts, _, _ := f.fileMap(dir)
	// Rebuild its one-extent map as a depth-2 tree (two tree blocks).
	tree, _ := f.allocMeta(f.groupFirstBlock(0), 2)
	leaf := make([]byte, f.bs)
	le.PutUint16(leaf[0:], extentMagic)
	le.PutUint16(leaf[2:], 1)
	le.PutUint16(leaf[4:], uint16((f.bs-12)/12))
	le.PutUint32(leaf[12:], 0)
	le.PutUint16(leaf[16:], uint16(exts[0].length))
	le.PutUint32(leaf[20:], uint32(exts[0].phys))
	idx := make([]byte, f.bs)
	le.PutUint16(idx[0:], extentMagic)
	le.PutUint16(idx[2:], 1)
	le.PutUint16(idx[4:], uint16((f.bs-12)/12))
	le.PutUint16(idx[6:], 1)
	le.PutUint32(idx[16:], uint32(tree[1]))
	for _, b := range []struct {
		blk  uint64
		data []byte
	}{{tree[1], leaf}, {tree[0], idx}} {
		if f.hasMetadataCsum() {
			off := f.extentTailOffset(b.data)
			le.PutUint32(b.data[off:], crc32c(f.inodeSeed(dir.num, dir.generation()), b.data[:off]))
		}
		f.writeBlock(b.blk, b.data)
	}
	ib := dir.iblock()
	clear(ib)
	le.PutUint16(ib[0:], extentMagic)
	le.PutUint16(ib[2:], 1)
	le.PutUint16(ib[4:], 4)
	le.PutUint16(ib[6:], 2)
	le.PutUint32(ib[16:], uint32(tree[0]))
	dir.setBlocks512(f, 3*uint64(f.bs/512))
	f.writeInode(dir)
	mustCheck(t, img)
	// Fill the directory past one block: appendBlock rewrites the tree
	// in the inode and frees both tree blocks.
	var cs ChangeSet
	for i := 0; i < 200; i++ {
		cs.Files = append(cs.Files, memEntry(fmt.Sprintf("d/%03d-%s", i, strings.Repeat("n", 40)), 0o644, nil))
	}
	if err := img.Apply(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	mustCheck(t, img)
	dir, _ = f.lstat("d")
	_, meta, _ := f.fileMap(dir)
	if got, want := dir.blocks512(f), (dir.size()/uint64(f.bs)+uint64(len(meta)))*uint64(f.bs/512); got != want {
		t.Fatalf("i_blocks %d, want %d", got, want)
	}
}
