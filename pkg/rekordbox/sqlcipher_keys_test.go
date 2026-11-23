package rekordbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyCacheSharesDerivedKeyAcrossCacheInstances(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", dir)
	salt := []byte("0123456789abcdef")
	passphrase := []byte("shared-cache-test")
	before := KeyDerivations()
	first := NewKeyCache()
	if _, err := first.get(salt, passphrase); err != nil {
		t.Fatal(err)
	}
	second := NewKeyCache()
	if _, err := second.get(salt, passphrase); err != nil {
		t.Fatal(err)
	}
	if got := KeyDerivations() - before; got != 1 {
		t.Fatalf("derived %d keys, want one shared derivation", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var keyPath string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == "" {
			keyPath = filepath.Join(dir, entry.Name())
			break
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
