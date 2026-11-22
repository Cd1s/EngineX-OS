package rekordbox

import (
	"crypto/pbkdf2"
	"crypto/sha512"
	"fmt"
	"sync"
	"sync/atomic"
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
}

type cipherKeys struct{ enc, mac []byte }

// NewKeyCache returns an empty cache.
func NewKeyCache() *KeyCache { return &KeyCache{} }

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

// done clears keys that no cache keeps.
func (c *KeyCache) done(k *cipherKeys) {
	if c == nil && k != nil {
		clear(k.enc)
		clear(k.mac)
	}
}
