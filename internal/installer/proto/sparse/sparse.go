// Package sparse implements Android sparse image v1 (0xed26ff3a) parsing,
// validation, encoding, decoding, and bounded-size resparsing for fastboot.
//
// Sparse streams are partitioned into pieces whose total encoded byte length
// (including the 28-byte sparse header, leading DONT_CARE positioning chunk,
// chunk headers, payloads, and optional CRC32 chunk) never exceeds the caller's
// download limit.
package sparse

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
)

const (
	// Magic identifies an Android sparse image header (little-endian).
	Magic uint32 = 0xED26FF3A

	// MajorVersion is the only supported Android sparse major version.
	MajorVersion uint16 = 1

	// FileHeaderSize is the canonical sparse file header size in bytes.
	FileHeaderSize uint16 = 28

	// ChunkHeaderSize is the canonical sparse chunk header size in bytes.
	ChunkHeaderSize uint16 = 12

	// DefaultBlockSize is the standard 4 KiB sparse block size.
	DefaultBlockSize uint32 = 4096

	maxHeaderSize   uint16 = 4096
	maxBlockSize    uint32 = 64 << 20
	maxTotalChunks  uint32 = 16 << 20
	scanBufferBytes int64  = 1 << 20
)

// Android sparse chunk type codes.
const (
	ChunkTypeRaw      uint16 = 0xCAC1
	ChunkTypeFill     uint16 = 0xCAC2
	ChunkTypeDontCare uint16 = 0xCAC3
	ChunkTypeCRC32    uint16 = 0xCAC4
)

var (
	ErrNotSparse     = errors.New("sparse: input does not have Android sparse magic")
	ErrLimitTooSmall = errors.New("sparse: max piece byte limit is too small for a single block")
	ErrUnalignedSize = errors.New("sparse: raw image size is not a multiple of block size")
)

// Header represents the 28-byte Android sparse file header.
type Header struct {
	MajorVersion  uint16
	MinorVersion  uint16
	FileHdrSize   uint16
	ChunkHdrSize  uint16
	BlockSize     uint32
	TotalBlocks   uint32
	TotalChunks   uint32
	ImageChecksum uint32
}

// LogicalSize returns the uncompressed image byte length described by Header.
func (h Header) LogicalSize() uint64 {
	return uint64(h.TotalBlocks) * uint64(h.BlockSize)
}

// ParsedChunk describes one validated chunk in an Android sparse stream.
type ParsedChunk struct {
	Type        uint16
	Blocks      uint32
	TotalBytes  uint32
	DataOffset  int64  // file offset of chunk payload (0 for DONT_CARE)
	DataBytes   uint64 // byte length of chunk payload in sparse stream
	LogicalBlk  uint64 // starting logical block index
	FillPattern [4]byte
	CRC32       uint32
}

// IsSparse reports whether src begins with the 4-byte Android sparse magic.
func IsSparse(src io.ReaderAt, size int64) (bool, error) {
	if src == nil {
		return false, errors.New("sparse: nil reader")
	}
	if size < 4 {
		return false, nil
	}
	var magic [4]byte
	if err := readAtExact(src, 0, magic[:]); err != nil {
		return false, err
	}
	return binary.LittleEndian.Uint32(magic[:]) == Magic, nil
}

// ParseHeader reads and validates the sparse file header at offset 0.
func ParseHeader(src io.ReaderAt, size int64) (Header, error) {
	if src == nil {
		return Header{}, errors.New("sparse: nil reader")
	}
	if size < int64(FileHeaderSize) {
		return Header{}, fmt.Errorf("sparse: file too short (%d bytes)", size)
	}
	var buf [FileHeaderSize]byte
	if err := readAtExact(src, 0, buf[:]); err != nil {
		return Header{}, err
	}
	if gotMagic := binary.LittleEndian.Uint32(buf[0:4]); gotMagic != Magic {
		return Header{}, fmt.Errorf("%w (got 0x%08x)", ErrNotSparse, gotMagic)
	}
	h := Header{
		MajorVersion:  binary.LittleEndian.Uint16(buf[4:6]),
		MinorVersion:  binary.LittleEndian.Uint16(buf[6:8]),
		FileHdrSize:   binary.LittleEndian.Uint16(buf[8:10]),
		ChunkHdrSize:  binary.LittleEndian.Uint16(buf[10:12]),
		BlockSize:     binary.LittleEndian.Uint32(buf[12:16]),
		TotalBlocks:   binary.LittleEndian.Uint32(buf[16:20]),
		TotalChunks:   binary.LittleEndian.Uint32(buf[20:24]),
		ImageChecksum: binary.LittleEndian.Uint32(buf[24:28]),
	}
	if h.MajorVersion != MajorVersion {
		return Header{}, fmt.Errorf("sparse: unsupported major version %d", h.MajorVersion)
	}
	if h.FileHdrSize < FileHeaderSize || h.FileHdrSize > maxHeaderSize {
		return Header{}, fmt.Errorf("sparse: invalid file header size %d", h.FileHdrSize)
	}
	if int64(h.FileHdrSize) > size {
		return Header{}, fmt.Errorf("sparse: file header size %d exceeds input size %d", h.FileHdrSize, size)
	}
	if h.ChunkHdrSize < ChunkHeaderSize || h.ChunkHdrSize > maxHeaderSize {
		return Header{}, fmt.Errorf("sparse: invalid chunk header size %d", h.ChunkHdrSize)
	}
	if h.BlockSize == 0 || h.BlockSize%4 != 0 || h.BlockSize > maxBlockSize {
		return Header{}, fmt.Errorf("sparse: invalid block size %d", h.BlockSize)
	}
	if h.TotalChunks > maxTotalChunks {
		return Header{}, fmt.Errorf("sparse: chunk count %d exceeds limit %d", h.TotalChunks, maxTotalChunks)
	}
	if h.TotalBlocks > 0 && h.TotalChunks == 0 {
		return Header{}, errors.New("sparse: non-zero TotalBlocks with zero TotalChunks")
	}
	return h, nil
}

// MarshalHeader encodes h as a canonical 28-byte sparse file header.
func MarshalHeader(h Header) [FileHeaderSize]byte {
	var buf [FileHeaderSize]byte
	binary.LittleEndian.PutUint32(buf[0:4], Magic)
	binary.LittleEndian.PutUint16(buf[4:6], MajorVersion)
	binary.LittleEndian.PutUint16(buf[6:8], h.MinorVersion)
	binary.LittleEndian.PutUint16(buf[8:10], FileHeaderSize)
	binary.LittleEndian.PutUint16(buf[10:12], ChunkHeaderSize)
	binary.LittleEndian.PutUint32(buf[12:16], h.BlockSize)
	binary.LittleEndian.PutUint32(buf[16:20], h.TotalBlocks)
	binary.LittleEndian.PutUint32(buf[20:24], h.TotalChunks)
	binary.LittleEndian.PutUint32(buf[24:28], h.ImageChecksum)
	return buf
}

// MarshalChunkHeader encodes a canonical 12-byte sparse chunk header.
func MarshalChunkHeader(chunkType uint16, blocks, totalBytes uint32) [ChunkHeaderSize]byte {
	var buf [ChunkHeaderSize]byte
	binary.LittleEndian.PutUint16(buf[0:2], chunkType)
	binary.LittleEndian.PutUint16(buf[2:4], 0)
	binary.LittleEndian.PutUint32(buf[4:8], blocks)
	binary.LittleEndian.PutUint32(buf[8:12], totalBytes)
	return buf
}

// ParseChunks parses and validates all chunk headers, sizes, block counts, EOF
// alignment, and any embedded CRC32 checksums in a sparse stream.
func ParseChunks(src io.ReaderAt, size int64) (Header, []ParsedChunk, error) {
	h, err := ParseHeader(src, size)
	if err != nil {
		return Header{}, nil, err
	}
	chunks := make([]ParsedChunk, 0, minUint32(h.TotalChunks, 1024))
	offset := int64(h.FileHdrSize)
	var logicalBlk uint64
	var runningCRC uint32
	crcScratch := make([]byte, minUint32(h.BlockSize, 64<<10))
	zeroBlock := make([]byte, minUint32(h.BlockSize, 64<<10))
	chunkHdrBuf := make([]byte, ChunkHeaderSize)

	for i := uint32(0); i < h.TotalChunks; i++ {
		if offset+int64(h.ChunkHdrSize) > size {
			return Header{}, nil, fmt.Errorf("sparse: chunk %d header truncated at offset %d", i, offset)
		}
		if err := readAtExact(src, offset, chunkHdrBuf); err != nil {
			return Header{}, nil, fmt.Errorf("sparse: read chunk %d header: %w", i, err)
		}
		ctype := binary.LittleEndian.Uint16(chunkHdrBuf[0:2])
		cblocks := binary.LittleEndian.Uint32(chunkHdrBuf[4:8])
		ctotal := binary.LittleEndian.Uint32(chunkHdrBuf[8:12])
		if uint64(ctotal) < uint64(h.ChunkHdrSize) {
			return Header{}, nil, fmt.Errorf("sparse: chunk %d total_sz %d smaller than chunk header %d", i, ctotal, h.ChunkHdrSize)
		}
		dataOff := offset + int64(h.ChunkHdrSize)
		if offset+int64(ctotal) > size {
			return Header{}, nil, fmt.Errorf("sparse: chunk %d truncated (ends at %d > size %d)", i, offset+int64(ctotal), size)
		}
		pc := ParsedChunk{
			Type:       ctype,
			Blocks:     cblocks,
			TotalBytes: ctotal,
			DataOffset: dataOff,
			LogicalBlk: logicalBlk,
		}
		switch ctype {
		case ChunkTypeRaw:
			wantData := uint64(cblocks) * uint64(h.BlockSize)
			wantTotal := uint64(h.ChunkHdrSize) + wantData
			if wantTotal != uint64(ctotal) {
				return Header{}, nil, fmt.Errorf("sparse: RAW chunk %d size mismatch (total_sz=%d want=%d)", i, ctotal, wantTotal)
			}
			pc.DataBytes = wantData
			var err error
			runningCRC, err = crcUpdateReaderAt(runningCRC, src, dataOff, int64(wantData), crcScratch)
			if err != nil {
				return Header{}, nil, fmt.Errorf("sparse: RAW chunk %d data: %w", i, err)
			}
			logicalBlk += uint64(cblocks)
		case ChunkTypeFill:
			wantTotal := uint64(h.ChunkHdrSize) + 4
			if wantTotal != uint64(ctotal) {
				return Header{}, nil, fmt.Errorf("sparse: FILL chunk %d size mismatch (total_sz=%d want=%d)", i, ctotal, wantTotal)
			}
			pc.DataBytes = 4
			if err := readAtExact(src, dataOff, pc.FillPattern[:]); err != nil {
				return Header{}, nil, fmt.Errorf("sparse: FILL chunk %d pattern: %w", i, err)
			}
			runningCRC = crcUpdatePattern(runningCRC, pc.FillPattern, uint64(cblocks)*uint64(h.BlockSize), crcScratch)
			logicalBlk += uint64(cblocks)
		case ChunkTypeDontCare:
			if uint64(h.ChunkHdrSize) != uint64(ctotal) {
				return Header{}, nil, fmt.Errorf("sparse: DONT_CARE chunk %d size mismatch (total_sz=%d want=%d)", i, ctotal, h.ChunkHdrSize)
			}
			pc.DataOffset = 0
			runningCRC = crcUpdateZeroes(runningCRC, uint64(cblocks)*uint64(h.BlockSize), zeroBlock)
			logicalBlk += uint64(cblocks)
		case ChunkTypeCRC32:
			if cblocks != 0 {
				return Header{}, nil, fmt.Errorf("sparse: CRC32 chunk %d has non-zero block count %d", i, cblocks)
			}
			wantTotal := uint64(h.ChunkHdrSize) + 4
			if wantTotal != uint64(ctotal) {
				return Header{}, nil, fmt.Errorf("sparse: CRC32 chunk %d size mismatch (total_sz=%d want=%d)", i, ctotal, wantTotal)
			}
			pc.DataBytes = 4
			var rawCRC [4]byte
			if err := readAtExact(src, dataOff, rawCRC[:]); err != nil {
				return Header{}, nil, fmt.Errorf("sparse: CRC32 chunk %d: %w", i, err)
			}
			pc.CRC32 = binary.LittleEndian.Uint32(rawCRC[:])
			if pc.CRC32 != runningCRC {
				return Header{}, nil, fmt.Errorf("sparse: CRC32 chunk %d mismatch (got 0x%08x, computed 0x%08x)", i, pc.CRC32, runningCRC)
			}
		default:
			return Header{}, nil, fmt.Errorf("sparse: unknown chunk type 0x%04x at chunk %d", ctype, i)
		}
		if logicalBlk > uint64(h.TotalBlocks) {
			return Header{}, nil, fmt.Errorf("sparse: chunk %d exceeds header TotalBlocks (%d > %d)", i, logicalBlk, h.TotalBlocks)
		}
		chunks = append(chunks, pc)
		offset += int64(ctotal)
	}
	if offset != size {
		return Header{}, nil, fmt.Errorf("sparse: trailing data after chunks (%d != %d)", offset, size)
	}
	if logicalBlk != uint64(h.TotalBlocks) {
		return Header{}, nil, fmt.Errorf("sparse: block count sum %d != header TotalBlocks %d", logicalBlk, h.TotalBlocks)
	}
	if h.ImageChecksum != 0 && h.ImageChecksum != runningCRC {
		return Header{}, nil, fmt.Errorf("sparse: header image_checksum mismatch (got 0x%08x, computed 0x%08x)", h.ImageChecksum, runningCRC)
	}
	return h, chunks, nil
}

// DecodeOptions configures sparse image reconstruction into a WriterAt.
type DecodeOptions struct {
	// ZeroDontCare writes zeroes for DONT_CARE ranges instead of skipping them.
	// Leave false to emulate U-Boot's DONT_CARE skip semantics when applying
	// multiple sparse pieces sequentially onto a backing image.
	ZeroDontCare bool
}

// DecodedInfo summarizes a decoded sparse stream.
type DecodedInfo struct {
	Header         Header
	LogicalBytes   uint64
	WrittenBlocks  uint64
	DontCareBlocks uint64
}

// Decode validates src as an Android sparse v1 stream and applies its chunks
// sequentially to dst at their logical offsets.
func Decode(dst io.WriterAt, src io.ReaderAt, size int64, opts DecodeOptions) (DecodedInfo, error) {
	if dst == nil {
		return DecodedInfo{}, errors.New("sparse: nil destination")
	}
	h, chunks, err := ParseChunks(src, size)
	if err != nil {
		return DecodedInfo{}, err
	}
	scratch := make([]byte, minUint32(h.BlockSize, 64<<10))
	var info DecodedInfo
	info.Header = h
	info.LogicalBytes = h.LogicalSize()

	for _, c := range chunks {
		outOff := int64(c.LogicalBlk) * int64(h.BlockSize)
		totalLen := int64(c.Blocks) * int64(h.BlockSize)
		switch c.Type {
		case ChunkTypeRaw:
			info.WrittenBlocks += uint64(c.Blocks)
			if err := copyReaderAtToWriterAt(dst, outOff, src, c.DataOffset, totalLen, scratch); err != nil {
				return DecodedInfo{}, err
			}
		case ChunkTypeFill:
			info.WrittenBlocks += uint64(c.Blocks)
			if err := writePattern(dst, outOff, c.FillPattern, totalLen, scratch); err != nil {
				return DecodedInfo{}, err
			}
		case ChunkTypeDontCare:
			info.DontCareBlocks += uint64(c.Blocks)
			if opts.ZeroDontCare && totalLen > 0 {
				if err := writePattern(dst, outOff, [4]byte{}, totalLen, scratch); err != nil {
					return DecodedInfo{}, err
				}
			}
		case ChunkTypeCRC32:
			// Already verified by ParseChunks.
		}
	}
	return info, nil
}

// EncodeOptions configures single-stream sparse encoding of a raw image.
type EncodeOptions struct {
	BlockSize    uint32
	PadLastBlock bool
	IncludeCRC   bool
}

// Encode encodes a raw image into a single Android sparse v1 stream on dst.
func Encode(dst io.Writer, src io.ReaderAt, size int64, opts EncodeOptions) error {
	if dst == nil {
		return errors.New("sparse: nil writer")
	}
	iter, err := NewResparse(src, size, ResparseOptions{
		MaxPieceBytes: 1<<62 - 1,
		BlockSize:     opts.BlockSize,
		PadLastBlock:  opts.PadLastBlock,
		IncludeCRC:    opts.IncludeCRC,
	})
	if err != nil {
		return err
	}
	piece, err := iter.Next()
	if err != nil {
		return err
	}
	if _, err := piece.WriteTo(dst); err != nil {
		return err
	}
	if extra, err := iter.Next(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("sparse: unexpected extra piece starting at block %d", extra.StartBlock)
	}
	return nil
}

// ResparseOptions controls how a raw or sparse image is partitioned into
// download-sized sparse pieces.
type ResparseOptions struct {
	// MaxPieceBytes is the maximum encoded byte length of each piece,
	// including all sparse headers and footers. Required.
	MaxPieceBytes int64

	// BlockSize sets the block size when encoding raw images (default 4096).
	// Ignored when the input is already sparse (the input header's BlockSize
	// is preserved).
	BlockSize uint32

	// PadLastBlock zero-pads a trailing partial block when encoding a raw
	// image whose size is not an exact multiple of BlockSize. If false,
	// non-aligned raw images return ErrUnalignedSize.
	PadLastBlock bool

	// IncludeCRC appends a per-piece CHUNK_TYPE_CRC32 footer to every piece.
	IncludeCRC bool
}

type chunkSpec struct {
	typ       uint16
	blocks    uint32
	srcOffset int64
	srcBytes  int64 // actual bytes readable from src for RAW; <= blocks*blkSize (tail padded with 0)
	fill      [4]byte
}

type pieceChunk struct {
	typ       uint16
	blocks    uint32
	srcOffset int64
	srcBytes  int64
	fill      [4]byte
	crc32     uint32
	dataOff   int64 // offset of this chunk header inside the encoded piece
	totalLen  int64 // encoded byte length of this chunk (header + payload)
}

// Piece represents one bounded Android sparse image download slice.
type Piece struct {
	Index         int
	TotalPieces   int
	StartBlock    uint64
	WrittenBlocks uint64
	TotalBlocks   uint32
	BlockSize     uint32
	EncodedSize   int64
	LogicalOffset int64
	LogicalSize   int64
	ChunkCount    uint32

	src     io.ReaderAt
	header  [FileHeaderSize]byte
	chunks  []pieceChunk
	offsets []int64 // starting encoded offset of each chunk
}

// Open returns an io.ReadCloser streaming the encoded sparse bytes of this piece.
func (p *Piece) Open() io.ReadCloser {
	return io.NopCloser(io.NewSectionReader(p, 0, p.EncodedSize))
}

// WriteTo streams the full encoded sparse bytes of this piece to dst.
func (p *Piece) WriteTo(dst io.Writer) (int64, error) {
	return io.CopyBuffer(dst, p.Open(), make([]byte, 64<<10))
}

// ReadAt implements io.ReaderAt over the synthesized sparse piece bytes.
func (p *Piece) ReadAt(dst []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("sparse: negative ReadAt offset")
	}
	if off >= p.EncodedSize {
		return 0, io.EOF
	}
	want := len(dst)
	if int64(want) > p.EncodedSize-off {
		want = int(p.EncodedSize - off)
	}
	written := 0
	for written < want {
		pos := off + int64(written)
		if pos < int64(FileHeaderSize) {
			n := copy(dst[written:want], p.header[pos:])
			written += n
			continue
		}
		ci := sort.Search(len(p.offsets), func(i int) bool {
			return p.offsets[i] > pos
		}) - 1
		if ci < 0 || ci >= len(p.chunks) {
			return written, fmt.Errorf("sparse: internal chunk offset lookup failed at %d", pos)
		}
		c := p.chunks[ci]
		rel := pos - p.offsets[ci]
		if rel < int64(ChunkHeaderSize) {
			hdr := MarshalChunkHeader(c.typ, c.blocks, uint32(c.totalLen))
			n := copy(dst[written:want], hdr[rel:])
			written += n
			continue
		}
		payRel := rel - int64(ChunkHeaderSize)
		payNeed := want - written
		payAvail := c.totalLen - int64(ChunkHeaderSize) - payRel
		if int64(payNeed) > payAvail {
			payNeed = int(payAvail)
		}
		switch c.typ {
		case ChunkTypeRaw:
			if err := readPieceRaw(dst[written:written+payNeed], p.src, c.srcOffset, c.srcBytes, payRel); err != nil {
				return written, err
			}
			written += payNeed
		case ChunkTypeFill:
			for i := 0; i < payNeed; i++ {
				dst[written+i] = c.fill[(int(payRel)+i)%4]
			}
			written += payNeed
		case ChunkTypeCRC32:
			var rawCRC [4]byte
			binary.LittleEndian.PutUint32(rawCRC[:], c.crc32)
			n := copy(dst[written:written+payNeed], rawCRC[payRel:])
			written += n
		default:
			return written, fmt.Errorf("sparse: unexpected chunk payload type 0x%04x", c.typ)
		}
	}
	if written < len(dst) {
		return written, io.EOF
	}
	return written, nil
}

func readPieceRaw(dst []byte, src io.ReaderAt, srcOffset, srcBytes, payRel int64) error {
	if payRel >= srcBytes {
		clear(dst)
		return nil
	}
	fromSrc := len(dst)
	if payRel+int64(fromSrc) > srcBytes {
		fromSrc = int(srcBytes - payRel)
	}
	if err := readAtExact(src, srcOffset+payRel, dst[:fromSrc]); err != nil {
		return err
	}
	if fromSrc < len(dst) {
		clear(dst[fromSrc:])
	}
	return nil
}

// Iterator yields bounded sparse pieces sequentially.
type Iterator struct {
	pieces []*Piece
	pos    int
}

// Count returns the total number of planned sparse pieces.
func (it *Iterator) Count() int {
	return len(it.pieces)
}

// TotalLogicalBlocks returns the total logical blocks across the planned image.
func (it *Iterator) TotalLogicalBlocks() uint64 {
	if len(it.pieces) == 0 {
		return 0
	}
	last := it.pieces[len(it.pieces)-1]
	return uint64(last.TotalBlocks)
}

// Next returns the next sparse Piece, or io.EOF when all pieces have been yielded.
func (it *Iterator) Next() (*Piece, error) {
	if it.pos >= len(it.pieces) {
		return nil, io.EOF
	}
	p := it.pieces[it.pos]
	it.pos++
	return p, nil
}

// NewResparse plans bounded Android sparse v1 pieces from either a raw image or
// an already-sparse image.
func NewResparse(src io.ReaderAt, size int64, opts ResparseOptions) (*Iterator, error) {
	if src == nil {
		return nil, errors.New("sparse: nil input reader")
	}
	if size <= 0 {
		return nil, fmt.Errorf("sparse: input size must be positive, got %d", size)
	}
	if opts.MaxPieceBytes <= 0 {
		return nil, fmt.Errorf("sparse: MaxPieceBytes must be positive, got %d", opts.MaxPieceBytes)
	}

	isSp, err := IsSparse(src, size)
	if err != nil {
		return nil, err
	}

	var blkSize uint32
	var specs []chunkSpec
	var totalBlocks uint64

	if isSp {
		h, parsed, err := ParseChunks(src, size)
		if err != nil {
			return nil, err
		}
		blkSize = h.BlockSize
		totalBlocks = uint64(h.TotalBlocks)
		specs = make([]chunkSpec, 0, len(parsed))
		for _, c := range parsed {
			if c.Type == ChunkTypeCRC32 {
				continue
			}
			specs = append(specs, chunkSpec{
				typ:       c.Type,
				blocks:    c.Blocks,
				srcOffset: c.DataOffset,
				srcBytes:  int64(c.DataBytes),
				fill:      c.FillPattern,
			})
		}
	} else {
		blkSize = opts.BlockSize
		if blkSize == 0 {
			blkSize = DefaultBlockSize
		}
		if blkSize%4 != 0 || blkSize > maxBlockSize {
			return nil, fmt.Errorf("sparse: invalid block size %d", blkSize)
		}
		specs, totalBlocks, err = scanRawToSpecs(src, size, blkSize, opts.PadLastBlock)
		if err != nil {
			return nil, err
		}
	}

	if totalBlocks > uint64(^uint32(0)) {
		return nil, fmt.Errorf("sparse: total blocks %d exceeds uint32", totalBlocks)
	}

	// Minimum budget for a piece that can carry at least one RAW block plus a
	// leading DONT_CARE chunk (when splitting across multiple pieces) and optional CRC32.
	crcOverhead := int64(0)
	if opts.IncludeCRC {
		crcOverhead = int64(ChunkHeaderSize) + 4
	}
	minSinglePiece := int64(FileHeaderSize) + int64(ChunkHeaderSize) + int64(blkSize) + crcOverhead
	if opts.MaxPieceBytes < minSinglePiece {
		return nil, fmt.Errorf("%w: limit %d < minimum %d", ErrLimitTooSmall, opts.MaxPieceBytes, minSinglePiece)
	}

	pieces, err := buildPieces(src, blkSize, specs, totalBlocks, opts.MaxPieceBytes, opts.IncludeCRC)
	if err != nil {
		return nil, err
	}
	return &Iterator{pieces: pieces}, nil
}

func scanRawToSpecs(src io.ReaderAt, size int64, blkSize uint32, padLast bool) ([]chunkSpec, uint64, error) {
	blk := int64(blkSize)
	rem := size % blk
	if rem != 0 && !padLast {
		return nil, 0, fmt.Errorf("%w: size %d not divisible by block size %d", ErrUnalignedSize, size, blkSize)
	}
	totalBlocks := uint64((size + blk - 1) / blk)
	if totalBlocks > uint64(^uint32(0)) {
		return nil, 0, fmt.Errorf("sparse: block count %d overflows uint32", totalBlocks)
	}

	bufCap := (scanBufferBytes / blk) * blk
	if bufCap < blk {
		bufCap = blk
	}
	buf := make([]byte, bufCap)
	zeroBlock := make([]byte, blk)

	var specs []chunkSpec
	var off int64
	for off < size {
		readLen := bufCap
		if size-off < readLen {
			readLen = size - off
		}
		if err := readAtExact(src, off, buf[:readLen]); err != nil {
			return nil, 0, err
		}
		// Pad any trailing partial block with zeroes.
		paddedLen := ((readLen + blk - 1) / blk) * blk
		if paddedLen > readLen {
			clear(buf[readLen:paddedLen])
		}
		for bOff := int64(0); bOff < paddedLen; bOff += blk {
			blockBytes := buf[bOff : bOff+blk]
			srcOff := off + bOff
			srcLen := blk
			if srcOff+srcLen > size {
				srcLen = size - srcOff
			}
			var pattern [4]byte
			uniform := false
			if bytes.Equal(blockBytes, zeroBlock) {
				uniform = true
			} else {
				pattern, uniform = uniform4BytePattern(blockBytes)
			}
			if uniform {
				if n := len(specs); n > 0 && specs[n-1].typ == ChunkTypeFill && specs[n-1].fill == pattern {
					specs[n-1].blocks++
				} else {
					specs = append(specs, chunkSpec{
						typ:    ChunkTypeFill,
						blocks: 1,
						fill:   pattern,
					})
				}
			} else {
				if n := len(specs); n > 0 && specs[n-1].typ == ChunkTypeRaw && specs[n-1].srcOffset+specs[n-1].srcBytes == srcOff {
					specs[n-1].blocks++
					specs[n-1].srcBytes += srcLen
				} else {
					specs = append(specs, chunkSpec{
						typ:       ChunkTypeRaw,
						blocks:    1,
						srcOffset: srcOff,
						srcBytes:  srcLen,
					})
				}
			}
		}
		off += readLen
	}
	return specs, totalBlocks, nil
}

func uniform4BytePattern(b []byte) ([4]byte, bool) {
	var pat [4]byte
	if len(b) < 4 || len(b)%4 != 0 {
		return pat, false
	}
	copy(pat[:], b[:4])
	if len(b) >= 8 && len(b)%8 == 0 {
		p32 := binary.LittleEndian.Uint32(pat[:])
		p64 := uint64(p32) | (uint64(p32) << 32)
		for i := 0; i < len(b); i += 8 {
			if binary.LittleEndian.Uint64(b[i:i+8]) != p64 {
				return pat, false
			}
		}
		return pat, true
	}
	for i := 4; i < len(b); i += 4 {
		if b[i] != pat[0] || b[i+1] != pat[1] || b[i+2] != pat[2] || b[i+3] != pat[3] {
			return pat, false
		}
	}
	return pat, true
}

func buildPieces(src io.ReaderAt, blkSize uint32, specs []chunkSpec, totalBlocks uint64, maxBytes int64, includeCRC bool) ([]*Piece, error) {
	crcReserve := int64(0)
	if includeCRC {
		crcReserve = int64(ChunkHeaderSize) + 4
	}

	type draftPiece struct {
		startBlock    uint64
		writtenBlocks uint64
		chunks        []pieceChunk
		encodedBytes  int64
	}

	newDraft := func(startBlk uint64) draftPiece {
		d := draftPiece{
			startBlock:   startBlk,
			encodedBytes: int64(FileHeaderSize),
		}
		if startBlk > 0 {
			d.chunks = append(d.chunks, pieceChunk{
				typ:      ChunkTypeDontCare,
				blocks:   uint32(startBlk),
				dataOff:  d.encodedBytes,
				totalLen: int64(ChunkHeaderSize),
			})
			d.encodedBytes += int64(ChunkHeaderSize)
		}
		return d
	}

	var drafts []draftPiece
	cur := newDraft(0)
	curBlock := uint64(0)

	flush := func() {
		if cur.writtenBlocks > 0 {
			drafts = append(drafts, cur)
		}
	}

	for _, sp := range specs {
		if sp.blocks == 0 {
			continue
		}
		switch sp.typ {
		case ChunkTypeDontCare:
			if cur.writtenBlocks == 0 {
				curBlock += uint64(sp.blocks)
				cur = newDraft(curBlock)
				continue
			}
			// Only embed DONT_CARE inside an active piece if it fits cleanly;
			// otherwise flushing and advancing startBlock achieves the exact same seek.
			if cur.encodedBytes+int64(ChunkHeaderSize)+crcReserve <= maxBytes {
				cur.chunks = append(cur.chunks, pieceChunk{
					typ:      ChunkTypeDontCare,
					blocks:   sp.blocks,
					dataOff:  cur.encodedBytes,
					totalLen: int64(ChunkHeaderSize),
				})
				cur.encodedBytes += int64(ChunkHeaderSize)
				cur.writtenBlocks += uint64(sp.blocks)
				curBlock += uint64(sp.blocks)
			} else {
				flush()
				curBlock += uint64(sp.blocks)
				cur = newDraft(curBlock)
			}

		case ChunkTypeFill:
			fillLen := int64(ChunkHeaderSize) + 4
			if cur.encodedBytes+fillLen+crcReserve > maxBytes {
				if cur.writtenBlocks == 0 {
					return nil, fmt.Errorf("%w: cannot fit FILL chunk in %d bytes", ErrLimitTooSmall, maxBytes)
				}
				flush()
				cur = newDraft(curBlock)
				if cur.encodedBytes+fillLen+crcReserve > maxBytes {
					return nil, fmt.Errorf("%w: cannot fit FILL chunk with leading DONT_CARE in %d bytes", ErrLimitTooSmall, maxBytes)
				}
			}
			cur.chunks = append(cur.chunks, pieceChunk{
				typ:      ChunkTypeFill,
				blocks:   sp.blocks,
				fill:     sp.fill,
				dataOff:  cur.encodedBytes,
				totalLen: fillLen,
			})
			cur.encodedBytes += fillLen
			cur.writtenBlocks += uint64(sp.blocks)
			curBlock += uint64(sp.blocks)

		case ChunkTypeRaw:
			remBlocks := uint64(sp.blocks)
			srcOff := sp.srcOffset
			remSrcBytes := sp.srcBytes
			for remBlocks > 0 {
				avail := maxBytes - cur.encodedBytes - crcReserve - int64(ChunkHeaderSize)
				maxBlks := avail / int64(blkSize)
				if maxBlks <= 0 {
					if cur.writtenBlocks == 0 {
						return nil, fmt.Errorf("%w: cannot fit 1 RAW block (%d bytes) in %d bytes", ErrLimitTooSmall, blkSize, maxBytes)
					}
					flush()
					cur = newDraft(curBlock)
					avail = maxBytes - cur.encodedBytes - crcReserve - int64(ChunkHeaderSize)
					maxBlks = avail / int64(blkSize)
					if maxBlks <= 0 {
						return nil, fmt.Errorf("%w: cannot fit 1 RAW block (%d bytes) with leading DONT_CARE in %d bytes", ErrLimitTooSmall, blkSize, maxBytes)
					}
				}
				take := remBlocks
				if uint64(maxBlks) < take {
					take = uint64(maxBlks)
				}
				takeBytes := int64(take) * int64(blkSize)
				takeSrcBytes := takeBytes
				if takeSrcBytes > remSrcBytes {
					takeSrcBytes = remSrcBytes
				}
				chunkTotal := int64(ChunkHeaderSize) + takeBytes
				cur.chunks = append(cur.chunks, pieceChunk{
					typ:       ChunkTypeRaw,
					blocks:    uint32(take),
					srcOffset: srcOff,
					srcBytes:  takeSrcBytes,
					dataOff:   cur.encodedBytes,
					totalLen:  chunkTotal,
				})
				cur.encodedBytes += chunkTotal
				cur.writtenBlocks += take
				curBlock += take
				srcOff += takeSrcBytes
				remSrcBytes -= takeSrcBytes
				remBlocks -= take
			}
		}
	}
	flush()

	// Degenerate sparse input consisting solely of DONT_CARE chunks.
	if len(drafts) == 0 && totalBlocks > 0 {
		d := draftPiece{
			startBlock:    0,
			writtenBlocks: totalBlocks,
			chunks: []pieceChunk{{
				typ:      ChunkTypeDontCare,
				blocks:   uint32(totalBlocks),
				dataOff:  int64(FileHeaderSize),
				totalLen: int64(ChunkHeaderSize),
			}},
			encodedBytes: int64(FileHeaderSize) + int64(ChunkHeaderSize),
		}
		if d.encodedBytes+crcReserve > maxBytes {
			return nil, fmt.Errorf("%w: cannot fit DONT_CARE piece in %d bytes", ErrLimitTooSmall, maxBytes)
		}
		drafts = append(drafts, d)
	}

	pieces := make([]*Piece, 0, len(drafts))
	scratch := make([]byte, minUint32(blkSize, 64<<10))
	zeroBuf := make([]byte, minUint32(blkSize, 64<<10))

	for idx, d := range drafts {
		endBlk := d.startBlock + d.writtenBlocks
		if endBlk > uint64(^uint32(0)) {
			return nil, fmt.Errorf("sparse: piece %d end block %d overflows uint32", idx, endBlk)
		}
		if includeCRC {
			crc, err := computePieceCRC(src, blkSize, d.chunks, scratch, zeroBuf)
			if err != nil {
				return nil, err
			}
			d.chunks = append(d.chunks, pieceChunk{
				typ:      ChunkTypeCRC32,
				crc32:    crc,
				dataOff:  d.encodedBytes,
				totalLen: int64(ChunkHeaderSize) + 4,
			})
			d.encodedBytes += int64(ChunkHeaderSize) + 4
		}
		offsets := make([]int64, len(d.chunks))
		for i, c := range d.chunks {
			offsets[i] = c.dataOff
		}
		hdr := MarshalHeader(Header{
			MajorVersion: MajorVersion,
			BlockSize:    blkSize,
			TotalBlocks:  uint32(endBlk),
			TotalChunks:  uint32(len(d.chunks)),
		})
		pieces = append(pieces, &Piece{
			Index:         idx,
			TotalPieces:   len(drafts),
			StartBlock:    d.startBlock,
			WrittenBlocks: d.writtenBlocks,
			TotalBlocks:   uint32(endBlk),
			BlockSize:     blkSize,
			EncodedSize:   d.encodedBytes,
			LogicalOffset: int64(d.startBlock) * int64(blkSize),
			LogicalSize:   int64(d.writtenBlocks) * int64(blkSize),
			ChunkCount:    uint32(len(d.chunks)),
			src:           src,
			header:        hdr,
			chunks:        d.chunks,
			offsets:       offsets,
		})
	}
	return pieces, nil
}

func computePieceCRC(src io.ReaderAt, blkSize uint32, chunks []pieceChunk, scratch, zeroBuf []byte) (uint32, error) {
	var crc uint32
	for _, c := range chunks {
		switch c.typ {
		case ChunkTypeDontCare:
			crc = crcUpdateZeroes(crc, uint64(c.blocks)*uint64(blkSize), zeroBuf)
		case ChunkTypeFill:
			crc = crcUpdatePattern(crc, c.fill, uint64(c.blocks)*uint64(blkSize), scratch)
		case ChunkTypeRaw:
			total := int64(c.blocks) * int64(blkSize)
			var err error
			crc, err = crcUpdateReaderAt(crc, src, c.srcOffset, c.srcBytes, scratch)
			if err != nil {
				return 0, err
			}
			if total > c.srcBytes {
				crc = crcUpdateZeroes(crc, uint64(total-c.srcBytes), zeroBuf)
			}
		}
	}
	return crc, nil
}

func crcUpdateReaderAt(crc uint32, src io.ReaderAt, off, n int64, scratch []byte) (uint32, error) {
	var rem = n
	pos := off
	for rem > 0 {
		step := int64(len(scratch))
		if rem < step {
			step = rem
		}
		if err := readAtExact(src, pos, scratch[:step]); err != nil {
			return 0, err
		}
		crc = crc32.Update(crc, crc32.IEEETable, scratch[:step])
		pos += step
		rem -= step
	}
	return crc, nil
}

func crcUpdatePattern(crc uint32, pat [4]byte, total uint64, scratch []byte) uint32 {
	if total == 0 {
		return crc
	}
	fillLen := len(scratch) - (len(scratch) % 4)
	if fillLen == 0 {
		fillLen = 4
	}
	for i := 0; i < fillLen; i += 4 {
		copy(scratch[i:i+4], pat[:])
	}
	for total > 0 {
		step := uint64(fillLen)
		if total < step {
			step = total
		}
		crc = crc32.Update(crc, crc32.IEEETable, scratch[:step])
		total -= step
	}
	return crc
}

func crcUpdateZeroes(crc uint32, total uint64, zeroBuf []byte) uint32 {
	for total > 0 {
		step := uint64(len(zeroBuf))
		if total < step {
			step = total
		}
		crc = crc32.Update(crc, crc32.IEEETable, zeroBuf[:step])
		total -= step
	}
	return crc
}

func copyReaderAtToWriterAt(dst io.WriterAt, dstOff int64, src io.ReaderAt, srcOff, n int64, scratch []byte) error {
	for n > 0 {
		step := int64(len(scratch))
		if n < step {
			step = n
		}
		if err := readAtExact(src, srcOff, scratch[:step]); err != nil {
			return err
		}
		if err := writeAtExact(dst, dstOff, scratch[:step]); err != nil {
			return err
		}
		srcOff += step
		dstOff += step
		n -= step
	}
	return nil
}

func writePattern(dst io.WriterAt, dstOff int64, pat [4]byte, n int64, scratch []byte) error {
	fillLen := len(scratch) - (len(scratch) % 4)
	if fillLen == 0 {
		fillLen = 4
	}
	for i := 0; i < fillLen; i += 4 {
		copy(scratch[i:i+4], pat[:])
	}
	for n > 0 {
		step := int64(fillLen)
		if n < step {
			step = n
		}
		if err := writeAtExact(dst, dstOff, scratch[:step]); err != nil {
			return err
		}
		dstOff += step
		n -= step
	}
	return nil
}

func readAtExact(src io.ReaderAt, off int64, dst []byte) error {
	got := 0
	for got < len(dst) {
		n, err := src.ReadAt(dst[got:], off+int64(got))
		got += n
		if err != nil {
			if errors.Is(err, io.EOF) && got == len(dst) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func writeAtExact(dst io.WriterAt, off int64, src []byte) error {
	written := 0
	for written < len(src) {
		n, err := dst.WriteAt(src[written:], off+int64(written))
		written += n
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// ReconstructPieces applies pieces sequentially to dst, simulating U-Boot's
// multi-download sparse flashing behavior (DONT_CARE skips without erasing or
// zeroing previously written or untouched ranges). Intended for testing and
// verification.
func ReconstructPieces(dst io.WriterAt, pieces []*Piece) error {
	for _, p := range pieces {
		if _, err := Decode(dst, p, p.EncodedSize, DecodeOptions{ZeroDontCare: false}); err != nil {
			return fmt.Errorf("piece %d: %w", p.Index, err)
		}
	}
	return nil
}

var _ = bytes.Compare // keep bytes available if needed
