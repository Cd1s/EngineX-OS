package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	consoleSessionTTL   = 30 * 24 * time.Hour
	maxConsoleSessions  = 64
	consoleSessionsFile = "console-sessions.json"
	consoleTokenPrefix  = "s."
)

// consoleSessions are the console sign-ins. Each browser that signs in gets a random token in
// its cookie, so the access code itself crosses the network only on the sign-in form. Only a
// SHA-256 of each token is kept, in the state directory, so a restart keeps browsers signed
// in. A sign-in lasts 30 days; the oldest is dropped beyond 64.
type consoleSessions struct {
	mu     sync.Mutex
	loaded bool
	m      map[string]time.Time // hex SHA-256 of the token -> expiry
}

func consoleTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (c *consoleSessions) loadLocked(dir string) {
	if c.loaded {
		return
	}
	c.loaded, c.m = true, map[string]time.Time{}
	if dir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(dir, consoleSessionsFile)); err == nil && len(b) <= 64<<10 {
		var saved map[string]time.Time
		if json.Unmarshal(b, &saved) == nil {
			for h, exp := range saved {
				if len(h) == sha256.Size*2 && len(c.m) < maxConsoleSessions {
					c.m[h] = exp
				}
			}
		}
	}
}

func (c *consoleSessions) saveLocked(dir string) {
	if dir == "" {
		return
	}
	b, err := json.Marshal(c.m)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".console-sessions-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, consoleSessionsFile))
	}
	if err != nil {
		os.Remove(tmp)
	}
}

// issue returns a new session token.
func (c *consoleSessions) issue(dir string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := consoleTokenPrefix + hex.EncodeToString(raw)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked(dir)
	for h, exp := range c.m {
		if now.After(exp) {
			delete(c.m, h)
		}
	}
	for len(c.m) >= maxConsoleSessions {
		oldest := ""
		for h, exp := range c.m {
			if oldest == "" || exp.Before(c.m[oldest]) {
				oldest = h
			}
		}
		delete(c.m, oldest)
	}
	c.m[consoleTokenHash(token)] = now.Add(consoleSessionTTL)
	c.saveLocked(dir)
	return token, nil
}

// valid reports whether token is a current session token.
func (c *consoleSessions) valid(dir, token string) bool {
	if !strings.HasPrefix(token, consoleTokenPrefix) || len(token) != len(consoleTokenPrefix)+64 {
		return false
	}
	h := consoleTokenHash(token)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked(dir)
	exp, ok := c.m[h]
	if ok && time.Now().After(exp) {
		delete(c.m, h)
		c.saveLocked(dir)
		return false
	}
	return ok
}
