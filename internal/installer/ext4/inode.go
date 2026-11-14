package ext4

import (
	"fmt"
	"time"
)

// inode is one raw on-disk inode plus its number. Fields are read and
// written in place so unknown bytes (inline xattrs, reserved fields) survive.
type inode struct {
	num uint32
	raw []byte
}

func (f *fsys) inodeLocation(num uint32) (blk uint64, off int64, err error) {
	if num == 0 || num > f.inodesCount {
		return 0, 0, corruptf("inode number %d out of range", num)
	}
	g := int((num - 1) / f.inodesPerGroup)
	idx := uint64((num - 1) % f.inodesPerGroup)
	byteOff := idx * uint64(f.inodeSize)
	blk = f.inodeTableBlock(g) + byteOff/uint64(f.bs)
	return blk, int64(byteOff % uint64(f.bs)), nil
}

func (f *fsys) readInode(num uint32) (*inode, error) {
	blk, off, err := f.inodeLocation(num)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, f.inodeSize)
	if _, err := f.dev.ReadAt(raw, int64(blk)*f.bs+off); err != nil {
		return nil, fmt.Errorf("ext4: read inode %d: %w", num, err)
	}
	in := &inode{num: num, raw: raw}
	if f.hasMetadataCsum() && !in.allZero() {
		if got, want := f.storedInodeCsum(in), f.inodeCsum(in); got != want {
			return nil, corruptf("inode %d checksum %#x, computed %#x", num, got, want)
		}
	}
	return in, nil
}

func (in *inode) allZero() bool {
	for _, c := range in.raw {
		if c != 0 {
			return false
		}
	}
	return true
}

func (f *fsys) writeInode(in *inode) error {
	blk, off, err := f.inodeLocation(in.num)
	if err != nil {
		return err
	}
	if f.hasMetadataCsum() {
		c := f.inodeCsum(in)
		le.PutUint16(in.raw[inChecksumLo:], uint16(c))
		if in.hasExtra(inChecksumHi + 2) {
			le.PutUint16(in.raw[inChecksumHi:], uint16(c>>16))
		}
	}
	if f.wdev == nil {
		return fmt.Errorf("ext4: filesystem is read-only")
	}
	if _, err := f.wdev.WriteAt(in.raw, int64(blk)*f.bs+off); err != nil {
		return fmt.Errorf("ext4: write inode %d: %w", in.num, err)
	}
	return nil
}

// hasExtra reports whether the inode's extra area covers bytes up to end.
func (in *inode) hasExtra(end int) bool {
	if len(in.raw) <= 128 {
		return false
	}
	return 128+int(le.Uint16(in.raw[inExtraIsize:])) >= end
}

// inodeSeed is the per-inode checksum seed used by inode, extent, directory
// and xattr checksums.
func (f *fsys) inodeSeed(num, gen uint32) uint32 {
	c := crc32c(f.csumSeed, u32le(num))
	return crc32c(c, u32le(gen))
}

func (f *fsys) inodeCsum(in *inode) uint32 {
	seed := f.inodeSeed(in.num, in.generation())
	r := in.raw
	c := crc32c(seed, r[:inChecksumLo])
	c = crc32c(c, []byte{0, 0})
	c = crc32c(c, r[inChecksumLo+2:128])
	if len(r) > 128 {
		if in.hasExtra(inChecksumHi + 2) {
			c = crc32c(c, r[128:inChecksumHi])
			c = crc32c(c, []byte{0, 0})
			c = crc32c(c, r[inChecksumHi+2:])
		} else {
			c = crc32c(c, r[128:])
		}
	}
	if !in.hasExtra(inChecksumHi + 2) {
		c &= 0xFFFF
	}
	return c
}

func (f *fsys) storedInodeCsum(in *inode) uint32 {
	c := uint32(le.Uint16(in.raw[inChecksumLo:]))
	if in.hasExtra(inChecksumHi + 2) {
		c |= uint32(le.Uint16(in.raw[inChecksumHi:])) << 16
	}
	return c
}

func (in *inode) mode() uint16       { return le.Uint16(in.raw[inMode:]) }
func (in *inode) setMode(m uint16)   { le.PutUint16(in.raw[inMode:], m) }
func (in *inode) isDir() bool        { return in.mode()&sIFMT == sIFDIR }
func (in *inode) isReg() bool        { return in.mode()&sIFMT == sIFREG }
func (in *inode) isSymlink() bool    { return in.mode()&sIFMT == sIFLNK }
func (in *inode) links() uint16      { return le.Uint16(in.raw[inLinks:]) }
func (in *inode) setLinks(n uint16)  { le.PutUint16(in.raw[inLinks:], n) }
func (in *inode) flags() uint32      { return le.Uint32(in.raw[inFlags:]) }
func (in *inode) setFlags(v uint32)  { le.PutUint32(in.raw[inFlags:], v) }
func (in *inode) generation() uint32 { return le.Uint32(in.raw[inGeneration:]) }
func (in *inode) dtime() uint32      { return le.Uint32(in.raw[inDtime:]) }
func (in *inode) iblock() []byte     { return in.raw[inBlock : inBlock+inodeBlockLen] }
func (in *inode) uid() uint32 {
	return uint32(le.Uint16(in.raw[inUID:])) | uint32(le.Uint16(in.raw[inUIDHi:]))<<16
}
func (in *inode) gid() uint32 {
	return uint32(le.Uint16(in.raw[inGID:])) | uint32(le.Uint16(in.raw[inGIDHi:]))<<16
}

func (in *inode) size() uint64 {
	return uint64(le.Uint32(in.raw[inSizeLo:])) | uint64(le.Uint32(in.raw[inSizeHi:]))<<32
}

func (in *inode) setSize(v uint64) {
	le.PutUint32(in.raw[inSizeLo:], uint32(v))
	le.PutUint32(in.raw[inSizeHi:], uint32(v>>32))
}

func (in *inode) fileACL(f *fsys) uint64 {
	v := uint64(le.Uint32(in.raw[inFileACLLo:]))
	if f.is64() {
		v |= uint64(le.Uint16(in.raw[inFileACLHi:])) << 32
	}
	return v
}

func (in *inode) setFileACL(v uint64) {
	le.PutUint32(in.raw[inFileACLLo:], uint32(v))
	le.PutUint16(in.raw[inFileACLHi:], uint16(v>>32))
}

// blocks512 returns i_blocks in 512-byte units.
func (in *inode) blocks512(f *fsys) uint64 {
	v := uint64(le.Uint32(in.raw[inBlocksLo:]))
	if f.roCompat()&roCompatHugeFile != 0 {
		v |= uint64(le.Uint16(in.raw[inBlocksHi:])) << 32
		if in.flags()&flagHugeFile != 0 {
			v *= uint64(f.bs / 512)
		}
	}
	return v
}

func (in *inode) setBlocks512(f *fsys, v uint64) error {
	if in.flags()&flagHugeFile != 0 {
		return fmt.Errorf("ext4: inode %d uses huge_file block units; refusing to edit", in.num)
	}
	if v>>32 != 0 && (f.roCompat()&roCompatHugeFile == 0 || v>>48 != 0) {
		return fmt.Errorf("ext4: inode %d needs %d sectors, beyond what this filesystem records", in.num, v)
	}
	le.PutUint32(in.raw[inBlocksLo:], uint32(v))
	if f.roCompat()&roCompatHugeFile != 0 {
		le.PutUint16(in.raw[inBlocksHi:], uint16(v>>32))
	}
	return nil
}

func encodeTime(t time.Time) (uint32, uint32) {
	sec := t.Unix()
	extra := uint32((sec-int64(int32(sec)))>>32)&3 | uint32(t.Nanosecond())<<2
	return uint32(sec), extra
}

// setTimes stamps atime, ctime, mtime and (when room) crtime.
func (in *inode) setTimes(t time.Time, crtime bool) {
	s, x := encodeTime(t)
	for _, o := range []int{inAtime, inCtime, inMtime} {
		le.PutUint32(in.raw[o:], s)
	}
	for _, o := range []int{inAtimeExtra, inCtimeExtra, inMtimeExtra} {
		if in.hasExtra(o + 4) {
			le.PutUint32(in.raw[o:], x)
		}
	}
	if crtime && in.hasExtra(inCrtimeExtra+4) {
		le.PutUint32(in.raw[inCrtime:], s)
		le.PutUint32(in.raw[inCrtimeExtra:], x)
	}
}

// touchDir updates a directory's ctime and mtime after an entry change.
func (in *inode) touch(t time.Time) {
	s, x := encodeTime(t)
	for _, o := range []int{inCtime, inMtime} {
		le.PutUint32(in.raw[o:], s)
	}
	for _, o := range []int{inCtimeExtra, inMtimeExtra} {
		if in.hasExtra(o + 4) {
			le.PutUint32(in.raw[o:], x)
		}
	}
}

func (in *inode) mtime() time.Time {
	s := int64(int32(le.Uint32(in.raw[inMtime:])))
	var ns int64
	if in.hasExtra(inMtimeExtra + 4) {
		x := le.Uint32(in.raw[inMtimeExtra:])
		s += int64(x&3) << 32
		ns = int64(x >> 2)
	}
	return time.Unix(s, ns)
}

// newInode builds a zeroed inode with the filesystem's default extra size.
func (f *fsys) newInodeRaw(num uint32, mode uint16, t time.Time) *inode {
	in := &inode{num: num, raw: make([]byte, f.inodeSize)}
	in.setMode(mode)
	if f.inodeSize > 128 {
		extra := le.Uint16(f.sb[sbWantExtraIsize:])
		if extra < 32 {
			extra = 32
		}
		if 128+int(extra) > f.inodeSize {
			extra = uint16(f.inodeSize - 128)
		}
		le.PutUint16(in.raw[inExtraIsize:], extra&^3)
	}
	in.setTimes(t, true)
	return in
}
