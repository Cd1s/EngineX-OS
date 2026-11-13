package webui

import (
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

// The deck overview draws a track as r5 did unless this vouches for its scale. It must
// not vouch for an overview an earlier build imported and nothing completed (a read-only
// library, a stick that was never inserted again, a failed scan), and it must for one
// Engine scales or has yet to build.
func TestOverviewScaleVouchesOnlyForBandMaxima(t *testing.T) {
	mdb, stick, db := testlibrary.Create(t)
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 44100}, &rekordbox.Analysis{
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := enginedb.QUncompress(p.Overview, 1<<20)
	if err != nil || raw[len(raw)-3] == 0 {
		t.Fatal("fixture overview has no band maxima", err)
	}
	clear(raw[len(raw)-3:])
	old, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, overview := range map[string][]byte{"old.mp3": old, "done.mp3": p.Overview, "new.mp3": nil, "unreadable.mp3": {0}} {
		if _, err := db.Exec(`INSERT INTO Track(path,filename) VALUES(?,?)`, "../Contents/"+name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE PerformanceData SET overviewWaveFormData=? WHERE trackId=(SELECT id FROM Track WHERE filename=?)`, overview, name); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(mdb)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.dataRoot, s.shadow.mediaRoot = t.TempDir(), filepath.Dir(stick)
	for path, want := range map[string]int{
		filepath.Join(stick, "Contents/old.mp3"):        http.StatusNotFound,
		filepath.Join(stick, "Contents/done.mp3"):       http.StatusOK,
		filepath.Join(stick, "Contents/new.mp3"):        http.StatusOK,
		filepath.Join(stick, "Contents/unreadable.mp3"): http.StatusNotFound,
		filepath.Join(stick, "Contents/unlisted.mp3"):   http.StatusNotFound,
		"/elsewhere/Contents/done.mp3":                  http.StatusNotFound,
		"":                                              http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		s.overviewScaleGet(w, httptest.NewRequest("GET", "/v1/sc5000/overview-scale?path="+url.QueryEscape(path), nil))
		if w.Code != want {
			t.Errorf("%q: status %d, want %d", path, w.Code, want)
		}
	}
	if after, err := os.ReadFile(mdb); err != nil || string(after) != string(before) {
		t.Error("the library was written to", err)
	}
}
