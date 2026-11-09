package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

type displayNFSFixture struct {
	mu    sync.Mutex
	files map[string][]byte
	reads map[string]int
	mtime uint32
	size  uint32
}

func newDisplayNFSFixture() *displayNFSFixture {
	// The standard fixture intentionally has only four detail bins. Extend its
	// PWV7 to a real two-second, 150 Hz timeline matching the PDB's duration.
	w := make([]byte, 12+300*3)
	binary.BigEndian.PutUint32(w, 3)
	binary.BigEndian.PutUint32(w[4:], 300)
	for i := 12; i < len(w); i++ {
		w[i] = byte(i%128 + 1)
	}
	return &displayNFSFixture{files: map[string][]byte{
		"PIONEER/rekordbox/export.pdb":      testfixture.PDB(),
		"PIONEER/USBANLZ/P001/ANLZ0000.DAT": testfixture.DAT(),
		"PIONEER/USBANLZ/P001/ANLZ0000.EXT": testfixture.EXT(),
		"PIONEER/USBANLZ/P001/ANLZ0000.2EX": testfixture.PMAI(testfixture.Section("PWV7", 24, w)),
	}, reads: map[string]int{}, mtime: 1}
}

func (f *displayNFSFixture) Lookup(_ context.Context, export, path string) ([]byte, prolink.NFSFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path]
	if !ok || export != "/C/" && export != "/B/" {
		return nil, prolink.NFSFile{}, fmt.Errorf("missing %s", path)
	}
	n := uint32(len(b))
	if f.size != 0 {
		n = f.size
	}
	return nil, prolink.NFSFile{Size: n, MTime: f.mtime}, nil
}

func (f *displayNFSFixture) Fetch(ctx context.Context, export, path string, limit uint32) ([]byte, prolink.NFSFile, error) {
	_, st, err := f.Lookup(ctx, export, path)
	if err != nil {
		return nil, st, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[path]++
	if st.Size > limit {
		return nil, st, fmt.Errorf("too large")
	}
	return append([]byte(nil), f.files[path]...), st, nil
}

func TestCDJDisplayPerformanceAndCache(t *testing.T) {
	f := newDisplayNFSFixture()
	var cache prolink.DisplayAnalysisCache
	dir := t.TempDir()
	tk, a, err := cache.Read(t.Context(), f, "192.0.2.1", 3, 1, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Independent direct parsing/conversion of exactly the served ANLZ files.
	lib, err := rekordbox.ParsePDB(bytes.NewReader(testfixture.PDB()), int64(len(testfixture.PDB())))
	if err != nil {
		t.Fatal(err)
	}
	var direct rekordbox.Analysis
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		part, e := rekordbox.ParseAnalysis(f.files["PIONEER/USBANLZ/P001/ANLZ0000"+ext])
		if e != nil {
			t.Fatal(e)
		}
		direct.Merge(part)
	}
	got, err := enginedb.EncodePerformance(tk, a)
	if err != nil {
		t.Fatal(err)
	}
	want, err := enginedb.EncodePerformance(lib.Tracks[0], &direct)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || !bytes.Equal(got.Beats, want.Beats) || !bytes.Equal(got.Overview, want.Overview) || !bytes.Equal(got.QuickCues, want.QuickCues) || !bytes.Equal(got.Loops, want.Loops) {
		t.Fatal("PerformanceData differs from direct conversion")
	}
	asset, err := cdjDisplayAsset(tk, a, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(asset.Beats) != 4 || len(asset.Cues) == 0 || len(asset.Loops) == 0 || !asset.Detail || asset.DetailSource != "rekordbox" {
		t.Fatalf("missing analysis: %+v", asset)
	}
	wave := enginedb.DisplayWaveFromRekordbox(&direct, asset.SampleRate)
	if !reflect.DeepEqual(asset.Waveform, wave) {
		t.Fatal("CDJ-3000 three-band waveform differs")
	}
	if _, _, err = cache.Read(t.Context(), f, "192.0.2.1", 3, 1, dir); err != nil {
		t.Fatal(err)
	}
	const pdb = "PIONEER/rekordbox/export.pdb"
	if f.reads[pdb] != 1 {
		t.Fatal("unchanged PDB re-downloaded")
	}
	f.mtime++
	if _, _, err = cache.Read(t.Context(), f, "192.0.2.1", 3, 1, dir); err != nil {
		t.Fatal(err)
	}
	if f.reads[pdb] != 2 {
		t.Fatal("changed PDB not refreshed")
	}
	if _, _, err = cache.Read(t.Context(), f, "192.0.2.1", 2, 1, dir); err != nil {
		t.Fatal(err)
	}
	if f.reads[pdb] != 3 {
		t.Fatal("SD and USB caches alias")
	}
	for path := range f.reads {
		if strings.HasPrefix(path, "Music/") {
			t.Fatal("audio was fetched")
		}
	}
	// Many source slots cannot accumulate unbounded disk entries.
	for i := 0; i < 10; i++ {
		if _, _, err = cache.Read(t.Context(), f, fmt.Sprint(i), 3, 1, dir); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*.pdb"))
	if len(entries) > 8 {
		t.Fatalf("cache has %d entries", len(entries))
	}
	var total int64
	filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			st, _ := d.Info()
			total += st.Size()
		}
		return e
	})
	if total > prolink.DisplayCacheLimit {
		t.Fatal("cache exceeds byte budget")
	}
}

func TestCDJDisplayRejectsRemoteData(t *testing.T) {
	for _, mode := range []string{"oversized", "bad-pdb", "bad-anlz", "unsafe-path", "missing", "no-analysis"} {
		t.Run(mode, func(t *testing.T) {
			f := newDisplayNFSFixture()
			id := uint32(1)
			switch mode {
			case "oversized":
				f.size = prolink.DisplayPDBLimit + 1
			case "bad-pdb":
				f.files["PIONEER/rekordbox/export.pdb"] = []byte("garbage")
			case "bad-anlz":
				f.files["PIONEER/USBANLZ/P001/ANLZ0000.DAT"] = []byte("PMAI")
			case "unsafe-path":
				p := f.files["PIONEER/rekordbox/export.pdb"]
				f.files["PIONEER/rekordbox/export.pdb"] = bytes.ReplaceAll(p, []byte("P\x00I\x00O\x00N\x00E\x00E\x00R"), []byte(".\x00.\x00/\x00.\x00.\x00/\x00x"))
			case "missing":
				id = 999
			case "no-analysis":
				id = 2
			}
			var c prolink.DisplayAnalysisCache
			if _, _, err := c.Read(t.Context(), f, "192.0.2.1", 3, id, t.TempDir()); err == nil {
				t.Fatal("unusable input accepted")
			}
		})
	}
}

func TestAllInOneDisplayRole12(t *testing.T) {
	oldRole, oldCompat := fourDeckRolePath, deviceCompatiblePath
	fourDeckRolePath, deviceCompatiblePath = filepath.Join(t.TempDir(), "absent-role"), filepath.Join(t.TempDir(), "compatible")
	t.Cleanup(func() { fourDeckRolePath, deviceCompatiblePath = oldRole, oldCompat })
	for _, board := range []string{"inmusic,jc11", "inmusic,jc11s", "inmusic,jp21"} {
		t.Run(board, func(t *testing.T) {
			if err := os.WriteFile(deviceCompatiblePath, []byte(board), 0600); err != nil {
				t.Fatal(err)
			}
			s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
			srv := httptest.NewServer(s)
			defer srv.Close()
			uiPost(t, srv.URL, "fourDeckWaveform=1")
			if role := s.activeRole(uiSettings{FourDeckWaveform: true}); role != "12" {
				t.Fatalf("role %q", role)
			}
			p := peerDeck{deckState: deckState{Deck: 1, Key: "track-a"}, ip: "192.0.2.2", at: time.Now()}
			s.shadow.peerDecks[1] = p
			a := testDisplayAsset()
			wave, _ := renderShadowWave(a.Overview, 8, 32)
			s.shadow.display.entries = map[string]*shadowDisplayEntry{displayPeerID(p): {asset: a, wave: wave, overview: wave}}
			f := newDisplayNFSFixture()
			s.shadow.cdjReader = func(net.IP) prolink.AnalysisReader { return f }
			dev := prolink.Device{Name: "CDJ-3000", Number: 2, Address: "192.0.2.3", SourcePlayer: 2, SourceSlot: 3, TrackType: 1, TrackID: 1, LastSeen: time.Now()}
			b, _ := json.Marshal(prolink.Snapshot{Devices: []prolink.Device{dev}})
			if err := os.WriteFile(filepath.Join(s.cfg.StateDir, "prolink.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			var decks []deckState
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				decks = s.shadowComposeDisplay("12", time.Now())
				if len(decks) == 2 && !decks[1].MetadataOnly {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(decks) != 2 || decks[0].Deck != 3 || decks[1].Deck != 4 || decks[1].MetadataOnly {
				t.Fatalf("role 12 mapping: %+v", decks)
			}
			for _, d := range decks {
				if d.Path != "" || d.Ext != "" || d.Net != "" {
					t.Fatal("display-only exposed audio path")
				}
				req := httptest.NewRequest(http.MethodGet, "/v1/sc5000/shadow/display?deck="+fmt.Sprint(d.Deck)+"&key="+url.QueryEscape(d.Key), nil)
				rec := httptest.NewRecorder()
				s.shadowDisplayGet(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "waveformUrl") {
					t.Fatalf("display endpoint %d: %.200s", rec.Code, rec.Body.String())
				}
			}
			if len(s.shadow.files) != 0 || len(s.shadow.busy) != 0 {
				t.Fatal("legacy audio fetch used")
			}
			for _, slot := range []byte{0, 1, 4} {
				dev.SourceSlot = slot
				if cdjDisplayHost(dev, []prolink.Device{dev}) != nil {
					t.Fatal("non-USB/SD accepted")
				}
			}
			dev.SourceSlot = 3
			dev.TrackType = 2
			if cdjDisplayHost(dev, []prolink.Device{dev}) != nil {
				t.Fatal("non-rekordbox accepted")
			}
		})
	}
}

func TestCDJDisplayRefreshVersionAndEject(t *testing.T) {
	oldRole := fourDeckRolePath
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	defer func() { fourDeckRolePath = oldRole }()
	if err := os.WriteFile(fourDeckRolePath, []byte("13"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{UIDir: filepath.Join(t.TempDir(), "ui"), StateDir: t.TempDir()}, nil)
	s.shadow.running = true // exercise the HTTP API without starting LAN beacons
	srv := httptest.NewServer(s)
	defer srv.Close()
	uiPost(t, srv.URL, "fourDeckWaveform=1")
	f := newDisplayNFSFixture()
	s.shadow.cdjReader = func(net.IP) prolink.AnalysisReader { return f }
	dev := prolink.Device{Name: "CDJ-3000", Number: 2, Address: "192.0.2.3", SourcePlayer: 2, SourceSlot: 3, TrackType: 1, TrackID: 1, LastSeen: time.Now()}
	writeState := func() {
		t.Helper()
		b, _ := json.Marshal(prolink.Snapshot{Devices: []prolink.Device{dev}})
		if err := os.WriteFile(filepath.Join(s.cfg.StateDir, "prolink.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeState()
	wait := func(previous string) deckState {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			ds := s.shadowComposeDisplay("13", time.Now())
			if len(ds) == 1 && !ds[0].MetadataOnly && ds[0].Key != previous {
				return ds[0]
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("CDJ asset did not refresh")
		return deckState{}
	}
	first := wait("")
	f.mu.Lock()
	f.mtime++
	dat := append([]byte(nil), f.files["PIONEER/USBANLZ/P001/ANLZ0000.DAT"]...)
	off := bytes.Index(dat, []byte("PQTZ"))
	binary.BigEndian.PutUint32(dat[off+24+3*8+4:], 1600)
	f.files["PIONEER/USBANLZ/P001/ANLZ0000.DAT"] = dat
	f.mu.Unlock()
	c := &s.shadow.display
	c.mu.Lock()
	c.entries[first.displayID].retry = time.Time{}
	c.mu.Unlock()
	second := wait(first.Key)
	if first.displayID != second.displayID {
		t.Fatal("track identity unexpectedly changed")
	}
	get := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/sc5000/shadow/display?deck=2&key="+url.QueryEscape(key), nil)
		w := httptest.NewRecorder()
		s.shadowDisplayGet(w, r)
		return w
	}
	if rec := get(first.Key); rec.Code != 404 {
		t.Fatalf("stale asset key accepted: %d", rec.Code)
	}
	if rec := get(second.Key); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ms":1600`) {
		t.Fatalf("new beat grid unavailable: %d %.200s", rec.Code, rec.Body.String())
	}
	// The unchanged peer version must not suppress continuous CDJ samples.
	rec := httptest.NewRecorder()
	s.shadowGet(rec, httptest.NewRequest(http.MethodGet, "/v1/sc5000/shadow", nil))
	if rec.Header().Get("X-Shadow-State") != "" {
		t.Fatal("CDJ positions use peer version cache")
	}
	dev.TrackID = 0
	writeState()
	s.shadowComposeDisplay("13", time.Now())
	c.mu.Lock()
	remaining := len(c.entries)
	c.mu.Unlock()
	if remaining != 0 {
		t.Fatal("ejected CDJ retained assets")
	}
	dev.TrackID = 1
	writeState()
	if ds := s.shadowComposeDisplay("13", time.Now()); len(ds) != 1 || !ds[0].MetadataOnly {
		t.Fatal("same-ID reload reused unvalidated assets")
	}
	wait("")
}
