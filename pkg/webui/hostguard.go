package webui

// Neither the tests nor a web service started off a deck may change the machine
// they run on. r3: an older test wrote the build host's
// /etc/systemd/system/sshd.service.d/51-port.conf, a later run of the service
// outside a sandbox restarted the host's sshd with it, and the host lost SSH.
// A test's pkill of ^/usr/Engine/Engine also ended other jobs' Engines there.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var errHostCommand = errors.New("refused: system commands run only on a deck")

// onDeck: the device tree names a known Denon DJ / Numark / Rane board.
func onDeck() bool { return imagePrefix(deviceCompatible()) != "" }

// guardHostCommand reports whether name may run. In a test binary only a
// stand-in under the temporary directory may (tests put fakes on PATH there);
// in the service only on a deck.
func guardHostCommand(name string) error {
	if testing.Testing() {
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil && strings.HasPrefix(abs, filepath.Clean(os.TempDir())+string(filepath.Separator)) {
				return nil
			}
		}
		return fmt.Errorf("%w (%s in a test: put a stand-in under the temporary directory or mock the runner)", errHostCommand, name)
	}
	if !onDeck() {
		return fmt.Errorf("%w (%s)", errHostCommand, name)
	}
	return nil
}
