package rekordbox

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// KeyCache keeps the SQLCipher 4 keys derived for an export's salt in memory and in the approved
// bounded shared cache when configured (#321). PBKDF2-HMAC-SHA512 with 256 000 rounds is the
// dearest step of a cue write-back on a
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

type deriveLease struct {
	file *os.File
}

// NewKeyCache returns an empty cache.
func NewKeyCache() *KeyCache {
	sharedDir := os.Getenv("ENGINEX_RB_SHARED_KEY_CACHE")
	if sharedDir == "" && flag.Lookup("test.v") == nil && !strings.HasSuffix(filepath.Base(os.Args[0]), ".test") && os.Geteuid() == 0 {
		const dir = "/run/enginex-rb-keys"
		sharedDir = dir
	}
	return newKeyCache(sharedDir)
}

// NewKeyCacheAt returns an empty cache that uses sharedDir for the approved persistent cache.
// An empty directory keeps keys in memory only.
func NewKeyCacheAt(sharedDir string) *KeyCache {
	return newKeyCache(sharedDir)
}

func newKeyCache(sharedDir string) *KeyCache {
	c := &KeyCache{}
	if sharedDir != "" {
		if err := os.MkdirAll(sharedDir, 0o700); err == nil && os.Chmod(sharedDir, 0o700) == nil {
			c.shared = sharedDir
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

func cloneCipherKeys(k *cipherKeys) *cipherKeys {
	if k == nil {
		return nil
	}
	return &cipherKeys{enc: append([]byte(nil), k.enc...), mac: append([]byte(nil), k.mac...)}
}

func deriveCipherKeys(salt, passphrase []byte) (*cipherKeys, error) {
	if len(salt) != sqlcipherSaltSize {
		return nil, fmt.Errorf("sqlcipher: invalid salt length %d", len(salt))
	}
	derivations.Add(1)
	enc, err := pbkdf2.Key(sha512.New, string(passphrase), salt, sqlcipherKDFIter, 32)
	if err != nil {
		return nil, fmt.Errorf("sqlcipher: derive encryption key: %w", err)
	}
	var macSalt [sqlcipherSaltSize]byte
	for i := range macSalt {
		macSalt[i] = salt[i] ^ sqlcipherHMACSaltMask
	}
	mac, err := pbkdf2.Key(sha512.New, string(enc), macSalt[:], sqlcipherFastKDFIter, 32)
	if err != nil {
		clear(enc)
		return nil, fmt.Errorf("sqlcipher: derive HMAC key: %w", err)
	}
	return &cipherKeys{enc: enc, mac: mac}, nil
}

func (c *KeyCache) remember(id string, k *cipherKeys) {
	if c.keys == nil {
		c.keys = map[string]*cipherKeys{}
	}
	c.keys[id] = k
}

// get returns an independent copy of the keys of salt and passphrase. The cache owns its copy,
// so clearing or invalidating the cache cannot race with a caller that is still using the result.
func (c *KeyCache) get(salt, passphrase []byte) (*cipherKeys, error) {
	k, _, lease, err := c.getWithLease(context.Background(), salt, passphrase)
	lease.release()
	return k, err
}

func (c *KeyCache) getWithLease(ctx context.Context, salt, passphrase []byte) (*cipherKeys, bool, *deriveLease, error) {
	id := string(salt) + "\x00" + string(passphrase)
	if c == nil {
		k, err := deriveCipherKeys(salt, passphrase)
		return k, false, nil, err
	}
	c.mu.Lock()
	if k, ok := c.keys[id]; ok {
		c.mu.Unlock()
		return cloneCipherKeys(k), true, nil, nil
	}
	shared := c.shared
	c.mu.Unlock()
	if shared == "" {
		t0 := time.Now()
		k, err := deriveCipherKeys(salt, passphrase)
		if err != nil {
			return nil, false, nil, err
		}
		c.mu.Lock()
		c.remember(id, k)
		c.derived += time.Since(t0)
		c.mu.Unlock()
		return cloneCipherKeys(k), false, nil, nil
	}
	name := sharedKeyName(salt, passphrase)
	if k, ok := c.readSharedLocked(salt, passphrase); ok {
		c.mu.Lock()
		c.remember(id, k)
		c.mu.Unlock()
		return cloneCipherKeys(k), true, nil, nil
	}
	lease, err := c.claimDerive(ctx, name)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, nil, err
		}
		// Shared storage is an optimization. If it disappears or becomes
		// unwritable, continue with the in-memory cache exactly as a nil shared dir.
		t0 := time.Now()
		k, deriveErr := deriveCipherKeys(salt, passphrase)
		if deriveErr != nil {
			return nil, false, nil, deriveErr
		}
		c.mu.Lock()
		c.remember(id, k)
		c.derived += time.Since(t0)
		c.mu.Unlock()
		return cloneCipherKeys(k), false, nil, nil
	}
	if k, ok := c.readSharedLocked(salt, passphrase); ok {
		lease.release()
		c.mu.Lock()
		c.remember(id, k)
		c.mu.Unlock()
		return cloneCipherKeys(k), true, nil, nil
	}
	t0 := time.Now()
	k, err := deriveCipherKeys(salt, passphrase)
	if err != nil {
		lease.release()
		return nil, false, nil, err
	}
	c.mu.Lock()
	c.remember(id, k)
	c.derived += time.Since(t0)
	c.mu.Unlock()
	return cloneCipherKeys(k), false, lease, nil
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

func (c *KeyCache) readSharedLocked(salt, passphrase []byte) (*cipherKeys, bool) {
	if c == nil || c.shared == "" {
		return nil, false
	}
	lock, err := lockSharedDir(c.shared, syscall.LOCK_SH)
	if err != nil {
		return nil, false
	}
	defer unlockSharedDir(lock)
	return c.readShared(salt, passphrase)
}

func (c *KeyCache) writeShared(salt, passphrase []byte, k *cipherKeys) {
	if c == nil || c.shared == "" || k == nil {
		return
	}
	if st, err := os.Lstat(c.shared); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	lock, err := lockSharedDir(c.shared, syscall.LOCK_EX)
	if err != nil {
		return
	}
	defer unlockSharedDir(lock)
	name := sharedKeyName(salt, passphrase)
	path := filepath.Join(c.shared, name)
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return
	}
	tmpPath := filepath.Join(c.shared, fmt.Sprintf(".%s.%d.%d.tmp", name, os.Getpid(), time.Now().UnixNano()))
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
	c.pruneSharedLocked()
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
	c.mu.Lock()
	if k, ok := c.keys[id]; ok {
		clear(k.enc)
		clear(k.mac)
		delete(c.keys, id)
	}
	shared := c.shared
	c.mu.Unlock()
	if shared != "" {
		lock, err := lockSharedDir(shared, syscall.LOCK_EX)
		if err != nil {
			return
		}
		defer unlockSharedDir(lock)
		path := filepath.Join(shared, sharedKeyName(salt, passphrase))
		if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
			removeSameFile(path, st)
		}
		c.pruneSharedLocked()
	}
}

func (c *KeyCache) claimDerive(ctx context.Context, name string) (*deriveLease, error) {
	path := filepath.Join(c.shared, name+".derive")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, err
		}
		err = flockFile(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &deriveLease{file: f}, nil
		}
		_ = f.Close()
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *deriveLease) release() {
	if l == nil || l.file == nil {
		return
	}
	for {
		err := flockFile(int(l.file.Fd()), syscall.LOCK_UN)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	_ = l.file.Close()
	l.file = nil
}

// done clears keys that no cache keeps.
func (c *KeyCache) done(k *cipherKeys) {
	if k != nil {
		clear(k.enc)
		clear(k.mac)
	}
}

var flockFile = syscall.Flock

func lockSharedDir(dir string, operation int) (*os.File, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	for {
		err = flockFile(int(f.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func unlockSharedDir(f *os.File) {
	if f == nil {
		return
	}
	for {
		err := flockFile(int(f.Fd()), syscall.LOCK_UN)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	_ = f.Close()
}

const maxSharedKeyFiles = 8

func (c *KeyCache) pruneSharedLocked() {
	if c == nil || c.shared == "" {
		return
	}
	entries, err := os.ReadDir(c.shared)
	if err != nil {
		return
	}
	type candidate struct {
		path string
		name string
		when time.Time
		info os.FileInfo
	}
	var files []candidate
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".derive") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			if st, err := entry.Info(); err == nil {
				removeSameFile(filepath.Join(c.shared, entry.Name()), st)
			}
			continue
		}
		st, err := entry.Info()
		if err != nil || !st.Mode().IsRegular() || st.Size() != 64 || filepath.Ext(entry.Name()) != "" {
			continue
		}
		files = append(files, candidate{path: filepath.Join(c.shared, entry.Name()), name: entry.Name(), when: st.ModTime(), info: st})
	}
	if len(files) <= maxSharedKeyFiles {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].when.Equal(files[j].when) {
			return files[i].name < files[j].name
		}
		return files[i].when.Before(files[j].when)
	})
	for _, file := range files[:len(files)-maxSharedKeyFiles] {
		removeSameFile(file.path, file.info)
	}
}

func removeSameFile(path string, expected os.FileInfo) {
	if expected == nil {
		return
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, expected) {
		return
	}
	_ = os.Remove(path)
}
