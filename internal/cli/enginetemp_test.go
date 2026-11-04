package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCleanEngineTempKeepsOpenAndCurrentCopies(t *testing.T) {
	dir, proc := t.TempDir(), t.TempDir()
	boot := time.Now().Add(-time.Hour).Truncate(time.Second)
	started := boot.Add(30 * time.Minute) // Engine started 30 minutes ago
	write := func(name string, size int, mod time.Time) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, make([]byte, size), 0o644)
		os.Chtimes(p, mod, mod)
		return p
	}
	leftover := write("a_Song.mp3", 100, started.Add(-time.Minute))
	leftoverOpen := write("b_Song.mp3", 100, started.Add(-time.Minute))
	older := write("c_Other.flac", 100, started.Add(time.Minute))
	newer := write("d_Other.flac", 100, started.Add(2*time.Minute))

	os.WriteFile(filepath.Join(proc, "stat"), []byte("cpu 1 2 3\nbtime "+strconv.FormatInt(boot.Unix(), 10)+"\n"), 0o644)
	engine := filepath.Join(proc, "321")
	os.MkdirAll(filepath.Join(engine, "fd"), 0o755)
	os.WriteFile(filepath.Join(engine, "cmdline"), []byte("/usr/Engine/Engine\x00-x\x00"), 0o644)
	ticks := strconv.Itoa(30 * 60 * 100)
	os.WriteFile(filepath.Join(engine, "stat"), []byte("321 (EMain) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 9 0 "+ticks+" 0 0"), 0o644)
	os.Symlink(leftoverOpen, filepath.Join(engine, "fd", "7"))

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	// Under budget: only the closed copy from an earlier Engine run goes.
	if n, _ := cleanEngineTemp(dir, proc, 1<<20, 0); n != 1 || exists(leftover) || !exists(leftoverOpen) || !exists(older) || !exists(newer) {
		t.Fatalf("removed %d", n)
	}
	// Over budget: the oldest closed copy of this run goes, the open one stays.
	if n, _ := cleanEngineTemp(dir, proc, 250, 0); n != 1 || exists(older) || !exists(newer) || !exists(leftoverOpen) {
		t.Fatalf("removed %d", n)
	}
	// Engine gone: every closed copy is a leftover.
	os.Remove(filepath.Join(engine, "cmdline"))
	if n, _ := cleanEngineTemp(dir, proc, 1<<20, 0); n != 1 || exists(newer) || !exists(leftoverOpen) {
		t.Fatalf("removed %d", n)
	}
}
