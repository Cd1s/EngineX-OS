package webui

// Share Decks (#329): a player in the Stock layout with the row on is shown by a console's Line In view without the
// 4-deck layout. It listens for a Line In console's ask, answers with a beacon, gives its decks to a pull with line=1
// and to nobody else, and sends its posts to the consoles that asked: no deck loop, no beacon of its own, no push
// to anybody else. The console asks while a channel is on Line / AUX and says line=1 on its pulls.
//
// These tests use only what the code had before (the settings file, the HTTP routes, the UDP ports), so on the old
// code they compile and fail on what they check.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

const shareAskDatagram = "EngineX-ask-player 1" // what a console with Line In on says beside its beacon

// sharePlayer is an SC5000 (JP07) in the Stock layout, with Share Decks on or off, served like its stream-sync.
func sharePlayer(t *testing.T, share bool, extra string) (*Server, *httptest.Server) {
	t.Helper()
	clSetBoard(t, "jp07")
	dir := t.TempDir()
	conf := extra
	if share {
		conf += "share_decks=1\n"
	}
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(conf), 0o600)
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

// sharePost is what the player's QML posts with Share Decks (tagged with the session the held request told it):
// decks 1 and 2, deck 2 on the active layer, player number 2. fourDeckPost is the same decks as a player in the
// 4-deck layout posts them (untagged).
func sharePost(t *testing.T, s *Server) {
	t.Helper()
	decksPostTo(t, s, "/v1/enginex/decks"+shareTag(s))
}

// shareTag is the tag of the session stream-sync is in now: what the QML's posts carry.
func shareTag(s *Server) string {
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	return "?share=" + strconv.FormatUint(s.shadow.shareSession, 10)
}

func fourDeckPost(t *testing.T, s *Server) { t.Helper(); decksPostTo(t, s, "/v1/enginex/decks") }

func decksPostTo(t *testing.T, s *Server, path string) {
	t.Helper()
	at := time.Now().UnixMilli()
	body := fmt.Sprintf(`[{"deck":1,"title":"P1","link":"{p1ay3r}","playing":false,"positionMs":0,"player":2,"at":%d},`+
		`{"deck":2,"title":"P2","link":"{p1ay3r}","playing":true,"positionMs":5000,"active":true,"player":2,"at":%d}]`, at, at)
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("post: %d", w.Code)
	}
}

func sharePull(t *testing.T, srv *httptest.Server, query string) []string {
	t.Helper()
	var titles []string
	for _, d := range csPull(t, srv, query) {
		titles = append(titles, d.Title)
	}
	return titles
}

// shareAsked is what the player's QML is told by its held request when it believes nobody asks.
func shareAsked(t *testing.T, srv *httptest.Server) bool {
	t.Helper()
	old := consoleAskHold
	consoleAskHold = 50 * time.Millisecond
	defer func() { consoleAskHold = old }()
	resp, err := http.Get(srv.URL + "/v1/enginex/consolesend?asked=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Asked bool }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Asked
}

func TestShareDecksSettingIsPlayerOnlyLiveAndDefaultsOff(t *testing.T) {
	for _, board := range []string{"jp07", "jp08", "jp13", "jp14"} {
		t.Run("player "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["shareDecks"] != false {
				t.Fatalf("default must be off: %v", v["shareDecks"])
			}
			if v := clUI(t, srv.URL, "shareDecks=1"); v["shareDecks"] != true {
				t.Fatalf("on: %v", v["shareDecks"])
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "share_decks=1\n") {
				t.Fatalf("not stored: %q", b)
			}
			// Live: the next pull follows it. A pull without line=1 (a player's) gets nothing from a sharing player.
			sharePost(t, s)
			if got := sharePull(t, srv, ""); len(got) != 0 {
				t.Fatalf("a player's pull got the decks of a sharing player: %v", got)
			}
			// It is its own row: the 4-deck layout neither needs it nor clears it, and there it changes nothing (the
			// player is a peer: its decks go to every peer that pulls, as before).
			clUI(t, srv.URL, "fourDeckWaveform=1")
			fourDeckPost(t, s)
			if got := strings.Join(sharePull(t, srv, ""), ","); got != "P1,P2" {
				t.Fatalf("the 4-deck layout with the row on: %q", got)
			}
			if v := clUI(t, srv.URL, "fourDeckWaveform=0"); v["shareDecks"] != true {
				t.Fatalf("the layout cleared it: %v", v["shareDecks"])
			}
			// Back in the Stock layout the 4-deck layout's last decks are not shared: only what the QML posts now.
			if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
				t.Fatalf("the 4-deck layout's decks were shared: %v", got)
			}
			if v := clUI(t, srv.URL, "shareDecks=0"); v["shareDecks"] != false {
				t.Fatalf("off: %v", v["shareDecks"])
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(b), "share_decks") {
				t.Fatalf("the key stays: %q", b)
			}
			if code, _ := uiPost(t, srv.URL, "shareDecks=maybe"); code != 400 {
				t.Fatalf("a bad value must be refused, got %d", code)
			}
		})
	}
	// Every other board ignores it, also a stored value: consoles, GO, an unknown board.
	for _, board := range []string{"jc11", "jc11s", "jp21", "jp24", "jc16", "nh08", "jp11", ""} {
		t.Run("not on "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("share_decks=1\n"), 0o600)
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["shareDecks"] != false {
				t.Fatalf("a stored value must not be honoured here: %v", v["shareDecks"])
			}
			if v := clUI(t, srv.URL, "shareDecks=1"); v["shareDecks"] != false {
				t.Fatalf("the row must not be turned on here: %v", v["shareDecks"])
			}
		})
	}
	// Settings copy never carries it: one player's choice is not another unit's.
	for _, key := range []string{"shareDecks", "share_decks"} {
		if copyUIAllowed(key) {
			t.Fatalf("%s is on the settings-copy allow-list", key)
		}
	}
	var all map[string]any
	b, _ := json.Marshal(uiSettings{})
	json.Unmarshal(b, &all)
	if _, ok := all["shareDecks"]; !ok {
		t.Fatal("the setting is not in the settings at all")
	}
	all["shareDecks"] = true
	b, _ = json.Marshal(all)
	var v uiSettings
	json.Unmarshal(b, &v)
	for k := range copyUIExport(v) {
		if strings.Contains(strings.ToLower(k), "sharedecks") {
			t.Fatalf("settings copy exports %q", k)
		}
	}
}

func TestShareDecksGivesItsDecksOnlyToALineInConsoleThatAsks(t *testing.T) {
	s, srv := sharePlayer(t, true, "")
	sharePost(t, s) // the QML does not sample before an ask; a post that comes anyway is dropped
	if shareAsked(t, srv) {
		t.Fatal("asked before anybody asked")
	}
	// A Line In console's pull is an ask. Its first answer is empty: nothing was posted for this ask yet.
	if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
		t.Fatalf("the first pull of an ask got decks posted before it: %v", got)
	}
	if !shareAsked(t, srv) {
		t.Fatal("the console's pull was not an ask: the QML would never sample")
	}
	// Asked, the QML samples and posts: the console's next pull gets the decks.
	sharePost(t, s)
	if got := strings.Join(sharePull(t, srv, "?line=1"), ","); got != "P1,P2" {
		t.Fatalf("the Line In console's pull: %q", got)
	}
	// Even now a player that pulls (4-deck layout, or Show Console) and an unknown puller get nothing.
	for _, q := range []string{"", "?console=1"} {
		if got := sharePull(t, srv, q); len(got) != 0 {
			t.Fatalf("pull %q got the decks of a Stock player: %v", q, got)
		}
	}
	// A stall (no post for 3 s): the console keeps a frozen picture, marked stale, as of any player.
	s.shadow.mu.Lock()
	s.shadow.ownAt = time.Now().Add(-3 * time.Second)
	s.shadow.mu.Unlock()
	if got := csPull(t, srv, "?line=1"); len(got) != 2 || !got[0].Stale || !got[1].Stale {
		t.Fatalf("a stalled player's decks: %+v", got)
	}
	// Older than an ask lasts (6 s): the decks of an earlier ask, never shown to a new one.
	s.shadow.mu.Lock()
	s.shadow.ownAt = time.Now().Add(-7 * time.Second)
	s.shadow.mu.Unlock()
	if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
		t.Fatalf("the decks of an earlier ask were given: %v", got)
	}
	// Silence ends it: no ask for 6 s and the QML is told nobody asks.
	s.shadow.mu.Lock()
	for ip := range s.shadow.askers {
		s.shadow.askers[ip] = time.Now().Add(-7 * time.Second)
	}
	s.shadow.mu.Unlock()
	if shareAsked(t, srv) {
		t.Fatal("still asked after 6 s of silence")
	}
	// The row switched off during an ask: what it shared is forgotten, so a console that still pulls gets nothing.
	sharePull(t, srv, "?line=1")
	sharePost(t, s)
	if got := sharePull(t, srv, "?line=1"); len(got) != 2 {
		t.Fatalf("no session to switch off: %v", got)
	}
	if code, _ := uiPost(t, srv.URL, "shareDecks=0"); code != 200 {
		t.Fatalf("row off: %d", code)
	}
	for _, q := range []string{"", "?line=1"} {
		if got := sharePull(t, srv, q); len(got) != 0 {
			t.Fatalf("pull %q after the row went off: %v", q, got)
		}
	}
	// Row never on: as before, a Stock player answers what it has; it never posts, so nothing.
	off, offSrv := sharePlayer(t, false, "")
	if got := sharePull(t, offSrv, "?line=1"); len(got) != 0 {
		t.Fatalf("a player that never posted gave decks: %v", got)
	}
	if shareAsked(t, offSrv) {
		t.Fatal("a player without the row was asked")
	}
	_ = off
	// The 4-deck layout with the row on is as before: its decks go to every peer that pulls.
	four, fourSrv := sharePlayer(t, true, "four_deck_waveform=1\n")
	fourDeckPost(t, four)
	if got := strings.Join(sharePull(t, fourSrv, ""), ","); got != "P1,P2" {
		t.Fatalf("the 4-deck layout changed: %q", got)
	}
}

// shareTrack is a 4 s WAV with analysed performance data (beat grid, overview) in its drive's Engine library, as
// Engine leaves a track it played: what a console's Line In lane draws. It returns the net path Engine posts.
func shareTrack(t *testing.T, s *Server) string {
	t.Helper()
	_, root, db := testlibrary.Create(t)
	path := filepath.Join(root, "Contents/share.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	if err := os.WriteFile(path, wav.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	perf, err := enginedb.EncodePerformance(rekordbox.Track{Duration: 4, SampleRate: rate}, &rekordbox.Analysis{
		Beats:     []rekordbox.Beat{{TimeMS: 250, BPM: 120, Number: 1}, {TimeMS: 750, BPM: 120, Number: 2}},
		Waveforms: []rekordbox.Waveform{{Tag: "PWV7", EntryBytes: 3, Data: []byte{100, 50, 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO Track(path,filename,bpmAnalyzed,isAnalyzed) VALUES('../Contents/share.wav','share.wav',120,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE PerformanceData SET trackData=?,overviewWaveFormData=?,beatData=?,quickCues=?,loops=?`, perf.TrackData, perf.Overview, perf.Beats, perf.QuickCues, perf.Loops); err != nil {
		t.Fatal(err)
	}
	s.shadow.mediaRoot = filepath.Dir(root)
	return "net://u/" + filepath.Base(root) + "/Contents/share.wav"
}

// A console's Line In view fetches only the display data of the track; a Share Decks player gives exactly that, a
// decodable picture (duration, beat grid, overview), and nothing more: no audio file and no performance data, which
// only a shadow copy (the 4-deck layout) is made from.
func TestShareDecksGivesDisplayDataOnlyNeverTheAudio(t *testing.T) {
	for _, share := range []bool{true, false} {
		extra := ""
		if !share {
			extra = "four_deck_waveform=1\n" // the 4-deck layout: a peer copies the track, as before
		}
		s, srv := sharePlayer(t, true, extra)
		np := shareTrack(t, s)
		q := ""
		if share {
			q = "?line=1"
			csPull(t, srv, q) // the console asks first: the QML posts only while asked
		}
		body := `[{"deck":1,"net":"` + np + `","playing":true,"positionMs":1000,"player":2}]`
		path := "/v1/enginex/decks" // the 4-deck layout's post is untagged; the share post carries its session
		if share {
			path += shareTag(s)
		}
		s.decksPost(httptest.NewRecorder(), httptest.NewRequest("POST", path, strings.NewReader(body)))
		decks := csPull(t, srv, q)
		if len(decks) != 1 || decks[0].Key == "" {
			t.Fatalf("share=%t: the deck and its key: %+v", share, decks)
		}
		key := url.QueryEscape(decks[0].Key)
		for _, path := range []string{"/peer/decks/1/audio?key=" + key, "/peer/decks/1/perf?key=" + key} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if share && resp.StatusCode != http.StatusForbidden || !share && resp.StatusCode != http.StatusOK {
				t.Fatalf("share=%t: %s answered %d", share, path, resp.StatusCode)
			}
		}
		resp, err := http.Get(srv.URL + "/peer/decks/1/display?key=" + key)
		if err != nil {
			t.Fatal(err)
		}
		var a shadowDisplayAsset
		err = json.NewDecoder(resp.Body).Decode(&a)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || validateDisplayAsset(&a, decks[0].Key) != nil {
			t.Fatalf("share=%t: the display data: %d %v %v", share, resp.StatusCode, err, validateDisplayAsset(&a, decks[0].Key))
		}
		if a.DurationMS < 3900 || a.DurationMS > 4100 || len(a.Beats) != 2 {
			t.Fatalf("share=%t: the picture is not the track's: duration %v ms, %d beats", share, a.DurationMS, len(a.Beats))
		}
	}
}

// heldBody is a request body that arrives only when released: a QML post still in flight.
type heldBody struct {
	io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.Reader.Read(p)
}

// The review's case (#329 review finding 1): the row switched off while the QML's share post is still arriving.
// The post, tagged as a share's, is dropped: no peer decks, no deck loop, nothing reaches the peer path.
func TestAShareDecksPostInFlightWhenTheRowGoesOffIsDropped(t *testing.T) {
	s, srv := sharePlayer(t, true, "")
	sharePull(t, srv, "?line=1")
	sharePost(t, s)
	if len(sharePull(t, srv, "?line=1")) == 0 {
		t.Fatal("no share session to start from")
	}
	tag := shareTag(s)
	body := &heldBody{
		Reader:  strings.NewReader(fmt.Sprintf(`[{"deck":1,"title":"Late sample","playing":true,"active":true,"at":%d}]`, time.Now().UnixMilli())),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.decksPost(w, httptest.NewRequest(http.MethodPost, "/v1/enginex/decks"+tag, body))
		done <- w.Code
	}()
	select {
	case <-body.started:
	case <-time.After(3 * time.Second):
		close(body.release)
		t.Fatal("the post did not start arriving")
	}
	if v := clUI(t, srv.URL, "shareDecks=0"); v["shareDecks"] != false {
		close(body.release)
		t.Fatalf("row off: %v", v["shareDecks"])
	}
	close(body.release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the post did not finish")
	}
	s.shadow.mu.Lock()
	running := s.shadow.running
	s.shadow.mu.Unlock()
	if got := sharePull(t, srv, ""); running || len(got) != 0 {
		t.Fatalf("the late share post: deck loop running=%t, a peer's pull got %v", running, got)
	}
}

// The review's case (#329 review finding 3): the last ask ran out, then a console asks again. Its first pull gets
// nothing of the earlier session (those decks are gone with it), and the display work on their tracks stops.
func TestANewAskNeverSeesTheDecksOfAnEarlierOne(t *testing.T) {
	s, srv := sharePlayer(t, true, "")
	sharePull(t, srv, "?line=1")
	sharePost(t, s)
	const key = "share-earlier-track"
	s.shadow.mu.Lock()
	s.shadow.own[0].Key, s.shadow.own[0].Path = key, "/earlier/track.wav"
	s.shadow.mu.Unlock()
	if !s.shadowSourceLoaded(key) {
		t.Fatal("the display work is not eligible during the session")
	}
	s.shadow.mu.Lock()
	now := time.Now()
	for ip := range s.shadow.askers {
		s.shadow.askers[ip] = now.Add(-shadowPeerSeenTimeout - time.Second)
	}
	s.shadow.ownAt = now.Add(-3 * time.Second) // within an ask's life: only the ended session makes it old
	asked := s.shadow.askedNow(now)
	s.shadow.mu.Unlock()
	if asked {
		t.Fatal("the ask did not run out")
	}
	if s.shadowSourceLoaded(key) {
		t.Fatal("the display work goes on after the last ask ran out")
	}
	if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
		t.Fatalf("the first pull of a new ask got the earlier session's decks: %v", got)
	}
	// The ended session is not only noticed by the held request: the next pull ends it too.
	s2, srv2 := sharePlayer(t, true, "")
	sharePull(t, srv2, "?line=1")
	sharePost(t, s2)
	s2.shadow.mu.Lock()
	for ip := range s2.shadow.askers {
		s2.shadow.askers[ip] = time.Now().Add(-shadowPeerSeenTimeout - time.Second)
	}
	s2.shadow.mu.Unlock()
	if got := sharePull(t, srv2, "?line=1"); len(got) != 0 {
		t.Fatalf("the first pull after silence got the earlier session's decks: %v", got)
	}
}

// A post or a ping that crossed the end of a session (the QML learns of it up to about a second later) is dropped:
// nothing of the ended session is kept, served or kept alive, and the display work on it does not start again.
func TestASharePostOrPingAfterTheLastAskIsDropped(t *testing.T) {
	for _, path := range []string{"/v1/enginex/decks", "/v1/enginex/decks/alive"} {
		s, srv := sharePlayer(t, true, "")
		sharePull(t, srv, "?line=1")
		sharePost(t, s)
		path += shareTag(s) // the session that is about to end
		s.shadow.mu.Lock()
		for ip := range s.shadow.askers { // the last ask ran out; nothing has noticed yet (no held request, no pull)
			s.shadow.askers[ip] = time.Now().Add(-shadowPeerSeenTimeout - time.Second)
		}
		s.shadow.mu.Unlock()
		h := s.decksPost
		if strings.Contains(path, "/alive") {
			h = s.decksAlive
		}
		body := fmt.Sprintf(`[{"deck":1,"title":"Crossed","playing":true,"at":%d}]`, time.Now().UnixMilli())
		h(httptest.NewRecorder(), httptest.NewRequest("POST", path, strings.NewReader(body)))
		s.shadow.mu.Lock()
		kept := len(s.shadow.own)
		s.shadow.mu.Unlock()
		if kept != 0 {
			t.Fatalf("%s after the last ask ran out: %d decks kept", path, kept)
		}
		if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
			t.Fatalf("%s: the first pull of a new ask got %v", path, got)
		}
	}
}

// The review's case (#329 review finding 5): a closing server frees the ask port whichever mode bound it.
func TestClosingASharingServerFreesTheAskPort(t *testing.T) {
	probe := listenUDP(t)
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	clSetBoard(t, "jp07")
	oldPort, oldBeacon := shadowAskPort, shadowBeaconOn
	shadowAskPort, shadowBeaconOn = port, true
	defer func() { shadowAskPort, shadowBeaconOn = oldPort, oldBeacon }()
	s, _ := sharePlayer(t, true, "")
	s.shadow.mu.Lock()
	bound := s.shadow.askConn != nil
	s.shadow.mu.Unlock()
	if !bound {
		t.Fatal("no listener to free")
	}
	s.Close()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatalf("a closed Share Decks server still holds the ask port: %v", err)
	}
	c.Close()
	if s.shadow.shares.Load() || s.shadow.hears.Load() {
		t.Fatal("a closed server still shares or hears")
	}
}

// A share post or ping that comes after the row went off must not become a peer's post: stream-sync reads the row
// once, under the lock its switch takes, and drops a tagged post or ping when it is off. Untagged posts (a 4-deck
// player's) are taken as before.
func TestAShareDecksPostAfterTheRowWentOffIsDropped(t *testing.T) {
	s, srv := sharePlayer(t, true, "")
	sharePull(t, srv, "?line=1") // a console asks: the QML samples
	tag := shareTag(s)
	post := func(path string) int {
		at := time.Now().UnixMilli()
		body := fmt.Sprintf(`[{"deck":1,"title":"P1","playing":true,"positionMs":0,"player":2,"at":%d}]`, at)
		w := httptest.NewRecorder()
		h := s.decksPost
		if strings.Contains(path, "/alive") {
			h = s.decksAlive
		}
		h(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return w.Code
	}
	owned := func() int { s.shadow.mu.Lock(); defer s.shadow.mu.Unlock(); return len(s.shadow.own) }
	post("/v1/enginex/decks" + tag)
	if code := post("/v1/enginex/decks/alive" + tag); code != http.StatusNoContent || owned() != 1 {
		t.Fatalf("a share post and ping with the row on: ping %d, %d decks kept", code, owned())
	}
	if code, _ := uiPost(t, srv.URL, "shareDecks=0"); code != 200 {
		t.Fatalf("row off: %d", code)
	}
	// The late post and ping of the QML, still tagged: dropped, so nothing is kept, nothing reaches a pulling peer and
	// no loop starts.
	post("/v1/enginex/decks" + tag)
	post("/v1/enginex/decks/alive" + tag)
	if n := owned(); n != 0 {
		t.Fatalf("a late share post was kept: %d decks", n)
	}
	if got := sharePull(t, srv, ""); len(got) != 0 {
		t.Fatalf("a late share post became a peer's decks: %v", got)
	}
	s.shadow.mu.Lock()
	running := s.shadow.running
	s.shadow.mu.Unlock()
	if running {
		t.Fatal("a late share post started the deck loop")
	}
	// An untagged post (what a player in the 4-deck layout sends) is taken as before.
	if code := post("/v1/enginex/decks"); code != 200 {
		t.Fatalf("an untagged post: %d", code)
	}
	if got := sharePull(t, srv, ""); len(got) != 1 {
		t.Fatalf("an untagged post was not taken: %v", got)
	}
}

// The review's round-2 case: a share post sampled for one session that arrives after that session ended, while the
// next one starts or the row went off and on, is dropped: the next session never shows the earlier track, whatever
// the QML believes (it may hear "asked" both times).
func TestAPostOfAnEndedSessionNeverReachesTheNextOne(t *testing.T) {
	for _, scenario := range []string{"the last ask ran out", "a new ask began", "the row went off and on"} {
		t.Run(scenario, func(t *testing.T) {
			s, srv := sharePlayer(t, true, "")
			sharePull(t, srv, "?line=1")
			sharePost(t, s)
			if got := sharePull(t, srv, "?line=1"); len(got) == 0 {
				t.Fatal("no session to start from")
			}
			tag := shareTag(s) // the session the QML sampled the late post for
			body := &heldBody{
				Reader:  strings.NewReader(fmt.Sprintf(`[{"deck":1,"title":"Old session sample","active":true,"at":%d}]`, time.Now().UnixMilli())),
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.decksPost(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/enginex/decks"+tag, body))
			}()
			select {
			case <-body.started:
			case <-time.After(3 * time.Second):
				close(body.release)
				t.Fatal("the old post did not start")
			}
			if scenario == "the row went off and on" {
				clUI(t, srv.URL, "shareDecks=0")
				clUI(t, srv.URL, "shareDecks=1")
			} else {
				s.shadow.mu.Lock()
				for ip := range s.shadow.askers {
					s.shadow.askers[ip] = time.Now().Add(-shadowPeerSeenTimeout - time.Second)
				}
				s.shadow.mu.Unlock()
				if shareAsked(t, srv) {
					t.Fatal("the QML was not told the last ask ran out")
				}
			}
			if scenario == "a new ask began" {
				if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
					t.Fatalf("the new session's first pull: %v", got)
				}
			}
			close(body.release)
			<-done
			if got := sharePull(t, srv, "?line=1"); len(got) != 0 {
				t.Fatalf("the next session got the ended session's post: %v", got)
			}
			// What the new session's QML posts, with its own session, is taken.
			sharePost(t, s)
			if got := strings.Join(sharePull(t, srv, "?line=1"), ","); got != "P1,P2" {
				t.Fatalf("the new session's own post: %q", got)
			}
		})
	}
}

// The review's round-2 case: the last ask runs out and nobody looks (Engine or its QML stopped, no console asks
// again): the session ends anyway, for the source-waveform work on its tracks and for their display data, each on
// its own (no held request, pull or post in between).
func TestTheWorkOfAnEndedSessionStopsUnobserved(t *testing.T) {
	for _, observer := range []string{"the source-waveform work", "the display data"} {
		t.Run(observer, func(t *testing.T) {
			s, srv := sharePlayer(t, true, "")
			np := shareTrack(t, s)
			sharePull(t, srv, "?line=1")
			body := `[{"deck":1,"net":"` + np + `","playing":true,"positionMs":1000,"player":2}]`
			s.decksPost(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/enginex/decks"+shareTag(s), strings.NewReader(body)))
			decks := csPull(t, srv, "?line=1")
			if len(decks) != 1 || decks[0].Key == "" {
				t.Fatalf("no shared track: %+v", decks)
			}
			key := decks[0].Key
			// detail=0: the display data as it is, without queueing the source-waveform work (which would look too).
			display := func() int {
				resp, err := http.Get(srv.URL + "/peer/decks/1/display?detail=0&key=" + url.QueryEscape(key))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp.StatusCode
			}
			if observer == "the source-waveform work" && !s.shadowSourceLoaded(key) || observer == "the display data" && display() != http.StatusOK {
				t.Fatalf("%s is not there during the session", observer)
			}
			s.shadow.mu.Lock()
			for ip := range s.shadow.askers {
				s.shadow.askers[ip] = time.Now().Add(-shadowPeerSeenTimeout - time.Second)
			}
			s.shadow.ownAt = time.Now() // the decks themselves are fresh: only the ended session makes them go
			s.shadow.mu.Unlock()
			if observer == "the source-waveform work" && s.shadowSourceLoaded(key) {
				t.Fatal("the source-waveform work goes on after the last ask ran out, unobserved")
			}
			if observer == "the display data" && display() == http.StatusOK {
				t.Fatal("the display data of an ended session is still served")
			}
		})
	}
}

// The held request tells the QML a new session even when it is asked both times: the last ask ran out and another
// began between two of its requests.
func TestTheHeldRequestTellsANewSession(t *testing.T) {
	old := consoleAskHold
	consoleAskHold = 2 * time.Second
	defer func() { consoleAskHold = old }()
	s, srv := sharePlayer(t, true, "")
	held := func(query string) (bool, string, time.Duration) {
		start := time.Now()
		resp, err := http.Get(srv.URL + "/v1/enginex/consolesend?" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r struct {
			Asked   bool
			Session uint64
		}
		json.NewDecoder(resp.Body).Decode(&r)
		return r.Asked, strconv.FormatUint(r.Session, 10), time.Since(start)
	}
	sharePull(t, srv, "?line=1")
	asked, first, _ := held("asked=0&session=")
	if !asked {
		t.Fatal("not asked")
	}
	// Nothing changed: held.
	if _, _, took := held("asked=1&session=" + first); took < time.Second {
		t.Fatalf("an unchanged session was answered after %v", took)
	}
	// The last ask runs out and another begins before the QML asks again.
	s.shadow.mu.Lock()
	for ip := range s.shadow.askers {
		s.shadow.askers[ip] = time.Now().Add(-shadowPeerSeenTimeout - time.Second)
	}
	s.shadow.mu.Unlock()
	sharePull(t, srv, "?line=1")
	asked, second, took := held("asked=1&session=" + first)
	if !asked || second == first || took > time.Second {
		t.Fatalf("a new session: asked=%t session %s (was %s) after %v", asked, second, first, took)
	}
	// A console's QML says no session: it is told only "asked", as before.
	if _, _, took := held("asked=1"); took < time.Second {
		t.Fatalf("a request without a session was answered on the session after %v", took)
	}
}

// listenUDP is a socket on the loopback; the test is skipped where the host gives none.
func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readUDP(c *net.UDPConn, wait time.Duration) string {
	c.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 64<<10)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		return ""
	}
	return string(b[:n])
}

func TestShareDecksListensForTheConsoleAndSendsOnlyToIt(t *testing.T) {
	console := listenUDP(t) // the console's beacon port: its answers and the decks arrive here
	probe := listenUDP(t)
	askPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	oldPort, oldBeacon := shadowAskPort, shadowBeaconOn
	clSetBoard(t, "jp07") // first: it switches the beacon off for the other tests, and puts it back
	shadowAskPort, shadowBeaconOn = askPort, true
	defer func() { shadowAskPort, shadowBeaconOn = oldPort, oldBeacon }()
	s, srv := sharePlayer(t, true, "")
	s.shadow.mu.Lock()
	s.shadow.pushPort = console.LocalAddr().(*net.UDPAddr).Port
	listening := s.shadow.askConn != nil
	s.shadow.mu.Unlock()
	if !listening {
		t.Fatal("a player with Share Decks does not listen for a Line In console's ask")
	}
	// Before any ask the post goes nowhere.
	sharePost(t, s)
	if got := readUDP(console, 300*time.Millisecond); got != "" {
		t.Fatalf("sent without an ask: %q", got)
	}
	cl, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: askPort})
	if err != nil {
		t.Skip(err)
	}
	defer cl.Close()
	// A player's ask (Show Console) and noise are not this player's business.
	cl.Write([]byte("EngineX-ask-console 1"))
	cl.Write([]byte("something else"))
	if got := readUDP(console, 300*time.Millisecond); got != "" {
		t.Fatalf("answered what is not a Line In console's ask: %q", got)
	}
	// The console's ask: the player answers with its beacon, so the console finds it and pulls.
	cl.Write([]byte(shareAskDatagram))
	if got := readUDP(console, 2*time.Second); got != "EngineX-shadow 1" {
		t.Fatalf("the console finds the player by its beacon: %q", got)
	}
	if !shareAsked(t, srv) {
		t.Fatal("the ask was not noted")
	}
	// Now a post goes to the console, unicast, at once.
	sharePost(t, s)
	got := readUDP(console, 2*time.Second)
	if !strings.HasPrefix(got, "EngineX-decks 1\n") || !strings.Contains(got, `"title":"P2"`) {
		t.Fatalf("the post did not reach the console that asked: %q", got)
	}
	// And nothing else ran: no deck loop (no beacon of its own, no pulls, no push to other peers).
	s.shadow.mu.Lock()
	running := s.shadow.running
	s.shadow.mu.Unlock()
	if running {
		t.Fatal("a Share Decks post started the deck loop")
	}
	// The row off: the listener closes, the asks are forgotten, a post goes nowhere.
	s.applyShadowFlags(uiSettings{})
	s.shadow.mu.Lock()
	closed := s.shadow.askConn == nil && len(s.shadow.askers) == 0
	s.shadow.mu.Unlock()
	if !closed {
		t.Fatal("the listener outlived the row")
	}
	sharePost(t, s) // a late share post of the QML: dropped
	if got := readUDP(console, 300*time.Millisecond); got != "" {
		t.Fatalf("sent after the row was switched off: %q", got)
	}
	s.shadow.mu.Lock()
	running = s.shadow.running
	s.shadow.mu.Unlock()
	if got := sharePull(t, srv, ""); running || len(got) != 0 {
		t.Fatalf("after the row went off: deck loop running=%t, a peer's pull got %v", running, got)
	}
}

// Two consoles with Line In on: each asks for players beside its beacon, and neither may take the other's ask for a
// player's (it would sample and send its decks for nobody). A player's ask (Show Console) still wakes it, as before.
func TestALineInConsoleIgnoresAnotherConsolesAskForPlayers(t *testing.T) {
	other := listenUDP(t) // where an answer would go
	probe := listenUDP(t)
	askPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	oldPort, oldBeacon := shadowAskPort, shadowBeaconOn
	clSetBoard(t, "jc11")
	shadowAskPort, shadowBeaconOn = askPort, true
	defer func() { shadowAskPort, shadowBeaconOn = oldPort, oldBeacon }()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("console_line_view=1\n"), 0o600)
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.shadow.mu.Lock()
	s.shadow.pushPort = other.LocalAddr().(*net.UDPAddr).Port
	listening := s.shadow.askConn != nil
	s.shadow.mu.Unlock()
	if !listening {
		t.Fatal("a console with Line In on does not listen")
	}
	cl, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: askPort})
	if err != nil {
		t.Skip(err)
	}
	defer cl.Close()
	cl.Write([]byte(shareAskDatagram))
	if got := readUDP(other, 300*time.Millisecond); got != "" {
		t.Fatalf("a console answered another console's ask for players: %q", got)
	}
	if shareAsked(t, srv) {
		t.Fatal("another console's ask for players made this console send")
	}
	cl.Write([]byte("EngineX-ask-console 1"))
	if got := readUDP(other, 2*time.Second); got != "EngineX-shadow 1" {
		t.Fatalf("a player's ask is no longer answered: %q", got)
	}
}

// peerRecorder is a unit on the console's network that records what the console's pulls ask for.
func peerRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		w.Write([]byte(`{"decks":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), queries...) }
}

// A console asks for Share Decks players only while a channel is on Line: its QML polls for one at least about
// once a second, and stops when the last one leaves Line. The deck loop lingers 10 s for discovery; the asks do not.
func TestALineInConsolePullsWithLineAndOthersDoNot(t *testing.T) {
	for _, tc := range []struct {
		name, board, conf string
		linePoll          time.Duration // how long ago the QML last polled for a channel on Line (0: never)
		want              string
	}{
		{"console, a channel on Line", "jc11", "console_line_view=1\n", time.Millisecond, "line=1"},
		{"console, the last channel left Line 4 s ago", "jc11", "console_line_view=1\n", 4 * time.Second, ""},
		{"console, Line In on, never on Line", "jc11", "console_line_view=1\n", 0, ""},
		{"console, Line In off", "jc11", "", time.Millisecond, ""},
		{"player, 4-deck", "jp13", "four_deck_waveform=1\n", time.Millisecond, ""},
		{"player, Show Console", "jp13", "four_deck_waveform=1\nconsole_decks=1\n", time.Millisecond, "console=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clSetBoard(t, tc.board)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(tc.conf), 0o600)
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			defer s.Close()
			peer, queries := peerRecorder(t)
			u, _ := url.Parse(peer.URL)
			port, _ := strconv.Atoi(u.Port())
			s.shadow.mu.Lock()
			s.shadow.peerPort = port
			s.shadow.peers["127.0.0.1"] = time.Now()
			if tc.linePoll > 0 {
				s.shadow.polled = time.Now().Add(-tc.linePoll)
			}
			s.shadow.mu.Unlock()
			s.shadowPollPeers()
			got := queries()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("pulled with %q, want %q", got, tc.want)
			}
		})
	}
}

// The whole chain over HTTP: a console with Line In on pulls a Share Decks player, and the channel on Line shows
// the player's deck on its active layer, display only, with its model and number.
func TestALineInConsoleShowsAShareDecksPlayer(t *testing.T) {
	p, psrv := sharePlayer(t, true, "")
	c, _ := csConsole(t, true)
	u, _ := url.Parse(psrv.URL)
	port, _ := strconv.Atoi(u.Port())
	c.shadow.mu.Lock()
	c.shadow.peerPort = port
	c.shadow.peers["127.0.0.1"] = time.Now()
	c.shadow.polled = time.Now() // its QML polls for channel 3 on Line
	c.shadow.mu.Unlock()
	c.shadowPollPeers() // the first pull: an ask, answered with nothing yet
	if !shareAsked(t, psrv) {
		t.Fatal("the console's pull did not ask the player")
	}
	sharePost(t, p) // the player's QML samples now
	c.shadowPollPeers()
	lanes := c.consoleLineLanes(time.Now(), []int{3})
	if len(lanes) != 1 {
		t.Fatalf("lanes: %d", len(lanes))
	}
	d := lanes[0].d
	if d.Deck != 3 || d.Title != "P2" || !d.LineIn || d.Model != "SC5000" || d.Player != 2 {
		t.Fatalf("channel 3 shows deck=%d title=%q lineIn=%t model=%q player=%d", d.Deck, d.Title, d.LineIn, d.Model, d.Player)
	}
}
