package webui

// Shadow decks (#49). In the 4-deck layout a player plays the decks of its
// role and shows the other two. Its QML polls GET /v1/sc5000/shadow and drives
// those decks (silent: the hook routes only the role's decks to the outputs)
// from whoever plays them: the other EngineX player, whose QML posts its own
// decks to POST /v1/sc5000/decks and which serves them on GET /peer/decks, or a
// CDJ on Pro DJ Link, by player number. Track audio is copied to this deck.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

const shadowBeaconPort = 50199 // EngineX players find each other here

// Where Engine's volumes live, and the beacon; New copies them, tests change them.
var shadowDataRoot, shadowMediaRoot, shadowBeaconOn = "/data", "/media", true

// deckState is one deck as a shadow deck follows it.
type deckState struct {
	Deck       int     `json:"deck"`
	Path       string  `json:"path,omitempty"` // a file on this deck
	Net        string  `json:"net,omitempty"`  // Engine's TrackNetworkPath, posted by the QML
	Key        string  `json:"key,omitempty"`  // changes with the track
	Ext        string  `json:"ext,omitempty"`
	Playing    bool    `json:"playing"`
	PositionMS float64 `json:"positionMs"`
	Pitch      float64 `json:"pitch"` // percent
	Master     bool    `json:"master,omitempty"`
	Sync       bool    `json:"sync,omitempty"` // a CDJ's BEAT SYNC: the shadow deck syncs in Engine
	BPM        float64 `json:"bpm,omitempty"`  // the track's
	Beat       float64 `json:"beat,omitempty"` // Engine's BeatNumber: beats into the bar, 0..4
	Dbg        string  `json:"dbg,omitempty"`  // shortcut: Engine's raw master state, until verified on hardware
}

type peerDeck struct {
	deckState
	ip string
	at time.Time
}

// precisePos is a CDJ-3000's playhead from its absolute position packet (0x0b,
// every 30 ms) and when it arrived; the status packet's playhead only moves on
// beats, and following that made the shadow deck jump (#49).
type precisePos struct {
	ms float64
	at time.Time
}

type shadowState struct {
	mu        sync.Mutex
	precise   map[byte]precisePos
	own       []deckState
	ownMaster bool
	ownAt     time.Time
	peers     map[string]time.Time // EngineX players heard on the beacon
	peerDecks map[int]peerDeck
	files     map[string]string // track key -> local copy
	busy      map[string]bool
	used      map[string]time.Time // when a shadow deck last wanted a copy
	polled    time.Time
	running   bool
	peerPort  int // tests only; 0 is 8080

	dataRoot, mediaRoot string
	beacon              bool
}

func newShadowState() *shadowState {
	return &shadowState{precise: map[byte]precisePos{}, peers: map[string]time.Time{}, peerDecks: map[int]peerDeck{}, files: map[string]string{}, busy: map[string]bool{}, used: map[string]time.Time{}, dataRoot: shadowDataRoot, mediaRoot: shadowMediaRoot, beacon: shadowBeaconOn}
}

func (s *Server) shadowDir() string { return filepath.Join(filepath.Dir(s.uiDir()), "shadow") }

// activeRole is the role engine-ui-guard applied ("13", "24", "12", "1", "2"),
// or "" without the 4-deck layout.
func (s *Server) activeRole(v uiSettings) string {
	if !v.FourDeckWaveform {
		return ""
	}
	b, _ := os.ReadFile(fourDeckRolePath)
	if role := strings.TrimSpace(string(b)); slices.Contains(uiFourDeckRoles[1:], role) {
		return role
	}
	return ""
}

func (s *Server) role() string {
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	return s.activeRole(v)
}

// netPathFile maps Engine's TrackNetworkPath ("net://<uuid>/<volume>/<path>")
// to the file on this deck: "(Unknown)" is /data, other volumes are /media/<volume>.
func netPathFile(np, dataRoot, mediaRoot string) string {
	rest, ok := strings.CutPrefix(np, "net://")
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return ""
	}
	base := filepath.Join(mediaRoot, parts[1])
	if parts[1] == "(Unknown)" {
		base = dataRoot
	}
	rel, ok := prolink.SafeRel(parts[2])
	if !ok || strings.Contains(parts[1], "..") {
		return ""
	}
	return filepath.Join(base, rel)
}

var shadowAudioExt = []string{".mp3", ".flac", ".wav", ".aif", ".aiff", ".m4a", ".mp4", ".aac", ".ogg", ".alac"}

// decksPost takes this deck's own decks from its QML (loopback only: /v1/).
func (s *Server) decksPost(w http.ResponseWriter, r *http.Request) {
	var decks []deckState
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&decks); err != nil {
		fail(w, 400, err)
		return
	}
	own := decks[:0]
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 {
			continue
		}
		d.Path = ""
		if f := netPathFile(d.Net, s.shadow.dataRoot, s.shadow.mediaRoot); f != "" && slices.Contains(shadowAudioExt, strings.ToLower(filepath.Ext(f))) {
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
				d.Path, d.Ext = f, strings.ToLower(filepath.Ext(f))
				d.Key = fmt.Sprintf("%x", fnvSum(fmt.Sprintf("%s %d %d", f, st.Size(), st.ModTime().UnixNano())))
			}
		}
		d.Net = ""
		own = append(own, d)
	}
	// Pro DJ Link players see these decks in our status: playing, tempo and
	// master. Taking master asks a CDJ that still holds it to hand it over.
	ex := prolink.ExportDeck{At: time.Now()}
	for _, d := range own {
		if d.Master || (d.Playing && !ex.Playing && !ex.Master) {
			ex.BPM, ex.Pitch, ex.Beat = d.BPM, d.Pitch, d.Beat
		}
		ex.Playing = ex.Playing || d.Playing
		ex.Master = ex.Master || d.Master
	}
	s.shadow.mu.Lock()
	s.shadow.own, s.shadow.ownAt = own, time.Now()
	claimed := ex.Master && !s.shadow.ownMaster
	s.shadow.ownMaster = ex.Master
	s.shadow.mu.Unlock()
	if s.prolinkExport != nil {
		s.prolinkExport.SetDeck(ex)
		if claimed {
			asked := map[string]bool{} // an all-in-one is one address for several players
			for _, dev := range s.mergedDevices() {
				if ip := net.ParseIP(dev.Address); dev.Master && ip != nil && !asked[dev.Address] && !strings.HasPrefix(dev.Name, "EngineX") && time.Since(dev.LastSeen) < 5*time.Second {
					asked[dev.Address] = true
					s.prolinkExport.RequestMaster(ip)
				}
			}
		}
	}
	s.shadowWake()
	respond(w, map[string]any{"ok": true})
}

func fnvSum(v string) uint64 { h := fnv.New64a(); h.Write([]byte(v)); return h.Sum64() }

// peerDecksGet serves this deck's own decks to the other EngineX player (LAN,
// read-only: deck state and the loaded track, nothing else).
func (s *Server) peerDecksGet(w http.ResponseWriter, r *http.Request) {
	s.shadow.mu.Lock()
	var out []deckState
	if time.Since(s.shadow.ownAt) < 2*time.Second {
		for _, d := range s.shadow.own {
			d.Path = ""
			if d.Playing {
				d.PositionMS += float64(time.Since(s.shadow.ownAt).Milliseconds()) * (1 + d.Pitch/100)
			}
			out = append(out, d)
		}
	}
	s.shadow.mu.Unlock()
	respond(w, map[string]any{"decks": out})
}

func (s *Server) peerAudioGet(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	s.shadow.mu.Lock()
	var path string
	for _, d := range s.shadow.own {
		if d.Deck == n && d.Key != "" && d.Key == r.URL.Query().Get("key") {
			path = d.Path
		}
	}
	s.shadow.mu.Unlock()
	if path == "" {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "gone", http.StatusConflict)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// observe keeps the absolute positions of Pro DJ Link players.
func (sh *shadowState) observe(d prolink.Device) {
	if d.PacketType == 0x0b && d.PrecisePosition {
		sh.mu.Lock()
		sh.precise[d.Number] = precisePos{float64(d.PlayPositionMS), time.Now()}
		sh.mu.Unlock()
	}
}

// shadowGet is what the QML applies to the decks this player does not play.
func (s *Server) shadowGet(w http.ResponseWriter, r *http.Request) {
	role := s.role()
	out := []deckState{}
	if role != "" {
		s.shadowWake()
		out = s.shadowCompose(role, time.Now())
	}
	respond(w, map[string]any{"role": role, "decks": out})
}

func (s *Server) shadowCompose(role string, now time.Time) []deckState {
	devices := s.mergedDevices()
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	wanted := sh.used
	var out []deckState
	var free []int // shadow decks the other EngineX player does not fill
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		p, ok := sh.peerDecks[deck]
		if !ok || now.Sub(p.at) >= 2*time.Second {
			free = append(free, deck)
			continue
		}
		d := p.deckState
		d.Master = false // the two Engines share master natively (Engine Link)
		if d.Playing {
			d.PositionMS += float64(now.Sub(p.at).Milliseconds()) * (1 + d.Pitch/100)
		}
		if d.Key != "" {
			key := "peer:" + p.ip + ":" + d.Key
			wanted[key] = now
			d.Path = sh.files[key]
			if d.Path == "" {
				s.shadowFetchPeer(key, p)
			}
		}
		d.Key, d.Ext = "", ""
		out = append(out, d)
	}
	for deck, dev := range cdjDecks(devices, free, now) {
		d := deckState{Deck: deck, Playing: dev.Playing, PositionMS: float64(dev.PlayPositionMS), Pitch: dev.Pitch, Master: dev.Master, Sync: dev.Sync, BPM: dev.BPM}
		at := dev.LastSeen
		if p, ok := sh.precise[dev.Number]; ok && now.Sub(p.at) < time.Second {
			d.PositionMS, at = p.ms, p.at
		}
		if d.Playing {
			d.PositionMS += float64(now.Sub(at).Milliseconds()) * (1 + d.Pitch/100)
		}
		if dev.TrackID != 0 {
			if host := s.sourceHost(devices, dev.SourcePlayer); host != nil {
				key := fmt.Sprintf("cdj:%s:%d:%d", host, dev.SourceSlot, dev.TrackID)
				wanted[key] = now
				d.Path = sh.files[key]
				if d.Path == "" {
					s.shadowFetchCDJ(key, host, dev.SourceSlot, dev.TrackID)
				}
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	return out
}

// cdjDecks puts the CDJs on the free shadow decks: on their own player number
// when it is free, the rest on what is left, Deck 2 first. A lone CDJ next to
// a 1+3 player then shows beside Deck 1 also in the 2-waveform layout.
func cdjDecks(devices []prolink.Device, free []int, now time.Time) map[int]prolink.Device {
	// A CDJ that changed its number keeps its old entry for a few seconds, which
	// became a second (master) CDJ. An all-in-one (XDJ-XZ, XDJ-AZ) is several
	// players on one address, all sending every 200 ms or so. So keep only the
	// numbers an address sent from within 1.5 s of its newest packet.
	var live []prolink.Device
	newest := map[string]time.Time{}
	for _, dev := range devices {
		if dev.Number >= 1 && dev.Number <= 6 && !strings.HasPrefix(dev.Name, "EngineX") && now.Sub(dev.LastSeen) <= 5*time.Second {
			live = append(live, dev)
			if dev.LastSeen.After(newest[dev.Address]) {
				newest[dev.Address] = dev.LastSeen
			}
		}
	}
	var cdjs []prolink.Device
	for _, dev := range live {
		if newest[dev.Address].Sub(dev.LastSeen) <= 1500*time.Millisecond {
			cdjs = append(cdjs, dev)
		}
	}
	slices.SortFunc(cdjs, func(a, b prolink.Device) int { return int(a.Number) - int(b.Number) })
	out := map[int]prolink.Device{}
	var rest []prolink.Device
	for _, dev := range cdjs {
		if deck := int(dev.Number); slices.Contains(free, deck) {
			out[deck] = dev
		} else {
			rest = append(rest, dev)
		}
	}
	for _, deck := range []int{2, 4, 1, 3} {
		if _, taken := out[deck]; len(rest) > 0 && !taken && slices.Contains(free, deck) {
			out[deck], rest = rest[0], rest[1:]
		}
	}
	return out
}

// sourceHost is the player whose media holds a CDJ's track: another player,
// or this deck's own export (#63).
func (s *Server) sourceHost(devices []prolink.Device, n byte) net.IP {
	if s.prolinkExport != nil {
		if st := s.prolinkExport.Status(); st.Enabled && st.Number == n {
			if host, _, err := net.SplitHostPort(st.DBAddress); err == nil {
				return net.ParseIP(host)
			}
		}
	}
	for _, d := range devices {
		if d.Number == n {
			return net.ParseIP(d.Address)
		}
	}
	return nil
}

// shadowFetch* run with sh.mu held; the copy itself runs unlocked.
func (s *Server) shadowFetchCDJ(key string, host net.IP, slot byte, id uint32) {
	s.shadowFetch(key, func(ctx context.Context) (string, error) {
		return prolink.FetchTrack(ctx, &prolink.NFSClient{IP: host}, slot, id, s.shadowDir())
	})
}

func (s *Server) shadowFetchPeer(key string, p peerDeck) {
	port := s.shadow.peerPort
	if port == 0 {
		port = 8080
	}
	url := fmt.Sprintf("http://%s/peer/decks/%d/audio?key=%s", net.JoinHostPort(p.ip, strconv.Itoa(port)), p.Deck, p.Key)
	ext := ".mp3"
	if slices.Contains(shadowAudioExt, p.Ext) {
		ext = p.Ext
	}
	local := filepath.Join(s.shadowDir(), fmt.Sprintf("peer-%x%s", fnvSum(key), ext))
	s.shadowFetch(key, func(ctx context.Context) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("peer audio: %s", resp.Status)
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, err = io.Copy(f, io.LimitReader(resp.Body, 1<<30))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		return local, os.Rename(f.Name(), local)
	})
}

func (s *Server) shadowFetch(key string, fetch func(context.Context) (string, error)) {
	sh := s.shadow
	if sh.busy[key] {
		return
	}
	sh.busy[key] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		path, err := fetch(ctx)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		delete(sh.busy, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream-sync: shadow deck copy %s: %v\n", key, err)
			return
		}
		sh.files[key] = path
		// Keep copies a shadow deck wanted in the last 2 minutes: deleting the
		// previous track at once failed Engine's load when the CDJ switched fast.
		for k, f := range sh.files {
			if k != key && time.Since(sh.used[k]) > 2*time.Minute {
				os.Remove(f)
				delete(sh.files, k)
				delete(sh.used, k)
			}
		}
	}()
}

// shadowWake starts the beacon and peer polling while a shadow deck is in use;
// they stop 10 s after the last poll.
func (s *Server) shadowWake() {
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.polled = time.Now()
	if sh.running {
		return
	}
	sh.running = true
	go s.shadowLoop()
}

func (s *Server) shadowLoop() {
	sh := s.shadow
	var conn *net.UDPConn
	if sh.beacon {
		conn = shadowBeacon()
	}
	if conn != nil {
		defer conn.Close()
		go func() {
			b := make([]byte, 64)
			for {
				n, addr, err := conn.ReadFromUDP(b)
				if err != nil {
					return
				}
				if string(b[:n]) == "EngineX-shadow 1" && !isLocalIP(addr.IP) {
					sh.mu.Lock()
					sh.peers[addr.IP.String()] = time.Now()
					sh.mu.Unlock()
				}
			}
		}()
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.closed:
			sh.mu.Lock()
			sh.running = false
			sh.mu.Unlock()
			return
		case <-tick.C:
		}
		sh.mu.Lock()
		if time.Since(sh.polled) > 10*time.Second {
			sh.running = false
			sh.mu.Unlock()
			return
		}
		sh.mu.Unlock()
		if conn != nil && i%8 == 0 {
			// shortcut: limited broadcast leaves on the default route only; enough for one DJ LAN.
			conn.WriteToUDP([]byte("EngineX-shadow 1"), &net.UDPAddr{IP: net.IPv4bcast, Port: shadowBeaconPort})
		}
		s.shadowPollPeers()
	}
}

func shadowBeacon() *net.UDPConn {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":"+strconv.Itoa(shadowBeaconPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "stream-sync: shadow deck beacon: %v\n", err)
		return nil
	}
	return pc.(*net.UDPConn)
}

func isLocalIP(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return ip.IsLoopback()
}

var peerClient = &http.Client{Timeout: 400 * time.Millisecond}

// shadowPollPeers reads the other EngineX players' decks.
func (s *Server) shadowPollPeers() {
	sh := s.shadow
	sh.mu.Lock()
	var ips []string
	for ip, seen := range sh.peers {
		if time.Since(seen) < 6*time.Second {
			ips = append(ips, ip)
		}
	}
	port := sh.peerPort
	sh.mu.Unlock()
	if port == 0 {
		port = 8080
	}
	for _, ip := range ips {
		resp, err := peerClient.Get("http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/peer/decks")
		if err != nil {
			continue
		}
		var body struct{ Decks []deckState }
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		now := time.Now()
		sh.mu.Lock()
		for _, d := range body.Decks {
			if d.Deck >= 1 && d.Deck <= 4 {
				sh.peerDecks[d.Deck] = peerDeck{deckState: d, ip: ip, at: now}
			}
		}
		sh.mu.Unlock()
	}
}
