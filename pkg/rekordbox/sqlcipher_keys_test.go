package rekordbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyCacheSharesDerivedKeyAcrossCacheInstances(t *testing.T) {
	if os.Getenv("KEYCACHE_HELPER") != "" {
		cache := NewKeyCache()
		if _, err := cache.get([]byte("0123456789abcdef"), []byte("shared-cache-test")); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("derivations=%d\n", KeyDerivations())
		return
	}
	dir := t.TempDir()
	for i, want := range []string{"derivations=1", "derivations=0"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestKeyCacheSharesDerivedKeyAcrossCacheInstances$", "-test.v")
		cmd.Env = append(os.Environ(), "KEYCACHE_HELPER=1", "ENGINEX_RB_SHARED_KEY_CACHE="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper %d: %v\n%s", i+1, err, out)
		}
		if !strings.Contains(string(out), want) {
			t.Fatalf("helper %d output=%q, want %q", i+1, out, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var keyPath string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == "" {
			candidate := filepath.Join(dir, entry.Name())
			if st, statErr := os.Stat(candidate); statErr == nil && st.Size() == 64 {
				keyPath = candidate
				break
			}
		}
	}
	if keyPath == "" {
		t.Fatal("shared key file was not written")
	}
	if mode := mustStat(t, keyPath).Mode().Perm(); mode != 0o600 {
		t.Fatalf("shared key mode=%04o, want 0600", mode)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
