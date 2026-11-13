package rockchip_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/rockchip"
)

type gptPartSpec struct {
	name     string
	startLBA uint64
	endLBA   uint64
}

func buildValidGPTDisk(totalSectors uint64, specs []gptPartSpec) []byte {
	disk := make([]byte, totalSectors*rockchip.SectorSize)
	// Protective MBR at LBA 0.
	disk[510] = 0x55
	disk[511] = 0xAA

	const numEntries = 128
	const entrySize = 128
	entryBytes := make([]byte, numEntries*entrySize)
	for i, sp := range specs {
		ent := entryBytes[i*entrySize : (i+1)*entrySize]
		ent[0] = 0xAF // non-zero TypeGUID
		ent[16] = byte(i + 1)
		binary.LittleEndian.PutUint64(ent[32:40], sp.startLBA)
		binary.LittleEndian.PutUint64(ent[40:48], sp.endLBA)
		u16 := utf16.Encode([]rune(sp.name))
		for j, u := range u16 {
			if j*2+1 >= 72 {
				break
			}
			binary.LittleEndian.PutUint16(ent[56+j*2:], u)
		}
	}
	copy(disk[2*rockchip.SectorSize:], entryBytes)

	hdr := disk[rockchip.SectorSize : 2*rockchip.SectorSize]
	copy(hdr[0:8], rockchip.GPTSignature)
	binary.LittleEndian.PutUint32(hdr[8:12], rockchip.GPTRevision1)
	binary.LittleEndian.PutUint32(hdr[12:16], 92)
	binary.LittleEndian.PutUint64(hdr[24:32], 1)                 // MyLBA
	binary.LittleEndian.PutUint64(hdr[32:40], totalSectors-1)    // AlternateLBA
	binary.LittleEndian.PutUint64(hdr[40:48], 34)                // FirstUsableLBA
	binary.LittleEndian.PutUint64(hdr[48:56], totalSectors-34)   // LastUsableLBA
	binary.LittleEndian.PutUint64(hdr[72:80], 2)                 // PartitionEntryLBA
	binary.LittleEndian.PutUint32(hdr[80:84], numEntries)        // NumberOfPartitionEntries
	binary.LittleEndian.PutUint32(hdr[84:88], entrySize)         // SizeOfPartitionEntry
	entryCRC := crc32.ChecksumIEEE(entryBytes)
	binary.LittleEndian.PutUint32(hdr[88:92], entryCRC)
	hdrCRC := crc32.ChecksumIEEE(hdr[:92])
	binary.LittleEndian.PutUint32(hdr[16:20], hdrCRC)
	return disk
}

type fakeRockUSBDevice struct {
	maskrom     bool
	chipInfo    [16]byte
	capability  [8]byte
	flashID     [5]byte
	flashInfo   [11]byte
	disk        []byte
	opcodesSeen []uint8
	inQueue     [][]byte
	corruptTag  bool
	cswOverride func(tag uint32, opcode uint8) []byte
}

func newFakeRockLoader(disk []byte) *fakeRockUSBDevice {
	var fi [11]byte
	totalSecs := uint32(len(disk) / rockchip.SectorSize)
	binary.LittleEndian.PutUint32(fi[0:4], totalSecs)
	binary.LittleEndian.PutUint16(fi[4:6], 1024)
	fi[6] = 8
	return &fakeRockUSBDevice{
		chipInfo:   [16]byte{'8', '8', '2', '3'},
		capability: [8]byte{0x01, 0x00},
		flashID:    [5]byte{0x45, 0x4d, 0x4d, 0x43, 0x00},
		flashInfo:  fi,
		disk:       disk,
	}
}

func makeCSW(tag, residue uint32, status uint8) []byte {
	csw := make([]byte, rockchip.CSWLength)
	binary.LittleEndian.PutUint32(csw[0:4], rockchip.CSWSignature)
	binary.LittleEndian.PutUint32(csw[4:8], tag)
	binary.LittleEndian.PutUint32(csw[8:12], residue)
	csw[12] = status
	return csw
}

func (d *fakeRockUSBDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if ep != 0x01 || len(data) != rockchip.CBWLength {
		return 0, errors.New("bad CBW write")
	}
	if sig := binary.LittleEndian.Uint32(data[0:4]); sig != rockchip.CBWSignature {
		return 0, errors.New("bad CBW signature")
	}
	tag := binary.LittleEndian.Uint32(data[4:8])
	opcode := data[15]
	d.opcodesSeen = append(d.opcodesSeen, opcode)

	sendCSW := func(status uint8, residue uint32) {
		if d.cswOverride != nil {
			d.inQueue = append(d.inQueue, d.cswOverride(tag, opcode))
			return
		}
		outTag := tag
		if d.corruptTag {
			outTag ^= 0x1
		}
		d.inQueue = append(d.inQueue, makeCSW(outTag, residue, status))
	}

	switch opcode {
	case rockchip.OpReadChipInfo:
		d.inQueue = append(d.inQueue, append([]byte(nil), d.chipInfo[:]...))
		sendCSW(0, 0)
	case rockchip.OpReadCapability:
		d.inQueue = append(d.inQueue, append([]byte(nil), d.capability[:]...))
		sendCSW(0, 0)
	case rockchip.OpTestUnitReady:
		if d.maskrom {
			sendCSW(1, 0)
		} else {
			sendCSW(0, 0)
		}
	case rockchip.OpReadFlashID:
		d.inQueue = append(d.inQueue, append([]byte(nil), d.flashID[:]...))
		sendCSW(0, 0)
	case rockchip.OpReadFlashInfo:
		d.inQueue = append(d.inQueue, append([]byte(nil), d.flashInfo[:]...))
		sendCSW(0, 0)
	case rockchip.OpReadLBA:
		lba := binary.BigEndian.Uint32(data[17:21])
		cnt := binary.BigEndian.Uint16(data[22:24])
		start := int(lba) * rockchip.SectorSize
		end := start + int(cnt)*rockchip.SectorSize
		if end > len(d.disk) {
			d.inQueue = append(d.inQueue, make([]byte, int(cnt)*rockchip.SectorSize))
			sendCSW(1, 0)
		} else {
			d.inQueue = append(d.inQueue, append([]byte(nil), d.disk[start:end]...))
			sendCSW(0, 0)
		}
	default:
		sendCSW(1, 0)
	}
	return len(data), nil
}

func (d *fakeRockUSBDevice) ReadBulk(ctx context.Context, ep uint8, buf []byte) (int, error) {
	if ep != 0x81 {
		return 0, errors.New("wrong IN ep")
	}
	if len(d.inQueue) == 0 {
		return 0, io.EOF
	}
	pkt := d.inQueue[0]
	if len(pkt) <= len(buf) {
		d.inQueue = d.inQueue[1:]
		return copy(buf, pkt), nil
	}
	n := copy(buf, pkt)
	d.inQueue[0] = pkt[n:]
	return n, nil
}

func TestProbeDistinguishesMaskROMFromLoaderAndRejectsWrongVID(t *testing.T) {
	disk := buildValidGPTDisk(2048, nil)
	loaderDev := newFakeRockLoader(disk)
	cl, _ := rockchip.New(loaderDev, proto.Endpoints{In: 0x81, Out: 0x01})

	if _, err := cl.Probe(context.Background(), 0x15e4, 0xd006); !errors.Is(err, rockchip.ErrWrongVendor) {
		t.Fatalf("expected ErrWrongVendor, got %v", err)
	}

	probeLoader, err := cl.Probe(context.Background(), rockchip.VID, rockchip.PIDRK3288)
	if err != nil {
		t.Fatalf("Probe(loader): %v", err)
	}
	if probeLoader.Mode != rockchip.ModeLoader || !probeLoader.StorageReady || probeLoader.ChipTag != "3288" {
		t.Fatalf("unexpected loader probe result: %+v", probeLoader)
	}

	// Same VID:PID 2207:320a when TEST_UNIT_READY fails -> ModeMaskROM (never inferred Loader from VID/PID!).
	maskDev := newFakeRockLoader(disk)
	maskDev.maskrom = true
	clMask, _ := rockchip.New(maskDev, proto.Endpoints{In: 0x81, Out: 0x01})
	probeMask, err := clMask.Probe(context.Background(), rockchip.VID, rockchip.PIDRK3288)
	if err != nil {
		t.Fatalf("Probe(maskrom): %v", err)
	}
	if probeMask.Mode != rockchip.ModeMaskROM || probeMask.StorageReady {
		t.Fatalf("expected ModeMaskROM with StorageReady=false, got %+v", probeMask)
	}
	if _, err := clMask.ReadLayout(context.Background(), probeMask); !errors.Is(err, rockchip.ErrNotLoader) {
		t.Fatalf("ReadLayout on MaskROM must fail with ErrNotLoader, got %v", err)
	}
}

func TestReadLayoutGPTValidationAndAllowlistWriteGate(t *testing.T) {
	disk := buildValidGPTDisk(2048, []gptPartSpec{
		{name: "uboot", startLBA: 100, endLBA: 199},
		{name: "boot", startLBA: 200, endLBA: 399},
		{name: "rootfs", startLBA: 400, endLBA: 899},
		{name: "data", startLBA: 900, endLBA: 1499},
	})
	// Fill rootfs with deterministic test pattern for readback check.
	rootfsOff := 400 * rockchip.SectorSize
	for i := 0; i < 4096+123; i++ {
		disk[rootfsOff+i] = byte(i*13 + 7)
	}

	dev := newFakeRockLoader(disk)
	cl, _ := rockchip.New(dev, proto.Endpoints{In: 0x81, Out: 0x01}, rockchip.WithMaxReadSectors(4))
	probe, err := cl.Probe(context.Background(), rockchip.VID, rockchip.PIDRK3588)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := cl.ReadLayout(context.Background(), probe)
	if err != nil {
		t.Fatalf("ReadLayout: %v", err)
	}
	if !layout.Verified || len(layout.Partitions) != 4 {
		t.Fatalf("unexpected layout: %+v", layout)
	}

	// Non-allowlisted partitions ("uboot", "boot", "misc") must be refused.
	for _, forbidden := range []string{"uboot", "boot", "loader", "gpt", ""} {
		if _, err := rockchip.PrepareVerifiedTarget(probe, layout, forbidden, 512); !errors.Is(err, rockchip.ErrTargetDisallowed) {
			t.Errorf("PrepareVerifiedTarget(%q) = %v, want ErrTargetDisallowed", forbidden, err)
		}
	}

	// Oversized image for rootfs refused.
	tooBig := int64(500*rockchip.SectorSize + 1)
	if _, err := rockchip.PrepareVerifiedTarget(probe, layout, "rootfs", tooBig); err == nil {
		t.Fatal("expected size-exceeded error for rootfs")
	}

	// Valid allowlisted target.
	target, err := rockchip.PrepareVerifiedTarget(probe, layout, "rootfs", 4096+123)
	if err != nil {
		t.Fatalf("PrepareVerifiedTarget(rootfs): %v", err)
	}

	// Readback matches disk bytes (including non-sector-aligned length).
	rbBuf := make([]byte, 4096+123)
	var gotProgress proto.Progress
	if err := cl.Readback(context.Background(), target, &memWriterAt{buf: rbBuf}, int64(len(rbBuf)), func(p proto.Progress) {
		gotProgress = p
	}); err != nil {
		t.Fatalf("Readback: %v", err)
	}
	if gotProgress.Sent != int64(len(rbBuf)) || !bytes.Equal(rbBuf, disk[rootfsOff:rootfsOff+len(rbBuf)]) {
		t.Fatal("Readback data or progress mismatch")
	}

	// WriteVerifiedTarget must fail closed with ErrWriteNotApproved and never emit OpWriteLBA (0x15).
	img := proto.Image{Data: bytes.NewReader(rbBuf), Size: int64(len(rbBuf))}
	if err := cl.WriteVerifiedTarget(context.Background(), target, img, nil); !errors.Is(err, rockchip.ErrWriteNotApproved) {
		t.Fatalf("expected ErrWriteNotApproved, got %v", err)
	}
	for _, op := range dev.opcodesSeen {
		if op == rockchip.OpWriteLBA {
			t.Fatal("OpWriteLBA must never be sent without approval")
		}
	}
	// Hand-crafted unverified target refused.
	if err := cl.WriteVerifiedTarget(context.Background(), rockchip.VerifiedTarget{
		Partition: rockchip.Partition{Name: "rootfs", StartLBA: 400, EndLBA: 500},
	}, img, nil); !errors.Is(err, rockchip.ErrTargetUnverified) {
		t.Fatalf("expected ErrTargetUnverified, got %v", err)
	}
}

func TestGPTCorruptionAndWrapperFailures(t *testing.T) {
	// 1. CSW tag mismatch.
	disk := buildValidGPTDisk(2048, []gptPartSpec{{name: "data", startLBA: 40, endLBA: 100}})
	devTag := newFakeRockLoader(disk)
	devTag.corruptTag = true
	clTag, _ := rockchip.New(devTag, proto.Endpoints{In: 0x81, Out: 0x01})
	if _, err := clTag.Probe(context.Background(), rockchip.VID, rockchip.PIDRK3288); !errors.Is(err, rockchip.ErrTagMismatch) {
		t.Fatalf("expected ErrTagMismatch, got %v", err)
	}

	// 2. Bad CSW length / signature / status.
	for _, badCSW := range [][]byte{
		[]byte("short"),
		func() []byte { b := makeCSW(1, 0, 0); b[0] = 'X'; return b }(),
		makeCSW(1, 0, 9),
		makeCSW(1, 999, 0),
	} {
		if _, err := rockchip.ParseCSW(badCSW, 1, 16); !errors.Is(err, rockchip.ErrBadWrapper) {
			t.Errorf("ParseCSW(%x) = %v, want ErrBadWrapper", badCSW, err)
		}
	}

	// 3. Corrupt GPT header CRC.
	badHdr := append([]byte(nil), disk...)
	badHdr[rockchip.SectorSize+24] ^= 0x01
	if _, err := rockchip.ParseGPTHeader(badHdr[rockchip.SectorSize:2*rockchip.SectorSize], 2048); !errors.Is(err, rockchip.ErrGPTChecksum) {
		t.Fatalf("expected ErrGPTChecksum on header, got %v", err)
	}

	// 4. Corrupt GPT entry array CRC.
	badEnt := append([]byte(nil), disk...)
	badEnt[2*rockchip.SectorSize+5] ^= 0x80
	hdr, err := rockchip.ParseGPTHeader(badEnt[rockchip.SectorSize:2*rockchip.SectorSize], 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rockchip.ParseGPTEntries(hdr, badEnt[2*rockchip.SectorSize:34*rockchip.SectorSize]); !errors.Is(err, rockchip.ErrGPTChecksum) {
		t.Fatalf("expected ErrGPTChecksum on entries, got %v", err)
	}

	// 5. Overlapping partitions.
	overlapDisk := buildValidGPTDisk(2048, []gptPartSpec{
		{name: "rootfs", startLBA: 100, endLBA: 200},
		{name: "data", startLBA: 150, endLBA: 300},
	})
	hdrO, err := rockchip.ParseGPTHeader(overlapDisk[rockchip.SectorSize:2*rockchip.SectorSize], 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rockchip.ParseGPTEntries(hdrO, overlapDisk[2*rockchip.SectorSize:34*rockchip.SectorSize]); !errors.Is(err, rockchip.ErrPartitionOverlap) {
		t.Fatalf("expected ErrPartitionOverlap, got %v", err)
	}

	// 6. Out-of-bounds LastUsableLBA vs disk capacity.
	if _, err := rockchip.ParseGPTHeader(disk[rockchip.SectorSize:2*rockchip.SectorSize], 100); !errors.Is(err, rockchip.ErrInvalidGPT) {
		t.Fatalf("expected ErrInvalidGPT for out-of-bounds disk sectors, got %v", err)
	}
}

type memWriterAt struct{ buf []byte }

func (m *memWriterAt) WriteAt(p []byte, off int64) (int, error) {
	copy(m.buf[off:], p)
	return len(p), nil
}
