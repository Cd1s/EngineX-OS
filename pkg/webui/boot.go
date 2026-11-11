package webui

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var bootTokenRE = regexp.MustCompile(`^[!-~]{1,128}$`)

// State markers are owned by the guard; their contents never reach public routes.
func bootMarker(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return !errors.Is(err, os.ErrNotExist) // inaccessible state fails closed
}

func currentBootToken(dir string) (string, error) {
	b, err := readBootFile(dir, "boot-token", 129)
	if err != nil {
		return "", err
	}
	token := strings.TrimSuffix(string(b), "\n")
	if !bootTokenRE.MatchString(token) {
		return "", errors.New("invalid boot attempt")
	}
	return token, nil
}

func (s *Server) bootGet(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	token, err := currentBootToken(s.uiDir())
	if err != nil || bootMarker(s.uiDir(), "safe-mode") {
		fail(w, http.StatusConflict, errors.New("no active boot attempt"))
		return
	}
	respond(w, map[string]string{"token": token})
}

func (s *Server) bootReady(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !input(w, r, &in) {
		return
	}
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	dir := s.uiDir()
	token, err := currentBootToken(dir)
	if err != nil || !bootTokenRE.MatchString(in.Token) || in.Token != token || bootMarker(dir, "safe-mode") {
		fail(w, http.StatusConflict, errors.New("boot attempt is no longer active"))
		return
	}
	if err := writeBootAtomic(dir, "boot-ready", []byte(token+"\n"), true); err != nil {
		fail(w, 500, errors.New("could not acknowledge boot"))
		return
	}
	// Recheck because the shell can advance independently of the HTTP mutex.
	// The watcher also requires an exact match with the current attempt token.
	now, err := currentBootToken(dir)
	if err != nil || now != token || bootMarker(dir, "safe-mode") {
		if err := os.Remove(filepath.Join(dir, "boot-ready")); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(w, 500, errors.New("could not invalidate acknowledgement"))
			return
		}
		fail(w, 409, errors.New("boot attempt is no longer active"))
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) bootReset(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	dir := s.uiDir()
	lockFD, err := lockBootReports(dir)
	if err != nil {
		fail(w, 500, errors.New("boot report in progress or state unavailable; retry later"))
		return
	}
	defer unix.Close(lockFD)
	if err := writeBootAtomic(dir, "safe-mode", nil, true); err != nil {
		fail(w, 500, errors.New("could not secure boot safety state"))
		return
	}
	// safe-mode is removed last. Any earlier failure leaves fallback in force;
	// quarantine is deliberately independent and never touched here.
	for _, name := range []string{"boot-token", "boot-ready", "boot-count", "boot-window", "boot-failures", "last-crash", "boot-progress", "boot-stage", "boot-report-latest", "boot-report-usb", "safe-mode"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(w, 500, errors.New("could not clear boot safety state"))
			return
		}
	}
	if err := s.restartEngine(); err != nil {
		// Keep fallback active if systemd did not accept the explicit retry.
		if restoreErr := writeBootAtomic(dir, "safe-mode", nil, true); restoreErr != nil {
			fail(w, 500, errors.New("restart and safety marker restoration failed"))
			return
		}
		fail(w, 500, errors.New("Engine did not restart"))
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) bootStatus(w http.ResponseWriter, r *http.Request) {
	dir := s.uiDir()
	fallback := bootMarker(dir, "safe-mode")
	token, tokenErr := currentBootToken(dir)
	ready, readyErr := readBootFile(dir, "boot-ready", 129)
	preparing := !fallback && tokenErr == nil && (readyErr != nil || strings.TrimSuffix(string(ready), "\n") != token)
	message := ""
	if preparing {
		message = "preparing"
	}
	if fallback {
		message = "fallback"
	}
	name, _, reportErr := existingBootReport(dir)
	exported, exportErr := readBootFile(dir, "boot-report-usb", 128)
	respond(w, struct {
		Fallback       bool   `json:"fallback"`
		Preparing      bool   `json:"preparing"`
		ReportSaved    bool   `json:"reportSaved"`
		DriveAvailable bool   `json:"driveAvailable"`
		Message        string `json:"message"`
	}{fallback, preparing, reportErr == nil, reportErr == nil && exportErr == nil && strings.TrimSpace(string(exported)) == name, message})
}

func (s *Server) downloadBootReport(w http.ResponseWriter, r *http.Request) {
	name, body, err := existingBootReport(s.uiDir())
	if err != nil {
		fail(w, 404, errors.New("boot report unavailable"))
		return
	}
	body = []byte(sanitizeBootReport(body, []string{s.cfg.AccessToken}))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write(body)
}

// RecordBootDatabaseProgress reports completed work, never a timer heartbeat.
// The watcher accepts it only when the attempt token still matches.
func RecordBootDatabaseProgress(dir string) error {
	token, err := currentBootToken(dir)
	if err != nil || bootMarker(dir, "safe-mode") {
		return nil
	}
	return writeBootAtomic(dir, "boot-progress", []byte(fmt.Sprintf("%s database %d\n", token, time.Now().UnixNano())), true)
}
