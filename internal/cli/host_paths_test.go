package cli

import (
	"strings"
	"testing"
)

// The package's tests run as root on shared build hosts: the markers and the import lock must point
// into the private directory of TestMain, not into the host's /run (86 stray markers, 2026-11-22).
func TestMarkersStayOffTheHost(t *testing.T) {
	for name, p := range map[string]string{"ejectMarkDir": ejectMarkDir, "importLock": importLock} {
		if p == "/run" || strings.HasPrefix(p, "/run/") {
			t.Errorf("%s = %s: this package's tests would write the host's /run", name, p)
		}
	}
}
