package webui

import (
	"path/filepath"
	"strings"
	"testing"
)

// A loop cut from a track that is not on a stick is saved under the deck's data directory. With the
// old fixed path the tests, run as root on a build host that has a /data, wrote that host's own
// /data/enginex/samples (seen 2026-11-22). TestMain points the directory into its private one.
func TestSamplerOutputStaysOffTheHost(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir(), MediaRoot: t.TempDir()}, nil)
	got := s.determineSampleOutputPath(filepath.Join(t.TempDir(), "track.wav"), 1, 2)
	want := filepath.Join("data", "enginex", "samples", "Loop_Deck1_Pad2.wav")
	if strings.HasPrefix(got, "/data/") || !strings.HasSuffix(got, string(filepath.Separator)+want) {
		t.Fatalf("a loop from the internal drive goes to %s; want the private .../%s", got, want)
	}
}
