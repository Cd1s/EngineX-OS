package leave

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestUpgradeMetadataAndLockFailuresStopActivation(t *testing.T) {
	for _, kind := range []string{"journal-stat", "version-read", "lock-open", "lock-flock"} {
		t.Run(kind, func(t *testing.T) {
			d, _ := newDeck(t)
			target := targetBundle(t, "5.3.0.6", "v5.3.0.6")
			stat, read, open, lock := lstat, readFile, openFile, flock
			defer func() { lstat, readFile, openFile, flock = stat, read, open, lock }()
			switch kind {
			case "journal-stat":
				lstat = func(p string) (os.FileInfo, error) {
					if p == d.data(journalName) {
						return nil, syscall.EIO
					}
					return stat(p)
				}
			case "version-read":
				readFile = func(p string) ([]byte, error) {
					if p == d.data("enginex/version.txt") {
						return nil, syscall.EIO
					}
					return read(p)
				}
			case "lock-open":
				openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, syscall.EIO }
			case "lock-flock":
				flock = func(int, int) error { return syscall.EIO }
			}
			if err := d.Install(target); !errors.Is(err, syscall.EIO) {
				t.Fatalf("failure was not preserved: %v", err)
			}
			b, _ := os.ReadFile(d.data("enginex/version.txt"))
			if string(b) != "5.3.0.5\n" {
				t.Fatalf("live version changed: %q", b)
			}
		})
	}
}
