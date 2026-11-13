package usb

import (
	"encoding/binary"
	"errors"
)

// parseConfiguration accepts one complete USB configuration descriptor. It
// rejects ambiguous endpoints/alternates instead of choosing the first one.
func parseConfiguration(data []byte) ([]Interface, error) {
	bad := func() ([]Interface, error) {
		return nil, fault("descriptors", Protocol, false, errors.New("invalid configuration descriptors"))
	}
	if len(data) < 9 || data[0] < 9 || data[1] != 2 || int(binary.LittleEndian.Uint16(data[2:4])) != len(data) {
		return bad()
	}
	var result []Interface
	current := -1
	counts := make(map[[2]uint8]uint8)
	numbers := make(map[uint8]bool)
	for offset := 0; offset < len(data); {
		if len(data)-offset < 2 {
			return bad()
		}
		n := int(data[offset])
		if n < 2 || n > len(data)-offset {
			return bad()
		}
		d := data[offset : offset+n]
		switch d[1] {
		case 4:
			if n < 9 {
				return bad()
			}
			key := [2]uint8{d[2], d[3]}
			if _, ok := counts[key]; ok {
				return bad()
			}
			counts[key] = d[4]
			numbers[d[2]] = true
			result = append(result, Interface{Number: d[2], Alternate: d[3], Class: d[5], Subclass: d[6], Protocol: d[7]})
			current = len(result) - 1
		case 5:
			if n < 7 || current < 0 {
				return bad()
			}
			it := &result[current]
			if d[2]&0x0f == 0 || d[2]&0x70 != 0 {
				return bad()
			}
			for _, ep := range it.Endpoints {
				if ep.Address == d[2] {
					return bad()
				}
			}
			it.Endpoints = append(it.Endpoints, Endpoint{Address: d[2], Attributes: d[3], MaxPacketSize: binary.LittleEndian.Uint16(d[4:6]) & 0x7ff})
		}
		offset += n
	}
	for _, it := range result {
		if int(counts[[2]uint8{it.Number, it.Alternate}]) != len(it.Endpoints) {
			return bad()
		}
	}
	if len(numbers) != int(data[4]) {
		return bad()
	}
	return result, nil
}
