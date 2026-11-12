// Package mods provides USB-drive plug-and-play mod loader functionality,
// including discovery, security validation, state management, QML overlay
// injection, and safe-mode quarantine isolation.
package mods

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Manifest defines the metadata and files of an external mod on a USB drive.
type Manifest struct {
	Category    string            `json:"category,omitempty"`
	Homepage    string            `json:"homepage,omitempty"`
	Official    bool              `json:"official,omitempty"`
	ID          string            `json:"id"`                 // only [a-zA-Z0-9._-]
	Name        string            `json:"name"`               // display name
	Version     string            `json:"version"`            // version string
	Author      string            `json:"author"`             // author
	Description string            `json:"description"`        // description
	QMLFiles    map[string]string `json:"qmlFiles,omitempty"` // mod relative path -> qrc target relative path
	Script      string            `json:"script,omitempty"`   // optional user script relative path
}

// Mod represents a discovered mod along with its runtime and validation status.
type Mod struct {
	Category    string            `json:"category,omitempty"`
	Homepage    string            `json:"homepage,omitempty"`
	Official    bool              `json:"official,omitempty"`
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Author      string            `json:"author"`
	Description string            `json:"description"`
	Drive       string            `json:"drive"`
	Enabled     bool              `json:"enabled"`
	Valid       bool              `json:"valid"`
	Error       string            `json:"error"`
	QMLCount    int               `json:"qmlCount"`
	QMLFiles    map[string]string `json:"qmlFiles,omitempty"`
	Script      string            `json:"script,omitempty"`
	Dir         string            `json:"-"`
	Quarantined bool              `json:"quarantined,omitempty"`
}

// ModState tracks whether an individual mod is enabled or quarantined.
type ModState struct {
	Enabled     bool `json:"enabled"`
	Quarantined bool `json:"quarantined"`
}

// State persists the mod states to mods-state.json.
type State struct {
	SafeModeQuarantined bool                `json:"safeModeQuarantined,omitempty"`
	Mods                map[string]ModState `json:"mods"`
}

// OverlayManifest records the applied mods and files in mods-overlay.
type OverlayManifest struct {
	Generated string            `json:"generated"`
	Mods      []string          `json:"mods"`
	Files     []string          `json:"files"`
	Sources   map[string]string `json:"sources,omitempty"` // target file -> mod ID
}

// Manager coordinates discovery, validation, state, and overlay generation.
type Manager struct {
	mediaRoot string
	uiDir     string
	mu        *sync.Mutex
	runScript func(Mod, string) error
}

var managerMu sync.Mutex

func (m *Manager) lifecycle(mod Mod, action string) error {
	if mod.Script == "" {
		return nil
	}
	if m.runScript != nil {
		return m.runScript(mod, action)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// stream-sync runs SCHED_IDLE, nice 19 and pinned to the background CPU; mod daemons must not
	// inherit that. Reset best-effort inside the child before handing over to the script.
	const reset = `chrt -o -p 0 $$ 2>/dev/null; renice -n 0 -p $$ >/dev/null 2>&1; taskset -pc 0-3 $$ >/dev/null 2>&1; exec /bin/sh "$@"`
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", reset, "sh", filepath.Join(mod.Dir, mod.Script), action)
	cmd.Env = append(os.Environ(), "ENGINEX_MOD_ID="+mod.ID, "ENGINEX_MOD_DIR="+mod.Dir, "ENGINEX_MOD_ACTION="+action)
	// Own process group: a timeout kills the script's children too, not just sh.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mod %s %s: %w", mod.ID, action, err)
	}
	return nil
}

// StartEnabled runs the "start" script of every enabled, valid, non-quarantined mod; used at boot
// because the enabled flag survives reboots but the daemons do not. Failures are logged only.
func (m *Manager) StartEnabled() {
	m.mu.Lock()
	defer m.mu.Unlock()
	list, err := m.discoverLocked()
	if err != nil {
		log.Printf("mods: start enabled: %v", err)
		return
	}
	for _, mod := range list {
		if mod.Enabled && mod.Valid && !mod.Quarantined && mod.Script != "" {
			if err := m.lifecycle(mod, "start"); err != nil {
				log.Printf("mods: %v", err)
			}
		}
	}
}

// NewManager creates a mod manager operating on the given media and UI directories.
func NewManager(mediaRoot, uiDir string) *Manager {
	return &Manager{
		mediaRoot: mediaRoot,
		uiDir:     uiDir,
		mu:        &managerMu,
	}
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidateMod performs strict security validation on the mod manifest and files.
func ValidateMod(modDir string, m *Manifest) error {
	if m == nil {
		return errors.New("nil manifest")
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid mod ID %q: must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$", m.ID)
	}

	evalModDir, err := filepath.EvalSymlinks(modDir)
	if err != nil {
		return fmt.Errorf("cannot resolve mod directory: %w", err)
	}

	if len(m.QMLFiles) == 0 && m.Script == "" {
		return errors.New("mod must define at least one QML file or script")
	}

	for src, dst := range m.QMLFiles {
		if err := validateQMLMapping(modDir, evalModDir, src, dst); err != nil {
			return err
		}
	}

	if m.Script != "" {
		if err := validateScript(modDir, evalModDir, m.Script); err != nil {
			return err
		}
	}

	return nil
}

func isSafeRelPath(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "..\\") {
		return false
	}
	slash := filepath.ToSlash(p)
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func validateQMLMapping(modDir, evalModDir, src, dst string) error {
	if !isSafeRelPath(src) {
		return fmt.Errorf("insecure QML source path %q: must be a relative path without '..'", src)
	}
	if !isSafeRelPath(dst) {
		return fmt.Errorf("insecure QML destination path %q: must be a relative path without '..'", dst)
	}
	if !strings.HasSuffix(strings.ToLower(dst), ".qml") {
		return fmt.Errorf("QML destination path %q must end with .qml", dst)
	}

	fullSrc := filepath.Join(modDir, src)
	evalSrc, err := filepath.EvalSymlinks(fullSrc)
	if err != nil {
		return fmt.Errorf("QML source file %q cannot be resolved: %w", src, err)
	}

	rel, err := filepath.Rel(evalModDir, evalSrc)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("QML source file %q escapes mod directory via symlink", src)
	}

	st, err := os.Stat(evalSrc)
	if err != nil {
		return fmt.Errorf("cannot stat QML source file %q: %w", src, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("QML source file %q is not a regular file", src)
	}
	if st.Size() == 0 {
		return fmt.Errorf("QML source file %q is empty", src)
	}
	if st.Size() > 512*1024 {
		return fmt.Errorf("QML source file %q exceeds 512 KiB limit (%d bytes)", src, st.Size())
	}

	content, err := os.ReadFile(evalSrc)
	if err != nil {
		return fmt.Errorf("cannot read QML source file %q: %w", src, err)
	}
	if !utf8.Valid(content) {
		return fmt.Errorf("QML source file %q is not valid UTF-8", src)
	}
	if err := validateQMLBrackets(content); err != nil {
		return fmt.Errorf("QML source file %q has invalid syntax: %w", src, err)
	}

	return nil
}

func validateScript(modDir, evalModDir, script string) error {
	if !isSafeRelPath(script) {
		return fmt.Errorf("insecure script path %q: must be a relative path without '..'", script)
	}
	fullScript := filepath.Join(modDir, script)
	evalScript, err := filepath.EvalSymlinks(fullScript)
	if err == nil {
		rel, err := filepath.Rel(evalModDir, evalScript)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." || filepath.IsAbs(rel) {
			return fmt.Errorf("script %q escapes mod directory via symlink", script)
		}
		st, err := os.Stat(evalScript)
		if err == nil && !st.Mode().IsRegular() {
			return fmt.Errorf("script %q is not a regular file", script)
		}
	} else {
		return fmt.Errorf("cannot resolve script %q: %w", script, err)
	}
	return nil
}

func validateQMLBrackets(data []byte) error {
	var stack []byte
	n := len(data)
	i := 0
	for i < n {
		b := data[i]

		// Single-line comment //
		if b == '/' && i+1 < n && data[i+1] == '/' {
			i += 2
			for i < n && data[i] != '\n' {
				i++
			}
			continue
		}

		// Multi-line comment /* ... */
		if b == '/' && i+1 < n && data[i+1] == '*' {
			i += 2
			closed := false
			for i+1 < n {
				if data[i] == '*' && data[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				i++
			}
			if !closed {
				return errors.New("unclosed multi-line comment in QML")
			}
			continue
		}

		// Strings: ", ', `
		if b == '"' || b == '\'' || b == '`' {
			quote := b
			i++
			closed := false
			for i < n {
				if data[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if data[i] == quote {
					i++
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Errorf("unclosed string literal (%c) in QML", quote)
			}
			continue
		}

		// Brackets
		if b == '{' || b == '(' {
			stack = append(stack, b)
		} else if b == '}' {
			if len(stack) == 0 {
				return errors.New("unbalanced '}' in QML: unexpected closing brace")
			}
			top := stack[len(stack)-1]
			if top != '{' {
				return fmt.Errorf("mismatched closing brace '}': expected match for '%c'", top)
			}
			stack = stack[:len(stack)-1]
		} else if b == ')' {
			if len(stack) == 0 {
				return errors.New("unbalanced ')' in QML: unexpected closing parenthesis")
			}
			top := stack[len(stack)-1]
			if top != '(' {
				return fmt.Errorf("mismatched closing parenthesis ')': expected match for '%c'", top)
			}
			stack = stack[:len(stack)-1]
		}

		i++
	}

	if len(stack) > 0 {
		return fmt.Errorf("unclosed '%c' in QML", stack[len(stack)-1])
	}
	return nil
}

// Discover scans all drives under mediaRoot for valid and invalid mods.
func (m *Manager) Discover() ([]Mod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.discoverLocked()
}

func (m *Manager) discoverLocked() ([]Mod, error) {
	state := m.loadStateLocked()
	quarantined := m.isSafeModeQuarantinedLocked(state)

	var result []Mod
	seenIDs := make(map[string]bool)

	var entries []os.DirEntry
	if m.mediaRoot != "" {
		var err error
		entries, err = os.ReadDir(m.mediaRoot)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})

	type source struct {
		drive string
		paths []string
	}
	sources := []source{{"internal", []string{filepath.Join(filepath.Dir(m.uiDir), "mods")}}}
	for _, driveEntry := range entries {
		driveName := driveEntry.Name()
		drivePath := filepath.Join(m.mediaRoot, driveName)
		fi, err := os.Stat(drivePath)
		if err != nil || !fi.IsDir() {
			continue
		}

		sources = append(sources, source{driveName, []string{
			filepath.Join(drivePath, "mods"),
			filepath.Join(drivePath, "EngineX", "mods"),
		}})
	}

	for _, source := range sources {
		for _, candidate := range source.paths {
			modEntries, err := os.ReadDir(candidate)
			if err != nil {
				continue
			}
			slices.SortFunc(modEntries, func(a, b os.DirEntry) int {
				return strings.Compare(a.Name(), b.Name())
			})

			for _, me := range modEntries {
				if source.drive == "internal" && strings.HasPrefix(me.Name(), ".install-") {
					continue
				}
				modDir := filepath.Join(candidate, me.Name())
				st, err := os.Stat(modDir)
				if err != nil || !st.IsDir() {
					continue
				}

				data, err := os.ReadFile(filepath.Join(modDir, "manifest.json"))
				if os.IsNotExist(err) {
					data, err = os.ReadFile(filepath.Join(modDir, "mod.json"))
				}
				if err != nil {
					continue
				}

				var manifest Manifest
				if err := json.Unmarshal(data, &manifest); err != nil {
					mod := Mod{
						ID:    me.Name(),
						Name:  me.Name(),
						Drive: source.drive,
						Valid: false,
						Error: "invalid manifest.json: " + err.Error(),
						Dir:   modDir,
					}
					result = append(result, mod)
					continue
				}

				if seenIDs[manifest.ID] {
					continue
				}
				seenIDs[manifest.ID] = true

				if manifest.Name == "" {
					manifest.Name = manifest.ID
				}

				modState := state.Mods[manifest.ID]
				mod := Mod{
					Category:    manifest.Category,
					Homepage:    manifest.Homepage,
					Official:    manifest.Official,
					ID:          manifest.ID,
					Name:        manifest.Name,
					Version:     manifest.Version,
					Author:      manifest.Author,
					Description: manifest.Description,
					Drive:       source.drive,
					Enabled:     modState.Enabled,
					Quarantined: quarantined || modState.Quarantined,
					QMLFiles:    manifest.QMLFiles,
					Script:      manifest.Script,
					Dir:         modDir,
					QMLCount:    len(manifest.QMLFiles),
				}

				if valErr := ValidateMod(modDir, &manifest); valErr != nil {
					mod.Valid = false
					mod.Error = valErr.Error()
				} else {
					mod.Valid = true
				}

				result = append(result, mod)
			}
		}
	}

	if result == nil {
		result = make([]Mod, 0)
	}
	return result, nil
}

// InstallArchive validates and installs a gzip-compressed tar archive.
func (m *Manager) InstallArchive(r io.Reader) (*Mod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	const limit = 16 << 20
	compressed, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if len(compressed) > limit {
		return nil, errors.New("archive exceeds 16 MiB")
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	root := filepath.Join(filepath.Dir(m.uiDir), "mods")
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	unpacked := &io.LimitedReader{R: gz, N: (32 << 20) + 1}
	tr := tar.NewReader(unpacked)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "." && h.Typeflag == tar.TypeDir {
			continue
		}
		if !isSafeRelPath(name) || strings.Contains(name, "\\") {
			return nil, fmt.Errorf("unsafe archive path %q", h.Name)
		}
		path := filepath.Join(stage, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return nil, err
			}
		case tar.TypeXGlobalHeader: // git archive / GitHub tarballs start with a pax global header
			continue
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || h.Size > limit-total {
				return nil, errors.New("archive contents exceed 16 MiB")
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return nil, err
			}
			// Keep only the exec bits so a mod's own binaries can run; never setuid/setgid/sticky (#170).
			mode := 0644 | h.FileInfo().Mode().Perm()&0111
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return nil, err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
		default:
			return nil, fmt.Errorf("unsupported archive entry %q", h.Name)
		}
	}
	// Consume the gzip trailer to detect truncation and checksum errors.
	if _, err := io.Copy(io.Discard, unpacked); err != nil {
		return nil, err
	}
	if unpacked.N == 0 {
		return nil, errors.New("archive expands beyond limit")
	}
	var manifests []string
	for _, pattern := range []string{"manifest.json", "mod.json", "*/manifest.json", "*/mod.json"} {
		matches, err := filepath.Glob(filepath.Join(stage, pattern))
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, matches...)
	}
	if len(manifests) != 1 {
		return nil, errors.New("archive must contain exactly one root or first-level manifest")
	}
	data, err := os.ReadFile(manifests[0])
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	dir := filepath.Dir(manifests[0])
	if err := ValidateMod(dir, &manifest); err != nil {
		return nil, err
	}
	if manifest.ID == "tailscale-remote" {
		return nil, errors.New("tailscale-remote is built in: install and enable it from Mods in the web console")
	}
	discovered, err := m.discoverLocked()
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(root, manifest.ID)
	for _, mod := range discovered {
		if mod.ID != manifest.ID {
			continue
		}
		// A newer version of an internal mod replaces it in place, keeping its enabled
		// state and its state directory: official 1.0.0 helpers needed a Python the
		// decks never had (#208), and an upload of 1.1.0 must reach those decks.
		if mod.Drive != "internal" || mod.Dir != dest || !newerModVersion(manifest.Version, mod.Version) {
			return nil, errors.New("mod ID already installed")
		}
		return m.upgradeLocked(mod, dir, manifest.ID)
	}
	if _, err := os.Lstat(dest); err == nil {
		return nil, errors.New("mod directory already exists")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.Rename(dir, dest); err != nil {
		return nil, err
	}
	// Installed mods stay disabled until the user enables them (registry promises explicit activation).
	state := m.loadStateLocked()
	delete(state.Mods, manifest.ID)
	if err := m.saveStateLocked(state); err != nil {
		_ = os.RemoveAll(dest)
		return nil, err
	}
	list, err := m.discoverLocked()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == manifest.ID {
			return &list[i], nil
		}
	}
	return nil, errors.New("installed mod could not be discovered")
}

// upgradeLocked swaps an installed internal mod for the staged newer version.
func (m *Manager) upgradeLocked(old Mod, staged, id string) (*Mod, error) {
	state := m.loadStateLocked()
	enabled := state.Mods[id].Enabled
	if enabled && old.Valid {
		if err := m.lifecycle(old, "stop"); err != nil {
			log.Printf("mods: upgrade %s: ignoring failed stop of the old version: %v", id, err) // a broken old script must not block its own fix
		}
	}
	// The previous version waits in a directory of its own (".install-" names are never
	// discovered as mods): a name derived from the mod ID could be another installed mod's.
	backupRoot, err := os.MkdirTemp(filepath.Dir(old.Dir), ".install-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(backupRoot)
	backup := filepath.Join(backupRoot, "previous")
	if err := os.Rename(old.Dir, backup); err != nil {
		return nil, err
	}
	if err := os.Rename(staged, old.Dir); err != nil {
		_ = os.Rename(backup, old.Dir)
		return nil, err
	}
	list, err := m.discoverLocked()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if enabled && (!list[i].Valid || m.isSafeModeQuarantinedLocked(state) || m.lifecycle(list[i], "start") != nil) {
			if list[i].Valid && !m.isSafeModeQuarantinedLocked(state) {
				log.Printf("mods: upgrade %s: the new version did not start; left disabled", id)
				ms := state.Mods[id]
				ms.Enabled = false
				state.Mods[id] = ms
				if err := m.saveStateLocked(state); err != nil {
					return nil, err
				}
				list[i].Enabled = false
			}
		}
		if err := m.applyActiveOverlayLocked(""); err != nil {
			return nil, err
		}
		return &list[i], nil
	}
	return nil, errors.New("upgraded mod could not be discovered")
}

// newerModVersion compares dotted numeric versions (1.10.0 > 1.9.2); anything
// non-numeric is never newer.
func newerModVersion(candidate, installed string) bool {
	parse := func(v string) ([]int, bool) {
		var out []int
		for _, part := range strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".") {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return nil, false
			}
			out = append(out, n)
		}
		return out, len(out) > 0
	}
	c, ok1 := parse(candidate)
	i, ok2 := parse(installed)
	if !ok1 || !ok2 {
		return false
	}
	for k := 0; k < len(c) || k < len(i); k++ {
		var a, b int
		if k < len(c) {
			a = c[k]
		}
		if k < len(i) {
			b = i[k]
		}
		if a != b {
			return a > b
		}
	}
	return false
}

// UninstallMod removes an internal mod and its state.
func (m *Manager) UninstallMod(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !idPattern.MatchString(id) {
		return errors.New("invalid mod ID")
	}
	list, err := m.discoverLocked()
	if err != nil {
		return err
	}
	for _, mod := range list {
		if mod.ID != id {
			continue
		}
		if mod.Drive != "internal" || mod.Dir != filepath.Join(filepath.Dir(m.uiDir), "mods", id) {
			return errors.New("only internal mods can be uninstalled")
		}
		if mod.Enabled && mod.Valid {
			if err := m.lifecycle(mod, "stop"); err != nil {
				log.Printf("mods: uninstall %s: ignoring failed stop: %v", id, err) // a broken script must not make the mod permanent
			}
		}
		if err := os.RemoveAll(mod.Dir); err != nil {
			return err
		}
		state := m.loadStateLocked()
		delete(state.Mods, id)
		if err := m.saveStateLocked(state); err != nil {
			return err
		}
		return m.applyActiveOverlayLocked("")
	}
	return errors.New("mod not found")
}

// IsSafeModeQuarantined checks if external mods are quarantined.
func (m *Manager) IsSafeModeQuarantined() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.loadStateLocked()
	return m.isSafeModeQuarantinedLocked(state)
}

// IsModEnabled reports whether id is enabled and not quarantined without taking
// managerMu or scanning media drives. Safe for concurrent reads from /v1/sc5000/ui
// because saveStateLocked writes mods-state.json atomically via temp file + rename.
func (m *Manager) IsModEnabled(id string) bool {
	if m == nil || id == "" {
		return false
	}
	state := m.loadStateFile()
	if m.isSafeModeQuarantinedState(state) {
		return false
	}
	ms := state.Mods[id]
	return ms.Enabled && !ms.Quarantined
}

func (m *Manager) isSafeModeQuarantinedLocked(state State) bool {
	return m.isSafeModeQuarantinedState(state)
}

func (m *Manager) isSafeModeQuarantinedState(state State) bool {
	if state.SafeModeQuarantined {
		return true
	}
	if _, err := os.Stat(filepath.Join(m.uiDir, "mods-quarantined")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(m.uiDir, "safe-mode")); err == nil {
		return true
	}
	return false
}

func (m *Manager) stateFile() string {
	return filepath.Join(m.uiDir, "mods-state.json")
}

func (m *Manager) loadStateLocked() State {
	return m.loadStateFile()
}

func (m *Manager) loadStateFile() State {
	s := State{Mods: make(map[string]ModState)}
	b, err := os.ReadFile(m.stateFile())
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	if s.Mods == nil {
		s.Mods = make(map[string]ModState)
	}
	return s
}

func (m *Manager) saveStateLocked(s State) error {
	if err := os.MkdirAll(m.uiDir, 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.uiDir, ".mods-state-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.stateFile())
}

// SetModEnabled toggles the enabled state of a mod and reapplies overlays.
func (m *Manager) SetModEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadStateLocked()
	{
		modsList, err := m.discoverLocked()
		if err != nil {
			return err
		}
		var found *Mod
		for i := range modsList {
			if modsList[i].ID == id {
				found = &modsList[i]
				break
			}
		}
		if found == nil {
			return fmt.Errorf("mod %q not found", id)
		}
		if enabled && !found.Valid {
			return fmt.Errorf("cannot enable invalid mod %q: %s", id, found.Error)
		}
		prev := state.Mods[id]
		if found.Valid && (prev.Enabled != enabled || (enabled && prev.Quarantined)) {
			action := "stop"
			if enabled {
				action = "start"
			}
			// A safe-mode quarantine keeps daemons stopped until ResetQuarantine.
			if action == "stop" || !m.isSafeModeQuarantinedLocked(state) {
				if err := m.lifecycle(*found, action); err != nil {
					return err
				}
			}
		}
	}

	modState := state.Mods[id]
	modState.Enabled = enabled
	modState.Quarantined = false // an explicit user choice lifts a per-mod quarantine
	state.Mods[id] = modState

	if err := m.saveStateLocked(state); err != nil {
		return err
	}

	return m.applyActiveOverlayLocked("")
}

// QuarantineAll isolates all mods and cleans up overlay files upon safe-mode crash protection.
func (m *Manager) QuarantineAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Quarantine means "off": stop the daemons of enabled mods too (best effort).
	if list, err := m.discoverLocked(); err == nil {
		for _, mod := range list {
			if mod.Enabled && mod.Valid && mod.Script != "" {
				_ = m.lifecycle(mod, "stop")
			}
		}
	}

	state := m.loadStateLocked()
	state.SafeModeQuarantined = true
	for id, ms := range state.Mods {
		ms.Quarantined = true
		state.Mods[id] = ms
	}
	if err := m.saveStateLocked(state); err != nil {
		return err
	}

	_ = os.MkdirAll(m.uiDir, 0755)
	_ = os.WriteFile(filepath.Join(m.uiDir, "mods-quarantined"), []byte("quarantined\n"), 0644)
	_ = os.RemoveAll(filepath.Join(m.uiDir, "mods-overlay"))
	m.cleanupActivePackDir(filepath.Join(m.uiDir, "active"), nil)

	return nil
}

// ResetQuarantine clears the quarantined status of all mods and reapplies overlays.
func (m *Manager) ResetQuarantine() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state := m.loadStateLocked()
	state.SafeModeQuarantined = false
	for id, ms := range state.Mods {
		ms.Quarantined = false
		state.Mods[id] = ms
	}
	if err := m.saveStateLocked(state); err != nil {
		return err
	}

	_ = os.Remove(filepath.Join(m.uiDir, "mods-quarantined"))

	if list, err := m.discoverLocked(); err == nil {
		for _, mod := range list {
			if mod.Enabled && mod.Valid && !mod.Quarantined && mod.Script != "" {
				if err := m.lifecycle(mod, "start"); err != nil {
					log.Printf("mods: %v", err)
				}
			}
		}
	}
	return m.applyActiveOverlayLocked("")
}

// ApplyActiveOverlay merges active, validated QML files into mods-overlay and optionally activePackDir.
func (m *Manager) ApplyActiveOverlay(activePackDir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.applyActiveOverlayLocked(activePackDir)
}

func (m *Manager) applyActiveOverlayLocked(activePackDir string) error {
	overlayDir := filepath.Join(m.uiDir, "mods-overlay")

	state := m.loadStateLocked()
	if m.isSafeModeQuarantinedLocked(state) {
		_ = os.RemoveAll(overlayDir)
		if activePackDir != "" {
			m.cleanupActivePackDir(activePackDir, nil)
		}
		return nil
	}

	discovered, err := m.discoverLocked()
	if err != nil {
		return err
	}

	var activeMods []Mod
	for _, mod := range discovered {
		if mod.Enabled && !mod.Quarantined && mod.Valid {
			activeMods = append(activeMods, mod)
		}
	}

	if len(activeMods) == 0 {
		_ = os.RemoveAll(overlayDir)
		if activePackDir != "" {
			m.cleanupActivePackDir(activePackDir, nil)
		}
		return nil
	}

	if err := os.MkdirAll(m.uiDir, 0755); err != nil {
		return err
	}
	stagingDir, err := os.MkdirTemp(m.uiDir, ".mods-overlay-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)

	var appliedFiles []string
	var appliedMods []string
	sources := make(map[string]string)

	slices.SortFunc(activeMods, func(a, b Mod) int {
		return strings.Compare(a.ID, b.ID)
	})

	stateDirty := false
	for _, mod := range activeMods {
		// Stage every file of the mod under a temp name first: one unreadable file must
		// quarantine only this mod, never leave it half applied or abort the others.
		srcs := make([]string, 0, len(mod.QMLFiles))
		for src := range mod.QMLFiles {
			srcs = append(srcs, src)
		}
		sort.Strings(srcs)
		var staged []string // destination names
		var copyErr error
		for _, src := range srcs {
			dst := mod.QMLFiles[src]
			if copyErr = copyFile(filepath.Join(mod.Dir, src), filepath.Join(stagingDir, dst+".modtmp")); copyErr != nil {
				copyErr = fmt.Errorf("failed to copy %s to overlay: %w", src, copyErr)
				break
			}
			staged = append(staged, dst)
		}
		if copyErr != nil {
			for _, dst := range staged {
				_ = os.Remove(filepath.Join(stagingDir, dst+".modtmp"))
			}
			log.Printf("mods: quarantining %s: %v", mod.ID, copyErr)
			ms := state.Mods[mod.ID]
			ms.Quarantined = true
			state.Mods[mod.ID] = ms
			stateDirty = true
			_ = m.lifecycle(mod, "stop")
			continue
		}
		for _, dst := range staged {
			if err := os.Rename(filepath.Join(stagingDir, dst+".modtmp"), filepath.Join(stagingDir, dst)); err != nil {
				return err
			}
			if _, exists := sources[dst]; !exists {
				appliedFiles = append(appliedFiles, dst)
			}
			sources[dst] = mod.ID
		}
		appliedMods = append(appliedMods, mod.ID)
	}
	if stateDirty {
		if err := m.saveStateLocked(state); err != nil {
			log.Printf("mods: saving quarantine state: %v", err)
		}
	}
	if len(appliedMods) == 0 {
		_ = os.RemoveAll(overlayDir)
		if activePackDir != "" {
			m.cleanupActivePackDir(activePackDir, nil)
		}
		return nil
	}

	manifest := OverlayManifest{
		Generated: time.Now().UTC().Format(time.RFC3339),
		Mods:      appliedMods,
		Files:     appliedFiles,
		Sources:   sources,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), manifestData, 0644); err != nil {
		return err
	}

	_ = os.RemoveAll(overlayDir)
	if err := os.Rename(stagingDir, overlayDir); err != nil {
		if err := copyDir(stagingDir, overlayDir); err != nil {
			return err
		}
	}

	if activePackDir != "" {
		if err := m.syncToActivePackDir(activePackDir, overlayDir, appliedFiles, manifestData); err != nil {
			return err
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		return copyFile(path, target)
	})
}

// modsBackupDir keeps the base UI pack's own copy of any file a mod replaced in the active pack.
const modsBackupDir = ".mods-backup"

func readOldManifest(activePackDir string) (OverlayManifest, bool) {
	var old OverlayManifest
	data, err := os.ReadFile(filepath.Join(activePackDir, ".mods-manifest.json"))
	if err != nil || json.Unmarshal(data, &old) != nil {
		return old, false
	}
	return old, true
}

func (m *Manager) cleanupActivePackDir(activePackDir string, keepFiles []string) {
	manifestPath := filepath.Join(activePackDir, ".mods-manifest.json")
	oldManifest, ok := readOldManifest(activePackDir)
	if !ok {
		return
	}
	keepSet := make(map[string]bool)
	for _, f := range keepFiles {
		keepSet[f] = true
	}
	for _, oldFile := range oldManifest.Files {
		if keepSet[oldFile] {
			continue
		}
		dst := filepath.Join(activePackDir, oldFile)
		_ = os.Remove(dst)
		_ = os.Remove(filepath.Join(activePackDir, "mods-overlay", oldFile))
		// Put the base pack's original back instead of leaving a hole.
		if bak := filepath.Join(activePackDir, modsBackupDir, oldFile); fileExists(bak) {
			if err := os.Rename(bak, dst); err != nil {
				log.Printf("mods: restoring %s: %v", oldFile, err)
			}
		}
	}
	if len(keepFiles) == 0 {
		_ = os.Remove(manifestPath)
		_ = os.RemoveAll(filepath.Join(activePackDir, "mods-overlay"))
		_ = os.RemoveAll(filepath.Join(activePackDir, modsBackupDir))
	}
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func (m *Manager) syncToActivePackDir(activePackDir, overlayDir string, appliedFiles []string, manifestData []byte) error {
	ours := make(map[string]bool) // files a previous sync already wrote (their original is in the backup dir, if any)
	if old, ok := readOldManifest(activePackDir); ok {
		for _, f := range old.Files {
			ours[f] = true
		}
	}
	m.cleanupActivePackDir(activePackDir, appliedFiles)

	for _, f := range appliedFiles {
		src := filepath.Join(overlayDir, f)
		dst := filepath.Join(activePackDir, f)
		if bak := filepath.Join(activePackDir, modsBackupDir, f); !ours[f] && fileExists(dst) && !fileExists(bak) {
			if err := copyFile(dst, bak); err != nil {
				return err
			}
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
		overlayDst := filepath.Join(activePackDir, "mods-overlay", f)
		if err := copyFile(src, overlayDst); err != nil {
			return err
		}
	}

	manifestPath := filepath.Join(activePackDir, ".mods-manifest.json")
	return os.WriteFile(manifestPath, manifestData, 0644)
}
