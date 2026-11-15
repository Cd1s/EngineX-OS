package stemsd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultBackend(t *testing.T) {
	t.Setenv("ENGINEX_STEMS_BACKEND", "")
	if got := defaultBackend(); got != "htdemucs" {
		t.Fatalf("defaultBackend() = %q, want htdemucs", got)
	}
	t.Setenv("ENGINEX_STEMS_BACKEND", "hpss")
	if got := defaultBackend(); got != "hpss" {
		t.Fatalf("defaultBackend() with env = %q, want hpss", got)
	}
}

func TestResolveWorker(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Current worker.py with neural deps importable: default htdemucs resolves to htdemucs.
	w := script("new.sh", "echo htdemucs\n")
	var log bytes.Buffer
	if name, args, err := resolveWorkerWithLog("/bin/sh", w, defaultBackendOf(""), &log); err != nil || name != "htdemucs" || !slices.Equal(args, []string{"/bin/sh", "-u", w, "--backend", "htdemucs"}) || log.Len() != 0 {
		t.Fatalf("%s %q err=%v log=%q", name, args, err, log.String())
	}
	// Current worker.py without torch/demucs/julius: default htdemucs fails probe, falls back to hpss with one log line.
	wNoTorch := script("notorch.sh", `if [ "$2" = "hpss" ]; then echo hpss; exit 0; fi; echo "the demucs backend needs: pip install demucs" >&2; exit 1`+"\n")
	log.Reset()
	if name, args, err := resolveWorkerWithLog("/bin/sh", wNoTorch, "htdemucs", &log); err != nil || name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", wNoTorch, "--backend", "hpss"}) {
		t.Fatalf("%s %q err=%v", name, args, err)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "falling back to hpss") {
		t.Fatalf("expected single-line fallback log, got %q", log.String())
	}
	// Explicit hpss emits no fallback warning.
	log.Reset()
	if name, args, err := resolveWorkerWithLog("/bin/sh", wNoTorch, "hpss", &log); err != nil || name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", wNoTorch, "--backend", "hpss"}) || log.Len() != 0 {
		t.Fatalf("%s %q err=%v log=%q", name, args, err, log.String())
	}
	// Older worker.py rejects the flags: hpss, started exactly as before.
	w = script("old.sh", "echo 'unrecognized arguments' >&2; exit 2\n")
	if name, args, err := resolveWorker("/bin/sh", w, "htdemucs"); err != nil || name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", w}) {
		t.Fatalf("%s %q err=%v", name, args, err)
	}
}

func TestResolveWorkerPathFindsSiblingThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "bundle", "stems")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(bundleDir, "enginex-stemsd")
	if err := os.WriteFile(realExe, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	siblingWorker := filepath.Join(bundleDir, "worker.py")
	if err := os.WriteFile(siblingWorker, []byte("# worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symExe := filepath.Join(binDir, "enginex-stemsd")
	if err := os.Symlink(realExe, symExe); err != nil {
		t.Fatal(err)
	}
	missingDefault := filepath.Join(dir, "opt", "enginex-stems", "worker.py")

	got := resolveWorkerPath(missingDefault, false, func() (string, error) { return symExe, nil }, "")
	if got != siblingWorker {
		t.Fatalf("resolveWorkerPath(missing default, symlink exe) = %q, want %q", got, siblingWorker)
	}

	// When the default path exists, keep it.
	presentDefault := filepath.Join(dir, "present", "worker.py")
	if err := os.MkdirAll(filepath.Dir(presentDefault), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(presentDefault, []byte("# default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveWorkerPath(presentDefault, false, func() (string, error) { return symExe, nil }, ""); got != presentDefault {
		t.Fatalf("resolveWorkerPath(existing default) = %q, want %q", got, presentDefault)
	}

	// Explicit -worker flag is never overridden.
	explicit := filepath.Join(dir, "custom", "worker.py")
	if got := resolveWorkerPath(explicit, true, func() (string, error) { return symExe, nil }, ""); got != explicit {
		t.Fatalf("resolveWorkerPath(explicit) = %q, want %q", got, explicit)
	}
}

func TestResolvePythonPathPrefersHomeVenv(t *testing.T) {
	dir := t.TempDir()
	homeDir := filepath.Join(dir, "home")
	venvPy := filepath.Join(homeDir, ".enginex-stems", "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(venvPy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvPy, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := resolvePythonPath("/usr/bin/python3", false, "darwin", "", func() (string, error) { return homeDir, nil }, nil)
	if got != venvPy {
		t.Fatalf("resolvePythonPath(darwin with venv) = %q, want %q", got, venvPy)
	}

	// Explicit -python flag is preserved even when venv exists.
	if got := resolvePythonPath("/custom/python", true, "darwin", "", func() (string, error) { return homeDir, nil }, nil); got != "/custom/python" {
		t.Fatalf("resolvePythonPath(explicit) = %q, want /custom/python", got)
	}

	// Windows venv uses Scripts\python.exe.
	winHome := filepath.Join(dir, "winhome")
	winVenvPy := filepath.Join(winHome, ".enginex-stems", "venv", "Scripts", "python.exe")
	if err := os.MkdirAll(filepath.Dir(winVenvPy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(winVenvPy, []byte("exe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolvePythonPath("/usr/bin/python3", false, "windows", "", func() (string, error) { return winHome, nil }, nil); got != winVenvPy {
		t.Fatalf("resolvePythonPath(windows with venv) = %q, want %q", got, winVenvPy)
	}

	// Without venv and missing /usr/bin/python3, falls back to LookPath.
	emptyHome := filepath.Join(dir, "emptyhome")
	missingDefault := filepath.Join(dir, "no-such-python3")
	got = resolvePythonPath(missingDefault, false, "windows", "", func() (string, error) { return emptyHome, nil }, func(name string) (string, error) {
		if name == "python.exe" {
			return `C:\Python312\python.exe`, nil
		}
		return "", errors.New("not found")
	})
	if got != `C:\Python312\python.exe` {
		t.Fatalf("resolvePythonPath(windows LookPath) = %q, want C:\\Python312\\python.exe", got)
	}
}

func TestResolveWorkerFailsAtStartupWhenPythonLacksPackages(t *testing.T) {
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.py")
	if err := os.WriteFile(worker, []byte("# worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate system python3 without numpy/scipy/demucs.
	noPkgPy := filepath.Join(dir, "python3")
	if err := os.WriteFile(noPkgPy, []byte("#!/bin/sh\necho \"ModuleNotFoundError: No module named 'numpy'\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	for _, backend := range []string{"htdemucs", "hpss"} {
		_, _, err := resolveWorkerWithLog(noPkgPy, worker, backend, &log)
		if err == nil {
			t.Fatalf("resolveWorkerWithLog(%s) succeeded with python lacking packages; expected startup error", backend)
		}
		msg := err.Error()
		if !strings.Contains(msg, "run-home-worker") {
			t.Fatalf("startup error %q does not tell user to run launcher script", msg)
		}
		if !strings.Contains(msg, "ModuleNotFoundError") {
			t.Fatalf("startup error %q missing python import failure detail", msg)
		}
	}

	// Missing worker.py also fails clearly at startup.
	if _, _, err := resolveWorkerWithLog(noPkgPy, filepath.Join(dir, "missing.py"), "htdemucs", &log); err == nil || !strings.Contains(err.Error(), "run-home-worker") {
		t.Fatalf("missing worker error = %v, expected launcher hint", err)
	}

	// Missing python interpreter also fails clearly at startup.
	if _, _, err := resolveWorkerWithLog(filepath.Join(dir, "no-python"), worker, "htdemucs", &log); err == nil || !strings.Contains(err.Error(), "run-home-worker") {
		t.Fatalf("missing python error = %v, expected launcher hint", err)
	}
}

func TestMainFailsOnceAtStartupWithSiblingWorkerAndBadPython(t *testing.T) {
	if os.Getenv("ENGINEX_TEST_STEMSD_MAIN") == "1" {
		os.Args = []string{os.Args[0], "-token-file="}
		Main()
		return
	}
	dir := t.TempDir()
	bundle := filepath.Join(dir, "stems")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "worker.py"), []byte("# sibling worker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exeCopy := filepath.Join(bundle, "enginex-stemsd")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfBytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exeCopy, selfBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	stemsHome := filepath.Join(dir, "stems-home")
	venvPy := filepath.Join(stemsHome, "venv", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(venvPy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvPy, []byte("#!/bin/sh\necho \"ModuleNotFoundError: No module named 'numpy'\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exeCopy)
	cmd.Env = append(os.Environ(),
		"ENGINEX_TEST_STEMSD_MAIN=1",
		"ENGINEX_STEMS_HOME="+stemsHome,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when python lacks packages, got 0; output=%s", out)
	}
	s := string(out)
	if !strings.Contains(s, filepath.Join(bundle, "worker.py")) {
		t.Fatalf("expected Main() to resolve sibling worker.py next to executable; output=%s", s)
	}
	if !strings.Contains(s, "run-home-worker") {
		t.Fatalf("expected Main() startup error to mention run-home-worker; output=%s", s)
	}
}

func defaultBackendOf(env string) string {
	if env != "" {
		return env
	}
	return "htdemucs"
}

func TestEmptyTokenFileFlagEnablesZeroConfigLANListen(t *testing.T) {
	var log bytes.Buffer
	listen, tok := resolveListenAndToken("127.0.0.1:6596", "", map[string]bool{"token-file": true}, &log)
	if listen != "0.0.0.0:18765" || tok != "" {
		t.Fatalf("resolveListenAndToken(-token-file \"\") = (%q, %q), want (\"0.0.0.0:18765\", \"\")", listen, tok)
	}
	// Explicit -listen is always respected.
	listen2, tok2 := resolveListenAndToken("127.0.0.1:6596", "", map[string]bool{"listen": true, "token-file": true}, &log)
	if listen2 != "127.0.0.1:6596" || tok2 != "" {
		t.Fatalf("resolveListenAndToken(-listen 127.0.0.1:6596 -token-file \"\") = (%q, %q)", listen2, tok2)
	}
	// Explicit non-empty -token-file without -listen stays on loopback for relay mode.
	listen3, tok3 := resolveListenAndToken("127.0.0.1:6596", "/tmp/tok", map[string]bool{"token-file": true}, &log)
	if listen3 != "127.0.0.1:6596" || tok3 != "/tmp/tok" {
		t.Fatalf("resolveListenAndToken(-token-file /tmp/tok) = (%q, %q)", listen3, tok3)
	}
}

func TestResolveWorkerPathKeepsLegacyInstallation(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "enginex-stems", "worker.py")
	legacy := filepath.Join(root, "sc5000-stems", "worker.py")
	for _, path := range []string{legacy, current} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# worker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveWorkerPath(current, false, nil, legacy); got != path {
			t.Fatalf("resolved %q, want %q", got, path)
		}
	}
	if got := resolveWorkerPath(current+".explicit", true, nil, legacy); got != current+".explicit" {
		t.Fatalf("explicit worker changed to %q", got)
	}
}
