package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Engine reads this stock Qt logging configuration at startup.
var verboseLogPath = "/data/logging.ini"

const verboseLogRules = "[Rules]\n*.debug=true\nqt.*.debug=false\n"

func writeVerboseLog(enabled bool) error {
	if !enabled {
		if err := os.Remove(verboseLogPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(verboseLogPath), ".logging-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(verboseLogRules); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), verboseLogPath)
}

const (
	logDownloadLimit = 2 << 20
	logReadLimit     = 4 << 20
	logTruncated     = "[Log truncated at export limit]\n"
)

var (
	// Drop the whole journal message, including multiline/escaped payloads, when
	// it mentions credentials. This deliberately prefers privacy over detail.
	sensitiveLog = regexp.MustCompile(`(?i)[a-z0-9_-]*(?:token|arl|password|passwd|passphrase|pwd|cookie|authorization|bearer|credential|signature|jwt|secret|api[ _-]?key|access[ _-]?code)[a-z0-9_-]*|\b(?:music_u|session|sid)\b`)
	encodedLog   = regexp.MustCompile(`%[0-9a-fA-F]{2}`)
	logQuery     = regexp.MustCompile(`([?&][a-zA-Z0-9_.%~-]+=)[^&\s<>"']*`)
	logURL       = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"']+`)
)

func redactLog(message, accessCode string) string {
	message = strings.ReplaceAll(message, `\/`, `/`)
	// Hide encoded messages in full: malformed or repeated escapes must not
	// bypass credential checks or turn encoded spaces into partial URL leaks.
	// stdout continuations can be separate journal entries, so hide indentation too.
	if sensitiveLog.MatchString(message) || encodedLog.MatchString(message) || strings.HasPrefix(message, " ") || strings.HasPrefix(message, "\t") || accessCode != "" && strings.Contains(normalizeAccessCode(message), normalizeAccessCode(accessCode)) {
		return "[REDACTED: credential-related message]"
	}
	message = logURL.ReplaceAllStringFunc(message, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[REDACTED URL]"
		}
		if u.User != nil {
			u.User = url.User("REDACTED")
		}
		if u.RawQuery != "" || u.ForceQuery {
			u.RawQuery = "REDACTED"
			u.ForceQuery = false
		}
		if u.Fragment != "" {
			u.Fragment = "REDACTED"
		}
		return u.String()
	})
	return logQuery.ReplaceAllString(message, "${1}REDACTED")
}

// Read bounded JSON journal entries without exporting fragments of a MESSAGE.
// Only the resulting redacted plain text reaches the browser.
func journalText(src io.Reader, accessCode string) ([]byte, bool, error) {
	input := &io.LimitedReader{R: src, N: logReadLimit}
	dec := json.NewDecoder(input)
	var out bytes.Buffer
	for {
		var entry struct {
			Message   json.RawMessage `json:"MESSAGE"`
			Unit      string          `json:"_SYSTEMD_UNIT"`
			Timestamp string          `json:"__REALTIME_TIMESTAMP"`
		}
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) && input.N == 0 {
			return out.Bytes(), input.N == 0, nil
		}
		if err != nil {
			return nil, false, err
		}
		if entry.Unit != "engine.service" && entry.Unit != "stream-sync.service" && entry.Unit != "enginex-stream-sync.service" {
			continue
		}
		var message string
		if json.Unmarshal(entry.Message, &message) != nil {
			continue
		} // binary entries
		us, err := strconv.ParseInt(entry.Timestamp, 10, 64)
		if err != nil {
			continue
		}
		line := fmt.Sprintf("%s %s: %s\n", time.UnixMicro(us).UTC().Format(time.RFC3339Nano), entry.Unit, redactLog(message, accessCode))
		if out.Len()+len(line) > logDownloadLimit-len(logTruncated) {
			return out.Bytes(), true, nil
		}
		out.WriteString(line)
	}
}

func (s *Server) downloadLogs(w http.ResponseWriter, r *http.Request) {
	if !s.logsMu.TryLock() {
		fail(w, http.StatusTooManyRequests, errors.New("log download already in progress"))
		return
	}
	defer s.logsMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "--no-pager", "--quiet", "--all", "--reverse", "--boot", "--since=-30min", "--until=now", "--lines=5000", "--output=json", "--unit=engine.service", "--unit=stream-sync.service", "--unit=enginex-stream-sync.service")
	pipe, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fail(w, 500, errors.New("could not read logs"))
		return
	}
	body, limited, readErr := journalText(pipe, s.cfg.AccessToken)
	// journalText stops at either byte limit; reap the producer in all cases.
	if limited || readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil || waitErr != nil && !limited {
		fail(w, 500, errors.New("could not read logs"))
		return
	}
	if limited {
		body = append(body, logTruncated...)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="enginex-logs.txt"`)
	w.Write(body)
}
