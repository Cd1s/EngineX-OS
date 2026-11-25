package rekordbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRound3WaiterHonorsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	salt, err := ReadSQLCipherSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, sharedKeyName(salt, []byte(oneLibraryPassphrase))+".derive")
	holder, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		holder.Close()
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
		_ = holder.Close()
	}()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = decryptSQLCipher4(ctx, f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), NewKeyCacheAt(dir))
	if err == nil || !containsContextError(err) {
		t.Fatalf("decrypt error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("cancelled waiter took %s", elapsed)
	}
}

func containsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func TestRound3KilledDeriverDoesNotDelayNextRead(t *testing.T) {
	if os.Getenv("ROUND3_DERIVER_HELPER") == "1" {
		dir := os.Getenv("ROUND3_DERIVER_DIR")
		path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
		salt, err := ReadSQLCipherSalt(path)
		if err != nil {
			os.Exit(2)
		}
		lockPath := filepath.Join(dir, sharedKeyName(salt, []byte(oneLibraryPassphrase))+".derive")
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil || syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
			os.Exit(4)
		}
		_, _ = deriveCipherKeys(salt, []byte(oneLibraryPassphrase))
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRound3KilledDeriverDoesNotDelayNextRead$", "-test.v")
	cmd.Env = append(os.Environ(), "ROUND3_DERIVER_HELPER=1", "ROUND3_DERIVER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("deriver helper did not acquire its lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := decryptSQLCipher4(context.Background(), f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), NewKeyCacheAt(dir)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("next read waited %s after killed deriver", elapsed)
	}
}

func TestRound3MissingSharedDirectoryFallsBackToMemory(t *testing.T) {
	dir := t.TempDir()
	kc := NewKeyCacheAt(dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := decryptSQLCipher4(context.Background(), f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), kc); err != nil {
		t.Fatalf("missing shared directory broke decryption: %v", err)
	}
}
