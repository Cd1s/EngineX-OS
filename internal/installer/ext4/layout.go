package ext4

import (
	"encoding/binary"
	"hash/crc32"
)

// On-disk constants. Offsets follow the Linux ext4 disk layout
// (fs/ext4/ext4.h) and e2fsprogs lib/ext2fs/ext2_fs.h.

const (
	superblockOffset = 1024
	superblockSize   = 1024
	sbMagic          = 0xEF53

	rootIno       = 2
	resizeIno     = 7
	journalIno    = 8
	firstGoodIno  = 11
	extentMagic   = 0xF30A
	jbd2Magic     = 0xC03B3998
	maxInitExtent = 32768
)

// Superblock field offsets.
const (
	sbInodesCount       = 0x00
	sbBlocksCountLo     = 0x04
	sbRBlocksCountLo    = 0x08
	sbFreeBlocksLo      = 0x0C
	sbFreeInodes        = 0x10
	sbFirstDataBlock    = 0x14
	sbLogBlockSize      = 0x18
	sbLogClusterSize    = 0x1C
	sbBlocksPerGroup    = 0x20
	sbClustersPerGroup  = 0x24
	sbInodesPerGroup    = 0x28
	sbMtime             = 0x2C
	sbWtime             = 0x30
	sbMntCount          = 0x34
	sbMaxMntCount       = 0x36
	sbMagicOff          = 0x38
	sbState             = 0x3A
	sbErrors            = 0x3C
	sbLastCheck         = 0x40
	sbCreatorOS         = 0x48
	sbRevLevel          = 0x4C
	sbFirstIno          = 0x54
	sbInodeSize         = 0x58
	sbBlockGroupNr      = 0x5A
	sbFeatureCompat     = 0x5C
	sbFeatureIncompat   = 0x60
	sbFeatureROCompat   = 0x64
	sbUUID              = 0x68
	sbVolumeName        = 0x78
	sbReservedGDTBlocks = 0xCE
	sbJournalUUID       = 0xD0
	sbJournalInum       = 0xE0
	sbJournalDev        = 0xE4
	sbLastOrphan        = 0xE8
	sbHashSeed          = 0xEC
	sbDefHashVersion    = 0xFC
	sbJnlBackupType     = 0xFD
	sbDescSize          = 0xFE
	sbDefaultMountOpts  = 0x100
	sbFirstMetaBg       = 0x104
	sbMkfsTime          = 0x108
	sbJnlBlocks         = 0x10C
	sbBlocksCountHi     = 0x150
	sbRBlocksCountHi    = 0x154
	sbFreeBlocksHi      = 0x158
	sbMinExtraIsize     = 0x15C
	sbWantExtraIsize    = 0x15E
	sbFlags             = 0x160
	sbLogGroupsPerFlex  = 0x174
	sbChecksumType      = 0x175
	sbBackupBgs         = 0x24C
	sbChecksumSeed      = 0x270
	sbWtimeHi           = 0x274
	sbMtimeHi           = 0x275
	sbMkfsTimeHi        = 0x276
	sbLastCheckHi       = 0x277
	sbOrphanFileInum    = 0x280
	sbChecksum          = 0x3FC
)

// Feature flags.
const (
	compatDirPrealloc  = 0x0001
	compatHasJournal   = 0x0004
	compatExtAttr      = 0x0008
	compatResizeInode  = 0x0010
	compatDirIndex     = 0x0020
	compatSparseSuper2 = 0x0200
	compatStableInodes = 0x0800
	compatOrphanFile   = 0x1000

	incompatFiletype = 0x0002
	incompatRecover  = 0x0004
	incompatExtents  = 0x0040
	incompat64bit    = 0x0080
	incompatFlexBg   = 0x0200
	incompatCsumSeed = 0x2000
	incompatEncrypt  = 0x10000

	roCompatSparseSuper   = 0x0001
	roCompatLargeFile     = 0x0002
	roCompatHugeFile      = 0x0008
	roCompatGdtCsum       = 0x0010
	roCompatDirNlink      = 0x0020
	roCompatExtraIsize    = 0x0040
	roCompatMetadataCsum  = 0x0400
	roCompatOrphanPresent = 0x10000
)

// Features this package reads and mutates. Anything else is refused.
const (
	supportedCompat = compatDirPrealloc | compatHasJournal | compatExtAttr | compatResizeInode |
		compatDirIndex | compatSparseSuper2 | compatStableInodes | compatOrphanFile
	supportedIncompat = incompatFiletype | incompatExtents | incompat64bit | incompatFlexBg |
		incompatCsumSeed | incompatEncrypt
	supportedROCompat = roCompatSparseSuper | roCompatLargeFile | roCompatHugeFile | roCompatGdtCsum |
		roCompatDirNlink | roCompatExtraIsize | roCompatMetadataCsum
)

// Group descriptor field offsets.
const (
	gdBlockBitmapLo     = 0x00
	gdInodeBitmapLo     = 0x04
	gdInodeTableLo      = 0x08
	gdFreeBlocksLo      = 0x0C
	gdFreeInodesLo      = 0x0E
	gdUsedDirsLo        = 0x10
	gdFlags             = 0x12
	gdBlockBitmapCsumLo = 0x18
	gdInodeBitmapCsumLo = 0x1A
	gdItableUnusedLo    = 0x1C
	gdChecksum          = 0x1E
	gdBlockBitmapHi     = 0x20
	gdInodeBitmapHi     = 0x24
	gdInodeTableHi      = 0x28
	gdFreeBlocksHi      = 0x2C
	gdFreeInodesHi      = 0x2E
	gdUsedDirsHi        = 0x30
	gdItableUnusedHi    = 0x32
	gdBlockBitmapCsumHi = 0x38
	gdInodeBitmapCsumHi = 0x3A

	bgInodeUninit  = 0x1
	bgBlockUninit  = 0x2
	bgItableZeroed = 0x4
)

// Inode field offsets.
const (
	inMode        = 0x00
	inUID         = 0x02
	inSizeLo      = 0x04
	inAtime       = 0x08
	inCtime       = 0x0C
	inMtime       = 0x10
	inDtime       = 0x14
	inGID         = 0x18
	inLinks       = 0x1A
	inBlocksLo    = 0x1C
	inFlags       = 0x20
	inBlock       = 0x28
	inGeneration  = 0x64
	inFileACLLo   = 0x68
	inSizeHi      = 0x6C
	inBlocksHi    = 0x74
	inFileACLHi   = 0x76
	inUIDHi       = 0x78
	inGIDHi       = 0x7A
	inChecksumLo  = 0x7C
	inExtraIsize  = 0x80
	inChecksumHi  = 0x82
	inCtimeExtra  = 0x84
	inMtimeExtra  = 0x88
	inAtimeExtra  = 0x8C
	inCrtime      = 0x90
	inCrtimeExtra = 0x94

	inodeBlockLen = 60

	flagIndex      = 0x00001000
	flagEncrypt    = 0x00000800
	flagHugeFile   = 0x00040000
	flagExtents    = 0x00080000
	flagEAInode    = 0x00200000
	flagInlineData = 0x10000000
	flagCasefold   = 0x40000000
	flagVerity     = 0x00100000
)

// File type bits of i_mode and dirent file_type codes.
const (
	sIFMT   = 0xF000
	sIFSOCK = 0xC000
	sIFLNK  = 0xA000
	sIFREG  = 0x8000
	sIFBLK  = 0x6000
	sIFDIR  = 0x4000
	sIFCHR  = 0x2000
	sIFIFO  = 0x1000

	ftUnknown = 0
	ftReg     = 1
	ftDir     = 2
	ftChr     = 3
	ftBlk     = 4
	ftFifo    = 5
	ftSock    = 6
	ftSymlink = 7
	ftCsum    = 0xDE
)

func fileTypeOf(mode uint16) uint8 {
	switch mode & sIFMT {
	case sIFREG:
		return ftReg
	case sIFDIR:
		return ftDir
	case sIFCHR:
		return ftChr
	case sIFBLK:
		return ftBlk
	case sIFIFO:
		return ftFifo
	case sIFSOCK:
		return ftSock
	case sIFLNK:
		return ftSymlink
	}
	return ftUnknown
}

var le = binary.LittleEndian
var be = binary.BigEndian

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c continues a raw (non-inverted) CRC32C as the kernel's crc32c_le does.
func crc32c(seed uint32, b []byte) uint32 {
	// hash/crc32.Update inverts on entry and exit; undo both so the
	// seed and result match the kernel's convention.
	return ^crc32.Update(^seed, castagnoli, b)
}

// crc16 is the CRC-16/ARC used by the gdt_csum (uninit_bg) group checksum.
func crc16(crc uint16, b []byte) uint16 {
	for _, c := range b {
		crc ^= uint16(c)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

func u32le(v uint32) []byte {
	var b [4]byte
	le.PutUint32(b[:], v)
	return b[:]
}

func direntLen(nameLen int) int { return (8 + nameLen + 3) &^ 3 }
