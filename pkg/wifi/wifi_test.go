//go:build linux

package wifi

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPowerSaveWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cmd   byte
		ack   bool
		state uint32
	}{
		{"get", 62, false, 0}, {"off", 61, true, 0}, {"on", 61, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attrs := [][]byte{number(3, 7)}
			if tc.ack {
				attrs = append(attrs, number(93, tc.state))
			}
			b := message(42, tc.cmd, 9, tc.ack, attrs...)
			if binary.NativeEndian.Uint32(b) != uint32(len(b)) || binary.NativeEndian.Uint16(b[4:]) != 42 || binary.NativeEndian.Uint32(b[8:]) != 9 || b[16] != tc.cmd || b[17] != 1 {
				t.Fatalf("bad header %x", b)
			}
			flags := uint16(unix.NLM_F_REQUEST)
			if tc.ack {
				flags |= unix.NLM_F_ACK
			}
			if binary.NativeEndian.Uint16(b[6:]) != flags {
				t.Fatal("bad flags")
			}
			v, err := value(b[20:], 3, 4)
			if err != nil || binary.NativeEndian.Uint32(v) != 7 {
				t.Fatalf("ifindex: %x %v", v, err)
			}
			if tc.ack {
				v, err = value(b[20:], 93, 4)
				if err != nil || binary.NativeEndian.Uint32(v) != tc.state {
					t.Fatalf("state: %x %v", v, err)
				}
			}
		})
	}
}

func TestPowerSaveReplyValidation(t *testing.T) {
	ack := message(unix.NLMSG_ERROR, 0, 9, false)
	ack[16], ack[17] = 0, 0 // nlmsgerr's zero errno, not a generic header
	denied := append([]byte(nil), ack...)
	binary.NativeEndian.PutUint32(denied[16:], ^uint32(unix.EPERM-1))
	shortAck := append([]byte(nil), ack...)
	binary.NativeEndian.PutUint32(shortAck, 18)
	for _, tc := range []struct {
		name string
		data []byte
		ack  bool
		fail bool
	}{
		{"state", message(42, 62, 9, false, number(93, 1)), false, false},
		{"ack", ack, true, false}, {"denied", denied, true, true},
		{"wrong_sequence", message(42, 62, 8, false), false, true},
		{"wrong_family", message(41, 62, 9, false), false, true},
		{"set_needs_ack", message(42, 61, 9, false), true, true},
		{"truncated", ack[:18], true, true},
		{"short_ack", shortAck, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reply(tc.data, 42, 9, tc.ack)
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v", err)
			}
			if tc.name == "denied" && !errors.Is(err, unix.EPERM) {
				t.Fatalf("errno lost: %v", err)
			}
		})
	}
}

func TestAttributesValidateBoundsAndAlignment(t *testing.T) {
	b := append(attribute(2, []byte("nl80211\x00")), attribute(1, []byte{42, 0})...)
	v, err := value(b, 1, 2)
	if err != nil || binary.NativeEndian.Uint16(v) != 42 {
		t.Fatalf("family=%x err=%v", v, err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"short_header", []byte{1}}, {"short_length", []byte{3, 0, 1, 0}},
		{"overrun", []byte{8, 0, 1, 0}}, {"short_value", attribute(1, []byte{1})},
		{"missing", attribute(2, []byte{1, 2})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := value(tc.data, 1, 2); err == nil {
				t.Fatal("accepted malformed attributes")
			}
		})
	}
}

func TestPowerSaveMissingInterface(t *testing.T) {
	if _, err := net.InterfaceByName("enginex_missing"); err == nil {
		t.Skip("fixture name exists")
	}
	if _, err := GetPowerSave("enginex_missing"); err == nil || !strings.Contains(err.Error(), "enginex_missing") {
		t.Fatalf("get: %v", err)
	}
	if err := SetPowerSave("enginex_missing", false); err == nil || !strings.Contains(err.Error(), "enginex_missing") {
		t.Fatalf("set: %v", err)
	}
}
