package sparse_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto/sparse"
)

type memWriterAt struct {
	buf []byte
}

func (m *memWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(m.buf)) {
		return 0, errors.New("out of range")
	}
	copy(m.buf[off:], p)
	return len(p), nil
}

func makeRawPatternImage(blocks int, blkSize int) []byte {
	out := make([]byte, blocks*blkSize)
	for b := 0; b < blocks; b++ {
		off := b * blkSize
		switch {
		case b == 0 || b == blocks-1 || b%5 == 2:
			// Non-uniform raw data block.
			for i := 0; i < blkSize; i++ {
				out[off+i] = byte((b*37 + i*13) ^ 0x5a)
			}
		case b%5 == 0:
			// Uniform all-zero block (must NOT become DONT_CARE in raw encoding!).
		case b%5 == 1:
			// Uniform non-zero 4-byte pattern -> FILL.
			pat := []byte{0xDE, 0xAD, 0xBE, 0xEF}
			for i := 0; i < blkSize; i += 4 {
				copy(out[off+i:], pat)
			}
		default:
			// Another distinct 4-byte pattern.
			pat := []byte{0x11, 0x22, 0x33, byte(b)}
			for i := 0; i < blkSize; i += 4 {
				copy(out[off+i:], pat)
			}
		}
	}
	return out
}

func collectPieces(t *testing.T, iter *sparse.Iterator) []*sparse.Piece {
	t.Helper()
	var out []*sparse.Piece
	for {
		p, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("iter.Next: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestEncodeDecodeAndCRC32(t *testing.T) {
	raw := makeRawPatternImage(16, 4096)
	var sp bytes.Buffer
	if err := sparse.Encode(&sp, bytes.NewReader(raw), int64(len(raw)), sparse.EncodeOptions{
		BlockSize:  4096,
		IncludeCRC: true,
	}); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	spBytes := sp.Bytes()
	ok, err := sparse.IsSparse(bytes.NewReader(spBytes), int64(len(spBytes)))
	if err != nil || !ok {
		t.Fatalf("IsSparse = (%v, %v), want (true, nil)", ok, err)
	}

	h, chunks, err := sparse.ParseChunks(bytes.NewReader(spBytes), int64(len(spBytes)))
	if err != nil {
		t.Fatalf("ParseChunks: %v", err)
	}
	if h.TotalBlocks != 16 || h.BlockSize != 4096 {
		t.Fatalf("unexpected header: %+v", h)
	}
	// Verify no DONT_CARE was emitted for zero blocks of a raw image.
	for _, c := range chunks {
		if c.Type == sparse.ChunkTypeDontCare {
			t.Fatalf("raw encode unexpectedly emitted DONT_CARE chunk: %+v", c)
		}
	}

	reconstructed := make([]byte, len(raw))
	for i := range reconstructed {
		reconstructed[i] = 0xAA // sentinel
	}
	if _, err := sparse.Decode(&memWriterAt{buf: reconstructed}, bytes.NewReader(spBytes), int64(len(spBytes)), sparse.DecodeOptions{}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(reconstructed, raw) {
		t.Fatal("decoded image differs from raw input")
	}

	// Corrupt a payload byte -> CRC32 chunk must catch it.
	bad := append([]byte(nil), spBytes...)
	bad[sparse.FileHeaderSize+sparse.ChunkHeaderSize+5] ^= 0x01
	if _, _, err := sparse.ParseChunks(bytes.NewReader(bad), int64(len(bad))); err == nil {
		t.Fatal("expected CRC32 mismatch on corrupted sparse payload")
	}
}

func TestResparseBoundaryAndMultiPieceReconstruction(t *testing.T) {
	const blkSize = 4096
	raw := makeRawPatternImage(30, blkSize)

	// Exact boundary: encode single piece, measure exact size, test exact cap vs cap-1.
	var single bytes.Buffer
	if err := sparse.Encode(&single, bytes.NewReader(raw), int64(len(raw)), sparse.EncodeOptions{BlockSize: blkSize}); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	exactCap := int64(single.Len())

	iterExact, err := sparse.NewResparse(bytes.NewReader(raw), int64(len(raw)), sparse.ResparseOptions{
		MaxPieceBytes: exactCap,
		BlockSize:     blkSize,
	})
	if err != nil {
		t.Fatalf("NewResparse(exactCap): %v", err)
	}
	if iterExact.Count() != 1 {
		t.Fatalf("exactCap piece count = %d, want 1", iterExact.Count())
	}

	// Cap smaller than exact size forces multi-piece split.
	smallCap := int64(sparse.FileHeaderSize + sparse.ChunkHeaderSize + sparse.ChunkHeaderSize + 3*blkSize)
	iterSplit, err := sparse.NewResparse(bytes.NewReader(raw), int64(len(raw)), sparse.ResparseOptions{
		MaxPieceBytes: smallCap,
		BlockSize:     blkSize,
		IncludeCRC:    true,
	})
	if err != nil {
		t.Fatalf("NewResparse(smallCap): %v", err)
	}
	pieces := collectPieces(t, iterSplit)
	if len(pieces) < 3 {
		t.Fatalf("expected >= 3 pieces for smallCap=%d, got %d", smallCap, len(pieces))
	}

	for _, p := range pieces {
		if p.EncodedSize > smallCap {
			t.Fatalf("piece %d EncodedSize %d exceeds limit %d", p.Index, p.EncodedSize, smallCap)
		}
		rawPiece, err := io.ReadAll(p.Open())
		if err != nil {
			t.Fatalf("read piece %d: %v", p.Index, err)
		}
		if int64(len(rawPiece)) != p.EncodedSize {
			t.Fatalf("piece %d streamed %d bytes != EncodedSize %d", p.Index, len(rawPiece), p.EncodedSize)
		}
		if _, _, err := sparse.ParseChunks(bytes.NewReader(rawPiece), int64(len(rawPiece))); err != nil {
			t.Fatalf("piece %d failed ParseChunks: %v", p.Index, err)
		}
	}

	// Simulate U-Boot flashing Piece 0..N onto a pre-filled 0xA5 buffer:
	// earlier pieces must NOT be erased and zero blocks MUST overwrite 0xA5 with 0x00.
	target := make([]byte, len(raw))
	for i := range target {
		target[i] = 0xA5
	}
	if err := sparse.ReconstructPieces(&memWriterAt{buf: target}, pieces); err != nil {
		t.Fatalf("ReconstructPieces: %v", err)
	}
	if sha256.Sum256(target) != sha256.Sum256(raw) {
		t.Fatal("reconstructed multi-piece image SHA-256 differs from original raw image")
	}
}

func TestResparsePreservesDontCareOnAlreadySparseInput(t *testing.T) {
	// Build an already-sparse image:
	// [0..2) RAW, [2..6) DONT_CARE, [6..10) FILL(0xCAFEBABE), [10..12) RAW
	const blkSize = 4096
	rawBlock := bytes.Repeat([]byte{0x33, 0x44, 0x55, 0x66}, blkSize/4)
	// Make rawBlock non-uniform so it stays RAW.
	rawBlock[0] = 0x99

	var sp bytes.Buffer
	hdr := sparse.MarshalHeader(sparse.Header{
		MajorVersion: sparse.MajorVersion,
		BlockSize:    blkSize,
		TotalBlocks:  12,
		TotalChunks:  4,
	})
	sp.Write(hdr[:])
	c0 := sparse.MarshalChunkHeader(sparse.ChunkTypeRaw, 2, uint32(sparse.ChunkHeaderSize)+2*blkSize)
	sp.Write(c0[:])
	sp.Write(rawBlock)
	sp.Write(rawBlock)
	c1 := sparse.MarshalChunkHeader(sparse.ChunkTypeDontCare, 4, uint32(sparse.ChunkHeaderSize))
	sp.Write(c1[:])
	c2 := sparse.MarshalChunkHeader(sparse.ChunkTypeFill, 4, uint32(sparse.ChunkHeaderSize)+4)
	sp.Write(c2[:])
	sp.Write([]byte{0xCA, 0xFE, 0xBA, 0xBE})
	c3 := sparse.MarshalChunkHeader(sparse.ChunkTypeRaw, 2, uint32(sparse.ChunkHeaderSize)+2*blkSize)
	sp.Write(c3[:])
	sp.Write(rawBlock)
	sp.Write(rawBlock)

	capBytes := int64(sparse.FileHeaderSize + 2*sparse.ChunkHeaderSize + 2*blkSize)
	iter, err := sparse.NewResparse(bytes.NewReader(sp.Bytes()), int64(sp.Len()), sparse.ResparseOptions{
		MaxPieceBytes: capBytes,
	})
	if err != nil {
		t.Fatalf("NewResparse(sparse): %v", err)
	}
	pieces := collectPieces(t, iter)
	if len(pieces) < 2 {
		t.Fatalf("expected >= 2 pieces, got %d", len(pieces))
	}

	target := bytes.Repeat([]byte{0x77}, 12*blkSize)
	if err := sparse.ReconstructPieces(&memWriterAt{buf: target}, pieces); err != nil {
		t.Fatalf("ReconstructPieces: %v", err)
	}
	// Verify DONT_CARE blocks [2..6) retained sentinel 0x77 untouched.
	for b := 2; b < 6; b++ {
		slice := target[b*blkSize : (b+1)*blkSize]
		if !bytes.Equal(slice, bytes.Repeat([]byte{0x77}, blkSize)) {
			t.Fatalf("DONT_CARE block %d was modified", b)
		}
	}
	// Verify FILL blocks [6..10).
	for b := 6; b < 10; b++ {
		if !bytes.Equal(target[b*blkSize:b*blkSize+4], []byte{0xCA, 0xFE, 0xBA, 0xBE}) {
			t.Fatalf("FILL block %d mismatch", b)
		}
	}
	// Verify RAW blocks [0..2) and [10..12).
	if !bytes.Equal(target[0:blkSize], rawBlock) || !bytes.Equal(target[10*blkSize:11*blkSize], rawBlock) {
		t.Fatal("RAW blocks mismatch")
	}
}

// syntheticLargeReader simulates a >4 GiB raw image in constant memory with
// non-zero raw islands at offset 0, 2 GiB, and 4.25 GiB.
type syntheticLargeReader struct {
	size    int64
	blkSize int64
	islands map[int64][]byte
}

func (s *syntheticLargeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := len(p)
	eof := false
	if off+int64(n) > s.size {
		n = int(s.size - off)
		eof = true
	}
	clear(p[:n])
	for blkOff, data := range s.islands {
		end := blkOff + int64(len(data))
		if off < end && off+int64(n) > blkOff {
			lo := max(off, blkOff)
			hi := min(off+int64(n), end)
			copy(p[lo-off:hi-off], data[lo-blkOff:hi-blkOff])
		}
	}
	if eof {
		return n, io.EOF
	}
	return n, nil
}

func TestLargeLogicalImageOver4GiB(t *testing.T) {
	const blkSize = 4096
	const totalSize = (4 << 30) + (256 << 20) // 4.25 GiB
	island := func(seed byte) []byte {
		b := make([]byte, 3*blkSize)
		for i := range b {
			b[i] = byte(i) ^ seed
		}
		return b
	}
	islands := map[int64][]byte{
		0:                 island(0x11),
		2 << 30:           island(0x22),
		(4 << 30) + 65536: island(0x33),
	}
	// Sparse scan over 4.25 GiB would take a couple seconds if scanning every block;
	// verify large BlockSize (e.g. 64 KiB) or 4 KiB? 4.25 GiB / 1 MiB = 4352 iterations,
	// fast (~0.2s) even with 4 KiB blocks!
	r := &syntheticLargeReader{size: totalSize, blkSize: blkSize, islands: islands}
	const cap512MiB = 0x20000000
	iter, err := sparse.NewResparse(r, totalSize, sparse.ResparseOptions{
		MaxPieceBytes: cap512MiB,
		BlockSize:     blkSize,
	})
	if err != nil {
		t.Fatalf("NewResparse(4.25GiB): %v", err)
	}
	pieces := collectPieces(t, iter)
	if len(pieces) == 0 {
		t.Fatal("expected at least 1 piece")
	}
	last := pieces[len(pieces)-1]
	wantTotalBlocks := uint64(totalSize / blkSize)
	if uint64(last.TotalBlocks) != wantTotalBlocks {
		t.Fatalf("last.TotalBlocks = %d, want %d", last.TotalBlocks, wantTotalBlocks)
	}
	for _, p := range pieces {
		if p.EncodedSize > cap512MiB {
			t.Fatalf("piece %d size %d exceeds 512 MiB", p.Index, p.EncodedSize)
		}
	}
	// Independently verify island bytes streamed from the pieces.
	checked := 0
	for _, p := range pieces {
		rawPiece, err := io.ReadAll(p.Open())
		if err != nil {
			t.Fatalf("read piece %d: %v", p.Index, err)
		}
		_, chunks, err := sparse.ParseChunks(bytes.NewReader(rawPiece), int64(len(rawPiece)))
		if err != nil {
			t.Fatalf("ParseChunks piece %d: %v", p.Index, err)
		}
		for _, c := range chunks {
			if c.Type != sparse.ChunkTypeRaw {
				continue
			}
			logicalOff := int64(c.LogicalBlk) * blkSize
			if want, ok := islands[logicalOff]; ok {
				got := rawPiece[c.DataOffset : c.DataOffset+int64(len(want))]
				if !bytes.Equal(got, want) {
					t.Fatalf("island at offset %d mismatch", logicalOff)
				}
				checked++
			}
		}
	}
	if checked != len(islands) {
		t.Fatalf("verified %d islands, want %d", checked, len(islands))
	}
}

func TestUnalignedRawAndRemainderBlocks(t *testing.T) {
	const blkSize = 4096
	raw := make([]byte, 3*blkSize+123)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}

	if _, err := sparse.NewResparse(bytes.NewReader(raw), int64(len(raw)), sparse.ResparseOptions{
		MaxPieceBytes: 64 << 10,
		BlockSize:     blkSize,
		PadLastBlock:  false,
	}); !errors.Is(err, sparse.ErrUnalignedSize) {
		t.Fatalf("expected ErrUnalignedSize, got %v", err)
	}

	// Small piece budget forces a split mid-RAW leaving remainder blocks.
	capBytes := int64(sparse.FileHeaderSize + sparse.ChunkHeaderSize + sparse.ChunkHeaderSize + 2*blkSize)
	iter, err := sparse.NewResparse(bytes.NewReader(raw), int64(len(raw)), sparse.ResparseOptions{
		MaxPieceBytes: capBytes,
		BlockSize:     blkSize,
		PadLastBlock:  true,
	})
	if err != nil {
		t.Fatalf("NewResparse(PadLastBlock): %v", err)
	}
	pieces := collectPieces(t, iter)
	if len(pieces) != 2 {
		t.Fatalf("expected 2 pieces, got %d", len(pieces))
	}
	if pieces[0].WrittenBlocks != 2 || pieces[1].WrittenBlocks != 2 {
		t.Fatalf("unexpected written blocks: p0=%d p1=%d", pieces[0].WrittenBlocks, pieces[1].WrittenBlocks)
	}

	out := make([]byte, 4*blkSize)
	if err := sparse.ReconstructPieces(&memWriterAt{buf: out}, pieces); err != nil {
		t.Fatalf("ReconstructPieces: %v", err)
	}
	if !bytes.Equal(out[:len(raw)], raw) {
		t.Fatal("reconstructed unaligned bytes mismatch")
	}
	if !bytes.Equal(out[len(raw):], make([]byte, 4*blkSize-len(raw))) {
		t.Fatal("trailing pad bytes are not zero")
	}
}

func TestParseRejectsCorruptAndTruncatedInputs(t *testing.T) {
	valid := makeRawPatternImage(2, 4096)
	var buf bytes.Buffer
	if err := sparse.Encode(&buf, bytes.NewReader(valid), int64(len(valid)), sparse.EncodeOptions{BlockSize: 4096}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()

	// Truncated header.
	if _, _, err := sparse.ParseChunks(bytes.NewReader(b[:10]), 10); err == nil {
		t.Fatal("expected error for truncated header")
	}
	// Truncated payload.
	if _, _, err := sparse.ParseChunks(bytes.NewReader(b[:len(b)-5]), int64(len(b)-5)); err == nil {
		t.Fatal("expected error for truncated chunk")
	}
	// Trailing garbage.
	withTail := append(append([]byte(nil), b...), 0x00)
	if _, _, err := sparse.ParseChunks(bytes.NewReader(withTail), int64(len(withTail))); err == nil {
		t.Fatal("expected error for trailing garbage")
	}
	// Bad ImageChecksum in header.
	badHdrCRC := append([]byte(nil), b...)
	binary.LittleEndian.PutUint32(badHdrCRC[24:28], crc32.ChecksumIEEE([]byte("wrong")))
	if _, _, err := sparse.ParseChunks(bytes.NewReader(badHdrCRC), int64(len(badHdrCRC))); err == nil {
		t.Fatal("expected error for bad header image_checksum")
	}
	// Unknown chunk type.
	badType := append([]byte(nil), b...)
	binary.LittleEndian.PutUint16(badType[sparse.FileHeaderSize:sparse.FileHeaderSize+2], 0xDEAD)
	if _, _, err := sparse.ParseChunks(bytes.NewReader(badType), int64(len(badType))); err == nil {
		t.Fatal("expected error for unknown chunk type")
	}
	// Too-small piece limit.
	if _, err := sparse.NewResparse(bytes.NewReader(valid), int64(len(valid)), sparse.ResparseOptions{
		MaxPieceBytes: 100,
		BlockSize:     4096,
	}); !errors.Is(err, sparse.ErrLimitTooSmall) {
		t.Fatalf("expected ErrLimitTooSmall, got %v", err)
	}
}
