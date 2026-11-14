package ext4

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"time"
)

// device is the byte store under a filesystem. A read-only filesystem only
// needs io.ReaderAt; mutation additionally needs io.WriterAt.
type device interface {
	io.ReaderAt
}

type writableDevice interface {
	io.ReaderAt
	io.WriterAt
}

// fsys is an open ext4 filesystem. All raw on-disk structures are kept as
// byte slices and edited field by field, so bytes this package does not
// interpret are preserved exactly.
type fsys struct {
	dev  device
	wdev writableDevice // nil when read-only
	size int64

	sb  []byte // primary superblock, superblockSize bytes
	bs  int64  // block size
	gd  [][]byte
	now func() time.Time

	blocksCount    uint64
	firstDataBlock uint64
	blocksPerGroup uint64
	inodesPerGroup uint32
	inodesCount    uint32
	inodeSize      int
	descSize       int
	groups         int
	gdtBlocks      uint64
	csumSeed       uint32
	firstIno       uint32

	blockBitmaps [][]byte
	inodeBitmaps [][]byte
	dirtyBB      map[int]bool
	dirtyIB      map[int]bool
	dirtyGD      map[int]bool
	dirtySB      bool

	// metaClaimed, when set by the checker, reports blocks already owned by
	// fixed metadata or another inode, so a mapping walk stops at the first
	// shared block instead of re-walking someone else's tree.
	metaClaimed func(uint64) bool
}

func (f *fsys) compat() uint32   { return le.Uint32(f.sb[sbFeatureCompat:]) }
func (f *fsys) incompat() uint32 { return le.Uint32(f.sb[sbFeatureIncompat:]) }
func (f *fsys) roCompat() uint32 { return le.Uint32(f.sb[sbFeatureROCompat:]) }

func (f *fsys) hasMetadataCsum() bool { return f.roCompat()&roCompatMetadataCsum != 0 }
func (f *fsys) hasGdtCsum() bool      { return f.roCompat()&roCompatGdtCsum != 0 }
func (f *fsys) hasGroupCsum() bool    { return f.hasMetadataCsum() || f.hasGdtCsum() }
func (f *fsys) is64() bool            { return f.incompat()&incompat64bit != 0 }
func (f *fsys) hasFiletype() bool     { return f.incompat()&incompatFiletype != 0 }
func (f *fsys) hasExtents() bool      { return f.incompat()&incompatExtents != 0 }

// errCorrupt marks structural damage found while reading.
var errCorrupt = errors.New("ext4: filesystem is corrupt")

func corruptf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errCorrupt}, a...)...)
}

// openFS parses the superblock and group descriptors. It validates every
// checksum of the structures it loads and refuses features it cannot handle.
func openFS(dev device, size int64, writable bool) (*fsys, error) {
	f := &fsys{dev: dev, size: size, now: time.Now,
		dirtyBB: map[int]bool{}, dirtyIB: map[int]bool{}, dirtyGD: map[int]bool{}}
	if writable {
		w, ok := dev.(writableDevice)
		if !ok {
			return nil, errors.New("ext4: device is not writable")
		}
		f.wdev = w
	}
	if size < superblockOffset+superblockSize {
		return nil, fmt.Errorf("%w: image of %d bytes is too small for ext4", ErrNotExt4, size)
	}
	f.sb = make([]byte, superblockSize)
	if _, err := dev.ReadAt(f.sb, superblockOffset); err != nil {
		return nil, fmt.Errorf("ext4: read superblock: %w", err)
	}
	if le.Uint16(f.sb[sbMagicOff:]) != sbMagic {
		return nil, fmt.Errorf("%w: no ext2/3/4 superblock magic", ErrNotExt4)
	}
	if err := f.checkFeatures(); err != nil {
		return nil, err
	}
	if f.hasMetadataCsum() {
		if f.sb[sbChecksumType] != 1 {
			return nil, fmt.Errorf("%w: unknown checksum type %d", ErrUnsupported, f.sb[sbChecksumType])
		}
		if got, want := le.Uint32(f.sb[sbChecksum:]), crc32c(^uint32(0), f.sb[:sbChecksum]); got != want {
			return nil, corruptf("superblock checksum %#08x, computed %#08x", got, want)
		}
	}
	logBS := le.Uint32(f.sb[sbLogBlockSize:])
	if logBS > 6 {
		return nil, corruptf("block size 2^%d KiB", logBS)
	}
	f.bs = 1024 << logBS
	if le.Uint32(f.sb[sbLogClusterSize:]) != logBS {
		return nil, fmt.Errorf("%w: cluster size differs from block size (bigalloc)", ErrUnsupported)
	}
	f.blocksCount = uint64(le.Uint32(f.sb[sbBlocksCountLo:]))
	if f.is64() {
		f.blocksCount |= uint64(le.Uint32(f.sb[sbBlocksCountHi:])) << 32
	}
	f.firstDataBlock = uint64(le.Uint32(f.sb[sbFirstDataBlock:]))
	f.blocksPerGroup = uint64(le.Uint32(f.sb[sbBlocksPerGroup:]))
	f.inodesPerGroup = le.Uint32(f.sb[sbInodesPerGroup:])
	f.inodesCount = le.Uint32(f.sb[sbInodesCount:])
	if le.Uint32(f.sb[sbRevLevel:]) == 0 {
		return nil, fmt.Errorf("%w: revision 0 filesystem", ErrUnsupported)
	}
	f.inodeSize = int(le.Uint16(f.sb[sbInodeSize:]))
	f.firstIno = le.Uint32(f.sb[sbFirstIno:])
	f.descSize = 32
	if f.is64() {
		f.descSize = int(le.Uint16(f.sb[sbDescSize:]))
		if f.descSize < 64 || f.descSize > 1024 || f.descSize&(f.descSize-1) != 0 {
			return nil, corruptf("descriptor size %d", f.descSize)
		}
	}
	switch {
	case f.inodeSize < 128 || f.inodeSize > int(f.bs) || f.inodeSize&(f.inodeSize-1) != 0:
		return nil, corruptf("inode size %d", f.inodeSize)
	case f.blocksPerGroup == 0 || f.blocksPerGroup > uint64(f.bs)*8:
		return nil, corruptf("%d blocks per group", f.blocksPerGroup)
	case f.inodesPerGroup == 0 || f.inodesPerGroup > uint32(f.bs)*8 || f.inodesPerGroup%uint32(f.bs/int64(f.inodeSize)) != 0:
		return nil, corruptf("%d inodes per group", f.inodesPerGroup)
	case f.blocksCount <= f.firstDataBlock:
		return nil, corruptf("%d blocks", f.blocksCount)
	case (f.bs == 1024) != (f.firstDataBlock == 1):
		return nil, corruptf("first data block %d with %d-byte blocks", f.firstDataBlock, f.bs)
	case f.firstIno < firstGoodIno || f.firstIno >= f.inodesCount:
		return nil, corruptf("first inode %d", f.firstIno)
	}
	if uint64(f.blocksCount)*uint64(f.bs) > uint64(size) {
		return nil, fmt.Errorf("%w: filesystem of %d bytes is larger than the %d-byte image (truncated?)",
			ErrCorrupt, uint64(f.blocksCount)*uint64(f.bs), size)
	}
	groups := (f.blocksCount - f.firstDataBlock + f.blocksPerGroup - 1) / f.blocksPerGroup
	if groups == 0 || groups > 1<<24 {
		return nil, corruptf("%d groups", groups)
	}
	f.groups = int(groups)
	if uint64(f.inodesCount) != uint64(f.groups)*uint64(f.inodesPerGroup) {
		return nil, corruptf("inode count %d is not %d groups x %d", f.inodesCount, f.groups, f.inodesPerGroup)
	}
	f.gdtBlocks = (uint64(f.groups)*uint64(f.descSize) + uint64(f.bs) - 1) / uint64(f.bs)
	if f.incompat()&incompatCsumSeed != 0 {
		f.csumSeed = le.Uint32(f.sb[sbChecksumSeed:])
	} else {
		f.csumSeed = crc32c(^uint32(0), f.sb[sbUUID:sbUUID+16])
	}
	gdt := make([]byte, f.gdtBlocks*uint64(f.bs))
	if _, err := dev.ReadAt(gdt, int64(f.firstDataBlock+1)*f.bs); err != nil {
		return nil, fmt.Errorf("ext4: read group descriptors: %w", err)
	}
	f.gd = make([][]byte, f.groups)
	for g := range f.gd {
		f.gd[g] = gdt[g*f.descSize : (g+1)*f.descSize : (g+1)*f.descSize]
		if f.hasGroupCsum() {
			if got, want := le.Uint16(f.gd[g][gdChecksum:]), f.groupCsum(g); got != want {
				return nil, corruptf("group %d descriptor checksum %#04x, computed %#04x", g, got, want)
			}
		}
		for _, b := range []uint64{f.blockBitmapBlock(g), f.inodeBitmapBlock(g)} {
			if b < f.firstDataBlock || b >= f.blocksCount {
				return nil, corruptf("group %d bitmap at block %d", g, b)
			}
		}
		if it := f.inodeTableBlock(g); it < f.firstDataBlock || it+f.itableBlocks() > f.blocksCount {
			return nil, corruptf("group %d inode table at block %d", g, it)
		}
	}
	f.blockBitmaps = make([][]byte, f.groups)
	f.inodeBitmaps = make([][]byte, f.groups)
	return f, nil
}

func (f *fsys) checkFeatures() error {
	c, i, r := f.compat(), f.incompat(), f.roCompat()
	if i&incompatRecover != 0 {
		return fmt.Errorf("%w: the journal needs recovery (filesystem was not cleanly unmounted)", ErrDirty)
	}
	if r&roCompatOrphanPresent != 0 {
		return fmt.Errorf("%w: orphan inodes are pending", ErrDirty)
	}
	var bad []string
	if x := c &^ supportedCompat; x != 0 {
		bad = append(bad, fmt.Sprintf("compat %#x", x))
	}
	if x := i &^ supportedIncompat; x != 0 {
		bad = append(bad, fmt.Sprintf("incompat %#x", x))
	}
	if x := r &^ supportedROCompat; x != 0 {
		bad = append(bad, fmt.Sprintf("ro_compat %#x", x))
	}
	if len(bad) != 0 {
		return fmt.Errorf("%w: unsupported features %v", ErrUnsupported, bad)
	}
	if i&incompatFiletype == 0 {
		return fmt.Errorf("%w: directory entries without file types", ErrUnsupported)
	}
	if i&incompatExtents == 0 {
		return fmt.Errorf("%w: not an extent-based (ext4) filesystem", ErrUnsupported)
	}
	if r&roCompatMetadataCsum != 0 && r&roCompatGdtCsum != 0 {
		return corruptf("both metadata_csum and gdt_csum are set")
	}
	return nil
}

// Group geometry.

func (f *fsys) groupFirstBlock(g int) uint64 {
	return f.firstDataBlock + uint64(g)*f.blocksPerGroup
}

func (f *fsys) groupBlocks(g int) uint64 {
	if g == f.groups-1 {
		return f.blocksCount - f.groupFirstBlock(g)
	}
	return f.blocksPerGroup
}

func (f *fsys) groupOf(b uint64) int { return int((b - f.firstDataBlock) / f.blocksPerGroup) }

func (f *fsys) itableBlocks() uint64 {
	return (uint64(f.inodesPerGroup)*uint64(f.inodeSize) + uint64(f.bs) - 1) / uint64(f.bs)
}

// hasSuper reports whether group g holds a superblock and descriptor backup.
func (f *fsys) hasSuper(g int) bool {
	if g == 0 {
		return true
	}
	if f.compat()&compatSparseSuper2 != 0 {
		return uint32(g) == le.Uint32(f.sb[sbBackupBgs:]) || uint32(g) == le.Uint32(f.sb[sbBackupBgs+4:])
	}
	if f.roCompat()&roCompatSparseSuper == 0 || g == 1 {
		return true
	}
	if g&1 == 0 {
		return false
	}
	return isPower(g, 3) || isPower(g, 5) || isPower(g, 7)
}

func isPower(n, base int) bool {
	for n > 1 && n%base == 0 {
		n /= base
	}
	return n == 1
}

// baseMetaBlocks is the number of blocks at the start of group g used by the
// superblock backup, descriptors and reserved descriptor blocks.
func (f *fsys) baseMetaBlocks(g int) uint64 {
	if !f.hasSuper(g) {
		return 0
	}
	n := 1 + f.gdtBlocks
	if f.compat()&compatResizeInode != 0 {
		n += uint64(le.Uint16(f.sb[sbReservedGDTBlocks:]))
	}
	if g == 0 && f.firstDataBlock == 0 && f.bs == 1024 {
		n++ // cannot happen (1 KiB blocks start at block 1), kept for clarity
	}
	return n
}

// Group descriptor accessors.

func (f *fsys) gdGet(g int, lo, hi int, width int) uint64 {
	d := f.gd[g]
	var v uint64
	if width == 32 {
		v = uint64(le.Uint32(d[lo:]))
		if f.descSize >= 64 {
			v |= uint64(le.Uint32(d[hi:])) << 32
		}
	} else {
		v = uint64(le.Uint16(d[lo:]))
		if f.descSize >= 64 {
			v |= uint64(le.Uint16(d[hi:])) << 16
		}
	}
	return v
}

func (f *fsys) gdSet(g int, lo, hi int, width int, v uint64) {
	d := f.gd[g]
	if width == 32 {
		le.PutUint32(d[lo:], uint32(v))
		if f.descSize >= 64 {
			le.PutUint32(d[hi:], uint32(v>>32))
		}
	} else {
		le.PutUint16(d[lo:], uint16(v))
		if f.descSize >= 64 {
			le.PutUint16(d[hi:], uint16(v>>16))
		}
	}
	f.dirtyGD[g] = true
}

func (f *fsys) blockBitmapBlock(g int) uint64 {
	return f.gdGet(g, gdBlockBitmapLo, gdBlockBitmapHi, 32)
}
func (f *fsys) inodeBitmapBlock(g int) uint64 {
	return f.gdGet(g, gdInodeBitmapLo, gdInodeBitmapHi, 32)
}
func (f *fsys) inodeTableBlock(g int) uint64 { return f.gdGet(g, gdInodeTableLo, gdInodeTableHi, 32) }
func (f *fsys) gdFreeBlocks(g int) uint64    { return f.gdGet(g, gdFreeBlocksLo, gdFreeBlocksHi, 16) }
func (f *fsys) gdFreeInodes(g int) uint64    { return f.gdGet(g, gdFreeInodesLo, gdFreeInodesHi, 16) }
func (f *fsys) gdUsedDirs(g int) uint64      { return f.gdGet(g, gdUsedDirsLo, gdUsedDirsHi, 16) }
func (f *fsys) gdItableUnused(g int) uint64 {
	return f.gdGet(g, gdItableUnusedLo, gdItableUnusedHi, 16)
}
func (f *fsys) gdFlags(g int) uint16 { return le.Uint16(f.gd[g][gdFlags:]) }
func (f *fsys) setGdFlags(g int, v uint16) {
	le.PutUint16(f.gd[g][gdFlags:], v)
	f.dirtyGD[g] = true
}

// groupCsum computes bg_checksum for group g.
func (f *fsys) groupCsum(g int) uint16 {
	d := f.gd[g]
	if f.hasMetadataCsum() {
		c := crc32c(f.csumSeed, u32le(uint32(g)))
		c = crc32c(c, d[:gdChecksum])
		c = crc32c(c, []byte{0, 0})
		if f.descSize > gdChecksum+2 {
			c = crc32c(c, d[gdChecksum+2:])
		}
		return uint16(c)
	}
	c := crc16(^uint16(0), f.sb[sbUUID:sbUUID+16])
	c = crc16(c, u32le(uint32(g)))
	c = crc16(c, d[:gdChecksum])
	if f.is64() && f.descSize > gdChecksum+2 {
		c = crc16(c, d[gdChecksum+2:])
	}
	return c
}

// Superblock accessors.

func (f *fsys) sbFreeBlocks() uint64 {
	v := uint64(le.Uint32(f.sb[sbFreeBlocksLo:]))
	if f.is64() {
		v |= uint64(le.Uint32(f.sb[sbFreeBlocksHi:])) << 32
	}
	return v
}

func (f *fsys) setSbFreeBlocks(v uint64) {
	le.PutUint32(f.sb[sbFreeBlocksLo:], uint32(v))
	if f.is64() {
		le.PutUint32(f.sb[sbFreeBlocksHi:], uint32(v>>32))
	}
	f.dirtySB = true
}

func (f *fsys) sbFreeInodes() uint32 { return le.Uint32(f.sb[sbFreeInodes:]) }
func (f *fsys) setSbFreeInodes(v uint32) {
	le.PutUint32(f.sb[sbFreeInodes:], v)
	f.dirtySB = true
}

// Block I/O.

func (f *fsys) readBlock(b uint64) ([]byte, error) {
	if b >= f.blocksCount {
		return nil, corruptf("block %d beyond the end (%d blocks)", b, f.blocksCount)
	}
	buf := make([]byte, f.bs)
	if _, err := f.dev.ReadAt(buf, int64(b)*f.bs); err != nil {
		return nil, fmt.Errorf("ext4: read block %d: %w", b, err)
	}
	return buf, nil
}

func (f *fsys) writeBlock(b uint64, data []byte) error {
	if f.wdev == nil {
		return errors.New("ext4: filesystem is read-only")
	}
	if b >= f.blocksCount || b < f.firstDataBlock && !(b == 0 && f.bs > 1024) {
		return fmt.Errorf("ext4: refusing to write block %d outside the filesystem", b)
	}
	if int64(len(data)) != f.bs {
		return fmt.Errorf("ext4: write of %d bytes to block %d", len(data), b)
	}
	_, err := f.wdev.WriteAt(data, int64(b)*f.bs)
	if err != nil {
		return fmt.Errorf("ext4: write block %d: %w", b, err)
	}
	return nil
}

// Bitmaps. Bits for blocks or inodes past the end of a group are padding
// and always set, as e2fsck expects.

func (f *fsys) blockBitmap(g int) ([]byte, error) {
	if f.blockBitmaps[g] != nil {
		return f.blockBitmaps[g], nil
	}
	var bm []byte
	if f.hasGroupCsum() && f.gdFlags(g)&bgBlockUninit != 0 {
		bm = make([]byte, f.bs)
		f.initUninitBlockBitmap(g, bm)
	} else {
		var err error
		if bm, err = f.readBlock(f.blockBitmapBlock(g)); err != nil {
			return nil, err
		}
		if f.hasMetadataCsum() {
			got := f.gdGet(g, gdBlockBitmapCsumLo, gdBlockBitmapCsumHi, 16)
			want := f.blockBitmapCsum(bm)
			if f.descSize < 64 {
				want &= 0xFFFF
			}
			if got != want {
				return nil, corruptf("group %d block bitmap checksum %#x, computed %#x", g, got, want)
			}
		}
	}
	for i := f.groupBlocks(g); i < uint64(f.bs)*8; i++ {
		setBit(bm, i)
	}
	f.blockBitmaps[g] = bm
	return bm, nil
}

// initUninitBlockBitmap builds the implied bitmap of a BLOCK_UNINIT group as
// the kernel's ext4_init_block_bitmap does.
func (f *fsys) initUninitBlockBitmap(g int, bm []byte) {
	for i := uint64(0); i < f.baseMetaBlocks(g); i++ {
		setBit(bm, i)
	}
	start := f.groupFirstBlock(g)
	mark := func(b, n uint64) {
		for i := uint64(0); i < n; i++ {
			if x := b + i; x >= start && x < start+f.groupBlocks(g) {
				setBit(bm, x-start)
			}
		}
	}
	mark(f.blockBitmapBlock(g), 1)
	mark(f.inodeBitmapBlock(g), 1)
	mark(f.inodeTableBlock(g), f.itableBlocks())
}

func (f *fsys) blockBitmapCsum(bm []byte) uint64 {
	n := f.blocksPerGroup / 8
	return uint64(crc32c(f.csumSeed, bm[:n]))
}

func (f *fsys) inodeBitmapCsum(bm []byte) uint64 {
	n := f.inodesPerGroup / 8
	return uint64(crc32c(f.csumSeed, bm[:n]))
}

func (f *fsys) inodeBitmap(g int) ([]byte, error) {
	if f.inodeBitmaps[g] != nil {
		return f.inodeBitmaps[g], nil
	}
	var bm []byte
	if f.hasGroupCsum() && f.gdFlags(g)&bgInodeUninit != 0 {
		bm = make([]byte, f.bs)
	} else {
		var err error
		if bm, err = f.readBlock(f.inodeBitmapBlock(g)); err != nil {
			return nil, err
		}
		if f.hasMetadataCsum() {
			got := f.gdGet(g, gdInodeBitmapCsumLo, gdInodeBitmapCsumHi, 16)
			want := f.inodeBitmapCsum(bm)
			if f.descSize < 64 {
				want &= 0xFFFF
			}
			if got != want {
				return nil, corruptf("group %d inode bitmap checksum %#x, computed %#x", g, got, want)
			}
		}
	}
	for i := uint64(f.inodesPerGroup); i < uint64(f.bs)*8; i++ {
		setBit(bm, i)
	}
	f.inodeBitmaps[g] = bm
	return bm, nil
}

func testBit(bm []byte, i uint64) bool { return bm[i>>3]&(1<<(i&7)) != 0 }
func setBit(bm []byte, i uint64)       { bm[i>>3] |= 1 << (i & 7) }
func clearBit(bm []byte, i uint64)     { bm[i>>3] &^= 1 << (i & 7) }

func countZeroBits(bm []byte, n uint64) uint64 {
	var used uint64
	full := n / 8
	for _, c := range bm[:full] {
		used += uint64(bits.OnesCount8(c))
	}
	for i := full * 8; i < n; i++ {
		if testBit(bm, i) {
			used++
		}
	}
	return n - used
}

// flush writes dirty bitmaps, descriptors and the superblock, including
// every backup copy, as ext2fs_flush does.
func (f *fsys) flush() error {
	if f.wdev == nil {
		return nil
	}
	for g := range f.dirtyBB {
		bm := f.blockBitmaps[g]
		if f.gdFlags(g)&bgBlockUninit != 0 {
			f.setGdFlags(g, f.gdFlags(g)&^bgBlockUninit)
		}
		if f.hasMetadataCsum() {
			f.gdSet(g, gdBlockBitmapCsumLo, gdBlockBitmapCsumHi, 16, f.blockBitmapCsum(bm))
		}
		if err := f.writeBlock(f.blockBitmapBlock(g), bm); err != nil {
			return err
		}
		f.dirtyGD[g] = true
	}
	for g := range f.dirtyIB {
		bm := f.inodeBitmaps[g]
		if f.gdFlags(g)&bgInodeUninit != 0 {
			f.setGdFlags(g, f.gdFlags(g)&^bgInodeUninit)
		}
		if f.hasMetadataCsum() {
			f.gdSet(g, gdInodeBitmapCsumLo, gdInodeBitmapCsumHi, 16, f.inodeBitmapCsum(bm))
		}
		if err := f.writeBlock(f.inodeBitmapBlock(g), bm); err != nil {
			return err
		}
		f.dirtyGD[g] = true
	}
	if len(f.dirtyGD) == 0 && !f.dirtySB {
		return nil
	}
	for g := range f.dirtyGD {
		if f.hasGroupCsum() {
			le.PutUint16(f.gd[g][gdChecksum:], f.groupCsum(g))
		}
	}
	gdt := make([]byte, f.gdtBlocks*uint64(f.bs))
	for g := range f.gd {
		copy(gdt[g*f.descSize:], f.gd[g])
	}
	t := f.now().Unix()
	le.PutUint32(f.sb[sbWtime:], uint32(t))
	f.sb[sbWtimeHi] = byte(t >> 32)
	for g := 0; g < f.groups; g++ {
		if !f.hasSuper(g) {
			continue
		}
		sb := append([]byte(nil), f.sb...)
		le.PutUint16(sb[sbBlockGroupNr:], uint16(g))
		if g == 0 {
			le.PutUint16(sb[sbBlockGroupNr:], 0)
		}
		if f.hasMetadataCsum() {
			le.PutUint32(sb[sbChecksum:], crc32c(^uint32(0), sb[:sbChecksum]))
		}
		var off int64
		if g == 0 {
			off = superblockOffset
		} else {
			off = int64(f.groupFirstBlock(g)) * f.bs
		}
		if _, err := f.wdev.WriteAt(sb, off); err != nil {
			return fmt.Errorf("ext4: write superblock copy %d: %w", g, err)
		}
		if g == 0 {
			copy(f.sb, sb)
		}
		start := f.groupFirstBlock(g) + 1
		if _, err := f.wdev.WriteAt(gdt, int64(start)*f.bs); err != nil {
			return fmt.Errorf("ext4: write descriptor copy %d: %w", g, err)
		}
	}
	f.dirtyBB, f.dirtyIB, f.dirtyGD, f.dirtySB = map[int]bool{}, map[int]bool{}, map[int]bool{}, false
	return nil
}
