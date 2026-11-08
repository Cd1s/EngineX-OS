package stemsd

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveWorker(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Current worker.py: the answer is passed on to every session.
	w := script("new.sh", "echo htdemucs\n")
	if name, args := resolveWorker("/bin/sh", w, "auto"); name != "htdemucs" || !slices.Equal(args, []string{"/bin/sh", "-u", w, "--backend", "htdemucs"}) {
		t.Fatalf("%s %q", name, args)
	}
	// Older worker.py rejects the flags: hpss, started exactly as before.
	w = script("old.sh", "echo 'unrecognized arguments' >&2; exit 2\n")
	if name, args := resolveWorker("/bin/sh", w, "auto"); name != "hpss" || !slices.Equal(args, []string{"/bin/sh", "-u", w}) {
		t.Fatalf("%s %q", name, args)
	}
}
