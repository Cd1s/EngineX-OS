package stemsd

import (
	"bytes"
	"context"
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

func TestResolveNativeBackendAndFallbacks(t *testing.T) {
	ctx := context.Background()
	emptyModels := t.TempDir()
	cacheDir := t.TempDir()
	var log bytes.Buffer

	// Explicit native-hpss succeeds with zero warning logs and runs an Engine greeting cleanly.
	name, fn, cleanup := resolveNativeBackend(ctx, "native-hpss", emptyModels, cacheDir, 64, false, nil, &log)
	if cleanup != nil {
		defer cleanup()
	}
	if name != "hpss" || fn == nil || log.Len() != 0 {
		t.Fatalf("native-hpss got name=%q log=%q", name, log.String())
	}
	var out bytes.Buffer
	if err := fn(ctx, strings.NewReader(""), &out); err != nil || !strings.HasPrefix(out.String(), "session=") {
		t.Fatalf("greeting failed: err=%v out=%q", err, out.String())
	}

	// Missing neural weights fall back cleanly to in-process hpss without network access.
	log.Reset()
	name2, fn2, cleanup2 := resolveNativeBackend(ctx, "spleeter", emptyModels, cacheDir, 64, false, nil, &log)
	if cleanup2 != nil {
		defer cleanup2()
	}
	if name2 != "hpss" || fn2 == nil || !strings.Contains(log.String(), "falling back to in-process hpss") {
		t.Fatalf("spleeter fallback got name=%q log=%q", name2, log.String())
	}

	// Tiered mode with an external HQ worker reports hpss+htdemucs when Spleeter weights are absent.
	log.Reset()
	name3, fn3, cleanup3 := resolveNativeBackend(ctx, "tiered", emptyModels, cacheDir, 64, false, []string{"/bin/true"}, &log)
	if cleanup3 != nil {
		defer cleanup3()
	}
	if name3 != "hpss+htdemucs" || fn3 == nil {
		t.Fatalf("tiered with pyHQ got name=%q", name3)
	}
}
