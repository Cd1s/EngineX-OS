package azupdate_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/azupdate"
)

const usbTransferLimit = 1 << 20

func TestNewRejectsOversizedUSBChunkBeforeIO(t *testing.T) {
	dev := &fakeAZDevice{}
	client, err := azupdate.New(dev, proto.Endpoints{In: 0x81, Out: 1}, azupdate.WithChunkSize(usbTransferLimit+1))
	if err == nil || client != nil {
		t.Fatal("oversized USB chunk accepted")
	}
	if dev.writeCall != 0 || dev.readCall != 0 {
		t.Fatal("invalid configuration performed USB I/O")
	}
}

func TestWriteDataAtUSBTransferLimitKeepsFinalZLP(t *testing.T) {
	dev := &fakeAZDevice{replies: [][]byte{
		buildReply(azupdate.CmdWritePart, 1, azupdate.StatusReady, 24),
		buildReply(azupdate.CmdWritePart, 1, azupdate.StatusWritten, 24),
	}}
	client, err := azupdate.New(dev, proto.Endpoints{In: 0x81, Out: 1}, azupdate.WithChunkSize(usbTransferLimit))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x5d}, 2*usbTransferLimit+32)
	if _, err := client.WriteData(context.Background(), proto.Image{Data: bytes.NewReader(payload), Size: int64(len(payload))}, nil); err != nil {
		t.Fatal(err)
	}
	if len(dev.writes) != 5 || len(dev.writes[len(dev.writes)-1]) != 0 {
		t.Fatal("expected header, three data transfers and one final ZLP")
	}
	var streamed bytes.Buffer
	for _, data := range dev.writes[1 : len(dev.writes)-1] {
		if len(data) == 0 || len(data) > usbTransferLimit {
			t.Fatalf("invalid data transfer length %d", len(data))
		}
		streamed.Write(data)
	}
	if !bytes.Equal(streamed.Bytes(), payload) {
		t.Fatal("bounded writes changed the payload")
	}
}
