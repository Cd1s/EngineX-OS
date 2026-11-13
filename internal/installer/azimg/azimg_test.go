package azimg_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/azimg"
	"github.com/ulikunitz/xz"
)

func az01String(s string) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(s)))
	b.WriteString(s)
	b.WriteByte(0)
	pad := (4 - ((len(s) + 1) % 4)) % 4
	b.Write(make([]byte, pad))
	return b.Bytes()
}

type az01Opts struct {
	version    uint32
	build      string
	compat     []string
	usbs       []uint32
	rootfsName string
	rootfsComp string
	hashAlgo   string
	rootfs     []byte
	alignEOF   bool
	extraTrail []byte
}

func buildSyntheticAZ01(o az01Opts) []byte {
	if o.version == 0 {
		o.version = 1
	}
	if o.build == "" {
		o.build = "test-build"
	}
	if len(o.compat) == 0 {
		o.compat = []string{"inmusic,jp07"}
	}
	if len(o.usbs) == 0 {
		o.usbs = []uint32{0x15e4d006}
	}
	if o.rootfsName == "" {
		o.rootfsName = "rootfs"
	}
	if o.rootfsComp == "" {
		o.rootfsComp = "xz"
	}
	if o.hashAlgo == "" {
		o.hashAlgo = "sha1"
	}
	if o.rootfs == nil {
		o.rootfs = []byte("rootfs")
	}

	var hdr bytes.Buffer
	hdr.Write(az01String(o.build))
	_ = binary.Write(&hdr, binary.LittleEndian, uint32(len(o.compat)))
	for _, c := range o.compat {
		hdr.Write(az01String(c))
	}
	_ = binary.Write(&hdr, binary.LittleEndian, uint32(len(o.usbs)))
	for _, u := range o.usbs {
		_ = binary.Write(&hdr, binary.LittleEndian, u)
	}
	hdr.Write(az01String("test"))

	var out bytes.Buffer
	out.WriteString("AZ01")
	_ = binary.Write(&out, binary.LittleEndian, o.version)
	_ = binary.Write(&out, binary.LittleEndian, uint32(12+hdr.Len()))
	out.Write(hdr.Bytes())

	parts := []struct {
		tag  [8]byte
		name string
		comp string
		blob []byte
	}{
		{[8]byte{'P', 'A', 'R', 'T', 'P', 0, 0, 0}, "splash", "none", []byte("S")},
		{[8]byte{'P', 'A', 'R', 'T', 'X', 0, 0, 0}, "recoverysplash", "none", []byte("R")},
		{[8]byte{'P', 'A', 'R', 'T', 'L', 0, 0, 0}, o.rootfsName, o.rootfsComp, o.rootfs},
	}
	for _, p := range parts {
		out.Write(p.tag[:])
		_ = binary.Write(&out, binary.LittleEndian, uint64(len(p.blob)))
		out.Write(az01String(p.name))
		out.Write(az01String(p.comp))
		_ = binary.Write(&out, binary.LittleEndian, uint32(1))
		out.Write(az01String(o.hashAlgo))
		_ = binary.Write(&out, binary.LittleEndian, uint32(20))
		sum := sha1.Sum(p.blob)
		out.Write(sum[:])
		out.Write(p.blob)
	}
	if o.alignEOF {
		if pad := (8 - (out.Len() % 8)) % 8; pad > 0 {
			out.Write(make([]byte, pad))
		}
	}
	out.Write([]byte{'E', 'O', 'F', 0})
	_ = binary.Write(&out, binary.LittleEndian, uint32(16))
	_ = binary.Write(&out, binary.LittleEndian, uint64(0))
	out.Write(o.extraTrail)
	return out.Bytes()
}

func compressXZ(t *testing.T, data []byte, checkSum uint8) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := xz.WriterConfig{CheckSum: checkSum}
	if checkSum == 0 {
		cfg.NoCheckSum = true
	}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func xzUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// buildMultiBlockXZ builds a valid multi-block XZ stream (CRC32 check) with unsized block
// headers (as xz -T1 writes them) and the given LZMA2 dictionary property byte per block.
func buildMultiBlockXZ(dictProps []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xfd, '7', 'z', 'X', 'Z', 0})
	flags := []byte{0x00, 0x01}
	b.Write(flags)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(flags))
	type rec struct{ unpadded, uncompressed uint64 }
	var recs []rec
	data := []byte("hello")
	for _, d := range dictProps {
		bh := []byte{0x02, 0x00, 0x21, 0x01, d, 0x00, 0x00, 0x00}
		b.Write(bh)
		_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(bh))
		chunk := append([]byte{0x01, 0x00, byte(len(data) - 1)}, data...)
		chunk = append(chunk, 0x00)
		b.Write(chunk)
		b.Write(make([]byte, (4-len(chunk)%4)%4))
		_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(data))
		recs = append(recs, rec{uint64(12 + len(chunk) + 4), uint64(len(data))})
	}
	idx := []byte{0x00}
	idx = append(idx, xzUvarint(uint64(len(recs)))...)
	for _, r := range recs {
		idx = append(idx, xzUvarint(r.unpadded)...)
		idx = append(idx, xzUvarint(r.uncompressed)...)
	}
	for len(idx)%4 != 0 {
		idx = append(idx, 0)
	}
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(idx))
	idx = append(idx, crc[:]...)
	b.Write(idx)
	var foot [6]byte
	binary.LittleEndian.PutUint32(foot[:4], uint32(len(idx)/4-1))
	foot[4], foot[5] = 0x00, 0x01
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(foot[:]))
	b.Write(foot[:])
	b.WriteString("YZ")
	return b.Bytes()
}

// buildMultiBlockXZWithBadCompSize builds an XZ stream whose block header includes
// compressedSize (flags=0x40) that disagrees with the stream index record.
func buildMultiBlockXZWithBadCompSize() []byte {
	var b bytes.Buffer
	b.Write([]byte{0xfd, '7', 'z', 'X', 'Z', 0})
	flags := []byte{0x00, 0x01}
	b.Write(flags)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(flags))
	data := []byte("hello")
	chunk := append([]byte{0x01, 0x00, byte(len(data) - 1)}, data...)
	chunk = append(chunk, 0x00)
	// Block header: size=2 (12 bytes), flags=0x40 (compressedSize present), compSize=99 (wrong, real is 9), filter=LZMA2 prop=0
	bh := []byte{0x02, 0x40, 99, 0x21, 0x01, 0x00, 0x00, 0x00}
	b.Write(bh)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(bh))
	b.Write(chunk)
	b.Write(make([]byte, (4-len(chunk)%4)%4))
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(data))
	unpadded := uint64(12 + len(chunk) + 4)
	idx := append([]byte{0x00}, xzUvarint(1)...)
	idx = append(idx, xzUvarint(unpadded)...)
	idx = append(idx, xzUvarint(uint64(len(data)))...)
	for len(idx)%4 != 0 {
		idx = append(idx, 0)
	}
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(idx))
	idx = append(idx, crc[:]...)
	b.Write(idx)
	var foot [6]byte
	binary.LittleEndian.PutUint32(foot[:4], uint32(len(idx)/4-1))
	foot[4], foot[5] = 0x00, 0x01
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(foot[:]))
	b.Write(foot[:])
	b.WriteString("YZ")
	return b.Bytes()
}

func totalAllocBytes() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc
}

func TestAZ01ValidAlignedAndModels(t *testing.T) {
	ctx := context.Background()
	raw := []byte("hello engine rootfs payload")
	xzBytes := compressXZ(t, raw, xz.CRC32)

	imgBytes := buildSyntheticAZ01(az01Opts{
		compat:   []string{"inmusic,jp07", "inmusic,jp08", "inmusic,jp11"},
		usbs:     []uint32{0x15e4d006, 0x15e4d00a, 0x15e4d00c},
		rootfs:   xzBytes,
		alignEOF: true,
	})

	im, err := azimg.Open(ctx, bytes.NewReader(imgBytes), int64(len(imgBytes)), azimg.Limits{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if im.Format() != azimg.FormatAZ01 {
		t.Fatalf("Format = %q, want %q", im.Format(), azimg.FormatAZ01)
	}
	if len(im.Models()) != 3 || !im.SupportsBoard("jp07") || !im.SupportsBoard("SC5000") || !im.SupportsBoard("PRIMEGO") || im.SupportsBoard("jp13") {
		t.Fatalf("unexpected models: %+v", im.Models())
	}
	meta, ok := im.AZ01()
	if !ok || !meta.Aligned || meta.Build != "test-build" || len(meta.Partitions) != 3 {
		t.Fatalf("unexpected AZ01 metadata: %+v (ok=%v)", meta, ok)
	}
	var got bytes.Buffer
	n, err := im.ExtractRootfs(ctx, &got)
	if err != nil {
		t.Fatalf("ExtractRootfs: %v", err)
	}
	if n != int64(len(raw)) || !bytes.Equal(got.Bytes(), raw) {
		t.Fatalf("ExtractRootfs mismatch: n=%d got=%q", n, got.Bytes())
	}
	var splash bytes.Buffer
	if _, err := im.ExtractPartition(ctx, "splash", &splash); err != nil || splash.String() != "S" {
		t.Fatalf("ExtractPartition(splash) = %q, err=%v", splash.String(), err)
	}
}

func TestAZ01HostileInputsAndTruncation(t *testing.T) {
	ctx := context.Background()
	good := buildSyntheticAZ01(az01Opts{rootfs: compressXZ(t, []byte("rootfs"), xz.CRC64), alignEOF: true})

	for n := 0; n < len(good); n++ {
		if _, err := azimg.Open(ctx, bytes.NewReader(good[:n]), int64(n), azimg.Limits{}); err == nil {
			t.Fatalf("truncated image of length %d was accepted", n)
		}
	}

	// Signed AZ0x and FDT containers must fail closed with clear typed errors.
	az0x := append([]byte("AZ0x"), good[4:]...)
	if _, err := azimg.Open(ctx, bytes.NewReader(az0x), int64(len(az0x)), azimg.Limits{}); !errors.Is(err, azimg.ErrSignedContainer) {
		t.Fatalf("AZ0x err = %v, want ErrSignedContainer", err)
	}
	fdt := append([]byte{0xd0, 0x0d, 0xfe, 0xed}, good[4:]...)
	if _, err := azimg.Open(ctx, bytes.NewReader(fdt), int64(len(fdt)), azimg.Limits{}); !errors.Is(err, azimg.ErrInvalidFormat) {
		t.Fatalf("FDT err = %v, want ErrInvalidFormat", err)
	}

	// Compatible-string match must be exact like az01_packer.py (reject case/whitespace variants).
	for _, badCompat := range []string{"inmusic,xx99", " inmusic,jp07", "INMUSIC,JP07", "inmusic,jp07 "} {
		img := buildSyntheticAZ01(az01Opts{compat: []string{badCompat}})
		if _, err := azimg.Open(ctx, bytes.NewReader(img), int64(len(img)), azimg.Limits{}); !errors.Is(err, azimg.ErrModelMismatch) {
			t.Errorf("compat %q err = %v, want ErrModelMismatch", badCompat, err)
		}
	}

	// Corrupt version, strings, partition name, algo, trailing bytes, nonzero align padding, missing NUL.
	badCases := map[string]struct {
		data []byte
		want error
	}{
		"version-2":       {buildSyntheticAZ01(az01Opts{version: 2}), azimg.ErrUnsupported},
		"bad-part-name":   {buildSyntheticAZ01(az01Opts{rootfsName: "uboot"}), azimg.ErrCorrupt},
		"bad-part-path":   {buildSyntheticAZ01(az01Opts{rootfsName: "../rootfs"}), azimg.ErrCorrupt},
		"bad-comp":        {buildSyntheticAZ01(az01Opts{rootfsComp: "none"}), azimg.ErrCorrupt},
		"bad-hash-algo":   {buildSyntheticAZ01(az01Opts{hashAlgo: "sha256"}), azimg.ErrCorrupt},
		"extra-signature": {buildSyntheticAZ01(az01Opts{extraTrail: []byte("sig")}), azimg.ErrCorrupt},
		"build-nul":       {buildSyntheticAZ01(az01Opts{build: "bad\x00str"}), azimg.ErrCorrupt},
		"build-nonascii":  {buildSyntheticAZ01(az01Opts{build: "bad\x80str"}), azimg.ErrCorrupt},
	}
	for name, tc := range badCases {
		if _, err := azimg.Open(ctx, bytes.NewReader(tc.data), int64(len(tc.data)), azimg.Limits{}); !errors.Is(err, tc.want) {
			t.Errorf("%s err = %v, want %v", name, err, tc.want)
		}
	}

	// Corrupt NUL terminator of "test-build" string specifically (without changing padding).
	badTerm := append([]byte(nil), good...)
	// Offset 12=len("test-build")=10; bytes 16..25 are "test-build"; byte 26 is NUL terminator.
	badTerm[26] = 'X'
	if _, err := azimg.Open(ctx, bytes.NewReader(badTerm), int64(len(badTerm)), azimg.Limits{}); !errors.Is(err, azimg.ErrCorrupt) {
		t.Errorf("missing string NUL terminator err = %v, want ErrCorrupt", err)
	}

	// Non-zero alignment padding before EOF must be rejected (aligned image with pad > 0).
	unaligned := buildSyntheticAZ01(az01Opts{rootfs: []byte("rootfs0"), alignEOF: true})
	imUn, err := azimg.Open(ctx, bytes.NewReader(unaligned), int64(len(unaligned)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	metaUn, _ := imUn.AZ01()
	if metaUn.EOFOffset%8 == 0 {
		t.Fatal("test setup expected non-8-byte-aligned EOFOffset before padding")
	}
	badAlignPad := append([]byte(nil), unaligned...)
	badAlignPad[metaUn.EOFOffset] = 0x01
	if _, err := azimg.Open(ctx, bytes.NewReader(badAlignPad), int64(len(badAlignPad)), azimg.Limits{}); !errors.Is(err, azimg.ErrCorrupt) {
		t.Errorf("nonzero alignment padding err = %v, want ErrCorrupt", err)
	}

	im, err := azimg.Open(ctx, bytes.NewReader(good), int64(len(good)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := im.AZ01()
	for _, idx := range []int64{
		meta.Partitions[0].Offset,
		meta.Partitions[0].DigestOffset,
		meta.Partitions[2].Offset,
		int64(len(good) - 1),
		8, // tableOffset
	} {
		bad := append([]byte(nil), good...)
		bad[idx] ^= 1
		if _, err := azimg.Open(ctx, bytes.NewReader(bad), int64(len(bad)), azimg.Limits{}); err == nil {
			t.Errorf("corrupted byte at %d was accepted", idx)
		}
	}
}

func TestAZ01HostileXZStreams(t *testing.T) {
	ctx := context.Background()
	payload := compressXZ(t, bytes.Repeat([]byte("A"), 256*1024), xz.CRC32)

	for name, badXZ := range map[string][]byte{
		"trailing-junk":     append(append([]byte(nil), payload...), []byte("junk")...),
		"trailing-zero-pad": append(buildMultiBlockXZ([]byte{0x00}), 0, 0, 0, 0),
		"concatenated":      append(append([]byte(nil), payload...), payload...),
		"truncated":         payload[:len(payload)-1],
		"not-xz":            []byte("notxz"),
		"no-check":          compressXZ(t, []byte("hello"), 0),
	} {
		t.Run(name, func(t *testing.T) {
			imgBytes := buildSyntheticAZ01(az01Opts{rootfs: badXZ})
			im, err := azimg.Open(ctx, bytes.NewReader(imgBytes), int64(len(imgBytes)), azimg.Limits{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := im.ExtractRootfs(ctx, io.Discard); err == nil {
				t.Fatalf("%s XZ stream was accepted", name)
			}
		})
	}

	// Output size limit enforced.
	imgBytes := buildSyntheticAZ01(az01Opts{rootfs: payload})
	im, err := azimg.Open(ctx, bytes.NewReader(imgBytes), int64(len(imgBytes)), azimg.Limits{MaxRootfsSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := im.ExtractRootfs(ctx, io.Discard); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("oversized rootfs err = %v, want ErrLimitExceeded", err)
	}

	// Multi-block XZ without compressedSize in block headers: control succeeds, but block 2
	// declaring a 1 GiB dictionary (prop 36) must be rejected BEFORE allocation.
	okMulti := buildSyntheticAZ01(az01Opts{rootfs: buildMultiBlockXZ([]byte{0x00, 0x00})})
	imOKMulti, err := azimg.Open(ctx, bytes.NewReader(okMulti), int64(len(okMulti)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var outMulti bytes.Buffer
	if _, err := imOKMulti.ExtractRootfs(ctx, &outMulti); err != nil || outMulti.String() != "hellohello" {
		t.Fatalf("2-block valid XZ failed: %q err=%v", outMulti.String(), err)
	}

	hostileMulti := buildSyntheticAZ01(az01Opts{rootfs: buildMultiBlockXZ([]byte{0x00, 36})})
	imHostileMulti, err := azimg.Open(ctx, bytes.NewReader(hostileMulti), int64(len(hostileMulti)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	before := totalAllocBytes()
	_, err = imHostileMulti.ExtractRootfs(ctx, io.Discard)
	allocDelta := totalAllocBytes() - before
	if !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("2nd-block 1 GiB dict err = %v, want ErrLimitExceeded", err)
	}
	if allocDelta > 32<<20 {
		t.Fatalf("2nd-block 1 GiB dict allocated %d MiB before rejection", allocDelta>>20)
	}

	// Also test sized XZ block header whose compressedSize mismatches the index record (M32).
	sizedBadXZ := buildMultiBlockXZWithBadCompSize()
	sizedImg := buildSyntheticAZ01(az01Opts{rootfs: sizedBadXZ})
	imSized, err := azimg.Open(ctx, bytes.NewReader(sizedImg), int64(len(sizedImg)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imSized.ExtractRootfs(ctx, io.Discard); !errors.Is(err, azimg.ErrCorrupt) {
		t.Fatalf("mismatched block compSize err = %v, want ErrCorrupt", err)
	}

	// Also test SHA256 XZ integrity check is accepted.
	shaXZ := compressXZ(t, []byte("sha256-checked"), xz.SHA256)
	shaImg := buildSyntheticAZ01(az01Opts{rootfs: shaXZ})
	imSHA, err := azimg.Open(ctx, bytes.NewReader(shaImg), int64(len(shaImg)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := imSHA.ExtractRootfs(ctx, &out); err != nil || out.String() != "sha256-checked" {
		t.Fatalf("SHA256 XZ failed: %q err=%v", out.String(), err)
	}
}

type tarEntrySpec struct {
	name       string
	typeflag   byte
	mode       int64
	data       []byte
	linkname   string
	paxRecords map[string]string
}

func buildRawTar(t *testing.T, specs []tarEntrySpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, s := range specs {
		h := &tar.Header{
			Name:       s.name,
			Mode:       s.mode,
			Size:       int64(len(s.data)),
			Typeflag:   s.typeflag,
			Linkname:   s.linkname,
			PAXRecords: s.paxRecords,
		}
		if len(s.paxRecords) > 0 {
			h.Format = tar.FormatPAX
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(s.data) > 0 {
			if _, err := tw.Write(s.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type bundleBuildOpts struct {
	prefix          string
	top             string
	extra           map[string][]byte
	modes           map[string]int64
	omitFromSums    map[string]bool
	extraSumsLines  []string
	rawExtraEntries []tarEntrySpec
	omitSumsFile    bool
	emptySumsFile   bool
	gzipStored      bool
	corruptSumFor   string
}

func buildTestBundleTarGz(t *testing.T, o bundleBuildOpts) []byte {
	t.Helper()
	if o.prefix == "" {
		o.prefix = "SCLIVE4"
	}
	version := "5.3.0.4"
	top := o.top
	if top == "" {
		top = o.prefix + "-" + version + "-EngineX-OS-Data-Bundle"
	}
	uiTar := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/en/Main.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("Item {}\n")},
		{name: "icons/deezer.svg", typeflag: tar.TypeReg, mode: 0o644, data: []byte("<svg/>\n")},
	})
	alsa := "system/var/overlay/lib/alsa/conf.d\n"
	if o.prefix == "SYSTEMONE" || o.prefix == "PRIME4G2" {
		alsa = "system/var-lib/alsa/conf.d\n"
	}
	files := map[string][]byte{
		"bin/sc5000-suite":         []byte("suite-bin-PAYLOAD-0123456789"),
		"lib/libengine-ui-hook.so": []byte("hook-so"),
		"boot-activate.sh":         []byte("#!/bin/sh\n"),
		"99-sc5000-custom.conf":    []byte("pcm.!default {}\n"),
		"alsa-dir":                 []byte(alsa),
		"version.txt":              []byte(version + "\n"),
		"release-tag":              []byte("v" + version + "-r4\n"),
		"model":                    []byte(o.prefix + "\n"),
		"ui-packs.tar":             uiTar,
	}
	for k, v := range o.extra {
		files[k] = v
	}
	var sums strings.Builder
	for rel, data := range files {
		if o.omitFromSums[rel] {
			continue
		}
		h := sha256.Sum256(data)
		hexStr := hex.EncodeToString(h[:])
		if o.corruptSumFor == rel {
			hexStr = strings.Repeat("0", 64)
		}
		sums.WriteString(hexStr + "  " + rel + "\n")
	}
	for _, extraLine := range o.extraSumsLines {
		sums.WriteString(extraLine + "\n")
	}

	var gzBuf bytes.Buffer
	level := gzip.DefaultCompression
	if o.gzipStored {
		level = gzip.NoCompression
	}
	gw, err := gzip.NewWriterLevel(&gzBuf, level)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755})
	for rel, data := range files {
		mode := int64(0o644)
		if m, ok := o.modes[rel]; ok {
			mode = m
		}
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + rel, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	for _, s := range o.rawExtraEntries {
		h := &tar.Header{Name: s.name, Typeflag: s.typeflag, Mode: s.mode, Size: int64(len(s.data)), Linkname: s.linkname}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(s.data) > 0 {
			_, _ = tw.Write(s.data)
		}
	}
	if !o.omitSumsFile {
		sumsBytes := []byte(sums.String())
		if o.emptySumsFile {
			sumsBytes = nil
		}
		_ = tw.WriteHeader(&tar.Header{Name: top + "/SHA256SUMS", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(sumsBytes))})
		_, _ = tw.Write(sumsBytes)
	}
	_ = tw.Close()
	_ = gw.Close()
	return gzBuf.Bytes()
}

func wrapGen2Image(tarGz []byte, prefix, version string) []byte {
	sum := sha256.Sum256(tarGz)
	hdr := "ENGINEX_DATA_BUNDLE_IMG_V1\n" + hex.EncodeToString(sum[:]) + "\n" + prefix + "\n" + version + "\n"
	padded := make([]byte, 128)
	copy(padded, hdr)
	return append(padded, tarGz...)
}

type mutatingReaderAt struct {
	b []byte
	n atomic.Int64
}

func (r *mutatingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	r.n.Add(int64(n))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestGen2ImageAndBundleValidWalkAndOpen(t *testing.T) {
	ctx := context.Background()
	tarGz := buildTestBundleTarGz(t, bundleBuildOpts{
		modes: map[string]int64{"bin/sc5000-suite": 0o6755},
	})
	imgBytes := wrapGen2Image(tarGz, "SCLIVE4", "5.3.0.4")

	for _, tc := range []struct {
		name string
		data []byte
		want azimg.Format
	}{
		{"gen2-image", imgBytes, azimg.FormatGen2Image},
		{"gen2-bundle", tarGz, azimg.FormatGen2Bundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			im, err := azimg.Open(ctx, bytes.NewReader(tc.data), int64(len(tc.data)), azimg.Limits{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if im.Format() != tc.want {
				t.Fatalf("Format = %q, want %q", im.Format(), tc.want)
			}
			if !im.SupportsBoard("jp21") || !im.SupportsBoard("SCLIVE4") || im.SupportsBoard("rmz2") {
				t.Fatalf("unexpected model support: %+v", im.Models())
			}

			// Concurrent Bundle() calls must be race-free and return the same verified bundle.
			var bwg sync.WaitGroup
			for i := 0; i < 4; i++ {
				bwg.Add(1)
				go func() {
					defer bwg.Done()
					if _, err := im.Bundle(ctx); err != nil {
						t.Errorf("concurrent Bundle: %v", err)
					}
				}()
			}
			bwg.Wait()

			b, err := im.Bundle(ctx)
			if err != nil {
				t.Fatalf("Bundle: %v", err)
			}
			if b.Version() != "5.3.0.4" || b.ReleaseTag() != "v5.3.0.4-r4" || b.Model().Prefix != "SCLIVE4" || !b.HasUIPacks() {
				t.Fatalf("unexpected Bundle meta: version=%q tag=%q model=%+v ui=%v", b.Version(), b.ReleaseTag(), b.Model(), b.HasUIPacks())
			}

			var entries []azimg.Entry
			if err := b.Walk(ctx, func(e azimg.Entry) error {
				entries = append(entries, e)
				return nil
			}); err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if len(entries) < 8 {
				t.Fatalf("too few bundle entries: %d", len(entries))
			}
			for _, e := range entries {
				if e.Path == "bin/sc5000-suite" && e.Mode != 0o755 {
					t.Fatalf("Entry.Mode = %#o, want 0755 (setuid/setgid masked)", e.Mode)
				}
				rc, err := e.Open()
				if err != nil {
					t.Fatalf("Open(%s): %v", e.Path, err)
				}
				got, err := io.ReadAll(rc)
				_ = rc.Close()
				if err != nil || int64(len(got)) != e.Size || sha256.Sum256(got) != e.SHA256 {
					t.Fatalf("entry %s mismatch: len=%d/%d err=%v", e.Path, len(got), e.Size, err)
				}
			}

			var uiEntries []azimg.Entry
			if err := b.WalkUI(ctx, func(e azimg.Entry) error {
				uiEntries = append(uiEntries, e)
				return nil
			}); err != nil {
				t.Fatalf("WalkUI: %v", err)
			}
			if len(uiEntries) != 2 {
				t.Fatalf("uiEntries = %d, want 2", len(uiEntries))
			}
			var wg sync.WaitGroup
			for i := len(uiEntries) - 1; i >= 0; i-- {
				e := uiEntries[i]
				wg.Add(1)
				go func() {
					defer wg.Done()
					rc, err := e.Open()
					if err != nil {
						t.Errorf("UI Open(%s): %v", e.Path, err)
						return
					}
					got, err := io.ReadAll(rc)
					_ = rc.Close()
					if err != nil || sha256.Sum256(got) != e.SHA256 {
						t.Errorf("UI %s read err=%v", e.Path, err)
					}
				}()
			}
			wg.Wait()

			tgzRC, err := b.OpenTarGz(ctx)
			if err != nil {
				t.Fatalf("OpenTarGz: %v", err)
			}
			gotTGZ, err := io.ReadAll(tgzRC)
			_ = tgzRC.Close()
			if err != nil || !bytes.Equal(gotTGZ, tarGz) {
				t.Fatalf("OpenTarGz mismatch err=%v", err)
			}
		})
	}
}

// TestGen2EntryOpenVerifiesOnExactSizeRead proves Entry.Open() detects TOCTOU tampering
// immediately once Entry.Size bytes have been read (io.ReadFull / io.CopyN), not only at EOF.
func TestGen2EntryOpenVerifiesOnExactSizeRead(t *testing.T) {
	ctx := context.Background()
	raw := buildTestBundleTarGz(t, bundleBuildOpts{gzipStored: true})
	ra := &mutatingReaderAt{b: append([]byte(nil), raw...)}
	im, err := azimg.Open(ctx, ra, int64(len(raw)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := im.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pos := bytes.Index(ra.b, []byte("suite-bin-PAYLOAD"))
	if pos < 0 {
		t.Fatal("stored payload not found")
	}
	ra.b[pos] ^= 0x20

	var target azimg.Entry
	_ = b.Walk(ctx, func(e azimg.Entry) error {
		if e.Path == "bin/sc5000-suite" {
			target = e
		}
		return nil
	})

	rc1, err := target.Open()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadFull(rc1, make([]byte, target.Size))
	_ = rc1.Close()
	if !errors.Is(err, azimg.ErrDigestMismatch) {
		t.Fatalf("io.ReadFull(exact size) err = %v, want ErrDigestMismatch", err)
	}

	rc2, err := target.Open()
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.CopyN(io.Discard, rc2, target.Size)
	_ = rc2.Close()
	if !errors.Is(err, azimg.ErrDigestMismatch) {
		t.Fatalf("io.CopyN(exact size) err = %v, want ErrDigestMismatch", err)
	}
}

func TestGen2HostileWrappersManifestsAndArchives(t *testing.T) {
	ctx := context.Background()
	tarGz := buildTestBundleTarGz(t, bundleBuildOpts{})
	goodImg := wrapGen2Image(tarGz, "SCLIVE4", "5.3.0.4")

	// 1. Corrupt 128-byte wrapper cases.
	badDirtyPad := append([]byte(nil), goodImg...)
	badDirtyPad[127] = 'X'
	badDigestTerm := append([]byte(nil), goodImg...)
	badDigestTerm[27+64] = ' ' // break newline after 64-hex digest
	// Build a lower-case prefix wrapper manually so wrapGen2Image doesn't normalize it.
	sum := sha256.Sum256(tarGz)
	lowerPrefixHdr := "ENGINEX_DATA_BUNDLE_IMG_V1\n" + hex.EncodeToString(sum[:]) + "\nsclive4\n5.3.0.4\n"
	badLowerPrefix := make([]byte, 128)
	copy(badLowerPrefix, lowerPrefixHdr)
	badLowerPrefix = append(badLowerPrefix, tarGz...)

	badSHA := append([]byte(nil), goodImg...)
	badSHA[len(badSHA)-1] ^= 1
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"truncated-hdr":    {goodImg[:100], azimg.ErrCorrupt},
		"hdr-only":         {goodImg[:128], azimg.ErrCorrupt},
		"legacy-no-v1":     {append([]byte("ENGINEX_DATA_BUNDLE_OLD\n"), make([]byte, 200)...), azimg.ErrCorrupt},
		"bad-digest-term":  {badDigestTerm, azimg.ErrCorrupt},
		"dirty-padding":    {badDirtyPad, azimg.ErrCorrupt},
		"bad-model":        {wrapGen2Image(tarGz, "UNKNOWN", "5.3.0.4"), azimg.ErrModelMismatch},
		"lowercase-prefix": {badLowerPrefix, azimg.ErrModelMismatch},
		"bad-version":      {wrapGen2Image(tarGz, "SCLIVE4", "5.3"), azimg.ErrCorrupt},
		"payload-sha":      {badSHA, azimg.ErrDigestMismatch},
		"non-gzip":         {wrapGen2Image([]byte("not gzip data"), "SCLIVE4", "5.3.0.4"), azimg.ErrCorrupt},
	} {
		if _, err := azimg.Open(ctx, bytes.NewReader(tc.data), int64(len(tc.data)), azimg.Limits{}); !errors.Is(err, tc.want) {
			t.Errorf("%s err = %v, want %v", name, err, tc.want)
		}
	}

	// 2. Trailing/concatenated gzip stream rejected by Bundle().
	concatImg := wrapGen2Image(append(append([]byte(nil), tarGz...), tarGz...), "SCLIVE4", "5.3.0.4")
	imConcat, err := azimg.Open(ctx, bytes.NewReader(concatImg), int64(len(concatImg)), azimg.Limits{})
	if err != nil {
		t.Fatalf("Open concatImg: %v", err)
	}
	if _, err := imConcat.Bundle(ctx); !errors.Is(err, azimg.ErrCorrupt) {
		t.Fatalf("concatenated gzip err = %v, want ErrCorrupt", err)
	}

	// 3. Manifest consistency checks (all with otherwise valid archives).
	for _, name := range []string{"missing-sums", "empty-sums"} {
		opts := bundleBuildOpts{omitSumsFile: name == "missing-sums", emptySumsFile: name == "empty-sums"}
		raw := buildTestBundleTarGz(t, opts)
		_, err := azimg.Open(ctx, bytes.NewReader(raw), int64(len(raw)), azimg.Limits{})
		if !errors.Is(err, azimg.ErrCorrupt) || !strings.Contains(err.Error(), "SHA256SUMS missing or empty") {
			t.Errorf("%s err = %v, want ErrCorrupt with SHA256SUMS missing or empty", name, err)
		}
	}
	badManifests := map[string]struct {
		data []byte
		want error
	}{
		"unlisted-file": {buildTestBundleTarGz(t, bundleBuildOpts{extra: map[string][]byte{"evil.sh": []byte("x")}, omitFromSums: map[string]bool{"evil.sh": true}}), azimg.ErrCorrupt},
		"missing-file":  {buildTestBundleTarGz(t, bundleBuildOpts{extraSumsLines: []string{strings.Repeat("a", 64) + "  missing.txt"}}), azimg.ErrCorrupt},
		"dup-sums-line": {buildTestBundleTarGz(t, bundleBuildOpts{extraSumsLines: []string{strings.Repeat("b", 64) + "  version.txt"}}), azimg.ErrCorrupt},
		"bad-sums-path": {buildTestBundleTarGz(t, bundleBuildOpts{extraSumsLines: []string{strings.Repeat("c", 64) + "  ../version.txt"}}), azimg.ErrUnsafePath},
		"sums-mismatch": {buildTestBundleTarGz(t, bundleBuildOpts{corruptSumFor: "version.txt"}), azimg.ErrDigestMismatch},
	}
	for name, tc := range badManifests {
		if _, err := azimg.Open(ctx, bytes.NewReader(tc.data), int64(len(tc.data)), azimg.Limits{}); !errors.Is(err, tc.want) {
			t.Errorf("%s err = %v, want %v", name, err, tc.want)
		}
	}

	// 4. Strict metadata validation (version.txt NBSP, release-tag, alsa-dir, top-vs-model).
	badMeta := map[string]struct {
		opts bundleBuildOpts
		want error
	}{
		"version-nbsp":     {bundleBuildOpts{extra: map[string][]byte{"version.txt": []byte("5.3.0.4\u00a0\n")}}, azimg.ErrCorrupt},
		"reltag-shell":     {bundleBuildOpts{extra: map[string][]byte{"release-tag": []byte("v5.3.0.4\n$(id)\n")}}, azimg.ErrCorrupt},
		"reltag-wrong-ver": {bundleBuildOpts{extra: map[string][]byte{"release-tag": []byte("v9.9.9\n")}}, azimg.ErrCorrupt},
		"alsa-arbitrary":   {bundleBuildOpts{extra: map[string][]byte{"alsa-dir": []byte("etc/cron.d\n")}}, azimg.ErrCorrupt},
		"alsa-wrong-arch":  {bundleBuildOpts{prefix: "SCLIVE4", extra: map[string][]byte{"alsa-dir": []byte("system/var-lib/alsa/conf.d\n")}}, azimg.ErrCorrupt},
		"alsa-az04-wrong":  {bundleBuildOpts{prefix: "SYSTEMONE", extra: map[string][]byte{"alsa-dir": []byte("system/var/overlay/lib/alsa/conf.d\n")}}, azimg.ErrCorrupt},
		"top-wrong-model":  {bundleBuildOpts{prefix: "SCLIVE4", top: "PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle"}, azimg.ErrModelMismatch},
	}
	for name, tc := range badMeta {
		raw := buildTestBundleTarGz(t, tc.opts)
		if _, err := azimg.Open(ctx, bytes.NewReader(raw), int64(len(raw)), azimg.Limits{}); !errors.Is(err, tc.want) {
			t.Errorf("%s err = %v, want %v", name, err, tc.want)
		}
	}

	// 5. Configured Limits enforced on outer AND nested archives.
	for name, lim := range map[string]azimg.Limits{
		"MaxCompressedSize": {MaxCompressedSize: 64},
		"MaxExpandedSize":   {MaxExpandedSize: 20},
		"MaxArchiveEntries": {MaxArchiveEntries: 3},
		"MaxMemberSize":     {MaxMemberSize: 4},
		"MaxManifestSize":   {MaxManifestSize: 8},
	} {
		im, err := azimg.Open(ctx, bytes.NewReader(goodImg), int64(len(goodImg)), lim)
		if err == nil {
			_, err = im.Bundle(ctx)
		}
		if !errors.Is(err, azimg.ErrLimitExceeded) {
			t.Errorf("%s err = %v, want ErrLimitExceeded", name, err)
		}
	}

	// Outer MaxMemberSize tested without ui-packs.tar so nested MaxMemberSize cannot mask it (M13).
	outerMemTar := func() []byte {
		top := "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle"
		ver := []byte("5.3.0.4\n")
		mod := []byte("SCLIVE4\n")
		big := bytes.Repeat([]byte("B"), 300)
		sv, sm, sb := sha256.Sum256(ver), sha256.Sum256(mod), sha256.Sum256(big)
		sums := []byte(hex.EncodeToString(sv[:]) + "  version.txt\n" +
			hex.EncodeToString(sm[:]) + "  model\n" +
			hex.EncodeToString(sb[:]) + "  bin/big\n")
		raw := buildRawTar(t, []tarEntrySpec{
			{name: top, typeflag: tar.TypeDir, mode: 0o755},
			{name: top + "/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: ver},
			{name: top + "/model", typeflag: tar.TypeReg, mode: 0o644, data: mod},
			{name: top + "/bin/big", typeflag: tar.TypeReg, mode: 0o644, data: big},
			{name: top + "/SHA256SUMS", typeflag: tar.TypeReg, mode: 0o644, data: sums},
		})
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		_, _ = gw.Write(raw)
		_ = gw.Close()
		return gz.Bytes()
	}()
	if _, err := azimg.Open(ctx, bytes.NewReader(outerMemTar), int64(len(outerMemTar)), azimg.Limits{MaxMemberSize: 250}); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Errorf("outer MaxMemberSize err = %v, want ErrLimitExceeded", err)
	}
	if _, err := azimg.Open(ctx, bytes.NewReader(outerMemTar), int64(len(outerMemTar)), azimg.Limits{MaxArchiveEntries: 3}); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Errorf("outer MaxArchiveEntries err = %v, want ErrLimitExceeded", err)
	}

	// Nested ui-packs.tar placed LAST (after SHA256SUMS) so outer loop cannot mask M41.
	nestedUIOnly := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/a.qml", typeflag: tar.TypeReg, mode: 0o644, data: bytes.Repeat([]byte("Q"), 200)},
		{name: "packs/b.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("B")},
		{name: "packs/c.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("C")},
	})
	nestedLastTarGz := func() []byte {
		top := "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle"
		ver := []byte("5.3.0.4\n")
		mod := []byte("SCLIVE4\n")
		sv, sm, su := sha256.Sum256(ver), sha256.Sum256(mod), sha256.Sum256(nestedUIOnly)
		sums := []byte(hex.EncodeToString(sv[:]) + "  version.txt\n" +
			hex.EncodeToString(sm[:]) + "  model\n" +
			hex.EncodeToString(su[:]) + "  ui-packs.tar\n")
		raw := buildRawTar(t, []tarEntrySpec{
			{name: top, typeflag: tar.TypeDir, mode: 0o755},
			{name: top + "/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: ver},
			{name: top + "/model", typeflag: tar.TypeReg, mode: 0o644, data: mod},
			{name: top + "/SHA256SUMS", typeflag: tar.TypeReg, mode: 0o644, data: sums},
			{name: top + "/ui-packs.tar", typeflag: tar.TypeReg, mode: 0o644, data: nestedUIOnly},
		})
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		_, _ = gw.Write(raw)
		_ = gw.Close()
		return gz.Bytes()
	}()
	// Outer has 5 entries (top + version.txt + model + SHA256SUMS + ui-packs.tar); nested adds 4 -> 9.
	if _, err := azimg.Open(ctx, bytes.NewReader(nestedLastTarGz), int64(len(nestedLastTarGz)), azimg.Limits{MaxArchiveEntries: 6}); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Errorf("nested MaxArchiveEntries err = %v, want ErrLimitExceeded", err)
	}
	// Nested ui-packs.tar header declaring h.Size=128 KiB inside a small 1.5 KiB ui-packs.tar
	// must fail nested MaxMemberSize (64 KiB) with ErrLimitExceeded even though outer ui-packs.tar passes.
	var truncUI bytes.Buffer
	twUI := tar.NewWriter(&truncUI)
	_ = twUI.WriteHeader(&tar.Header{Name: "packs", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = twUI.WriteHeader(&tar.Header{Name: "packs/big.qml", Typeflag: tar.TypeReg, Mode: 0o644, Size: 128 << 10})
	// Note: do not write 128 KiB payload; ui-packs.tar stays ~1.5 KiB.
	nestedMemberLimTar := buildTestBundleTarGz(t, bundleBuildOpts{
		extra: map[string][]byte{"ui-packs.tar": truncUI.Bytes()},
	})
	if _, err := azimg.Open(ctx, bytes.NewReader(nestedMemberLimTar), int64(len(nestedMemberLimTar)), azimg.Limits{MaxMemberSize: 64 << 10}); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Errorf("nested MaxMemberSize err = %v, want ErrLimitExceeded", err)
	}
	// Outer files are <2500 bytes, ui-packs.tar itself is 2560 bytes, wait: set MaxMemberSize only?
	// Test nested MaxMemberSize where outer ui-packs.tar is smaller? No: outer ui-packs.tar >= sum(nested).
	// Instead test nested duplicate path in ui-packs.tar!
	dupUITar := buildTestBundleTarGz(t, bundleBuildOpts{
		extra: map[string][]byte{
			"ui-packs.tar": buildRawTar(t, []tarEntrySpec{
				{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
				{name: "packs/Main.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("a")},
				{name: "./packs/main.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("b")},
			}),
		},
	})
	if _, err := azimg.Open(ctx, bytes.NewReader(dupUITar), int64(len(dupUITar)), azimg.Limits{}); !errors.Is(err, azimg.ErrCorrupt) {
		t.Errorf("nested case-insensitive duplicate err = %v, want ErrCorrupt", err)
	}

	// 6. Outer hostile entry types, traversal/outside-top, and duplicate paths (all with valid SHA256SUMS).
	top := "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle"
	outerHostiles := map[string]struct {
		spec tarEntrySpec
		want error
	}{
		"outside-top":  {tarEntrySpec{name: "OTHER/evil", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"traversal":    {tarEntrySpec{name: top + "/../escape", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"abs-path":     {tarEntrySpec{name: "/etc/passwd", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"win-drive":    {tarEntrySpec{name: `C:\Windows\win.ini`, typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"unc-path":     {tarEntrySpec{name: `\\server\share\x`, typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"symlink":      {tarEntrySpec{name: top + "/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/etc/passwd"}, azimg.ErrUnsafePath},
		"hardlink":     {tarEntrySpec{name: top + "/hard", typeflag: tar.TypeLink, mode: 0o644, linkname: "version.txt"}, azimg.ErrUnsafePath},
		"chardev":      {tarEntrySpec{name: top + "/dev", typeflag: tar.TypeChar, mode: 0o600}, azimg.ErrUnsafePath},
		"dup-outer":    {tarEntrySpec{name: "./" + top + "/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("5.3.0.4\n")}, azimg.ErrCorrupt},
		"dup-case":     {tarEntrySpec{name: top + "/VERSION.TXT", typeflag: tar.TypeReg, mode: 0o644, data: []byte("5.3.0.4\n")}, azimg.ErrCorrupt},
		"dup-nfc-nfd":  {tarEntrySpec{name: top + "/cafe\u0301.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrCorrupt},
		"win-reserved": {tarEntrySpec{name: top + "/aux.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"ntfs-ads":     {tarEntrySpec{name: top + "/a:b", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"trail-dot":    {tarEntrySpec{name: top + "/x.", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"trail-space":  {tarEntrySpec{name: top + "/x ", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"bidi-rlo":     {tarEntrySpec{name: top + "/\u202etxt.exe", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"c1-ctrl":      {tarEntrySpec{name: top + "/a\u0085b", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"bad-utf8":     {tarEntrySpec{name: top + "/\xff\xfe", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
		"long-seg":     {tarEntrySpec{name: top + "/" + strings.Repeat("a", 300), typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")}, azimg.ErrUnsafePath},
	}
	for name, tc := range outerHostiles {
		raw := buildTestBundleTarGz(t, bundleBuildOpts{
			extra:           map[string][]byte{"caf\u00e9.txt": []byte("x")},
			rawExtraEntries: []tarEntrySpec{tc.spec},
		})
		if _, err := azimg.Open(ctx, bytes.NewReader(raw), int64(len(raw)), azimg.Limits{}); !errors.Is(err, tc.want) {
			t.Errorf("outer hostile %s err = %v, want %v", name, err, tc.want)
		}
	}

	// 7. Wrapper model/version mismatch against inner bundle rejected.
	mismatchModelImg := wrapGen2Image(tarGz, "SYSTEMONE", "5.3.0.4")
	imMisModel, err := azimg.Open(ctx, bytes.NewReader(mismatchModelImg), int64(len(mismatchModelImg)), azimg.Limits{})
	if err != nil {
		t.Fatalf("Open mismatchModelImg: %v", err)
	}
	if _, err := imMisModel.Bundle(ctx); !errors.Is(err, azimg.ErrModelMismatch) {
		t.Fatalf("wrapper vs bundle model mismatch err = %v, want ErrModelMismatch", err)
	}
	mismatchVerImg := wrapGen2Image(tarGz, "SCLIVE4", "5.3.0.5")
	imMisVer, err := azimg.Open(ctx, bytes.NewReader(mismatchVerImg), int64(len(mismatchVerImg)), azimg.Limits{})
	if err != nil {
		t.Fatalf("Open mismatchVerImg: %v", err)
	}
	if _, err := imMisVer.Bundle(ctx); !errors.Is(err, azimg.ErrCorrupt) {
		t.Fatalf("wrapper vs bundle version mismatch err = %v, want ErrCorrupt", err)
	}

	// 8. Nested ui-packs.tar without packs/ or with symlink rejected.
	noPacksUI := buildRawTar(t, []tarEntrySpec{
		{name: "icons/x.svg", typeflag: tar.TypeReg, mode: 0o644, data: []byte("<svg/>")},
	})
	badUITarGz := buildTestBundleTarGz(t, bundleBuildOpts{extra: map[string][]byte{"ui-packs.tar": noPacksUI}})
	if _, err := azimg.Open(ctx, bytes.NewReader(badUITarGz), int64(len(badUITarGz)), azimg.Limits{}); !errors.Is(err, azimg.ErrCorrupt) {
		t.Fatalf("ui-packs.tar without packs/ err = %v, want ErrCorrupt", err)
	}
	symlinkUI := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/evil", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/etc/shadow"},
	})
	symUITarGz := buildTestBundleTarGz(t, bundleBuildOpts{extra: map[string][]byte{"ui-packs.tar": symlinkUI}})
	if _, err := azimg.Open(ctx, bytes.NewReader(symUITarGz), int64(len(symUITarGz)), azimg.Limits{}); !errors.Is(err, azimg.ErrUnsafePath) {
		t.Fatalf("symlink in ui-packs.tar err = %v, want ErrUnsafePath", err)
	}
}

// TestGen2PAXHeaderBombAndLongPathAllocation verifies that PAX extended header bombs
// are charged against MaxExpandedSize and overlong archive paths fail before large allocations.
func TestGen2PAXHeaderBombAndLongPathAllocation(t *testing.T) {
	ctx := context.Background()
	top := "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle"

	// 1. Entries with 500,000-byte names must be rejected on entry 0 without large allocations.
	var longBuf bytes.Buffer
	gw1, err := gzip.NewWriterLevel(&longBuf, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw1 := tar.NewWriter(gw1)
	_ = tw1.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755})
	for i := 0; i < 10; i++ {
		name := top + "/" + fmt.Sprintf("%05d", i) + strings.Repeat("a", 500_000)
		if err := tw1.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw1.Close()
	_ = gw1.Close()

	before := totalAllocBytes()
	_, err = azimg.Open(ctx, bytes.NewReader(longBuf.Bytes()), int64(longBuf.Len()), azimg.Limits{})
	delta := totalAllocBytes() - before
	if !errors.Is(err, azimg.ErrUnsafePath) {
		t.Fatalf("long archive path err = %v, want ErrUnsafePath", err)
	}
	if delta > 32<<20 {
		t.Fatalf("long archive path allocated %d MiB before rejection", delta>>20)
	}

	// 2. ~16 MiB of consecutive PAX extended headers must hit MaxExpandedSize (8 MiB) early.
	var head bytes.Buffer
	twHead := tar.NewWriter(&head)
	_ = twHead.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755})
	var one bytes.Buffer
	twOne := tar.NewWriter(&one)
	_ = twOne.WriteHeader(&tar.Header{
		Name: top + "/f", Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX,
		PAXRecords: map[string]string{"comment": strings.Repeat("A", 500_000)},
	})
	xPart := one.Bytes()[:one.Len()-512]
	var paxRaw bytes.Buffer
	gw2, err := gzip.NewWriterLevel(&paxRaw, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = gw2.Write(head.Bytes())
	for i := 0; i < 32; i++ {
		_, _ = gw2.Write(xPart)
	}
	_, _ = gw2.Write(one.Bytes())
	_, _ = gw2.Write(make([]byte, 1024))
	_ = gw2.Close()

	ra := &mutatingReaderAt{b: paxRaw.Bytes()}
	_, err = azimg.Open(ctx, ra, int64(len(ra.b)), azimg.Limits{MaxExpandedSize: 8 << 20})
	if !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("PAX header bomb err = %v, want ErrLimitExceeded", err)
	}
	if frac := float64(ra.n.Load()) / float64(len(ra.b)); frac > 0.75 {
		t.Fatalf("PAX header bomb read fraction %.2f > 0.75 before rejection", frac)
	}

	// 3. Gzip trailer zero-padding bomb exceeding remainingBudget (MaxExpandedSize - gzOut.n - uiExpanded)
	// while still staying below gzOut's standalone cap (kills M26).
	uiPayload := bytes.Repeat([]byte("U"), 8192)
	uiTar := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/ui.qml", typeflag: tar.TypeReg, mode: 0o644, data: uiPayload},
	})
	baseTarGz := buildTestBundleTarGz(t, bundleBuildOpts{
		gzipStored: true,
		extra:      map[string][]byte{"ui-packs.tar": uiTar},
	})
	// Inflate baseTarGz to inspect uncompressed tar length, then append 5 KiB trailing zeros
	// and pick MaxExpandedSize so gzOut.n + trailer <= MaxExpandedSize < gzOut.n + uiExpanded + trailer.
	gzr, err := gzip.NewReader(bytes.NewReader(baseTarGz))
	if err != nil {
		t.Fatal(err)
	}
	baseTarBytes, err := io.ReadAll(gzr)
	_ = gzr.Close()
	if err != nil {
		t.Fatal(err)
	}
	var zeroBomb bytes.Buffer
	gw3 := gzip.NewWriter(&zeroBomb)
	_, _ = gw3.Write(baseTarBytes)
	_, _ = gw3.Write(make([]byte, 5120))
	_ = gw3.Close()
	limM26 := azimg.Limits{MaxExpandedSize: int64(len(baseTarBytes)) + 10000}
	if _, err := azimg.Open(ctx, bytes.NewReader(zeroBomb.Bytes()), int64(zeroBomb.Len()), limM26); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("gzip trailer bomb err = %v, want ErrLimitExceeded", err)
	}
}

func TestHostAndArchivePathsSeparation(t *testing.T) {
	// Windows host paths allow backslashes and drive/UNC paths; POSIX host paths reject backslashes.
	if err := azimg.ValidateHostPath(`C:\Users\DJ\SC5000-5.3.0.4-Update.img`, true); err != nil {
		t.Errorf("Windows host path rejected: %v", err)
	}
	if err := azimg.ValidateHostPath(`\\nas\fw\PRIME4-Update.img`, true); err != nil {
		t.Errorf("Windows UNC host path rejected: %v", err)
	}
	if err := azimg.ValidateHostPath(`C:\Users\DJ\img.img`, false); !errors.Is(err, azimg.ErrUnsafePath) {
		t.Errorf("POSIX host path accepted backslash: %v", err)
	}
	for _, bad := range []string{"", "/tmp/bad\"name", "/tmp/bad\nname", "/tmp/bad\x00name", "/tmp/bad\u202ename", "/tmp/\xff"} {
		if err := azimg.ValidateHostPath(bad, true); err == nil {
			t.Errorf("host path %q accepted on Windows", bad)
		}
	}

	// Canonical archive paths reject unsafe/Windows/overlong/invalid-UTF-8 inputs.
	for _, bad := range []string{
		"", "/", "/etc/passwd", "//server/share", `\\server\share`, `C:foo`, `C:\foo`, `C:/foo`,
		`a\b`, "a:b", "../a", "a/../b", "a/./b", "a//b", "a/b/", ".\x00a", "a\nb",
		"docs/CON", "docs/aux.txt", "docs/x.", "docs/x ", "docs/\u202etxt.exe", "docs/a\u0085b",
		"docs/\xff\xfe", "docs/" + strings.Repeat("a", 256), strings.Repeat("a/", 2050) + "f",
	} {
		if _, err := azimg.CleanArchivePath(bad, false); !errors.Is(err, azimg.ErrUnsafePath) {
			t.Errorf("archive path %q accepted or wrong err: %v", bad, err)
		}
	}
	if m, ok := azimg.LookupModel("prime4+"); !ok || m.Prefix != "PRIME4PLUS" || m.Board != "jc11s" {
		t.Errorf("LookupModel(prime4+) = %+v, %v", m, ok)
	}
	if m, ok := azimg.LookupModel("rk3288-az05-jp11s"); !ok || m.Prefix != "PRIMEGOPLUS" || m.Board != "jp11s" {
		t.Errorf("LookupModel(rk3288-az05-jp11s) = %+v, %v", m, ok)
	}
	if got, err := azimg.CleanArchivePath("./packs/en/Main.qml", false); err != nil || got != "packs/en/Main.qml" {
		t.Errorf("CleanArchivePath valid = %q, err=%v", got, err)
	}
}

// TestModelTablesMatchInstallerMain guards against drift between azimg's model tables
// and cmd/enginex-installer/main.go's gen1Boards / gen2Boards without cross-package imports.
func TestModelTablesMatchInstallerMain(t *testing.T) {
	mainPath := filepath.Join("..", "..", "..", "cmd", "enginex-installer", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}

	type boardSpec struct{ prefix, fullName string }
	extractMap := func(varName string) map[string]boardSpec {
		out := map[string]boardSpec{}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) == 0 || vs.Names[0].Name != varName {
					continue
				}
				cl, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s is not a composite literal", varName)
				}
				for _, elt := range cl.Elts {
					kv := elt.(*ast.KeyValueExpr)
					k, _ := strconv.Unquote(kv.Key.(*ast.BasicLit).Value)
					val := kv.Value.(*ast.CompositeLit)
					p, _ := strconv.Unquote(val.Elts[0].(*ast.BasicLit).Value)
					n, _ := strconv.Unquote(val.Elts[1].(*ast.BasicLit).Value)
					out[k] = boardSpec{prefix: p, fullName: n}
				}
			}
		}
		return out
	}

	gen1Main := extractMap("gen1Boards")
	gen2Main := extractMap("gen2Boards")
	if len(gen1Main) != 8 || len(gen2Main) != 8 {
		t.Fatalf("unexpected board counts in main.go: gen1=%d gen2=%d", len(gen1Main), len(gen2Main))
	}

	for board, spec := range gen1Main {
		mCompat, ok := azimg.LookupGen1Compatible("inmusic," + board)
		if !ok {
			t.Errorf("azimg missing gen1 compatible %q", "inmusic,"+board)
			continue
		}
		if mCompat.Board != board || mCompat.Prefix != spec.prefix || mCompat.FullName != spec.fullName || mCompat.Gen2 {
			t.Errorf("gen1 drift for %q: azimg=%+v main=%+v", board, mCompat, spec)
		}
		if mBoard, ok := azimg.LookupModel(board); !ok || mBoard != mCompat {
			t.Errorf("LookupModel(%q) mismatch: %+v vs %+v", board, mBoard, mCompat)
		}
	}
	for board, spec := range gen2Main {
		mPrefix, ok := azimg.LookupGen2Prefix(spec.prefix)
		if !ok {
			t.Errorf("azimg missing gen2 prefix %q", spec.prefix)
			continue
		}
		if mPrefix.Board != board || mPrefix.Prefix != spec.prefix || mPrefix.FullName != spec.fullName || !mPrefix.Gen2 {
			t.Errorf("gen2 drift for %q: azimg=%+v main=%+v", board, mPrefix, spec)
		}
		if mBoard, ok := azimg.LookupModel(board); !ok || mBoard != mPrefix {
			t.Errorf("LookupModel(%q) mismatch: %+v vs %+v", board, mBoard, mPrefix)
		}
	}
}

// TestRealFixturesParity runs against real firmware fixtures when ENGINEX_AZIMG_PARITY=1.
// It verifies byte-exact parity against tools/az01-packer/az01_packer.py for all 3 official
// 5.3.0 AZ01 groups and EngineX 5.3.0.4 AZ01 images, fails if signed vendor AZ0x/AZ04 fixtures
// are missing or accepted, and validates Gen-2 *-Update.img archives when ENGINEX_GEN2_FIXTURE_DIR is set.
func TestRealFixturesParity(t *testing.T) {
	if os.Getenv("ENGINEX_AZIMG_PARITY") != "1" {
		t.Skip("set ENGINEX_AZIMG_PARITY=1 with firmware fixtures directory")
	}
	ctx := context.Background()

	labDir := os.Getenv("ENGINEX_LAB_DIR")
	if labDir == "" {
		labDir = "/root/build/sc5000-lab"
	}
	packer := filepath.Join("..", "..", "..", "tools", "az01-packer", "az01_packer.py")
	if _, err := os.Stat(packer); err != nil {
		t.Fatalf("missing az01_packer.py at %s: %v", packer, err)
	}

	az01Images := []string{
		filepath.Join(labDir, "fw-5.3.0", "SC5000-5.3.0-Update.img"),
		filepath.Join(labDir, "fw-5.3.0", "SC6000-5.3.0-Update.img"),
		filepath.Join(labDir, "fw-5.3.0", "PRIME4-5.3.0-Update.img"),
		filepath.Join(labDir, "release-5.3.0.4", "SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"),
		filepath.Join(labDir, "release-5.3.0.4", "SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img"),
		filepath.Join(labDir, "release-5.3.0.4", "PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img"),
	}
	for _, imgPath := range az01Images {
		if _, err := os.Stat(imgPath); err != nil {
			t.Fatalf("missing AZ01 fixture %s: %v", imgPath, err)
		}
		t.Run(filepath.Base(imgPath), func(t *testing.T) {
			pyOut := filepath.Join(t.TempDir(), "py-unpacked")
			cmd := exec.CommandContext(ctx, "python3", "-I", packer, "unpack", imgPath, "--output", pyOut)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("az01_packer.py unpack failed: %v\n%s", err, out)
			}
			manifestBytes, err := os.ReadFile(filepath.Join(pyOut, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			var pyInfo struct {
				Compatible []string `json:"compatible"`
				Build      string   `json:"build"`
				Verified   bool     `json:"verified"`
			}
			if err := json.Unmarshal(manifestBytes, &pyInfo); err != nil {
				t.Fatal(err)
			}
			if !pyInfo.Verified {
				t.Fatal("python unpack not verified")
			}

			f, err := os.Open(imgPath)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			im, err := azimg.Open(ctx, f, st.Size(), azimg.Limits{})
			if err != nil {
				t.Fatalf("azimg.Open: %v", err)
			}
			meta, ok := im.AZ01()
			if !ok || meta.Build != pyInfo.Build || len(meta.Compatible) != len(pyInfo.Compatible) {
				t.Fatalf("metadata mismatch: go=%+v py=%+v", meta, pyInfo)
			}

			goHash := sha256.New()
			goSize, err := im.ExtractRootfs(ctx, goHash)
			if err != nil {
				t.Fatalf("ExtractRootfs: %v", err)
			}
			pyRootfs, err := os.Open(filepath.Join(pyOut, "rootfs.ext2"))
			if err != nil {
				t.Fatal(err)
			}
			defer pyRootfs.Close()
			pyHash := sha256.New()
			pySize, err := io.Copy(pyHash, pyRootfs)
			if err != nil {
				t.Fatal(err)
			}
			if goSize != pySize || !bytes.Equal(goHash.Sum(nil), pyHash.Sum(nil)) {
				t.Fatalf("rootfs parity mismatch: go=(%d, %x) py=(%d, %x)", goSize, goHash.Sum(nil), pySize, pyHash.Sum(nil))
			}

			for _, part := range []string{"splash", "recoverysplash"} {
				gh := sha256.New()
				if _, err := im.ExtractPartition(ctx, part, gh); err != nil {
					t.Fatalf("ExtractPartition(%s): %v", part, err)
				}
				pb, err := os.ReadFile(filepath.Join(pyOut, part+".payload"))
				if err != nil {
					t.Fatal(err)
				}
				ps := sha256.Sum256(pb)
				if !bytes.Equal(gh.Sum(nil), ps[:]) {
					t.Fatalf("%s parity mismatch: go=%x py=%x", part, gh.Sum(nil), ps)
				}
			}
		})
	}

	// Signed vendor AZ0x / AZ04 images must exist and be rejected with ErrSignedContainer.
	for _, signedPath := range []string{
		filepath.Join(labDir, "fw-5.3.0", "SCLIVE4-5.3.0-Update.img"),
		filepath.Join(labDir, "fw-5.3.0", "SCLIVE2-5.3.0-Update.img"),
		filepath.Join(labDir, "fw-5.3.0-gen2", "PRIME4G2-5.3.0-Update.img"),
	} {
		f, err := os.Open(signedPath)
		if err != nil {
			t.Fatalf("missing signed vendor fixture %s: %v", signedPath, err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		_, err = azimg.Open(ctx, f, st.Size(), azimg.Limits{})
		_ = f.Close()
		if !errors.Is(err, azimg.ErrSignedContainer) {
			t.Errorf("signed vendor container %s err = %v, want ErrSignedContainer", signedPath, err)
		}
	}

	if gen2Dir := os.Getenv("ENGINEX_GEN2_FIXTURE_DIR"); gen2Dir != "" {
		imgs, err := filepath.Glob(filepath.Join(gen2Dir, "*-Update.img"))
		if err != nil || len(imgs) == 0 {
			t.Fatalf("no *-Update.img fixtures in %s (err=%v)", gen2Dir, err)
		}
		for _, p := range imgs {
			t.Run("gen2/"+filepath.Base(p), func(t *testing.T) {
				f, err := os.Open(p)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				st, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				im, err := azimg.Open(ctx, f, st.Size(), azimg.Limits{})
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				b, err := im.Bundle(ctx)
				if err != nil {
					t.Fatalf("Bundle: %v", err)
				}
				var count, uiCount int
				if err := b.Walk(ctx, func(e azimg.Entry) error {
					count++
					rc, err := e.Open()
					if err != nil {
						return err
					}
					_, rerr := io.Copy(io.Discard, rc)
					cerr := rc.Close()
					if rerr != nil {
						return rerr
					}
					return cerr
				}); err != nil {
					t.Fatalf("Walk: %v", err)
				}
				if err := b.WalkUI(ctx, func(e azimg.Entry) error {
					uiCount++
					rc, err := e.Open()
					if err != nil {
						return err
					}
					_, rerr := io.Copy(io.Discard, rc)
					cerr := rc.Close()
					if rerr != nil {
						return rerr
					}
					return cerr
				}); err != nil {
					t.Fatalf("WalkUI: %v", err)
				}
				if count < 10 || uiCount < 100 || !b.HasUIPacks() {
					t.Fatalf("unexpected counts for %s: outer=%d ui=%d", p, count, uiCount)
				}
			})
		}
	}
}
