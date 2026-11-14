//go:build (!linux && !darwin && !windows) || ((linux || darwin) && !amd64 && !arm64) || (windows && !amd64)

package usb

import "errors"

func NewBackend() (Backend, error) {
	return nil, fault("initialize", Unsupported, false, errors.New("native USB backend is unavailable on this target"))
}
