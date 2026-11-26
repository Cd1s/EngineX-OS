package webui

// Share Decks (#329): a player in the Stock layout with the row on is shown by a console's Line In view without the
// 4-deck layout. It listens for a Line In console's ask, answers with a beacon, gives its decks to a pull with line=1
// and to nobody else, and sends its posts to the consoles that asked: no deck loop, no beacon of its own, no push
// to anybody else. The console asks while a channel is on Line / AUX and says line=1 on its pulls.
//
// These tests use only what the code had before (the settings file, the HTTP routes, the UDP ports), so on the old
// code they compile and fail on what they check.

import (
	"encoding/json"
	"fmt"
	"github.com/Cd1s/EngineX-OS/internal/migration"
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

// sharePost is what the player's QML posts: decks 1 and 2, deck 2 on the active layer, player number 2.
func sharePost(t *testing.T, s *Server) {
	t.Helper()
	at := time.Now().UnixMilli()
	body := fmt.Sprintf(`[{"deck":1,"title":"P1","link":"{p1ay3r}","playing":false,"positionMs":0,"player":2,"at":%d},`+
		`{"deck":2,"title":"P2","link":"{p1ay3r}","playing":true,"positionMs":5000,"active":true,"player":2,"at":%d}]`, at, at)
	w := httptest.NewRecorder()
	s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(body)))
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
	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body["asked"]
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
			sharePost(t, s)
			if got := strings.Join(sharePull(t, srv, ""), ","); got != "P1,P2" {
				t.Fatalf("the 4-deck layout with the row on: %q", got)
			}
			if v := clUI(t, srv.URL, "fourDeckWaveform=0"); v["shareDecks"] != true {
				t.Fatalf("the layout cleared it: %v", v["shareDecks"])
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
	sharePost(t, s)
	if shareAsked(t, srv) {
		t.Fatal("asked before anybody asked")
	}
	// A player that pulls (4-deck layout, or Show Console) and an unknown puller get nothing.
	for _, q := range []string{"", "?console=1"} {
		if got := sharePull(t, srv, q); len(got) != 0 {
			t.Fatalf("pull %q got the decks of a Stock player: %v", q, got)
		}
	}
	// A Line In console's pull is an ask and gets the decks.
	if got := strings.Join(sharePull(t, srv, "?line=1"), ","); got != "P1,P2" {
		t.Fatalf("the Line In console's pull: %q", got)
	}
	if !shareAsked(t, srv) {
		t.Fatal("the console's pull was not an ask: the QML would never sample")
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
	// The row switched off: what it shared is forgotten, so a console that still pulls gets nothing.
	sharePost(t, s)
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
	sharePost(t, four)
	if got := strings.Join(sharePull(t, fourSrv, ""), ","); got != "P1,P2" {
		t.Fatalf("the 4-deck layout changed: %q", got)
	}
}

// A console's Line In view fetches only the display data of the track; a Share Decks player gives nothing more: no
// audio file and no performance data, which only a shadow copy (the 4-deck layout) is made from.
func TestShareDecksGivesDisplayDataOnlyNeverTheAudio(t *testing.T) {
	data := t.TempDir()
	oldRoot := shadowDataRoot
	shadowDataRoot = data
	defer func() { shadowDataRoot = oldRoot }()
	os.MkdirAll(filepath.Join(data, migration.DataName+"/Stream"), 0o755)
	os.WriteFile(filepath.Join(data, migration.DataName+"/Stream/a.mp3"), []byte("AUDIO"), 0o644)
	for _, share := range []bool{true, false} {
		extra := ""
		if !share {
			extra = "four_deck_waveform=1\n" // the 4-deck layout: a peer copies the track, as before
		}
		s, srv := sharePlayer(t, true, extra)
		body := `[{"deck":1,"net":"net://u/(Unknown)/` + migration.DataName + `/Stream/a.mp3","playing":true,"positionMs":1000,"player":2}]`
		w := httptest.NewRecorder()
		s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader(body)))
		q := ""
		if share {
			q = "?line=1"
		}
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
			if refused := resp.StatusCode == http.StatusForbidden; refused != share {
				t.Fatalf("share=%t: %s answered %d", share, path, resp.StatusCode)
			}
		}
		resp, err := http.Get(srv.URL + "/peer/decks/1/display?key=" + key)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Fatalf("share=%t: the display data was refused", share)
		}
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
	sharePost(t, s)
	if got := readUDP(console, 300*time.Millisecond); got != "" && !strings.HasPrefix(got, "EngineX-shadow") {
		t.Fatalf("sent after the row was switched off: %q", got)
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

func TestALineInConsolePullsWithLineAndOthersDoNot(t *testing.T) {
	for _, tc := range []struct {
		name, board, conf, want string
	}{
		{"console, Line In on", "jc11", "console_line_view=1\n", "line=1"},
		{"console, Line In off", "jc11", "", ""},
		{"player, 4-deck", "jp13", "four_deck_waveform=1\n", ""},
		{"player, Show Console", "jp13", "four_deck_waveform=1\nconsole_decks=1\n", "console=1"},
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
	sharePost(t, p)
	c, _ := csConsole(t, true)
	u, _ := url.Parse(psrv.URL)
	port, _ := strconv.Atoi(u.Port())
	c.shadow.mu.Lock()
	c.shadow.peerPort = port
	c.shadow.peers["127.0.0.1"] = time.Now()
	c.shadow.mu.Unlock()
	c.shadowPollPeers()
	lanes := c.consoleLineLanes(time.Now(), []int{3})
	if len(lanes) != 1 {
		t.Fatalf("lanes: %d", len(lanes))
	}
	d := lanes[0].d
	if d.Deck != 3 || d.Title != "P2" || !d.LineIn || d.Model != "SC5000" || d.Player != 2 {
		t.Fatalf("channel 3 shows deck=%d title=%q lineIn=%t model=%q player=%d", d.Deck, d.Title, d.LineIn, d.Model, d.Player)
	}
	if !shareAsked(t, psrv) {
		t.Fatal("the console's pull did not ask the player")
	}
}
