package azimg_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/azimg"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

type xzTestRecord struct{ unpadded, uncompressed uint64 }

// rewriteXZIndex changes only the Index and footer, including their CRCs. Block
// headers and data remain intact, so decoder errors cannot substitute for preflight.
func rewriteXZIndex(t *testing.T, stream []byte, rewrite func([]xzTestRecord) []xzTestRecord) []byte {
	t.Helper()
	footer := stream[len(stream)-12:]
	indexSize := (int(binary.LittleEndian.Uint32(footer[4:8])) + 1) * 4
	indexStart := len(stream) - 12 - indexSize
	index := stream[indexStart : len(stream)-16]
	count, n := binary.Uvarint(index[1:])
	if n <= 0 {
		t.Fatal("invalid fixture Index count")
	}
	pos := 1 + n
	var records []xzTestRecord
	for i := uint64(0); i < count; i++ {
		unpadded, n := binary.Uvarint(index[pos:])
		if n <= 0 {
			t.Fatal("invalid fixture unpadded size")
		}
		pos += n
		uncompressed, n := binary.Uvarint(index[pos:])
		if n <= 0 {
			t.Fatal("invalid fixture uncompressed size")
		}
		pos += n
		records = append(records, xzTestRecord{unpadded, uncompressed})
	}
	changed := rewrite(records)
	var out bytes.Buffer
	out.Write(stream[:indexStart])
	index = append([]byte{0}, xzUvarint(uint64(len(changed)))...)
	for _, rec := range changed {
		index = append(index, xzUvarint(rec.unpadded)...)
		index = append(index, xzUvarint(rec.uncompressed)...)
	}
	for len(index)%4 != 0 {
		index = append(index, 0)
	}
	out.Write(index)
	_ = binary.Write(&out, binary.LittleEndian, crc32.ChecksumIEEE(index))
	var tail [6]byte
	binary.LittleEndian.PutUint32(tail[:4], uint32(len(index)/4))
	copy(tail[4:], footer[8:10])
	_ = binary.Write(&out, binary.LittleEndian, crc32.ChecksumIEEE(tail[:]))
	out.Write(tail[:])
	out.WriteString("YZ")
	return out.Bytes()
}

type rootfsCountWriter struct{ n int64 }

func (w *rootfsCountWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func TestAZ01XZIndexCannotHideRealBlocks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		flags byte
	}{
		{"unsized", 0},
		{"compressed-size", 0x40},
		{"both-sizes", 0xc0},
		{"uncompressed-size", 0x80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			honest := buildMultiBlockXZWithFlags([]byte{0, 0}, tc.flags)
			image := buildSyntheticAZ01(az01Opts{rootfs: honest})
			im, err := azimg.Open(ctx, bytes.NewReader(image), int64(len(image)), azimg.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if n, err := im.ExtractRootfs(ctx, &out); err != nil || n != 10 || out.String() != "hellohello" {
				t.Fatalf("honest control: n=%d output=%q err=%v", n, out.String(), err)
			}

			merge := func(records []xzTestRecord) []xzTestRecord {
				return []xzTestRecord{{
					unpadded:     (records[0].unpadded+3)/4*4 + records[1].unpadded,
					uncompressed: records[0].uncompressed + records[1].uncompressed,
				}}
			}
			for _, attack := range []struct {
				name   string
				stream []byte
				want   error
			}{
				{"hidden-large-dictionary", rewriteXZIndex(t, buildMultiBlockXZWithFlags([]byte{0, 36}, tc.flags), merge), azimg.ErrLimitExceeded},
				{"merged-index", rewriteXZIndex(t, honest, merge), azimg.ErrCorrupt},
				{"fewer-index-records", rewriteXZIndex(t, honest, func(records []xzTestRecord) []xzTestRecord { return records[:1] }), azimg.ErrCorrupt},
				{"extra-index-record", rewriteXZIndex(t, honest, func(records []xzTestRecord) []xzTestRecord { return append(records, records[1]) }), azimg.ErrCorrupt},
			} {
				t.Run(attack.name, func(t *testing.T) {
					image := buildSyntheticAZ01(az01Opts{rootfs: attack.stream})
					im, err := azimg.Open(ctx, bytes.NewReader(image), int64(len(image)), azimg.Limits{})
					if err != nil {
						t.Fatal(err)
					}
					var out rootfsCountWriter
					before := totalAllocBytes()
					n, err := im.ExtractRootfs(ctx, &out)
					allocated := totalAllocBytes() - before
					if !errors.Is(err, attack.want) || n != 0 || out.n != 0 {
						t.Fatalf("n=%d written=%d err=%v; want preflight %v", n, out.n, err, attack.want)
					}
					if allocated > 32<<20 {
						t.Fatalf("preflight allocated %d MiB before rejection", allocated>>20)
					}
				})
			}
		})
	}
}

// A ReaderAt can change after Open or preflight. Switch only when the decoder's
// buffered read starts; header-only preflight sees the earlier bytes throughout.
type changingXZReaderAt struct {
	preflight, decoded []byte
	rootfsOffset       int64
	armed, switched    bool
}

func (r *changingXZReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.armed && off == r.rootfsOffset && len(p) > 12 {
		r.switched = true
	}
	data := r.preflight
	if r.switched {
		data = r.decoded
	}
	return bytes.NewReader(data).ReadAt(p, off)
}

func TestAZ01XZUncompressedBoundsDespiteLyingIndex(t *testing.T) {
	ctx := context.Background()
	stream := compressXZ(t, make([]byte, 64<<20), xz.CRC32)
	lying := rewriteXZIndex(t, stream, func(records []xzTestRecord) []xzTestRecord {
		records[0].uncompressed = 1
		return records
	})
	t.Run("chunk-output-before-decoding", func(t *testing.T) {
		image := buildSyntheticAZ01(az01Opts{rootfs: lying})
		im, err := azimg.Open(ctx, bytes.NewReader(image), int64(len(image)), azimg.Limits{MaxRootfsSize: 2 << 20})
		if err != nil {
			t.Fatal(err)
		}
		var out rootfsCountWriter
		if n, err := im.ExtractRootfs(ctx, &out); !errors.Is(err, azimg.ErrLimitExceeded) || n != 0 || out.n != 0 {
			t.Fatalf("n=%d written=%d err=%v; want size rejection before decoding", n, out.n, err)
		}
	})

	t.Run("output-cap-after-source-changes", func(t *testing.T) {
		fake := append([]byte(nil), lying...)
		pos := 12 + (int(fake[12])+1)*4
		var chunks uint64
		for fake[pos] != 0 {
			control := fake[pos]
			if control < 0x80 {
				t.Fatalf("fixture needs compressed chunks, got 0x%02x", control)
			}
			headerLen := 5
			if control >= 0xc0 {
				headerLen = 6
			}
			packed := int(binary.BigEndian.Uint16(fake[pos+3:pos+5])) + 1
			fake[pos] &= 0xe0
			fake[pos+1], fake[pos+2] = 0, 0
			pos += headerLen + packed
			chunks++
		}
		fake = rewriteXZIndex(t, fake, func(records []xzTestRecord) []xzTestRecord {
			records[0].uncompressed = chunks
			return records
		})
		r := &changingXZReaderAt{
			preflight: buildSyntheticAZ01(az01Opts{rootfs: fake}),
			decoded:   buildSyntheticAZ01(az01Opts{rootfs: lying}),
		}
		if len(r.preflight) != len(r.decoded) {
			t.Fatal("fixture variants must have identical lengths")
		}
		const limit = 2 << 20
		im, err := azimg.Open(ctx, r, int64(len(r.preflight)), azimg.Limits{MaxRootfsSize: limit})
		if err != nil {
			t.Fatal(err)
		}
		meta, _ := im.AZ01()
		for _, part := range meta.Partitions {
			if part.Name == "rootfs" {
				r.rootfsOffset = part.Offset
			}
		}
		r.armed = true
		var out rootfsCountWriter
		n, err := im.ExtractRootfs(ctx, &out)
		if !r.switched || !errors.Is(err, azimg.ErrLimitExceeded) || n != out.n || out.n > limit {
			t.Fatalf("switched=%v n=%d written=%d err=%v; want bounded output after source changes", r.switched, n, out.n, err)
		}
	})
}

// xzOneBlock wraps LZMA2 data (end marker included) in a CRC32 XZ stream with an
// unsized block header (4 KiB dictionary) and an Index that matches the block.
func xzOneBlock(t *testing.T, lzma2, data []byte) []byte {
	t.Helper()
	b := []byte{0xfd, '7', 'z', 'X', 'Z', 0, 0x00, 0x01}
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b[6:8]))
	bh := []byte{0x02, 0x00, 0x21, 0x01, 0x00, 0x00, 0x00, 0x00}
	b = binary.LittleEndian.AppendUint32(append(b, bh...), crc32.ChecksumIEEE(bh))
	b = append(append(b, lzma2...), make([]byte, (4-len(lzma2)%4)%4)...)
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(data))
	// Empty Index and footer placeholders; rewriteXZIndex replaces both.
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0x00, 0x01, 'Y', 'Z')
	return rewriteXZIndex(t, b, func([]xzTestRecord) []xzTestRecord {
		return []xzTestRecord{{uint64(12 + len(lzma2) + 4), uint64(len(data))}}
	})
}

func extractXZ(t *testing.T, stream []byte, w io.Writer) (int64, error) {
	t.Helper()
	ctx := context.Background()
	image := buildSyntheticAZ01(az01Opts{rootfs: stream})
	im, err := azimg.Open(ctx, bytes.NewReader(image), int64(len(image)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return im.ExtractRootfs(ctx, w)
}

// The LZMA decoder stops when a chunk's range coder finishes; it does not require
// the chunk's declared packed size to be consumed. Bytes appended inside that size
// were parsed as an end marker, block check and a second block header declaring a
// 1 GiB dictionary, which header-only preflight never sees.
func TestAZ01XZChunkOverrunCannotHideBlockHeader(t *testing.T) {
	data := bytes.Repeat([]byte("engine rootfs "), 64)
	var raw bytes.Buffer
	zw, err := lzma.Writer2Config{DictCap: lzma.MinDictCap}.NewWriter2(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	chunk := raw.Bytes()[:raw.Len()-1] // one LZMA chunk; drop the end marker
	if chunk[0] < 0xe0 || int(binary.BigEndian.Uint16(chunk[3:5]))+7 != len(chunk) {
		t.Fatalf("fixture must be a single LZMA chunk, control 0x%02x", chunk[0])
	}
	var out bytes.Buffer
	if _, err := extractXZ(t, xzOneBlock(t, append(chunk, 0), data), &out); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("honest control: %d bytes, err=%v", out.Len(), err)
	}

	pad := (4 - (len(chunk)+1)%4) % 4 // block padding in the decoder's view
	hidden := append([]byte{0}, make([]byte, pad)...)
	hidden = binary.LittleEndian.AppendUint32(hidden, crc32.ChecksumIEEE(data))
	bh := []byte{0x02, 0x00, 0x21, 0x01, 36, 0x00, 0x00, 0x00}
	hidden = binary.LittleEndian.AppendUint32(append(hidden, bh...), crc32.ChecksumIEEE(bh))
	// 4-pad data bytes give the outer block the same padding, so only the
	// block-end check can tell the two views apart.
	hidden = append(hidden, 0x01, 0x00, byte(3-pad))
	hidden = append(hidden, bytes.Repeat([]byte("x"), 4-pad)...)
	overrun := append(append([]byte(nil), chunk...), hidden...)
	binary.BigEndian.PutUint16(overrun[3:5], uint16(len(overrun)-6-1))
	overrun = append(overrun, 0)

	// Fixture precondition: the decoder really leaves the appended bytes unread.
	src := bytes.NewReader(overrun)
	zr, err := lzma.NewReader2(src)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, data) || src.Len() != len(hidden) {
		t.Fatalf("decoder did not stop early: %d bytes, %d unread, err=%v", len(got), src.Len(), err)
	}

	before := totalAllocBytes()
	_, err = extractXZ(t, xzOneBlock(t, overrun, data), io.Discard)
	allocated := totalAllocBytes() - before
	if !errors.Is(err, azimg.ErrCorrupt) || allocated > 32<<20 {
		t.Fatalf("err=%v allocated=%d MiB; want ErrCorrupt without the hidden dictionary", err, allocated>>20)
	}
}

// With an honest Index, block header size fields must still match the block.
func TestAZ01XZHeaderSizeFieldsMatchBlock(t *testing.T) {
	for _, flags := range []byte{0x40, 0x80} {
		stream := buildMultiBlockXZWithFlags([]byte{0}, flags)
		bh := stream[12 : 12+(int(stream[12])+1)*4]
		bh[2]++ // the only size field present
		binary.LittleEndian.PutUint32(bh[len(bh)-4:], crc32.ChecksumIEEE(bh[:len(bh)-4]))
		if n, err := extractXZ(t, stream, io.Discard); !errors.Is(err, azimg.ErrCorrupt) || n != 0 {
			t.Errorf("flags 0x%02x: n=%d err=%v; want preflight ErrCorrupt", flags, n, err)
		}
	}
}

func TestAZ01XZBlockCheckVerified(t *testing.T) {
	for _, check := range []byte{xz.CRC32, xz.CRC64, xz.SHA256} {
		stream := compressXZ(t, []byte("rootfs payload"), check)
		var out bytes.Buffer
		if _, err := extractXZ(t, stream, &out); err != nil || out.String() != "rootfs payload" {
			t.Fatalf("check 0x%02x control: %q err=%v", check, out.String(), err)
		}
		// The last byte before the Index belongs to the single block's check.
		indexSize := (int(binary.LittleEndian.Uint32(stream[len(stream)-8:])) + 1) * 4
		stream[len(stream)-12-indexSize-1] ^= 1
		if _, err := extractXZ(t, stream, io.Discard); !errors.Is(err, azimg.ErrCorrupt) {
			t.Errorf("check 0x%02x: tampered block check err=%v; want ErrCorrupt", check, err)
		}
	}
}

func TestAZ01XZNonzeroPaddingRejected(t *testing.T) {
	stream := buildMultiBlockXZ([]byte{0})
	headerPad := append([]byte(nil), stream...)
	bh := headerPad[12:24]
	bh[7] = 1
	binary.LittleEndian.PutUint32(bh[8:], crc32.ChecksumIEEE(bh[:8]))
	blockPad := append([]byte(nil), stream...)
	blockPad[24+9] = 1 // after the 9-byte LZMA2 data
	for name, bad := range map[string][]byte{"header": headerPad, "block": blockPad} {
		if n, err := extractXZ(t, bad, io.Discard); !errors.Is(err, azimg.ErrCorrupt) || n != 0 {
			t.Errorf("nonzero %s padding: n=%d err=%v; want preflight ErrCorrupt", name, n, err)
		}
	}
}

// Decoding no longer re-reads the Index and footer, so preflight alone rejects them.
func TestAZ01XZIndexAndFooterIntegrity(t *testing.T) {
	stream := buildMultiBlockXZ([]byte{0})
	n := len(stream)
	for name, off := range map[string]int{"index-crc": n - 13, "footer-crc": n - 12, "footer-magic": n - 1} {
		bad := append([]byte(nil), stream...)
		bad[off] ^= 1
		if _, err := extractXZ(t, bad, io.Discard); !errors.Is(err, azimg.ErrCorrupt) {
			t.Errorf("%s: err=%v; want ErrCorrupt", name, err)
		}
	}
}

func TestAZ01XZBlockCountCaps(t *testing.T) {
	one := buildMultiBlockXZ([]byte{0})
	indexStart := len(one) - 12 - (int(binary.LittleEndian.Uint32(one[len(one)-8:]))+1)*4
	tooManyRecords := rewriteXZIndex(t, one, func(r []xzTestRecord) []xzTestRecord { return slices.Repeat(r, 65537) })
	// 65537 real blocks behind the original one-record Index.
	tooManyBlocks := slices.Concat(one[:12], bytes.Repeat(one[12:indexStart], 65537), one[indexStart:])
	for name, stream := range map[string][]byte{"index-records": tooManyRecords, "real-blocks": tooManyBlocks} {
		if _, err := extractXZ(t, stream, io.Discard); !errors.Is(err, azimg.ErrLimitExceeded) {
			t.Errorf("%s: err=%v; want ErrLimitExceeded", name, err)
		}
	}
}
