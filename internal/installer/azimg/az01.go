package azimg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"

	"github.com/ulikunitz/xz"
)

const (
	az01MaxStringLen = 4096
	az01MaxArrayLen  = 32
)

var (
	az01EOFFixed = [16]byte{
		'E', 'O', 'F', 0,
		16, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
	}
	partSpecs = [3]struct {
		tag  [8]byte
		name string
		comp string
	}{
		{tag: [8]byte{'P', 'A', 'R', 'T', 'P', 0, 0, 0}, name: "splash", comp: "none"},
		{tag: [8]byte{'P', 'A', 'R', 'T', 'X', 0, 0, 0}, name: "recoverysplash", comp: "none"},
		{tag: [8]byte{'P', 'A', 'R', 'T', 'L', 0, 0, 0}, name: "rootfs", comp: "xz"},
	}
)

type sectionCursor struct {
	r    io.ReaderAt
	size int64
	pos  int64
}

func (c *sectionCursor) take(n int64) ([]byte, error) {
	if n < 0 || n > c.size-c.pos {
		return nil, fmt.Errorf("%w: truncated AZ01 container at offset %d (need %d, remaining %d)", ErrCorrupt, c.pos, n, c.size-c.pos)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(c.r, c.pos, n), buf); err != nil {
		return nil, fmt.Errorf("%w: short read at offset %d: %v", ErrCorrupt, c.pos, err)
	}
	c.pos += n
	return buf, nil
}

func (c *sectionCursor) u32() (uint32, error) {
	b, err := c.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (c *sectionCursor) u64() (uint64, error) {
	b, err := c.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (c *sectionCursor) str() (string, error) {
	n, err := c.u32()
	if err != nil {
		return "", err
	}
	if n > az01MaxStringLen {
		return "", fmt.Errorf("%w: oversized AZ01 string (%d bytes)", ErrLimitExceeded, n)
	}
	pad := (4 - ((int64(n) + 1) % 4)) % 4
	raw, err := c.take(int64(n) + 1 + pad)
	if err != nil {
		return "", err
	}
	if raw[n] != 0 {
		return "", fmt.Errorf("%w: missing AZ01 string NUL terminator", ErrCorrupt)
	}
	for _, b := range raw[n+1:] {
		if b != 0 {
			return "", fmt.Errorf("%w: nonzero AZ01 string padding", ErrCorrupt)
		}
	}
	for _, b := range raw[:n] {
		if b == 0 || b >= 0x80 {
			return "", fmt.Errorf("%w: non-ASCII or NUL byte in AZ01 container string", ErrCorrupt)
		}
	}
	return string(raw[:n]), nil
}

func openAZ01(ctx context.Context, r io.ReaderAt, size int64, limits Limits) (*Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size > limits.MaxAZ01Size {
		return nil, fmt.Errorf("%w: AZ01 image size %d exceeds limit %d", ErrLimitExceeded, size, limits.MaxAZ01Size)
	}
	c := &sectionCursor{r: r, size: size}
	magic, err := c.take(4)
	if err != nil {
		return nil, err
	}
	if string(magic) != "AZ01" {
		return nil, fmt.Errorf("%w: expected AZ01 magic, got %q", ErrInvalidFormat, string(magic))
	}
	wireVersion, err := c.u32()
	if err != nil {
		return nil, err
	}
	if wireVersion != 1 {
		return nil, fmt.Errorf("%w: unsupported AZ01 wire version %d", ErrUnsupported, wireVersion)
	}
	tableOffset, err := c.u32()
	if err != nil {
		return nil, err
	}
	build, err := c.str()
	if err != nil {
		return nil, err
	}
	compatCount, err := c.u32()
	if err != nil {
		return nil, err
	}
	if compatCount < 1 || compatCount > az01MaxArrayLen {
		return nil, fmt.Errorf("%w: invalid AZ01 compatibility count %d", ErrCorrupt, compatCount)
	}
	compatible := make([]string, compatCount)
	for i := range compatible {
		s, err := c.str()
		if err != nil {
			return nil, err
		}
		compatible[i] = s
	}
	usbCount, err := c.u32()
	if err != nil {
		return nil, err
	}
	if usbCount < 1 || usbCount > az01MaxArrayLen {
		return nil, fmt.Errorf("%w: invalid AZ01 USB device count %d", ErrCorrupt, usbCount)
	}
	usbDevices := make([]uint32, usbCount)
	for i := range usbDevices {
		id, err := c.u32()
		if err != nil {
			return nil, err
		}
		usbDevices[i] = id
	}
	description, err := c.str()
	if err != nil {
		return nil, err
	}
	if c.pos != int64(tableOffset) {
		return nil, fmt.Errorf("%w: unrecognized AZ01 header extensions (pos=%d, table=%d)", ErrCorrupt, c.pos, tableOffset)
	}
	var models []Model
	for _, comp := range compatible {
		if m, ok := LookupGen1Compatible(comp); ok {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: no supported model in compatible list %v", ErrModelMismatch, compatible)
	}

	parts := make([]Partition, 0, len(partSpecs))
	buf := make([]byte, 1<<20)
	for _, spec := range partSpecs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entryOffset := c.pos
		tag, err := c.take(8)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(tag, spec.tag[:]) {
			return nil, fmt.Errorf("%w: unexpected AZ01 partition record %q (expected %q)", ErrCorrupt, string(tag), string(spec.tag[:]))
		}
		partSizeU64, err := c.u64()
		if err != nil {
			return nil, err
		}
		actualName, err := c.str()
		if err != nil {
			return nil, err
		}
		if actualName != spec.name {
			return nil, fmt.Errorf("%w: unexpected AZ01 partition %q (expected %q)", ErrCorrupt, actualName, spec.name)
		}
		comp, err := c.str()
		if err != nil {
			return nil, err
		}
		if comp != spec.comp {
			return nil, fmt.Errorf("%w: unsupported compression %q for partition %s", ErrCorrupt, comp, spec.name)
		}
		hashCount, err := c.u32()
		if err != nil {
			return nil, err
		}
		hashAlgo, err := c.str()
		if err != nil {
			return nil, err
		}
		digestLen, err := c.u32()
		if err != nil {
			return nil, err
		}
		if hashCount != 1 || hashAlgo != "sha1" || digestLen != 20 {
			return nil, fmt.Errorf("%w: unsupported AZ01 hash/signature layout (count=%d, algo=%q, len=%d)", ErrCorrupt, hashCount, hashAlgo, digestLen)
		}
		digestOffset := c.pos
		digestBytes, err := c.take(20)
		if err != nil {
			return nil, err
		}
		var wantSHA1 [20]byte
		copy(wantSHA1[:], digestBytes)

		payloadOffset := c.pos
		if partSizeU64 > math.MaxInt64 || partSizeU64 > uint64(c.size-payloadOffset) {
			return nil, fmt.Errorf("%w: truncated AZ01 partition %s (size=%d, remaining=%d)", ErrCorrupt, spec.name, partSizeU64, c.size-payloadOffset)
		}
		partSize := int64(partSizeU64)
		if err := hashSectionSHA1(ctx, r, payloadOffset, partSize, wantSHA1, buf); err != nil {
			return nil, fmt.Errorf("%w: partition %s: %v", ErrDigestMismatch, spec.name, err)
		}
		c.pos = payloadOffset + partSize
		parts = append(parts, Partition{
			Name:         spec.name,
			Compression:  comp,
			EntryOffset:  entryOffset,
			Offset:       payloadOffset,
			Size:         partSize,
			DigestOffset: digestOffset,
			SHA1:         wantSHA1,
		})
	}

	eofOffset := c.pos
	pad := (8 - (c.pos % 8)) % 8
	if pad > 0 && c.pos+pad+4 <= c.size {
		peek, err := c.take(pad + 4)
		if err != nil {
			return nil, err
		}
		allZero := true
		for _, b := range peek[:pad] {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero && string(peek[pad:]) == "EOF\x00" {
			// Rewind 4 bytes so the 16-byte EOF check consumes the full EOF marker.
			c.pos -= 4
		} else {
			c.pos -= pad + 4
		}
	}
	aligned := c.pos%8 == 0
	eofBytes, err := c.take(16)
	if err != nil {
		return nil, fmt.Errorf("%w: missing or truncated EOF trailer: %v", ErrCorrupt, err)
	}
	if !bytes.Equal(eofBytes, az01EOFFixed[:]) || c.pos != c.size {
		return nil, fmt.Errorf("%w: unexpected AZ01 EOF/trailer/signature (pos=%d, size=%d)", ErrCorrupt, c.pos, c.size)
	}

	return &Image{
		r:      r,
		size:   size,
		limits: limits,
		format: FormatAZ01,
		models: models,
		az01: &AZ01Metadata{
			WireVersion: wireVersion,
			Build:       build,
			Compatible:  compatible,
			USBDevices:  usbDevices,
			Description: description,
			TableOffset: int64(tableOffset),
			EOFOffset:   eofOffset,
			Aligned:     aligned,
			Partitions:  parts,
		},
	}, nil
}

func hashSectionSHA1(ctx context.Context, r io.ReaderAt, offset, size int64, want [20]byte, buf []byte) error {
	h := sha1.New()
	sr := io.NewSectionReader(r, offset, size)
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := sr.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != size {
		return fmt.Errorf("short read (%d of %d bytes)", total, size)
	}
	var got [20]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return fmt.Errorf("SHA-1 mismatch (got %x, want %x)", got, want)
	}
	return nil
}

// ExtractPartition streams the raw verified payload of a named AZ01 partition
// ("splash", "recoverysplash", or "rootfs") into w.
func (im *Image) ExtractPartition(ctx context.Context, name string, w io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if im.format != FormatAZ01 || im.az01 == nil {
		return 0, fmt.Errorf("%w: ExtractPartition requires an AZ01 image", ErrUnsupported)
	}
	var part *Partition
	for i := range im.az01.Partitions {
		if im.az01.Partitions[i].Name == name {
			part = &im.az01.Partitions[i]
			break
		}
	}
	if part == nil {
		return 0, fmt.Errorf("%w: partition %q not found", ErrCorrupt, name)
	}
	h := sha1.New()
	sr := io.NewSectionReader(im.r, part.Offset, part.Size)
	buf := make([]byte, 1<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, err := sr.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			nw, werr := w.Write(buf[:n])
			written += int64(nw)
			if werr != nil {
				return written, werr
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, err
		}
	}
	if written != part.Size {
		return written, fmt.Errorf("%w: truncated partition read (%d of %d)", ErrCorrupt, written, part.Size)
	}
	var got [20]byte
	copy(got[:], h.Sum(nil))
	if got != part.SHA1 {
		return written, fmt.Errorf("%w: partition %s SHA-1 mismatch", ErrDigestMismatch, name)
	}
	return written, nil
}

// ExtractRootfs verifies and decodes the single-stream XZ rootfs partition of an AZ01
// image into w, enforcing output size, dictionary/memory, integrity check, and trailing-data bounds.
func (im *Image) ExtractRootfs(ctx context.Context, w io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if im.format != FormatAZ01 || im.az01 == nil {
		return 0, fmt.Errorf("%w: ExtractRootfs requires an AZ01 image", ErrUnsupported)
	}
	var part *Partition
	for i := range im.az01.Partitions {
		if im.az01.Partitions[i].Name == "rootfs" {
			part = &im.az01.Partitions[i]
			break
		}
	}
	if part == nil {
		return 0, fmt.Errorf("%w: rootfs partition missing", ErrCorrupt)
	}
	if err := preflightXZStream(im.r, part.Offset, part.Size, im.limits); err != nil {
		return 0, err
	}

	sr := io.NewSectionReader(im.r, part.Offset, part.Size)
	// Buffer underlying SectionReader reads: ulikunitz/xz's lzma breader reads
	// one byte at a time when the reader does not buffer, which would otherwise
	// turn a 150 MiB rootfs extraction into ~150M pread syscalls.
	br := bufio.NewReaderSize(sr, 1<<20)
	h := sha1.New()
	cr := &countingSHA1Reader{r: io.TeeReader(br, h)}

	xzr, err := xz.ReaderConfig{
		SingleStream: true,
	}.NewReader(cr)
	if err != nil {
		return 0, fmt.Errorf("%w: XZ header error: %v", ErrCorrupt, err)
	}

	buf := make([]byte, 1<<20)
	var outBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return outBytes, err
		}
		n, rerr := xzr.Read(buf)
		if n > 0 {
			if outBytes+int64(n) > im.limits.MaxRootfsSize {
				return outBytes, fmt.Errorf("%w: rootfs exceeds %d byte safety limit", ErrLimitExceeded, im.limits.MaxRootfsSize)
			}
			nw, werr := w.Write(buf[:n])
			outBytes += int64(nw)
			if werr != nil {
				return outBytes, werr
			}
			if nw != n {
				return outBytes, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return outBytes, fmt.Errorf("%w: XZ decode failed: %v", ErrCorrupt, rerr)
		}
	}

	// SingleStream alone is not sufficient unless the remaining input is also checked:
	// ensure the entire PARTL payload was consumed (no trailing bytes or second stream).
	if cr.n != part.Size {
		return outBytes, fmt.Errorf("%w: trailing or concatenated data after XZ stream (%d of %d bytes consumed)", ErrCorrupt, cr.n, part.Size)
	}
	var got [20]byte
	copy(got[:], h.Sum(nil))
	if got != part.SHA1 {
		return outBytes, fmt.Errorf("%w: rootfs partition SHA-1 changed during extraction", ErrDigestMismatch)
	}
	return outBytes, nil
}

type countingSHA1Reader struct {
	r io.Reader
	n int64
}

func (c *countingSHA1Reader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// preflightXZStream validates the XZ stream header, footer, index, and every block header
// (including LZMA2 dictionary/memory limits and uncompressed size caps) before ulikunitz/xz
// allocates decoder buffers. Necessary because ulikunitz/xz's ReaderConfig.DictCap acts as
// a floor rather than a cap.
func preflightXZStream(r io.ReaderAt, offset, size int64, limits Limits) error {
	if size < 32 {
		return fmt.Errorf("%w: truncated XZ stream (%d bytes)", ErrCorrupt, size)
	}
	var hdr [12]byte
	if _, err := io.ReadFull(io.NewSectionReader(r, offset, 12), hdr[:]); err != nil {
		return fmt.Errorf("%w: reading XZ stream header: %v", ErrCorrupt, err)
	}
	if !bytes.Equal(hdr[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0}) {
		return fmt.Errorf("%w: invalid XZ magic", ErrCorrupt)
	}
	if hdr[6] != 0 || hdr[7]&0xf0 != 0 {
		return fmt.Errorf("%w: unsupported XZ stream flags %x %x", ErrCorrupt, hdr[6], hdr[7])
	}
	if crc32.ChecksumIEEE(hdr[6:8]) != binary.LittleEndian.Uint32(hdr[8:12]) {
		return fmt.Errorf("%w: invalid XZ stream header CRC32", ErrCorrupt)
	}
	checkID := hdr[7] & 0x0f
	var checkLen int64
	switch checkID {
	case 0x01:
		checkLen = 4 // CRC32
	case 0x04:
		checkLen = 8 // CRC64
	case 0x0a:
		checkLen = 32 // SHA-256
	default:
		return fmt.Errorf("%w: missing or unsupported XZ integrity check id 0x%02x", ErrCorrupt, checkID)
	}

	// Validate XZ stream footer at the exact end of the PARTL payload (rejects trailing
	// bytes, concatenated streams, and post-stream zero padding).
	var ftr [12]byte
	if _, err := io.ReadFull(io.NewSectionReader(r, offset+size-12, 12), ftr[:]); err != nil {
		return fmt.Errorf("%w: reading XZ stream footer: %v", ErrCorrupt, err)
	}
	if ftr[10] != 'Y' || ftr[11] != 'Z' {
		return fmt.Errorf("%w: invalid XZ stream footer magic or trailing data", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(ftr[4:10]) != binary.LittleEndian.Uint32(ftr[0:4]) {
		return fmt.Errorf("%w: invalid XZ stream footer CRC32", ErrCorrupt)
	}
	if ftr[8] != hdr[6] || ftr[9] != hdr[7] {
		return fmt.Errorf("%w: XZ stream footer flags mismatch", ErrCorrupt)
	}

	backwardSize := binary.LittleEndian.Uint32(ftr[4:8])
	indexSize := (int64(backwardSize) + 1) * 4
	const maxIndexSize = 4 << 20
	if indexSize < 8 || indexSize > maxIndexSize || 12+indexSize > size-12 {
		return fmt.Errorf("%w: invalid XZ index size %d", ErrCorrupt, indexSize)
	}
	indexStart := size - 12 - indexSize
	idx := make([]byte, indexSize)
	if _, err := io.ReadFull(io.NewSectionReader(r, offset+indexStart, indexSize), idx); err != nil {
		return fmt.Errorf("%w: reading XZ index: %v", ErrCorrupt, err)
	}
	if idx[0] != 0x00 {
		return fmt.Errorf("%w: invalid XZ index indicator 0x%02x", ErrCorrupt, idx[0])
	}
	if crc32.ChecksumIEEE(idx[:indexSize-4]) != binary.LittleEndian.Uint32(idx[indexSize-4:]) {
		return fmt.Errorf("%w: invalid XZ index CRC32", ErrCorrupt)
	}

	ip := 1
	idxBodyEnd := int(indexSize - 4)
	numRecords, n := readUvarintSlice(idx[ip:idxBodyEnd])
	if n <= 0 {
		return fmt.Errorf("%w: invalid XZ index record count", ErrCorrupt)
	}
	ip += n
	const maxPreflightBlocks = 65536
	if numRecords > maxPreflightBlocks {
		return fmt.Errorf("%w: XZ block count %d exceeds %d limit", ErrLimitExceeded, numRecords, maxPreflightBlocks)
	}

	type xzIndexRec struct {
		unpadded     int64
		uncompressed uint64
	}
	records := make([]xzIndexRec, 0, int(numRecords))
	var totalUncompressed uint64
	for i := uint64(0); i < numRecords; i++ {
		unpadded, n1 := readUvarintSlice(idx[ip:idxBodyEnd])
		if n1 <= 0 {
			return fmt.Errorf("%w: invalid XZ index unpadded size", ErrCorrupt)
		}
		ip += n1
		uncomp, n2 := readUvarintSlice(idx[ip:idxBodyEnd])
		if n2 <= 0 {
			return fmt.Errorf("%w: invalid XZ index uncompressed size", ErrCorrupt)
		}
		ip += n2
		if unpadded < 8 || unpadded > uint64(indexStart-12) {
			return fmt.Errorf("%w: XZ index unpadded size %d out of bounds", ErrCorrupt, unpadded)
		}
		if uncomp > uint64(limits.MaxRootfsSize) || totalUncompressed > uint64(limits.MaxRootfsSize)-uncomp {
			return fmt.Errorf("%w: XZ total uncompressed size exceeds %d limit", ErrLimitExceeded, limits.MaxRootfsSize)
		}
		totalUncompressed += uncomp
		records = append(records, xzIndexRec{unpadded: int64(unpadded), uncompressed: uncomp})
	}
	padLen := idxBodyEnd - ip
	if padLen < 0 || padLen > 3 {
		return fmt.Errorf("%w: invalid XZ index padding length %d", ErrCorrupt, padLen)
	}
	for _, b := range idx[ip:idxBodyEnd] {
		if b != 0 {
			return fmt.Errorf("%w: nonzero XZ index padding", ErrCorrupt)
		}
	}

	// Walk every block using the verified Index offsets so later blocks cannot bypass dictionary caps.
	pos := int64(12)
	for _, rec := range records {
		paddedBlock := ((rec.unpadded + 3) / 4) * 4
		if paddedBlock > indexStart-pos {
			return fmt.Errorf("%w: XZ block extends past index start", ErrCorrupt)
		}
		var b0 [1]byte
		if _, err := io.ReadFull(io.NewSectionReader(r, offset+pos, 1), b0[:]); err != nil {
			return fmt.Errorf("%w: reading XZ block header size: %v", ErrCorrupt, err)
		}
		if b0[0] == 0 {
			return fmt.Errorf("%w: unexpected index indicator inside XZ block", ErrCorrupt)
		}
		bhLen := (int64(b0[0]) + 1) * 4
		if bhLen < 8 || bhLen > 1024 || bhLen > rec.unpadded {
			return fmt.Errorf("%w: invalid XZ block header length %d", ErrCorrupt, bhLen)
		}
		bh := make([]byte, bhLen)
		if _, err := io.ReadFull(io.NewSectionReader(r, offset+pos, bhLen), bh); err != nil {
			return fmt.Errorf("%w: reading XZ block header: %v", ErrCorrupt, err)
		}
		if crc32.ChecksumIEEE(bh[:bhLen-4]) != binary.LittleEndian.Uint32(bh[bhLen-4:]) {
			return fmt.Errorf("%w: invalid XZ block header CRC32", ErrCorrupt)
		}
		flags := bh[1]
		if flags&0x3c != 0 {
			return fmt.Errorf("%w: reserved XZ block flags 0x%02x", ErrCorrupt, flags)
		}
		numFilters := int(flags&0x03) + 1
		p := 2
		if flags&0x40 != 0 {
			compSize, n := readUvarintSlice(bh[p : bhLen-4])
			if n <= 0 {
				return fmt.Errorf("%w: invalid XZ block compressed size", ErrCorrupt)
			}
			if compSize == 0 || compSize > uint64(rec.unpadded-bhLen-checkLen) || uint64(bhLen)+compSize+uint64(checkLen) != uint64(rec.unpadded) {
				return fmt.Errorf("%w: XZ block compressed size mismatch against index", ErrCorrupt)
			}
			p += n
		}
		if flags&0x80 != 0 {
			v, n := readUvarintSlice(bh[p : bhLen-4])
			if n <= 0 {
				return fmt.Errorf("%w: invalid XZ block uncompressed size", ErrCorrupt)
			}
			if v > uint64(limits.MaxRootfsSize) {
				return fmt.Errorf("%w: XZ block uncompressed size %d exceeds %d limit", ErrLimitExceeded, v, limits.MaxRootfsSize)
			}
			if v != rec.uncompressed {
				return fmt.Errorf("%w: XZ block uncompressed size mismatch against index", ErrCorrupt)
			}
			p += n
		}
		for i := 0; i < numFilters; i++ {
			fid, n := readUvarintSlice(bh[p : bhLen-4])
			if n <= 0 {
				return fmt.Errorf("%w: invalid XZ filter id", ErrCorrupt)
			}
			p += n
			propLen, n := readUvarintSlice(bh[p : bhLen-4])
			if n <= 0 || propLen > uint64(int(bhLen)-4-p-n) {
				return fmt.Errorf("%w: invalid XZ filter property size", ErrCorrupt)
			}
			p += n
			props := bh[p : p+int(propLen)]
			p += int(propLen)
			if fid == 0x21 {
				if len(props) != 1 || props[0] > 40 {
					return fmt.Errorf("%w: invalid LZMA2 dictionary property", ErrCorrupt)
				}
				var dictCap uint64
				if props[0] == 40 {
					dictCap = 0xffffffff
				} else {
					dictCap = (uint64(2) | uint64(props[0]&1)) << (props[0]/2 + 11)
				}
				if dictCap > uint64(limits.MaxXZDictSize) || dictCap > limits.MaxXZMemory {
					return fmt.Errorf("%w: XZ LZMA2 dictionary %d bytes exceeds %d limit", ErrLimitExceeded, dictCap, min(uint64(limits.MaxXZDictSize), limits.MaxXZMemory))
				}
			}
		}
		pos += paddedBlock
	}
	if pos != indexStart {
		return fmt.Errorf("%w: XZ block span (%d) does not match index start (%d)", ErrCorrupt, pos, indexStart)
	}
	return nil
}

func readUvarintSlice(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 9; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			if i > 0 && b[i] == 0 {
				return 0, -1
			}
			return v, i + 1
		}
	}
	return 0, -1
}
