package azimg_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/azimg"
	"github.com/ulikunitz/xz"
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
