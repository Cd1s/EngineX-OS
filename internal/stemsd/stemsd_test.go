package stemsd

import (
	"bytes"
	"os"
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
	if name, args := resolveWorkerWithLog("/bin/sh", w, defaultBackendOf(""), &log); name != "htdemucs" || !slices.Equal(args, []string{"/bin/sh", "-u", w, "--backend", "htdemucs"}) || log.Len() != 0 {
		t.Fatalf("%s %q log=%q", name, args, log.String())
	}
	// Current worker.py without torch/demucs/julius: default htdemucs fails probe, falls back to hpss with one log line.
	wNoTorch := script("notorch.sh", `if [ "$2" = "hpss" ]; then echo hpss; exit 0; fi; echo "the demucs backend needs: pip install demucs" >&2; exit 1`+"\n")
	log.Reset()
	if name, args := resolveWorkerWithLog("/bin/sh", wNoTorch, "htdemucs", &log); name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", wNoTorch, "--backend", "hpss"}) {
		t.Fatalf("%s %q", name, args)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "falling back to hpss") {
		t.Fatalf("expected single-line fallback log, got %q", log.String())
	}
	// Explicit hpss emits no fallback warning.
	log.Reset()
	if name, args := resolveWorkerWithLog("/bin/sh", wNoTorch, "hpss", &log); name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", wNoTorch, "--backend", "hpss"}) || log.Len() != 0 {
		t.Fatalf("%s %q log=%q", name, args, log.String())
	}
	// Older worker.py rejects the flags: hpss, started exactly as before.
	w = script("old.sh", "echo 'unrecognized arguments' >&2; exit 2\n")
	if name, args := resolveWorker("/bin/sh", w, "htdemucs"); name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", w}) {
		t.Fatalf("%s %q", name, args)
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
