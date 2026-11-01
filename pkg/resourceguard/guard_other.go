//go:build !linux

package resourceguard

import "errors"

func applyCurrentThread(cfg Config) Report {
	return Report{Nice: -1, Degraded: []string{"resource guard requires linux"}}
}

func applyProcess(cfg Config, pid int) ([]Report, error) {
	return nil, errors.New("resource guard requires linux")
}
