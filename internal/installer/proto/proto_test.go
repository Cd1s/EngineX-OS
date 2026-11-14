package proto_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
)

func TestBoardNormalizationAndLookup(t *testing.T) {
	cases := []struct {
		raw      string
		wantCode string
		wantPref string
		wantGen2 bool
		wantOK   bool
	}{
		{"inmusic,jp07", "jp07", "SC5000", false, true},
		{"JC11", "jc11", "PRIME4", false, true},
		{"inmusic,jc11s", "jc11s", "PRIME4PLUS", true, true},
		{"rk3288-az05-jp11s", "jp11s", "PRIMEGOPLUS", true, true},
		{"rk3288_az0x_nh08s", "nh08s", "MIXSTREAMPROPLUS", true, true},
		{"prime4plus", "jc11s", "PRIME4PLUS", true, true},
		{"PRIME4+", "jc11s", "PRIME4PLUS", true, true},
		{"PRIME 4+", "jc11s", "PRIME4PLUS", true, true},
		{"jp24", "jp24", "PRIME4G2", true, true},
		{"rmz2", "rmz2", "SYSTEMONE", true, true},
		{"nh10", "nh10", "MIXSTREAMPROGO", true, true},
		{"unknown-board", "unknown-board", "", false, false},
	}
	for _, tc := range cases {
		if got := proto.BoardCode(tc.raw); got != tc.wantCode {
			t.Errorf("BoardCode(%q) = %q, want %q", tc.raw, got, tc.wantCode)
		}
		b, ok := proto.LookupBoard(tc.raw)
		if ok != tc.wantOK {
			t.Errorf("LookupBoard(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			continue
		}
		if ok && (b.Prefix != tc.wantPref || b.Gen2 != tc.wantGen2) {
			t.Errorf("LookupBoard(%q) = %+v, want prefix=%s gen2=%v", tc.raw, b, tc.wantPref, tc.wantGen2)
		}
	}
}

func TestGen1BoardByUSBID(t *testing.T) {
	b, ok := proto.Gen1BoardByUSBID(0x15e4, 0xd006)
	if !ok || b.Code != "jp07" || b.Gen2 {
		t.Fatalf("Gen1BoardByUSBID(15e4:d006) = (%+v, %v)", b, ok)
	}
	if _, ok := proto.Gen1BoardByUSBID(0x15e4, 0xd007); ok {
		t.Fatal("Gen-2 PRIME 4+ USB ID must not map as Gen-1")
	}
}

func TestEndpointsAndImageValidation(t *testing.T) {
	if err := (proto.Endpoints{In: 0x81, Out: 0x01}).Validate(); err != nil {
		t.Fatalf("valid endpoints rejected: %v", err)
	}
	for _, bad := range []proto.Endpoints{
		{In: 0x01, Out: 0x01},
		{In: 0x81, Out: 0x82},
		{In: 0x00, Out: 0x01},
		{In: 0x81, Out: 0x00},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("expected invalid endpoints error for %+v", bad)
		}
	}

	img := proto.Image{Data: bytes.NewReader([]byte("abcdef")), Size: 6}
	if err := img.Validate(); err != nil {
		t.Fatalf("valid image rejected: %v", err)
	}
	buf := make([]byte, 4)
	if err := img.ReadExact(2, buf); err != nil || string(buf) != "cdef" {
		t.Fatalf("ReadExact = %q, %v", buf, err)
	}
	if err := img.ReadExact(4, buf); err == nil {
		t.Fatal("expected out-of-bounds ReadExact to fail")
	}
	shortImg := proto.Image{Data: bytes.NewReader([]byte("ab")), Size: 6}
	if err := shortImg.ReadExact(0, buf); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && err == nil {
		t.Fatal("expected short backing ReaderAt to fail")
	}
}

func TestOutcomeUnknownError(t *testing.T) {
	base := errors.New("usb timeout")
	err := proto.MarkOutcomeUnknown("flash", base)
	if !proto.IsOutcomeUnknown(err) {
		t.Fatal("IsOutcomeUnknown = false, want true")
	}
	if !errors.Is(err, proto.ErrOutcomeUnknown) || !errors.Is(err, base) {
		t.Fatalf("errors.Is failed on %v", err)
	}
	if proto.MarkOutcomeUnknown("noop", nil) != nil {
		t.Fatal("MarkOutcomeUnknown(nil) must return nil")
	}
}
