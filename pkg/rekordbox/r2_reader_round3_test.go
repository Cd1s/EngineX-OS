package rekordbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
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
		time.Sleep(time.Minute)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRound3KilledDeriverDoesNotDelayNextRead$", "-test.v")
	cmd.Env = append(os.Environ(), "ROUND3_DERIVER_HELPER=1", "ROUND3_DERIVER_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	deadline := time.Now().Add(10 * time.Second)
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
	salt, err := ReadSQLCipherSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	origFlock := flockFile
	var deriveAttempts, deriveContended, deriveAcquired, deriveReleased atomic.Int32
	var deriveFD atomic.Int64
	deriveFD.Store(-1)
	flockFile = func(fd, op int) error {
		err := origFlock(fd, op)
		if op&(syscall.LOCK_EX|syscall.LOCK_NB) == syscall.LOCK_EX|syscall.LOCK_NB {
			deriveAttempts.Add(1)
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				deriveContended.Add(1)
			} else if err == nil {
				deriveAcquired.Add(1)
				deriveFD.Store(int64(fd))
			}
		} else if op == syscall.LOCK_UN && err == nil {
			if deriveFD.CompareAndSwap(int64(fd), -1) {
				deriveReleased.Add(1)
			}
		}
		return err
	}
	defer func() { flockFile = origFlock }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	kc := NewKeyCacheAt(dir)
	if err := decryptSQLCipher4(ctx, f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), kc); err != nil {
		t.Fatal(err)
	}
	if got := deriveAttempts.Load(); got != 1 {
		t.Fatalf("derive lock attempts = %d, want 1 (immediate non-blocking recovery)", got)
	}
	if n := deriveContended.Load(); n != 0 {
		t.Fatalf("next read contended %d times on killed deriver's .derive lock", n)
	}
	if got := deriveAcquired.Load(); got != 1 {
		t.Fatalf("derive lock acquisitions = %d, want 1", got)
	}
	if got := deriveReleased.Load(); got != 1 {
		t.Fatalf("derive lock releases = %d, want 1", got)
	}

	lockPath := filepath.Join(dir, sharedKeyName(salt, []byte(oneLibraryPassphrase))+".derive")
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open recovered .derive lock file: %v", err)
	}
	if err := origFlock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = probe.Close()
		t.Fatalf(".derive lock remained held after decrypt returned: %v", err)
	}
	_ = origFlock(int(probe.Fd()), syscall.LOCK_UN)
	_ = probe.Close()

	gotKey, ok := NewKeyCacheAt(dir).readShared(salt, []byte(oneLibraryPassphrase))
	if !ok || gotKey == nil || len(gotKey.enc) != 32 || len(gotKey.mac) != 32 {
		t.Fatalf("shared key cache was not populated with valid 64-byte key after killed deriver (ok=%v)", ok)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	derivBefore := KeyDerivations()
	if err := decryptSQLCipher4(ctx, f, st.Size(), io.Discard, []byte(oneLibraryPassphrase), NewKeyCacheAt(dir)); err != nil {
		t.Fatalf("second read with recovered shared key failed: %v", err)
	}
	if delta := KeyDerivations() - derivBefore; delta != 0 {
		t.Fatalf("second read re-derived key %d times after recovery, want 0", delta)
	}
	if got := deriveAttempts.Load(); got != 1 {
		t.Fatalf("second read attempted .derive lock %d total times, want 1 (shared cache hit)", got)
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
