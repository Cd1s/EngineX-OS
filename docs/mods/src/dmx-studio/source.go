package dmxstudio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// selectClockDeck picks the deck DMX Studio should follow (#209):
//  1. Playing MASTER / LEAD deck (local > peer > prolink)
//  2. Any playing deck (local > peer > prolink)
//  3. Paused MASTER / LEAD deck (holds the look)
//  4. nil (manual BPM fallback)
//
// Stale decks or decks with no valid tempo (effective BPM <= 0) are ignored.
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

func (f *Feature) fetchDeckSnapshots() ([]deckSnapshot, error) {
	decks, _, err := f.fetchDeckSnapshotsMode()
	return decks, err
}

func (f *Feature) fetchDeckSnapshotsMode() ([]deckSnapshot, bool, error) {
	f.mu.Lock()
	skipClockAPI := time.Now().Before(f.clockAPIAbsentUntil)
	f.mu.Unlock()
	if !skipClockAPI {
		var clockResp struct {
			ClockAPI bool           `json:"clockApi"`
			Now      int64          `json:"now"`
			Decks    []deckSnapshot `json:"decks"`
		}
		err := f.fetchJSON("/v1/sc5000/clock", &clockResp)
		if err == nil {
			if clockResp.ClockAPI {
				return clockResp.Decks, false, nil
			}
			// 5.3.0.4 gateway catch-all emptyPage answers 200 without clockApi and logs
			// every unknown /v1/ hit; cache its absence for 60 s to stop log spam.
			f.mu.Lock()
			f.clockAPIAbsentUntil = time.Now().Add(60 * time.Second)
			f.mu.Unlock()
		} else {
			var hs httpStatusError
			if errors.As(err, &hs) && (hs.status == "404" || hs.status == "501") {
				f.mu.Lock()
				f.clockAPIAbsentUntil = time.Now().Add(30 * time.Second)
				f.mu.Unlock()
			} else {
				return nil, false, err
			}
		}
	}

	// Fallback for 5.3.0.4 stream-sync builds that expose /peer/decks and /v1/sc5000/prolink
	// without /v1/sc5000/clock.
	var peerResp struct {
		Decks []deckSnapshot `json:"decks"`
	}
	var decks []deckSnapshot
	if err := f.fetchJSON("/peer/decks", &peerResp); err == nil {
		for _, d := range peerResp.Decks {
			d.Source = "local"
			d.SourceID = fmt.Sprintf("local:%d", d.Deck)
			if d.At == 0 {
				d.At = d.Sent
			}
			decks = append(decks, d)
		}
	}
	var prolinkResp struct {
		Devices []struct {
			Name         string  `json:"name"`
			Number       int     `json:"number"`
			BPM          float64 `json:"bpm"`
			Pitch        float64 `json:"pitch"`
			EffectiveBPM float64 `json:"effective_bpm"`
			BeatInBar    int     `json:"beat_in_bar"`
			BeatPhase    float64 `json:"beat_phase"`
			PlayState1   int     `json:"play_state_1"`
			Playing      bool    `json:"playing"`
			Master       bool    `json:"master"`
			LastSeen     string  `json:"last_seen"`
		} `json:"devices"`
	}
	if err := f.fetchJSON("/v1/sc5000/prolink", &prolinkResp); err == nil {
		now := time.Now()
		for _, dev := range prolinkResp.Devices {
			if dev.Number < 1 || dev.Number > 4 || strings.HasPrefix(dev.Name, "EngineX") {
				continue
			}
			seen, err := time.Parse(time.RFC3339Nano, dev.LastSeen)
			if err != nil || now.Sub(seen) >= 2*time.Second {
				continue
			}
			tempo := dev.EffectiveBPM
			if tempo <= 0 && dev.BPM > 0 {
				tempo = dev.BPM * (1 + dev.Pitch/100)
			}
			if tempo <= 0 {
				continue
			}
			beat := dev.BeatPhase
			if dev.BeatInBar >= 1 && dev.BeatInBar <= 4 {
				beat = float64(dev.BeatInBar-1) + dev.BeatPhase
			}
			// Pioneer CDJ PlayState1 0x07 is CUE Play (holding CUE); 0x06 is paused at CUE.
			cue := dev.PlayState1 == 7
			decks = append(decks, deckSnapshot{
				Deck:       dev.Number,
				Source:     "prolink",
				SourceID:   fmt.Sprintf("prolink:%d", dev.Number),
				Master:     dev.Master,
				Lead:       dev.Master,
				Playing:    dev.Playing || cue,
				Cue:        cue,
				BPM:        dev.BPM,
				CurrentBPM: tempo,
				Pitch:      dev.Pitch,
				Beat:       beat,
				At:         seen.UnixMilli(),
			})
		}
	}
	return decks, true, nil
}

// pollClockOnce fetches deck state from stream-sync and updates beatClock in memory.
// Returns the suggested delay before the next poll.
func (f *Feature) pollClockOnce() time.Duration {
	f.mu.Lock()
	follow, _ := f.Config["followDeck"].(bool)
	now := f.now()
	if !follow {
		if f.clock.deck != 0 && f.pollerActive {
			f.clock.toManual(now, f.Config["bpm"].(float64))
			f.pollerActive = false
		}
		f.mu.Unlock()
		return 250 * time.Millisecond
	}
	if now-f.lastExternalClock < clockStale {
		f.mu.Unlock()
		return 150 * time.Millisecond
	}
	webUI := f.webUIURL
	f.mu.Unlock()

	if webUI == "" {
		return 500 * time.Millisecond
	}

	decks, fallback, err := f.fetchDeckSnapshotsMode()
	if err != nil {
		return 500 * time.Millisecond
	}

	picked := selectClockDeck(decks)
	now = f.now()
	wall := wallMillis()

	f.mu.Lock()
	defer f.mu.Unlock()
	if follow, _ := f.Config["followDeck"].(bool); !follow || now-f.lastExternalClock < clockStale {
		return 250 * time.Millisecond
	}
	manual := f.Config["bpm"].(float64)
	if picked == nil {
		if f.pollerActive {
			f.clock.update(now, now, manual, clockSample{deck: 0})
			f.pollerActive = false
		}
		if fallback {
			return 500 * time.Millisecond
		}
		return 200 * time.Millisecond
	}
	t := now
	if picked.At > 0 {
		t = now - math.Min(maxLag, math.Max(0, (wall-float64(picked.At))/1000))
	}
	srcID := picked.SourceID
	if srcID == "" {
		srcID = fmt.Sprintf("%s:%d", picked.Source, picked.Deck)
	}
	f.clock.update(t, now, manual, clockSample{
		deck:         picked.Deck,
		sourceOrigin: picked.Source,
		sourceID:     srcID,
		playing:      picked.Playing || picked.Cue,
		beat:         picked.Beat,
		bpm:          picked.effectiveBPM(),
		position:     picked.PositionMS,
	})
	f.pollerActive = true
	if picked.Playing || picked.Cue {
		if fallback {
			return 250 * time.Millisecond
		}
		return 100 * time.Millisecond
	}
	if fallback {
		return 500 * time.Millisecond
	}
	return 200 * time.Millisecond
}

func (f *Feature) clockLoop() {
	defer close(f.clockDone)
	for {
		delay := f.pollClockOnce()
		timer := time.NewTimer(delay)
		select {
		case <-f.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
