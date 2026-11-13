// Package usb provides native, interface-scoped USB access without libusb.
// Enumeration never claims an interface. Protocol and model selection belong
// to the caller; an open device always refers to one enumerated attachment.
package usb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type DeviceID string

type Endpoint struct {
	Address, Attributes uint8
	MaxPacketSize       uint16
}

type Interface struct {
	Number, Alternate, Class, Subclass, Protocol uint8
	Endpoints                                    []Endpoint
}

type DeviceInfo struct {
	ID              DeviceID
	VID, PID        uint16
	Serial, Product string
	Interfaces      []Interface
	// Driver is diagnostic only. Its presence does not prove a usable binding.
	Driver string
}

type Backend interface {
	Enumerate(context.Context) ([]DeviceInfo, error)
	Open(context.Context, DeviceID, uint8, uint8) (Device, error)
}

// Device serializes transfers. Close cancels and drains a pending transfer
// before releasing the interface. Each WriteBulk is exactly one OS transfer,
// including a zero-length write; short transfers are returned without replay.
type Device interface {
	Info() DeviceInfo
	Control(context.Context, uint8, uint8, uint16, uint16, []byte) (int, error)
	ReadBulk(context.Context, uint8, []byte) (int, error)
	WriteBulk(context.Context, uint8, []byte) (int, error)
	ClearHalt(context.Context, uint8) error
	Close() error
}

type Kind string

const (
	Permission   Kind = "permission"
	Binding      Kind = "binding"
	Busy         Kind = "busy"
	Disconnected Kind = "disconnected"
	Timeout      Kind = "timeout"
	Cancelled    Kind = "cancelled"
	Protocol     Kind = "protocol"
	Unsupported  Kind = "unsupported"
	System       Kind = "system"
)

// Error preserves the underlying OS/context error for errors.Is/As. A write
// with MayHaveWritten must never be automatically retried, even if N is zero.
type Error struct {
	Op             string
	Kind           Kind
	MayHaveWritten bool
	Err            error
}

func (e *Error) Error() string { return fmt.Sprintf("usb %s (%s): %v", e.Op, e.Kind, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func fault(op string, kind Kind, written bool, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		kind = Timeout
	}
	if errors.Is(err, context.Canceled) {
		kind = Cancelled
	}
	return &Error{Op: op, Kind: kind, MayHaveWritten: written, Err: err}
}

func cloneInfo(d DeviceInfo) DeviceInfo {
	d.Interfaces = append([]Interface(nil), d.Interfaces...)
	for i := range d.Interfaces {
		d.Interfaces[i].Endpoints = append([]Endpoint(nil), d.Interfaces[i].Endpoints...)
	}
	return d
}

func selected(info DeviceInfo, number, alternate uint8) (Interface, error) {
	for _, it := range info.Interfaces {
		if it.Number == number && it.Alternate == alternate {
			if it.Class != 0xff {
				return Interface{}, fault("open", Unsupported, false, errors.New("selected interface is not vendor-specific"))
			}
			return it, nil
		}
	}
	return Interface{}, fault("open", Protocol, false, errors.New("selected interface/alternate is absent"))
}

func bulkEndpoint(it Interface, address uint8, in bool) error {
	if address&0x0f == 0 || (address&0x80 != 0) != in {
		return fault("bulk", Protocol, false, errors.New("invalid endpoint direction"))
	}
	for _, ep := range it.Endpoints {
		if ep.Address == address && ep.Attributes&3 == 2 && ep.MaxPacketSize != 0 {
			return nil
		}
	}
	return fault("bulk", Protocol, false, errors.New("bulk endpoint does not belong to the claimed interface"))
}

const transferLimit = 16 << 20
const defaultTimeout = 30 * time.Second

// session owns cancellation and the serialized native handle. The native call
// must finish/drain before end; the context watcher never owns a caller buffer.
type session struct {
	gate   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newSession() *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{gate: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
}
func (s *session) begin(parent context.Context) (context.Context, func(), error) {
	return s.beginTimeout(parent, defaultTimeout)
}

func (s *session) beginTimeout(parent context.Context, fallback time.Duration) (context.Context, func(), error) {
	select {
	case <-s.ctx.Done():
		return nil, nil, fault("transfer", Disconnected, false, errors.New("device is closed"))
	case <-parent.Done():
		return nil, nil, fault("transfer", Cancelled, false, parent.Err())
	case s.gate <- struct{}{}:
	}
	select {
	case <-s.ctx.Done():
		<-s.gate
		return nil, nil, fault("transfer", Disconnected, false, errors.New("device is closed"))
	default:
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if _, ok := parent.Deadline(); ok {
		ctx, cancel = context.WithCancel(parent)
	} else {
		ctx, cancel = context.WithTimeout(parent, fallback)
	}
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel(); <-s.gate }, nil
}

func (s *session) close(fn func() error) error {
	var err error
	s.once.Do(func() { s.cancel(); s.gate <- struct{}{}; defer func() { <-s.gate }(); err = fn() })
	return err
}

var idCounter atomic.Uint64

func nextID() DeviceID { return DeviceID(fmt.Sprintf("usb-%x", idCounter.Add(1))) }

func timeoutMS(ctx context.Context) uint32 {
	d := defaultTimeout
	if deadline, ok := ctx.Deadline(); ok {
		d = time.Until(deadline)
	}
	if d < time.Millisecond {
		return 1
	}
	if d/time.Millisecond >= time.Duration(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32((d + time.Millisecond - 1) / time.Millisecond)
}

func transferSize(n int) error {
	if n > transferLimit {
		return fault("transfer", Protocol, false, errors.New("transfer exceeds 16 MiB limit"))
	}
	return nil
}

func controlRequest(it Interface, typ, request uint8, index uint16, length int) error {
	if length > 65535 {
		return fault("control", Protocol, false, errors.New("control length exceeds uint16"))
	}
	switch typ & 0x1f {
	case 1:
		if index != uint16(it.Number) {
			return fault("control", Protocol, false, errors.New("control targets another interface"))
		}
	case 2:
		if index > 255 {
			return fault("control", Protocol, false, errors.New("invalid endpoint index"))
		}
		if err := bulkEndpoint(it, uint8(index), index&0x80 != 0); err != nil {
			return err
		}
	case 0:
	default:
		return fault("control", Unsupported, false, errors.New("unsupported control recipient"))
	}
	// Configuration/address/alternate selection is owned by Open, never by
	// protocol control traffic on an already selected interface.
	if typ&0x60 == 0 && typ&0x80 == 0 && (request == 5 || request == 9 || request == 11) {
		return fault("control", Unsupported, false, errors.New("control request changes device configuration"))
	}
	return nil
}
