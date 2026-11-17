package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestPrepareUSBIsSkippedByDeckMount(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	var out bytes.Buffer
	if err := prepareUSB(t.Context(), []string{root}, &out); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "Engine Library", "Database2")
	before := map[string][32]byte{}
	for _, name := range []string{"m.db", ".rb-bridge-state.db", ".rb-bridge-stamp"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = sha256.Sum256(data)
	}
	result, err := bridge.ConvertAtMount(t.Context(), root, enginedb.Options{}, false)
	if err != nil || !result.Bypassed || result.Added != 0 || result.Updated != 0 || result.PlaylistsChanged {
		t.Fatalf("deck mount did work: %+v, %v", result, err)
	}
	for name, want := range before {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || sha256.Sum256(data) != want {
			t.Fatalf("deck mount changed %s: %v", name, err)
		}
	}
	if !strings.Contains(out.String(), "5/5") || !strings.Contains(out.String(), "Prepared") {
		t.Fatalf("missing preparation progress: %s", &out)
	}
}
