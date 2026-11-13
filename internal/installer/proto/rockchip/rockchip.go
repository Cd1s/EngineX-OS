// Package rockchip implements a read-only RockUSB capability, storage, and
// GPT layout query client over the shared USB bulk transport.
//
// Write paths remain disabled pending verified model recovery artifacts,
// rollback procedures, and explicit PM approval.
package rockchip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
)

const (
	// VID and PIDs associated with Rockchip loader/MaskROM states.
	VID            uint16 = 0x2207
	PIDRK3288      uint16 = 0x320A
	PIDRK3588      uint16 = 0x350B
	PIDGenericDiag uint16 = 0x0006

	CBWSignature uint32 = 0x43425355 // "USBC" little-endian
	CSWSignature uint32 = 0x53425355 // "USBS" little-endian
	CBWLength           = 31
	CSWLength           = 13

	cbwFlagDataOut uint8 = 0x00
	cbwFlagDataIn  uint8 = 0x80

	cswStatusOK         uint8 = 0x00
	cswStatusFail       uint8 = 0x01
	cswStatusPhaseError uint8 = 0x02

	// RockUSB CDB opcodes.
	OpTestUnitReady  uint8 = 0x00
	OpReadFlashID    uint8 = 0x01
	OpReadLBA        uint8 = 0x14
	OpWriteLBA       uint8 = 0x15
	OpReadFlashInfo  uint8 = 0x1A
	OpReadChipInfo   uint8 = 0xAA
	OpReadCapability uint8 = 0xAC

	defaultCommandTimeout = 5 * time.Second
	defaultDataTimeout    = 15 * time.Second
	defaultMaxReadSectors = 128 // 64 KiB per READ_LBA transfer
)

var (
	ErrWrongVendor        = errors.New("rockchip: unexpected USB vendor ID")
	ErrTagMismatch        = errors.New("rockchip: CSW tag does not match CBW tag")
	ErrBadWrapper         = errors.New("rockchip: invalid CBW/CSW wrapper")
	ErrNotLoader          = errors.New("rockchip: device is not in ready Loader mode")
	ErrTargetDisallowed   = errors.New("rockchip: target partition is not in allowlist")
	ErrTargetUnverified   = errors.New("rockchip: target was not prepared from a verified layout")
	ErrWriteNotApproved   = errors.New("rockchip: write path disabled without model recovery artifact and approval")
	ErrLayoutUnavailable  = errors.New("rockchip: storage layout unavailable")
	ErrUnsupportedStorage = errors.New("rockchip: storage geometry invalid")
)

var allowedTargetPartitions = map[string]bool{
	"rootfs":   true,
	"data":     true,
	"userdata": true,
}

// Mode identifies the runtime protocol state discovered via Probe.
type Mode int

const (
	ModeUnknown Mode = iota
	ModeMaskROM
	ModeLoader
)

func (m Mode) String() string {
	switch m {
	case ModeMaskROM:
		return "maskrom"
	case ModeLoader:
		return "loader"
	default:
		return "unknown"
	}
}

// StatusError records a non-zero CSW status returned by the Rockchip device.
type StatusError struct {
	Opcode  uint8
	Status  uint8
	Residue uint32
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("rockchip opcode 0x%02x failed: CSW status=0x%02x residue=%d", e.Opcode, e.Status, e.Residue)
}

// FlashInfo holds parsed storage geometry from READ_FLASH_INFO (0x1A).
type FlashInfo struct {
	TotalSectors uint32
	BlockSectors uint16
	PageSectors  uint8
	ECCBits      uint8
	AccessTime   uint8
	ManufCode    uint8
	FlashCS      uint8
}

// TotalBytes returns the storage capacity in bytes.
func (f FlashInfo) TotalBytes() uint64 {
	return uint64(f.TotalSectors) * SectorSize
}

// ProbeResult captures read-only RockUSB device capability and storage state.
type ProbeResult struct {
	VID          uint16
	PID          uint16
	Mode         Mode
	ChipInfo     [16]byte
	ChipTag      string
	Capability   [8]byte
	FlashID      [5]byte
	FlashInfo    FlashInfo
	StorageReady bool
}

// Layout holds a CRC-verified GPT layout read from the device.
type Layout struct {
	SectorSize   uint32
	TotalSectors uint64
	Header       GPTHeader
	Partitions   []Partition
	Verified     bool
}

// VerifiedTarget binds a validated device identity and allowlisted partition
// bounds. Callers can only construct one via PrepareVerifiedTarget.
type VerifiedTarget struct {
	VID       uint16
	PID       uint16
	ChipInfo  [16]byte
	Partition Partition
	ImageSize int64
	verified  bool
}

// Option configures a RockUSB Client.
type Option func(*Client)

func WithCommandTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.cmdTimeout = d
		}
	}
}

func WithDataTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.dataTimeout = d
		}
	}
}

func WithMaxReadSectors(n uint16) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxReadSectors = n
		}
	}
}

// Client implements read-only RockUSB queries and layout verification.
type Client struct {
	dev            proto.BulkDevice
	eps            proto.Endpoints
	cmdTimeout     time.Duration
	dataTimeout    time.Duration
	maxReadSectors uint16
	nextTag        uint32
}

// New constructs a RockUSB Client bound to an open USB bulk device.
func New(dev proto.BulkDevice, eps proto.Endpoints, opts ...Option) (*Client, error) {
	if dev == nil {
		return nil, errors.New("rockchip: nil USB device")
	}
	if err := eps.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		dev:            dev,
		eps:            eps,
		cmdTimeout:     defaultCommandTimeout,
		dataTimeout:    defaultDataTimeout,
		maxReadSectors: defaultMaxReadSectors,
		nextTag:        1,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// Probe identifies whether the attached 0x2207 device is in MaskROM or Loader
// mode and queries capability and flash geometry without performing any writes.
func (c *Client) Probe(ctx context.Context, vid, pid uint16) (ProbeResult, error) {
	if vid != VID {
		return ProbeResult{}, fmt.Errorf("%w: got 0x%04x, want 0x%04x", ErrWrongVendor, vid, VID)
	}
	res := ProbeResult{
		VID:  vid,
		PID:  pid,
		Mode: ModeUnknown,
	}

	chipRaw, err := c.execDataIn(ctx, OpReadChipInfo, 6, nil, 16)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("rockchip read chip info: %w", err)
	}
	copy(res.ChipInfo[:], chipRaw)
	res.ChipTag = formatChipTag(res.ChipInfo[:4])

	if capRaw, capErr := c.execDataIn(ctx, OpReadCapability, 6, nil, 8); capErr == nil {
		copy(res.Capability[:], capRaw)
	}

	readyErr := c.execNoData(ctx, OpTestUnitReady, 6, nil)
	if readyErr != nil {
		var se *StatusError
		if errors.As(readyErr, &se) {
			res.Mode = ModeMaskROM
			res.StorageReady = false
			return res, nil
		}
		return ProbeResult{}, readyErr
	}

	flashID, err := c.execDataIn(ctx, OpReadFlashID, 6, nil, 5)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			res.Mode = ModeMaskROM
			res.StorageReady = false
			return res, nil
		}
		return ProbeResult{}, fmt.Errorf("rockchip read flash id: %w", err)
	}
	copy(res.FlashID[:], flashID)

	flashInfoRaw, err := c.execDataIn(ctx, OpReadFlashInfo, 6, nil, 11)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			res.Mode = ModeMaskROM
			res.StorageReady = false
			return res, nil
		}
		return ProbeResult{}, fmt.Errorf("rockchip read flash info: %w", err)
	}
	res.FlashInfo = parseFlashInfo(flashInfoRaw)
	if res.FlashInfo.TotalSectors == 0 {
		res.Mode = ModeMaskROM
		res.StorageReady = false
		return res, nil
	}

	res.Mode = ModeLoader
	res.StorageReady = true
	return res, nil
}

// ReadLayout reads and CRC-validates the primary GPT partition table from a
// device probed in ready Loader mode.
func (c *Client) ReadLayout(ctx context.Context, probe ProbeResult) (Layout, error) {
	if probe.Mode != ModeLoader || !probe.StorageReady {
		return Layout{}, fmt.Errorf("%w (mode=%s, storageReady=%v)", ErrNotLoader, probe.Mode, probe.StorageReady)
	}
	if probe.FlashInfo.TotalSectors < 34 {
		return Layout{}, fmt.Errorf("%w: total sectors %d too small for GPT", ErrUnsupportedStorage, probe.FlashInfo.TotalSectors)
	}

	// Read LBA 0 (Protective MBR) and LBA 1 (Primary GPT Header).
	lba01, err := c.readLBA(ctx, 0, 2)
	if err != nil {
		return Layout{}, fmt.Errorf("rockchip read GPT header: %w", err)
	}
	hdr, err := ParseGPTHeader(lba01[SectorSize:2*SectorSize], uint64(probe.FlashInfo.TotalSectors))
	if err != nil {
		return Layout{}, err
	}

	entrySectors := hdr.EntryArraySectors()
	if entrySectors == 0 || hdr.PartitionEntryLBA+entrySectors > uint64(probe.FlashInfo.TotalSectors) {
		return Layout{}, fmt.Errorf("%w: entry array [%d..%d) exceeds disk sectors %d",
			ErrInvalidGPT, hdr.PartitionEntryLBA, hdr.PartitionEntryLBA+entrySectors, probe.FlashInfo.TotalSectors)
	}
	entryBytes, err := c.readSectorsRange(ctx, hdr.PartitionEntryLBA, entrySectors)
	if err != nil {
		return Layout{}, fmt.Errorf("rockchip read GPT entries: %w", err)
	}
	parts, err := ParseGPTEntries(hdr, entryBytes)
	if err != nil {
		return Layout{}, err
	}

	return Layout{
		SectorSize:   SectorSize,
		TotalSectors: uint64(probe.FlashInfo.TotalSectors),
		Header:       hdr,
		Partitions:   parts,
		Verified:     true,
	}, nil
}

// PrepareVerifiedTarget validates that probe is a ready Loader, layout is
// CRC-verified, partition is in the strict allowlist (rootfs, data, userdata),
// and imageSize fits inside the verified partition bounds.
func PrepareVerifiedTarget(probe ProbeResult, layout Layout, partition string, imageSize int64) (VerifiedTarget, error) {
	if probe.VID != VID {
		return VerifiedTarget{}, fmt.Errorf("%w: 0x%04x", ErrWrongVendor, probe.VID)
	}
	if probe.Mode != ModeLoader || !probe.StorageReady {
		return VerifiedTarget{}, fmt.Errorf("%w (mode=%s)", ErrNotLoader, probe.Mode)
	}
	if !layout.Verified || layout.SectorSize != SectorSize {
		return VerifiedTarget{}, fmt.Errorf("%w: layout is not verified", ErrLayoutUnavailable)
	}
	if !allowedTargetPartitions[partition] {
		return VerifiedTarget{}, fmt.Errorf("%w: %q (allowed: rootfs, data, userdata)", ErrTargetDisallowed, partition)
	}
	if imageSize <= 0 {
		return VerifiedTarget{}, fmt.Errorf("rockchip: image size must be positive, got %d", imageSize)
	}

	var matched *Partition
	for i := range layout.Partitions {
		if layout.Partitions[i].Name == partition {
			if matched != nil {
				return VerifiedTarget{}, fmt.Errorf("%w: duplicate partition %q", ErrInvalidGPT, partition)
			}
			p := layout.Partitions[i]
			matched = &p
		}
	}
	if matched == nil {
		return VerifiedTarget{}, fmt.Errorf("%w: partition %q not found in GPT", ErrLayoutUnavailable, partition)
	}
	if uint64(imageSize) > matched.ByteSize() {
		return VerifiedTarget{}, fmt.Errorf("rockchip: image (%d bytes) exceeds partition %q (%d bytes)",
			imageSize, partition, matched.ByteSize())
	}

	return VerifiedTarget{
		VID:       probe.VID,
		PID:       probe.PID,
		ChipInfo:  probe.ChipInfo,
		Partition: *matched,
		ImageSize: imageSize,
		verified:  true,
	}, nil
}

// Readback reads length bytes from the verified target partition via read-only
// READ_LBA transfers.
func (c *Client) Readback(ctx context.Context, target VerifiedTarget, dst io.WriterAt, length int64, progress proto.ProgressFunc) error {
	if !target.verified {
		return ErrTargetUnverified
	}
	if dst == nil {
		return errors.New("rockchip: nil readback destination")
	}
	if length <= 0 || uint64(length) > target.Partition.ByteSize() {
		return fmt.Errorf("rockchip: readback length %d outside partition %q bounds (%d bytes)",
			length, target.Partition.Name, target.Partition.ByteSize())
	}

	totalSectors := uint64((length + SectorSize - 1) / SectorSize)
	var doneSectors uint64
	var written int64
	proto.EmitProgress(progress, "readback", 0, length)

	for doneSectors < totalSectors {
		step := uint64(c.maxReadSectors)
		if rem := totalSectors - doneSectors; rem < step {
			step = rem
		}
		lba := target.Partition.StartLBA + doneSectors
		if lba > uint64(^uint32(0)) {
			return fmt.Errorf("rockchip: LBA %d overflows 32-bit RockUSB READ_LBA", lba)
		}
		secBytes, err := c.readLBA(ctx, uint32(lba), uint16(step))
		if err != nil {
			return err
		}
		toWrite := int64(len(secBytes))
		if remBytes := length - written; remBytes < toWrite {
			toWrite = remBytes
		}
		if err := writeAtExact(dst, written, secBytes[:toWrite]); err != nil {
			return err
		}
		written += toWrite
		doneSectors += step
		proto.EmitProgress(progress, "readback", written, length)
	}
	return nil
}

// WriteVerifiedTarget is intentionally disabled in the read-only phase. It
// validates target invariants and fails closed with ErrWriteNotApproved without
// sending any WRITE_LBA command to the device.
func (c *Client) WriteVerifiedTarget(ctx context.Context, target VerifiedTarget, img proto.Image, _ proto.ProgressFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !target.verified {
		return ErrTargetUnverified
	}
	if !allowedTargetPartitions[target.Partition.Name] {
		return fmt.Errorf("%w: %q", ErrTargetDisallowed, target.Partition.Name)
	}
	if err := img.Validate(); err != nil {
		return err
	}
	if uint64(img.Size) > target.Partition.ByteSize() {
		return fmt.Errorf("rockchip: image (%d bytes) exceeds partition %q (%d bytes)",
			img.Size, target.Partition.Name, target.Partition.ByteSize())
	}
	return ErrWriteNotApproved
}

func (c *Client) readSectorsRange(ctx context.Context, startLBA, count uint64) ([]byte, error) {
	if startLBA+count > uint64(^uint32(0)) {
		return nil, fmt.Errorf("rockchip: LBA range [%d..%d) overflows 32-bit RockUSB", startLBA, startLBA+count)
	}
	out := make([]byte, 0, int(count)*SectorSize)
	var done uint64
	for done < count {
		step := uint64(c.maxReadSectors)
		if rem := count - done; rem < step {
			step = rem
		}
		chunk, err := c.readLBA(ctx, uint32(startLBA+done), uint16(step))
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		done += step
	}
	return out, nil
}

func (c *Client) readLBA(ctx context.Context, lba uint32, sectors uint16) ([]byte, error) {
	if sectors == 0 {
		return nil, errors.New("rockchip: zero sector read")
	}
	var cdbArgs [9]byte
	// Standard 10-byte CDB layout: byte 0 = opcode, byte 1 = subcode (0),
	// bytes 2..5 = big-endian LBA, byte 6 = 0, bytes 7..8 = big-endian sector count.
	binary.BigEndian.PutUint32(cdbArgs[1:5], lba)
	binary.BigEndian.PutUint16(cdbArgs[6:8], sectors)
	return c.execDataIn(ctx, OpReadLBA, 10, cdbArgs[:], uint32(sectors)*SectorSize)
}

func (c *Client) execNoData(ctx context.Context, opcode, cbLen uint8, cdbArgs []byte) error {
	tag := c.allocTag()
	cbw, err := EncodeCBW(tag, 0, cbwFlagDataOut, opcode, cbLen, cdbArgs)
	if err != nil {
		return err
	}
	if err := c.writeExact(ctx, cbw[:], c.cmdTimeout); err != nil {
		return err
	}
	csw, err := c.readCSW(ctx, tag, 0)
	if err != nil {
		return err
	}
	if csw.Status != cswStatusOK {
		return &StatusError{Opcode: opcode, Status: csw.Status, Residue: csw.Residue}
	}
	return nil
}

func (c *Client) execDataIn(ctx context.Context, opcode, cbLen uint8, cdbArgs []byte, wantLen uint32) ([]byte, error) {
	tag := c.allocTag()
	cbw, err := EncodeCBW(tag, wantLen, cbwFlagDataIn, opcode, cbLen, cdbArgs)
	if err != nil {
		return nil, err
	}
	if err := c.writeExact(ctx, cbw[:], c.cmdTimeout); err != nil {
		return nil, err
	}
	data := make([]byte, wantLen)
	got := 0
	for got < int(wantLen) {
		tctx, cancel := context.WithTimeout(ctx, c.dataTimeout)
		n, rerr := c.dev.ReadBulk(tctx, c.eps.In, data[got:])
		cancel()
		got += n
		if rerr != nil {
			return nil, fmt.Errorf("rockchip opcode 0x%02x data-in: %w", opcode, rerr)
		}
		if n == 0 {
			return nil, fmt.Errorf("rockchip opcode 0x%02x data-in: empty USB bulk read", opcode)
		}
	}
	csw, err := c.readCSW(ctx, tag, wantLen)
	if err != nil {
		return nil, err
	}
	if csw.Status != cswStatusOK {
		return nil, &StatusError{Opcode: opcode, Status: csw.Status, Residue: csw.Residue}
	}
	if csw.Residue != 0 {
		return nil, fmt.Errorf("rockchip opcode 0x%02x: non-zero CSW residue %d", opcode, csw.Residue)
	}
	return data, nil
}

func (c *Client) readCSW(ctx context.Context, expectedTag, maxTransfer uint32) (CSW, error) {
	if err := ctx.Err(); err != nil {
		return CSW{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, c.cmdTimeout)
	defer cancel()
	var raw [64]byte
	n, err := c.dev.ReadBulk(tctx, c.eps.In, raw[:])
	if err != nil {
		return CSW{}, err
	}
	return ParseCSW(raw[:n], expectedTag, maxTransfer)
}

func (c *Client) writeExact(ctx context.Context, data []byte, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	n, err := c.dev.WriteBulk(tctx, c.eps.Out, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("rockchip: short USB bulk write (%d of %d bytes)", n, len(data))
	}
	return nil
}

func (c *Client) allocTag() uint32 {
	t := c.nextTag
	if t == 0 {
		t = 1
	}
	c.nextTag = t + 1
	if c.nextTag == 0 {
		c.nextTag = 1
	}
	return t
}

// CSW represents a parsed 13-byte Command Status Wrapper.
type CSW struct {
	Tag     uint32
	Residue uint32
	Status  uint8
}

// EncodeCBW builds a 31-byte RockUSB Command Block Wrapper.
func EncodeCBW(tag, transferLen uint32, flags, opcode, cbLen uint8, cdbArgs []byte) ([CBWLength]byte, error) {
	var buf [CBWLength]byte
	if tag == 0 {
		return buf, fmt.Errorf("%w: CBW tag must be non-zero", ErrBadWrapper)
	}
	if cbLen == 0 || cbLen > 16 {
		return buf, fmt.Errorf("%w: invalid CDB length %d", ErrBadWrapper, cbLen)
	}
	if len(cdbArgs) > int(cbLen)-1 {
		return buf, fmt.Errorf("%w: CDB args (%d) exceed cbLen-1 (%d)", ErrBadWrapper, len(cdbArgs), cbLen-1)
	}
	binary.LittleEndian.PutUint32(buf[0:4], CBWSignature)
	binary.LittleEndian.PutUint32(buf[4:8], tag)
	binary.LittleEndian.PutUint32(buf[8:12], transferLen)
	buf[12] = flags
	buf[13] = 0 // LUN 0
	buf[14] = cbLen
	buf[15] = opcode
	copy(buf[16:], cdbArgs)
	return buf, nil
}

// ParseCSW validates a 13-byte RockUSB Command Status Wrapper against the
// expected CBW tag and transfer length.
func ParseCSW(raw []byte, expectedTag, maxTransfer uint32) (CSW, error) {
	if len(raw) != CSWLength {
		return CSW{}, fmt.Errorf("%w: CSW length %d != %d", ErrBadWrapper, len(raw), CSWLength)
	}
	if sig := binary.LittleEndian.Uint32(raw[0:4]); sig != CSWSignature {
		return CSW{}, fmt.Errorf("%w: bad CSW signature 0x%08x", ErrBadWrapper, sig)
	}
	csw := CSW{
		Tag:     binary.LittleEndian.Uint32(raw[4:8]),
		Residue: binary.LittleEndian.Uint32(raw[8:12]),
		Status:  raw[12],
	}
	if csw.Tag != expectedTag {
		return CSW{}, fmt.Errorf("%w: got 0x%08x, want 0x%08x", ErrTagMismatch, csw.Tag, expectedTag)
	}
	if csw.Status > cswStatusPhaseError {
		return CSW{}, fmt.Errorf("%w: invalid CSW status 0x%02x", ErrBadWrapper, csw.Status)
	}
	if csw.Residue > maxTransfer {
		return CSW{}, fmt.Errorf("%w: CSW residue %d exceeds transfer length %d", ErrBadWrapper, csw.Residue, maxTransfer)
	}
	return csw, nil
}

func parseFlashInfo(raw []byte) FlashInfo {
	return FlashInfo{
		TotalSectors: binary.LittleEndian.Uint32(raw[0:4]),
		BlockSectors: binary.LittleEndian.Uint16(raw[4:6]),
		PageSectors:  raw[6],
		ECCBits:      raw[7],
		AccessTime:   raw[8],
		ManufCode:    raw[9],
		FlashCS:      raw[10],
	}
}

func formatChipTag(raw []byte) string {
	// Rockchip READ_CHIP_INFO places a 4-byte chip tag in little-endian order
	// (e.g. "8823" for RK3288 or "8853" for RK3588). Reverse printable bytes.
	rev := [4]byte{raw[3], raw[2], raw[1], raw[0]}
	for _, b := range rev {
		if b < 0x20 || b > 0x7e {
			return fmt.Sprintf("0x%08x", binary.LittleEndian.Uint32(raw))
		}
	}
	return strings.TrimSpace(string(rev[:]))
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
