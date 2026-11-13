package usb

import (
	"context"
	"encoding/binary"
	"errors"
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
func TestErrorWriteUncertainty(t *testing.T) {
	err := fault("write", System, true, context.DeadlineExceeded)
	var detail *Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &detail) || detail.Kind != Timeout || !detail.MayHaveWritten {
		t.Fatal(err)
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
