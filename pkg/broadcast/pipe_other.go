//go:build !linux

package broadcast

import "os"

// growPipe is a no-op where the pipe capacity cannot be set (development machines).
func growPipe(*os.File) {}
