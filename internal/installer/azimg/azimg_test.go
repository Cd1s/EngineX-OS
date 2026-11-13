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
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	hdr.Write(az01String("test-build"))
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

	// Corrupt version, unknown model, wrong partition name, wrong algo, trailing bytes, corrupt payload/digest/EOF.
	badCases := map[string][]byte{
		"version-2":       buildSyntheticAZ01(az01Opts{version: 2}),
		"unknown-compat":  buildSyntheticAZ01(az01Opts{compat: []string{"inmusic,xx99"}}),
		"bad-part-name":   buildSyntheticAZ01(az01Opts{rootfsName: "uboot"}),
		"bad-part-path":   buildSyntheticAZ01(az01Opts{rootfsName: "../rootfs"}),
		"bad-comp":        buildSyntheticAZ01(az01Opts{rootfsComp: "none"}),
		"bad-hash-algo":   buildSyntheticAZ01(az01Opts{hashAlgo: "sha256"}),
		"extra-signature": buildSyntheticAZ01(az01Opts{extraTrail: []byte("sig")}),
	}
	for name, b := range badCases {
		if _, err := azimg.Open(ctx, bytes.NewReader(b), int64(len(b)), azimg.Limits{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
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
		"trailing-junk": append(append([]byte(nil), payload...), []byte("junk")...),
		"concatenated":  append(append([]byte(nil), payload...), payload...),
		"truncated":     payload[:len(payload)-1],
		"not-xz":        []byte("notxz"),
		"no-check":      compressXZ(t, []byte("hello"), 0),
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

	// Hostile 4 GiB LZMA2 dictionary in XZ block header must be rejected before allocation.
	hostileDictXZ := buildHostileDictXZ(0x28) // 4 GiB - 1 dictionary
	dictImg := buildSyntheticAZ01(az01Opts{rootfs: hostileDictXZ})
	imDict, err := azimg.Open(ctx, bytes.NewReader(dictImg), int64(len(dictImg)), azimg.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imDict.ExtractRootfs(ctx, io.Discard); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("hostile LZMA2 dict err = %v, want ErrLimitExceeded", err)
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

func buildHostileDictXZ(dictProp byte) []byte {
	var b bytes.Buffer
	// XZ Stream Header (12 bytes): magic + flags (CRC32) + CRC32
	b.Write([]byte{0xfd, '7', 'z', 'X', 'Z', 0})
	flags := []byte{0x00, 0x01}
	b.Write(flags)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(flags))
	// XZ Block Header (12 bytes): size=2 -> 12 bytes, flags=0x00 (1 filter, no sizes), filter=LZMA2 (0x21, len=1, dictProp), pad, CRC32
	bh := []byte{0x02, 0x00, 0x21, 0x01, dictProp, 0x00, 0x00, 0x00}
	b.Write(bh)
	_ = binary.Write(&b, binary.LittleEndian, crc32.ChecksumIEEE(bh))
	// Minimal trailing padding so stream length >= 24
	b.Write(make([]byte, 12))
	return b.Bytes()
}

type tarEntrySpec struct {
	name     string
	typeflag byte
	mode     int64
	data     []byte
	linkname string
}

func buildRawTar(t *testing.T, specs []tarEntrySpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, s := range specs {
		h := &tar.Header{
			Name:     s.name,
			Mode:     s.mode,
			Size:     int64(len(s.data)),
			Typeflag: s.typeflag,
			Linkname: s.linkname,
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

func buildValidGen2BundleTarGz(t *testing.T, prefix, version string, extra map[string][]byte, omitFromSums map[string]bool) []byte {
	t.Helper()
	top := prefix + "-" + version + "-EngineX-OS-Data-Bundle"
	uiTar := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/en/Main.qml", typeflag: tar.TypeReg, mode: 0o644, data: []byte("Item {}\n")},
		{name: "icons/deezer.svg", typeflag: tar.TypeReg, mode: 0o644, data: []byte("<svg/>\n")},
	})
	files := map[string][]byte{
		"bin/sc5000-suite":         []byte("suite-bin"),
		"lib/libengine-ui-hook.so": []byte("hook-so"),
		"boot-activate.sh":         []byte("#!/bin/sh\n"),
		"99-sc5000-custom.conf":    []byte("pcm.!default {}\n"),
		"alsa-dir":                 []byte("system/var/overlay/lib/alsa/conf.d\n"),
		"version.txt":              []byte(version + "\n"),
		"release-tag":              []byte("v" + version + "-r4\n"),
		"model":                    []byte(prefix + "\n"),
		"ui-packs.tar":             uiTar,
	}
	for k, v := range extra {
		files[k] = v
	}
	var sums strings.Builder
	for rel, data := range files {
		if omitFromSums[rel] {
			continue
		}
		h := sha256.Sum256(data)
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + rel + "\n")
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755})
	for rel, data := range files {
		_ = tw.WriteHeader(&tar.Header{Name: top + "/" + rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	sumsBytes := []byte(sums.String())
	_ = tw.WriteHeader(&tar.Header{Name: top + "/SHA256SUMS", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(sumsBytes))})
	_, _ = tw.Write(sumsBytes)
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

func TestGen2ImageAndBundleValidWalkAndOpen(t *testing.T) {
	ctx := context.Background()
	tarGz := buildValidGen2BundleTarGz(t, "SCLIVE4", "5.3.0.4", nil, nil)
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
			b, err := im.Bundle(ctx)
			if err != nil {
				t.Fatalf("Bundle: %v", err)
			}
			if b.Version() != "5.3.0.4" || b.ReleaseTag() != "v5.3.0.4-r4" || b.Model().Prefix != "SCLIVE4" || !b.HasUIPacks() {
				t.Fatalf("unexpected Bundle meta: version=%q tag=%q model=%+v ui=%v", b.Version(), b.ReleaseTag(), b.Model(), b.HasUIPacks())
			}

			// Collect entries during Walk and open them AFTER Walk returns (both sequential and reverse order).
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
			// Reverse order + concurrent read verification.
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

func TestGen2HostileWrappersManifestsAndArchives(t *testing.T) {
	ctx := context.Background()
	tarGz := buildValidGen2BundleTarGz(t, "SCLIVE4", "5.3.0.4", nil, nil)
	goodImg := wrapGen2Image(tarGz, "SCLIVE4", "5.3.0.4")

	// 1. Corrupt 128-byte wrapper cases.
	badDirtyPad := append([]byte(nil), goodImg...)
	badDirtyPad[127] = 'X' // non-zero padding byte
	badModel := wrapGen2Image(tarGz, "UNKNOWN", "5.3.0.4")
	badVer := wrapGen2Image(tarGz, "SCLIVE4", "5.3")
	badSHA := append([]byte(nil), goodImg...)
	badSHA[len(badSHA)-1] ^= 1
	badGzip := wrapGen2Image([]byte("not gzip data"), "SCLIVE4", "5.3.0.4")
	for name, b := range map[string][]byte{
		"truncated-hdr": goodImg[:100],
		"hdr-only":      goodImg[:128],
		"dirty-padding": badDirtyPad,
		"bad-model":     badModel,
		"bad-version":   badVer,
		"payload-sha":   badSHA,
		"non-gzip":      badGzip,
	} {
		if _, err := azimg.Open(ctx, bytes.NewReader(b), int64(len(b)), azimg.Limits{}); err == nil {
			t.Errorf("%s accepted by Open", name)
		}
	}

	// 2. Trailing/concatenated gzip stream rejected by Bundle().
	concatImg := wrapGen2Image(append(append([]byte(nil), tarGz...), tarGz...), "SCLIVE4", "5.3.0.4")
	imConcat, err := azimg.Open(ctx, bytes.NewReader(concatImg), int64(len(concatImg)), azimg.Limits{})
	if err != nil {
		t.Fatalf("Open concatImg: %v", err)
	}
	if _, err := imConcat.Bundle(ctx); err == nil {
		t.Fatal("concatenated gzip stream was accepted")
	}

	// 3. Unlisted file smuggled into bundle rejected.
	smuggled := buildValidGen2BundleTarGz(t, "SCLIVE4", "5.3.0.4", map[string][]byte{"evil.sh": []byte("x")}, map[string]bool{"evil.sh": true})
	if _, err := azimg.Open(ctx, bytes.NewReader(smuggled), int64(len(smuggled)), azimg.Limits{}); err == nil {
		t.Fatal("unlisted smuggled bundle member was accepted")
	}

	// 4. Expanded budget including nested ui-packs.tar enforced.
	imSmall, err := azimg.Open(ctx, bytes.NewReader(goodImg), int64(len(goodImg)), azimg.Limits{MaxExpandedSize: 20})
	if err != nil {
		t.Fatalf("Open with small expanded limit: %v", err)
	}
	if _, err := imSmall.Bundle(ctx); !errors.Is(err, azimg.ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded for expanded budget, got %v", err)
	}

	// 5. Unsafe paths, duplicates, and unsupported link/device types in outer and nested tar.
	hostileMembers := []tarEntrySpec{
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/../escape", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")},
		{name: "/etc/passwd", typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")},
		{name: `C:\Windows\win.ini`, typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")},
		{name: `\\server\share\x`, typeflag: tar.TypeReg, mode: 0o644, data: []byte("x")},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/etc/passwd"},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/hard", typeflag: tar.TypeLink, mode: 0o644, linkname: "version.txt"},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/dev", typeflag: tar.TypeChar, mode: 0o600},
	}
	for _, hm := range hostileMembers {
		rawTar := buildRawTar(t, []tarEntrySpec{
			{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle", typeflag: tar.TypeDir, mode: 0o755},
			hm,
		})
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		_, _ = gw.Write(rawTar)
		_ = gw.Close()
		if _, err := azimg.Open(ctx, bytes.NewReader(gz.Bytes()), int64(gz.Len()), azimg.Limits{}); err == nil {
			t.Errorf("hostile member %q (type %q) was accepted", hm.name, hm.typeflag)
		}
	}

	// Duplicate member rejected.
	dupTar := buildRawTar(t, []tarEntrySpec{
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle", typeflag: tar.TypeDir, mode: 0o755},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("5.3.0.4\n")},
		{name: "./SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("5.3.0.4\n")},
	})
	var dupGz bytes.Buffer
	gw := gzip.NewWriter(&dupGz)
	_, _ = gw.Write(dupTar)
	_ = gw.Close()
	if _, err := azimg.Open(ctx, bytes.NewReader(dupGz.Bytes()), int64(dupGz.Len()), azimg.Limits{}); err == nil {
		t.Fatal("duplicate archive member was accepted")
	}

	// Duplicate manifest line in SHA256SUMS rejected.
	verSum := sha256.Sum256([]byte("5.3.0.4\n"))
	dupSums := hex.EncodeToString(verSum[:]) + "  version.txt\n" + hex.EncodeToString(verSum[:]) + "  version.txt\n"
	dupManifestTar := buildRawTar(t, []tarEntrySpec{
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle", typeflag: tar.TypeDir, mode: 0o755},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/version.txt", typeflag: tar.TypeReg, mode: 0o644, data: []byte("5.3.0.4\n")},
		{name: "SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle/SHA256SUMS", typeflag: tar.TypeReg, mode: 0o644, data: []byte(dupSums)},
	})
	var dupMGz bytes.Buffer
	gw2 := gzip.NewWriter(&dupMGz)
	_, _ = gw2.Write(dupManifestTar)
	_ = gw2.Close()
	if _, err := azimg.Open(ctx, bytes.NewReader(dupMGz.Bytes()), int64(dupMGz.Len()), azimg.Limits{}); err == nil {
		t.Fatal("duplicate SHA256SUMS entry was accepted")
	}

	// Wrapper model/version mismatch against inner bundle rejected.
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
	if _, err := imMisVer.Bundle(ctx); err == nil {
		t.Fatal("wrapper vs bundle version mismatch was accepted")
	}

	// Nested ui-packs.tar without packs/ directory rejected.
	noPacksUI := buildRawTar(t, []tarEntrySpec{
		{name: "icons/x.svg", typeflag: tar.TypeReg, mode: 0o644, data: []byte("<svg/>")},
	})
	badUITarGz := buildValidGen2BundleTarGz(t, "SCLIVE4", "5.3.0.4", map[string][]byte{"ui-packs.tar": noPacksUI}, nil)
	if _, err := azimg.Open(ctx, bytes.NewReader(badUITarGz), int64(len(badUITarGz)), azimg.Limits{}); err == nil {
		t.Fatal("ui-packs.tar without packs/ was accepted")
	}

	// Nested ui-packs.tar with symlink rejected.
	symlinkUI := buildRawTar(t, []tarEntrySpec{
		{name: "packs", typeflag: tar.TypeDir, mode: 0o755},
		{name: "packs/evil", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/etc/shadow"},
	})
	symUITarGz := buildValidGen2BundleTarGz(t, "SCLIVE4", "5.3.0.4", map[string][]byte{"ui-packs.tar": symlinkUI}, nil)
	if _, err := azimg.Open(ctx, bytes.NewReader(symUITarGz), int64(len(symUITarGz)), azimg.Limits{}); !errors.Is(err, azimg.ErrUnsafePath) {
		t.Fatalf("symlink in ui-packs.tar err = %v, want ErrUnsafePath", err)
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
	for _, bad := range []string{"", "/tmp/bad\"name", "/tmp/bad\nname", "/tmp/bad\x00name"} {
		if err := azimg.ValidateHostPath(bad, true); err == nil {
			t.Errorf("host path %q accepted on Windows", bad)
		}
	}

	// Canonical archive paths always reject Windows drive/UNC, backslashes, leading /, and traversal.
	for _, bad := range []string{
		"", "/", "/etc/passwd", "//server/share", `\\server\share`, `C:foo`, `C:\foo`, `C:/foo`,
		`a\b`, "../a", "a/../b", "a/./b", "a//b", "a/b/", ".\x00a", "a\nb",
	} {
		if _, err := azimg.CleanArchivePath(bad, false); err == nil {
			t.Errorf("archive path %q accepted", bad)
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

// TestRealFixturesParity runs against real firmware fixtures when ENGINEX_AZIMG_PARITY=1.
// On Oracle-Batam-arm, it verifies byte-exact parity against tools/az01-packer/az01_packer.py
// for all 3 official 5.3.0 AZ01 groups, EngineX 5.3.0.4 AZ01 images, rejects signed AZ0x/AZ04
// vendor images, and validates Gen-2 r4 *-Update.img archives.
func TestRealFixturesParity(t *testing.T) {
	if os.Getenv("ENGINEX_AZIMG_PARITY") != "1" {
		t.Skip("set ENGINEX_AZIMG_PARITY=1 on ARM build host with /root/build/sc5000-lab")
	}
	ctx := context.Background()

	packer := filepath.Join("..", "..", "..", "tools", "az01-packer", "az01_packer.py")
	if _, err := os.Stat(packer); err != nil {
		t.Fatalf("missing az01_packer.py at %s: %v", packer, err)
	}

	az01Images := []string{
		"/root/build/sc5000-lab/fw-5.3.0/SC5000-5.3.0-Update.img",
		"/root/build/sc5000-lab/fw-5.3.0/SC6000-5.3.0-Update.img",
		"/root/build/sc5000-lab/fw-5.3.0/PRIME4-5.3.0-Update.img",
		"/root/build/sc5000-lab/release-5.3.0.4/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img",
		"/root/build/sc5000-lab/release-5.3.0.4/SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img",
		"/root/build/sc5000-lab/release-5.3.0.4/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img",
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

			// Compare rootfs SHA-256 against Python-extracted rootfs.ext2 without buffering entire 400+ MiB in RAM.
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

			// Also verify splash and recoverysplash parity.
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

	// Signed vendor AZ0x / AZ04 images must be rejected with ErrSignedContainer.
	for _, signedPath := range []string{
		"/root/build/sc5000-lab/fw-5.3.0/SCLIVE4-5.3.0-Update.img",
		"/root/build/sc5000-lab/fw-5.3.0/SCLIVE2-5.3.0-Update.img",
		"/root/build/sc5000-lab/fw-5.3.0-gen2/PRIME4G2-5.3.0-Update.img",
	} {
		if _, err := os.Stat(signedPath); err != nil {
			continue
		}
		f, err := os.Open(signedPath)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := f.Stat()
		_, err = azimg.Open(ctx, f, st.Size(), azimg.Limits{})
		_ = f.Close()
		if !errors.Is(err, azimg.ErrSignedContainer) {
			t.Errorf("signed vendor container %s err = %v, want ErrSignedContainer", signedPath, err)
		}
	}

	// Real r4 Gen-2 *-Update.img fixtures if provided via ENGINEX_GEN2_FIXTURE_DIR.
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
				st, _ := f.Stat()
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
