package rekordbox

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// KeyCache keeps the SQLCipher 4 keys derived for an export's salt in memory, never on disk
// (#321). PBKDF2-HMAC-SHA512 with 256 000 rounds is the dearest step of a cue write-back on a
// deck: with one cache a run decrypts and encrypts with a single derivation, and a long-lived
// caller (the stick's watcher in stream-sync) derives once per export it sees. rekordbox keeps
// an export's salt when it writes the file again, and so does EncryptSQLCipher4's caller, so
// a new salt means a new export. A nil *KeyCache derives every time, as before.
type KeyCache struct {
	mu      sync.Mutex
	keys    map[string]*cipherKeys
	derived time.Duration
	shared  string
}

type cipherKeys struct{ enc, mac []byte }

// NewKeyCache returns an empty cache.
func NewKeyCache() *KeyCache {
	c := &KeyCache{}
	if sharedDir := os.Getenv("ENGINEX_RB_SHARED_KEY_CACHE"); sharedDir != "" {
		if err := os.MkdirAll(sharedDir, 0o700); err == nil && os.Chmod(sharedDir, 0o700) == nil {
			c.shared = sharedDir
		}
	} else if flag.Lookup("test.v") == nil && !strings.HasSuffix(filepath.Base(os.Args[0]), ".test") && os.Geteuid() == 0 {
		const dir = "/run/enginex-rb-keys"
		if err := os.MkdirAll(dir, 0o700); err == nil && os.Chmod(dir, 0o700) == nil {
			c.shared = dir
		}
	}
	return c
}

// derivations counts the 256 000-round derivations of this process (tests, logs).
var derivations atomic.Int64

// KeyDerivations is the number of 256 000-round key derivations this process has made.
func KeyDerivations() int64 { return derivations.Load() }

// DeriveTime is the time this cache spent deriving keys.
func (c *KeyCache) DeriveTime() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.derived
}

// Clear forgets every key.
func (c *KeyCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.keys {
		clear(k.enc)
		clear(k.mac)
	}
	c.keys = nil
}

// get returns the keys of salt and passphrase, deriving them once. The lock is held while
// deriving, so two callers of one cache never derive the same keys twice.
func (c *KeyCache) get(salt, passphrase []byte) (*cipherKeys, error) {
	id := string(salt) + "\x00" + string(passphrase)
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if k, ok := c.keys[id]; ok {
			return k, nil
		}
		if c.shared != "" {
			if k, ok := c.readShared(salt, passphrase); ok {
				if c.keys == nil {
					c.keys = map[string]*cipherKeys{}
				}
				c.keys[id] = k
				return k, nil
			}
		}
	}
	var lock *os.File
	if c != nil && c.shared != "" {
		var err error
		lock, err = os.OpenFile(filepath.Join(c.shared, sharedKeyName(salt, passphrase)+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err == nil {
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				lock.Close()
				lock = nil
			}
		}
		if lock != nil {
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				_ = lock.Close()
				lock = nil
			}
		}
		if lock != nil {
			defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
			if k, ok := c.readShared(salt, passphrase); ok {
				if c.keys == nil {
					c.keys = map[string]*cipherKeys{}
				}
				c.keys[id] = k
				return k, nil
			}
		}
	}
	t0 := time.Now()
	derivations.Add(1)
	enc, err := pbkdf2.Key(sha512.New, string(passphrase), salt, sqlcipherKDFIter, 32)
	if err != nil {
		return nil, fmt.Errorf("sqlcipher: derive encryption key: %w", err)
	}
	var macSalt [sqlcipherSaltSize]byte
	for i := 0; i < sqlcipherSaltSize; i++ {
		macSalt[i] = salt[i] ^ sqlcipherHMACSaltMask
	}
	mac, err := pbkdf2.Key(sha512.New, string(enc), macSalt[:], sqlcipherFastKDFIter, 32)
	if err != nil {
		clear(enc)
		return nil, fmt.Errorf("sqlcipher: derive HMAC key: %w", err)
	}
	k := &cipherKeys{enc: enc, mac: mac}
	if c != nil {
		if c.keys == nil {
			c.keys = map[string]*cipherKeys{}
		}
		c.keys[id] = k
		c.derived += time.Since(t0)
	}
	return k, nil
}

func sharedKeyName(salt, passphrase []byte) string {
	h := sha256.New()
	_, _ = h.Write(salt)
	_, _ = h.Write(passphrase)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *KeyCache) readShared(salt, passphrase []byte) (*cipherKeys, bool) {
	if c == nil || c.shared == "" {
		return nil, false
	}
	path := filepath.Join(c.shared, sharedKeyName(salt, passphrase))
	if st, err := os.Lstat(path); err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) != 64 {
		return nil, false
	}
	return &cipherKeys{enc: append([]byte(nil), b[:32]...), mac: append([]byte(nil), b[32:]...)}, true
}

func (c *KeyCache) writeShared(salt, passphrase []byte, k *cipherKeys) {
	if c == nil || c.shared == "" || k == nil {
		return
	}
	if st, err := os.Lstat(c.shared); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	path := filepath.Join(c.shared, sharedKeyName(salt, passphrase))
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return
	}
	tmpPath := filepath.Join(c.shared, fmt.Sprintf(".%s.%d.tmp", sharedKeyName(salt, passphrase), os.Getpid()))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	var b [64]byte
	copy(b[:32], k.enc)
	copy(b[32:], k.mac)
	if n, err := f.Write(b[:]); err != nil || n != len(b) || f.Sync() != nil || f.Close() != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return
	}
	ok = true
	if dir, err := os.Open(c.shared); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
}

func (c *KeyCache) invalidate(salt, passphrase []byte) {
	if c == nil {
		return
	}
	id := string(salt) + "\x00" + string(passphrase)
	if k, ok := c.keys[id]; ok {
		clear(k.enc)
		clear(k.mac)
		delete(c.keys, id)
	}
	if c.shared != "" {
		path := filepath.Join(c.shared, sharedKeyName(salt, passphrase))
		if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
			_ = os.Remove(path)
		}
	}
}

// done clears keys that no cache keeps.
func (c *KeyCache) done(k *cipherKeys) {
	if c == nil && k != nil {
		clear(k.enc)
		clear(k.mac)
	}
}
