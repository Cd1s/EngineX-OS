package webui

// Console linking, stage 1: a linked unit (an EngineX player or a CDJ) is shown on a
// console channel while that channel's input is Line / AUX. Display only: no Engine
// deck, no copy, no hook. The setting is console_line_view, default off, honoured on
// the four consoles only. Nothing here changes what two standalone players show each
// other (see TestPlayerLanesDoNotMoveWithAConsoleOnTheNetwork, a pin).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// clSetBoard makes this process a given board (an lower-case device-tree name such as
// "jc11"); the previous board, the role file and the beacon switch come back after the test.
func clSetBoard(t *testing.T, board string) {
	t.Helper()
	if deviceCompatiblePath == "" || !strings.Contains(deviceCompatiblePath, "cl-board") {
		oldC, oldR, oldB := deviceCompatiblePath, fourDeckRolePath, shadowBeaconOn
		dir := t.TempDir()
		deviceCompatiblePath = filepath.Join(dir, "cl-board")
		fourDeckRolePath = filepath.Join(dir, "missing-role")
		shadowBeaconOn = false
		t.Cleanup(func() { deviceCompatiblePath, fourDeckRolePath, shadowBeaconOn = oldC, oldR, oldB })
	}
	if board == "" {
		os.Remove(deviceCompatiblePath)
		return
	}
	if err := os.WriteFile(deviceCompatiblePath, []byte("inmusic,"+board+"\x00rockchip,rk3288\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clUI POSTs settings and returns the served JSON as a map: the key is looked up by name,
// so a build without the setting fails on the value, not on a missing field.
func clUI(t *testing.T, base, query string) map[string]any {
	t.Helper()
	resp, err := http.Post(base+"/v1/enginex/ui?"+query, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConsoleLineSettingIsConsoleOnlyAndDefaultsOff(t *testing.T) {
	for _, board := range []string{"jc11", "jc11s", "jp21", "jp24"} {
		t.Run("console "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			cfg := Config{UIDir: dir, StateDir: t.TempDir()}
			srv := httptest.NewServer(New(cfg, nil))
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["consoleLineView"] != false {
				t.Fatalf("default must be off and served: %v", v["consoleLineView"])
			}
			v := clUI(t, srv.URL, "consoleLineView=1")
			if v["consoleLineView"] != true {
				t.Fatalf("consoleLineView=1 not served: %v", v["consoleLineView"])
			}
			if v["fourDeckWaveform"] != false || v["pairedView"] != false {
				t.Fatalf("the console switch must not turn the 4-deck layout on: %v %v", v["fourDeckWaveform"], v["pairedView"])
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); !slices.Contains(splitLines(b), "console_line_view=1") {
				t.Fatalf("not stored: %q", b)
			}
			srv2 := httptest.NewServer(New(cfg, nil))
			defer srv2.Close()
			if v := clUI(t, srv2.URL, ""); v["consoleLineView"] != true {
				t.Fatalf("not reloaded: %v", v["consoleLineView"])
			}
			if v := clUI(t, srv2.URL, "consoleLineView=0"); v["consoleLineView"] != false {
				t.Fatalf("not turned off: %v", v["consoleLineView"])
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "settings.conf")); strings.Contains(string(b), "console_line_view") {
				t.Fatalf("the key stays after turning it off: %q", b)
			}
			if code, _ := uiPost(t, srv2.URL, "consoleLineView=maybe"); code != 400 {
				t.Fatalf("a bad value must be refused, got %d", code)
			}
		})
	}
	// Every other board keeps ignoring it, also a value stored by hand or copied: players,
	// the consoles Line In never covered, and an unknown board.
	for _, board := range []string{"jp07", "jp08", "jp13", "jp14", "jc16", "nh08", "jp11", "jp20", "nh10", ""} {
		t.Run("not on "+board, func(t *testing.T) {
			clSetBoard(t, board)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("console_line_view=1\n"), 0o600)
			srv := httptest.NewServer(New(Config{UIDir: dir, StateDir: t.TempDir()}, nil))
			defer srv.Close()
			if v := clUI(t, srv.URL, ""); v["consoleLineView"] != false {
				t.Fatalf("a stored value must not be honoured here: %v", v["consoleLineView"])
			}
			if v := clUI(t, srv.URL, "consoleLineView=1"); v["consoleLineView"] != false {
				t.Fatalf("the setting must not be turned on here: %v", v["consoleLineView"])
			}
		})
	}
}

// Settings copy: the key never travels between units (a console's Line In choice is not a
// player's, and a player's settings must not switch it on).
func TestConsoleLineSettingIsNotCopied(t *testing.T) {
	for _, key := range []string{"consoleLineView", "console_line_view"} {
		if copyUIAllowed(key) {
			t.Fatalf("%s is on the settings-copy allow-list", key)
		}
	}
	all := copyUIExport(uiSettings{FourDeckWaveform: true})
	for k := range all {
		if strings.Contains(strings.ToLower(k), "consoleline") {
			t.Fatalf("settings copy exports %q", k)
		}
	}
	for _, key := range []string{"fourDeckWaveform", "shadowMode"} {
		if !copyUIAllowed(key) {
			t.Fatalf("the rest of the settings copy was changed: %s", key)
		}
	}
}

// clUnit is one linked EngineX unit: its own Server (as its stream-sync) and a fake address.
type clUnit struct {
	s  *Server
	ip string
}

func clNewUnit(t *testing.T, board, ip string) *clUnit {
	t.Helper()
	clSetBoard(t, board)
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	return &clUnit{s: s, ip: ip}
}

type clDeck struct {
	deck   int
	title  string
	active bool
	player int
}

// post is what the unit's QML posts: a track name and position, no file (so no copy, no display fetch).
func (u *clUnit) post(t *testing.T, decks ...clDeck) {
	t.Helper()
	var parts []string
	for _, d := range decks {
		p := fmt.Sprintf(`{"deck":%d,"title":%q,"playing":true,"positionMs":1000,"active":%t,"at":%d`, d.deck, d.title, d.active, time.Now().UnixMilli())
		if d.player != 0 {
			p += fmt.Sprintf(`,"player":%d`, d.player)
		}
		parts = append(parts, p+"}")
	}
	w := httptest.NewRecorder()
	u.s.decksPost(w, httptest.NewRequest("POST", "/v1/enginex/decks", strings.NewReader("["+strings.Join(parts, ",")+"]")))
	if w.Code != 200 {
		t.Fatalf("post: %d", w.Code)
	}
}

// packet is what the unit's stream-sync sends its peers (shadowPush: magic + ownForPeer).
func (u *clUnit) packet() []byte {
	u.s.shadow.mu.Lock()
	b, _ := json.Marshal(u.s.shadow.ownForPeer(time.Now()))
	u.s.shadow.mu.Unlock()
	return append([]byte(shadowDecksMagic), b...)
}

// send delivers the unit's decks to another Server the way its datagram would, beacon first.
func (u *clUnit) send(to *Server) {
	to.shadow.receive([]byte("EngineX-shadow 1"), u.ip, time.Now())
	to.shadow.receive(u.packet(), u.ip, time.Now())
}

// clConsole is a console (PRIME 4 by default) with the Line In view on; it listens on an
// httptest server like the deck's own 127.0.0.1:8080.
func clConsole(t *testing.T, board string, on bool) (*Server, *httptest.Server) {
	t.Helper()
	clSetBoard(t, board)
	dir := t.TempDir()
	if on {
		os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("console_line_view=1\nprolink_share=1\n"), 0o600)
	}
	s := New(Config{UIDir: dir, StateDir: t.TempDir()}, nil)
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

type clLane struct {
	Deck        int
	Title       string
	Player      int
	Model       string
	LineIn      bool
	DisplayOnly bool
	Key         string
	Path        string
	Net         string
}

func clShadow(t *testing.T, srv *httptest.Server, line string) (map[string]any, []clLane) {
	t.Helper()
	u := srv.URL + "/v1/enginex/shadow"
	if line != "" {
		u += "?line=" + line
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var head map[string]any
	var body struct{ Decks []clLane }
	if json.Unmarshal(raw, &head) != nil || json.Unmarshal(raw, &body) != nil {
		t.Fatalf("shadow answer: %s", raw)
	}
	return head, body.Decks
}

func clLaneView(lanes []clLane) string {
	var out []string
	for _, l := range lanes {
		s := fmt.Sprintf("%d=%s", l.Deck, l.Title)
		if l.Player != 0 {
			s += "/P" + strconv.Itoa(l.Player)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// A console with the setting off, or on with no channel on Line / AUX, never starts the
// deck loop (no beacon, no port 50199) and is nobody's peer: the 5.3.0.5-r1 state.
func TestConsoleLineIdleNeverStartsTheLoop(t *testing.T) {
	player := clNewUnit(t, "jp13", "192.0.2.11")
	player.post(t, clDeck{deck: 3, title: "A3", active: true, player: 1})
	for _, c := range []struct {
		name string
		on   bool
		line string
	}{{"setting off, channel 3 on Line", false, "3"}, {"setting on, no channel on Line", true, ""}} {
		t.Run(c.name, func(t *testing.T) {
			s, srv := clConsole(t, "jc11", c.on)
			player.send(s)
			head, lanes := clShadow(t, srv, c.line)
			if len(lanes) != 0 {
				t.Fatalf("lanes without a Line channel or the setting: %+v", lanes)
			}
			s.shadow.mu.Lock()
			running := s.shadow.running
			s.shadow.mu.Unlock()
			if running {
				t.Fatal("the deck loop started (it beacons and binds UDP 50199)")
			}
			if c.on && head["displayOnly"] != true {
				t.Fatalf("answer: %v", head)
			}
		})
	}
}

func TestConsoleLineMapsLinkedUnitsOntoLineChannels(t *testing.T) {
	t.Run("one player on the first Line channel", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		a.post(t, clDeck{deck: 3, title: "A3", active: true, player: 1}, clDeck{deck: 4, title: "A4", player: 1})
		s, srv := clConsole(t, "jc11", true)
		a.send(s)
		head, lanes := clShadow(t, srv, "3")
		if len(lanes) != 1 || lanes[0].Deck != 3 || lanes[0].Title != "A3" || !lanes[0].LineIn || !lanes[0].DisplayOnly || lanes[0].Player != 1 || lanes[0].Model != "SC6000" {
			t.Fatalf("lanes %s: %+v", clLaneView(lanes), lanes)
		}
		if head["role"] == "" || head["displayOnly"] != true {
			t.Fatalf("answer %v", head)
		}
		s.shadow.mu.Lock()
		running := s.shadow.running
		s.shadow.mu.Unlock()
		if !running {
			t.Fatal("polled with a Line channel: the loop (beacon, listener) must run")
		}
	})
	t.Run("two players on channels 3 and 4 by player number, whoever arrives first", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		b := clNewUnit(t, "jp07", "192.0.2.12")
		a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 2})
		b.post(t, clDeck{deck: 1, title: "B1", active: true, player: 1})
		for _, order := range [][]*clUnit{{a, b}, {b, a}} {
			s, srv := clConsole(t, "jc11", true)
			for _, u := range order {
				u.send(s)
			}
			_, lanes := clShadow(t, srv, "3,4")
			if got := clLaneView(lanes); got != "3=B1/P1 4=A1/P2" {
				t.Fatalf("order %s first: %s", order[0].ip, got)
			}
			if lanes[0].Model != "SC5000" || lanes[1].Model != "SC6000" {
				t.Fatalf("models: %+v", lanes)
			}
		}
	})
	t.Run("lines 2 and 4 take the numbers in ascending order", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		b := clNewUnit(t, "jp13", "192.0.2.12")
		a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 3})
		b.post(t, clDeck{deck: 1, title: "B1", active: true, player: 2})
		s, srv := clConsole(t, "jc11", true)
		a.send(s)
		b.send(s)
		if _, lanes := clShadow(t, srv, "4,2"); clLaneView(lanes) != "2=B1/P2 4=A1/P3" {
			t.Fatalf("lanes %s", clLaneView(lanes))
		}
	})
	t.Run("more units than Line channels: the lowest numbers are shown", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		b := clNewUnit(t, "jp13", "192.0.2.12")
		a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 1})
		b.post(t, clDeck{deck: 1, title: "B1", active: true, player: 2})
		s, srv := clConsole(t, "jc11", true)
		b.send(s)
		a.send(s)
		if _, lanes := clShadow(t, srv, "3"); clLaneView(lanes) != "3=A1/P1" {
			t.Fatalf("lanes %s", clLaneView(lanes))
		}
	})
	t.Run("a player and a CDJ share one order", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 2})
		s, srv := clConsole(t, "jc11", true)
		a.send(s)
		cdj := prolink.Device{Address: "192.0.2.30", Name: "CDJ-3000", Number: 1, Title: "C1", LastSeen: time.Now()}
		prolinkMu.Lock()
		postedDevices[s] = map[string]prolink.Device{deviceKey(cdj): cdj}
		prolinkMu.Unlock()
		_, lanes := clShadow(t, srv, "3,4")
		if got := clLaneView(lanes); got != "3=C1/P1 4=A1/P2" {
			t.Fatalf("CDJ number 1 first, then the player: %s", got)
		}
		if lanes[0].Model != "CDJ-3000" || !lanes[0].LineIn || !lanes[0].DisplayOnly {
			t.Fatalf("CDJ lane: %+v", lanes[0])
		}
		// The same number on both: the EngineX unit goes first, as a tie always does.
		cdj.Number = 2
		prolinkMu.Lock()
		postedDevices[s] = map[string]prolink.Device{deviceKey(cdj): cdj}
		prolinkMu.Unlock()
		if _, lanes := clShadow(t, srv, "3,4"); clLaneView(lanes) != "3=A1/P2 4=C1/P2" {
			t.Fatalf("a tie: %s", clLaneView(lanes))
		}
	})
	t.Run("a post without a player number comes after the numbered ones, by address", func(t *testing.T) {
		old1 := clNewUnit(t, "jp13", "192.0.2.12") // an older build: no number
		old2 := clNewUnit(t, "jp13", "192.0.2.11")
		num := clNewUnit(t, "jp13", "192.0.2.13")
		old1.post(t, clDeck{deck: 1, title: "O1", active: true})
		old2.post(t, clDeck{deck: 1, title: "O2", active: true})
		num.post(t, clDeck{deck: 1, title: "N4", active: true, player: 4})
		s, srv := clConsole(t, "jc11", true)
		for _, u := range []*clUnit{old1, num, old2} {
			u.send(s)
		}
		if _, lanes := clShadow(t, srv, "1,2,3"); clLaneView(lanes) != "1=N4/P4 2=O2 3=O1" {
			t.Fatalf("lanes %s", clLaneView(lanes))
		}
	})
	t.Run("a player with two layers shows its active layer", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		s, srv := clConsole(t, "jc11", true)
		a.post(t, clDeck{deck: 1, title: "L-A", active: true, player: 1}, clDeck{deck: 3, title: "L-B", player: 1})
		a.send(s)
		if _, lanes := clShadow(t, srv, "3"); clLaneView(lanes) != "3=L-A/P1" {
			t.Fatalf("layer A active: %s", clLaneView(lanes))
		}
		a.post(t, clDeck{deck: 1, title: "L-A", player: 1}, clDeck{deck: 3, title: "L-B", active: true, player: 1})
		a.send(s)
		if _, lanes := clShadow(t, srv, "3"); clLaneView(lanes) != "3=L-B/P1" {
			t.Fatalf("layer B active: %s", clLaneView(lanes))
		}
	})
	t.Run("nothing is loaded, copied or placed on a channel that is not on Line", func(t *testing.T) {
		a := clNewUnit(t, "jp13", "192.0.2.11")
		a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 1}, clDeck{deck: 2, title: "A2", player: 1})
		s, srv := clConsole(t, "jc11", true)
		a.send(s)
		_, lanes := clShadow(t, srv, "4")
		if len(lanes) != 1 || lanes[0].Deck != 4 {
			t.Fatalf("only the Line channel gets a lane: %+v", lanes)
		}
		for _, l := range lanes {
			if l.Path != "" || l.Net != "" {
				t.Fatalf("a lane carries a file for Engine to load: %+v", l)
			}
		}
		assertNoShadowAudioFetch(t, s)
	})
	t.Run("the other consoles too", func(t *testing.T) {
		for _, board := range []string{"jc11s", "jp21", "jp24"} {
			a := clNewUnit(t, "jp13", "192.0.2.11")
			a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 1})
			s, srv := clConsole(t, board, true)
			a.send(s)
			if _, lanes := clShadow(t, srv, "3"); clLaneView(lanes) != "3=A1/P1" {
				t.Fatalf("%s: %s", board, clLaneView(lanes))
			}
		}
	})
}

// The lane's picture comes from the same endpoints as any display lane; on a console without a
// role they used to answer nothing.
func TestConsoleLineDisplayEndpointsServeTheLane(t *testing.T) {
	cache, dev := cachedDisplayCDJ(t)
	dev.Number = 2
	dev.SourcePlayer = 2
	if err := os.Symlink(filepath.Join(cache.Dir, "127.0.0.1", "1-3"), filepath.Join(cache.Dir, "127.0.0.1", "2-3")); err != nil {
		t.Fatal(err)
	}
	clSetBoard(t, "jc11")
	s := displayCDJServer(t, cache, dev)
	os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("prolink_share=1\nconsole_line_view=1\n"), 0o600)
	srv := httptest.NewServer(s)
	defer srv.Close()
	get := func(path string, want int) []byte {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("GET %s: %d, want %d (%s)", path, resp.StatusCode, want, b)
		}
		return b
	}
	var lanes []clLane
	var body struct{ Decks []clLane }
	json.Unmarshal(get("/v1/enginex/shadow?line=3", 200), &body)
	lanes = body.Decks
	if len(lanes) != 1 || lanes[0].Deck != 3 || lanes[0].Key == "" || !lanes[0].LineIn || !lanes[0].DisplayOnly {
		t.Fatalf("CDJ on Line channel 3: %+v", lanes)
	}
	q := "?deck=3&key=" + url.QueryEscape(lanes[0].Key)
	var meta struct {
		Detail        bool
		WaveformTiles []string
	}
	if err := json.Unmarshal(get("/v1/enginex/shadow/display"+q, 200), &meta); err != nil || !meta.Detail || len(meta.WaveformTiles) != 1 {
		t.Fatalf("display asset: %v %+v", err, meta)
	}
	if _, err := png.Decode(bytes.NewReader(get("/v1/enginex/shadow/waveform.png"+q+"&tile=0", 200))); err != nil {
		t.Fatal(err)
	}
	get("/v1/enginex/shadow/display?deck=3&key=wrong", 404)
	get("/v1/enginex/shadow/display?deck=4&key="+url.QueryEscape(lanes[0].Key), 404) // a channel that is not the CDJ's
	assertNoShadowAudioFetch(t, s)

	// Off: the same request is refused again.
	os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("prolink_share=1\n"), 0o600)
	get("/v1/enginex/shadow/display"+q, 404)
}

// The post carries the player number (read once by the QML), and stream-sync adds the
// model, free, from the board it runs on.
func TestOwnDecksCarryPlayerNumberAndModel(t *testing.T) {
	a := clNewUnit(t, "jp13", "192.0.2.11")
	a.post(t, clDeck{deck: 1, title: "A1", active: true, player: 3})
	var got []map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(a.packet(), []byte(shadowDecksMagic)), &got); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", err, got)
	}
	if got[0]["player"] != float64(3) || got[0]["model"] != "SC6000" {
		t.Fatalf("what a peer receives: %v", got[0])
	}
	// An older sender's post has no number: nothing is invented.
	b := clNewUnit(t, "jp07", "192.0.2.12")
	b.post(t, clDeck{deck: 1, title: "B1", active: true})
	got = nil // Unmarshal would reuse the first map
	json.Unmarshal(bytes.TrimPrefix(b.packet(), []byte(shadowDecksMagic)), &got)
	if _, has := got[0]["player"]; has || got[0]["model"] != "SC5000" {
		t.Fatalf("no number posted: %v", got[0])
	}
}

// PIN. Two standalone players, with and without a console on the network, show each other
// exactly what they showed on 5.3.0.5-r1: the rows of Answer 2 of the design report. The
// console here is the worst case a stage-1 console can be: it beacons and sends no deck (a
// beacon, a goodbye "[]" and an empty answer). The new player-number and model fields never
// reach a player's lane answer either.
func TestPlayerLanesDoNotMoveWithAConsoleOnTheNetwork(t *testing.T) {
	rows := []struct {
		name         string
		roleA, roleB string
		decksA       []int
		decksB       []int
		wantA, wantB string
	}{
		{"complementary 1+3 / 2+4", "13", "24", []int{1, 3}, []int{2, 4}, "1=own 2=B2 3=own 4=B4", "1=A1 2=own 3=A3 4=own"},
		{"both 3+4, one deck each", "34", "34", []int{3}, []int{4}, "1=B4 2=- 3=own 4=own", "1=A3 2=- 3=own 4=own"},
		{"both 3+4, both decks", "34", "34", []int{3, 4}, []int{3, 4}, "1=B3 2=B4 3=own 4=own", "1=A3 2=A4 3=own 4=own"},
		{"single decks 1 and 2", "1", "2", []int{1}, []int{2}, "1=own 2=B2 3=- 4=-", "1=A1 2=own 3=- 4=-"},
		{"both 1+3", "13", "13", []int{1, 3}, []int{1, 3}, "1=own 2=B1 3=own 4=B3", "1=own 2=A1 3=own 4=A3"},
	}
	laneView := func(s *Server, role string) string {
		var out []string
		byLane := map[int]string{}
		got := s.shadowComposeLegacy(role, time.Now())
		if b, _ := json.Marshal(got); strings.Contains(string(b), `"player"`) || strings.Contains(string(b), `"model"`) {
			t.Fatalf("a player's lane answer carries console-only fields: %s", b)
		}
		for _, d := range got {
			byLane[d.Deck] = d.Title
		}
		for lane := 1; lane <= 4; lane++ {
			switch {
			case strings.Contains(role, strconv.Itoa(lane)):
				out = append(out, fmt.Sprintf("%d=own", lane))
			case byLane[lane] != "":
				out = append(out, fmt.Sprintf("%d=%s", lane, byLane[lane]))
			default:
				out = append(out, fmt.Sprintf("%d=-", lane))
			}
		}
		return strings.Join(out, " ")
	}
	for _, withConsole := range []bool{false, true} {
		for _, r := range rows {
			t.Run(fmt.Sprintf("%s console=%v", r.name, withConsole), func(t *testing.T) {
				a := clNewUnit(t, "jp13", "192.0.2.11")
				b := clNewUnit(t, "jp07", "192.0.2.12")
				var da, db []clDeck
				for _, d := range r.decksA {
					da = append(da, clDeck{deck: d, title: "A" + strconv.Itoa(d), active: true, player: 1})
				}
				for _, d := range r.decksB {
					db = append(db, clDeck{deck: d, title: "B" + strconv.Itoa(d), active: true, player: 2})
				}
				a.post(t, da...)
				b.post(t, db...)
				a.send(b.s)
				b.send(a.s)
				if withConsole {
					for _, p := range []*Server{a.s, b.s} {
						p.shadow.receive([]byte("EngineX-shadow 1"), "192.0.2.20", time.Now())
						p.shadow.receive([]byte(shadowDecksMagic+"[]"), "192.0.2.20", time.Now())
						p.shadow.mu.Lock()
						p.shadow.takePeer("192.0.2.20", nil, time.Now()) // the answer of an empty /peer/decks
						p.shadow.mu.Unlock()
					}
				}
				if got := laneView(a.s, r.roleA); got != r.wantA {
					t.Fatalf("A(%s): %s, want %s", r.roleA, got, r.wantA)
				}
				if got := laneView(b.s, r.roleB); got != r.wantB {
					t.Fatalf("B(%s): %s, want %s", r.roleB, got, r.wantB)
				}
			})
		}
	}
}
