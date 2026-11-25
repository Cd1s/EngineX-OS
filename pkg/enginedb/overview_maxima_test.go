package enginedb

import (
	"bytes"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestOverviewEndsWithBandMaxima(t *testing.T) {
	wave := rekordbox.Waveform{Tag: "PWV7", EntryBytes: 3, Data: []byte{10, 20, 30, 102, 5, 7, 4, 123, 90}}
	p, err := EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{wave}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := QUncompress(p.Overview, 1<<20)
	if err != nil || !bytes.Equal(raw[len(raw)-3:], []byte{102, 123, 90}) {
		t.Fatalf("trailer %v, err %v", raw[len(raw)-3:], err)
	}
}

func TestOverviewMaximaHelper(t *testing.T) {
	raw := make([]byte, 24+6+3)
	copy(raw[24:], []byte{10, 200, 30, 150, 50, 90, 0, 0, 0})
	got := OverviewMaxima(raw)
	if !bytes.Equal(got[len(got)-3:], []byte{150, 200, 90}) {
		t.Fatalf("trailer = %v", got[len(got)-3:])
	}
	raw[len(raw)-1] = 1
	if got2 := OverviewMaxima(raw); !bytes.Equal(got2, raw) {
		t.Fatal("non-zero trailer changed")
	}
}
