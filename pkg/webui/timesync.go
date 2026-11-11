package webui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultTimezoneStatePath = "/data/enginex/state/timezone"
	defaultZoneinfoDir       = "/usr/share/zoneinfo"
)

func (s *Server) timezoneStatePath() string {
	if s.cfg.StateDir != "" && s.cfg.StateDir != "/data/sc5000-custom/state" {
		return filepath.Join(s.cfg.StateDir, "timezone")
	}
	if _, err := os.Stat("/data/enginex"); err == nil {
		return defaultTimezoneStatePath
	}
	if s.cfg.StateDir != "" {
		return filepath.Join(s.cfg.StateDir, "timezone")
	}
	return defaultTimezoneStatePath
}

func (s *Server) zoneinfoDir() string {
	if s.cfg.ZoneinfoDir != "" {
		return s.cfg.ZoneinfoDir
	}
	return defaultZoneinfoDir
}

// normalizeTimeZoneID reverses Engine's getSystemTimeZone() display formatting,
// which replaces '_' with ' ' (e.g. "America/New York" -> "America/New_York").
func normalizeTimeZoneID(raw string) string {
	return strings.ReplaceAll(strings.TrimSpace(raw), " ", "_")
}

func validIANATimeZone(tz, zoneinfoDir string) bool {
	if len(tz) < 3 || len(tz) > 63 || !strings.Contains(tz, "/") {
		return false
	}
	parts := strings.Split(tz, "/")
	if len(parts) < 2 {
		return false
	}
	for _, seg := range parts {
		if len(seg) == 0 || len(seg) > 32 || seg == "." || seg == ".." {
			return false
		}
		switch strings.ToUpper(seg) {
		case "UTC", "GMT", "UCT", "GMT0", "ZULU", "UNIVERSAL":
			return false
		}
		for i := 0; i < len(seg); i++ {
			b := seg[i]
			if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
				(b >= '0' && b <= '9') || b == '_' || b == '+' || b == '-') {
				return false
			}
		}
	}
	info, err := os.Stat(filepath.Join(zoneinfoDir, tz))
	return err == nil && info.Mode().IsRegular()
}

func checkRegularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink path: %s", path)
	}
	return nil
}

// Caller holds uiMu.
func (s *Server) loadSavedTimeZoneLocked() string {
	path := s.timezoneStatePath()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	tz := normalizeTimeZoneID(string(b))
	if !validIANATimeZone(tz, s.zoneinfoDir()) {
		return ""
	}
	return tz
}

func (s *Server) savedTimeZone() string {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadSavedTimeZoneLocked()
}

// Caller holds uiMu. Invalid or UTC/Universal values never replace a previously
// saved good zone.
func (s *Server) saveTimeZoneLocked(raw string) (saved bool, current string, err error) {
	tz := normalizeTimeZoneID(raw)
	cur := s.loadSavedTimeZoneLocked()
	if !validIANATimeZone(tz, s.zoneinfoDir()) {
		return false, cur, nil
	}
	if tz == cur {
		return true, tz, nil
	}
	path := s.timezoneStatePath()
	dir := filepath.Dir(path)
	for _, p := range []string{dir, path} {
		if err := checkRegularOrMissing(p); err != nil {
			return false, cur, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, cur, err
	}
	if err := checkRegularOrMissing(dir); err != nil {
		return false, cur, err
	}
	tmp, err := os.CreateTemp(dir, ".timezone-*")
	if err != nil {
		return false, cur, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.WriteString(tz + "\n"); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmpName, 0o644)
	}
	if err == nil {
		err = checkRegularOrMissing(path)
	}
	if err == nil {
		err = os.Rename(tmpName, path)
	}
	if err != nil {
		return false, cur, err
	}
	if df, derr := os.Open(dir); derr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return true, tz, nil
}

func (s *Server) timezoneUpdate(w http.ResponseWriter, raw string) {
	s.uiMu.Lock()
	saved, cur, err := s.saveTimeZoneLocked(raw)
	s.uiMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	respond(w, struct {
		FXSelectorReady bool   `json:"fxSelectorReady"`
		TimeZoneSaved   bool   `json:"timezoneSaved"`
		TimeZone        string `json:"timezone,omitempty"`
	}{
		FXSelectorReady: true,
		TimeZoneSaved:   saved,
		TimeZone:        cur,
	})
}
