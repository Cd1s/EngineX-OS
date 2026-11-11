package providers

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var spotifyIdentityMu sync.Mutex
var spotifyMachineIdentity = sync.OnceValue(func() string {
	machine, _ := os.ReadFile("/etc/machine-id")
	host, _ := os.Hostname()
	if len(machine) == 0 && host == "" {
		b := make([]byte, 20)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return hex.EncodeToString(b)
	}
	return SpotifyDeviceID(strings.TrimSpace(string(machine)) + ":" + host)
})

// LoadSpotifyDeviceID gives each deck its own identity, shared by AP and Zeroconf.
// O_CREATE|O_EXCL creates a 0600 file on exFAT/vfat as well as POSIX filesystems
// without ever overwriting an existing identity.
func LoadSpotifyDeviceID(stateDir string) (string, error) {
	if stateDir == "" {
		return spotifyMachineIdentity(), nil
	}
	spotifyIdentityMu.Lock()
	defer spotifyIdentityMu.Unlock()
	path := filepath.Join(stateDir, "spotify-device-id")
	read := func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(string(b))
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 20 || id != strings.ToLower(id) {
			return "", errors.New("spotify: invalid persisted device identity")
		}
		if info, statErr := os.Stat(path); statErr != nil {
			return "", statErr
		} else if info.Mode().Perm() != 0o600 {
			if chmodErr := os.Chmod(path, 0o600); chmodErr != nil && !errors.Is(chmodErr, syscall.EPERM) && !errors.Is(chmodErr, syscall.EOPNOTSUPP) {
				return "", chmodErr
			}
		}
		return id, nil
	}
	if id, err := read(); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		for attempt := 0; attempt < 5; attempt++ {
			if id, readErr := read(); readErr == nil {
				return id, nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return read()
	}
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(hex.EncodeToString(b) + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return read()
}

func SpotifyDeviceID(seed string) string {
	h := sha1.Sum([]byte("enginex-os-spotify:" + seed))
	return hex.EncodeToString(h[:])
}
