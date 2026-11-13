package testfixture

import (
	"os"
	"testing"
)

// TestGateExport writes the fixture Rekordbox USB export that
// tools/az01-packer/release-gate.sh converts with the image's rb-bridge.
func TestGateExport(t *testing.T) {
	dir := os.Getenv("ENGINEX_GATE_EXPORT")
	if dir == "" {
		t.Skip("set ENGINEX_GATE_EXPORT to write the release-gate fixture")
	}
	Export(t, dir)
}
