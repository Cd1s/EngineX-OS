// Package proto defines shared transport interfaces, image/progress types, and
// protocol-safety error semantics for EngineX-Installer USB protocols.
//
// The package intentionally consumes a local structural USB device interface so
// it remains decoupled from OS USB implementations and can be exercised with
// deterministic in-memory fakes.
package proto

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Progress reports byte transfer progress for a flashing or readback phase.
type Progress struct {
	Phase string
	Sent  int64
	Total int64
}

// ProgressFunc receives optional non-blocking progress updates.
type ProgressFunc func(Progress)

// Image describes a fixed-size random-access payload to transfer to a device.
type Image struct {
	Data io.ReaderAt
	Size int64
}

// Validate checks that img has a backing ReaderAt and a positive size.
func (img Image) Validate() error {
	if img.Data == nil {
		return errors.New("proto: image data reader is nil")
	}
	if img.Size <= 0 {
		return fmt.Errorf("proto: image size must be positive, got %d", img.Size)
	}
	return nil
}

// ReadExact reads exactly len(dst) bytes from img.Data starting at offset,
// failing on short reads or underlying errors.
func (img Image) ReadExact(offset int64, dst []byte) error {
	if offset < 0 || offset+int64(len(dst)) > img.Size {
		return fmt.Errorf("proto: read [%d:%d] out of image bounds [0:%d]", offset, offset+int64(len(dst)), img.Size)
	}
	read := 0
	for read < len(dst) {
		n, err := img.Data.ReadAt(dst[read:], offset+int64(read))
		read += n
		if err != nil {
			if errors.Is(err, io.EOF) && read == len(dst) {
				return nil
			}
			return fmt.Errorf("proto: short read at offset %d (got %d of %d bytes): %w", offset, read, len(dst), err)
		}
		if n == 0 {
			return fmt.Errorf("proto: zero-byte read at offset %d", offset+int64(read))
		}
	}
	return nil
}

// Endpoints identifies the USB bulk endpoint addresses and packet size for a
// claimed interface.
type Endpoints struct {
	In            uint8
	Out           uint8
	MaxPacketSize uint16
}

// Validate ensures the bulk IN and OUT endpoint directions are well-formed.
func (e Endpoints) Validate() error {
	if e.In == 0 || e.In&0x80 == 0 {
		return fmt.Errorf("proto: invalid bulk IN endpoint 0x%02x", e.In)
	}
	if e.Out == 0 || e.Out&0x80 != 0 {
		return fmt.Errorf("proto: invalid bulk OUT endpoint 0x%02x", e.Out)
	}
	return nil
}

// BulkDevice is the structural USB bulk transport contract satisfied by the OS
// USB device handle. One WriteBulk call corresponds to one USB transfer,
// including a zero-length packet when len(data) == 0.
type BulkDevice interface {
	ReadBulk(ctx context.Context, ep uint8, buf []byte) (int, error)
	WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error)
}

// USBDevice extends BulkDevice with control transfers, endpoint halt recovery,
// and handle closure.
type USBDevice interface {
	BulkDevice
	Control(ctx context.Context, requestType, request uint8, value, index uint16, data []byte) (int, error)
	ClearHalt(ctx context.Context, ep uint8) error
	Close() error
}

// ErrOutcomeUnknown indicates a failure occurred after host-to-device write
// bytes may already have been accepted by the device. Callers must never
// automatically replay a failed write wrapped with this sentinel.
var ErrOutcomeUnknown = errors.New("write outcome unknown")

// OutcomeUnknownError wraps an operation error whose write side-effect on the
// target device cannot be ruled out (for example, a timeout or disconnect while
// awaiting final write acknowledgement).
type OutcomeUnknownError struct {
	Op  string
	Err error
}

func (e *OutcomeUnknownError) Error() string {
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", ErrOutcomeUnknown, e.Err)
	}
	return fmt.Sprintf("%s (%s): %v", e.Op, ErrOutcomeUnknown, e.Err)
}

func (e *OutcomeUnknownError) Unwrap() error {
	return e.Err
}

func (e *OutcomeUnknownError) Is(target error) bool {
	return target == ErrOutcomeUnknown
}

// MarkOutcomeUnknown wraps err in an OutcomeUnknownError unless err is nil or
// already marks an unknown outcome.
func MarkOutcomeUnknown(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		return err
	}
	return &OutcomeUnknownError{Op: op, Err: err}
}

// IsOutcomeUnknown reports whether err indicates a write whose device-side
// completion cannot be ruled out.
func IsOutcomeUnknown(err error) bool {
	return errors.Is(err, ErrOutcomeUnknown)
}

// EmitProgress safely invokes cb if non-nil.
func EmitProgress(cb ProgressFunc, phase string, sent, total int64) {
	if cb != nil {
		cb(Progress{Phase: phase, Sent: sent, Total: total})
	}
}
