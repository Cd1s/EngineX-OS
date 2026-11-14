package proto_test

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/azupdate"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/fastboot"
)

func TestConstructorsRejectOversizedUSBOptions(t *testing.T) {
	const oversized = (1 << 20) + 1
	// Constructors must not call the embedded, nil transport.
	dev := struct{ proto.BulkDevice }{}
	eps := proto.Endpoints{In: 0x81, Out: 1}
	for _, test := range []struct {
		name string
		new  func() error
	}{
		{"fastboot chunk", func() error {
			_, err := fastboot.New(dev, eps, fastboot.WithChunkSize(oversized))
			return err
		}},
		{"fastboot command", func() error {
			_, err := fastboot.New(dev, eps, fastboot.WithMaxCommandLength(oversized))
			return err
		}},
		{"AZ chunk", func() error {
			_, err := azupdate.New(dev, eps, azupdate.WithChunkSize(oversized))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.new(); err == nil {
				t.Fatal("oversized USB option accepted")
			}
		})
	}
}
