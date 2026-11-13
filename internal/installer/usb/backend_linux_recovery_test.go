//go:build linux && (amd64 || arm64)

package usb

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"testing"
	"unsafe"
)

func TestLinuxPollCompletion(t *testing.T) {
	var active *linuxURB
	ready := false
	polls := 0
	device := linuxTestDevice(linuxCalls{ioctl: func(_ int, request uintptr, arg unsafe.Pointer) error {
		switch request {
		case 0x8038550a:
			active = (*linuxURB)(arg)
		case 0x4008550d:
			if !ready {
				return unix.EAGAIN
			}
			active.Actual = 4
			*(**linuxURB)(arg) = active
		}
		return nil
	}})
	device.calls.poll = func(fds []unix.PollFd, _ int) (int, error) {
		polls++
		if fds[0].Fd != 10 || fds[0].Events&unix.POLLOUT == 0 {
			t.Fatal("wrong poll target")
		}
		ready = true
		return 1, nil
	}
	n, err := device.WriteBulk(context.Background(), 2, []byte{1, 2, 3, 4})
	if err != nil || n != 4 || polls != 1 {
		t.Fatalf("bytes=%d polls=%d error=%v", n, polls, err)
	}
}

func TestLinuxNativeTimeoutResetsAfterDrainWithoutReplay(t *testing.T) {
	var active *linuxURB
	drained := false
	submits, resets := 0, 0
	device := linuxTestDevice(linuxCalls{ioctl: func(_ int, request uintptr, arg unsafe.Pointer) error {
		switch request {
		case 0x8038550a:
			active = (*linuxURB)(arg)
			submits++
		case 0x4008550d:
			active.Status = -int32(unix.ETIMEDOUT)
			*(**linuxURB)(arg) = active
			drained = true
		case 0x80045515:
			if !drained || *(*uint32)(arg) != 2 {
				t.Fatal("reset before drain or on wrong pipe")
			}
			resets++
			return unix.EPERM
		}
		return nil
	}})
	_, err := device.WriteBulk(context.Background(), 2, []byte{1})
	var detail *Error
	if !errors.Is(err, unix.ETIMEDOUT) || !errors.Is(err, unix.EPERM) || !errors.As(err, &detail) || !detail.MayHaveWritten || submits != 1 || resets != 1 {
		t.Fatalf("error=%v submits=%d resets=%d", err, submits, resets)
	}
}
