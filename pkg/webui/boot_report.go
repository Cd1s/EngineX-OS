package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const bootReportLimit = 512 << 10

var bootReportModels = []string{"SC5000", "SC5000M", "PRIMEGO", "SC6000", "SC6000M", "PRIME4", "PRIME2", "MIXSTREAMPRO", "SCLIVE4", "SCLIVE2", "PRIMEGOPLUS", "PRIME4PLUS", "PRIME4G2", "SYSTEMONE", "MIXSTREAMPROGO", "MIXSTREAMPROPLUS"}

const bootReportHeader = "EngineX boot report (sanitized)\n"

var bootReportName = regexp.MustCompile(`^EngineX-boot-report-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z(?:-[1-9][0-9]?)?\.txt$`)
var bootReportMu sync.Mutex
var bootRenameNoReplace = unix.Renameat2

// Fixed sources only: never capture arbitrary files, environment or command lines.
var bootReportUnits = []string{"engine.service", "engine-guardian.service", "stream-sync.service", "enginex-stream-sync.service", "rb-bridge.service", "enginex-rb-bridge.service", "sc5000-deemixd.service", "enginex-sc5000-deemixd.service", "sc5000-custom-prepare.service", "sc5000-custom-etc-reset.service", "sc5000-debloat.service", "sc5000-perf-tune.service", "sc5000-storage-discover.service"}

type bootReportInputs struct {
	run                   func(context.Context, string, ...string) ([]byte, error)
	mounts                func() ([]byte, error)
	removable             func(string) bool
	model, engineX, stock string
	codes                 []string
	now                   func() time.Time
}

// WriteBootReport saves one bounded report per fallback, then retries export to
// mounted removable drives. It never opens or heals an Engine database. Callers
// must treat errors as diagnostic only and continue stock boot.
func WriteBootReport(uiDir, mediaRoot string) error {
	inputs := bootReportInputs{
		run:       runBootCommand,
		mounts:    func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") },
		removable: removableBootDevice,
		model:     imagePrefix(deviceCompatible()),
		engineX:   bootVersion(filepath.Join(filepath.Dir(uiDir), "version.txt"), imageVersionFile),
		stock:     bootStockVersion("/usr/Engine/Engine"),
		now:       time.Now,
	}
	if code, err := readBootFile(filepath.Join(filepath.Dir(uiDir), "state"), "access-token", 256); err == nil {
		inputs.codes = []string{strings.TrimSpace(string(code))}
	}
	return writeBootReport(uiDir, mediaRoot, inputs)
}

// These are the same verified GNU build IDs as engine_builds in the UI hook.
// The vendor /etc/version is a timestamp, not the Engine version. Read ELF
// metadata without executing Engine or guessing from the EngineX version.
var bootStockBuilds = map[string]string{
	"af073865a3318d807a8303f716152eac923a4c6f": "5.0.4",
	"be537e908121e76164ddbbf9730d472ba0b8f9d4": "5.3.0",
	"b946c3b8d99d32998c8197d5dc905a6bddaa5d2f": "5.3.0",
	"040335918db1144f0c3452d6f23088d3c3a94613": "5.3.0",
}

func bootStockVersion(path string) string {
	f, err := elf.Open(path)
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_NOTE || p.Filesz > 4096 {
			continue
		}
		notes, err := io.ReadAll(io.LimitReader(p.Open(), 4097))
		if err == nil && len(notes) <= 4096 {
			if version := bootStockNoteVersion(notes, f.ByteOrder); version != "unknown" {
				return version
			}
		}
	}
	return "unknown"
}

func bootStockNoteVersion(notes []byte, order binary.ByteOrder) string {
	for len(notes) >= 12 {
		names, size, kind := uint64(order.Uint32(notes)), uint64(order.Uint32(notes[4:])), order.Uint32(notes[8:])
		nameSpace, descSpace := (names+3)&^uint64(3), (size+3)&^uint64(3)
		body := notes[12:]
		if nameSpace > uint64(len(body)) || descSpace > uint64(len(body))-nameSpace {
			break
		}
		if kind == 3 && names == 4 && size == 20 && bytes.Equal(body[:4], []byte("GNU\x00")) {
			if version, known := bootStockBuilds[hex.EncodeToString(body[nameSpace:nameSpace+size])]; known {
				return version
			}
		}
		notes = body[nameSpace+descSpace:]
	}
	return "unknown"
}

var bootVersionRE = regexp.MustCompile(`^[0-9]{1,3}(?:\.[0-9]{1,4}){1,3}$`)

func bootVersion(paths ...string) string {
	for _, p := range paths {
		b, err := readBootFile(filepath.Dir(p), filepath.Base(p), 128)
		if err == nil && bootVersionRE.MatchString(strings.TrimSpace(string(b))) {
			return strings.TrimSpace(string(b))
		}
	}
	return "unknown"
}

// Each command has its own deadline and output cap, including on failure.
func runBootCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out := &bootCommandBuffer{remaining: 256 << 10, tail: name == "dmesg"}
	cmd.Stdout = out
	// Stderr can contain private metadata; it is never captured or logged.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type bootCommandBuffer struct {
	bytes.Buffer
	remaining int
	tail      bool
}

func (b *bootCommandBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.tail {
		const limit = 256 << 10
		if len(p) >= limit {
			b.Reset()
			p = p[len(p)-limit:]
		} else if b.Len()+len(p) > limit {
			b.Next(b.Len() + len(p) - limit)
		}
		_, err := b.Buffer.Write(p)
		return n, err
	}
	if n > b.remaining {
		p = p[:b.remaining]
	}
	_, err := b.Buffer.Write(p)
	b.remaining -= len(p)
	// Discard overflow while draining the pipe to avoid blocking the producer.
	return n, err
}

var bootPrivateMetadata = regexp.MustCompile(`(?i)serial|volume[ _-]?(?:name|label)|\blabel\b|\b(?:host(?:name)?|serial|uuid|ssid|bssid|mac(?:addr(?:ess)?)?|user(?:name)?|home|path|mountpoint|machine[_ -]?id|client[_ -]?id)\b|/(?:home|root|media|run/media|data)/`)
var bootUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
var bootHost = regexp.MustCompile(`(?i)\b[a-z0-9_-]+(?:\.[a-z0-9_-]+)+\b`)
var bootIPv6 = regexp.MustCompile(`(?i)[0-9a-f]*:[0-9a-f:]*:[0-9a-f:.]*(?:%[a-z0-9_-]+)?`)
var bootMAC = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)

func bootCodeText(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s))
}
func sanitizeBootText(message string, codes []string) string {
	// Redact whole records before splitting: credentials can span multiple lines.
	if bootPrivateMetadata.MatchString(message) {
		return "[REDACTED: private metadata]"
	}
	for _, code := range codes {
		if code != "" && bootCodeText(code) != "" && strings.Contains(bootCodeText(message), bootCodeText(code)) {
			return "[REDACTED: credential-related message]"
		}
	}
	message = redactLog(message, "")
	lines := strings.Split(message, "\n")
	for i, line := range lines {
		if bootPrivateMetadata.MatchString(line) {
			lines[i] = "[REDACTED: private metadata]"
			continue
		}
		line = logURL.ReplaceAllString(line, "[REDACTED URL]")
		line = bootUUID.ReplaceAllString(line, "[REDACTED identifier]")
		line = bootMAC.ReplaceAllString(line, "[REDACTED address]")
		line = bootIPv6.ReplaceAllString(line, "[REDACTED address]")
		lines[i] = bootHost.ReplaceAllString(line, "[REDACTED host/address]")
	}
	return strings.Join(lines, "\n")
}

func bootJournal(raw []byte, codes []string) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out strings.Builder
	for {
		var e struct {
			Unit    string          `json:"_SYSTEMD_UNIT"`
			Message json.RawMessage `json:"MESSAGE"`
		}
		if err := dec.Decode(&e); err != nil {
			break
		} // truncated records are never exported
		if !slices.Contains(bootReportUnits, e.Unit) {
			continue
		}
		var message string
		if json.Unmarshal(e.Message, &message) != nil {
			continue
		}
		line := e.Unit + ": " + sanitizeBootText(message, codes) + "\n"
		if out.Len()+len(line) > 256<<10 {
			break
		}
		out.WriteString(line)
	}
	return out.String()
}

var bootKernelPrefix = regexp.MustCompile("^\\[\\s*[0-9]+(?:\\.[0-9]+)?\\]\\s*")

// Keep complete kernel records: truncation can discard a credential label but
// leave its continuation. Leading partial records are never exported.
func bootKernelRecords(text string, codes []string) string {
	var out, message strings.Builder
	prefix := ""
	flush := func() {
		if prefix != "" {
			out.WriteString(prefix)
			out.WriteString(sanitizeBootText(message.String(), codes))
			out.WriteByte('\n')
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if next := bootKernelPrefix.FindString(line); next != "" {
			flush()
			prefix = next
			message.Reset()
			message.WriteString(strings.TrimPrefix(line, next))
		} else if prefix != "" {
			message.WriteByte('\n')
			message.WriteString(line)
		}
	}
	flush()
	if out.Len() == 0 {
		return "[REDACTED: unrecognized kernel log format]\n"
	}
	return out.String()
}

func writeBootReport(uiDir, mediaRoot string, in bootReportInputs) error {
	if !bootReportMu.TryLock() {
		return errors.New("boot report already in progress")
	}
	defer bootReportMu.Unlock()
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		return errors.New("boot state unavailable")
	}
	lockFD, err := lockBootReports(uiDir)
	if err != nil {
		return errors.New("boot report already in progress or unavailable")
	}
	defer unix.Close(lockFD)
	name, body, err := existingBootReport(uiDir)
	if err != nil {
		// Bound each input and sanitize records before assembling the report.
		stage := "unknown"
		if b, e := readBootFile(uiDir, "boot-stage", 64); e == nil {
			stage = bootStage(strings.TrimSpace(string(b)))
		}
		model := in.model
		if !slices.Contains(bootReportModels, model) {
			model = "unknown"
		}
		ex, stock := in.engineX, in.stock
		if !bootVersionRE.MatchString(ex) {
			ex = "unknown"
		}
		if !bootVersionRE.MatchString(stock) {
			stock = "unknown"
		}
		var report strings.Builder
		fmt.Fprintf(&report, "%sUTC: %s\nEngineX: %s\nStock Engine: %s\nModel: %s\nStage: %s\n", bootReportHeader, in.now().UTC().Format(time.RFC3339), ex, stock, model, stage)
		args := []string{"--boot", "--no-pager", "--quiet", "--all", "--lines=1000", "--output=json"}
		for _, unit := range bootReportUnits {
			args = append(args, "--unit="+unit)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		journal, e := in.run(ctx, "journalctl", args...)
		report.WriteString("\nCurrent boot journal:\n")
		if e != nil {
			report.WriteString("unavailable\n")
		} else {
			if len(journal) > 256<<10 {
				journal = journal[:256<<10]
			}
			report.WriteString(bootJournal(journal, in.codes))
		}
		kernel, e := in.run(ctx, "dmesg")
		cancel()
		report.WriteString("\nKernel tail:\n")
		if e != nil {
			report.WriteString("unavailable\n")
		} else {
			// Keep the tail; discard any initial partial line at the cap.
			if len(kernel) > 64<<10 {
				kernel = kernel[len(kernel)-(64<<10):]
				if n := bytes.IndexByte(kernel, '\n'); n >= 0 {
					kernel = kernel[n+1:]
				} else {
					kernel = nil
				}
			}
			lines := strings.Split(string(kernel), "\n")
			if len(lines) > 100 {
				lines = lines[len(lines)-100:]
			}
			report.WriteString(bootKernelRecords(strings.Join(lines, "\n"), in.codes))
		}
		body = []byte(report.String())
		if len(body) > bootReportLimit {
			return errors.New("boot report exceeds limit")
		}
		name = "EngineX-boot-report-" + in.now().UTC().Format("20060102T150405.000000000Z") + ".txt"
		logs := filepath.Join(filepath.Dir(uiDir), "logs")
		if err := os.MkdirAll(logs, 0o700); err != nil {
			return errors.New("boot report directory unavailable")
		}
		if err := writeBootAtomic(logs, name, body, false); err != nil {
			return errors.New("boot report save failed")
		}
		if err := writeBootAtomic(uiDir, "boot-report-latest", []byte(name+"\n"), true); err != nil {
			return errors.New("boot report marker save failed")
		}
	}
	// Apply current known access codes even when reusing an older report.
	body = []byte(sanitizeBootReport(body, in.codes))
	table, err := in.mounts()
	if err != nil {
		return errors.New("mount table unavailable")
	}
	var errs []error
	exported := false
	for _, mount := range bootReportMounts(table, mediaRoot, in.removable) {
		if err := exportBootReport(mount, name, body); err != nil {
			errs = append(errs, errors.New("boot report USB export failed"))
		} else {
			exported = true
		}
	}
	if exported {
		if err := writeBootAtomic(uiDir, "boot-report-usb", []byte(name+"\n"), true); err != nil {
			errs = append(errs, errors.New("boot report export marker failed"))
		}
	} else {
		if err := os.Remove(filepath.Join(uiDir, "boot-report-usb")); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, errors.New("boot report export marker removal failed"))
		}
	}
	return errors.Join(errs...)
}

// A single nonblocking lock covers report publication and explicit reset across
// the web server and independent guardian processes.
func lockBootReports(dir string) (int, error) {
	dirFD, err := openBootDir(dir)
	if err != nil {
		return -1, err
	}
	defer unix.Close(dirFD)
	fd, err := unix.Openat(dirFD, ".boot-report.lock", unix.O_CREAT|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return -1, errors.New("invalid boot report lock")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func bootStage(stage string) string {
	// Values are fixed guard stages, never arbitrary diagnostic strings.
	switch stage {
	case "database", "preparing", "pre", "starting", "cache", "packs", "mods", "guardian", "engine", "qml", "ready", "fallback", "stock", "engine-start", "ui-ready", "ui-ready-timeout", "crash-loop", "previous-fallback":
		return stage
	default:
		return "unknown"
	}
}

// Preserve the fixed report header/metadata; re-sanitize each diagnostic record.
func sanitizeBootReport(body []byte, codes []string) string {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if i >= 6 {
			lines[i] = sanitizeBootText(line, codes)
		}
	}
	return strings.Join(lines, "\n")
}

func existingBootReport(uiDir string) (string, []byte, error) {
	b, err := readBootFile(uiDir, "boot-report-latest", 128)
	if err != nil {
		return "", nil, err
	}
	name := strings.TrimSuffix(string(b), "\n")
	if !bootReportName.MatchString(name) {
		return "", nil, errors.New("invalid boot report marker")
	}
	body, err := readBootFile(filepath.Join(filepath.Dir(uiDir), "logs"), name, bootReportLimit)
	if err != nil || !validBootReport(body) {
		return "", nil, errors.New("invalid boot report")
	}
	return name, body, nil
}

func validBootReport(body []byte) bool {
	if !bytes.HasPrefix(body, []byte(bootReportHeader)) {
		return false
	}
	lines := strings.SplitN(string(body), "\n", 7)
	if len(lines) != 7 {
		return false
	}
	if !strings.HasPrefix(lines[1], "UTC: ") {
		return false
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(lines[1], "UTC: ")); err != nil {
		return false
	}
	for i, label := range []string{"EngineX: ", "Stock Engine: "} {
		if !strings.HasPrefix(lines[i+2], label) {
			return false
		}
		v := strings.TrimPrefix(lines[i+2], label)
		if v != "unknown" && !bootVersionRE.MatchString(v) {
			return false
		}
	}
	if !strings.HasPrefix(lines[4], "Model: ") || !strings.HasPrefix(lines[5], "Stage: ") {
		return false
	}
	model := strings.TrimPrefix(lines[4], "Model: ")
	stage := strings.TrimPrefix(lines[5], "Stage: ")
	return (model == "unknown" || slices.Contains(bootReportModels, model)) && (stage == "unknown" || bootStage(stage) == stage)
}

// Open every directory component with O_NOFOLLOW. A removable drive cannot
// redirect a privileged writer through an ancestor symlink or a replaced leaf.
func openBootDir(dir string) (int, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	return fd, nil
}

func readBootFile(dir, name string, limit int) ([]byte, error) {
	fd, err := openBootDir(dir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return readBootFileAt(fd, name, limit)
}
func readBootFileAt(fd int, name string, limit int) ([]byte, error) {
	f, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(f), name)
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > int64(limit) {
		return nil, errors.New("invalid boot file")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err == nil && len(body) > limit {
		err = errors.New("boot file too large")
	}
	return body, err
}

func writeBootAtomic(dir, name string, body []byte, replace bool) error {
	fd, err := openBootDir(dir)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return writeBootAtomicAt(fd, name, body, replace)
}
func writeBootAtomicAt(fd int, name string, body []byte, replace bool) error {
	return writeBootAtomicAtMode(fd, name, body, replace, 0o600)
}
func writeBootAtomicAtMode(fd int, name string, body []byte, replace bool, mode uint32) error {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	tmp := ".enginex-boot-" + hex.EncodeToString(id[:])
	f, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd, tmp, 0)
	file := os.NewFile(uintptr(f), tmp)
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if replace {
		return unix.Renameat(fd, tmp, fd, name)
	}
	// RENAME_NOREPLACE is atomic and works on removable filesystems without
	// requiring hard links. Unsupported filesystems fail without changing files.
	err = bootRenameNoReplace(fd, tmp, fd, name, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
		// Older FUSE implementations may support hard links but not rename2.
		// linkat publishes atomically and fails if the user's name exists.
		return unix.Linkat(fd, tmp, fd, name, 0)
	}
	return err
}
