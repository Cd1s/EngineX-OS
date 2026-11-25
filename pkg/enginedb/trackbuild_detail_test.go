package enginedb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestDetailOfFallsBackFromBroken2EXToEXT(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "PIONEER", "USBANLZ", "P001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ANLZ0000.2EX"), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ANLZ0000.EXT"), testfixture.EXT(), 0o644); err != nil {
		t.Fatal(err)
	}
	a := DetailOf(root, []string{"PIONEER/USBANLZ/P001/ANLZ0000.2EX", "PIONEER/USBANLZ/P001/ANLZ0000.EXT"})
	if a == nil || len(a.Waveforms) == 0 {
		t.Fatal("DetailOf did not fall back to the valid .EXT waveform")
	}
}

func TestDetailOfRejectsOversizedDetailFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "PIONEER", "USBANLZ", "P001")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ANLZ0000.2EX"), make([]byte, 16<<20+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetailOf(root, []string{"PIONEER/USBANLZ/P001/ANLZ0000.2EX"}); got != nil {
		t.Fatal("oversized detail file was accepted")
	}
}
