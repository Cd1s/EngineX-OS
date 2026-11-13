package ext4

import (
	"context"
	"fmt"
	"strings"
)

// CheckError lists every structural problem the native checker found.
type CheckError struct{ Problems []string }

func (e *CheckError) Error() string {
	s := e.Problems
	more := ""
	if len(s) > 12 {
		more = fmt.Sprintf(" (+%d more)", len(s)-12)
		s = s[:12]
	}
	return "ext4: check failed: " + strings.Join(s, "; ") + more
}

// Check is a read-only structural check modelled on e2fsck passes 1-5: it
// verifies every checksum, extent tree and directory block, that no block is
// claimed twice, that bitmaps and all free/used counters match what the
// inodes actually use, that every in-use inode is reachable, and that link
// counts and '..' entries are right. It repairs nothing.
func (img *Image) Check(ctx context.Context) error {
	if !img.ro {
		if err := img.fs.flush(); err != nil {
			return err
		}
	}
	c := &checker{f: img.fs}
	if err := c.run(ctx); err != nil {
		return err
	}
	if len(c.problems) != 0 {
		return &CheckError{Problems: c.problems}
	}
	return nil
}

type checker struct {
	f        *fsys
	problems []string
	used     []byte // computed block usage, one bit per block
	inUse    map[uint32]*inodeInfo
	xattrRef map[uint64]uint32
}

type inodeInfo struct {
	links  uint16
	dir    bool
	ftype  uint8
	refs   uint32
	parent uint32 // directory holding this directory's name
	dotdot uint32 // target of this directory's '..'
	subdir uint32
}

func (c *checker) addf(format string, a ...any) {
	if len(c.problems) < 1000 {
		c.problems = append(c.problems, fmt.Sprintf(format, a...))
	}
}

func (c *checker) claim(b uint64, what string) {
	f := c.f
	if b < f.firstDataBlock || b >= f.blocksCount {
		c.addf("%s uses block %d outside the filesystem", what, b)
		return
	}
	if testBit(c.used, b) {
		c.addf("block %d is claimed twice (again by %s)", b, what)
		return
	}
	setBit(c.used, b)
}

func (c *checker) run(ctx context.Context) error {
	f := c.f
	c.used = make([]byte, (f.blocksCount+7)/8)
	c.inUse = map[uint32]*inodeInfo{}
	c.xattrRef = map[uint64]uint32{}

	// Pass 0: fixed metadata.
	for g := 0; g < f.groups; g++ {
		start := f.groupFirstBlock(g)
		for i := uint64(0); i < f.baseMetaBlocks(g); i++ {
			c.claim(start+i, fmt.Sprintf("group %d superblock/descriptors", g))
		}
		c.claim(f.blockBitmapBlock(g), fmt.Sprintf("group %d block bitmap", g))
		c.claim(f.inodeBitmapBlock(g), fmt.Sprintf("group %d inode bitmap", g))
		it := f.inodeTableBlock(g)
		for i := uint64(0); i < f.itableBlocks(); i++ {
			c.claim(it+i, fmt.Sprintf("group %d inode table", g))
		}
	}

	// Pass 1: inodes.
	orphanFile := uint32(0)
	if f.compat()&compatOrphanFile != 0 {
		orphanFile = le.Uint32(f.sb[sbOrphanFileInum:])
	}
	for g := 0; g < f.groups; g++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ibm, err := f.inodeBitmap(g)
		if err != nil {
			return err
		}
		limit := uint64(f.inodesPerGroup)
		if f.hasGroupCsum() {
			unused := f.gdItableUnused(g)
			if unused > limit {
				c.addf("group %d: %d unused inodes of %d", g, unused, limit)
				unused = 0
			}
			limit -= unused
			if f.gdFlags(g)&bgInodeUninit != 0 {
				limit = 0
			}
		}
		for idx := uint64(0); idx < uint64(f.inodesPerGroup); idx++ {
			num := uint32(g)*f.inodesPerGroup + uint32(idx) + 1
			marked := testBit(ibm, idx)
			if idx >= limit {
				if marked {
					c.addf("inode %d is marked in use in the unused part of group %d", num, g)
				}
				continue
			}
			in, err := f.readInode(num)
			if err != nil {
				c.addf("%v", err)
				continue
			}
			c.inode(in, marked, orphanFile)
		}
	}

	// Pass 2-4: directories, reachability, link counts.
	c.directories(ctx)
	for num, info := range c.inUse {
		switch {
		case num < f.firstIno && num != rootIno:
			continue
		case num == orphanFile:
			continue
		case num == rootIno:
			if info.refs != 0 {
				c.addf("the root directory is named by %d entries", info.refs)
			}
		case info.refs == 0:
			c.addf("inode %d is in use but no directory entry names it", num)
			continue
		}
		if info.dir {
			want := 2 + info.subdir
			if uint32(info.links) != want && !(info.links == 1 && f.roCompat()&roCompatDirNlink != 0) {
				c.addf("directory inode %d has link count %d, counted %d", num, info.links, want)
			}
			if info.refs > 1 {
				c.addf("directory inode %d is named by %d entries", num, info.refs)
			}
			if info.dotdot != info.parent {
				c.addf("directory inode %d: '..' is %d, parent is %d", num, info.dotdot, info.parent)
			}
		} else if uint32(info.links) != info.refs {
			c.addf("inode %d has link count %d, counted %d", num, info.links, info.refs)
		}
	}
	for blk, refs := range c.xattrRef {
		data, err := f.readXattrBlock(blk)
		if err != nil {
			c.addf("%v", err)
			continue
		}
		if got := le.Uint32(data[4:]); got != refs {
			c.addf("xattr block %d has refcount %d, counted %d", blk, got, refs)
		}
	}

	// Pass 5: bitmaps and counters.
	c.bitmaps()
	return nil
}

// inode checks one inode of the used part of the inode table.
func (c *checker) inode(in *inode, marked bool, orphanFile uint32) {
	f := c.f
	num := in.num
	if num < f.firstIno {
		if !marked {
			c.addf("reserved inode %d is not marked in use", num)
		}
		if in.allZero() || in.mode() == 0 && num != journalIno && num != resizeIno && num != rootIno {
			if num == rootIno {
				c.addf("root inode is empty")
			}
			return
		}
		if num == resizeIno {
			// Its indirect map covers the reserved descriptor blocks,
			// which pass 0 already claimed; only the DIND block is its own.
			if d := uint64(le.Uint32(in.iblock()[13*4:])); d != 0 {
				c.claim(d, "resize inode")
			}
			return
		}
		if num != rootIno && num != journalIno {
			exts, meta, err := f.fileMap(in)
			if err != nil {
				c.addf("reserved inode %d: %v", num, err)
				return
			}
			for _, e := range exts {
				for i := uint64(0); i < uint64(e.length); i++ {
					c.claim(e.phys+i, fmt.Sprintf("reserved inode %d", num))
				}
			}
			for _, b := range meta {
				c.claim(b, fmt.Sprintf("reserved inode %d", num))
			}
			return
		}
	}
	if in.links() == 0 {
		if marked && num >= f.firstIno {
			c.addf("inode %d is marked in use but has no links", num)
		}
		if in.dtime() == 0 && in.mode() != 0 {
			c.addf("deleted inode %d has zero dtime", num)
		}
		return
	}
	if !marked {
		c.addf("inode %d has %d links but is not marked in use", num, in.links())
	}
	ft := fileTypeOf(in.mode())
	if ft == ftUnknown {
		c.addf("inode %d has invalid mode %#o", num, in.mode())
		return
	}
	if fl := in.flags(); fl&(flagInlineData|flagEAInode|flagVerity) != 0 {
		c.addf("inode %d uses unsupported flags %#x", num, fl)
		return
	}
	info := &inodeInfo{links: in.links(), dir: in.isDir(), ftype: ft}
	c.inUse[num] = info
	exts, meta, err := f.fileMap(in)
	if err != nil {
		c.addf("%v", err)
		return
	}
	var nblocks uint64
	var lastInit uint64
	for _, e := range exts {
		for i := uint64(0); i < uint64(e.length); i++ {
			c.claim(e.phys+i, fmt.Sprintf("inode %d", num))
		}
		nblocks += uint64(e.length)
		if !e.uninit {
			lastInit = max(lastInit, uint64(e.logical)+uint64(e.length))
		}
	}
	for _, b := range meta {
		c.claim(b, fmt.Sprintf("inode %d mapping", num))
	}
	nblocks += uint64(len(meta))
	if acl := in.fileACL(f); acl != 0 {
		if c.xattrRef[acl] == 0 {
			c.claim(acl, fmt.Sprintf("inode %d xattr block", num))
		}
		c.xattrRef[acl]++
		nblocks++
	}
	if got, want := in.blocks512(f), nblocks*uint64(f.bs/512); got != want {
		c.addf("inode %d has i_blocks %d, counted %d", num, got, want)
	}
	if in.isReg() && lastInit*uint64(f.bs) > in.size()+uint64(f.bs)-1 && num != orphanFile {
		c.addf("inode %d maps initialized blocks past its size %d", num, in.size())
	}
	if in.isSymlink() && !f.isFastSymlink(in) && (in.size() == 0 || in.size() >= uint64(f.bs) || nblocks == 0) {
		c.addf("symlink inode %d has size %d and %d blocks", num, in.size(), nblocks)
	}
}

// directories reads every directory, checks its entries and counts names.
func (c *checker) directories(ctx context.Context) {
	f := c.f
	if info := c.inUse[rootIno]; info == nil || !info.dir {
		c.addf("root inode is not an in-use directory")
		return
	}
	c.inUse[rootIno].parent = rootIno
	for num, info := range c.inUse {
		if ctx.Err() != nil {
			return
		}
		if !info.dir {
			continue
		}
		in, err := f.readInode(num)
		if err != nil {
			c.addf("%v", err)
			continue
		}
		blocks, err := f.readDir(in)
		if err != nil {
			c.addf("%v", err)
			continue
		}
		ents, err := f.listEntries(blocks)
		if err != nil {
			c.addf("directory inode %d: %v", num, err)
			continue
		}
		if len(ents) < 2 || string(ents[0].name) != "." || ents[0].ino != num || string(ents[1].name) != ".." ||
			ents[0].off != 0 || ents[0].blockIdx != 0 || ents[1].blockIdx != 0 {
			c.addf("directory inode %d does not start with '.' and '..'", num)
			continue
		}
		info.dotdot = ents[1].ino
		if t := c.inUse[ents[1].ino]; t == nil || !t.dir {
			c.addf("directory inode %d: '..' points at %d, not a directory", num, ents[1].ino)
		}
		names := map[string]bool{}
		for _, e := range ents[2:] {
			name := string(e.name)
			if name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || name == "" {
				c.addf("directory inode %d has an invalid name %q", num, name)
				continue
			}
			if names[name] && in.flags()&flagEncrypt == 0 {
				c.addf("directory inode %d has a duplicate name %q", num, name)
			}
			names[name] = true
			t := c.inUse[e.ino]
			if t == nil || e.ino < f.firstIno && e.ino != rootIno {
				c.addf("directory inode %d: %q points at unused or reserved inode %d", num, name, e.ino)
				continue
			}
			if e.ftype != t.ftype {
				c.addf("directory inode %d: %q has file type %d, inode %d is type %d", num, name, e.ftype, e.ino, t.ftype)
			}
			t.refs++
			if t.dir {
				t.parent = num
				info.subdir++
			}
		}
	}
}

// bitmaps compares on-disk bitmaps and counters with what pass 0-1 found.
func (c *checker) bitmaps() {
	f := c.f
	var freeBlocks, freeInodes uint64
	for g := 0; g < f.groups; g++ {
		bm, err := f.blockBitmap(g)
		if err != nil {
			c.addf("%v", err)
			continue
		}
		start := f.groupFirstBlock(g)
		n := f.groupBlocks(g)
		var free uint64
		diffs := 0
		for i := uint64(0); i < n; i++ {
			disk := testBit(bm, i)
			calc := testBit(c.used, start+i)
			if !calc {
				free++
			}
			if disk != calc {
				if diffs < 5 {
					c.addf("block %d is %s in the bitmap but %s", start+i, usedWord(disk), usedWord(calc))
				}
				diffs++
			}
		}
		if diffs > 5 {
			c.addf("group %d: %d more block bitmap differences", g, diffs-5)
		}
		if f.gdFlags(g)&bgBlockUninit == 0 || !f.hasGroupCsum() {
			raw, err := f.readBlock(f.blockBitmapBlock(g))
			if err == nil {
				for i := n; i < uint64(f.bs)*8; i++ {
					if !testBit(raw, i) {
						c.addf("group %d: block bitmap padding is not set", g)
						break
					}
				}
			}
		}
		if got := f.gdFreeBlocks(g); got != free {
			c.addf("group %d: %d free blocks recorded, counted %d", g, got, free)
		}
		freeBlocks += free

		ibm, err := f.inodeBitmap(g)
		if err != nil {
			c.addf("%v", err)
			continue
		}
		var ifree, dirs uint64
		for idx := uint64(0); idx < uint64(f.inodesPerGroup); idx++ {
			num := uint32(g)*f.inodesPerGroup + uint32(idx) + 1
			info := c.inUse[num]
			calc := info != nil || num < f.firstIno
			if testBit(ibm, idx) != calc {
				c.addf("inode %d is %s in the bitmap but %s", num, usedWord(testBit(ibm, idx)), usedWord(calc))
			}
			if !calc {
				ifree++
			}
			if info != nil && info.dir {
				dirs++
			}
		}
		if got := f.gdFreeInodes(g); got != ifree {
			c.addf("group %d: %d free inodes recorded, counted %d", g, got, ifree)
		}
		if got := f.gdUsedDirs(g); got != dirs {
			c.addf("group %d: %d directories recorded, counted %d", g, got, dirs)
		}
		freeInodes += ifree
	}
	if got := f.sbFreeBlocks(); got != freeBlocks {
		c.addf("superblock: %d free blocks recorded, counted %d", got, freeBlocks)
	}
	if got := uint64(f.sbFreeInodes()); got != freeInodes {
		c.addf("superblock: %d free inodes recorded, counted %d", got, freeInodes)
	}
}

func usedWord(b bool) string {
	if b {
		return "used"
	}
	return "free"
}
