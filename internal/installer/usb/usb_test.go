package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"
)

func configurationFixture() []byte {
	b := []byte{9, 2, 32, 0, 1, 1, 0, 0x80, 50, 9, 4, 3, 0, 2, 0xff, 0x61, 0x7a, 0, 7, 5, 0x81, 2, 0, 2, 0, 7, 5, 0x02, 2, 0, 2, 0}
	return b
}
func TestConfigurationDescriptors(t *testing.T) {
	valid := configurationFixture()
	it, err := parseConfiguration(valid)
	if err != nil || len(it) != 1 || it[0].Number != 3 || it[0].Endpoints[1].MaxPacketSize != 512 {
		t.Fatalf("interfaces=%+v err=%v", it, err)
	}
	for name, change := range map[string]func([]byte) []byte{
		"zero length":        func(b []byte) []byte { b[9] = 0; return b },
		"overrun":            func(b []byte) []byte { b[18] = 255; return b },
		"truncated":          func(b []byte) []byte { return b[:len(b)-1] },
		"duplicate endpoint": func(b []byte) []byte { b[27] = b[20]; return b },
		"endpoint zero":      func(b []byte) []byte { b[20] = 0; return b },
		"count":              func(b []byte) []byte { b[13] = 3; return b },
		"interface count":    func(b []byte) []byte { b[4] = 2; return b },
		"duplicate alternate": func(b []byte) []byte {
			b = append(b, b[9:18]...)
			binary.LittleEndian.PutUint16(b[2:4], uint16(len(b)))
			return b
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseConfiguration(change(append([]byte(nil), valid...)))
			if err == nil {
				t.Fatal("accepted malformed descriptors")
			}
		})
	}
}
func TestSelectionAndIsolation(t *testing.T) {
	it, _ := parseConfiguration(configurationFixture())
	info := DeviceInfo{Interfaces: it}
	if _, err := selected(info, 0, 0); err == nil {
		t.Fatal("accepted another interface")
	}
	for _, ep := range []uint8{0, 0x82, 0x02} {
		if err := bulkEndpoint(it[0], ep, true); err == nil {
			t.Fatalf("accepted endpoint %x", ep)
		}
	}
	if err := bulkEndpoint(it[0], 0x81, true); err != nil {
		t.Fatal(err)
	}
	copy := cloneInfo(info)
	copy.Interfaces[0].Endpoints[0].Address = 0
	if info.Interfaces[0].Endpoints[0].Address != 0x81 {
		t.Fatal("descriptor alias escaped")
	}
	info.Interfaces[0].Class = 3
	if _, err := selected(info, 3, 0); err == nil {
		t.Fatal("opened HID interface")
	}
}
func TestSessionCloseCancelsAndWaits(t *testing.T) {
	s := newSession()
	ctx, end, err := s.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(finished)
		if err := s.close(func() error { close(closed); return nil }); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not cancel")
	}
	select {
	case <-closed:
		t.Fatal("native resources closed before drain")
	default:
	}
	end()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("close did not finish")
	}
	if _, _, err := s.begin(context.Background()); err == nil {
		t.Fatal("reused closed handle")
	}
	if err := s.close(func() error { t.Fatal("closed twice"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestSessionWaitingTransferCancellation(t *testing.T) {
	s := newSession()
	_, end, _ := s.begin(context.Background())
	defer end()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.begin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionHonorsLongerExplicitDeadline(t *testing.T) {
	s := newSession()
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, end, err := s.beginTimeout(parent, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	parentDeadline, _ := parent.Deadline()
	actualDeadline, _ := ctx.Deadline()
	if !actualDeadline.Equal(parentDeadline) {
		t.Fatalf("caller deadline clamped: %v versus %v", actualDeadline, parentDeadline)
	}
	select {
	case <-ctx.Done():
		t.Fatal("explicit deadline replaced by fallback")
	case <-time.After(5 * time.Millisecond):
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("explicit-deadline transfer did not cancel")
	}
}

func TestSessionAppliesDefaultWithoutDeadline(t *testing.T) {
	s := newSession()
	ctx, end, err := s.beginTimeout(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("default deadline absent")
	}
}
func TestErrorWriteUncertainty(t *testing.T) {
	err := fault("write", System, true, context.DeadlineExceeded)
	var detail *Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &detail) || detail.Kind != Timeout || !detail.MayHaveWritten {
		t.Fatal(err)
	}
}

func TestErrorWrappingPreservesClassification(t *testing.T) {
	cause := errors.New("invalid native descriptor")
	inner := fault("descriptors", Protocol, true, cause)
	outer := fault("enumerate", System, false, fmt.Errorf("descriptor context: %w", inner))
	var detail *Error
	if !errors.As(outer, &detail) || detail.Kind != Protocol || !detail.MayHaveWritten || !errors.Is(outer, cause) || !errors.Is(outer, inner) {
		t.Fatalf("typed cause hidden by wrapper: %v", outer)
	}
}

func TestJoinedErrorsPreserveEveryWriteUncertainty(t *testing.T) {
	safeCause := errors.New("discovery failed")
	writeCause := errors.New("write completion unknown")
	safe := fault("enumerate", Permission, false, safeCause)
	uncertain := fault("write", Protocol, true, writeCause)
	joined := errors.Join(fmt.Errorf("first branch: %w", safe), fmt.Errorf("second branch: %w", errors.Join(errors.New("other failure"), uncertain)))
	outer := fault("outer", System, false, joined)
	var detail *Error
	if !errors.As(outer, &detail) || detail.Kind != Permission || !detail.MayHaveWritten || !errors.Is(outer, safeCause) || !errors.Is(outer, writeCause) {
		t.Fatalf("joined write uncertainty lost: %v", outer)
	}
	timed := fault("outer", System, false, errors.Join(safe, errors.Join(context.DeadlineExceeded, uncertain)))
	if !errors.As(timed, &detail) || detail.Kind != Timeout || !detail.MayHaveWritten || !errors.Is(timed, context.DeadlineExceeded) {
		t.Fatalf("joined context cause lost: %v", timed)
	}
}

func TestControlStaysOnSelectedInterface(t *testing.T) {
	interfaces, _ := parseConfiguration(configurationFixture())
	it := interfaces[0]
	for _, test := range []struct {
		typ, request uint8
		index        uint16
		length       int
	}{
		{0xc1, 1, 4, 1}, {0xc1, 1, 0x103, 1}, {0xc2, 1, 0x82, 1}, {0xc2, 1, 0x181, 1}, {0xc0, 1, 0, 65536}, {0, 9, 0, 0}, {1, 11, 3, 0},
	} {
		if err := controlRequest(it, test.typ, test.request, test.index, test.length); err == nil {
			t.Fatalf("accepted escaping control %+v", test)
		}
	}
	for _, test := range []struct {
		typ, request uint8
		index        uint16
	}{{0xc1, 1, 3}, {0xc2, 1, 0x81}, {0xc0, 1, 0}, {0x80, 6, 0}} {
		if err := controlRequest(it, test.typ, test.request, test.index, 64); err != nil {
			t.Fatalf("rejected valid control %+v: %v", test, err)
		}
	}
}

// This smoke test enumerates descriptors only. It never opens or claims a
// device and is also run as a precompiled executable on Windows and macOS.
func TestNativeEnumeration(t *testing.T) {
	b, err := NewBackend()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devices, err := b.Enumerate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[DeviceID]bool{}
	for _, d := range devices {
		if d.ID == "" || seen[d.ID] {
			t.Fatal("missing or duplicate identity")
		}
		seen[d.ID] = true
	}
	t.Logf("enumerated %d USB attachments without opening them", len(devices))
}
