package ext4

import (
	"fmt"
)

// extent is one contiguous mapping of logical file blocks to physical blocks.
type extent struct {
	logical uint32
	phys    uint64
	length  uint32
	uninit  bool
}

func (e extent) end() uint64 { return e.phys + uint64(e.length) }

// Mapping bounds. A backup is untrusted: mapping one inode must stay
// linear in the image size whatever its metadata says.
const (
	maxFileExtents  = 1 << 22  // far beyond any real file's fragmentation
	maxJournalBlock = 10240000 // e2fsprogs' largest journal
)

// mapState collects one inode's mapping and enforces the bounds: no mapping
// block may appear twice, the inode may not map more blocks than the
// filesystem has, and an indirect map may not reach past i_size.
type mapState struct {
	in     *inode
	exts   []extent
	meta   []uint64
	seen   map[uint64]bool
	mapped uint64
	limit  uint64 // indirect maps: logical blocks allowed by i_size
}

func (f *fsys) visitMeta(st *mapState, b uint64) error {
	if b < f.firstDataBlock || b >= f.blocksCount {
		return corruptf("inode %d: mapping block %d out of range", st.in.num, b)
	}
	if st.seen[b] {
		return corruptf("inode %d: mapping block %d is used twice", st.in.num, b)
	}
	if f.metaClaimed != nil && f.metaClaimed(b) {
		return corruptf("inode %d: mapping block %d already belongs to other metadata", st.in.num, b)
	}
	st.seen[b] = true
	st.meta = append(st.meta, b)
	return nil
}

func (f *fsys) addExtent(st *mapState, e extent, merge bool) error {
	st.mapped += uint64(e.length)
	if st.mapped > f.blocksCount {
		return corruptf("inode %d maps more blocks than the filesystem has", st.in.num)
	}
	if n := len(st.exts); merge && n > 0 {
		last := &st.exts[n-1]
		if !last.uninit && uint64(last.logical)+uint64(last.length) == uint64(e.logical) &&
			last.end() == e.phys && last.length+e.length <= maxInitExtent {
			last.length += e.length
			return nil
		}
	}
	if len(st.exts) >= maxFileExtents {
		return fmt.Errorf("%w: inode %d has more than %d extents", ErrUnsupported, st.in.num, maxFileExtents)
	}
	st.exts = append(st.exts, e)
	return nil
}

// fileMap returns the data extents of an inode and the metadata blocks of its
// mapping (extent tree nodes or indirect blocks). It validates every pointer
// and, with metadata_csum, every extent block checksum.
func (f *fsys) fileMap(in *inode) ([]extent, []uint64, error) {
	if in.flags()&flagInlineData != 0 {
		return nil, nil, fmt.Errorf("%w: inode %d has inline data", ErrUnsupported, in.num)
	}
	if in.isSymlink() && f.isFastSymlink(in) {
		return nil, nil, nil
	}
	mode := in.mode() & sIFMT
	if mode == sIFCHR || mode == sIFBLK || mode == sIFIFO || mode == sIFSOCK {
		return nil, nil, nil
	}
	st := &mapState{in: in, seen: map[uint64]bool{}}
	var err error
	if in.flags()&flagExtents != 0 {
		err = f.walkExtents(st, in.iblock(), 4, -1, 0, ^uint32(0))
	} else {
		err = f.walkIndirect(st)
	}
	if err != nil {
		return nil, nil, err
	}
	return st.exts, st.meta, nil
}

func (f *fsys) isFastSymlink(in *inode) bool {
	return in.isSymlink() && in.size() > 0 && in.size() < inodeBlockLen && in.flags()&flagExtents == 0 &&
		in.blocks512(f) == f.xattrSectors(in)
}

// xattrSectors is the share of i_blocks taken by the external xattr block.
func (f *fsys) xattrSectors(in *inode) uint64 {
	if in.fileACL(f) != 0 {
		return uint64(f.bs / 512)
	}
	return 0
}

func (f *fsys) walkExtents(st *mapState, node []byte, wantMax int, wantDepth int, lo uint32, hi uint32) error {
	in := st.in
	if le.Uint16(node[0:]) != extentMagic {
		return corruptf("inode %d: bad extent header magic", in.num)
	}
	entries := int(le.Uint16(node[2:]))
	max := int(le.Uint16(node[4:]))
	depth := int(le.Uint16(node[6:]))
	// As the kernel's ext4_ext_check: an index node, and any node below the
	// root, must hold at least one entry.
	if max > wantMax || max == 0 || entries > max || depth > 5 || (wantDepth >= 0 && depth != wantDepth) ||
		entries == 0 && (depth > 0 || wantDepth >= 0) {
		return corruptf("inode %d: bad extent header (entries %d max %d depth %d)", in.num, entries, max, depth)
	}
	prev := int64(-1)
	for i := 0; i < entries; i++ {
		e := node[12+12*i:]
		first := le.Uint32(e[0:])
		if int64(first) <= prev || first < lo || first > hi {
			return corruptf("inode %d: extent entries out of order", in.num)
		}
		prev = int64(first)
		if depth == 0 {
			n := uint32(le.Uint16(e[4:]))
			un := false
			if n > maxInitExtent {
				n -= maxInitExtent
				un = true
			}
			phys := uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))
			if n == 0 || phys < f.firstDataBlock || phys+uint64(n) > f.blocksCount || uint64(first)+uint64(n) > 1<<32 {
				return corruptf("inode %d: extent %d+%d at block %d out of range", in.num, first, n, phys)
			}
			if uint64(first)+uint64(n)-1 > uint64(hi) {
				return corruptf("inode %d: extent overlaps the next index", in.num)
			}
			prev = int64(first) + int64(n) - 1
			if err := f.addExtent(st, extent{logical: first, phys: phys, length: n, uninit: un}, false); err != nil {
				return err
			}
			continue
		}
		child := uint64(le.Uint16(e[8:]))<<32 | uint64(le.Uint32(e[4:]))
		if err := f.visitMeta(st, child); err != nil {
			return err
		}
		nextHi := hi
		if i+1 < entries {
			nextHi = le.Uint32(node[12+12*(i+1):]) - 1
		}
		blk, err := f.readBlock(child)
		if err != nil {
			return err
		}
		if err := f.verifyExtentBlock(in, blk); err != nil {
			return err
		}
		if err := f.walkExtents(st, blk, int((f.bs-12)/12), depth-1, first, nextHi); err != nil {
			return err
		}
	}
	return nil
}

func (f *fsys) extentTailOffset(blk []byte) int { return 12 + 12*int(le.Uint16(blk[4:])) }

func (f *fsys) verifyExtentBlock(in *inode, blk []byte) error {
	if !f.hasMetadataCsum() {
		return nil
	}
	off := f.extentTailOffset(blk)
	if off+4 > len(blk) {
		return corruptf("inode %d: extent block without room for its checksum", in.num)
	}
	want := crc32c(f.inodeSeed(in.num, in.generation()), blk[:off])
	if got := le.Uint32(blk[off:]); got != want {
		return corruptf("inode %d: extent block checksum %#x, computed %#x", in.num, got, want)
	}
	return nil
}

// walkIndirect maps a classic block-mapped inode. Nothing may sit past
// i_size (block-mapped files cannot preallocate), so the work is bounded by
// the inode's size, and every indirect block may be used once only: a
// triple-indirect block whose entries repeat cannot fan out.
func (f *fsys) walkIndirect(st *mapState) error {
	in := st.in
	bs := uint64(f.bs)
	per := bs / 4
	st.limit = min((in.size()+bs-1)/bs, 12+per+per*per+per*per*per)
	add := func(lblk uint64, phys uint64) error {
		if lblk >= st.limit {
			return corruptf("inode %d maps logical block %d past its size %d", in.num, lblk, in.size())
		}
		if phys < f.firstDataBlock || phys >= f.blocksCount {
			return corruptf("inode %d: block pointer %d out of range", in.num, phys)
		}
		return f.addExtent(st, extent{logical: uint32(lblk), phys: phys, length: 1}, true)
	}
	var walk func(blk uint64, level int, base uint64) error
	walk = func(blk uint64, level int, base uint64) error {
		if base >= st.limit {
			return corruptf("inode %d maps logical block %d past its size %d", in.num, base, in.size())
		}
		if err := f.visitMeta(st, blk); err != nil {
			return err
		}
		data, err := f.readBlock(blk)
		if err != nil {
			return err
		}
		span := uint64(1)
		for i := 1; i < level; i++ {
			span *= per
		}
		for i := uint64(0); i < per; i++ {
			p := uint64(le.Uint32(data[4*i:]))
			if p == 0 {
				continue
			}
			if level == 1 {
				if err := add(base+i, p); err != nil {
					return err
				}
			} else if err := walk(p, level-1, base+i*span); err != nil {
				return err
			}
		}
		return nil
	}
	ib := in.iblock()
	for i := 0; i < 12; i++ {
		if p := uint64(le.Uint32(ib[4*i:])); p != 0 {
			if err := add(uint64(i), p); err != nil {
				return err
			}
		}
	}
	base := uint64(12)
	span := per
	for level := 1; level <= 3; level++ {
		if p := uint64(le.Uint32(ib[4*(11+level):])); p != 0 {
			if err := walk(p, level, base); err != nil {
				return err
			}
		}
		base += span
		span *= per
	}
	return nil
}

// writeExtentTree stores exts as the inode's extent tree, allocating tree
// blocks near goal when they do not fit in the inode. It returns the tree
// blocks it allocated. The caller frees the old tree first.
func (f *fsys) writeExtentTree(in *inode, exts []extent, goal uint64) ([]uint64, error) {
	type entry struct {
		first uint32
		raw   [12]byte
	}
	level := make([]entry, 0, len(exts))
	for _, e := range exts {
		if e.length == 0 || e.length > maxInitExtent || e.uninit && e.length > maxInitExtent-1 {
			return nil, fmt.Errorf("ext4: invalid extent length %d", e.length)
		}
		var r [12]byte
		le.PutUint32(r[0:], e.logical)
		n := uint16(e.length)
		if e.uninit {
			n += maxInitExtent
		}
		le.PutUint16(r[4:], n)
		le.PutUint16(r[6:], uint16(e.phys>>32))
		le.PutUint32(r[8:], uint32(e.phys))
		level = append(level, entry{e.logical, r})
	}
	perBlock := int((f.bs - 12) / 12)
	var allocated []uint64
	depth := 0
	for len(level) > 4 {
		nodes := (len(level) + perBlock - 1) / perBlock
		blocks, err := f.allocMeta(goal, nodes)
		if err != nil {
			f.freeList(allocated)
			return nil, err
		}
		allocated = append(allocated, blocks...)
		next := make([]entry, 0, nodes)
		for i := 0; i < nodes; i++ {
			chunk := level[i*perBlock : min((i+1)*perBlock, len(level))]
			blk := make([]byte, f.bs)
			le.PutUint16(blk[0:], extentMagic)
			le.PutUint16(blk[2:], uint16(len(chunk)))
			le.PutUint16(blk[4:], uint16(perBlock))
			le.PutUint16(blk[6:], uint16(depth))
			for j, c := range chunk {
				copy(blk[12+12*j:], c.raw[:])
			}
			if f.hasMetadataCsum() {
				off := f.extentTailOffset(blk)
				le.PutUint32(blk[off:], crc32c(f.inodeSeed(in.num, in.generation()), blk[:off]))
			}
			if err := f.writeBlock(blocks[i], blk); err != nil {
				return nil, err
			}
			var r [12]byte
			le.PutUint32(r[0:], chunk[0].first)
			le.PutUint32(r[4:], uint32(blocks[i]))
			le.PutUint16(r[8:], uint16(blocks[i]>>32))
			next = append(next, entry{chunk[0].first, r})
		}
		level = next
		depth++
	}
	ib := in.iblock()
	clear(ib)
	le.PutUint16(ib[0:], extentMagic)
	le.PutUint16(ib[2:], uint16(len(level)))
	le.PutUint16(ib[4:], 4)
	le.PutUint16(ib[6:], uint16(depth))
	for j, c := range level {
		copy(ib[12+12*j:], c.raw[:])
	}
	in.setFlags(in.flags() | flagExtents)
	return allocated, nil
}

// readAll returns the whole content of a regular file or symlink.
func (f *fsys) readAll(in *inode, limit uint64) ([]byte, error) {
	size := in.size()
	if size > limit {
		return nil, fmt.Errorf("ext4: inode %d is %d bytes, over the %d-byte limit", in.num, size, limit)
	}
	if in.isSymlink() && f.isFastSymlink(in) {
		return append([]byte(nil), in.iblock()[:size]...), nil
	}
	exts, _, err := f.fileMap(in)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	err = f.readRange(in, exts, buf, 0)
	return buf, err
}

// readRange fills buf with file bytes starting at off from a mapping made
// by fileMap; holes and unwritten extents read as zeros.
func (f *fsys) readRange(in *inode, exts []extent, buf []byte, off uint64) error {
	clear(buf)
	end := off + uint64(len(buf))
	bs := uint64(f.bs)
	for _, e := range exts {
		if e.uninit {
			continue
		}
		eStart := uint64(e.logical) * bs
		eEnd := eStart + uint64(e.length)*bs
		if eEnd <= off || eStart >= end {
			continue
		}
		s := max(eStart, off)
		t := min(eEnd, end)
		dev := int64(e.phys*bs + (s - eStart))
		if _, err := f.dev.ReadAt(buf[s-off:t-off], dev); err != nil {
			return fmt.Errorf("ext4: read inode %d data: %w", in.num, err)
		}
	}
	return nil
}
