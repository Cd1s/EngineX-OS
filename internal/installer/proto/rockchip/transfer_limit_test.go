package rockchip_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/rockchip"
)

const usbTransferLimit = 1 << 20

type cappedRockUSBDevice struct {
	*fakeRockUSBDevice
	largestRead int
}

func (d *cappedRockUSBDevice) ReadBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	d.largestRead = max(d.largestRead, len(data))
	if len(data) > usbTransferLimit {
		return 0, fmt.Errorf("USB read exceeds %d bytes", usbTransferLimit)
	}
	return d.fakeRockUSBDevice.ReadBulk(ctx, ep, data)
}

func TestLargeReadbackUsesBoundedUSBTransfers(t *testing.T) {
	disk := buildValidGPTDisk(16384, []gptPartSpec{{name: "rootfs", startLBA: 128, endLBA: 8191}})
	const length = 2*usbTransferLimit + 123
	const start = 128 * rockchip.SectorSize
	for i := 0; i < length; i++ {
		disk[start+i] = byte(i*13 + 7)
	}
	dev := &cappedRockUSBDevice{fakeRockUSBDevice: newFakeRockLoader(disk)}
	client, err := rockchip.New(dev, proto.Endpoints{In: 0x81, Out: 1}, rockchip.WithMaxReadSectors(8192))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := client.Probe(context.Background(), rockchip.VID, rockchip.PIDRK3288)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := client.ReadLayout(context.Background(), probe)
	if err != nil {
		t.Fatal(err)
	}
	target, err := rockchip.PrepareVerifiedTarget(probe, layout, "rootfs", length)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, length)
	if err := client.Readback(context.Background(), target, &memWriterAt{buf: out}, length, nil); err != nil {
		t.Fatal(err)
	}
	if dev.largestRead != usbTransferLimit || !bytes.Equal(out, disk[start:start+length]) {
		t.Fatal("bounded readback changed the payload or transfer boundary")
	}
	for _, opcode := range dev.opcodesSeen {
		if opcode == rockchip.OpWriteLBA {
			t.Fatal("readback emitted a storage write")
		}
	}
}
