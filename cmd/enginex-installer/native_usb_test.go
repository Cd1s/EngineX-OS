package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto/azupdate"
	"github.com/Cd1s/EngineX-OS/internal/installer/usb"
)

// testUSB exchanges real protocol packets with the production clients.
type testUSB struct {
	info        usb.DeviceInfo
	dir, img    string
	responses   [][]byte
	payload     bytes.Buffer
	remaining   int64
	seq         uint32
	status      uint16
	calls       int
	opens       int
	closes      int
	polls       int
	appearAfter int
	openErr     error
	enumerate   func() []usb.DeviceInfo
	beforeFlash func(context.Context) error
}

func usbInfo(vid, pid uint16, product string, az bool) usb.DeviceInfo {
	it := usb.Interface{Class: 0xff, Subclass: 0x42, Protocol: 3,
		Endpoints: []usb.Endpoint{{Address: 0x81, Attributes: 2, MaxPacketSize: 512}, {Address: 1, Attributes: 2, MaxPacketSize: 512}}}
	if az {
		it.Subclass, it.Protocol = 0x61, 0x7a
	}
	return usb.DeviceInfo{ID: "fixture-device", VID: vid, PID: pid, Product: product, Interfaces: []usb.Interface{it}}
}

func (d *testUSB) Enumerate(context.Context) ([]usb.DeviceInfo, error) {
	d.polls++
	if d.enumerate != nil {
		return d.enumerate(), nil
	}
	if d.info.ID == "" || d.polls < d.appearAfter {
		return nil, nil
	}
	return []usb.DeviceInfo{d.info}, nil
}

func (d *testUSB) Open(context.Context, usb.DeviceID, uint8, uint8) (usb.Device, error) {
	d.opens++
	return d, d.openErr
}

func (d *testUSB) Info() usb.DeviceInfo                   { return d.info }
func (d *testUSB) Close() error                           { d.closes++; return nil }
func (d *testUSB) ClearHalt(context.Context, uint8) error { return nil }
func (d *testUSB) Control(context.Context, uint8, uint8, uint16, uint16, []byte) (int, error) {
	return 0, errors.New("unexpected control transfer")
}

func (d *testUSB) ReadBulk(ctx context.Context, _ uint8, b []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(d.responses) == 0 {
		return 0, errors.New("unexpected protocol read")
	}
	response := d.responses[0]
	d.responses = d.responses[1:]
	return copy(b, response), nil
}

func (d *testUSB) WriteBulk(ctx context.Context, _ uint8, b []byte) (int, error) {
	if d.info.VID == 0x1cc5 {
		if d.remaining > 0 {
			d.payload.Write(b)
			d.remaining -= int64(len(b))
		} else if len(b) == 0 {
			d.calls++
			if err := os.WriteFile(d.img, d.payload.Bytes(), 0o600); err != nil {
				return 0, err
			}
			d.azReply(azupdate.StatusWritten)
		} else {
			d.seq = binary.LittleEndian.Uint32(b[16:20])
			d.remaining = int64(binary.LittleEndian.Uint64(b[8:16]))
			status := d.status
			if status == 0 {
				status = azupdate.StatusReady
			}
			d.azReply(status)
		}
		return len(b), nil
	}
	if d.remaining > 0 {
		d.payload.Write(b)
		d.remaining -= int64(len(b))
		if d.remaining == 0 {
			d.responses = append(d.responses, []byte("OKAY"))
		}
		return len(b), nil
	}
	command := string(b)
	call := strings.Replace(command, ":", " ", 1)
	if strings.HasPrefix(command, "flash:") {
		call += " native-image"
	}
	f, err := os.OpenFile(filepath.Join(d.dir, "calls.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(f, call)
	f.Close()
	reply := "OKAY"
	switch {
	case command == "getvar:product":
		product, _ := os.ReadFile(filepath.Join(d.dir, "product"))
		if len(product) == 0 {
			reply = "FAILVariable not implemented"
		} else {
			reply += string(product)
		}
	case command == "getvar:partition-size:data":
		if fileExists(filepath.Join(d.dir, "nodata")) {
			reply = "FAILVariable not implemented"
		} else {
			reply += "0x4000000"
		}
	case command == "getvar:partition-size:userdata":
		reply += "0x4000000"
	case command == "getvar:partition-type:data":
		if fileExists(filepath.Join(d.dir, "rawdata")) {
			reply += "raw"
		} else {
			reply += "ext4"
		}
	case command == "getvar:max-download-size":
		reply += "0x20000000"
	case strings.HasPrefix(command, "download:"):
		d.remaining, _ = strconv.ParseInt(strings.TrimPrefix(command, "download:"), 16, 64)
		d.payload.Reset()
		reply = fmt.Sprintf("DATA%08x", d.remaining)
	case strings.HasPrefix(command, "flash:"):
		if d.beforeFlash != nil {
			if err := d.beforeFlash(ctx); err != nil {
				return 0, err
			}
		}
		if err := os.WriteFile(filepath.Join(d.dir, "flashed-"+strings.TrimPrefix(command, "flash:")+".img"), d.payload.Bytes(), 0o600); err != nil {
			return 0, err
		}
	}
	d.responses = append(d.responses, []byte(reply))
	return len(b), nil
}

func (d *testUSB) azReply(status uint16) {
	b := make([]byte, azupdate.ReplySize)
	copy(b, azupdate.ReplyMagic)
	binary.LittleEndian.PutUint32(b[4:8], azupdate.CmdWritePart)
	binary.LittleEndian.PutUint32(b[16:20], d.seq)
	binary.LittleEndian.PutUint16(b[20:22], status)
	binary.LittleEndian.PutUint16(b[22:24], uint16(len(b)))
	d.responses = append(d.responses, b)
}

type fbShim struct {
	Bin *testUSB
	Dir string
}

func newFastboot(t *testing.T, product string, hasData bool) fbShim {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "product"), []byte(product), 0o600)
	if !hasData {
		os.WriteFile(filepath.Join(dir, "nodata"), nil, 0o600)
	}
	return fbShim{Bin: &testUSB{info: usbInfo(0x18d1, 0x4ee0, product, false), dir: dir}, Dir: dir}
}

func (f fbShim) Calls() []string {
	b, _ := os.ReadFile(filepath.Join(f.Dir, "calls.log"))
	return strings.FieldsFunc(strings.TrimSpace(string(b)), func(r rune) bool { return r == '\n' })
}

func (f fbShim) Flashed() []string {
	var out []string
	for _, call := range f.Calls() {
		if strings.HasPrefix(call, "flash ") {
			out = append(out, call)
		}
	}
	return out
}
