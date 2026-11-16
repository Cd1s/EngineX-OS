//go:build windows && amd64

package main

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/Cd1s/EngineX-OS/internal/installer/usb"
)

// The elevated child configures one live attachment and exits. It never runs
// discovery, image preparation or a firmware write.
func init() {
	if len(os.Args) != 4 || os.Args[1] != usb.WindowsDriverSetupArgument {
		return
	}
	if err := usb.ConfigureWindowsDriver(context.Background(), os.Args[2], os.Args[3]); err != nil {
		var code syscall.Errno
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		os.Exit(1)
	}
	os.Exit(0)
}
