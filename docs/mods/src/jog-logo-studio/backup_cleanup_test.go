package joglogo

import (
	"os"
	"path/filepath"
	"testing"
)

// An apply that fails after it copied the original logo aside must not leave that copy behind
// without its marker: every later apply refused it as somebody else's backup.
func TestApplyWorksAgainAfterAFailedMarkerSave(t *testing.T) {
	state := filepath.Join(t.TempDir(), "logo-state")
	app, err := newFeature(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app.path, []byte("cropped logo"), 0o644); err != nil {
		t.Fatal(err)
	}
	volume := t.TempDir()
	target := filepath.Join(volume, "logo.png")
	if err := os.WriteFile(target, []byte("original logo"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.library = func(any) (string, error) { return volume, nil }
	req := map[string]any{"action": "apply", "volumes": []any{"test"}, "sync": false, "enabled": true}

	// A directory where the marker's temporary file goes: the marker cannot be saved.
	blocker := filepath.Join(volume, ".enginex-logo-owned.tmp")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Action(req); err == nil {
		t.Fatal("apply succeeded although the marker could not be saved")
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "original logo" {
		t.Fatalf("the original logo changed in a failed apply: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(volume, ".enginex-logo-original.png")); err == nil {
		t.Fatal("the copy of the original was left behind without its marker")
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Action(req); err != nil {
		t.Fatalf("apply still refused after the fault was gone: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(volume, ".enginex-logo-original.png")); string(b) != "original logo" {
		t.Fatalf("the original was not kept aside by the second apply: %q", b)
	}
}
