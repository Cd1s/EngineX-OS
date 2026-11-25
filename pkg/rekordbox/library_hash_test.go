package rekordbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestTrackHashJSONCompatibility(t *testing.T) {
	cases := []Track{
		{ID: 1, Path: "/Music/a<&.mp3", AnalysisPath: "分析", Title: "七夕", Artist: "A\nB", BPM: 120.5, Duration: 2, SampleCount: 3, SampleRate: 44100, BitDepth: 24, Bitrate: 320, Year: 2025, Rating: 5, FileSize: 1234},
		{ID: 7, Path: "/Music/b.mp3", BPM: 0, Duration: 0, StartOffsetSamples: -12, Cues: []Cue{{HotCue: 2, TimeMS: 100, EndMS: 300, Loop: true, Active: true, Name: "Cue", Color: [4]byte{255, 1, 2, 3}, ColorID: 2, ColorCode: 9, LoopNumerator: 4, LoopDenominator: 1, Extended: true}}},
	}
	for _, track := range cases {
		track.RecordHash = ""
		want, err := json.Marshal(track)
		if err != nil {
			t.Fatal(err)
		}
		got := appendTrackJSON(nil, track)
		if string(got) != string(want) {
			t.Fatalf("track JSON mismatch\n got: %s\nwant: %s", got, want)
		}
		h := sha256.Sum256(want)
		if RecordHash(track) != hex.EncodeToString(h[:]) {
			t.Fatalf("RecordHash mismatch: got %s want %s", RecordHash(track), hex.EncodeToString(h[:]))
		}
	}
}
