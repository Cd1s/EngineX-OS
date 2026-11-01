//go:build !linux

package resourceguard

import "errors"

func applyCurrentThread(cfg Config) Report {
	if cfg.Disabled {
		return Report{Nice: -1, Degraded: []string{"resource guard disabled"}}
	}
	return Report{Nice: -1, Degraded: []string{"resource guard requires linux"}}
}

func applyProcess(cfg Config, pid int) ([]Report, error) {
	if cfg.Disabled {
		return []Report{applyCurrentThread(cfg)}, nil
	}
	return nil, errors.New("resource guard requires linux")
}
