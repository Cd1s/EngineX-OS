package webui

// Console linking, stage 2: a player with the row Show Console can show the decks of a linked console in the lanes
// it has free; the console sends only to a player that asks. The tables of the lanes two players show each other
// stay as they are with the row off (shadow_lane_matrix_test.go, which also passes before stage 2).

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// csConsole is a PRIME 4 with the Line In view on and a file to name on its decks, served like the deck's
// own stream-sync.
func csConsole(t *testing.T, lineIn bool) (*Server, *httptest.Server) {
	t.Helper()
	clSetBoard(t, "jc11")
	dir := t.TempDir()
	if lineIn {
		os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("console_line_view=1\n"), 0o600)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "enginex", "Stream"), 0o755)
	os.WriteFile(filepath.Join(root, "enginex", "Stream", "a.mp3"), make([]byte, 4096), 0o600)
	s.shadow.dataRoot = root
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

// csPost is what the console's QML posts: these decks, each with a track (none: the empty post).
func csPost(t *testing.T, c *Server, decks ...int) {
	t.Helper()
	var parts []string
	for _, d := range decks {
		parts = append(parts, fmt.Sprintf(`{"deck":%d,"title":"C%d","net":"net://u/(Unknown)/enginex/Stream/a.mp3","link":"{c0ns01e}","playing":true,"positionMs":1000,"at":%d}`, d, d, time.Now().UnixMilli()))
	}
	w := httptest.NewRecorder()
	c.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader("["+strings.Join(parts, ",")+"]")))
	if w.Code != 200 {
		t.Fatalf("post: %d", w.Code)
	}
}

// csPull is what a unit gets from the console's /peer/decks with this query.
func csPull(t *testing.T, srv *httptest.Server, query string) []deckState {
	t.Helper()
	resp, err := http.Get(srv.URL + "/peer/decks" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Decks []deckState }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Decks
}

// csPlayer is a standalone player (JP13) with the 4-deck layout and, with row, the row Show Console on. It
// pulls from the console's server.
func csPlayer(t *testing.T, row bool, console *httptest.Server) *Server {
	t.Helper()
	clSetBoard(t, "jp13")
	dir := t.TempDir()
	conf := "four_deck_waveform=1\n"
	if row {
		conf += "console_decks=1\n"
	}
	os.WriteFile(filepath.Join(dir, "settings.conf"), []byte(conf), 0o600)
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	if console != nil {
		u, _ := url.Parse(console.URL)
		port, _ := strconv.Atoi(u.Port())
		s.shadow.peerPort = port
		s.shadow.peers["127.0.0.1"] = time.Now()
	}
	return s
}

// csLanes is what the player's lanes show, in the notation of the lane table: B4 a peer's deck, C2 a console's.
func csLanes(t *testing.T, s *Server, role string) string {
	t.Helper()
	clSetBoard(t, "jp13")
	byLane := map[int]deckState{}
	for _, d := range s.shadowComposeMode("auto", role, time.Now()) {
		byLane[d.Deck] = d
	}
	var out []string
	for lane := 1; lane <= 4; lane++ {
		d, ok := byLane[lane]
		switch {
		case strings.Contains(role, strconv.Itoa(lane)):
			out = append(out, fmt.Sprintf("%d=own", lane))
		case !ok:
			out = append(out, fmt.Sprintf("%d=-", lane))
		case d.Console > 0:
			out = append(out, fmt.Sprintf("%d=C%d", lane, d.Console))
		default:
			out = append(out, fmt.Sprintf("%d=%s", lane, d.Title))
		}
	}
	return strings.Join(out, " ")
}

func TestConsoleSendGivesItsDecksOnlyToAPlayerThatAsks(t *testing.T) {
	c, srv := csConsole(t, true)
	csPost(t, c, 1, 2)
	if got := csPull(t, srv, ""); len(got) != 0 {
		t.Fatalf("a pull without console=1 (a player of 5.3.0.5, or with the row off) must get nothing, got %+v", got)
	}
	if len(c.shadow.askers) != 0 {
		t.Fatalf("a plain pull is no ask: %v", c.shadow.askers)
	}
	got := csPull(t, srv, "?console=1")
	if len(got) != 2 || got[0].Deck != 1 || got[1].Deck != 2 || got[0].Model != "PRIME4" || got[0].Title != "C1" || got[0].Path != "" {
		t.Fatalf("the asker gets both decks, stamped with the console's model and no file: %+v", got)
	}
	if got := csPull(t, srv, ""); len(got) != 0 {
		t.Fatalf("a plain pull still gets nothing while somebody else asks: %+v", got)
	}
	// The Line In view off (Stock): nobody is listened to, nothing is sent, nothing is noted.
	c2, srv2 := csConsole(t, false)
	csPost(t, c2, 1)
	if got := csPull(t, srv2, "?console=1"); len(got) != 0 || len(c2.shadow.askers) != 0 || c2.shadow.hears.Load() || c2.shadow.askConn != nil {
		t.Fatalf("a console on Stock answers an ask with nothing and notes nothing: %+v %v", got, c2.shadow.askers)
	}
	// A player that is not a console never answers an ask either.
	clSetBoard(t, "jp13")
	pl := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer pl.Close()
	w := httptest.NewRecorder()
	pl.peerDecksGet(w, httptest.NewRequest("GET", "/peer/decks?console=1", nil))
	if pl.shadow.hears.Load() || len(pl.shadow.askers) != 0 {
		t.Fatal("a player listens for no ask")
	}
}

func TestConsoleSendStartsWhenAskedAndStopsWhenTheLastAskerIsSilent(t *testing.T) {
	old := consoleAskHold
	consoleAskHold = 3 * time.Second
	defer func() { consoleAskHold = old }()
	c, srv := csConsole(t, true)
	held := func(believes string) (bool, time.Duration) {
		start := time.Now()
		resp, err := http.Get(srv.URL + "/v1/enginex/consolesend?asked=" + believes)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r struct{ Asked bool }
		json.NewDecoder(resp.Body).Decode(&r)
		return r.Asked, time.Since(start)
	}
	// Nobody asks: a QML that believes so is told at once, one that believes nobody does is held until the hold ends.
	if asked, took := held("1"); asked || took > time.Second {
		t.Fatalf("asked=%v after %v", asked, took)
	}
	if asked, took := held("0"); asked || took < 2*time.Second {
		t.Fatalf("a request that changes nothing is held: asked=%v after %v", asked, took)
	}
	// The first ask ends the hold at once.
	done := make(chan bool, 1)
	go func() { a, _ := held("0"); done <- a }()
	time.Sleep(200 * time.Millisecond)
	csPull(t, srv, "?console=1")
	select {
	case a := <-done:
		if !a {
			t.Fatal("told nobody asks after an ask")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the held request did not end with the first ask")
	}
	// Asked: a request that believes so is held; when the last ask runs out, it is told.
	c.shadow.mu.Lock()
	c.shadow.askers["127.0.0.1"] = time.Now().Add(-shadowPeerSeenTimeout + 1500*time.Millisecond) // runs out in 1.5 s
	c.shadow.mu.Unlock()
	if asked, took := held("1"); asked || took > 3*time.Second || took < time.Second {
		t.Fatalf("the last ask ran out: asked=%v after %v", asked, took)
	}
	// And nothing is sent any more: a pull with console=1 is an ask again, a plain one never was.
	csPost(t, c, 1)
	if got := csPull(t, srv, ""); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestConsoleAskDatagramWakesTheConsoleAndItAnswersWithABeacon(t *testing.T) {
	// The beacon the console answers with goes to the asker's beacon port: a socket of this test.
	bc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip(err)
	}
	defer bc.Close()
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip(err)
	}
	askPort := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	oldPort, oldBeacon := shadowAskPort, shadowBeaconOn
	clSetBoard(t, "jc11") // first: it switches the beacon off for the other tests, and puts it back
	shadowAskPort, shadowBeaconOn = askPort, true
	defer func() { shadowAskPort, shadowBeaconOn = oldPort, oldBeacon }()
	c, _ := csConsole(t, true) // its listener is up: Line In is on
	c.shadow.mu.Lock()         // the listener reads it
	c.shadow.pushPort = bc.LocalAddr().(*net.UDPAddr).Port
	c.shadow.mu.Unlock()
	if c.shadow.askConn == nil {
		t.Skip("no listener on this host")
	}
	cl, _ := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: askPort})
	defer cl.Close()
	cl.Write([]byte("something else"))
	cl.Write([]byte(shadowAskMagic))
	bc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := bc.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "EngineX-shadow 1" {
		t.Fatalf("the asker finds the console by its beacon: %q %v", buf[:n], err)
	}
	c.shadow.mu.Lock()
	asked := c.shadow.askedNow(time.Now())
	c.shadow.mu.Unlock()
	if !asked {
		t.Fatal("the ask was not noted")
	}
	// Line In off: the listener closes and the asks are forgotten.
	c.consoleAskSync(false)
	if c.shadow.askConn != nil || len(c.shadow.askers) != 0 {
		t.Fatal("the listener outlived the setting")
	}
	// An older unit takes the datagram for nothing: it is no beacon and no decks.
	clSetBoard(t, "jp13")
	old := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer old.Close()
	old.shadow.receive([]byte(shadowAskMagic), "192.0.2.50", time.Now())
	if len(old.shadow.peers) != 0 {
		t.Fatalf("a unit that does not know the ask made a peer of its sender: %v", old.shadow.peers)
	}
}

// What is in the lanes after everyone else: the table of decision 2.
func TestConsoleDecksFillOnlyTheLanesStillFree(t *testing.T) {
	c, srv := csConsole(t, true)
	pull := func(p *Server) { p.shadowPollPeers() }

	// Alone with a console: its decks on the free lanes, the lane of their own number first.
	a := csPlayer(t, true, srv)
	csPost(t, c, 1, 2)
	pull(a)
	if got := csLanes(t, a, "34"); got != "1=C1 2=C2 3=own 4=own" {
		t.Fatalf("lanes 1 and 2 are free: %s", got)
	}
	// Deck 2 only: it keeps lane 2. Both again; deck 1 ejects (its channel went to Line), then comes back.
	csPost(t, c, 2)
	pull(a)
	if got := csLanes(t, a, "34"); got != "1=- 2=C2 3=own 4=own" {
		t.Fatalf("deck 1 let go: %s", got)
	}
	csPost(t, c, 1, 2)
	pull(a)
	if got := csLanes(t, a, "34"); got != "1=C1 2=C2 3=own 4=own" {
		t.Fatalf("deck 1 back: %s", got)
	}
	// A player's deck takes lane 1 first (as today: its deck 4 is one of our own numbers): console deck 1 has lost its
	// lane and finds none (deck 2 keeps lane 2), so it is left out; nothing else moves.
	b := clNewUnit(t, "jp07", "192.0.2.12")
	b.post(t, clDeck{deck: 4, title: "B4", active: true, player: 2})
	b.send(a)
	if got := csLanes(t, a, "34"); got != "1=B4 2=C2 3=own 4=own" {
		t.Fatalf("a player's deck first, then the console's: %s", got)
	}
	// B's deck ejects: lane 1 is free; console deck 1, left out, comes in on it, deck 2 keeps lane 2.
	b.post(t) // the empty post of a deck that ejected
	b.send(a)
	if got := csLanes(t, a, "34"); got != "1=C1 2=C2 3=own 4=own" {
		t.Fatalf("the freed lane goes to the deck that did not fit, the other keeps its lane: %s", got)
	}
	// B loads again: a player's deck is placed first, as today, and the console deck on that lane steps aside.
	b.post(t, clDeck{deck: 4, title: "B4", active: true, player: 2})
	b.send(a)
	if got := csLanes(t, a, "34"); got != "1=B4 2=C2 3=own 4=own" {
		t.Fatalf("a player's deck is never displaced by the console's, the deck that holds a free lane keeps it: %s", got)
	}
	// Both players keep two layers (A 1+3, B 2+4): the lanes they show each other are the only ones, none is left.
	a2 := csPlayer(t, true, srv)
	csPost(t, c, 1, 2)
	pull(a2)
	b2 := clNewUnit(t, "jp07", "192.0.2.13")
	b2.post(t, clDeck{deck: 2, title: "B2", active: true, player: 2}, clDeck{deck: 4, title: "B4", active: true, player: 2})
	b2.send(a2)
	if got := csLanes(t, a2, "13"); got != "1=own 2=B2 3=own 4=B4" {
		t.Fatalf("no lane is left for the console: %s", got)
	}
	// The row off: the same console is in the air and the player shows nothing of it.
	off := csPlayer(t, false, srv)
	pull(off)
	if got := csLanes(t, off, "34"); got != "1=- 2=- 3=own 4=own" || len(off.shadow.consoleDecks) != 0 {
		t.Fatalf("row off: %s %v", got, off.shadow.consoleDecks)
	}
}

// With the row on and a console in the air, what a player shows of the other player is what the table says, and
// the console takes only lanes the table leaves empty.
func TestConsoleDecksNeverMoveWhatPlayersShowEachOther(t *testing.T) {
	c, srv := csConsole(t, true)
	for _, r := range laneMatrixRows {
		t.Run(r.name, func(t *testing.T) {
			csPost(t, c, 1, 2, 3, 4)
			a := csPlayer(t, true, srv)
			a.shadowPollPeers()
			b := clNewUnit(t, "jp07", "192.0.2.12")
			var db []clDeck
			for _, d := range r.decksB {
				db = append(db, clDeck{deck: d, title: "B" + strconv.Itoa(d), active: true, player: 2})
			}
			if len(db) > 0 {
				b.post(t, db...)
				b.send(a)
			}
			got, want := strings.Fields(csLanes(t, a, r.roleA)), strings.Fields(r.wantA)
			consoleLanes := 0
			for i := range want {
				switch {
				case !strings.HasSuffix(want[i], "=-") && got[i] != want[i]:
					t.Fatalf("lane %d moved: %s, want %s (all: %v)", i+1, got[i], want[i], got)
				case strings.HasSuffix(want[i], "=-") && strings.Contains(got[i], "=C"):
					consoleLanes++
				case strings.HasSuffix(want[i], "=-") && !strings.HasSuffix(got[i], "=-"):
					t.Fatalf("lane %d: %s where the table has nothing (all: %v)", i+1, got[i], got)
				}
			}
			free := strings.Count(r.wantA, "=-")
			if consoleLanes != min(free, 4) {
				t.Fatalf("%d free lanes, %d show a console deck (all: %v)", free, consoleLanes, got)
			}
		})
	}
}

func TestConsoleDecksAreNeverPeerDecksAndNeverPushed(t *testing.T) {
	c, srv := csConsole(t, true)
	csPost(t, c, 1)
	a := csPlayer(t, true, srv)
	a.shadowPollPeers()
	a.shadow.mu.Lock()
	peers, consoles := len(a.shadow.peerDecks), len(a.shadow.consoleDecks)
	a.shadow.mu.Unlock()
	if peers != 0 || consoles != 1 {
		t.Fatalf("console decks are kept apart from the peers' slots: %d peer decks, %d console decks", peers, consoles)
	}
	// A datagram with a console's decks (nothing sends one) is not taken as a peer's either.
	pkt := append([]byte(shadowDecksMagic), mustJSON(t, []deckState{{Deck: 3, Model: "PRIME4", Title: "X", Seq: 1}})...)
	a.shadow.receive(pkt, "192.0.2.77", time.Now())
	if len(a.shadow.peerDecks) != 0 {
		t.Fatalf("%v", a.shadow.peerDecks)
	}
	// The console never pushes: a post reaches nobody by datagram, however many peers it knows.
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip(err)
	}
	defer sink.Close()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip(err)
	}
	defer conn.Close()
	c.shadow.mu.Lock()
	c.shadow.conn, c.shadow.pushPort = conn, sink.LocalAddr().(*net.UDPAddr).Port
	c.shadow.peers["127.0.0.1"] = time.Now()
	c.shadow.mu.Unlock()
	csPost(t, c, 1, 2)
	c.shadowPush()
	sink.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := sink.ReadFromUDP(make([]byte, 64<<10)); err == nil {
		t.Fatalf("a console pushed %d bytes", n)
	}
	// And a console's posts feed neither the Pro DJ Link status nor a MASTER claim: nothing starts, nothing is exported.
	if c.shadow.running {
		t.Fatal("a console's post started the deck loop")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The mixed pairs: a console and a player of which one does not know stage 2. The old side is what 5.3.0.5-r1
// does, the exact requests of its code (the real binary of 147e37fb runs in the sandbox pair).
func TestMixedPairsOfConsoleAndPlayerStayAsTheyWere(t *testing.T) {
	// New console, old player: the old player pulls /peer/decks without a query and never sends an ask.
	c, srv := csConsole(t, true)
	csPost(t, c, 1, 2)
	// Somebody else asks, the console is sending, and the old player still gets what it always got from a console.
	csPull(t, srv, "?console=1")
	if got := csPull(t, srv, ""); len(got) != 0 {
		t.Fatalf("an old player must get an empty deck list, got %+v", got)
	}
	// Old console, new player: the console answers every pull with an empty list and ignores the query and the ask.
	oldConsole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/peer/decks" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"decks":null}`)
	}))
	defer oldConsole.Close()
	a := csPlayer(t, true, oldConsole)
	a.shadowPollPeers()
	if got := csLanes(t, a, "34"); got != "1=- 2=- 3=own 4=own" {
		t.Fatalf("new player beside an old console: %s", got)
	}
	// The same player with the row off sends no ask in any form: its pull has no query.
	var queries []string
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		io.WriteString(w, `{"decks":[]}`)
	}))
	defer spy.Close()
	for _, row := range []bool{false, true} {
		queries = nil
		p := csPlayer(t, row, spy)
		p.shadowPollPeers()
		want := ""
		if row {
			want = "console=1"
		}
		if len(queries) != 1 || queries[0] != want {
			t.Fatalf("row=%v: the pull's query is %q, want %q", row, queries, want)
		}
		if p.shadow.askConsole.Load() != row {
			t.Fatalf("row=%v: flag %v", row, p.shadow.askConsole.Load())
		}
	}
	// A console that sends decks to a player with the row off (it would not, but a bad one) is not shown.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"decks":[{"deck":1,"model":"PRIME4","title":"X","net":"net://u/(Unknown)/enginex/Stream/a.mp3","at":0}]}`)
	}))
	defer bad.Close()
	off := csPlayer(t, false, bad)
	off.shadowPollPeers()
	if got := csLanes(t, off, "34"); got != "1=- 2=- 3=own 4=own" || len(off.shadow.consoleDecks) != 0 || len(off.shadow.peerDecks) != 0 {
		t.Fatalf("row off must show nothing of a console: %s", got)
	}
}

// The lane is a picture: display only, silent, labelled as the console's, never a MASTER, no audio fetched.
func TestConsoleLaneIsDisplayOnlyAndSilent(t *testing.T) {
	c, srv := csConsole(t, true)
	csPost(t, c, 2)
	a := csPlayer(t, true, srv)
	a.shadowPollPeers()
	clSetBoard(t, "jp13")
	var lane *deckState
	for _, mode := range []string{"auto", "engine", "cdj"} {
		out := a.shadowComposeMode(mode, "34", time.Now())
		lane = nil
		for i := range out {
			if out[i].Console > 0 {
				lane = &out[i]
			}
		}
		if lane == nil {
			t.Fatalf("%s mode: no console lane in %+v", mode, out)
		}
		if lane.Deck != 2 || lane.Console != 2 || lane.Model != "PRIME4" || !lane.DisplayOnly || lane.Path != "" || lane.Net != "" || lane.LineIn ||
			lane.Active || lane.Master == nil || *lane.Master || lane.Lead == nil || *lane.Lead || lane.ShadowMaster != 0 {
			t.Fatalf("%s mode: %+v", mode, *lane)
		}
	}
	assertNoShadowAudioFetch(t, a)
	// The display request of that lane finds it (its picture comes from the console, like a peer's).
	a.shadow.mu.Lock()
	n := len(a.shadow.peerDecks)
	a.shadow.mu.Unlock()
	if n != 0 {
		t.Fatalf("a console's deck took a peer slot: %d", n)
	}
	req := httptest.NewRequest("GET", "/v1/enginex/shadow/display?deck=2&key="+url.QueryEscape(lane.Key), nil)
	os.WriteFile(filepath.Join(a.uiDir(), "settings.conf"), []byte("four_deck_waveform=1\nconsole_decks=1\n"), 0o600)
	fourDeckRolePath = filepath.Join(t.TempDir(), "role")
	os.WriteFile(fourDeckRolePath, []byte("34"), 0o600)
	if _, d := a.currentShadowDisplay(req); d.Console != 2 || d.Deck != 2 {
		t.Fatalf("the display request does not find the console's lane: %+v", d)
	}
}

func TestShowConsoleSettingIsPlayerOnlyNeedsTheLayoutAndDefaultsOff(t *testing.T) {
	for _, board := range []string{"jp07", "jp08", "jp13", "jp14"} {
		t.Run("player "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["consoleDecks"] != false {
				t.Fatalf("default must be off: %v", v["consoleDecks"])
			}
			if v := clUI(t, srv.URL, "consoleDecks=1"); v["consoleDecks"] != false {
				t.Fatalf("without the 4-deck layout the row is nothing: %v", v["consoleDecks"])
			}
			clUI(t, srv.URL, "fourDeckWaveform=1")
			if v := clUI(t, srv.URL, "consoleDecks=1"); v["consoleDecks"] != true || !s.shadow.askConsole.Load() {
				t.Fatalf("on: %v %v", v["consoleDecks"], s.shadow.askConsole.Load())
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !strings.Contains(string(b), "console_decks=1\n") {
				t.Fatalf("not stored: %q", b)
			}
			// Turning the layout off turns the row off (as the paired view goes), and the flag follows.
			if v := clUI(t, srv.URL, "fourDeckWaveform=0"); v["consoleDecks"] != false || s.shadow.askConsole.Load() {
				t.Fatalf("layout off: %v %v", v["consoleDecks"], s.shadow.askConsole.Load())
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(b), "console_decks") {
				t.Fatalf("the key stays: %q", b)
			}
			if code, _ := uiPost(t, srv.URL, "consoleDecks=maybe"); code != 400 {
				t.Fatalf("a bad value must be refused, got %d", code)
			}
		})
	}
	// Every other board ignores it, also a stored value: consoles, GO, an unknown board.
	for _, board := range []string{"jc11", "jc11s", "jp21", "jp24", "jc16", "nh08", "jp11", ""} {
		t.Run("not on "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("four_deck_waveform=1\nconsole_decks=1\n"), 0o600)
			s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
			defer s.Close()
			srv := httptest.NewServer(s)
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["consoleDecks"] != false || s.shadow.askConsole.Load() {
				t.Fatalf("a stored value must not be honoured here: %v", v["consoleDecks"])
			}
			if v := clUI(t, srv.URL, "consoleDecks=1"); v["consoleDecks"] != false {
				t.Fatalf("the row must not be turned on here: %v", v["consoleDecks"])
			}
		})
	}
	// Settings copy never carries it: a console's or a player's choice is not another unit's.
	for _, key := range []string{"consoleDecks", "console_decks"} {
		if copyUIAllowed(key) {
			t.Fatalf("%s is on the settings-copy allow-list", key)
		}
	}
	for k := range copyUIExport(uiSettings{FourDeckWaveform: true, ConsoleDecks: true}) {
		if strings.Contains(strings.ToLower(k), "consoledecks") {
			t.Fatalf("settings copy exports %q", k)
		}
	}
	// And the console's own Line In key is as it was.
	if copyUIAllowed("consoleLineView") || !copyUIAllowed("fourDeckWaveform") {
		t.Fatal("the rest of the settings copy was changed")
	}
}

// Switching the row off takes the console's lanes away at once, not when the last picture goes stale; switching it on
// again brings them back at the next pull.
func TestConsoleLanesGoTheMomentTheRowIsSwitchedOff(t *testing.T) {
	c, srv := csConsole(t, true)
	a := csPlayer(t, true, srv)
	csPost(t, c, 1, 2)
	a.shadowPollPeers()
	if got := csLanes(t, a, "34"); got != "1=C1 2=C2 3=own 4=own" {
		t.Fatalf("row on: %s", got)
	}
	a.applyShadowFlags(uiSettings{FourDeckWaveform: true}) // what a save with the row off does
	if got := csLanes(t, a, "34"); got != "1=- 2=- 3=own 4=own" {
		t.Fatalf("row switched off, the pictures are still fresh: %s", got)
	}
	a.applyShadowFlags(uiSettings{FourDeckWaveform: true, ConsoleDecks: true})
	a.shadowPollPeers()
	if got := csLanes(t, a, "34"); got != "1=C1 2=C2 3=own 4=own" {
		t.Fatalf("row on again: %s", got)
	}
}
