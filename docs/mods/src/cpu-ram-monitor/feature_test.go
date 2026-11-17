package cpuram

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCPUAndRAMStatus(t *testing.T) {
	dir := t.TempDir()
	statPath := filepath.Join(dir, "stat")
	memPath := filepath.Join(dir, "meminfo")

	if err := os.WriteFile(statPath, []byte("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 50 0 25 400 25 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memPath, []byte("MemTotal:       2048000 kB\nMemFree:         512000 kB\nMemAvailable:   1024000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := &feature{statPath: statPath, meminfoPath: memPath}
	got, err := f.Status()
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["cpuPercent"].(float64) != 0 {
		t.Fatalf("first sample cpuPercent = %v, want 0", m["cpuPercent"])
	}
	if m["ramPercent"].(float64) != 50 {
		t.Fatalf("ramPercent = %v, want 50", m["ramPercent"])
	}
	if m["ramUsedMB"].(uint64) != 1000 || m["ramTotalMB"].(uint64) != 2000 {
		t.Fatalf("ram MB = %v/%v, want 1000/2000", m["ramUsedMB"], m["ramTotalMB"])
	}

	// Second sample: total +200 (1000 -> 1200), idle +50 (850 -> 900) => 75.0% busy.
	if err := os.WriteFile(statPath, []byte("cpu  200 0 100 850 50 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got2, err := f.Status()
	if err != nil {
		t.Fatal(err)
	}
	if got2.(map[string]any)["cpuPercent"].(float64) != 75 {
		t.Fatalf("second sample cpuPercent = %v, want 75", got2.(map[string]any)["cpuPercent"])
	}
}

func TestMemInfoFallbackWithoutMemAvailable(t *testing.T) {
	dir := t.TempDir()
	memPath := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(memPath, []byte("MemTotal: 1000 kB\nMemFree: 200 kB\nBuffers: 100 kB\nCached: 300 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	total, used, err := readMemInfo(memPath)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1000 || used != 400 {
		t.Fatalf("got total=%d used=%d, want 1000/400", total, used)
	}
}
