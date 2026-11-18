package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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

// gridBeatData is Engine's beatData for a 4 s track at 8000 Hz: 120 BPM for 4 beats, then 140 BPM (three markers).
func gridBeatData(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	put := func(order binary.ByteOrder, v any) { binary.Write(&b, order, v) }
	put(binary.BigEndian, float64(8000))
	put(binary.BigEndian, float64(32000))
	b.WriteByte(1)
	for g := 0; g < 2; g++ {
		put(binary.BigEndian, uint64(3))
		for _, m := range [][2]float64{{0, 0}, {16000, 4}, {16000 + 4*8000*60/140, 8}} {
			put(binary.LittleEndian, m[0])
			put(binary.LittleEndian, int64(m[1]))
			put(binary.LittleEndian, uint32(0))
			put(binary.LittleEndian, uint32(0))
		}
	}
	q, err := enginedb.QCompress(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestCueGridServesTheSavedMarkersReadOnly(t *testing.T) {
	file, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Contents/song.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("RIFFxxxxWAVE"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: 8000}, &rekordbox.Analysis{
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/song.wav','song.wav',120,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?`, p.TrackData, p.Overview, gridBeatData(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	s.shadow.mediaRoot = filepath.Dir(root)
	get := func(np string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.cueGridGet(rec, httptest.NewRequest(http.MethodGet, "/v1/enginex/grid?net="+url.QueryEscape(np), nil))
		return rec
	}
	rec := get("net://deck/" + filepath.Base(root) + "/Contents/song.wav")
	var reply cueGridReply
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.Rate != 8000 || len(reply.Markers) != 3 ||
		reply.Markers[1] != (enginedb.GridMarker{Sample: 16000, Beat: 4}) || reply.Markers[2].Beat != 8 {
		t.Fatalf("markers of a tempo change: %d %s", rec.Code, rec.Body.String())
	}
	// Not a drive path, a path out of the drive, a track Engine has not analysed: no answer, the pack keeps counting by tempo.
	for _, np := range []string{"", "/etc/passwd", "net://deck/" + filepath.Base(root) + "/../../etc/passwd", "net://deck/" + filepath.Base(root) + "/Contents/none.wav"} {
		if c := get(np).Code; c != http.StatusNotFound {
			t.Fatalf("%q answered %d", np, c)
		}
	}
	after, _ := os.ReadFile(file)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("the grid read wrote Engine's database")
	}
}
