package samplersequencer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"strings"
	"time"
)

const defaultWebUIURL = "http://127.0.0.1:8080"

func envWebUIURL() string {
	if u := strings.TrimSpace(os.Getenv("ENGINEX_WEBUI_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultWebUIURL
}

type deckSnapshot struct {
	Deck       int     `json:"deck"`
	Source     string  `json:"source"`
	SourceID   string  `json:"sourceId"`
	Master     bool    `json:"master"`
	Lead       bool    `json:"lead"`
	Playing    bool    `json:"playing"`
	Cue        bool    `json:"cue"`
	BPM        float64 `json:"bpm"`
	CurrentBPM float64 `json:"currentBpm"`
	Pitch      float64 `json:"pitch"`
	Beat       float64 `json:"beat"`
	PositionMS float64 `json:"positionMs"`
	Stale      bool    `json:"stale"`
	At         int64   `json:"at"`
	Sent       int64   `json:"sent"`
}

func (d deckSnapshot) effectiveBPM() float64 {
	if d.CurrentBPM > 0 {
		return d.CurrentBPM
	}
	if d.BPM > 0 {
		return d.BPM * (1 + d.Pitch/100)
	}
	return 0
}

func sourceRank(source string) int {
	switch source {
	case "local":
		return 3
	case "peer":
		return 2
	case "prolink":
		return 1
	default:
		return 0
	}
}

// selectClockDeck picks the deck to follow:
//  1. Playing MASTER / LEAD deck (local > peer > prolink)
//  2. Any playing deck (local > peer > prolink)
//  3. Paused MASTER / LEAD deck
//  4. nil (manual BPM fallback)
func selectClockDeck(decks []deckSnapshot) *deckSnapshot {
	var best *deckSnapshot
	bestTier := 0
	for i := range decks {
		d := decks[i]
		if d.Deck < 1 || d.Deck > 4 || d.Stale || d.effectiveBPM() <= 0 {
			continue
		}
		isMaster := d.Lead || d.Master
		isPlaying := d.Playing || d.Cue
		tier := 0
		switch {
		case isPlaying && isMaster:
			tier = 3
		case isPlaying:
			tier = 2
		case isMaster:
			tier = 1
		default:
			continue
		}
		if best == nil || tier > bestTier ||
			(tier == bestTier && sourceRank(d.Source) > sourceRank(best.Source)) ||
			(tier == bestTier && sourceRank(d.Source) == sourceRank(best.Source) && d.Deck < best.Deck) {
			copyDeck := d
			best = &copyDeck
			bestTier = tier
		}
	}
	return best
}

func parseHTTPBase(base string) (addr, prefix string, err error) {
	rest, ok := strings.CutPrefix(base, "http://")
	if !ok {
		return "", "", fmt.Errorf("unsupported webui url %q", base)
	}
	hostPort, p, hasSlash := strings.Cut(rest, "/")
	if hostPort == "" {
		return "", "", fmt.Errorf("missing host in %q", base)
	}
	if !strings.Contains(hostPort, ":") || (strings.HasPrefix(hostPort, "[") && !strings.Contains(hostPort, "]:")) {
		hostPort += ":80"
	}
	if hasSlash && p != "" {
		prefix = "/" + strings.TrimRight(p, "/")
	}
	return hostPort, prefix, nil
}

type httpStatusError struct{ status string }

func (e httpStatusError) Error() string { return "HTTP " + e.status }

// fetchJSON uses raw HTTP/1.0 over TCP so the static Go helper does not pull
// net/http (+~2.5 MB) into every official Mod binary.
func (f *Feature) fetchJSON(path string, out any) error {
	addr, prefix, err := parseHTTPBase(f.webUIURL)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
	reqPath := prefix + path
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n", reqPath, addr); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return err
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[1] != "200" {
		status := line
		if len(fields) >= 2 {
			status = fields[1]
		}
		return httpStatusError{status: status}
	}
	if _, err := tp.ReadMIMEHeader(); err != nil {
		return err
	}
	return json.NewDecoder(io.LimitReader(r, 256<<10)).Decode(out)
}

func isMissingV1Error(err error) (bool, time.Duration) {
	var hs httpStatusError
	if errors.As(err, &hs) && (hs.status == "404" || hs.status == "501") {
		return true, 60 * time.Second
	}
	return false, 0
}

type clockInfo struct {
	Source  string  `json:"source"`
	Deck    int     `json:"deck"`
	Playing bool    `json:"playing"`
	BPM     float64 `json:"bpm"`
	Beat    float64 `json:"beat"`
}

func (f *Feature) fetchMasterClock() clockInfo {
	type clockPayload struct {
		ClockAPI bool           `json:"clockApi"`
		Decks    []deckSnapshot `json:"decks"`
	}
	now := f.now()
	f.mu.Lock()
	skipEngineX := now.Before(f.enginexAPIAbsentUntil)
	skipLegacy := now.Before(f.clockAPIAbsentUntil)
	f.mu.Unlock()

	tryEndpoint := func(path, label string, absentUntil *time.Time) ([]deckSnapshot, bool) {
		var resp clockPayload
		err := f.fetchJSON(path, &resp)
		if err == nil && resp.ClockAPI {
			return resp.Decks, true
		}
		if err == nil {
			f.mu.Lock()
			*absentUntil = now.Add(60 * time.Second)
			f.mu.Unlock()
		} else if missing, ttl := isMissingV1Error(err); missing {
			f.mu.Lock()
			*absentUntil = now.Add(ttl)
			f.mu.Unlock()
		}
		return nil, false
	}

	var decks []deckSnapshot
	source := "manual"
	if !skipEngineX {
		if d, ok := tryEndpoint("/v1/enginex/clock", "enginex", &f.enginexAPIAbsentUntil); ok {
			decks = d
			source = "enginex"
		}
	}
	if source == "manual" && !skipLegacy {
		if d, ok := tryEndpoint("/v1/sc5000/clock", "sc5000", &f.clockAPIAbsentUntil); ok {
			decks = d
			source = "sc5000"
		}
	}
	if best := selectClockDeck(decks); best != nil {
		return clockInfo{
			Source:  source,
			Deck:    best.Deck,
			Playing: best.Playing || best.Cue,
			BPM:     best.effectiveBPM(),
			Beat:    best.Beat,
		}
	}
	return clockInfo{Source: "manual"}
}
