package webui

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The copy replays the BPM of the source's analysis: it is the one the source deck shows,
// not the row's bpmAnalyzed (stick, 2026-11-20: Fons et Origo 129.00 on RIGHT, its copy on
// LEFT 130.00). A deck that reports no BPM keeps the row's.
func TestShadowPerfCarriesTheBPMTheSourceDeckShows(t *testing.T) {
	dataRoot := t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	for _, d := range []string{filepath.Join(lib, "Database2"), filepath.Join(dataRoot, "Music")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "Music/fons.wav"), []byte("WAVDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, _, db := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 129, Number: 1}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO Track(path,filename,bpm,bpmAnalyzed,isAnalyzed) VALUES('../../../../Music/fons.wav','fons.wav',129,130,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "Database2", "m.db"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = dataRoot, t.TempDir()

	perfBPM := func(deckBPM float64) float64 {
		t.Helper()
		body, _ := json.Marshal([]deckState{{Deck: 1, Net: "net://u/(Unknown)/Music/fons.wav", BPM: deckBPM}})
		w := httptest.NewRecorder()
		s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", bytes.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("decksPost %d", w.Code)
		}
		s.shadow.mu.Lock()
		key := s.shadow.own[0].Key
		s.shadow.mu.Unlock()
		req := httptest.NewRequest("GET", "/peer/decks/1/perf?key="+url.QueryEscape(key), nil)
		req.SetPathValue("n", "1")
		w = httptest.NewRecorder()
		s.peerPerfGet(w, req)
		b := w.Body.Bytes()
		if w.Code != http.StatusOK || len(b) < 16 || string(b[:4]) != "EXPF" {
			t.Fatalf("perf %d %q", w.Code, b)
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b[8:16]))
	}
	if got := perfBPM(129); got != 129 {
		t.Fatalf("perf bpm %v, want the 129 the source deck shows", got)
	}
	if got := perfBPM(0); got != 130 {
		t.Fatalf("perf bpm %v without a deck BPM, want the row's 130", got)
	}
}

// Engine keeps a copy's row for its path and does not analyse it again, so a copy whose row
// has another BPM than the source now gives goes under a path of its own (LEFT, 2026-11-20:
// Fons et Origo's copy kept 130.0 from an earlier build; its source shows 129.00).
func TestShadowCopyWithAStaleBPMGetsAFreshPath(t *testing.T) {
	dataRoot := t.TempDir()
	lib := filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	if err := os.MkdirAll(filepath.Join(lib, "Database2"), 0o755); err != nil {
		t.Fatal(err)
	}
	file, _, db := testlibrary.Create(t)
	if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../../../../enginex/shadow-v3/peer-1/Contents/Fons.flac','Fons.flac',130,1)`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "Database2", "m.db"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: filepath.Join(dataRoot, "enginex", "ui"), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = dataRoot, t.TempDir()
	local := filepath.Join(s.shadowDir(), "peer-1", "Contents", "Fons.flac")
	expf := func(bpm float64) []byte {
		b := make([]byte, 16)
		copy(b, "EXPF")
		binary.LittleEndian.PutUint64(b[8:], math.Float64bits(bpm))
		return b
	}
	moved := s.shadowCopyForBPM(t.Context(), "peer:k", local, expf(129))
	if moved == local || filepath.Base(moved) != "Fons.flac" || filepath.Base(filepath.Dir(moved)) != "Contents" {
		t.Fatalf("stale copy stayed or lost its name: %s", moved)
	}
	if again := s.shadowCopyForBPM(t.Context(), "peer:k", local, expf(129)); again != moved {
		t.Fatalf("the fresh path is not stable: %s vs %s", again, moved)
	}
	for _, c := range []struct {
		name  string
		local string
		perf  []byte
	}{
		{"same BPM", local, expf(130.001)},
		{"no analysis", local, nil},
		{"no row yet", filepath.Join(s.shadowDir(), "peer-2", "New.flac"), expf(129)},
	} {
		if got := s.shadowCopyForBPM(t.Context(), "peer:k", c.local, c.perf); got != c.local {
			t.Errorf("%s: copy moved to %s", c.name, got)
		}
	}
}
