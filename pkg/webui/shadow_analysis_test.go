package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestPeerShadowNativeAnalysis(t *testing.T) {
	_, volume, db := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 128, Number: 1}, {TimeMS: 719, BPM: 128, Number: 2}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops}
	if _, err := db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../song.wav','song.wav',128,1);
		INSERT INTO Track(path,filename) VALUES('../unanalysed.wav','unanalysed.wav')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?
		WHERE trackId=(SELECT id FROM Track WHERE filename='song.wav')`, fields[0], fields[1], fields[2], fields[3], fields[4]); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mediaRoot = filepath.Dir(volume)
	s.shadow.own = []deckState{
		{Deck: 1, Key: "analysed", Path: filepath.Join(volume, "song.wav")},
		{Deck: 2, Key: "pending", Path: filepath.Join(volume, "unanalysed.wav")},
	}
	peer := httptest.NewServer(s.mux)
	defer peer.Close()
	for _, tt := range []struct {
		name, query, magic string
		code               int
	}{
		{"native", "/1/perf?key=analysed&format=native", "RBXA1", 200},
		{"legacy", "/1/perf?key=analysed", "EXPF", 200},
		{"stale key", "/1/perf?key=pending&format=native", "", 409},
		{"unanalysed", "/2/perf?key=pending&format=native", "", 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/peer/decks"+tt.query, nil))
			if w.Code != tt.code || !bytes.HasPrefix(w.Body.Bytes(), []byte(tt.magic)) {
				t.Fatalf("status %d, body %q", w.Code, w.Body.Bytes())
			}
			if tt.magic == "RBXA1" {
				r := bytes.NewReader(w.Body.Bytes()[5:])
				for i, want := range fields {
					var n uint32
					if err := binary.Read(r, binary.LittleEndian, &n); err != nil || int(n) != len(want) {
						t.Fatalf("blob %d length %d, want %d", i, n, len(want))
					}
					got := make([]byte, n)
					if _, err := r.Read(got); err != nil || !bytes.Equal(got, want) {
						t.Fatalf("blob %d changed", i)
					}
				}
				if r.Len() != 0 {
					t.Fatal("unexpected trailing data")
				}
			}
		})
	}
	local := filepath.Join(t.TempDir(), "shadow.wav")
	url := peer.URL + "/peer/decks/1/perf?key=analysed"
	ctx := context.Background()
	why := saveShadowAnalysis(ctx, local, url+"&format=native")
	got, err := os.ReadFile(local + ".analysis")
	if err != nil || !validShadowAnalysis(got) || why != "" {
		t.Fatalf("native response not staged: %v %q", err, why)
	}
	// Engine has loaded the source's track since the import: trackData now has its decoder's
	// sample count, without the 2048 samples of priming it trims, and the grid the import's.
	// Every track loaded for the second time looked like that, and the copy was analysed by
	// the shadow deck itself: no overview at once, and other colours (real decks, 2026-11-19).
	track, err := enginedb.QUncompress(p.TrackData, 4096)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(track[8:], binary.BigEndian.Uint64(track[8:])-2048)
	loaded, err := enginedb.QCompress(track)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=? WHERE trackId=(SELECT id FROM Track WHERE filename='song.wav')`, loaded); err != nil {
		t.Fatal(err)
	}
	if why = saveShadowAnalysis(ctx, local, url+"&format=native"); why != "" {
		t.Fatalf("the row of a track Engine has loaded was not staged: %q", why)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=? WHERE trackId=(SELECT id FROM Track WHERE filename='song.wav')`, p.TrackData); err != nil {
		t.Fatal(err)
	}
	why = saveShadowAnalysis(ctx, local, url) // An old peer ignores the query and serves EXPF.
	if _, err = os.Stat(local + ".analysis"); !os.IsNotExist(err) || why != "not a native analysis" {
		t.Fatalf("legacy response left a native sidecar: %v %q", err, why)
	}
	// The log names the source's answer when it has none.
	if why = saveShadowAnalysis(ctx, local, peer.URL+"/peer/decks/1/perf?key=other&format=native"); why != "http 404" && why != "http 409" {
		t.Fatalf("a source without the track: %q", why)
	}
}
