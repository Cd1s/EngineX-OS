package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/stagelinq"
	"github.com/Cd1s/EngineX-OS/pkg/tracklist"
)

type tracklistRuntime struct {
	mgr       *tracklist.Manager
	stageLinQ *stagelinq.Monitor
	stopSLQ   context.CancelFunc
	mu        sync.Mutex
	stopObs   context.CancelFunc
	obsDone   chan struct{}
}

var (
	tracklistRuntimes sync.Map // map[*Server]*tracklistRuntime
	tracklistInitMu   sync.Mutex
)

func boolPtr(v bool) *bool { return &v }

func (s *Server) hasInternalMixer() bool {
	return boardCapabilities(imagePrefix(deviceCompatible())).Mixer
}

func (s *Server) tracklistState() *tracklistRuntime {
	if v, ok := tracklistRuntimes.Load(s); ok {
		return v.(*tracklistRuntime)
	}
	tracklistInitMu.Lock()
	defer tracklistInitMu.Unlock()
	if v, ok := tracklistRuntimes.Load(s); ok {
		return v.(*tracklistRuntime)
	}
	rt := &tracklistRuntime{
		mgr:       tracklist.NewManager(tracklist.Config{StateDir: s.cfg.StateDir}),
		stageLinQ: stagelinq.NewMonitor(),
	}
	if s.cfg.StateDir == "/data/sc5000-custom/state" && !s.hasInternalMixer() {
		ctx, cancel := context.WithCancel(context.Background())
		rt.stopSLQ = cancel
		go func() { _ = rt.stageLinQ.ListenAndServe(ctx, "") }()
	}
	tracklistRuntimes.Store(s, rt)
	return rt
}

func (s *Server) startStandaloneObserver(rt *tracklistRuntime) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.stopObs != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	rt.stopObs = cancel
	rt.obsDone = done
	go s.tracklistObserverLoop(ctx, done)
}

func (s *Server) stopStandaloneObserver(rt *tracklistRuntime) {
	rt.mu.Lock()
	cancel := rt.stopObs
	done := rt.obsDone
	rt.stopObs = nil
	rt.obsDone = nil
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *Server) closeTracklist() {
	if v, ok := tracklistRuntimes.LoadAndDelete(s); ok {
		rt := v.(*tracklistRuntime)
		s.stopStandaloneObserver(rt)
		if rt.stopSLQ != nil {
			rt.stopSLQ()
		}
	}
}

// TracklistManager exposes the server's tracklist.Manager for diagnostics and tests.
func (s *Server) TracklistManager() *tracklist.Manager {
	return s.tracklistState().mgr
}

// StageLinQMonitor exposes the server's external StageLinQ mixer monitor.
func (s *Server) StageLinQMonitor() *stagelinq.Monitor {
	return s.tracklistState().stageLinQ
}

func (s *Server) tracklistObserverLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.ObserveStandaloneTracklist(now)
		}
	}
}

func isStreamingRef(uri, path string) bool {
	for _, s := range []string{uri, path} {
		low := strings.ToLower(s)
		if strings.Contains(low, "/stream/") || strings.Contains(low, "tidal") ||
			strings.Contains(low, "deezer") || strings.Contains(low, "soundcloud") ||
			strings.Contains(low, "netease") || strings.Contains(low, "spotify") ||
			strings.Contains(low, "beatport") {
			return true
		}
	}
	return false
}

func localAndPeerPlayerNums(role string) (localPlayer, peerPlayer int) {
	switch role {
	case "24", "2", "34":
		return 2, 1
	default:
		return 1, 2
	}
}

// ObserveStandaloneTracklist evaluates this standalone player's own physical decks
// and its linked partner's physical decks (excluding shadow copies), applying
// StageLinQ X1800/X1850, Engine Connect ExternalMixerVolume, Pioneer DJM On-Air,
// or sustained play-duration fallback (#199).
func (s *Server) ObserveStandaloneTracklist(now time.Time) {
	rt := s.tracklistState()
	if _, active := rt.mgr.Active(); !active {
		return
	}

	s.uiMu.Lock()
	role := s.loadUI().FourDeckRole
	s.uiMu.Unlock()
	localP, peerP := localAndPeerPlayerNums(role)

	// Snapshot Pioneer DJM On-Air states if fresh (< 8s)
	djmOnAir := map[int]bool{}
	djmKnown := false
	for _, dev := range s.mergedDevices() {
		if !dev.LastSeen.IsZero() && now.Sub(dev.LastSeen) < 8*time.Second && dev.Number >= 1 && dev.Number <= 4 {
			if dev.PacketType != 0 {
				djmOnAir[int(dev.Number)] = dev.OnAir
				// Mark DJM known only when at least one device explicitly carries OnAir or a DJM mixer is on the link
				if dev.OnAir || strings.Contains(strings.ToUpper(dev.Name), "DJM") {
					djmKnown = true
				}
			}
		}
	}

	s.shadow.mu.Lock()
	ownDecks := make([]deckState, len(s.shadow.own))
	copy(ownDecks, s.shadow.own)
	var remoteDecks []deckState
	for _, pd := range s.shadow.peerDecks {
		if !now.IsZero() && !pd.at.IsZero() && now.Sub(pd.at) > 15*time.Second {
			continue
		}
		remoteDecks = append(remoteDecks, pd.deckState)
	}
	s.shadow.mu.Unlock()

	buildCand := func(playerNum int, d deckState) (tracklist.DeckCandidate, bool) {
		// Exclude held CUE previews and unloaded/untitled decks.
		title := strings.TrimSpace(d.Title)
		if title == "" {
			title = strings.TrimSpace(d.Name)
		}
		artist := strings.TrimSpace(d.Artist)
		realPlay := d.Playing && !d.Cue && !d.Loading && !d.Stale
		ch := d.Deck
		if ch < 1 || ch > 4 {
			ch = 1
		}
		bpm := d.CurrentBPM
		if bpm <= 0 {
			bpm = d.BPM
		}
		label := fmt.Sprintf("Player %d · Deck %d", playerNum, d.Deck)

		onAir, mixerKnown := rt.stageLinQ.ChannelOnAir(ch, now)
		if !mixerKnown && d.ExtVol != nil {
			mixerKnown = true
			onAir = *d.ExtVol > stagelinq.FaderAudibleThreshold
		}
		if !mixerKnown && djmKnown {
			if v, ok := djmOnAir[ch]; ok {
				mixerKnown = true
				onAir = v
			}
		}
		return tracklist.DeckCandidate{
			Player:      playerNum,
			Deck:        d.Deck,
			Channel:     ch,
			SourceLabel: label,
			Artist:      artist,
			Title:       title,
			BPM:         bpm,
			Key:         d.KeyName,
			Playing:     realPlay,
			OnAir:       onAir,
			MixerKnown:  mixerKnown,
			Streaming:   isStreamingRef(d.Net, d.Path),
		}, true
	}

	var cands []tracklist.DeckCandidate
	for _, d := range ownDecks {
		if c, ok := buildCand(localP, d); ok {
			cands = append(cands, c)
		}
	}
	for _, d := range remoteDecks {
		if c, ok := buildCand(peerP, d); ok {
			cands = append(cands, c)
		}
	}

	added, sess, changed := rt.mgr.ObserveCandidates(now, cands)
	if changed {
		last := added[len(added)-1]
		_ = s.broadcaster().UpdateMetadata(last.Artist, last.Title)
		go s.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "sync", Session: sess})
	}
}

func lanPeerRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func peerBaseURL(peerKey string) string {
	if strings.HasPrefix(peerKey, "http://") || strings.HasPrefix(peerKey, "https://") {
		return strings.TrimRight(peerKey, "/")
	}
	if _, _, err := net.SplitHostPort(peerKey); err == nil {
		return "http://" + peerKey
	}
	return "http://" + net.JoinHostPort(peerKey, "8080")
}

func (s *Server) pushTracklistToPeers(msg tracklist.PeerSyncMessage) {
	now := time.Now()
	s.shadow.mu.Lock()
	var targets []string
	for addr, seen := range s.shadow.peers {
		if now.Sub(seen) < 30*time.Second {
			targets = append(targets, peerBaseURL(addr))
		}
	}
	s.shadow.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return
	}
	client := s.cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	for _, base := range targets {
		req, err := http.NewRequest("POST", base+"/peer/tracklist", bytes.NewReader(raw))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
	}
}

func (s *Server) peerTracklistHandler(w http.ResponseWriter, r *http.Request) {
	if !lanPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	rt := s.tracklistState()
	if r.Method == http.MethodGet {
		sess, _ := rt.mgr.Latest()
		respond(w, tracklist.PeerSyncMessage{Action: "sync", Session: sess})
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var msg tracklist.PeerSyncMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&msg); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid peer tracklist payload"))
		return
	}
	merged := rt.mgr.ApplyPeerSync(msg)
	if !s.hasInternalMixer() {
		if merged.Active {
			s.startStandaloneObserver(rt)
		} else {
			s.stopStandaloneObserver(rt)
		}
	}
	if np := tracklist.NowPlayingLine(merged); np != "" {
		artist, title, ok := strings.Cut(np, " - ")
		if !ok {
			title, artist = np, ""
		}
		_ = s.broadcaster().UpdateMetadata(artist, title)
	}
	respond(w, map[string]any{"ok": true, "revision": merged.Revision})
}

func (s *Server) tracklistStatusPayload() map[string]any {
	rt := s.tracklistState()
	hasMixer := s.hasInternalMixer()
	mode := "tracklist_only"
	if hasMixer {
		mode = "stream_and_tracklist"
	}
	sess, hasSess := rt.mgr.Latest()
	activeSess, isActive := rt.mgr.Active()
	if isActive {
		sess, hasSess = activeSess, true
	}
	mx := rt.stageLinQ.Snapshot()
	s.uiMu.Lock()
	autoTL := s.loadUI().AutoTracklist
	s.uiMu.Unlock()

	var sessPtr *tracklist.Session
	nowPlaying := s.broadcaster().Status().CurrentTrack
	if hasSess {
		sessPtr = &sess
		if np := tracklist.NowPlayingLine(sess); np != "" {
			nowPlaying = np
		}
	}
	return map[string]any{
		"active":         isActive,
		"audioSupported": hasMixer,
		"mode":           mode,
		"autoTracklist":  autoTL,
		"minPlaySec":     int(rt.mgr.FallbackHold().Seconds()),
		"mixerConnected": mx.Connected,
		"mixerName":      mx.DeviceName,
		"nowPlaying":     nowPlaying,
		"session":        sessPtr,
		"sessions":       rt.mgr.Sessions(),
	}
}

func (s *Server) handleTracklist(w http.ResponseWriter, r *http.Request) {
	rt := s.tracklistState()
	if r.Method == http.MethodGet {
		respond(w, s.tracklistStatusPayload())
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	q := r.URL.Query()
	var (
		wantActive     *bool
		recorderActive *bool
		title          string
		minPlaySec     *int
	)
	if v := q.Get("active"); v != "" {
		if b, ok := parseBoolParam(v); ok {
			wantActive = &b
		}
	}
	if v := q.Get("recorderActive"); v != "" {
		if b, ok := parseBoolParam(v); ok {
			recorderActive = &b
		}
	}
	if v := q.Get("minPlaySec"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			minPlaySec = &n
		}
	}
	if r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
		if err != nil {
			fail(w, http.StatusBadRequest, errors.New("request body too large"))
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			var in struct {
				Active         *bool  `json:"active"`
				RecorderActive *bool  `json:"recorderActive"`
				Title          string `json:"title"`
				MinPlaySec     *int   `json:"minPlaySec"`
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&in); err != nil {
				fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
				return
			}
			if in.Active != nil {
				wantActive = in.Active
			}
			if in.RecorderActive != nil {
				recorderActive = in.RecorderActive
			}
			if in.Title != "" {
				title = in.Title
			}
			if in.MinPlaySec != nil {
				minPlaySec = in.MinPlaySec
			}
		}
	}
	if minPlaySec != nil {
		if *minPlaySec < 1 || *minPlaySec > 300 {
			fail(w, http.StatusBadRequest, errors.New("minPlaySec must be between 1 and 300"))
			return
		}
		rt.mgr.SetFallbackHold(time.Duration(*minPlaySec) * time.Second)
	}

	// Stock Engine recorder trigger (all-in-one consoles when autoTracklist is enabled)
	if recorderActive != nil {
		s.uiMu.Lock()
		autoOn := s.loadUI().AutoTracklist
		s.uiMu.Unlock()
		if autoOn {
			if *recorderActive {
				sess := rt.mgr.Start(time.Now(), title, "recorder", "")
				go s.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "sync", Session: sess})
			} else if !s.broadcaster().Status().Enabled {
				if done, ok := rt.mgr.Stop(time.Now()); ok {
					s.writeMediaSessionSidecars(done)
					go s.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "stop", Session: done})
				}
			}
		}
	}

	if wantActive != nil {
		mode := "tracklist_only"
		if s.hasInternalMixer() {
			mode = "stream_and_tracklist"
		}
		if *wantActive {
			sess := rt.mgr.Start(time.Now(), title, mode, "")
			if !s.hasInternalMixer() {
				s.startStandaloneObserver(rt)
				s.ObserveStandaloneTracklist(time.Now())
			}
			go s.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "sync", Session: sess})
		} else {
			s.stopStandaloneObserver(rt)
			if done, ok := rt.mgr.Stop(time.Now()); ok {
				s.writeMediaSessionSidecars(done)
				go s.pushTracklistToPeers(tracklist.PeerSyncMessage{Action: "stop", Session: done})
			}
		}
	}
	respond(w, s.tracklistStatusPayload())
}

// writeMediaSessionSidecars mirrors finalized tracklist sidecars into any writable
// mounted media's Sessions/ folder alongside stock Engine recordings (#199).
func (s *Server) writeMediaSessionSidecars(done tracklist.Session) {
	root := s.cfg.MediaRoot
	if root == "" {
		return // only mirror to external media when MediaRoot is configured or on-deck
	}
	entries, err := filepath.Glob(filepath.Join(root, "*", "Sessions"))
	if err != nil {
		return
	}
	for _, sessDir := range entries {
		_ = tracklist.WriteSidecars(done, sessDir, done.ID, done.AudioFile)
	}
}

func (s *Server) handleTracklistExport(w http.ResponseWriter, r *http.Request) {
	rt := s.tracklistState()
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	sess, ok := rt.mgr.SessionByID(id)
	if !ok {
		fail(w, http.StatusNotFound, errors.New("no set tracklist recorded yet"))
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "txt"
	}
	base := sess.ID
	if base == "" {
		base = "enginex-tracklist"
	}
	switch format {
	case "txt", "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".txt"))
		_, _ = w.Write([]byte(tracklist.FormatText(sess)))
	case "cue":
		w.Header().Set("Content-Type", "application/x-cue; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".cue"))
		_, _ = w.Write([]byte(tracklist.FormatCUE(sess, sess.AudioFile)))
	case "mixcloud":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".mixcloud.txt"))
		_, _ = w.Write([]byte(tracklist.FormatMixcloud(sess)))
	case "json":
		secs, form := tracklist.MixcloudSections(sess)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", base+".json"))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"session":          sess,
			"mixcloudSections": secs,
			"mixcloudForm":     form,
		})
	default:
		fail(w, http.StatusBadRequest, errors.New("unsupported format (use txt, cue, mixcloud, or json)"))
	}
}

// handleNowPlayingText serves GET /live/nowplaying.txt publicly for OBS overlays.
func (s *Server) handleNowPlayingText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	rt := s.tracklistState()
	if sess, ok := rt.mgr.Active(); ok {
		if np := tracklist.NowPlayingLine(sess); np != "" {
			_, _ = w.Write([]byte(np + "\n"))
			return
		}
	}
	if cur := strings.TrimSpace(s.broadcaster().Status().CurrentTrack); cur != "" {
		_, _ = w.Write([]byte(cur + "\n"))
		return
	}
	_, _ = w.Write([]byte("\n"))
}
