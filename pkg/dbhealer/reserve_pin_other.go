//go:build !linux

package dbhealer

import "os"

func openInodePin(string) (*os.File, error) {
	return nil, nil
}

func checkReadablePath(string) error {
	return nil
}
