package webui

// Console linking, stage 1 (console_line_view): a console shows a linked unit, an EngineX
// player or a CDJ, on a mixer channel while that channel's input is Line / AUX. Display
// only. Nothing is loaded into Engine, nothing is copied, no deck is taken and no hook is
// involved: the picture is the display renderer's (PNG tiles from stream-sync), as for a
// CDJ in All CDJ Mode. The console sends nothing (stage 2 would); it beacons and listens
// only while its QML polls, and the QML polls only while a channel is on Line / AUX.

import (
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// consoleLane is what one Line / AUX channel shows: the state for the QML and the entry
// its picture comes from (nil until that is ready).
type consoleLane struct {
	d deckState
	e *shadowDisplayEntry
}

// consoleUnit is one linked unit that can take a channel: an EngineX player (the deck of
// its active layer) or a CDJ.
type consoleUnit struct {
	player int  // Engine Link player number (EngineX) or Pro DJ Link number (CDJ); 0: an older sender, unknown
	cdj    bool // else an EngineX player
	id     string
	peer   peerDeck       // EngineX: the deck shown
	dev    prolink.Device // CDJ
}

// consoleUnitCmp is the one order EngineX players and CDJs take the Line channels in:
// ascending player number; the numbered ones first, a post without a number after them; on
// a tie the EngineX unit first; then by link id (address). It is the rule 5.3.0.4 had for CDJs.
func consoleUnitCmp(a, b consoleUnit) int {
	if (a.player > 0) != (b.player > 0) {
		if a.player > 0 {
			return -1
		}
		return 1
	}
	if a.player != b.player {
		return a.player - b.player
	}
	if a.cdj != b.cdj {
		if !a.cdj {
			return -1
		}
		return 1
	}
	return strings.Compare(a.id, b.id)
}

// consoleActiveDeck is the deck of a unit's active layer; none says it: the first with a track, else the lowest.
func consoleActiveDeck(decks []peerDeck) peerDeck {
	slices.SortFunc(decks, func(a, b peerDeck) int { return a.Deck - b.Deck })
	for _, p := range decks {
		if p.Active {
			return p
		}
	}
	for _, p := range decks {
		if p.Key != "" || p.Net != "" || p.Title != "" {
			return p
		}
	}
	return decks[0]
}

// consoleLineUnits lists the units a console can show now in the order they take channels, and
// the Pro DJ Link devices it read for them. A peer counts for 15 s after its last word, as a goodbye does.
func (s *Server) consoleLineUnits(now time.Time) ([]consoleUnit, []prolink.Device) {
	sh := s.shadow
	byUnit := map[string][]peerDeck{}
	sh.mu.Lock()
	for _, p := range sh.peerDecks {
		if now.Sub(p.at) > 15*time.Second {
			continue
		}
		id := p.Link
		if id == "" {
			id = p.ip
		}
		byUnit[id] = append(byUnit[id], p)
	}
	sh.mu.Unlock()
	var units []consoleUnit
	for id, decks := range byUnit {
		player := 0
		for _, p := range decks {
			if p.Player > 0 {
				player = p.Player
			}
		}
		units = append(units, consoleUnit{player: player, id: id, peer: consoleActiveDeck(decks)})
	}
	devices := s.mergedDevices()
	for _, dev := range liveCDJs(devices, now) {
		units = append(units, consoleUnit{player: int(dev.Number), cdj: true, id: fmt.Sprintf("%s#%d", dev.Address, dev.Number), dev: dev})
	}
	slices.SortFunc(units, consoleUnitCmp)
	return units, devices
}

// cdjProlinkOff is the one reason a console's Line In lane gives (deckState.Fallback, which the lane's
// busy line shows while there is no waveform to draw): a CDJ is on the lane, it has a track, and this
// unit's Pro DJ Link switch is off, so the display cache never fetches the CDJ's analysis. The switch
// decides whether this unit takes part in Pro DJ Link at all; a display option does not override it.
// The reasons of cdjFallback (link, media, streaming, network) stay unset on a Line In lane: they
// explain a load that does not exist there.
const cdjProlinkOff = "prolink-off"

// consoleLineLanes puts the units, in that order, on the channels that are on Line / AUX,
// in ascending channel order. A channel not on Line gets nothing; more units than channels:
// the ones with the lowest numbers are shown.
func (s *Server) consoleLineLanes(now time.Time, lines []int) []consoleLane {
	lines = slices.Clone(lines)
	slices.Sort(lines)
	if len(lines) == 0 {
		return nil
	}
	units, devices := s.consoleLineUnits(now)
	var out []consoleLane
	for i, u := range units {
		if i >= len(lines) {
			break
		}
		var l consoleLane
		if u.cdj {
			why := ""
			if u.dev.TrackID != 0 && !s.loadUI().ProlinkShare {
				why = cdjProlinkOff // nothing to explain on a CDJ with no track
			}
			l.d, l.e = s.cdjDisplayDeck(u.dev, lines[i], true, why, devices, now)
			l.d.Player, l.d.Model = int(u.dev.Number), u.dev.Name
		} else {
			l.d, l.e = s.peerDisplayDeck(u.peer, lines[i], now)
			l.d.LineIn = true
			l.d.Player, l.d.Model = u.player, u.peer.Model
		}
		l.d.Src = 0
		out = append(out, l)
	}
	return out
}

func (s *Server) shadowComposeConsoleLine(now time.Time, lines []int) []deckState {
	out := []deckState{}
	for _, l := range s.consoleLineLanes(now, lines) {
		out = append(out, l.d)
	}
	return out
}

// consoleLineGet answers GET /v1/enginex/shadow on a console with console_line_view on. The
// QML names the channels on Line / AUX (?line=3,4) and asks only while there are some; with none
// it gets an empty answer and the deck loop is not woken (it ends 10 s after the last poll,
// which closes UDP 50199 and stops the beacon). The answer is always display only.
func (s *Server) consoleLineGet(w http.ResponseWriter, r *http.Request, debug bool) {
	sh := s.shadow
	query := r.URL.Query()
	seen, seenErr := strconv.ParseUint(query.Get("ver"), 10, 64)
	lines := parseLineChannels(query.Get("line"))
	out := []deckState{}
	role := ""
	if len(lines) > 0 {
		role = "line"
		sh.mu.Lock()
		sh.lines = lines
		sh.mu.Unlock()
		s.shadowWake()
		if seenErr == nil {
			sh.wait(r.Context(), seen, sh.pollWait(false, time.Now()))
		}
		out = s.shadowComposeConsoleLine(time.Now(), lines)
	}
	metaOnly := slices.ContainsFunc(out, func(d deckState) bool { return d.MetadataOnly || strings.HasPrefix(d.Key, "cdj:") })
	sh.mu.Lock()
	sh.metaOnlyPoll = metaOnly
	ver := sh.ver
	sh.mu.Unlock()
	stale := slices.ContainsFunc(out, func(d deckState) bool { return d.Stale })
	h := fnv.New64a()
	for _, d := range out {
		fmt.Fprintf(h, "%d|%s|%t|%t|%t|%t|%.2f|%d|%s;", d.Deck, d.Key, d.Loading, d.Stale, d.MetadataOnly, d.DisplayDetail, d.Progress, d.Player, d.Model)
	}
	// CDJ positions do not advance the peer version: those answers must always reach the QML.
	if !metaOnly {
		w.Header().Set("X-Shadow-State", fmt.Sprintf("%d:%t:%s:%t:%t:%x", ver, true, role, stale, debug, h.Sum64()))
	}
	respond(w, map[string]any{"role": role, "decks": out, "ver": ver, "debug": debug, "displayOnly": true})
}
