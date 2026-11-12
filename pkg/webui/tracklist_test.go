package webui

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/broadcast"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
	"github.com/Cd1s/EngineX-OS/pkg/tracklist"
)

func setBoardForTest(t *testing.T, compat string) {
	t.Helper()
	old := deviceCompatiblePath
	cf := filepath.Join(t.TempDir(), "compatible")
	if err := os.WriteFile(cf, []byte(compat+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	deviceCompatiblePath = cf
	t.Cleanup(func() { deviceCompatiblePath = old })
}

func TestAllInOneStreamRecordAndAutoTracklist(t *testing.T) {
	setBoardForTest(t, "inmusic,jc11")

	mediaRoot := t.TempDir()
	usbSessions := filepath.Join(mediaRoot, "USB1", "Sessions")
	if err := os.MkdirAll(usbSessions, 0o755); err != nil {
		t.Fatal(err)
	}
	pcmPath := filepath.Join(t.TempDir(), "master.pcm")
	// 1 second of 48kHz stereo 16-bit PCM
	pcm := make([]byte, 48000*4)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 1024)
	}
	if err := os.WriteFile(pcmPath, pcm, 0o600); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	uiDir := t.TempDir()
	guard := resourceguard.New(resourceguard.Config{Disabled: true})
	jobs := NewJobs(Pipeline{Guard: guard})
	t.Cleanup(func() { jobs.Close() })

	s := New(Config{
		AccessToken: "tok",
		StateDir:    stateDir,
		UIDir:       uiDir,
		MediaRoot:   mediaRoot,
	}, jobs)
	t.Cleanup(func() { s.Close() })
	s.RegisterBroadcastRoutes()

	authed := func(method, path string, payload any) *http.Response {
		t.Helper()
		var body io.Reader
		if payload != nil {
			raw, _ := json.Marshal(payload)
			body = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer tok")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Result()
	}

	// 1. All-in-one reports audioSupported=true and stream_and_tracklist mode.
	resp := authed("GET", "/api/broadcast", nil)
	var st broadcast.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.AudioSupported == nil || !*st.AudioSupported || st.Mode != "stream_and_tracklist" {
		t.Fatalf("expected all-in-one audioSupported=true stream_and_tracklist, got %+v", st)
	}

	// 2. Start one-tap live stream with local WAV mix recording enabled.
	resp = authed("POST", "/api/broadcast", map[string]any{
		"enabled":    true,
		"streamName": "Friday Club Mix",
		"inputPath":  pcmPath,
		"recordMix":  true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start broadcast: %d", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if !st.Enabled || !st.RecordMix || st.RecordPath == "" {
		t.Fatalf("expected active broadcast with RecordPath, got %+v", st)
	}
	recWAV := st.RecordPath

	// Give the PCM capture loop a moment to write frames to the local WAV file.
	time.Sleep(80 * time.Millisecond)

	// 3. Post audible deck metadata (including a streaming track) and verify OBS nowplaying.txt.
	sess, ok := s.TracklistManager().Active()
	if !ok {
		t.Fatal("expected active tracklist session on broadcast start")
	}
	t1 := sess.StartedAt.Add(500 * time.Millisecond).UnixMilli()
	t2 := sess.StartedAt.Add(330 * time.Second).UnixMilli()
	authed("POST", "/api/broadcast/metadata", map[string]any{
		"artist":  "Bicep",
		"title":   "Glue",
		"deck":    1,
		"bpm":     130.0,
		"key":     "8A",
		"onsetMs": t1,
	})
	authed("POST", "/api/broadcast/metadata", map[string]any{
		"artist":    "Four Tet",
		"title":     "Looking at Your Pager",
		"deck":      2,
		"bpm":       132.0,
		"key":       "9A",
		"streaming": true,
		"onsetMs":   t2,
	})

	obsRec := httptest.NewRecorder()
	s.ServeHTTP(obsRec, httptest.NewRequest("GET", "/live/nowplaying.txt", nil))
	if got := strings.TrimSpace(obsRec.Body.String()); got != "Four Tet - Looking at Your Pager" {
		t.Fatalf("unexpected /live/nowplaying.txt: %q", got)
	}

	// 4. Stop broadcast -> WAV header patched and companion .txt / .cue / .mixcloud.txt / .json written.
	resp = authed("POST", "/api/broadcast?enabled=0", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop broadcast: %d", resp.StatusCode)
	}
	wavBytes, err := os.ReadFile(recWAV)
	if err != nil {
		t.Fatalf("read recorded WAV: %v", err)
	}
	if len(wavBytes) <= 44 || string(wavBytes[:4]) != "RIFF" {
		t.Fatalf("expected non-empty RIFF WAV recording, len=%d", len(wavBytes))
	}
	dataLen := binary.LittleEndian.Uint32(wavBytes[40:44])
	if int(dataLen) != len(wavBytes)-44 {
		t.Fatalf("WAV data chunk length mismatch: header=%d actual=%d", dataLen, len(wavBytes)-44)
	}
	base := strings.TrimSuffix(recWAV, ".wav")
	for _, ext := range []string{".txt", ".cue", ".mixcloud.txt", ".json"} {
		if _, err := os.Stat(base + ext); err != nil {
			t.Fatalf("missing recording sidecar %s: %v", base+ext, err)
		}
	}
	cueRaw, _ := os.ReadFile(base + ".cue")
	if !strings.Contains(string(cueRaw), `FILE "`+filepath.Base(recWAV)+`" WAVE`) || !strings.Contains(string(cueRaw), "INDEX 01 05:30:00") {
		t.Fatalf("unexpected recording CUE sidecar:\n%s", string(cueRaw))
	}

	// 5. Stock Engine recorder auto-tracklist trigger writes sidecars to <MediaRoot>/USB1/Sessions/.
	authed("POST", "/api/ui?autoTracklist=1", nil)
	authed("POST", "/api/tracklist?recorderActive=1", map[string]any{"title": "Recorder Set"})
	authed("POST", "/api/broadcast/metadata", map[string]any{
		"artist": "Overmono",
		"title":  "So U Kno",
		"deck":   1,
	})
	authed("POST", "/api/tracklist?recorderActive=0", nil)
	matches, _ := filepath.Glob(filepath.Join(usbSessions, "*.cue"))
	if len(matches) == 0 {
		t.Fatalf("expected stock recorder sidecars written under %s", usbSessions)
	}
}

func TestStandaloneTwoPlayerSimulated30MinSet(t *testing.T) {
	setBoardForTest(t, "inmusic,jp07")

	mkPlayer := func(role string) (*Server, *httptest.Server) {
		stateDir := t.TempDir()
		uiDir := t.TempDir()
		if err := os.MkdirAll(uiDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(uiDir, "settings.conf"), []byte("four_deck_waveform=1\nfour_deck_role="+role+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		guard := resourceguard.New(resourceguard.Config{Disabled: true})
		jobs := NewJobs(Pipeline{Guard: guard})
		t.Cleanup(func() { jobs.Close() })
		srv := New(Config{
			AccessToken: "tok",
			StateDir:    stateDir,
			UIDir:       uiDir,
		}, jobs)
		srv.RegisterBroadcastRoutes()
		ts := httptest.NewServer(srv)
		t.Cleanup(func() {
			ts.Close()
			srv.Close()
		})
		return srv, ts
	}

	p1, ts1 := mkPlayer("13")
	p2, ts2 := mkPlayer("24")

	// Register each player as an active LAN peer of the other (matching #49 shadow discovery).
	now := time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)
	p1.shadow.mu.Lock()
	p1.shadow.peers[ts2.Listener.Addr().String()] = now
	p1.shadow.mu.Unlock()
	p2.shadow.mu.Lock()
	p2.shadow.peers[ts1.Listener.Addr().String()] = now
	p2.shadow.mu.Unlock()

	authed := func(srv *Server, method, path string, payload any) *http.Response {
		t.Helper()
		var body io.Reader
		if payload != nil {
			raw, _ := json.Marshal(payload)
			body = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer tok")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Result()
	}

	// 1. Standalone player explicitly reports audioSupported=false and refuses /live.wav with 503.
	resp := authed(p1, "GET", "/api/broadcast", nil)
	var st broadcast.Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if st.AudioSupported == nil || *st.AudioSupported || st.Mode != "tracklist_only" {
		t.Fatalf("standalone player must report audioSupported=false tracklist_only, got %+v", st)
	}
	liveRec := httptest.NewRecorder()
	p1.ServeHTTP(liveRec, httptest.NewRequest("GET", "/live.wav", nil))
	if liveRec.Code != http.StatusServiceUnavailable || !strings.Contains(liveRec.Body.String(), "no internal master mix") {
		t.Fatalf("expected 503 no internal master mix on /live.wav, got %d %q", liveRec.Code, liveRec.Body.String())
	}

	// 2. Start shared Set Tracklist from Player 1; verify Player 2 also activates via /peer/tracklist!
	sess := p1.TracklistManager().Start(now, "30m B2B Club Set", "tracklist_only", "")
	p1.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "sync", Session: sess})
	if _, activeOnP2 := p2.TracklistManager().Active(); !activeOnP2 {
		t.Fatalf("expected Player 2 to receive active session via peer sync")
	}

	setOwn := func(srv *Server, decks []deckState) {
		srv.shadow.mu.Lock()
		srv.shadow.own = append([]deckState(nil), decks...)
		srv.shadow.mu.Unlock()
	}
	syncPeerState := func(at time.Time) {
		// Mirror own physical decks to partner's peerDecks (just as /peer/state does on LAN).
		p1.shadow.mu.Lock()
		own1 := append([]deckState(nil), p1.shadow.own...)
		p1.shadow.mu.Unlock()
		p2.shadow.mu.Lock()
		own2 := append([]deckState(nil), p2.shadow.own...)
		p2.shadow.peerDecks = map[int]peerDeck{}
		for _, d := range own1 {
			p2.shadow.peerDecks[d.Deck] = peerDeck{deckState: d, at: at}
		}
		p2.shadow.mu.Unlock()
		p1.shadow.mu.Lock()
		p1.shadow.peerDecks = map[int]peerDeck{}
		for _, d := range own2 {
			p1.shadow.peerDecks[d.Deck] = peerDeck{deckState: d, at: at}
		}
		p1.shadow.mu.Unlock()
		// Keep peer expiry fresh across simulated 30-minute clock jumps.
		p1.shadow.mu.Lock()
		p1.shadow.peers[ts2.Listener.Addr().String()] = time.Now()
		p1.shadow.mu.Unlock()
		p2.shadow.mu.Lock()
		p2.shadow.peers[ts1.Listener.Addr().String()] = time.Now()
		p2.shadow.mu.Unlock()
	}

	step := func(at time.Time, x1850 map[string]float64, x1850Assign map[string]string) {
		if x1850 != nil {
			for k, v := range x1850 {
				p1.StageLinQMonitor().ApplyStateMap("Denon DJ X1850", k, map[string]any{"value": v}, at)
			}
			for k, s := range x1850Assign {
				p1.StageLinQMonitor().ApplyStateMap("Denon DJ X1850", k, map[string]any{"string": s}, at)
			}
		}
		syncPeerState(at)
		p1.ObserveStandaloneTracklist(at)
	}

	assigns := map[string]string{
		"/Mixer/ChannelAssignment1": "A",
		"/Mixer/ChannelAssignment2": "B",
		"/Mixer/ChannelAssignment3": "Thru",
		"/Mixer/ChannelAssignment4": "Thru",
	}

	// 00:00 — Player 1 Deck 1 on air (CH1=1.0), while Player 2 Deck 2 cues in headphones (CH2=0.0).
	setOwn(p1, []deckState{{Deck: 1, Playing: true, Artist: "Deadmau5", Title: "Strobe", BPM: 128, KeyName: "9A"}})
	setOwn(p2, []deckState{{Deck: 2, Playing: true, Artist: "Headphone", Title: "Preview Track", BPM: 128, KeyName: "9A"}})
	for ms := 0; ms <= 2000; ms += 500 {
		step(now.Add(time.Duration(ms)*time.Millisecond), map[string]float64{
			"/Mixer/CH1faderPosition":   1.0,
			"/Mixer/CH2faderPosition":   0.0,
			"/Mixer/CH3faderPosition":   0.0,
			"/Mixer/CH4faderPosition":   0.0,
			"/Mixer/CrossfaderPosition": 0.5,
		}, assigns)
	}

	// 05:30 (330s) — Player 2 loads "Eric Prydz - Opus" on Deck 2 and brings CH2 fader up!
	t0530 := now.Add(330 * time.Second)
	setOwn(p2, []deckState{{Deck: 2, Playing: true, Artist: "Eric Prydz", Title: "Opus", BPM: 126, KeyName: "6A"}})
	for ms := 0; ms <= 2000; ms += 500 {
		step(t0530.Add(time.Duration(ms)*time.Millisecond), map[string]float64{
			"/Mixer/CH1faderPosition":   0.8,
			"/Mixer/CH2faderPosition":   0.9,
			"/Mixer/CrossfaderPosition": 0.5,
		}, assigns)
	}

	// 12:45 (765s) — Player 1 Layer B (Deck 3) brings in "CamelPhat - Cola", CH1 pulled down.
	// Also verify crossfader cut: CH2 is assigned to B, crossfader slammed left (0.0) cuts CH2.
	t1245 := now.Add(765 * time.Second)
	setOwn(p1, []deckState{
		{Deck: 1, Playing: false, Artist: "Deadmau5", Title: "Strobe"},
		{Deck: 3, Playing: true, Artist: "CamelPhat", Title: "Cola", BPM: 124, KeyName: "5A"},
	})
	setOwn(p2, []deckState{{Deck: 2, Playing: true, Artist: "CrossfaderCut", Title: "MutedOnSideB", BPM: 124}})
	for ms := 0; ms <= 2000; ms += 500 {
		step(t1245.Add(time.Duration(ms)*time.Millisecond), map[string]float64{
			"/Mixer/CH1faderPosition":   0.0,
			"/Mixer/CH2faderPosition":   1.0, // upfader up, but crossfader at 0.0 cuts Side B!
			"/Mixer/CH3faderPosition":   1.0, // Thru ignores crossfader -> on air!
			"/Mixer/CrossfaderPosition": 0.0,
		}, assigns)
	}

	// 21:10 (1270s) — Player 2 Layer B (Deck 4) brings in a streaming track "Peggy Gou - It Goes Like".
	t2110 := now.Add(1270 * time.Second)
	setOwn(p2, []deckState{
		{Deck: 4, Playing: true, Artist: "Peggy Gou", Title: "It Goes Like", BPM: 130, KeyName: "7A", Net: "http://127.0.0.1:8080/stream/tidal/99"},
	})
	for ms := 0; ms <= 2000; ms += 500 {
		step(t2110.Add(time.Duration(ms)*time.Millisecond), map[string]float64{
			"/Mixer/CH3faderPosition":   0.2,
			"/Mixer/CH4faderPosition":   1.0,
			"/Mixer/CrossfaderPosition": 0.5,
		}, assigns)
	}

	// 28:05 (1685s) — X1850 ethernet cable unplugged (>10s stale) -> falls back to sustained play duration (10s), backdated to 28:05!
	t2805 := now.Add(1685 * time.Second)
	setOwn(p1, []deckState{{Deck: 1, Playing: true, Artist: "Rufus Du Sol", Title: "Innerbloom", BPM: 122, KeyName: "4A"}})
	setOwn(p2, nil)
	for sec := 0; sec <= 11; sec++ {
		step(t2805.Add(time.Duration(sec)*time.Second), nil, nil)
	}

	// Wait briefly for async peer-sync goroutines to deliver to Player 2.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s2, ok := p2.TracklistManager().Latest(); ok && len(s2.Entries) == 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Stop from Player 2 via HTTP API and verify finalization syncs back to Player 1!
	stopResp := authed(p2, "POST", "/api/broadcast?enabled=0", nil)
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop on p2: %d", stopResp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	want := []struct {
		off    float64
		artist string
		title  string
		label  string
	}{
		{0, "Deadmau5", "Strobe", "Player 1 · Deck 1"},
		{330, "Eric Prydz", "Opus", "Player 2 · Deck 2"},
		{765, "CamelPhat", "Cola", "Player 1 · Deck 3"},
		{1270, "Peggy Gou", "It Goes Like", "Player 2 · Deck 4"},
		{1685, "Rufus Du Sol", "Innerbloom", "Player 1 · Deck 1"},
	}

	// Both Player 1 (:8080) and Player 2 (:8080) must serve the exact same 5-track merged set!
	for idx, srv := range []*Server{p1, p2} {
		latest, ok := srv.TracklistManager().Latest()
		if !ok {
			t.Fatalf("player %d missing latest session", idx+1)
		}
		if !latest.StreamingNotice {
			t.Fatalf("player %d expected StreamingNotice=true", idx+1)
		}
		if len(latest.Entries) != len(want) {
			t.Fatalf("player %d entries=%d want %d: %+v", idx+1, len(latest.Entries), len(want), latest.Entries)
		}
		for i, w := range want {
			got := latest.Entries[i]
			if math.Abs(got.OffsetSec-w.off) > 1.0 || got.Artist != w.artist || got.Title != w.title || got.SourceLabel != w.label {
				t.Fatalf("player %d entry[%d] = %+v, want %+v", idx+1, i, got, w)
			}
		}

		// Verify all 4 HTTP export formats from each player's console
		for _, fmtName := range []string{"txt", "cue", "mixcloud", "json"} {
			r := authed(srv, "GET", "/api/tracklist/export?format="+fmtName, nil)
			if r.StatusCode != http.StatusOK {
				t.Fatalf("player %d export %s: status %d", idx+1, fmtName, r.StatusCode)
			}
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), "Eric Prydz") || !strings.Contains(string(raw), "Innerbloom") {
				t.Fatalf("player %d export %s missing tracks:\n%s", idx+1, fmtName, string(raw))
			}
			if strings.Contains(string(raw), "Preview Track") || strings.Contains(string(raw), "MutedOnSideB") {
				t.Fatalf("player %d export %s leaked unplayed track:\n%s", idx+1, fmtName, string(raw))
			}
		}
	}
}
