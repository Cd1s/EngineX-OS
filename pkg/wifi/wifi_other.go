//go:build !linux

package wifi

import "errors"

// GetPowerSave returns whether Wi-Fi power saving is currently enabled.
func GetPowerSave(ifname string) (bool, error) {
	return false, errors.New("wifi: power save operations are only supported on linux")
}

// SetPowerSave switches Wi-Fi power saving on or off on the given interface.
func SetPowerSave(ifname string, enable bool) error {
	return errors.New("wifi: power save operations are only supported on linux")
}
