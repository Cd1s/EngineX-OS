package bridge_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func writeEncryptedExport(t *testing.T, path, plainPath string, salt []byte) {
	t.Helper()
	plain, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	info, err := plain.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rekordbox.EncryptSQLCipher4(context.Background(), plain, info.Size(), out, salt, []byte("r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls")); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMountCachesRekordbox7ExportKeysBySalt(t *testing.T) {
	plainPath := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	root := t.TempDir()
	first := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.db")
	second := filepath.Join(root, "PIONEER", "rekordbox", "exportLibrary.new.db")
	var firstSalt, secondSalt [16]byte
	if _, err := rand.Read(firstSalt[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(secondSalt[:]); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstSalt[:], secondSalt[:]) {
		t.Fatal("generated identical SQLCipher salts")
	}
	writeEncryptedExport(t, first, plainPath, firstSalt[:])
	writeEncryptedExport(t, second, plainPath, secondSalt[:])

	before := rekordbox.KeyDerivations()
	if _, err := bridge.ReadLibrary(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ReadLibrary(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := rekordbox.KeyDerivations() - before; got != 1 {
		t.Fatalf("same export derived %d keys, want 1", got)
	}

	if err := os.Rename(second, first); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.ReadLibrary(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := rekordbox.KeyDerivations() - before; got != 2 {
		t.Fatalf("new export salt derived %d keys total, want 2", got)
	}
}
