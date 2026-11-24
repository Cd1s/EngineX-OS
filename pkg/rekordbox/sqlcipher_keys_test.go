package rekordbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyCacheInvalidatesWrongSharedKeyAfterPageOneCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENGINEX_RB_SHARED_KEY_CACHE", dir)
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	salt, err := ReadSQLCipherSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	name := sharedKeyName(salt, []byte(oneLibraryPassphrase))
	wrong := make([]byte, 64)
	if err := os.WriteFile(filepath.Join(dir, name), wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewKeyCache()
	if _, err := ReadSQLiteKeys(context.Background(), path, nil, cache); err != nil {
		t.Fatalf("read with a corrupt shared key: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(wrong) {
		t.Fatal("corrupt shared key was retained after page-one verification")
	}
}

func TestKeyCacheSharesDerivedKeyAcrossCacheInstances(t *testing.T) {
	if os.Getenv("KEYCACHE_HELPER") != "" {
		cache := NewKeyCache()
		plain, err := os.Open(filepath.Join("testdata", "sqlcipher4_onelibrary_decrypted.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer plain.Close()
		st, err := plain.Stat()
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.CreateTemp(t.TempDir(), "encrypted-*.db")
		if err != nil {
			t.Fatal(err)
		}
		salt := []byte("0123456789abcdef")
		if err := encryptSQLCipher4(context.Background(), plain, st.Size(), out, salt, []byte(oneLibraryPassphrase), cache); err != nil {
			out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
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
