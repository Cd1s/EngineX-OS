package joglogo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOriginalRestore(t *testing.T) {
	state := filepath.Join(t.TempDir(), "logo-state")
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(state, 0o755)
	os.WriteFile(app.path, []byte("cropped test PNG"), 0o644)
	volume := t.TempDir()
	target := filepath.Join(volume, "logo.png")
	os.WriteFile(target, []byte("original logo"), 0o644)
	app.library = func(any) (string, error) { return volume, nil }
	if _, err := app.Action(map[string]any{"action": "apply", "volumes": []any{"test"}, "sync": false, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "cropped test PNG" {
		t.Fatalf("apply wrote %q", b)
	}
	if _, err := app.Action(map[string]any{"action": "apply", "volumes": []any{"test"}, "sync": false, "enabled": false}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "original logo" {
		t.Fatalf("restore wrote %q", b)
	}
	if _, err := os.Stat(filepath.Join(volume, ".enginex-logo-owned.json")); !os.IsNotExist(err) {
		t.Fatalf("marker left behind")
	}
}

func TestLibraryOnlyBelowMedia(t *testing.T) {
	for _, v := range []any{"/", "/media", "/etc", 5} {
		if _, err := Library(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
}
