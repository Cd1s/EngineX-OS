package fastboot_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/fastboot"
)

const usbTransferLimit = 1 << 20

func TestNewRejectsOversizedUSBOptionsBeforeIO(t *testing.T) {
	for _, test := range []struct {
		name string
		opt  fastboot.Option
	}{
		{"data chunk", fastboot.WithChunkSize(usbTransferLimit + 1)},
		{"command", fastboot.WithMaxCommandLength(usbTransferLimit + 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			dev := newFakeFastbootDevice()
			client, err := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 1}, test.opt)
			if err == nil || client != nil {
				t.Fatal("oversized USB option accepted")
			}
			if dev.writeCalls != 0 || dev.readCalls != 0 {
				t.Fatal("invalid configuration performed USB I/O")
			}
		})
	}
}

type cappedFastbootDevice struct {
	*fakeFastbootDevice
	largestWrite int
}

func (d *cappedFastbootDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	d.largestWrite = max(d.largestWrite, len(data))
	if len(data) > usbTransferLimit {
		return 0, fmt.Errorf("USB write exceeds %d bytes", usbTransferLimit)
	}
	return d.fakeFastbootDevice.WriteBulk(ctx, ep, data)
}

func TestDownloadStreamsAtUSBTransferLimit(t *testing.T) {
	dev := &cappedFastbootDevice{fakeFastbootDevice: newFakeFastbootDevice()}
	client, err := fastboot.New(dev, proto.Endpoints{In: 0x81, Out: 1}, fastboot.WithChunkSize(usbTransferLimit))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x73}, 2*usbTransferLimit+17)
	if err := client.Download(context.Background(), bytes.NewReader(payload), int64(len(payload)), nil); err != nil {
		t.Fatal(err)
	}
	if dev.largestWrite != usbTransferLimit || !bytes.Equal(dev.staged.Bytes(), payload) {
		t.Fatal("bounded download changed the payload or transfer boundary")
	}
}
