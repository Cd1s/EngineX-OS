package mods

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// Discovery reads a manifest only when it is a small regular file: a large file is not read into
// memory, a pipe or a link is skipped without waiting on it.
func TestDiscoveryReadsOnlySmallRegularManifests(t *testing.T) {
	base := t.TempDir()
	media := filepath.Join(base, "media")
	mods := filepath.Join(media, "USB", "mods")
	mk := func(name string) string {
		d := filepath.Join(mods, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if err := os.WriteFile(filepath.Join(mk("good"), "manifest.json"), []byte(`{"id":"good","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	big, err := os.Create(filepath.Join(mk("big"), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	big.Close()
	if err := syscall.Mkfifo(filepath.Join(mk("pipe"), "manifest.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.json")
	os.WriteFile(outside, []byte(`{"id":"linked","version":"1.0.0"}`), 0o644)
	if err := os.Symlink(outside, filepath.Join(mk("linked"), "manifest.json")); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(media, filepath.Join(base, "ui"))
	type result struct {
		list []Mod
		err  error
	}
	done := make(chan result, 1)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	go func() { l, err := mgr.Discover(); done <- result{l, err} }()
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		os.Remove(filepath.Join(mods, "pipe", "manifest.json")) // let a blocked open go
		t.Fatal("discovery is still waiting on a manifest")
	}
	runtime.ReadMemStats(&after)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Fatalf("discovery allocated %d bytes", n)
	}
	for _, m := range r.list {
		if m.ID == "linked" || m.ID == "pipe" || (m.ID == "big" && m.Valid) {
			t.Fatalf("listed %+v", m)
		}
	}
	found := false
	for _, m := range r.list {
		found = found || m.ID == "good"
	}
	if !found {
		t.Fatalf("the regular mod is missing: %+v", r.list)
	}
}
