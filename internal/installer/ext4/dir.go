package ext4

import (
	"bytes"
	"fmt"
)

// dirent is one parsed directory entry.
type dirent struct {
	blockIdx int // index in the directory's block list
	off      int
	ino      uint32
	recLen   int
	name     []byte
	ftype    uint8
}

// dirBlock is one directory block with its location.
type dirBlock struct {
	logical uint32
	phys    uint64
	data    []byte
	dx      bool // htree root or interior node: no ordinary entries past '.'/'..'
}

const maxDirBytes = 128 << 20

const dxRootLimitOff = 0x20
const dxNodeLimitOff = 0x08

func (f *fsys) dirEnd() int {
	if f.hasMetadataCsum() {
		return int(f.bs) - 12
	}
	return int(f.bs)
}

func (f *fsys) recLen(b []byte) int {
	v := int(le.Uint16(b))
	if f.bs < 65536 {
		return v
	}
	if v == 65535 || v == 0 {
		return int(f.bs)
	}
	return v&65532 | (v&3)<<16
}

func (f *fsys) putRecLen(b []byte, n int) {
	if f.bs >= 65536 && n == int(f.bs) {
		le.PutUint16(b, 65535)
		return
	}
	le.PutUint16(b, uint16(n&65532|n>>16&3))
}

// readDir loads every block of a directory, checking structure and checksums.
func (f *fsys) readDir(dir *inode) ([]dirBlock, error) {
	if !dir.isDir() {
		return nil, fmt.Errorf("ext4: inode %d is not a directory", dir.num)
	}
	// A directory is read whole: check its size before mapping it, so a
	// hostile size cannot force a huge walk or allocation. 128 MiB holds well
	// over a million entries.
	nblocks := dir.size() / uint64(f.bs)
	if dir.size()%uint64(f.bs) != 0 || nblocks == 0 {
		return nil, corruptf("directory inode %d has size %d", dir.num, dir.size())
	}
	if dir.size() > maxDirBytes {
		return nil, fmt.Errorf("%w: directory inode %d is %d bytes", ErrUnsupported, dir.num, dir.size())
	}
	exts, _, err := f.fileMap(dir)
	if err != nil {
		return nil, err
	}
	blocks := make([]dirBlock, 0, nblocks)
	next := uint64(0)
	for _, e := range exts {
		if e.uninit {
			return nil, corruptf("directory inode %d has an unwritten extent", dir.num)
		}
		for i := uint32(0); i < e.length; i++ {
			l := uint64(e.logical) + uint64(i)
			if l >= nblocks {
				return nil, corruptf("directory inode %d maps block %d past its size", dir.num, l)
			}
			if l != next {
				return nil, corruptf("directory inode %d has a hole at block %d", dir.num, next)
			}
			next++
			data, err := f.readBlock(e.phys + uint64(i))
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, dirBlock{logical: uint32(l), phys: e.phys + uint64(i), data: data})
		}
	}
	if next != nblocks {
		return nil, corruptf("directory inode %d maps %d of %d blocks", dir.num, next, nblocks)
	}
	if dir.flags()&flagIndex != 0 {
		if err := f.markDxBlocks(dir, blocks); err != nil {
			return nil, err
		}
	}
	for i := range blocks {
		if blocks[i].dx {
			continue
		}
		if err := f.verifyDirBlock(dir, blocks[i].data); err != nil {
			return nil, fmt.Errorf("directory inode %d block %d: %w", dir.num, i, err)
		}
	}
	return blocks, nil
}

// markDxBlocks finds the htree root and interior nodes and verifies their
// checksums. Leaf blocks are ordinary directory blocks.
func (f *fsys) markDxBlocks(dir *inode, blocks []dirBlock) error {
	root := blocks[0].data
	// '.' then '..' covering the rest of the block, then dx_root_info.
	if f.recLen(root[4:]) != 12 || f.recLen(root[12+4:]) != int(f.bs)-12 {
		return corruptf("directory inode %d: bad htree root", dir.num)
	}
	if root[0x18+5] != 8 || le.Uint32(root[0x18:]) != 0 {
		return corruptf("directory inode %d: bad htree root info", dir.num)
	}
	levels := int(root[0x18+6])
	if levels > 2 {
		return fmt.Errorf("%w: directory inode %d has a %d-level htree", ErrUnsupported, dir.num, levels+1)
	}
	if err := f.verifyDxBlock(dir, root, dxRootLimitOff); err != nil {
		return err
	}
	blocks[0].dx = true
	var mark func(blk []byte, off int, level int) error
	mark = func(blk []byte, off int, level int) error {
		count := int(le.Uint16(blk[off+2:]))
		for i := 0; i < count; i++ {
			var l uint32
			if i == 0 {
				l = le.Uint32(blk[off+4:])
			} else {
				l = le.Uint32(blk[off+8*i+4:])
			}
			l &= 0x0FFFFFFF
			if int(l) >= len(blocks) || l == 0 {
				return corruptf("directory inode %d: htree points at block %d", dir.num, l)
			}
			if level == 0 {
				continue
			}
			node := blocks[l].data
			if le.Uint32(node[0:]) != 0 || f.recLen(node[4:]) != int(f.bs) {
				return corruptf("directory inode %d: bad htree node %d", dir.num, l)
			}
			if err := f.verifyDxBlock(dir, node, dxNodeLimitOff); err != nil {
				return err
			}
			blocks[l].dx = true
			if err := mark(node, dxNodeLimitOff, level-1); err != nil {
				return err
			}
		}
		return nil
	}
	return mark(root, dxRootLimitOff, levels)
}

func (f *fsys) dxLimit(off int) int {
	n := (int(f.bs) - off) / 8
	if f.hasMetadataCsum() {
		n--
	}
	return n
}

func (f *fsys) verifyDxBlock(dir *inode, blk []byte, off int) error {
	limit := int(le.Uint16(blk[off:]))
	count := int(le.Uint16(blk[off+2:]))
	if limit != f.dxLimit(off) || count == 0 || count > limit {
		return corruptf("directory inode %d: htree node limit %d count %d", dir.num, limit, count)
	}
	if !f.hasMetadataCsum() {
		return nil
	}
	tail := off + limit*8
	c := crc32c(f.inodeSeed(dir.num, dir.generation()), blk[:off+count*8])
	c = crc32c(c, blk[tail:tail+4])
	c = crc32c(c, []byte{0, 0, 0, 0}) // dt_checksum itself, as zero
	if got := le.Uint32(blk[tail+4:]); got != c {
		return corruptf("directory inode %d: htree node checksum %#x, computed %#x", dir.num, got, c)
	}
	return nil
}

func (f *fsys) verifyDirBlock(dir *inode, blk []byte) error {
	end := f.dirEnd()
	if f.hasMetadataCsum() {
		t := blk[end:]
		if le.Uint32(t[0:]) != 0 || le.Uint16(t[4:]) != 12 || t[6] != 0 || t[7] != ftCsum {
			return corruptf("missing directory checksum tail")
		}
		want := crc32c(f.inodeSeed(dir.num, dir.generation()), blk[:end])
		if got := le.Uint32(t[8:]); got != want {
			return corruptf("directory block checksum %#x, computed %#x", got, want)
		}
	}
	_, err := f.parseEntries(blk, end, 0)
	return err
}

func (f *fsys) parseEntries(blk []byte, end int, idx int) ([]dirent, error) {
	var out []dirent
	for off := 0; off < end; {
		if off+8 > end {
			return nil, corruptf("directory entry header past the block end")
		}
		rl := f.recLen(blk[off+4:])
		nl := int(blk[off+6])
		if rl < 8 || rl%4 != 0 || off+rl > end || (le.Uint32(blk[off:]) != 0 && direntLen(nl) > rl) {
			return nil, corruptf("bad directory entry at offset %d (rec_len %d, name_len %d)", off, rl, nl)
		}
		ino := le.Uint32(blk[off:])
		if ino != 0 {
			out = append(out, dirent{blockIdx: idx, off: off, ino: ino, recLen: rl,
				name: blk[off+8 : off+8+nl], ftype: blk[off+7]})
		} else {
			out = append(out, dirent{blockIdx: idx, off: off, recLen: rl, ftype: blk[off+7]})
		}
		off += rl
	}
	return out, nil
}

// lookup finds name in dir. Names in an encrypted directory are ciphertext,
// so a plaintext lookup there is refused rather than reported as missing.
func (f *fsys) lookup(dir *inode, name string) (uint32, uint8, error) {
	if dir.flags()&flagEncrypt != 0 {
		return 0, 0, fmt.Errorf("%w: directory inode %d is encrypted; its names cannot be looked up", ErrUnsupported, dir.num)
	}
	blocks, err := f.readDir(dir)
	if err != nil {
		return 0, 0, err
	}
	ents, err := f.listEntries(blocks)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if string(e.name) == name {
			return e.ino, e.ftype, nil
		}
	}
	return 0, 0, nil
}

// listEntries returns all live entries; for an htree root it returns '.'
// and '..' read directly.
func (f *fsys) listEntries(blocks []dirBlock) ([]dirent, error) {
	var out []dirent
	for i, b := range blocks {
		if b.dx {
			if i == 0 {
				d := b.data
				out = append(out,
					dirent{blockIdx: 0, off: 0, ino: le.Uint32(d[0:]), recLen: 12, name: d[8 : 8+int(d[6])], ftype: d[7]},
					dirent{blockIdx: 0, off: 12, ino: le.Uint32(d[12:]), recLen: int(f.bs) - 12, name: d[20 : 20+int(d[18])], ftype: d[19]})
			}
			continue
		}
		ents, err := f.parseEntries(b.data, f.dirEnd(), i)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.ino != 0 {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

func (f *fsys) sealDirBlock(dir *inode, blk []byte) {
	if !f.hasMetadataCsum() {
		return
	}
	end := f.dirEnd()
	t := blk[end:]
	clear(t)
	le.PutUint16(t[4:], 12)
	t[7] = ftCsum
	le.PutUint32(t[8:], crc32c(f.inodeSeed(dir.num, dir.generation()), blk[:end]))
}

// linearize turns an htree directory into a plain linear one: the root and
// interior nodes become ordinary blocks holding only '.'/'..' or nothing,
// and the index flag is cleared. Leaf blocks are unchanged. This is a valid
// ext4 directory; the kernel keeps using it linearly.
func (f *fsys) linearize(dir *inode, blocks []dirBlock) error {
	end := f.dirEnd()
	for i := range blocks {
		b := &blocks[i]
		if !b.dx {
			continue
		}
		d := b.data
		if i == 0 {
			clear(d[24:])
			f.putRecLen(d[12+4:], end-12)
		} else {
			clear(d)
			f.putRecLen(d[4:], end)
		}
		f.sealDirBlock(dir, d)
		if err := f.writeBlock(b.phys, d); err != nil {
			return err
		}
		b.dx = false
	}
	dir.setFlags(dir.flags() &^ flagIndex)
	return f.writeInode(dir)
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || bytes.ContainsAny([]byte(name), "/\x00") {
		return fmt.Errorf("ext4: invalid file name %q", name)
	}
	return nil
}

// addEntry links name -> ino (of type ft) into dir.
func (f *fsys) addEntry(dir *inode, name string, ino uint32, ft uint8) error {
	if err := validName(name); err != nil {
		return err
	}
	if dir.flags()&(flagEncrypt|flagCasefold) != 0 {
		return fmt.Errorf("%w: directory inode %d is encrypted or case-folded", ErrUnsupported, dir.num)
	}
	blocks, err := f.readDir(dir)
	if err != nil {
		return err
	}
	if dir.flags()&flagIndex != 0 {
		if err := f.linearize(dir, blocks); err != nil {
			return err
		}
	}
	need := direntLen(len(name))
	end := f.dirEnd()
	put := func(d []byte, off, rl int) {
		le.PutUint32(d[off:], ino)
		f.putRecLen(d[off+4:], rl)
		d[off+6] = byte(len(name))
		d[off+7] = ft
		copy(d[off+8:], name)
		clear(d[off+8+len(name) : off+rl])
	}
	for i := range blocks {
		b := &blocks[i]
		ents, err := f.parseEntries(b.data, end, i)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if e.ino == 0 && e.recLen >= need {
				put(b.data, e.off, e.recLen)
			} else if e.ino != 0 && e.recLen-direntLen(len(e.name)) >= need {
				used := direntLen(len(e.name))
				f.putRecLen(b.data[e.off+4:], used)
				put(b.data, e.off+used, e.recLen-used)
			} else {
				continue
			}
			f.sealDirBlock(dir, b.data)
			if err := f.writeBlock(b.phys, b.data); err != nil {
				return err
			}
			dir.touch(f.now())
			return f.writeInode(dir)
		}
	}
	// No room: append a block.
	d := make([]byte, f.bs)
	put(d, 0, end)
	f.sealDirBlock(dir, d)
	return f.appendBlock(dir, d)
}

// appendBlock adds one data block at the end of a file (used for
// directories) and writes data into it.
func (f *fsys) appendBlock(in *inode, data []byte) error {
	exts, meta, err := f.fileMap(in)
	if err != nil {
		return err
	}
	if in.flags()&flagExtents == 0 {
		return fmt.Errorf("%w: inode %d uses block maps; refusing to extend it", ErrUnsupported, in.num)
	}
	logical := uint32(in.size() / uint64(f.bs))
	goal := f.goalFor(in.num)
	if n := len(exts); n > 0 {
		goal = exts[n-1].end()
	}
	ne, err := f.allocData(goal, 1, logical)
	if err != nil {
		return err
	}
	if err := f.writeBlock(ne[0].phys, data); err != nil {
		return err
	}
	if n := len(exts); n > 0 && !exts[n-1].uninit && exts[n-1].end() == ne[0].phys &&
		uint64(exts[n-1].logical)+uint64(exts[n-1].length) == uint64(logical) && exts[n-1].length < maxInitExtent {
		exts[n-1].length++
	} else {
		exts = append(exts, ne[0])
	}
	newMeta, err := f.rewriteTree(in, exts, meta, goal)
	if err != nil {
		return err
	}
	// The old tree blocks were freed and the new ones allocated: i_blocks
	// loses the first and gains the second plus the appended block.
	spb := uint64(f.bs / 512)
	sectors := in.blocks512(f)
	if old := uint64(len(meta)) * spb; sectors >= old {
		sectors -= old
	} else {
		return corruptf("inode %d records %d sectors but its tree alone has %d", in.num, sectors, old)
	}
	sectors += (1 + uint64(len(newMeta))) * spb
	if err := in.setBlocks512(f, sectors); err != nil {
		return err
	}
	in.setSize(in.size() + uint64(f.bs))
	in.touch(f.now())
	return f.writeInode(in)
}

// rewriteTree replaces an inode's extent tree, freeing the old tree blocks.
func (f *fsys) rewriteTree(in *inode, exts []extent, oldMeta []uint64, goal uint64) ([]uint64, error) {
	if len(exts) <= 4 && len(oldMeta) == 0 {
		return f.writeExtentTree(in, exts, goal)
	}
	for _, b := range oldMeta {
		if err := f.freeBlocks(b, 1); err != nil {
			return nil, err
		}
	}
	return f.writeExtentTree(in, exts, goal)
}

// removeEntry unlinks name from dir and returns the inode it pointed at.
func (f *fsys) removeEntry(dir *inode, name string) (uint32, error) {
	if dir.flags()&(flagEncrypt|flagCasefold) != 0 {
		return 0, fmt.Errorf("%w: directory inode %d is encrypted or case-folded", ErrUnsupported, dir.num)
	}
	blocks, err := f.readDir(dir)
	if err != nil {
		return 0, err
	}
	if dir.flags()&flagIndex != 0 {
		if err := f.linearize(dir, blocks); err != nil {
			return 0, err
		}
	}
	end := f.dirEnd()
	for i := range blocks {
		b := &blocks[i]
		ents, err := f.parseEntries(b.data, end, i)
		if err != nil {
			return 0, err
		}
		for k, e := range ents {
			if e.ino == 0 || string(e.name) != name {
				continue
			}
			if k > 0 {
				p := ents[k-1]
				f.putRecLen(b.data[p.off+4:], p.recLen+e.recLen)
				clear(b.data[e.off : e.off+e.recLen])
			} else {
				le.PutUint32(b.data[e.off:], 0)
			}
			f.sealDirBlock(dir, b.data)
			if err := f.writeBlock(b.phys, b.data); err != nil {
				return 0, err
			}
			dir.touch(f.now())
			return e.ino, f.writeInode(dir)
		}
	}
	return 0, nil
}

// newDirBlock builds the first block of a new directory.
func (f *fsys) newDirBlock(dir *inode, parent uint32) []byte {
	d := make([]byte, f.bs)
	le.PutUint32(d[0:], dir.num)
	f.putRecLen(d[4:], 12)
	d[6] = 1
	d[7] = ftDir
	d[8] = '.'
	le.PutUint32(d[12:], parent)
	f.putRecLen(d[16:], f.dirEnd()-12)
	d[18] = 2
	d[19] = ftDir
	d[20], d[21] = '.', '.'
	f.sealDirBlock(dir, d)
	return d
}

// emptyDirBlock is an extra (empty) directory block, as lost+found uses.
func (f *fsys) emptyDirBlock(dir *inode) []byte {
	d := make([]byte, f.bs)
	f.putRecLen(d[4:], f.dirEnd())
	f.sealDirBlock(dir, d)
	return d
}
