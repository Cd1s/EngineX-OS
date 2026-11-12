package webui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

func TestStemmedPathStatsOnlyMatchingTrack(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{StateDir: dir}}

	hitPath := filepath.Join(dir, "hit.flac")
	if err := os.WriteFile(hitPath, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.addStemmed("tidal", stemmedTrack{
		Track: providers.Track{Provider: "tidal", ID: "hit"},
		Path:  hitPath,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := s.addStemmed("tidal", stemmedTrack{
			Track: providers.Track{Provider: "tidal", ID: fmt.Sprintf("other-%d", i)},
			Path:  filepath.Join(dir, fmt.Sprintf("other-%d.flac", i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var stats int
	orig := statStemmedPath
	statStemmedPath = func(name string) (os.FileInfo, error) {
		stats++
		return orig(name)
	}
	t.Cleanup(func() { statStemmedPath = orig })

	if got := s.stemmedPath(providers.Track{Provider: "tidal", ID: "missing"}); got != "" {
		t.Fatalf("missing track returned %q", got)
	}
	if stats != 0 {
		t.Fatalf("stemmedPath on unstemmed track performed %d disk stat calls, want 0", stats)
	}

	if got := s.stemmedPath(providers.Track{Provider: "tidal", ID: "hit"}); got != hitPath {
		t.Fatalf("matching track returned %q, want %q", got, hitPath)
	}
	if stats != 1 {
		t.Fatalf("stemmedPath on matching track performed %d disk stat calls, want 1", stats)
	}
}
