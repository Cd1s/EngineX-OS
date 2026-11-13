package ext4

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Fresh filesystem profile. It is the stock deck's own mkfs.ext4 profile
// (e2fsprogs 1.47.0 with "-O encrypt") minus the three features that need
// extra on-disk structures this package does not build: resize_inode
// (reserved GDT blocks for online growth), orphan_file and
// metadata_csum_seed. The deck's kernel (6.18) accepts the result, and the
// deck's boot-time fsck.ext4 leaves it unchanged (see the tests).
const (
	freshCompat   = compatHasJournal | compatExtAttr | compatDirIndex
	freshIncompat = incompatFiletype | incompatExtents | incompat64bit | incompatFlexBg | incompatEncrypt
	freshROCompat = roCompatSparseSuper | roCompatLargeFile | roCompatHugeFile | roCompatDirNlink |
		roCompatExtraIsize | roCompatMetadataCsum

	freshBlockSize   = 4096
	freshInodeSize   = 256
	freshDescSize    = 64
	freshExtraIsize  = 32
	freshLogFlex     = 4
	lostFoundBlocks  = 4
	minFreshBlocks   = 2048 // 8 MiB: below this mke2fs would not add a journal either
	reservedPercent  = 5
	defaultMountOpts = 0x000C // user_xattr, acl
)

// journalBlocks follows ext2fs_default_journal_size (e2fsprogs 1.47).
func journalBlocks(blocks uint64) uint64 {
	switch {
	case blocks < 32768:
		return 1024
	case blocks < 256*1024:
		return 4096
	case blocks < 512*1024:
		return 8192
	case blocks < 4096*1024:
		return 16384
	case blocks < 8192*1024:
		return 32768
	case blocks < 16384*1024:
		return 65536
	case blocks < 32768*1024:
		return 131072
	}
	return 262144
}

type mkfsParams struct {
	size  int64
	label string
	uuid  [16]byte
	hash  [16]byte
	now   time.Time
}

// mkfs writes a fresh filesystem to dev, which must read as zeros.
func mkfs(dev writableDevice, p mkfsParams) (*fsys, error) {
	bs := uint64(freshBlockSize)
	blocks := uint64(p.size) / bs
	if blocks < minFreshBlocks {
		return nil, fmt.Errorf("%w: %d bytes is too small for a data filesystem", ErrNoSpace, p.size)
	}
	if blocks > 1<<32-1 {
		return nil, fmt.Errorf("ext4: %d blocks exceeds the supported 16 TiB", blocks)
	}
	bpg := bs * 8
	inodeRatio := uint64(16384)
	if uint64(p.size) < 512<<20 {
		inodeRatio = 4096 // mke2fs "small" type
	}
	ipb := bs / freshInodeSize
	itableFor := func(ipg uint64) uint64 { return ipg / ipb }
	descPerBlock := bs / freshDescSize

	var groups, ipg, gdtBlocks uint64
	for range 3 {
		groups = (blocks + bpg - 1) / bpg
		gdtBlocks = (groups + descPerBlock - 1) / descPerBlock
		ipg = (blocks*bs/inodeRatio + groups - 1) / groups
		ipg = (ipg + ipb - 1) / ipb * ipb
		ipg = max(ipg, ipb*2)
		ipg = min(ipg, bs*8, (bpg-gdtBlocks-3)/2*ipb)
		ipg = ipg / 8 * 8
		overhead := 1 + gdtBlocks + 2 + itableFor(ipg)
		last := blocks - (groups-1)*bpg
		if groups > 1 && last < overhead+50 {
			blocks -= last // drop a runt last group, as mke2fs does
			continue
		}
		break
	}
	if groups*ipg > 1<<32-1 {
		return nil, fmt.Errorf("ext4: too many inodes")
	}

	sb := make([]byte, superblockSize)
	put32 := func(off int, v uint32) { le.PutUint32(sb[off:], v) }
	put16 := func(off int, v uint16) { le.PutUint16(sb[off:], v) }
	put32(sbInodesCount, uint32(groups*ipg))
	put32(sbBlocksCountLo, uint32(blocks))
	put32(sbRBlocksCountLo, uint32(blocks*reservedPercent/100))
	put32(sbFirstDataBlock, 0)
	put32(sbLogBlockSize, 2)
	put32(sbLogClusterSize, 2)
	put32(sbBlocksPerGroup, uint32(bpg))
	put32(sbClustersPerGroup, uint32(bpg))
	put32(sbInodesPerGroup, uint32(ipg))
	put16(sbMaxMntCount, 0xFFFF)
	put16(sbMagicOff, sbMagic)
	put16(sbState, 1)
	put16(sbErrors, 1)
	put32(sbCreatorOS, 0)
	put32(sbRevLevel, 1)
	put32(sbFirstIno, firstGoodIno)
	put16(sbInodeSize, freshInodeSize)
	put32(sbFeatureCompat, freshCompat)
	put32(sbFeatureIncompat, freshIncompat)
	put32(sbFeatureROCompat, freshROCompat)
	copy(sb[sbUUID:], p.uuid[:])
	copy(sb[sbVolumeName:sbVolumeName+16], p.label)
	put32(sbJournalInum, journalIno)
	copy(sb[sbHashSeed:], p.hash[:])
	sb[sbDefHashVersion] = 1 // half_md4
	sb[sbJnlBackupType] = 1  // EXT3_JNL_BACKUP_BLOCKS
	put16(sbDescSize, freshDescSize)
	put32(sbDefaultMountOpts, defaultMountOpts)
	t := p.now.Unix()
	put32(sbMkfsTime, uint32(t))
	sb[sbMkfsTimeHi] = byte(t >> 32)
	put32(sbLastCheck, uint32(t))
	sb[sbLastCheckHi] = byte(t >> 32)
	put16(sbMinExtraIsize, freshExtraIsize)
	put16(sbWantExtraIsize, freshExtraIsize)
	put32(sbFlags, 0x1) // signed directory hash
	sb[sbLogGroupsPerFlex] = freshLogFlex
	sb[sbChecksumType] = 1

	f := &fsys{dev: dev, wdev: dev, size: p.size, sb: sb, bs: int64(bs), now: func() time.Time { return p.now },
		blocksCount: blocks, blocksPerGroup: bpg, inodesPerGroup: uint32(ipg), inodesCount: uint32(groups * ipg),
		inodeSize: freshInodeSize, descSize: freshDescSize, groups: int(groups), gdtBlocks: gdtBlocks,
		firstIno: firstGoodIno, dirtyBB: map[int]bool{}, dirtyIB: map[int]bool{}, dirtyGD: map[int]bool{}}
	f.csumSeed = crc32c(^uint32(0), p.uuid[:])
	f.gd = make([][]byte, groups)
	f.blockBitmaps = make([][]byte, groups)
	f.inodeBitmaps = make([][]byte, groups)

	// Classic layout: each group's bitmaps and inode table at its start,
	// after the superblock/descriptor copy. flex_bg permits this placement.
	var freeTotal uint64
	for g := 0; g < int(groups); g++ {
		f.gd[g] = make([]byte, freshDescSize)
		start := f.groupFirstBlock(g)
		at := start + f.baseMetaBlocks(g)
		f.gdSet(g, gdBlockBitmapLo, gdBlockBitmapHi, 32, at)
		f.gdSet(g, gdInodeBitmapLo, gdInodeBitmapHi, 32, at+1)
		f.gdSet(g, gdInodeTableLo, gdInodeTableHi, 32, at+2)
		used := at + 2 + itableFor(ipg) - start
		if used >= f.groupBlocks(g) {
			return nil, fmt.Errorf("%w: group %d has no room for data", ErrNoSpace, g)
		}
		bb := make([]byte, bs)
		for i := uint64(0); i < used; i++ {
			setBit(bb, i)
		}
		for i := f.groupBlocks(g); i < bs*8; i++ {
			setBit(bb, i)
		}
		ib := make([]byte, bs)
		for i := ipg; i < bs*8; i++ {
			setBit(ib, i)
		}
		f.blockBitmaps[g], f.inodeBitmaps[g] = bb, ib
		free := f.groupBlocks(g) - used
		freeTotal += free
		f.gdSet(g, gdFreeBlocksLo, gdFreeBlocksHi, 16, free)
		f.gdSet(g, gdFreeInodesLo, gdFreeInodesHi, 16, ipg)
		f.gdSet(g, gdItableUnusedLo, gdItableUnusedHi, 16, ipg)
		f.dirtyBB[g], f.dirtyIB[g] = true, true
	}
	f.setSbFreeBlocks(freeTotal)
	f.setSbFreeInodes(uint32(groups * ipg))

	// Reserved inodes 1..10 are in use; only the bad-block (1), root (2) and
	// journal (8) inodes have content.
	for ino := uint32(1); ino < firstGoodIno; ino++ {
		setBit(f.inodeBitmaps[0], uint64(ino-1))
	}
	f.gdSet(0, gdFreeInodesLo, gdFreeInodesHi, 16, ipg-(firstGoodIno-1))
	f.gdSet(0, gdItableUnusedLo, gdItableUnusedHi, 16, ipg-(firstGoodIno-1))
	f.setSbFreeInodes(uint32(groups*ipg) - (firstGoodIno - 1))

	bad := f.newInodeRaw(1, 0, p.now)
	le.PutUint32(bad.raw[inCrtime:], 0)
	le.PutUint32(bad.raw[inCrtimeExtra:], 0)
	if err := f.writeInode(bad); err != nil {
		return nil, err
	}

	// Root directory.
	root := f.newInodeRaw(rootIno, sIFDIR|0o755, p.now)
	rb, err := f.allocData(f.groupFirstBlock(0), 1, 0)
	if err != nil {
		return nil, err
	}
	if err := f.writeBlock(rb[0].phys, f.newDirBlock(root, rootIno)); err != nil {
		return nil, err
	}
	if _, err := f.writeExtentTree(root, rb, 0); err != nil {
		return nil, err
	}
	root.setSize(bs)
	root.setLinks(2)
	_ = root.setBlocks512(f, bs/512)
	f.gdSet(0, gdUsedDirsLo, gdUsedDirsHi, 16, 1)
	if err := f.writeInode(root); err != nil {
		return nil, err
	}

	// lost+found, preallocated to 16 KiB like mke2fs so e2fsck can relink
	// files without allocating.
	lpf, err := f.allocInode(0, true)
	if err != nil || lpf != firstGoodIno {
		return nil, fmt.Errorf("ext4: lost+found got inode %d: %v", lpf, err)
	}
	lin := f.newInodeRaw(lpf, sIFDIR|0o700, p.now)
	lb, err := f.allocData(f.groupFirstBlock(0), lostFoundBlocks, 0)
	if err != nil {
		return nil, err
	}
	if len(lb) != 1 {
		return nil, fmt.Errorf("ext4: lost+found is fragmented")
	}
	for i := uint64(0); i < lostFoundBlocks; i++ {
		d := f.emptyDirBlock(lin)
		if i == 0 {
			d = f.newDirBlock(lin, rootIno)
		}
		if err := f.writeBlock(lb[0].phys+i, d); err != nil {
			return nil, err
		}
	}
	if _, err := f.writeExtentTree(lin, lb, 0); err != nil {
		return nil, err
	}
	lin.setSize(lostFoundBlocks * bs)
	lin.setLinks(2)
	_ = lin.setBlocks512(f, lostFoundBlocks*bs/512)
	if err := f.writeInode(lin); err != nil {
		return nil, err
	}
	if err := f.addEntry(root, "lost+found", lpf, ftDir); err != nil {
		return nil, err
	}
	root.setLinks(3)
	if err := f.writeInode(root); err != nil {
		return nil, err
	}

	if err := f.makeJournal(journalBlocks(blocks), p); err != nil {
		return nil, err
	}
	if err := f.flush(); err != nil {
		return nil, err
	}
	return f, nil
}

// makeJournal creates the internal jbd2 journal in inode 8. Only the journal
// superblock has content; the rest of the journal must read as zeros, which
// is why the whole image (zeros included) has to be written to the deck.
func (f *fsys) makeJournal(n uint64, p mkfsParams) error {
	in := f.newInodeRaw(journalIno, sIFREG|0o600, p.now)
	// Place the journal in the middle group, as mke2fs does, to keep group 0
	// free for the files injected next.
	goal := f.groupFirstBlock(f.groups / 2)
	exts, err := f.allocData(goal, n, 0)
	if err != nil {
		return fmt.Errorf("%w: no room for a %d-block journal", ErrNoSpace, n)
	}
	jsb := make([]byte, f.bs)
	be.PutUint32(jsb[0x0:], jbd2Magic)
	be.PutUint32(jsb[0x4:], 4) // JBD2_SUPERBLOCK_V2
	be.PutUint32(jsb[0xC:], uint32(f.bs))
	be.PutUint32(jsb[0x10:], uint32(n))
	be.PutUint32(jsb[0x14:], 1) // s_first
	be.PutUint32(jsb[0x18:], 1) // s_sequence
	copy(jsb[0x30:], p.uuid[:])
	be.PutUint32(jsb[0x40:], 1) // s_nr_users
	copy(jsb[0x100:], p.uuid[:])
	if err := f.writeBlock(exts[0].phys, jsb); err != nil {
		return err
	}
	meta, err := f.writeExtentTree(in, exts, goal)
	if err != nil {
		return err
	}
	in.setSize(n * uint64(f.bs))
	in.setLinks(1)
	if err := in.setBlocks512(f, (n+uint64(len(meta)))*uint64(f.bs/512)); err != nil {
		return err
	}
	if err := f.writeInode(in); err != nil {
		return err
	}
	// Superblock backup of the journal inode's block map and size.
	copy(f.sb[sbJnlBlocks:], in.iblock())
	le.PutUint32(f.sb[sbJnlBlocks+15*4:], uint32(in.size()>>32))
	le.PutUint32(f.sb[sbJnlBlocks+16*4:], uint32(in.size()))
	f.dirtySB = true
	return nil
}

func randomUUID() ([16]byte, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return u, err
	}
	u[6] = u[6]&0x0F | 0x40
	u[8] = u[8]&0x3F | 0x80
	return u, nil
}
