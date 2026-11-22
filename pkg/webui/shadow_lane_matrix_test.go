package webui

// The lanes two standalone players show each other, the player columns of the tables of the console
// linking design (Answer 2 and Answer 5): the rows that need no console at all, and the same rows with a
// console on the network that sends nothing (it beacons, says goodbye, answers /peer/decks empty), which is
// every console before stage 2. The table pins what a player with the row Show Console off shows: it passes
// unchanged on the code before stage 2 and after it, in the three modes (Engine, auto, All CDJ), and the
// answer carries no console lane. Only helpers that exist before stage 2 are used.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// laneMatrixRows: two players A (JP13) and B (JP07) with their roles and the decks they have loaded, and the lanes
// each shows of the other (1=B4: lane 1 shows B's deck 4; own: its own deck; -: empty).
var laneMatrixRows = []struct {
	name         string
	roleA, roleB string
	decksA       []int
	decksB       []int
	wantA, wantB string
}{
	{"Report B: both 3+4, A has deck 3, B deck 4", "34", "34", []int{3}, []int{4}, "1=B4 2=- 3=own 4=own", "1=A3 2=- 3=own 4=own"},
	{"both 3+4, both decks loaded", "34", "34", []int{3, 4}, []int{3, 4}, "1=B3 2=B4 3=own 4=own", "1=A3 2=A4 3=own 4=own"},
	{"both 3+4, deck 3 on each", "34", "34", []int{3}, []int{3}, "1=B3 2=- 3=own 4=own", "1=A3 2=- 3=own 4=own"},
	{"complementary 1+3 and 2+4, all loaded", "13", "24", []int{1, 3}, []int{2, 4}, "1=own 2=B2 3=own 4=B4", "1=A1 2=own 3=A3 4=own"},
	{"both 1+3, all loaded", "13", "13", []int{1, 3}, []int{1, 3}, "1=own 2=B1 3=own 4=B3", "1=own 2=A1 3=own 4=A3"},
	{"A 3+4 and B 1+3, all loaded", "34", "13", []int{3, 4}, []int{1, 3}, "1=B1 2=B3 3=own 4=own", "1=own 2=A3 3=own 4=A4"},
	{"one player alone on 3+4", "34", "34", []int{3, 4}, nil, "1=- 2=- 3=own 4=own", "1=A3 2=A4 3=own 4=own"},
	{"single decks 1 and 2", "1", "2", []int{1}, []int{2}, "1=own 2=B2 3=- 4=-", "1=A1 2=own 3=- 4=-"},
	{"single decks 2 and 1", "2", "1", []int{2}, []int{1}, "1=B1 2=own 3=- 4=-", "1=own 2=A2 3=- 4=-"},
	{"A 1+3 (deck 1), B 2+4 (deck 2)", "13", "24", []int{1}, []int{2}, "1=own 2=B2 3=own 4=-", "1=A1 2=own 3=- 4=own"},
	{"single decks 1 and 1", "1", "1", []int{1}, []int{1}, "1=own 2=B1 3=- 4=-", "1=own 2=A1 3=- 4=-"},
}

func TestPlayerLaneMatrixIsTheSameWithTheRowOffAndAConsoleAround(t *testing.T) {
	rows := laneMatrixRows
	modes := []string{"engine", "auto", "cdj"}
	laneView := func(s *Server, mode, role string) string {
		got := s.shadowComposeMode(mode, role, time.Now())
		if b, _ := json.Marshal(got); strings.Contains(string(b), `"console"`) {
			t.Fatalf("%s: a lane answer carries a console lane with the row off: %s", mode, b)
		}
		byLane := map[int]string{}
		for _, d := range got {
			byLane[d.Deck] = d.Title
		}
		var out []string
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
				if len(db) > 0 {
					b.post(t, db...)
				}
				a.send(b.s)
				if len(db) > 0 {
					b.send(a.s)
				}
				if withConsole {
					for _, p := range []*Server{a.s, b.s} {
						p.shadow.receive([]byte("EngineX-shadow 1"), "192.0.2.20", time.Now())
						p.shadow.receive([]byte(shadowDecksMagic+"[]"), "192.0.2.20", time.Now())
						p.shadow.mu.Lock()
						p.shadow.takePeer("192.0.2.20", nil, time.Now()) // the answer of an empty /peer/decks
						p.shadow.mu.Unlock()
					}
				}
				for _, mode := range modes {
					if got := laneView(a.s, mode, r.roleA); got != r.wantA {
						t.Fatalf("A(%s) in %s mode: %s, want %s", r.roleA, mode, got, r.wantA)
					}
					if len(db) > 0 || r.wantB != "" {
						if got := laneView(b.s, mode, r.roleB); got != r.wantB {
							t.Fatalf("B(%s) in %s mode: %s, want %s", r.roleB, mode, got, r.wantB)
						}
					}
				}
			})
		}
	}
}
