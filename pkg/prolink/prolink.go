// Package prolink provides passive Pro DJ Link discovery/status. It does not impersonate a CDJ or write to Rekordbox.
package prolink

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const magic = "Qspt1WmJOL"

type Device struct {
	Address                  string `json:"address"`
	Name                     string `json:"name"`
	Number                   byte   `json:"number"`
	PacketType               byte   `json:"packet_type"`
	TrackID                  uint32 `json:"track_id,omitempty"`
	SourcePlayer, SourceSlot byte
	LastSeen                 time.Time `json:"last_seen"`
}

// ParsePacket only accepts known packet shapes on their corresponding ports.
// Packet offsets follow Deep Symmetry's DJ Link protocol analysis.
func ParsePacket(b []byte, port int, remote net.IP) (Device, error) {
	d := Device{}
	if len(b) < 0x21 || string(b[:10]) != magic {
		return d, fmt.Errorf("prolink: invalid packet header")
	}
	d.PacketType = b[10]
	d.Address = remote.String()
	name := b[12:32]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i] // C string: bytes after the NUL are buffer garbage (#36)
	}
	d.Name = strings.TrimRight(string(name), " ")
	d.LastSeen = time.Now()
	switch {
	case port == 50000 && b[10] == 0x06:
		if len(b) < 0x36 {
			return d, fmt.Errorf("prolink: truncated announcement")
		}
		d.Number = b[0x24]
	case port == 50002 && b[10] == 0x0a:
		if len(b) < 0xcc {
			return d, fmt.Errorf("prolink: truncated player status")
		}
		d.Number = b[0x21]
		d.SourcePlayer = b[0x28]
		d.SourceSlot = b[0x29]
		d.TrackID = binary.BigEndian.Uint32(b[0x2c:])
	default:
		return d, fmt.Errorf("prolink: unsupported packet type/port")
	}
	return d, nil
}

const maxDevices = 64

type Discovery struct {
	mu      sync.RWMutex
	devices map[string]Device
}

func NewDiscovery() *Discovery { return &Discovery{devices: map[string]Device{}} }
func (d *Discovery) Observe(device Device) Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.devices == nil {
		d.devices = map[string]Device{}
	}
	now := time.Now()
	d.prune(now)
	if device.LastSeen.IsZero() {
		device.LastSeen = now
	}
	key := fmt.Sprintf("%s/%d", device.Address, device.Number)
	// Spoofed UDP must not grow the table without bound; a real network has a
	// handful of players and mixers (#36).
	if _, ok := d.devices[key]; !ok && len(d.devices) >= maxDevices {
		return device
	}
	if old, ok := d.devices[key]; ok && device.PacketType == 6 {
		device.TrackID = old.TrackID
		device.SourcePlayer = old.SourcePlayer
		device.SourceSlot = old.SourceSlot
	}
	d.devices[key] = device
	return device
}
func (d *Discovery) Devices(now time.Time) []Device {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune(now)
	var out []Device
	for _, v := range d.devices {
		if now.Sub(v.LastSeen) <= 10*time.Second {
			out = append(out, v)
		}
	}
	return out
}

// prune runs while the discovery mutex is held.
func (d *Discovery) prune(now time.Time) {
	for key, device := range d.devices {
		if now.Sub(device.LastSeen) > 10*time.Second {
			delete(d.devices, key)
		}
	}
}

// Listen listens to discovery and status broadcasts on the selected interface
// address, with bounded reads and context cancellation. No network writes occur.
func (d *Discovery) Listen(ctx context.Context, address net.IP, notify func(Device)) error {
	var conns []*net.UDPConn
	for _, port := range []int{50000, 50002} {
		c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: address, Port: port})
		if e != nil {
			for _, old := range conns {
				old.Close()
			}
			return e
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	errs := make(chan error, len(conns))
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			b := make([]byte, 2048)
			for {
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				n, addr, e := c.ReadFromUDP(b)
				if ctx.Err() != nil {
					return
				}
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					continue
				}
				if e != nil {
					errs <- e
					return
				}
				device, e := ParsePacket(b[:n], c.LocalAddr().(*net.UDPAddr).Port, addr.IP)
				if e != nil {
					continue
				}
				device = d.Observe(device)
				if notify != nil {
					notify(device)
				}
			}
		}(c)
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}
	for _, c := range conns {
		c.Close()
	}
	wg.Wait()
	return err
}
