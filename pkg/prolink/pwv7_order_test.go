package prolink

import (
	"bytes"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// rekordbox's own analysis of its Demo Track 1 (pyrekordbox's test export, in the lab on
// the build host). Its PWV7 bytes are low, mid, high: pkg/enginedb's test measures which
// is which on the same file. prolink rotated them the way Deep Symmetry documents them
// (mid, high, low) in both directions, so a CDJ's waveform reached the deck's display with
// its bands swapped, and so did the deck's overview on a CDJ.
func TestPWV7KeepsItsBandsOnRekordboxDemoTrack(t *testing.T) {
	detail, err := rekordbox.ReadAnalysis("/root/build/sc5000-lab/deep-symmetry-archive/pyrekordbox/.testdata/export/PIONEER/USBANLZ/P016/0000875E/ANLZ0000.2EX")
	if err != nil {
		t.Skip("lab fixture:", err)
	}
	var wave rekordbox.Waveform
	for _, w := range detail.Waveforms {
		if w.Tag == "PWV7" {
			wave = w
		}
	}
	n := len(wave.Data) / 3
	if n < 1024 {
		t.Fatal("fixture has no PWV7")
	}
	a := &rekordbox.Analysis{Waveforms: []rekordbox.Waveform{wave}}
	// From a player: each column is the peak of its share of the entries, band for band.
	cols, _, _ := ExtractANLZ(a)
	for i, col := range cols {
		var want [3]byte
		for j := i * n / len(cols); j < (i+1)*n/len(cols); j++ {
			for band := range want {
				want[band] = max(want[band], wave.Data[3*j+band])
			}
		}
		if col != want {
			t.Fatalf("column %d read as %v, the file has %v", i, col, want)
		}
	}
	// To a player: Engine's overview of the same track goes out as it is.
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: float64(n) / 150, SampleRate: 44100}, a)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(p.Overview, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	out, err := engineWaveforms(raw, float64(n)/150)
	if err != nil || len(out) != 2 || out[1].Tag != "PWV7" || len(out[1].Data) < 3*1024 {
		t.Fatal("no PWV7 from the overview", err)
	}
	sent := out[1].Data
	for i := 0; i < len(sent)/3; i++ {
		bin := raw[24+3*(i*1024/(len(sent)/3)):][:3]
		if !bytes.Equal(sent[3*i:3*i+3], bin) {
			t.Fatalf("entry %d sent as %v, Engine has %v", i, sent[3*i:3*i+3], bin)
		}
	}
}
