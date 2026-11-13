package mods

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// withVersion is a copy of a package whose mod.json says version: a deck's installed older copy.
func withVersion(t *testing.T, blob []byte, version string) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "mod.json" {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			m["version"] = version
			if body, err = json.Marshal(m); err != nil {
				t.Fatal(err)
			}
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// #234 review round 2 (R3): the Minimal Club Mode helper that undoes a pause saved under another
// release's unit name must reach a deck that has 1.2.0, the version without it. An equal version
// is refused ("mod ID already installed"); the official package is newer, so an upload replaces
// the installed one in place. The pause itself lives in mods-state, which an upgrade keeps.
func TestRename234OfficialClubModePackageUpgradesAnInstalled120(t *testing.T) {
	docs := filepath.Join("..", "..", "docs", "mods")
	b, err := os.ReadFile(filepath.Join(docs, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Mods []struct {
			ID, Version string
			DownloadURL string `json:"downloadUrl"`
		} `json:"mods"`
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	for _, m := range reg.Mods {
		if m.ID != "minimal-club-mode" {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(docs, "packages", filepath.Base(m.DownloadURL)))
		if err != nil {
			t.Fatal(err)
		}
		base := t.TempDir()
		mgr := NewManager("", filepath.Join(base, "ui"))
		mgr.runScript = func(Mod, string) error { return nil }
		if _, err := mgr.InstallArchive(bytes.NewReader(withVersion(t, blob, "1.2.0"))); err != nil {
			t.Fatalf("installing 1.2.0: %v", err)
		}
		mod, err := mgr.InstallArchive(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("the official %s over an installed 1.2.0: %v", m.Version, err)
		}
		if mod.Version != m.Version {
			t.Fatalf("installed %s, want %s", mod.Version, m.Version)
		}
		return
	}
	t.Fatal("no minimal-club-mode in the registry")
}
