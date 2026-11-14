package enginedb

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// A 25-row batch of roughly 337 KiB JSON rows, matching the reported stick's
// base64 waveform payload rather than an unrealistically repeated zero buffer.
func BenchmarkPackAnalysisLargeBatch(b *testing.B) {
	rng := rand.New(rand.NewSource(273))
	data := make([]byte, 250<<10)
	for i := range data {
		data[i] = byte(rng.Intn(128))
	}
	source, err := json.Marshal(struct{ Analysis *rekordbox.Analysis }{&rekordbox.Analysis{Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: data}}}})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(source) * 25))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := 0; i < 25; i++ {
			packed, err := packAnalysis(source)
			if err != nil {
				b.Fatal(err)
			}
			if len(packed) >= len(source) {
				b.Fatal("large waveform row did not shrink")
			}
			if _, err = packAnalysis([]byte(`{"SampleRate":44100,"Samples":441000}`)); err != nil {
				b.Fatal(err)
			}
		}
	}
}
