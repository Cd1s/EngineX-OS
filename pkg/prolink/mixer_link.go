package prolink

import (
	"fmt"
)

// ParseDJMOnAirPacket parses a Pioneer DJM mixer On-Air status packet (packet type 0x03)
// broadcast on UDP port 50001, returning the On-Air states for channels 1 to 4.
// These flags allow SC6000 / SC5000 players to illuminate their jogwheel red ring indicators
// when their assigned mixer channel fader is up.
func ParseDJMOnAirPacket(pkt []byte) ([4]bool, error) {
	if len(pkt) < 0x28 {
		return [4]bool{}, fmt.Errorf("prolink: packet too short: %d bytes (min 40)", len(pkt))
	}
	if string(pkt[:10]) != magic {
		return [4]bool{}, fmt.Errorf("prolink: invalid packet header")
	}
	if pkt[10] != 0x03 {
		return [4]bool{}, fmt.Errorf("prolink: not an on-air packet type: 0x%02x", pkt[10])
	}
	subtype := pkt[0x20]
	if subtype != 0x00 && subtype != 0x03 {
		return [4]bool{}, fmt.Errorf("prolink: unsupported on-air subtype: 0x%02x", subtype)
	}

	var onAir [4]bool
	onAir[0] = pkt[0x24] != 0
	onAir[1] = pkt[0x25] != 0
	onAir[2] = pkt[0x26] != 0
	onAir[3] = pkt[0x27] != 0

	return onAir, nil
}
