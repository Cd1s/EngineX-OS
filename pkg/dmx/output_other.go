//go:build !linux

package dmx

import (
	"errors"
	"net"
)

func allowBroadcast(c *net.UDPConn) error { return nil }
func openUSB(o Output) (usbOutput, error) { return nil, errors.New("USB DMX requires Linux") }
