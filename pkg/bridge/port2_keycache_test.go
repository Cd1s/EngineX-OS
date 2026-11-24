package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func p2WriteEncryptedExport(t *testing.T, dst, plainPath string, salt []byte) {
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
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := rekordbox.EncryptSQLCipher4(context.Background(), plain, info.Size(), out, salt, []byte("r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls")); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPort2ReadSourcesCachesOneLibraryKeysBySalt(t *testing.T) {
	plainPath := filepath.Join("..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	mount := t.TempDir()
	first := filepath.Join(mount, "PIONEER/rekordbox/exportLibrary.db")
	second := filepath.Join(mount, "PIONEER/rekordbox/exportLibrary.new.db")
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
	p2WriteEncryptedExport(t, first, plainPath, firstSalt[:])
	p2WriteEncryptedExport(t, second, plainPath, secondSalt[:])
	r, err := compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err := FindSources(r, mount)
	if err != nil {
		t.Fatal(err)
	}
	cache := rekordbox.NewKeyCache()
	before := rekordbox.KeyDerivations()
	if _, failed, err := ReadSources(context.Background(), r, mount, files, cache); err != nil || len(failed) != 0 {
		t.Fatalf("first read: %v %v", failed, err)
	}
	if _, failed, err := ReadSources(context.Background(), r, mount, files, cache); err != nil || len(failed) != 0 {
		t.Fatalf("cached read: %v %v", failed, err)
	}
	if got := rekordbox.KeyDerivations() - before; got != 1 {
		t.Fatalf("same salt derived %d keys, want 1", got)
	}
	if err := os.Rename(second, first); err != nil {
		t.Fatal(err)
	}
	r, err = compat.NewResolver(mount)
	if err != nil {
		t.Fatal(err)
	}
	files, err = FindSources(r, mount)
	if err != nil {
		t.Fatal(err)
	}
	if _, failed, err := ReadSources(context.Background(), r, mount, files, cache); err != nil || len(failed) != 0 {
		t.Fatalf("new salt read: %v %v", failed, err)
	}
	if got := rekordbox.KeyDerivations() - before; got != 2 {
		t.Fatalf("new salt derived %d keys total, want 2", got)
	}
}
