package enginedb

import (
	"sort"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// Which PWV7 byte is which band, measured on rekordbox's own analysis of its two demo
// tracks (pyrekordbox's test export, in the lab on the build host) instead of taken from
// a document: after a beat the lows of a kick and bass ring on, the mids fade sooner and
// the highs of a hat are gone at once. Deep Symmetry documents the bytes as mid, high,
// low; read that way, the "lows" of both tracks would last under a tenth of a beat.
func TestPWV7BandsAreLowMidHighOnRekordboxDemoTracks(t *testing.T) {
	const export = "/root/build/sc5000-lab/deep-symmetry-archive/pyrekordbox/.testdata/export/PIONEER/USBANLZ/"
	for _, dir := range []string{"P016/0000875E", "P053/0001D21F"} { // Demo Track 1, Demo Track 2
		grid, err := rekordbox.ReadAnalysis(export + dir + "/ANLZ0000.DAT")
		if err != nil {
			t.Skip("lab fixture:", err)
		}
		detail, err := rekordbox.ReadAnalysis(export + dir + "/ANLZ0000.2EX")
		if err != nil {
			t.Fatal(err)
		}
		var wave rekordbox.Waveform
		for _, w := range detail.Waveforms {
			if w.Tag == "PWV7" {
				wave = w
			}
		}
		beats := grid.Beats
		if len(wave.Data) == 0 || len(beats) < 64 {
			t.Fatal(dir, "fixture has no PWV7 or beat grid")
		}
		// Each band's level in the second quarter of the beat, relative to the beat itself.
		var onBeat, later [3]float64
		for i := 0; i < len(wave.Data)/3; i++ {
			ms := float64(i) * 1000 / 150
			k := sort.Search(len(beats), func(j int) bool { return float64(beats[j].TimeMS) > ms }) - 1
			if k < 0 || k+1 >= len(beats) {
				continue
			}
			phase := (ms - float64(beats[k].TimeMS)) / float64(beats[k+1].TimeMS-beats[k].TimeMS)
			for band, v := range WaveRGB(wave, i) {
				if phase < 1.0/16 {
					onBeat[band] += float64(v) * 4
				} else if phase >= 0.25 && phase < 0.5 {
					later[band] += float64(v)
				}
			}
		}
		low, mid, high := later[0]/onBeat[0], later[1]/onBeat[1], later[2]/onBeat[2]
		t.Logf("%s: level left a quarter beat on: low %.2f, mid %.2f, high %.2f", dir, low, mid, high)
		if !(low > 1.5*mid && mid > 3*high) {
			t.Errorf("%s: bands are not low, mid, high", dir)
		}
	}
}
