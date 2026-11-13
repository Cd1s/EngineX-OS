package ext4

import (
	"fmt"
)

// ErrNoSpace means the filesystem cannot hold the requested change. It is
// returned by the capacity preflight before anything is written.
var errNoSpaceInternal = ErrNoSpace

// allocRun finds up to want free blocks starting at the first free block at
// or after goal, staying inside one group, and marks them used.
func (f *fsys) allocRun(goal uint64, want uint64) (uint64, uint64, error) {
	if goal < f.firstDataBlock || goal >= f.blocksCount {
		goal = f.firstDataBlock
	}
	startG := f.groupOf(goal)
	for i := 0; i <= f.groups; i++ {
		g := (startG + i) % f.groups
		if f.gdFreeBlocks(g) == 0 {
			continue
		}
		bm, err := f.blockBitmap(g)
		if err != nil {
			return 0, 0, err
		}
		first := uint64(0)
		if i == 0 {
			first = goal - f.groupFirstBlock(g)
		}
		n := f.groupBlocks(g)
		for b := first; b < n; b++ {
			if testBit(bm, b) {
				continue
			}
			run := uint64(0)
			for b+run < n && run < want && !testBit(bm, b+run) {
				run++
			}
			for k := uint64(0); k < run; k++ {
				setBit(bm, b+k)
			}
			f.dirtyBB[g] = true
			f.gdSet(g, gdFreeBlocksLo, gdFreeBlocksHi, 16, f.gdFreeBlocks(g)-run)
			f.setSbFreeBlocks(f.sbFreeBlocks() - run)
			return f.groupFirstBlock(g) + b, run, nil
		}
	}
	return 0, 0, errNoSpaceInternal
}

// allocData allocates n data blocks near goal as one or more extents.
func (f *fsys) allocData(goal uint64, n uint64, logical uint32) ([]extent, error) {
	var out []extent
	for n > 0 {
		want := min(n, maxInitExtent)
		start, got, err := f.allocRun(goal, want)
		if err != nil {
			for _, e := range out {
				f.freeBlocks(e.phys, uint64(e.length))
			}
			return nil, err
		}
		if k := len(out); k > 0 && out[k-1].end() == start && uint64(out[k-1].length)+got <= maxInitExtent {
			out[k-1].length += uint32(got)
		} else {
			out = append(out, extent{logical: logical, phys: start, length: uint32(got)})
		}
		logical += uint32(got)
		n -= got
		goal = start + got
	}
	return out, nil
}

// allocMeta allocates n single metadata blocks near goal.
func (f *fsys) allocMeta(goal uint64, n int) ([]uint64, error) {
	var out []uint64
	for len(out) < n {
		b, _, err := f.allocRun(goal, 1)
		if err != nil {
			f.freeList(out)
			return nil, err
		}
		out = append(out, b)
		goal = b + 1
	}
	return out, nil
}

func (f *fsys) freeList(blocks []uint64) {
	for _, b := range blocks {
		f.freeBlocks(b, 1)
	}
}

// freeBlocks releases n blocks starting at start. Releasing a block that is
// already free indicates corruption and is reported by the checker; here it
// is ignored so counters stay consistent.
func (f *fsys) freeBlocks(start, n uint64) error {
	for n > 0 {
		g := f.groupOf(start)
		bm, err := f.blockBitmap(g)
		if err != nil {
			return err
		}
		off := start - f.groupFirstBlock(g)
		k := min(n, f.groupBlocks(g)-off)
		var freed uint64
		for i := uint64(0); i < k; i++ {
			if testBit(bm, off+i) {
				clearBit(bm, off+i)
				freed++
			}
		}
		if freed != k {
			return corruptf("freeing %d blocks at %d: %d were already free", k, start, k-freed)
		}
		f.dirtyBB[g] = true
		f.gdSet(g, gdFreeBlocksLo, gdFreeBlocksHi, 16, f.gdFreeBlocks(g)+freed)
		f.setSbFreeBlocks(f.sbFreeBlocks() + freed)
		start += k
		n -= k
	}
	return nil
}

// allocInode takes a free inode, preferring group pref.
func (f *fsys) allocInode(pref int, dir bool) (uint32, error) {
	for i := 0; i < f.groups; i++ {
		g := (pref + i) % f.groups
		if f.gdFreeInodes(g) == 0 {
			continue
		}
		bm, err := f.inodeBitmap(g)
		if err != nil {
			return 0, err
		}
		for idx := uint64(0); idx < uint64(f.inodesPerGroup); idx++ {
			num := uint32(g)*f.inodesPerGroup + uint32(idx) + 1
			if num < f.firstIno || testBit(bm, idx) {
				continue
			}
			setBit(bm, idx)
			f.dirtyIB[g] = true
			f.gdSet(g, gdFreeInodesLo, gdFreeInodesHi, 16, f.gdFreeInodes(g)-1)
			if dir {
				f.gdSet(g, gdUsedDirsLo, gdUsedDirsHi, 16, f.gdUsedDirs(g)+1)
			}
			if f.hasGroupCsum() {
				used := uint64(f.inodesPerGroup) - f.gdItableUnused(g)
				if idx+1 > used {
					f.gdSet(g, gdItableUnusedLo, gdItableUnusedHi, 16, uint64(f.inodesPerGroup)-idx-1)
				}
			}
			f.setSbFreeInodes(f.sbFreeInodes() - 1)
			return num, nil
		}
	}
	return 0, errNoSpaceInternal
}

func (f *fsys) freeInode(num uint32, dir bool) error {
	g := int((num - 1) / f.inodesPerGroup)
	idx := uint64((num - 1) % f.inodesPerGroup)
	bm, err := f.inodeBitmap(g)
	if err != nil {
		return err
	}
	if !testBit(bm, idx) {
		return corruptf("inode %d is already free", num)
	}
	clearBit(bm, idx)
	f.dirtyIB[g] = true
	f.gdSet(g, gdFreeInodesLo, gdFreeInodesHi, 16, f.gdFreeInodes(g)+1)
	if dir {
		f.gdSet(g, gdUsedDirsLo, gdUsedDirsHi, 16, f.gdUsedDirs(g)-1)
	}
	f.setSbFreeInodes(f.sbFreeInodes() + 1)
	return nil
}

func (f *fsys) inodeGroup(num uint32) int { return int((num - 1) / f.inodesPerGroup) }

// goalFor returns a data-block goal in the inode's group.
func (f *fsys) goalFor(num uint32) uint64 {
	g := f.inodeGroup(num)
	return f.groupFirstBlock(g)
}
