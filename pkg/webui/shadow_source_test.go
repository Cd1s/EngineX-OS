package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestSourceEnvelopeCompletesWithoutChangingSavedGrid(t *testing.T) {
	file, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Contents/song.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const rate = 8000
	const samples = 4 * rate
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+samples*2))
	wav.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, v)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(samples*2))
	for i := 0; i < samples; i++ {
		binary.Write(&wav, binary.LittleEndian, int16(16000*math.Sin(2*math.Pi*100*float64(i)/rate)))
	}
	if err := os.WriteFile(path, wav.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: rate}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}, {TimeMS: 750, BPM: 120, Number: 2}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/song.wav','song.wav',120,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mediaRoot = filepath.Dir(root)
	s.shadow.own = []deckState{{Deck: 1, Key: "source-a", Path: path}}
	a, err := s.sourceDisplayAsset(context.Background(), path, "source-a")
	if err != nil || !a.DetailPending || a.Detail {
		t.Fatalf("initial overview: %v %+v", err, a)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, err = s.sourceDisplayAsset(context.Background(), path, "source-a")
		if err != nil {
			t.Fatal(err)
		}
		if !a.DetailPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source envelope never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !a.Detail || a.DetailSource != "source-envelope" || a.Waveform == nil || len(a.Waveform.Bands)/3 != 4*105 {
		t.Fatalf("detail: %+v", a)
	}
	if len(a.Beats) != 2 || a.Beats[0].MS != 250 || a.Beats[1].MS != 750 {
		t.Fatalf("source grid recomputed: %+v", a.Beats)
	}
	after, _ := os.ReadFile(file)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("waveform worker wrote Engine database")
	}
	s.shadow.sourceDisplay.mu.Lock()
	count := len(s.shadow.sourceDisplay.entries)
	s.shadow.sourceDisplay.mu.Unlock()
	if count != 1 {
		t.Fatalf("source key cache has %d entries", count)
	}
}

func TestSourceEnvelopeDeadlineDoesNotBecomeUnloadRetry(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	ticker := shadowSourceTicker{s: s, key: "not-loaded", deadline: time.Now().Add(-time.Second)}
	if err := ticker.Tick(); err != context.DeadlineExceeded {
		t.Fatalf("deadline classified as unload: %v", err)
	}
	ticker.deadline = time.Now().Add(time.Hour)
	if err := ticker.Tick(); err != context.Canceled {
		t.Fatalf("unload not canceled: %v", err)
	}
}

// The source envelope is PCM full scale, not each band stretched to trackData's
// own band peak: stretched bands stack past the lane edge on most bins.
func TestSourceEnvelopeKeepsPCMScale(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Contents/song.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	const rate = 8000
	const samples = 2 * rate
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+samples*2))
	wav.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&wav, binary.LittleEndian, v)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(samples*2))
	for i := 0; i < samples; i++ {
		binary.Write(&wav, binary.LittleEndian, int16(8192*math.Sin(2*math.Pi*100*float64(i)/rate))) // a quarter of full scale
	}
	if err := os.WriteFile(path, wav.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 2, SampleRate: rate}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}, {TimeMS: 750, BPM: 120, Number: 2}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/song.wav','song.wav',120,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mediaRoot = filepath.Dir(root)
	s.shadow.own = []deckState{{Deck: 1, Key: "source-a", Path: path}}
	var a *shadowDisplayAsset
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if a, err = s.sourceDisplayAsset(context.Background(), path, "source-a"); err != nil {
			t.Fatal(err)
		}
		if !a.DetailPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source envelope never completed")
		}
	}
	if a.Waveform == nil || a.Peaks[0] <= 0 || a.Peaks[0] >= 1 {
		t.Fatalf("detail or trackData peak: %+v", a)
	}
	if low := a.Waveform.Bands[len(a.Waveform.Bands)/2/3*3]; low < 60 || low > 68 {
		t.Fatalf("low band %d for a quarter-scale tone, want ~64 (trackData peaks %v)", low, a.Peaks)
	}
}
