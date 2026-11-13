// Package azupdate implements the AZ0O/AZ0I USB update protocol used by
// Gen-2 AZ04 decks in Firmware Update Mode (1cc5:2027, az0x-splash usbupdate).
package azupdate

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
)

const (
	// VID and PID are the USB identifiers exposed by az0x-splash usbupdate.
	VID uint16 = 0x1CC5
	PID uint16 = 0x2027

	RequestMagic = "AZ0O"
	ReplyMagic   = "AZ0I"

	// CmdWritePart is the write-part command code (0x5AA5FB04).
	CmdWritePart uint32 = 0x5AA5FB04

	StatusWritten uint16 = 1
	StatusNoEntry uint16 = 3
	StatusReady   uint16 = 5

	HeaderSize       = 40
	ReplySize        = 24
	DefaultChunkSize = 256 * 1024

	defaultCommandTimeout = 5 * time.Second
	defaultDataTimeout    = 10 * time.Second
	defaultZLPTimeout     = 30 * time.Second
	defaultStatusTimeout  = 300 * time.Second
	replyReadSize         = 512
)

var (
	ErrNoPartition = errors.New("azupdate: device has none of the candidate partitions")
	ErrRefused     = errors.New("azupdate: device refused write-part")
)

// RefusedError describes an explicit non-ready or non-written status from the deck.
type RefusedError struct {
	Partition string
	Status    uint16
	Phase     string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("azupdate %s partition %q: unexpected status %d", e.Phase, e.Partition, e.Status)
}

func (e *RefusedError) Unwrap() error {
	return ErrRefused
}

// Reply represents a parsed 24-byte AZ0I status packet.
type Reply struct {
	Command   uint32
	Param     uint64
	Seq       uint32
	Status    uint16
	PacketLen uint16
}

// EncodeWritePartHeader builds a 40+N+1 byte little-endian AZ0O write-part request.
func EncodeWritePartHeader(partition string, total uint64, seq uint32) ([]byte, error) {
	if partition == "" || strings.ContainsRune(partition, '\x00') {
		return nil, fmt.Errorf("azupdate: invalid partition name %q", partition)
	}
	pktLen := HeaderSize + len(partition) + 1
	if pktLen > 0xFFFF {
		return nil, fmt.Errorf("azupdate: partition name too long (%d bytes)", len(partition))
	}
	buf := make([]byte, pktLen)
	copy(buf[0:4], RequestMagic)
	binary.LittleEndian.PutUint32(buf[4:8], CmdWritePart)
	binary.LittleEndian.PutUint64(buf[8:16], total)
	binary.LittleEndian.PutUint32(buf[16:20], seq)
	binary.LittleEndian.PutUint16(buf[20:22], 0)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(pktLen))
	binary.LittleEndian.PutUint64(buf[24:32], 0) // part offset 0
	binary.LittleEndian.PutUint64(buf[32:40], 1) // flags: write if changed
	copy(buf[40:], partition)
	buf[pktLen-1] = 0
	return buf, nil
}

// ParseReply validates and decodes an AZ0I status packet, checking magic,
// command, sequence number, and packet length.
func ParseReply(rx []byte, expectedCmd, expectedSeq uint32) (Reply, error) {
	if len(rx) < ReplySize {
		return Reply{}, fmt.Errorf("azupdate: short reply (%d bytes, want >= %d)", len(rx), ReplySize)
	}
	if string(rx[0:4]) != ReplyMagic {
		return Reply{}, fmt.Errorf("azupdate: unexpected reply magic %x", rx[0:4])
	}
	r := Reply{
		Command:   binary.LittleEndian.Uint32(rx[4:8]),
		Param:     binary.LittleEndian.Uint64(rx[8:16]),
		Seq:       binary.LittleEndian.Uint32(rx[16:20]),
		Status:    binary.LittleEndian.Uint16(rx[20:22]),
		PacketLen: binary.LittleEndian.Uint16(rx[22:24]),
	}
	if r.Command != expectedCmd {
		return Reply{}, fmt.Errorf("azupdate: reply command 0x%08x != expected 0x%08x", r.Command, expectedCmd)
	}
	if r.PacketLen < ReplySize || int(r.PacketLen) > len(rx) {
		return Reply{}, fmt.Errorf("azupdate: invalid reply packet_len %d for %d-byte read", r.PacketLen, len(rx))
	}
	if r.Seq != expectedSeq {
		return Reply{}, fmt.Errorf("azupdate: reply seq %d != expected %d", r.Seq, expectedSeq)
	}
	return r, nil
}

// Option configures an azupdate Client.
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

func WithZLPTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.zlpTimeout = d
		}
	}
}

func WithStatusTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.statusTimeout = d
		}
	}
}

func WithChunkSize(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.chunkSize = n
		}
	}
}

func WithPartitions(parts []string) Option {
	return func(c *Client) {
		var allowed []string
		for _, p := range parts {
			if p == "data" || p == "userdata" {
				allowed = append(allowed, p)
			}
		}
		if len(allowed) > 0 {
			c.partitions = allowed
		}
	}
}

func WithInitialSeq(seq uint32) Option {
	return func(c *Client) {
		c.initialSeq = seq
	}
}

// Client speaks the AZ0O/AZ0I USB update protocol over a claimed bulk interface.
type Client struct {
	dev           proto.BulkDevice
	eps           proto.Endpoints
	cmdTimeout    time.Duration
	dataTimeout   time.Duration
	zlpTimeout    time.Duration
	statusTimeout time.Duration
	chunkSize     int
	partitions    []string
	initialSeq    uint32
}

// New constructs an azupdate Client bound to an open USB device and endpoint pair.
func New(dev proto.BulkDevice, eps proto.Endpoints, opts ...Option) (*Client, error) {
	if dev == nil {
		return nil, errors.New("azupdate: nil USB device")
	}
	if err := eps.Validate(); err != nil {
		return nil, err
	}
	c := &Client{
		dev:           dev,
		eps:           eps,
		cmdTimeout:    defaultCommandTimeout,
		dataTimeout:   defaultDataTimeout,
		zlpTimeout:    defaultZLPTimeout,
		statusTimeout: defaultStatusTimeout,
		chunkSize:     DefaultChunkSize,
		partitions:    []string{"data", "userdata"},
		initialSeq:    1,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// WriteData executes the AZ0O write-part state machine, probing candidate
// partitions ("data", then "userdata" only on StatusNoEntry), streaming raw
// chunks, sending an explicit zero-length packet, and requiring final
// StatusWritten acknowledgement.
func (c *Client) WriteData(ctx context.Context, img proto.Image, progress proto.ProgressFunc) (string, error) {
	if err := img.Validate(); err != nil {
		return "", err
	}

	var activePart string
	var activeSeq uint32
	seq := c.initialSeq

	for _, part := range c.partitions {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		hdr, err := EncodeWritePartHeader(part, uint64(img.Size), seq)
		if err != nil {
			return "", err
		}
		if err := c.writeExact(ctx, hdr, c.cmdTimeout); err != nil {
			return "", fmt.Errorf("azupdate send header for %q: %w", part, err)
		}
		reply, err := c.readReply(ctx, seq, c.cmdTimeout)
		if err != nil {
			return "", fmt.Errorf("azupdate read ready for %q: %w", part, err)
		}
		if reply.Status == StatusReady {
			activePart = part
			activeSeq = seq
			break
		}
		if reply.Status != StatusNoEntry {
			return "", &RefusedError{Partition: part, Status: reply.Status, Phase: "write-part"}
		}
		seq++
	}
	if activePart == "" {
		return "", fmt.Errorf("%w: tried %v", ErrNoPartition, c.partitions)
	}

	// From here onward, any transport/timeout/EOF failure leaves the device in
	// an unknown write state and must never be automatically replayed.
	buf := make([]byte, c.chunkSize)
	var sent int64
	proto.EmitProgress(progress, "write", 0, img.Size)
	for sent < img.Size {
		n := c.chunkSize
		if rem := img.Size - sent; rem < int64(n) {
			n = int(rem)
		}
		if err := img.ReadExact(sent, buf[:n]); err != nil {
			return "", proto.MarkOutcomeUnknown("azupdate read image", err)
		}
		if err := c.writeExact(ctx, buf[:n], c.dataTimeout); err != nil {
			return "", proto.MarkOutcomeUnknown("azupdate write chunk", err)
		}
		sent += int64(n)
		proto.EmitProgress(progress, "write", sent, img.Size)
	}

	if err := c.writeExact(ctx, []byte{}, c.zlpTimeout); err != nil {
		return "", proto.MarkOutcomeUnknown("azupdate write ZLP", err)
	}

	finalReply, err := c.readReply(ctx, activeSeq, c.statusTimeout)
	if err != nil {
		return "", proto.MarkOutcomeUnknown("azupdate final status", err)
	}
	if finalReply.Status != StatusWritten {
		return "", proto.MarkOutcomeUnknown("azupdate confirm "+activePart, &RefusedError{Partition: activePart, Status: finalReply.Status, Phase: "confirm"})
	}
	return activePart, nil
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
		return fmt.Errorf("short USB bulk write: %d of %d bytes", n, len(data))
	}
	return nil
}

func (c *Client) readReply(ctx context.Context, expectedSeq uint32, timeout time.Duration) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	buf := make([]byte, replyReadSize)
	n, err := c.dev.ReadBulk(tctx, c.eps.In, buf)
	if err != nil {
		return Reply{}, err
	}
	if n < 0 || n > len(buf) {
		return Reply{}, fmt.Errorf("azupdate: invalid ReadBulk count %d", n)
	}
	return ParseReply(buf[:n], CmdWritePart, expectedSeq)
}
