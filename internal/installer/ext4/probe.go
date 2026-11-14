package ext4

import (
	"fmt"
	"io"
	"strings"
)

// Source is a read-only image: a backup file or an in-memory buffer.
type Source interface {
	io.ReaderAt
	Size() int64
}

// Profile describes a probed filesystem.
type Profile struct {
	BlockSize   int
	Blocks      uint64
	FreeBlocks  uint64
	Inodes      uint32
	FreeInodes  uint32
	InodeSize   int
	Groups      int
	Label       string
	UUID        [16]byte
	Compat      uint32
	Incompat    uint32
	ROCompat    uint32
	Features    []string
	HasJournal  bool
	JournalSize uint64
}

var featureNames = []struct {
	set  int
	bit  uint32
	name string
}{
	{0, compatDirPrealloc, "dir_prealloc"}, {0, compatHasJournal, "has_journal"}, {0, compatExtAttr, "ext_attr"},
	{0, compatResizeInode, "resize_inode"}, {0, compatDirIndex, "dir_index"}, {0, compatSparseSuper2, "sparse_super2"},
	{0, compatStableInodes, "stable_inodes"}, {0, compatOrphanFile, "orphan_file"},
	{1, incompatFiletype, "filetype"}, {1, incompatExtents, "extent"}, {1, incompat64bit, "64bit"},
	{1, incompatFlexBg, "flex_bg"}, {1, incompatCsumSeed, "metadata_csum_seed"}, {1, incompatEncrypt, "encrypt"},
	{2, roCompatSparseSuper, "sparse_super"}, {2, roCompatLargeFile, "large_file"}, {2, roCompatHugeFile, "huge_file"},
	{2, roCompatGdtCsum, "uninit_bg"}, {2, roCompatDirNlink, "dir_nlink"}, {2, roCompatExtraIsize, "extra_isize"},
	{2, roCompatMetadataCsum, "metadata_csum"},
}

func (f *fsys) profile() *Profile {
	p := &Profile{BlockSize: int(f.bs), Blocks: f.blocksCount, FreeBlocks: f.sbFreeBlocks(), Inodes: f.inodesCount,
		FreeInodes: f.sbFreeInodes(), InodeSize: f.inodeSize, Groups: f.groups,
		Label:  strings.TrimRight(string(f.sb[sbVolumeName:sbVolumeName+16]), "\x00"),
		Compat: f.compat(), Incompat: f.incompat(), ROCompat: f.roCompat(), HasJournal: f.compat()&compatHasJournal != 0}
	copy(p.UUID[:], f.sb[sbUUID:sbUUID+16])
	sets := [3]uint32{p.Compat, p.Incompat, p.ROCompat}
	for _, n := range featureNames {
		if sets[n.set]&n.bit != 0 {
			p.Features = append(p.Features, n.name)
		}
	}
	return p
}

// Probe validates an image read-only and returns its profile. It refuses
// unsupported features, unclean state, a journal needing recovery and any
// checksum error in the superblock, descriptors, bitmaps, root directory or
// journal inode. It never writes.
func Probe(src Source) (*Profile, error) {
	f, err := openFS(src, src.Size(), false)
	if err != nil {
		return nil, err
	}
	if err := f.checkClean(); err != nil {
		return nil, err
	}
	p := f.profile()
	if p.HasJournal {
		j, err := f.readInode(journalIno)
		if err != nil {
			return nil, err
		}
		p.JournalSize = j.size()
	}
	return p, nil
}

// checkClean refuses a filesystem that was not cleanly unmounted, has
// recorded errors, or whose journal holds transactions.
func (f *fsys) checkClean() error {
	st := le.Uint16(f.sb[sbState:])
	if st&0x1 == 0 {
		return fmt.Errorf("%w: not cleanly unmounted (state %#x)", ErrDirty, st)
	}
	if st&0x2 != 0 {
		return fmt.Errorf("%w: errors were recorded (state %#x)", ErrDirty, st)
	}
	if st != 0x1 {
		return fmt.Errorf("%w: state flags %#x (orphan recovery or replay in progress)", ErrDirty, st)
	}
	if le.Uint32(f.sb[sbLastOrphan:]) != 0 {
		return fmt.Errorf("%w: orphan inode list is not empty", ErrDirty)
	}
	for _, g := range []int{0, f.groups - 1} {
		if _, err := f.blockBitmap(g); err != nil {
			return err
		}
		if _, err := f.inodeBitmap(g); err != nil {
			return err
		}
	}
	root, err := f.readInode(rootIno)
	if err != nil {
		return err
	}
	if !root.isDir() {
		return corruptf("root inode is not a directory")
	}
	if _, err := f.readDir(root); err != nil {
		return err
	}
	if f.compat()&compatHasJournal == 0 {
		return nil
	}
	jn := le.Uint32(f.sb[sbJournalInum:])
	if jn != journalIno || le.Uint32(f.sb[sbJournalDev:]) != 0 {
		return fmt.Errorf("%w: external or non-standard journal (inode %d)", ErrUnsupported, jn)
	}
	j, err := f.readInode(journalIno)
	if err != nil {
		return err
	}
	// The whole mapping is validated (bounded by the size check) before any
	// output exists, not just the block holding the journal superblock.
	if j.size() == 0 || j.size()%uint64(f.bs) != 0 || j.size()/uint64(f.bs) > min(f.blocksCount, maxJournalBlock) {
		return corruptf("journal inode has size %d", j.size())
	}
	exts, _, err := f.fileMap(j)
	if err != nil {
		return err
	}
	if len(exts) == 0 || exts[0].logical != 0 {
		return corruptf("journal inode has no first block")
	}
	jsb, err := f.readBlock(exts[0].phys)
	if err != nil {
		return err
	}
	if be.Uint32(jsb[0:]) != jbd2Magic {
		return corruptf("journal superblock magic missing")
	}
	if bt := be.Uint32(jsb[4:]); bt != 3 && bt != 4 {
		return corruptf("journal superblock type %d", bt)
	}
	if be.Uint32(jsb[0x1C:]) != 0 {
		return fmt.Errorf("%w: the journal holds transactions that need recovery", ErrDirty)
	}
	if be.Uint32(jsb[0x20:]) != 0 {
		return fmt.Errorf("%w: the journal recorded an error", ErrDirty)
	}
	return nil
}
