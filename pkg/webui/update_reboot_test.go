package webui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

// BH-GC2-4: when az01-update returned without restarting the deck, the update
// state stayed "updating" for good and every later try was refused until a
// power cycle. After the grace period it is released with an error.
func TestOnlineUpdateWithoutRebootReleasesTheState(t *testing.T) {
	dir := t.TempDir()
	version := filepath.Join(dir, "version.txt")
	if err := os.WriteFile(version, []byte("5.3.0.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const name = "SC5000-5.3.0.5-EngineX-OS-SC5000-Update.img"
	// A staged image (sparse: AZ01 head, EOF trailer) that is the signed file, so nothing downloads.
	staged := filepath.Join(dir, "az01-update.img")
	f, err := os.Create(staged)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("AZ01")
	f.WriteAt(append([]byte("EOF\x00"), make([]byte, 12)...), 60<<20-16)
	f.Close()
	digest, err := sha256OfFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte(fmt.Sprintf("%s  %s\n", digest, name))
	sig, err := releasesig.Sign(testReleasePriv, sums)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/"+releasesig.SumsAssetName):
			w.Write(sums)
		case strings.HasSuffix(r.URL.Path, "/"+releasesig.SigAssetName):
			w.Write(sig)
		default:
			t.Errorf("the staged image was downloaded again: %s", r.URL.Path) // it is the signed file
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	url := gh.URL + "/dl/" + name
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir(), VersionFile: version, HTTP: gh.Client()}, nil)
	if err := os.WriteFile(staged+".url", []byte(url+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "az01-update")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer func(bin string, grace time.Duration) { az01UpdateBin, updateRebootGrace = bin, grace }(az01UpdateBin, updateRebootGrace)
	az01UpdateBin, updateRebootGrace = fake, 300*time.Millisecond
	s.updateMu.Lock()
	s.updateCache.Updating = true
	s.updateMu.Unlock()
	s.applyOnlineUpdate(url)
	state := func() updateStatus {
		s.updateMu.Lock()
		defer s.updateMu.Unlock()
		return s.updateCache
	}
	if st := state(); !st.Updating || st.Progress != 1 {
		t.Fatalf("right after az01-update: %+v", st)
	}
	deadline := time.Now().Add(3 * time.Second)
	for state().Updating && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if st := state(); st.Updating || !strings.Contains(st.Error, "did not restart") {
		t.Fatalf("no restart after az01-update, still: %+v", st)
	}
}
