package prolink

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestBuildPDBRejectsInvalidAudioMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*rekordbox.Track)
	}{
		{"bpm", func(t *rekordbox.Track) { t.BPM = 1001 }},
		{"legacy_bpm_sentinel", func(t *rekordbox.Track) { t.BPM = float64(uint32(0xffffffff)) / 100 }},
		{"nan_bpm", func(t *rekordbox.Track) { t.BPM = math.NaN() }},
		{"duration", func(t *rekordbox.Track) { t.Duration = 1e7 + 1 }},
		{"sample_rate", func(t *rekordbox.Track) { t.SampleRate = 768001 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track := rekordbox.Track{ID: 1, Path: "/Music/test.wav", BPM: 120, Duration: 180, SampleRate: 48000}
			tc.set(&track)
			data, err := buildPDB([]rekordbox.Track{track}, nil)
			if err == nil || !strings.Contains(err.Error(), "invalid audio metadata") || data != nil {
				t.Fatalf("expected export validation error and no PDB, got %v (%d bytes)", err, len(data))
			}
		})
	}
	if _, err := buildPDB([]rekordbox.Track{{ID: 1, Path: "/Music/test.wav"}}, nil); err != nil {
		t.Fatalf("zero/unknown metadata must remain valid: %v", err)
	}
}

func TestWriteANLZRejectsInvalidAudioMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bpm    float64
		length int
	}{
		{"bpm", 1001, 180},
		{"nan_bpm", math.NaN(), 180},
		{"duration", 120, 10000001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path, err := writeANLZ(root, 1, "/Music/test.wav", tc.bpm, tc.length)
			if err == nil || !strings.Contains(err.Error(), "invalid audio metadata") || path != "" {
				t.Fatalf("expected analysis validation error, got %q, %v", path, err)
			}
			if _, err := os.Stat(filepath.Join(root, "PIONEER")); !os.IsNotExist(err) {
				t.Fatalf("invalid metadata created analysis output: %v", err)
			}
		})
	}
}
