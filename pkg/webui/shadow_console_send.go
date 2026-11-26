package webui

// Console linking, stage 2: a player can show the decks of a linked console in the lanes it has
// free (console_decks, the row Show Console, default off). The console sends only while a player
// asks, and a player asks only with the row on. Display only: nothing of the console is loaded
// into a player's deck, no audio is copied, no MASTER or Sync candidate is made.
//
// The protocol, five lines:
//  1. A player with the row on broadcasts shadowAskMagic on UDP shadowAskPort every 2 s, beside
//     its beacon, and pulls GET /peer/decks?console=1 from every peer it knows.
//  2. A console with Line In on listens on that port (a bound socket, nothing else), answers an
//     ask with a unicast beacon, so the asker finds it, and serves its decks to a pull with
//     console=1 and to nobody else. An older unit has no listener and ignores the query.
//  3. A pull with console=1 is an ask too: somebody asked in the last 6 s is what the console's
//     QML is told (a held request, GET /v1/enginex/consolesend), and it samples only then.
//  4. The player keeps a console's decks apart from its peers' (consoleDecks, never peerDecks),
//     so what the players show each other does not move, and places them in the lanes still free.
//  5. Silence ends it: no ask for 6 s, and the QML stops sampling; a channel put on Line / AUX
//     leaves the next post, and the player lets the deck go.
//
// Share Decks (#329) is the same channel the other way round: a player in the Stock layout with the row
// Share Decks on is shown by a console's Line In view without the 4-deck layout.
//  1. A console with Line In on, while a channel is on Line / AUX (its deck loop runs), broadcasts
//     shadowAskPlayerMagic on shadowAskPort beside its beacon and pulls GET /peer/decks?line=1.
//  2. A player with the row on listens on that port (the same bound socket), answers with a unicast
//     beacon, so the console finds it, and serves its decks to a pull with line=1 and to nobody else.
//  3. An ask or a pull with line=1 is noted (askers); the player's QML is told by the same held request
//     and samples only then. Its posts go to the consoles that asked, unicast, nowhere else: no deck loop,
//     no beacon of its own, no Pro DJ Link status, no MASTER claim.
//  4. Silence ends it, as above. An older unit has no listener and ignores the query.

import (
	"cmp"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// shadowAskPort is where a console listens for a player's ask; a var so tests use their own.
var shadowAskPort = 50198

const shadowAskMagic = "EngineX-ask-console 1"

// shadowAskPlayerMagic is what a console with Line In on says beside its beacon: a player with Share Decks answers.
const shadowAskPlayerMagic = "EngineX-ask-player 1"

// consoleAskHold is how long the QML's request is held while nothing changes; a var so tests need not wait.
var consoleAskHold = 25 * time.Second

// isConsoleModel tells a deck of an all-in-one console by the Model its sender stamped.
func isConsoleModel(model string) bool { return consoleLineBoards[model] }

// applyShadowFlags is called once at start and after every save: the loop, the pulls and the compose
// read these flags, not the settings file.
func (s *Server) applyShadowFlags(v uiSettings) {
	sh := s.shadow
	sh.askConsole.Store(v.ConsoleDecks && playerBoards[sh.model])
	s.consoleAskSync(v.ConsoleLineView && consoleLineBoards[sh.model])
	// Share Decks takes effect in the Stock layout only: in the 4-deck layout the player beacons and is shown anyway.
	s.shareAskSync(v.ShareDecks && !v.FourDeckWaveform && playerBoards[sh.model])
}

// ---- console side ----

// askedNow: some player asked within shadowPeerSeenTimeout. Called with sh.mu held.
func (sh *shadowState) askedNow(now time.Time) bool {
	asked := false
	for ip, t := range sh.askers {
		if now.Sub(t) < shadowPeerSeenTimeout {
			asked = true
		} else {
			delete(sh.askers, ip)
		}
	}
	return asked
}

// ask notes a player's ask and wakes the QML's held request when it is the first. Called with sh.mu held.
func (sh *shadowState) ask(ip string, now time.Time) {
	first := !sh.askedNow(now)
	sh.askers[ip] = now
	if first {
		close(sh.askNotify)
		sh.askNotify = make(chan struct{})
	}
}

// consoleAskSync opens or closes the listener: up only while Line In is on, on a console.
func (s *Server) consoleAskSync(want bool) {
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.hears.Store(want)
	s.askListenSync()
}

// shareAskSync opens or closes the same listener on a player: up only while Share Decks is on in the Stock layout.
// Switched off, the decks it shared are forgotten: a console that still pulls gets nothing, not the last picture.
func (s *Server) shareAskSync(want bool) {
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.shares.Swap(want) && !want {
		sh.own, sh.ownAt = nil, time.Time{}
	}
	s.askListenSync()
}

// askListenSync opens the ask listener while a console hears or a player shares, and closes it, forgetting the
// asks, when neither does (a unit is one or the other). Called with sh.mu held.
func (s *Server) askListenSync() {
	sh := s.shadow
	if want := sh.hears.Load() || sh.shares.Load(); !want {
		if sh.askConn != nil {
			sh.askConn.Close()
			sh.askConn = nil
		}
		if len(sh.askers) > 0 {
			clear(sh.askers)
			close(sh.askNotify)
			sh.askNotify = make(chan struct{})
		}
		return
	}
	if sh.askConn != nil || !sh.beacon {
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: shadowAskPort})
	if err != nil {
		return // the pulls with console=1 still ask; only a player that has not found us yet cannot wake us
	}
	sh.askConn = conn
	go s.consoleAskLoop(conn)
}

// consoleAskLoop reads the asks: a blocked read, nothing else, until the listener is closed. A console takes a
// player's ask, a player with Share Decks a Line In console's; each ignores the other kind.
func (s *Server) consoleAskLoop(conn *net.UDPConn) {
	sh := s.shadow
	b := make([]byte, 64)
	for {
		n, addr, err := conn.ReadFromUDP(b)
		if err != nil {
			return
		}
		if msg := string(b[:n]); !(msg == shadowAskMagic && sh.hears.Load()) && !(msg == shadowAskPlayerMagic && sh.shares.Load()) {
			continue
		}
		sh.mu.Lock()
		port := cmp.Or(sh.pushPort, shadowBeaconPort)
		sh.ask(addr.IP.String(), time.Now())
		sh.mu.Unlock()
		// The asker finds us by our beacon, as it finds any unit.
		conn.WriteToUDP([]byte("EngineX-shadow 1"), &net.UDPAddr{IP: addr.IP, Port: port})
	}
}

// consoleSendGet is the console QML's held request: "asked" is what it believes now, the answer comes when
// that changes or after consoleAskHold. Loopback only (/v1/).
func (s *Server) consoleSendGet(w http.ResponseWriter, r *http.Request) {
	sh := s.shadow
	believed := r.URL.Query().Get("asked") == "1"
	deadline := time.NewTimer(consoleAskHold)
	defer deadline.Stop()
	for {
		sh.mu.Lock()
		asked, changed := sh.askedNow(time.Now()), sh.askNotify
		sh.mu.Unlock()
		if asked != believed {
			respond(w, map[string]bool{"asked": asked})
			return
		}
		var expire <-chan time.Time // only while asked: the moment the last ask runs out is not announced by anything else
		if asked {
			t := time.NewTimer(time.Second)
			defer t.Stop()
			expire = t.C
		}
		select {
		case <-changed:
		case <-expire:
		case <-deadline.C:
			respond(w, map[string]bool{"asked": asked})
			return
		case <-r.Context().Done():
			return
		case <-s.closed:
			return
		}
	}
}

// remoteIP is the address a request came from.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// ---- player side ----

// takeConsole keeps the decks a console sent in answer to an ask. A console's decks are never peerDecks: a
// peer's slot is a player's, and what two players show each other must not depend on a console. One console
// at a time (the first keeps the lanes while it is heard); a deck missing from its newest answer is let go.
func (sh *shadowState) takeConsole(ip string, decks []deckState, now time.Time) {
	if sh.consoleIP != "" && sh.consoleIP != ip && len(sh.consoleDecks) > 0 {
		return
	}
	changed := false
	for n := range sh.consoleDecks {
		if !slices.ContainsFunc(decks, func(d deckState) bool { return d.Deck == n }) {
			delete(sh.consoleDecks, n)
			changed = true
		}
	}
	if len(decks) > 0 {
		sh.consoleIP = ip
	}
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 {
			continue
		}
		d.Sent, d.Rate = 0, 0
		p := peerDeck{deckState: d, ip: ip, at: now, anchorPos: d.PositionMS, anchorAt: now}
		if old, ok := sh.consoleDecks[d.Deck]; ok && samePeerPicture(old, d, now) {
			p.anchorPos, p.anchorAt = old.anchorPos, old.anchorAt
		} else {
			changed = true
		}
		sh.consoleDecks[d.Deck] = p
	}
	if len(sh.consoleDecks) == 0 {
		sh.consoleIP = ""
	}
	if changed {
		sh.bump()
	}
}

// splitConsole parts a pull's decks: a console's (by Model) and the others'.
func splitConsole(decks []deckState) (console, others []deckState) {
	for _, d := range decks {
		if isConsoleModel(d.Model) {
			console = append(console, d)
		} else {
			others = append(others, d)
		}
	}
	return console, others
}

// consoleLanes puts the console's decks in the lanes still free once the player's own, its peers' and the
// CDJs' are placed (taken, as composed). Nothing already placed is moved, and a console deck keeps the lane it
// has while that lane stays free: a deck lands on the lane of its own number when that is free, else on the
// lowest free one, in ascending console deck number; one that finds none is left out, and never takes a lane
// from a deck that keeps it. Called with the row on only.
func (s *Server) consoleLanes(role string, taken []deckState, now time.Time) []consoleLane {
	sh := s.shadow
	if !sh.askConsole.Load() || role == "" {
		return nil
	}
	sh.mu.Lock()
	var decks []peerDeck
	for n := 1; n <= 4; n++ {
		if p, ok := sh.consoleDecks[n]; ok && !p.Stale && now.Sub(p.at) < 2*time.Second && (p.Net != "" || p.Key != "") {
			decks = append(decks, p)
		}
	}
	if sh.consoleLane == nil {
		sh.consoleLane = map[int]int{}
	}
	for n := range sh.consoleLane { // a deck that left has left its lane too
		if !slices.ContainsFunc(decks, func(p peerDeck) bool { return p.Deck == n }) {
			delete(sh.consoleLane, n)
		}
	}
	free := map[int]bool{}
	for n := 1; n <= 4; n++ {
		if !strings.Contains(role, strconv.Itoa(n)) && !slices.ContainsFunc(taken, func(d deckState) bool { return d.Deck == n }) {
			free[n] = true
		}
	}
	place := map[int]int{}    // console deck -> lane
	for _, p := range decks { // the lane a deck holds stays its own while it is free
		if lane := sh.consoleLane[p.Deck]; free[lane] {
			place[p.Deck] = lane
			free[lane] = false
		}
	}
	for _, p := range decks {
		if _, ok := place[p.Deck]; ok {
			continue
		}
		lane := 0
		if free[p.Deck] {
			lane = p.Deck
		} else {
			for n := 1; n <= 4 && lane == 0; n++ {
				if free[n] {
					lane = n
				}
			}
		}
		if lane == 0 {
			continue // no lane left: left out
		}
		place[p.Deck] = lane
		free[lane] = false
	}
	sh.consoleLane = place
	sh.mu.Unlock()
	var out []consoleLane
	for _, p := range decks {
		lane, ok := place[p.Deck]
		if !ok {
			continue
		}
		d, e := s.peerDisplayDeck(p, lane, now)
		d.Console, d.Src = p.Deck, 0
		d.Active, d.Master, d.Lead, d.ShadowMaster = false, new(false), new(false), 0 // a picture: never a MASTER, never a layer's active deck
		out = append(out, consoleLane{d: d, e: e})
	}
	return out
}

// shadowSendAsk is what a player with the row on says beside its beacon.
func shadowSendAsk(conn *net.UDPConn) {
	shadowBroadcast(conn, []byte(shadowAskMagic), shadowAskPort)
}

// shadowSendAskPlayers is what a console with Line In on says beside its beacon (Share Decks).
func shadowSendAskPlayers(conn *net.UDPConn) {
	shadowBroadcast(conn, []byte(shadowAskPlayerMagic), shadowAskPort)
}

// sharePush sends a Share Decks player's decks to the consoles that asked, from its listener's socket, unicast:
// to nobody else, and nothing at all without an ask.
func (s *Server) sharePush() {
	sh := s.shadow
	now := time.Now()
	sh.mu.Lock()
	conn, port := sh.askConn, cmp.Or(sh.pushPort, shadowBeaconPort)
	var ips []net.IP
	for ip, t := range sh.askers {
		if now.Sub(t) < shadowPeerSeenTimeout {
			ips = append(ips, net.ParseIP(ip))
		}
	}
	b, err := json.Marshal(sh.ownForPeer(now))
	sh.mu.Unlock()
	if conn == nil || err != nil || len(ips) == 0 {
		return
	}
	pkt := append([]byte(shadowDecksMagic), b...)
	for _, ip := range ips {
		if ip != nil {
			conn.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: port})
		}
	}
}

// consoleDeckEntry is the display data of a console's deck as the lane at (deck, key) shows it.
func (s *Server) consoleDeckEntry(console int, now time.Time) *shadowDisplayEntry {
	sh := s.shadow
	sh.mu.Lock()
	p, ok := sh.consoleDecks[console]
	sh.mu.Unlock()
	if !ok {
		return nil
	}
	return s.ensureShadowDisplay(s.localPeerTrack(p, now), now)
}
