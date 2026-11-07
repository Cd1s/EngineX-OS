package prolink

import (
	"encoding/binary"
	"testing"
)

func buildDJMOnAirPacket(subtype byte, ch1, ch2, ch3, ch4 byte) []byte {
	// Standard 4-channel packet is 0x2d (45) bytes, 6-channel is 0x35 (53) bytes
	pktLen := 0x2d
	lenR := uint16(0x0009)
	if subtype == 0x03 {
		pktLen = 0x35
		lenR = uint16(0x0011)
	}
	pkt := make([]byte, pktLen)
	copy(pkt[0:10], []byte("Qspt1WmJOL"))
	pkt[10] = 0x03 // PacketType: CHANNELS_ON_AIR
	copy(pkt[11:31], []byte("DJM-900NXS2         "))
	pkt[0x1f] = 0x01
	pkt[0x20] = subtype
	pkt[0x21] = 0x0d // Mixer device number
	binary.BigEndian.PutUint16(pkt[0x22:0x24], lenR)
	pkt[0x24] = ch1
	pkt[0x25] = ch2
	pkt[0x26] = ch3
	pkt[0x27] = ch4
	return pkt
}

func TestParseDJMOnAirPacket(t *testing.T) {
	// 4-channel packet with Ch 1 and Ch 4 on-air
	pkt4 := buildDJMOnAirPacket(0x00, 1, 0, 0, 1)
	onAir, err := ParseDJMOnAirPacket(pkt4)
	if err != nil {
		t.Fatalf("unexpected error parsing 4-channel packet: %v", err)
	}
	expected := [4]bool{true, false, false, true}
	if onAir != expected {
		t.Fatalf("got %v, want %v", onAir, expected)
	}

	// 6-channel packet with Ch 2 and Ch 3 on-air
	pkt6 := buildDJMOnAirPacket(0x03, 0, 1, 1, 0)
	onAir6, err := ParseDJMOnAirPacket(pkt6)
	if err != nil {
		t.Fatalf("unexpected error parsing 6-channel packet: %v", err)
	}
	expected6 := [4]bool{false, true, true, false}
	if onAir6 != expected6 {
		t.Fatalf("got %v, want %v", onAir6, expected6)
	}

	// All channels off
	pktOff := buildDJMOnAirPacket(0x00, 0, 0, 0, 0)
	onAirOff, err := ParseDJMOnAirPacket(pktOff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if onAirOff != [4]bool{false, false, false, false} {
		t.Fatalf("got %v, want all false", onAirOff)
	}

	// Error cases:
	// 1. Truncated packet
	if _, err := ParseDJMOnAirPacket([]byte("short")); err == nil {
		t.Errorf("expected error for short packet")
	}

	// 2. Invalid magic
	badMagic := buildDJMOnAirPacket(0x00, 1, 0, 0, 0)
	copy(badMagic[0:4], []byte("XXXX"))
	if _, err := ParseDJMOnAirPacket(badMagic); err == nil {
		t.Errorf("expected error for bad magic")
	}

	// 3. Invalid packet type
	badType := buildDJMOnAirPacket(0x00, 1, 0, 0, 0)
	badType[10] = 0x06
	if _, err := ParseDJMOnAirPacket(badType); err == nil {
		t.Errorf("expected error for bad packet type")
	}

	// 4. Invalid subtype
	badSubtype := buildDJMOnAirPacket(0x00, 1, 0, 0, 0)
	badSubtype[0x20] = 0x05
	if _, err := ParseDJMOnAirPacket(badSubtype); err == nil {
		t.Errorf("expected error for unsupported subtype")
	}
}
