package enginedb

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestEncodePerformanceClampsEndOfTrackCuesAndLoopsForDisplay(t *testing.T) {
	track := rekordbox.Track{
		ID:          1,
		SampleRate:  44100,
		SampleCount: 44100 * 10, // 10.000 s exact audio
		Duration:    10,
	}
	analysis := &rekordbox.Analysis{
		Beats: []rekordbox.Beat{
			{Number: 1, BPM: 120, TimeMS: 0},
			{Number: 2, BPM: 120, TimeMS: 500},
		},
		Waveforms: []rekordbox.Waveform{
			{Tag: "PWV7", EntryBytes: 3, Data: make([]byte, 3*1500)},
		},
		Cues: []rekordbox.Cue{
			{HotCue: 1, TimeMS: 10050, Name: "Outro"},                            // 50 ms past EOF
			{HotCue: 0, TimeMS: 10020, Name: "EndMem"},                           // main memory cue past EOF
			{HotCue: 2, Loop: true, TimeMS: 9500, EndMS: 10050, Name: "EndLoop"}, // loop end past EOF
		},
	}

	perf, err := EncodePerformance(track, analysis)
	if err != nil {
		t.Fatalf("EncodePerformance: %v", err)
	}
	disp, err := ParseDisplayPerformance(perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops)
	if err != nil {
		t.Fatalf("ParseDisplayPerformance rejected clamped end-of-track cues/loops: %v", err)
	}
	if len(disp.Cues) < 2 || len(disp.Loops) != 1 {
		t.Fatalf("unexpected display cues/loops: cues=%+v loops=%+v", disp.Cues, disp.Loops)
	}
}
