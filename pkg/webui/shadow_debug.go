package webui

// Shadow deck debug telemetry: with debug_telemetry=1 in settings.conf, or the
// file /tmp/enginex-debug, every own post, every peer update and every decision
// the receiving QML takes (POST /v1/enginex/debug/shadow) is one JSON line in
// shadowDebugLog and in a ring that GET /v1/enginex/debug/shadow serves with the
// live state. Off by default: release images never write it.

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// shadowDebugFlags turn the telemetry on by hand; /tmp/sc5000-debug is the name before
// 5.3.0.5 (#234), still accepted in 5.3.0.5 and 5.3.0.6.
var shadowDebugFlags = [...]string{"/tmp/enginex-debug", "/tmp/sc5000-debug"}

func shadowDebugFlagged() bool { return anyFileExists(shadowDebugFlags[:]) }

func anyFileExists(paths []string) bool { return slices.ContainsFunc(paths, fileExists) }

// /tmp is RAM on the deck: one rotated .1 caps the log at 2 MB.
var shadowDebugLog, shadowDebugMax = "/tmp/enginex-shadow-debug.log", int64(1 << 20)

type shadowDebug struct {
	on   atomic.Bool
	mu   sync.Mutex
	f    *os.File
	size int64
	ring [400]string
	n    int // lines so far
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// set turns the telemetry on or off and returns on.
func (d *shadowDebug) set(on bool) bool {
	if d.on.Swap(on) && !on {
		d.mu.Lock()
		if d.f != nil {
			d.f.Close()
			d.f = nil
		}
		d.mu.Unlock()
	}
	return on
}

// event logs one line; fields becomes it, with "t" (Unix ms) and "ev".
func (d *shadowDebug) event(ev string, fields map[string]any) {
	if !d.on.Load() {
		return
	}
	fields["t"], fields["ev"] = time.Now().UnixMilli(), ev
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ring[d.n%len(d.ring)] = string(b)
	d.n++
	if d.f != nil && d.size+int64(len(b))+1 > shadowDebugMax {
		d.f.Close()
		d.f = nil
		os.Rename(shadowDebugLog, shadowDebugLog+".1")
	}
	if d.f == nil {
		f, err := os.OpenFile(shadowDebugLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		st, _ := f.Stat()
		d.f, d.size = f, st.Size()
	}
	n, _ := d.f.Write(append(b, '\n'))
	d.size += int64(n)
}

// lines is the ring, oldest first.
func (d *shadowDebug) lines() []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []json.RawMessage{}
	for i := max(0, d.n-len(d.ring)); i < d.n; i++ {
		out = append(out, json.RawMessage(d.ring[i%len(d.ring)]))
	}
	return out
}

// slimDecks drops the hot cues (8 per deck) from logged decks.
func slimDecks(decks []deckState) []deckState {
	out := slices.Clone(decks)
	for i := range out {
		out[i].Cues, out[i].CuePos = nil, nil
	}
	return out
}

// shadowDebugGet is the live shadow state and the newest telemetry lines.
func (s *Server) shadowDebugGet(w http.ResponseWriter, r *http.Request) {
	sh := s.shadow
	type peer struct {
		deckState
		IP    string `json:"ip"`
		AgeMS int64  `json:"ageMs"`
	}
	sh.mu.Lock()
	now := time.Now()
	peers := map[string]int64{}
	for ip, t := range sh.peers {
		peers[ip] = now.Sub(t).Milliseconds()
	}
	decks := []peer{}
	for _, p := range sh.peerDecks {
		decks = append(decks, peer{p.deckState, p.ip, now.Sub(p.at).Milliseconds()})
	}
	slices.SortFunc(decks, func(a, b peer) int { return a.Deck - b.Deck })
	out := map[string]any{"enabled": sh.dbg.on.Load(), "ver": sh.ver, "own": slices.Clone(sh.own), "ownAgeMs": now.Sub(sh.ownAt).Milliseconds(),
		"peers": peers, "peerDecks": decks, "peerSeq": maps.Clone(sh.peerSeq), "files": maps.Clone(sh.files), "running": sh.running}
	sh.mu.Unlock()
	sh.display.mu.Lock()
	ready, busy, detail := 0, 0, 0
	for _, e := range sh.display.entries {
		if e.busy {
			busy++
		}
		if e.asset != nil {
			ready++
			if e.asset.Detail {
				detail++
			}
		}
	}
	out["displayCache"] = map[string]int{"entries": len(sh.display.entries), "ready": ready, "busy": busy, "detail": detail}
	sh.display.mu.Unlock()
	out["log"] = sh.dbg.lines()
	respond(w, out)
}

// shadowDebugPost takes the receiving QML's decision records.
func (s *Server) shadowDebugPost(w http.ResponseWriter, r *http.Request) {
	var recs []map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&recs); err != nil {
		fail(w, 400, err)
		return
	}
	for _, m := range recs {
		if qt, ok := m["t"]; ok {
			m["qt"] = qt // the QML's clock at the decision; "t" becomes arrival
		}
		s.shadow.dbg.event("qml", m)
	}
	respond(w, map[string]any{"ok": true, "enabled": s.shadow.dbg.on.Load()})
}
