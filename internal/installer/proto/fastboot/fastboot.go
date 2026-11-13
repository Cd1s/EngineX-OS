// Package fastboot implements the Android Fastboot USB bulk protocol client
// without shelling out to an external fastboot binary.
package fastboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/sparse"
)

const (
	// Gen1OEMUnlockCommand is the bootloader OEM unlock command used by Gen-1 decks.
	Gen1OEMUnlockCommand = "oem inmusic-unlock-magic-7de5fbc22b8c524e"

	defaultCommandTimeout = 10 * time.Second
	defaultDataTimeout    = 30 * time.Second
	defaultFlashTimeout   = 10 * time.Minute
	defaultMaxCommandLen  = 64
	defaultChunkSize      = 1 << 20 // 1 MiB
	defaultMaxInfoPackets = 1024
	responseReadSize      = 512
	maxFastbootSingleSize = int64(0xFFFFFFFF)
)

var (
	ErrUnsupportedVar   = errors.New("fastboot: variable not supported")
	ErrProductMismatch  = errors.New("fastboot: device product does not match expected board")
	ErrSparseRequired   = errors.New("fastboot: sparse support required")
	ErrInvalidPartition = errors.New("fastboot: invalid partition name")
	ErrSessionDirty     = errors.New("fastboot: USB protocol session desynchronized after aborted data transfer")
)

// ProtocolError represents a FAIL or malformed response returned by the device.
type ProtocolError struct {
	Command string
	Message string
	Kind    error
}

func (e *ProtocolError) Error() string {
	if e.Command == "" {
		return fmt.Sprintf("fastboot error: %s", e.Message)
	}
	return fmt.Sprintf("fastboot %q failed: %s", e.Command, e.Message)
}

func (e *ProtocolError) Unwrap() error {
	return e.Kind
}

// Option configures a fastboot Client.
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

func WithFlashTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.flashTimeout = d
		}
	}
}

func WithMaxCommandLength(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxCmdLen = n
		}
	}
}

// WithChunkSize sets the download chunk size. New rejects values above the
// native USB transfer limit before any protocol I/O.
func WithChunkSize(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.chunkSize = n
		}
	}
}

func WithMaxInfoPackets(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxInfoPackets = n
		}
	}
}

func WithOnInfo(fn func(string)) Option {
	return func(c *Client) {
		c.onInfo = fn
	}
}

// Client speaks the Android Fastboot protocol over a claimed USB bulk interface.
type Client struct {
	dev            proto.BulkDevice
	eps            proto.Endpoints
	cmdTimeout     time.Duration
	dataTimeout    time.Duration
	flashTimeout   time.Duration
	maxCmdLen      int
	chunkSize      int
	maxInfoPackets int
	onInfo         func(string)
	dirty          bool
}

// New constructs a fastboot Client bound to an open USB device and endpoint pair.
func New(dev proto.BulkDevice, eps proto.Endpoints, opts ...Option) (*Client, error) {
	if dev == nil {
		return nil, errors.New("fastboot: nil USB device")
	}
	if err := eps.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		dev:            dev,
		eps:            eps,
		cmdTimeout:     defaultCommandTimeout,
		dataTimeout:    defaultDataTimeout,
		flashTimeout:   defaultFlashTimeout,
		maxCmdLen:      defaultMaxCommandLen,
		chunkSize:      defaultChunkSize,
		maxInfoPackets: defaultMaxInfoPackets,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.chunkSize > proto.MaxBulkTransferSize {
		return nil, fmt.Errorf("fastboot: chunk size %d exceeds USB transfer limit %d", c.chunkSize, proto.MaxBulkTransferSize)
	}
	if c.maxCmdLen > proto.MaxBulkTransferSize {
		return nil, fmt.Errorf("fastboot: command limit %d exceeds USB transfer limit %d", c.maxCmdLen, proto.MaxBulkTransferSize)
	}
	return c, nil
}

// GetVar queries a fastboot bootloader variable via "getvar:<name>".
func (c *Client) GetVar(ctx context.Context, name string) (string, error) {
	if err := validateToken("variable name", name); err != nil {
		return "", err
	}
	return c.simpleCommand(ctx, "getvar:"+name, c.cmdTimeout, false)
}

// GetMaxDownloadSize queries and parses "max-download-size".
func (c *Client) GetMaxDownloadSize(ctx context.Context) (int64, error) {
	raw, err := c.GetVar(ctx, "max-download-size")
	if err != nil {
		return 0, err
	}
	n, err := ParseSizeValue(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("fastboot: invalid max-download-size %q", raw)
	}
	return n, nil
}

// GetPartitionSize queries and parses "partition-size:<partition>".
func (c *Client) GetPartitionSize(ctx context.Context, partition string) (int64, error) {
	if err := validatePartition(partition); err != nil {
		return 0, err
	}
	raw, err := c.GetVar(ctx, "partition-size:"+partition)
	if err != nil {
		return 0, err
	}
	n, err := ParseSizeValue(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("fastboot: invalid partition-size:%s %q", partition, raw)
	}
	return n, nil
}

// GetPartitionType queries "partition-type:<partition>".
func (c *Client) GetPartitionType(ctx context.Context, partition string) (string, error) {
	if err := validatePartition(partition); err != nil {
		return "", err
	}
	raw, err := c.GetVar(ctx, "partition-type:"+partition)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// VerifyProduct checks "getvar:product" against expectedBoard on the active USB
// session. When allowMissingUSBBoard is non-empty and the bootloader reports
// "product" as unsupported (as on Gen-1 SC5000), it permits the match only if
// expectedBoard is a known Gen-1 board and allowMissingUSBBoard normalizes to it.
func (c *Client) VerifyProduct(ctx context.Context, expectedBoard, allowMissingUSBBoard string) (string, error) {
	wantBoard, ok := proto.LookupBoard(expectedBoard)
	if !ok {
		return "", fmt.Errorf("%w: unknown expected board %q", ErrProductMismatch, expectedBoard)
	}
	raw, err := c.GetVar(ctx, "product")
	if err != nil {
		if errors.Is(err, ErrUnsupportedVar) && allowMissingUSBBoard != "" {
			if wantBoard.Gen2 {
				return "", fmt.Errorf("%w: missing product fallback is only valid for Gen-1 boards, not %s", ErrProductMismatch, wantBoard.Code)
			}
			if gotFallback := proto.BoardCode(allowMissingUSBBoard); gotFallback == wantBoard.Code {
				return gotFallback, nil
			}
			return "", fmt.Errorf("%w: USB fallback %q != expected %q", ErrProductMismatch, allowMissingUSBBoard, expectedBoard)
		}
		return "", err
	}
	got := proto.BoardCode(raw)
	if got != wantBoard.Code {
		return "", fmt.Errorf("%w: got %q (%s), want %q", ErrProductMismatch, raw, got, expectedBoard)
	}
	return got, nil
}

// UnlockGen1 sends the Gen-1 OEM unlock command.
func (c *Client) UnlockGen1(ctx context.Context) error {
	_, err := c.simpleCommand(ctx, Gen1OEMUnlockCommand, c.cmdTimeout, true)
	return err
}

// Reboot sends the fastboot reboot command.
func (c *Client) Reboot(ctx context.Context) error {
	_, err := c.simpleCommand(ctx, "reboot", c.cmdTimeout, false)
	return err
}

// Download sends "download:<hexsize>" and streams exact size bytes from r to
// the device, validating both the DATA acknowledgement and final OKAY reply.
func (c *Client) Download(ctx context.Context, r io.Reader, size int64, progress proto.ProgressFunc) error {
	if c.dirty {
		return ErrSessionDirty
	}
	if r == nil {
		return errors.New("fastboot: nil download reader")
	}
	if size <= 0 || size > maxFastbootSingleSize {
		return fmt.Errorf("fastboot: download size %d out of protocol range [1..%d]", size, maxFastbootSingleSize)
	}
	cmd := fmt.Sprintf("download:%08x", uint32(size))
	if err := c.sendCommand(ctx, cmd, false); err != nil {
		return err
	}
	pktType, payload, err := c.readReplyUntil(ctx, cmd, c.cmdTimeout, false, true)
	if err != nil {
		return err
	}
	if pktType != "DATA" {
		return fmt.Errorf("fastboot %q: expected DATA response, got %s%s", cmd, pktType, payload)
	}
	offered, err := parseDataSize(payload)
	if err != nil {
		return fmt.Errorf("fastboot %q: %w", cmd, err)
	}
	if int64(offered) != size {
		return fmt.Errorf("fastboot %q: device requested %d bytes, host offered %d", cmd, offered, size)
	}

	// Device is now in the DATA phase; any premature exit leaves the USB
	// session out of sync until reopened.
	buf := make([]byte, minInt(c.chunkSize, int(minInt64(size, int64(c.chunkSize)))))
	var sent int64
	proto.EmitProgress(progress, "download", 0, size)
	for sent < size {
		toRead := len(buf)
		if rem := size - sent; rem < int64(toRead) {
			toRead = int(rem)
		}
		n, rerr := io.ReadFull(r, buf[:toRead])
		if rerr != nil {
			c.dirty = true
			if sent > 0 {
				return proto.MarkOutcomeUnknown("fastboot download read", rerr)
			}
			return fmt.Errorf("fastboot download read: %w", rerr)
		}
		if err := c.writeBulkExact(ctx, buf[:n], c.dataTimeout); err != nil {
			c.dirty = true
			return proto.MarkOutcomeUnknown("fastboot download write", err)
		}
		sent += int64(n)
		proto.EmitProgress(progress, "download", sent, size)
	}

	pktType, payload, err = c.readReplyUntil(ctx, cmd, c.cmdTimeout, true, false)
	if err != nil {
		var pe *ProtocolError
		if errors.As(err, &pe) {
			return err
		}
		return proto.MarkOutcomeUnknown("fastboot download final ack", err)
	}
	if pktType != "OKAY" {
		return fmt.Errorf("fastboot %q: expected OKAY after data, got %s%s", cmd, pktType, payload)
	}
	return nil
}

// FlashPartition issues "flash:<partition>" for the previously downloaded payload.
func (c *Client) FlashPartition(ctx context.Context, partition string) error {
	if err := validatePartition(partition); err != nil {
		return err
	}
	_, err := c.simpleCommand(ctx, "flash:"+partition, c.flashTimeout, true)
	return err
}

// FlashOptions configures high-level partition flashing, including product
// checks, max-download-size discovery, and Android sparse splitting.
type FlashOptions struct {
	// ExpectedBoard, when non-empty, validates getvar:product on this session.
	ExpectedBoard string

	// AllowMissingProductBoard optionally supplies an already-verified USB
	// VID/PID board match for Gen-1 bootloaders that do not implement getvar:product.
	AllowMissingProductBoard string

	// AllowUnverifiedProduct explicitly opts out of ExpectedBoard verification.
	AllowUnverifiedProduct bool

	// MaxDownloadSize overrides querying getvar:max-download-size when > 0.
	// If <= 0, Client queries getvar:max-download-size and refuses to guess if missing.
	MaxDownloadSize int64

	// AllowSparse permits encoding/resparsing into Android sparse pieces when
	// the image exceeds MaxDownloadSize or is already sparse.
	AllowSparse bool

	// ForceSparse forces sparse encoding even if the raw image fits in a single download.
	ForceSparse bool

	// SparseBlockSize sets the block size when sparsing raw images (default 4096).
	SparseBlockSize uint32

	// PadLastBlock zero-pads a trailing partial block on non-block-aligned raw images.
	PadLastBlock bool

	// IncludeSparseCRC appends CHUNK_TYPE_CRC32 footers to generated sparse pieces.
	IncludeSparseCRC bool
}

// Flash verifies optional product identity, plans raw or bounded sparse pieces
// respecting the device's max-download-size, and downloads/flashes each piece.
func (c *Client) Flash(ctx context.Context, partition string, img proto.Image, opts FlashOptions, progress proto.ProgressFunc) error {
	if err := validatePartition(partition); err != nil {
		return err
	}
	if err := img.Validate(); err != nil {
		return err
	}
	if opts.ExpectedBoard != "" {
		if _, err := c.VerifyProduct(ctx, opts.ExpectedBoard, opts.AllowMissingProductBoard); err != nil {
			return err
		}
	} else if !opts.AllowUnverifiedProduct {
		return errors.New("fastboot: ExpectedBoard is required unless AllowUnverifiedProduct is set")
	}

	maxDown := opts.MaxDownloadSize
	if maxDown <= 0 {
		var err error
		maxDown, err = c.GetMaxDownloadSize(ctx)
		if err != nil {
			return fmt.Errorf("fastboot flash %s: cannot determine max-download-size (refusing to guess): %w", partition, err)
		}
	}
	if maxDown > maxFastbootSingleSize {
		maxDown = maxFastbootSingleSize
	}

	isSp, err := sparse.IsSparse(img.Data, img.Size)
	if err != nil {
		return err
	}

	if !isSp && img.Size <= maxDown && !opts.ForceSparse {
		r := io.NewSectionReader(img.Data, 0, img.Size)
		if err := c.Download(ctx, r, img.Size, progress); err != nil {
			return err
		}
		proto.EmitProgress(progress, "flash", 0, img.Size)
		if err := c.FlashPartition(ctx, partition); err != nil {
			return err
		}
		proto.EmitProgress(progress, "flash", img.Size, img.Size)
		return nil
	}

	if !opts.AllowSparse {
		if isSp {
			return fmt.Errorf("%w: input is already sparse and AllowSparse is false", ErrSparseRequired)
		}
		return fmt.Errorf("%w: image %d bytes > max-download-size %d bytes", ErrSparseRequired, img.Size, maxDown)
	}

	iter, err := sparse.NewResparse(img.Data, img.Size, sparse.ResparseOptions{
		MaxPieceBytes: maxDown,
		BlockSize:     opts.SparseBlockSize,
		PadLastBlock:  opts.PadLastBlock,
		IncludeCRC:    opts.IncludeSparseCRC,
	})
	if err != nil {
		return fmt.Errorf("fastboot flash %s: resparse: %w", partition, err)
	}
	if iter.Count() == 0 {
		return fmt.Errorf("fastboot flash %s: resparse produced 0 pieces", partition)
	}

	var totalEncoded int64
	var pieces []*sparse.Piece
	for {
		p, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("fastboot flash %s: resparse piece: %w", partition, err)
		}
		pieces = append(pieces, p)
		totalEncoded += p.EncodedSize
	}

	var completed int64
	for _, p := range pieces {
		pieceReader := p.Open()
		dlErr := c.Download(ctx, pieceReader, p.EncodedSize, func(pr proto.Progress) {
			proto.EmitProgress(progress, "download", completed+pr.Sent, totalEncoded)
		})
		_ = pieceReader.Close()
		if dlErr != nil {
			if p.Index > 0 {
				return proto.MarkOutcomeUnknown(fmt.Sprintf("fastboot sparse piece %d/%d download", p.Index+1, p.TotalPieces), dlErr)
			}
			return dlErr
		}
		completed += p.EncodedSize
		if err := c.FlashPartition(ctx, partition); err != nil {
			return proto.MarkOutcomeUnknown(fmt.Sprintf("fastboot sparse piece %d/%d flash", p.Index+1, p.TotalPieces), err)
		}
		proto.EmitProgress(progress, "flash", completed, totalEncoded)
	}
	return nil
}

func (c *Client) simpleCommand(ctx context.Context, cmd string, timeout time.Duration, mutating bool) (string, error) {
	if c.dirty {
		return "", ErrSessionDirty
	}
	if err := c.sendCommand(ctx, cmd, mutating); err != nil {
		return "", err
	}
	pktType, payload, err := c.readReplyUntil(ctx, cmd, timeout, mutating, false)
	if err != nil {
		var pe *ProtocolError
		if mutating && !errors.As(err, &pe) {
			c.dirty = true
			return "", proto.MarkOutcomeUnknown("fastboot "+cmd, err)
		}
		return "", err
	}
	if pktType != "OKAY" {
		return "", fmt.Errorf("fastboot %q: unexpected response %s%s", cmd, pktType, payload)
	}
	return payload, nil
}

func (c *Client) sendCommand(ctx context.Context, cmd string, mutating bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd == "" || len(cmd) > c.maxCmdLen {
		return fmt.Errorf("fastboot: command length %d out of range [1..%d]", len(cmd), c.maxCmdLen)
	}
	if err := c.writeBulkExact(ctx, []byte(cmd), c.cmdTimeout); err != nil {
		if mutating {
			return proto.MarkOutcomeUnknown("fastboot "+cmd+" write", err)
		}
		return err
	}
	return nil
}

func (c *Client) writeBulkExact(ctx context.Context, data []byte, timeout time.Duration) error {
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
		return fmt.Errorf("fastboot: short USB bulk write (%d of %d bytes)", n, len(data))
	}
	return nil
}

func (c *Client) readReplyUntil(ctx context.Context, cmd string, timeout time.Duration, mutating, expectData bool) (string, string, error) {
	buf := make([]byte, responseReadSize)
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for infoCount := 0; infoCount <= c.maxInfoPackets; {
		if err := tctx.Err(); err != nil {
			if mutating {
				return "", "", proto.MarkOutcomeUnknown("fastboot "+cmd, err)
			}
			return "", "", err
		}
		n, err := c.dev.ReadBulk(tctx, c.eps.In, buf)
		if err != nil {
			if mutating {
				return "", "", proto.MarkOutcomeUnknown("fastboot "+cmd, err)
			}
			return "", "", err
		}
		if n < 0 || n > len(buf) {
			return "", "", fmt.Errorf("fastboot %q: invalid ReadBulk count %d", cmd, n)
		}
		if n < 4 {
			return "", "", fmt.Errorf("fastboot %q: truncated reply (%d bytes)", cmd, n)
		}
		prefix := string(buf[:4])
		body := string(buf[4:n])
		switch prefix {
		case "INFO":
			infoCount++
			if infoCount > c.maxInfoPackets {
				return "", "", fmt.Errorf("fastboot %q: exceeded max INFO packet limit (%d)", cmd, c.maxInfoPackets)
			}
			if c.onInfo != nil {
				c.onInfo(body)
			}
		case "OKAY":
			if expectData {
				return "", "", fmt.Errorf("fastboot %q: expected DATA, got OKAY%s", cmd, body)
			}
			return "OKAY", body, nil
		case "DATA":
			if !expectData {
				return "", "", fmt.Errorf("fastboot %q: unexpected DATA%s", cmd, body)
			}
			return "DATA", body, nil
		case "FAIL":
			return "", "", classifyFail(cmd, body)
		default:
			return "", "", fmt.Errorf("fastboot %q: unknown reply prefix %q", cmd, prefix)
		}
	}
	return "", "", fmt.Errorf("fastboot %q: exceeded INFO limit", cmd)
}

func classifyFail(cmd, msg string) error {
	lower := strings.ToLower(msg)
	var kind error
	if strings.Contains(lower, "variable not implemented") ||
		strings.Contains(lower, "unknown variable") ||
		strings.Contains(lower, "not supported") {
		kind = ErrUnsupportedVar
	}
	return &ProtocolError{Command: cmd, Message: msg, Kind: kind}
}

func parseDataSize(hexStr string) (uint32, error) {
	if len(hexStr) != 8 {
		return 0, fmt.Errorf("invalid DATA size %q: want 8 hex digits", hexStr)
	}
	for i := 0; i < len(hexStr); i++ {
		ch := hexStr[i]
		isHex := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !isHex {
			return 0, fmt.Errorf("invalid DATA hex size %q", hexStr)
		}
	}
	v, err := strconv.ParseUint(hexStr, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid DATA size %q: %w", hexStr, err)
	}
	return uint32(v), nil
}

// ParseSizeValue parses a fastboot size string (either "0x..." hex or unsigned
// decimal). Non-prefixed hex strings containing letters are rejected to catch
// malformed replies.
func ParseSizeValue(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("empty size")
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		digits := s[2:]
		if digits == "" {
			return 0, fmt.Errorf("empty hex size %q", raw)
		}
		v, err := strconv.ParseUint(digits, 16, 63)
		if err != nil {
			return 0, err
		}
		return int64(v), nil
	}
	v, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}

func validatePartition(name string) error {
	if err := validateToken("partition", name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPartition, err)
	}
	if strings.ContainsRune(name, ':') {
		return fmt.Errorf("%w: %q contains ':'", ErrInvalidPartition, name)
	}
	return nil
}

func validateToken(kind, s string) error {
	if s == "" {
		return fmt.Errorf("fastboot: empty %s", kind)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return fmt.Errorf("fastboot: invalid character in %s %q", kind, s)
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
