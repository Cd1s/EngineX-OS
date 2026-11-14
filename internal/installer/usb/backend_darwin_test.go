//go:build darwin && (amd64 || arm64)

package usb

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

func TestDarwinNativeUUIDABI(t *testing.T) {
	a, err := loadDarwin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.uuid("01234567-89AB-CDEF-1032-547698BADCFE")
	if err != nil {
		t.Fatal(err)
	}
	defer a.cfRelease(id)
	want := uuidBytes{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe}
	if got := a.uuidBytes(id); got != want {
		t.Fatalf("CFUUIDBytes ABI mismatch: %+v", got)
	}
	roundtrip := a.uuidFromBytes(0, want)
	if roundtrip == 0 {
		t.Fatal("CFUUIDBytes argument ABI returned nil")
	}
	defer a.cfRelease(roundtrip)
	if got := a.uuidBytes(roundtrip); got != want {
		t.Fatalf("CFUUIDBytes argument ABI mismatch: %+v", got)
	}
	lo, hi := uuidWords(want)
	if lo != 0xefcdab8967452301 || hi != 0xfedcba9876543210 {
		t.Fatalf("UUID register halves swapped/truncated: %016x %016x", lo, hi)
	}
	if unsafe.Sizeof(darwinRequest{}) != 32 || unsafe.Offsetof(darwinRequest{}.Data) != 8 || unsafe.Offsetof(darwinRequest{}.Done) != 16 {
		t.Fatal("IOUSBDevRequestTO ABI mismatch")
	}
}

// Creating/querying a plugin does not open/claim the USB interface. This proves
// QueryInterface's by-value UUID argument against Apple's actual COM object.
func TestDarwinNativeQueryInterfaceABI(t *testing.T) {
	target := os.Getenv("ENGINEX_USB_TEST_REGISTRY_ID")
	if target == "" {
		t.Skip("requires a PM-reserved vendor update interface registry ID; unrelated interfaces are never claimed")
	}
	targetID, err := strconv.ParseUint(target, 0, 64)
	if err != nil {
		t.Fatal("invalid test registry ID")
	}
	a, err := loadDarwin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	queried := 0
	seen := map[uint64]bool{}
	var failures []error
	for _, class := range []string{"IOUSBHostInterface", "IOUSBInterface"} {
		err := a.walk(ctx, class, func(service uint32) error {
			var id uint64
			if a.entryID(service, &id) != 0 || seen[id] {
				return nil
			}
			seen[id] = true
			if id != targetID {
				return nil
			}
			class, ok := a.number(service, "bInterfaceClass")
			if !ok || class != 0xff {
				return errors.New("reserved test service is not a vendor update interface")
			}
			iface, err := a.queryInterface(service)
			if err != nil {
				failures = append(failures, err)
				return nil
			}
			defer releaseCOM(iface)
			number, err := comByte(iface, 17)
			if err != nil {
				return err
			}
			if n, ok := a.number(service, "bInterfaceNumber"); ok && uint64(number) != n {
				return errors.New("native interface number disagrees with registry")
			}
			if _, err := comWord(iface, 13); err != nil {
				return err
			}
			queried++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if queried == 0 {
		if len(failures) > 0 {
			t.Fatal(errors.Join(failures...))
		}
		t.Skip("no USB interface service; native QueryInterface and open/close need a USB device")
	}
	t.Logf("queried %d native USB COM interfaces without claiming them", queried)
}

func TestDarwinInjectedBulkZLPAndShortRead(t *testing.T) {
	var table [64]uintptr
	object := &darwinCOM{Table: &table}
	writes := 0
	overrun := false
	table[40] = purego.NewCallback(func(_ *darwinCOM, pipe uint8, buffer unsafe.Pointer, size, noData, completion uint32) int32 {
		if pipe != 2 || size != 0 || buffer == nil || noData == 0 || completion == 0 {
			t.Error("incorrect native ZLP call")
		}
		writes++
		return 0
	})
	table[39] = purego.NewCallback(func(_ *darwinCOM, pipe uint8, buffer unsafe.Pointer, size *uint32, _, _ uint32) int32 {
		if overrun {
			*size++
			return 0
		}
		copy(unsafe.Slice((*byte)(buffer), int(*size)), []byte{3, 4, 5})
		*size = 3
		return 0
	})
	it, _ := parseConfiguration(configurationFixture())
	d := &darwinDevice{s: newSession(), object: object, it: it[0], pipes: map[uint8]uint8{2: 2, 0x81: 1}}
	n, err := d.WriteBulk(context.Background(), 2, nil)
	if n != 0 || err != nil || writes != 1 {
		t.Fatalf("n=%d err=%v writes=%d", n, err, writes)
	}
	b := make([]byte, 64)
	n, err = d.ReadBulk(context.Background(), 0x81, b)
	if n != 3 || err != nil || b[2] != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	overrun = true
	_, err = d.ReadBulk(context.Background(), 0x81, b)
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Protocol {
		t.Fatalf("invalid native count accepted: %v", err)
	}
}

func TestDarwinInjectedCancelDrains(t *testing.T) {
	var table [64]uintptr
	object := &darwinCOM{Table: &table}
	aborted := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	table[40] = purego.NewCallback(func(_ *darwinCOM, _ uint8, _ unsafe.Pointer, _ uint32, _, _ uint32) int32 {
		cancel()
		select {
		case <-aborted:
		case <-time.After(time.Second):
			t.Error("pipe was not aborted")
		}
		return -536870165
	})
	table[28] = purego.NewCallback(func(_ *darwinCOM, _ uint8) int32 {
		select {
		case aborted <- struct{}{}:
		default:
		}
		return 0
	})
	table[30] = purego.NewCallback(func(_ *darwinCOM, _ uint8) int32 { return 0 })
	it, _ := parseConfiguration(configurationFixture())
	d := &darwinDevice{s: newSession(), object: object, it: it[0], pipes: map[uint8]uint8{2: 2}}
	_, err := d.WriteBulk(ctx, 2, []byte{1})
	var detail *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &detail) || !detail.MayHaveWritten {
		t.Fatal(err)
	}
}
func TestDarwinNativeTimeoutResetsWithoutReplay(t *testing.T) {
	var table [64]uintptr
	object := &darwinCOM{Table: &table}
	writes, resets := 0, 0
	drained := false
	table[40] = purego.NewCallback(func(_ *darwinCOM, _ uint8, _ unsafe.Pointer, _ uint32, _, _ uint32) int32 {
		writes++
		drained = true
		status := uint32(0xe00002d6)
		return int32(status)
	})
	table[30] = purego.NewCallback(func(_ *darwinCOM, pipe uint8) int32 {
		if !drained || pipe != 2 {
			t.Error("reset before drain or on another pipe")
		}
		resets++
		status := uint32(0xe00002c1)
		return int32(status)
	})
	interfaces, _ := parseConfiguration(configurationFixture())
	device := &darwinDevice{s: newSession(), object: object, it: interfaces[0], pipes: map[uint8]uint8{2: 2}}
	_, err := device.WriteBulk(context.Background(), 2, []byte{1})
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != Timeout || !detail.MayHaveWritten || writes != 1 || resets != 1 {
		t.Fatalf("error=%v writes=%d resets=%d", err, writes, resets)
	}
}
