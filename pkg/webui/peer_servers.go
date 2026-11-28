package webui

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const (
	peerWebDAVScopePrefix = "linkwebdav:"
	peerNASScopePrefix    = "linknas:"
	peerWebDAVProvider    = "peer-webdav"
	peerSMBProvider       = "peer-smb"

	deckShareWhoMagic          = "EngineX-share-who 1"
	deckShareHereMagic         = "EngineX-share-here 1"
	deckSharePeerTTL           = 60 * time.Second
	deckShareProbeCooldown     = 10 * time.Second
	peerServersRefreshCooldown = 5 * time.Second
	peerServersEntryTTL        = 20 * time.Second
	peerTrackMaxBytes          = 1 << 30 // 1 GiB
	peerTrackMaxSlots          = 4
)

// Var so tests can override port/beacon behavior cleanly.
var (
	deckShareBeaconPort = 50197
	deckShareBeaconOn   = !isTestingBinary()
)

func isTestingBinary() bool {
	return strings.HasSuffix(os.Args[0], ".test")
}

// peerServerMeta is a WebDAV/NAS server summary exposed over /peer/servers.
// Credentials and server URLs never leave the source deck.
type peerServerMeta struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	TargetHash string `json:"targetHash"`
	Slot       bool   `json:"slot,omitempty"`
}

// linkedServerView is one peer's server exposed to B's uiStatus/Sources.
type linkedServerView struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Peer  string `json:"peer"`
}

type peerServerSnapshot struct {
	IP        string
	PeerKey   string
	Model     string
	FetchedAt time.Time
	Servers   []peerServerMeta
}

type peerServersState struct {
	mu           sync.Mutex
	peers        map[string]peerServerSnapshot // keyed by peerKey
	localTargets map[string]bool               // cached local enabled TargetHashes (avoid uiMu in omniSearch)
	lastRefresh  time.Time
	refreshing   bool
	dirty        atomic.Bool
}

var peerStreamHTTP = &http.Client{
	CheckRedirect: noRedirect,
}

func peerKeyOf(ip string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ip)))
	return hex.EncodeToString(sum[:4])
}

func isHexString(s string, wantLen int) bool {
	if len(s) != wantLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func peerProviderForKind(k serverKind) string {
	if k == kindNAS {
		return peerSMBProvider
	}
	return peerWebDAVProvider
}

func kindOfPeerProvider(p string) (serverKind, bool) {
	switch p {
	case peerWebDAVProvider:
		return kindWebDAV, true
	case peerSMBProvider:
		return kindNAS, true
	}
	return serverKind{}, false
}

func isPeerServerProvider(p string) bool {
	_, ok := kindOfPeerProvider(p)
	return ok
}

func peerServerScope(k serverKind, peerKey, serverID string) string {
	if k == kindNAS {
		return peerNASScopePrefix + peerKey + ":" + serverID
	}
	return peerWebDAVScopePrefix + peerKey + ":" + serverID
}

func parsePeerServerScope(scope string) (k serverKind, peerKey, serverID string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(scope, peerWebDAVScopePrefix):
		k = kindWebDAV
		rest = strings.TrimPrefix(scope, peerWebDAVScopePrefix)
	case strings.HasPrefix(scope, peerNASScopePrefix):
		k = kindNAS
		rest = strings.TrimPrefix(scope, peerNASScopePrefix)
	default:
		return serverKind{}, "", "", false
	}
	peerKey, serverID, ok = strings.Cut(rest, ":")
	if !ok || !isHexString(peerKey, 8) || !isRootID(serverID) {
		return serverKind{}, "", "", false
	}
	return k, peerKey, serverID, true
}

func isPeerServerScope(scope string) bool {
	_, _, _, ok := parsePeerServerScope(scope)
	return ok
}

func peerServerScopedID(peerKey, serverID, raw string) string {
	return peerKey + ":" + serverID + ":" + raw
}

func splitPeerServerScopedID(id string) (peerKey, serverID, raw string, ok bool) {
	peerKey, rest, ok := strings.Cut(id, ":")
	if !ok || !isHexString(peerKey, 8) {
		return "", "", "", false
	}
	serverID, raw, ok = strings.Cut(rest, ":")
	if !ok || !isRootID(serverID) {
		return "", "", "", false
	}
	return peerKey, serverID, raw, true
}

// ---- passive LAN peer discovery (UDP 50197) ----

func (s *Server) startDeckShareBeacon() {
	if !deckShareBeaconOn {
		return
	}
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
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":"+strconv.Itoa(deckShareBeaconPort))
	if err != nil {
		log.Printf("stream-sync: deck-share beacon: %v", err)
		return
	}
	conn := pc.(*net.UDPConn)
	s.shareMu.Lock()
	s.shareBeaconConn = conn
	s.shareMu.Unlock()

	s.lanProbeWG.Add(1)
	go func() {
		defer s.lanProbeWG.Done()
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if addr == nil || addr.IP == nil || isLocalIP(addr.IP) {
				continue
			}
			s.handleDeckShareUDP(conn, buf[:n], addr, time.Now())
		}
	}()
	go s.broadcastDeckShareHere()
}

func (s *Server) closeDeckShareBeacon() {
	s.shareMu.Lock()
	conn := s.shareBeaconConn
	s.shareBeaconConn = nil
	s.shareMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *Server) handleDeckShareUDP(conn *net.UDPConn, data []byte, addr *net.UDPAddr, now time.Time) {
	msg := strings.TrimSpace(string(data))
	ip := addr.IP.String()
	switch {
	case msg == deckShareWhoMagic:
		s.noteSharePeer(ip, now)
		reply := []byte(deckShareHereMagic + "\n" + s.board)
		_, _ = conn.WriteToUDP(reply, addr)
	case msg == deckShareHereMagic || strings.HasPrefix(msg, deckShareHereMagic+"\n"):
		s.noteSharePeer(ip, now)
		s.peerServers.dirty.Store(true)
	}
}

func (s *Server) noteSharePeer(ip string, now time.Time) {
	if net.ParseIP(ip) == nil {
		return
	}
	s.shareMu.Lock()
	if s.sharePeersMap == nil {
		s.sharePeersMap = map[string]time.Time{}
	}
	s.sharePeersMap[ip] = now
	s.shareMu.Unlock()
}

func (s *Server) notifySharePeers() {
	s.shareMu.Lock()
	conn := s.shareBeaconConn
	s.shareMu.Unlock()
	if conn == nil {
		return
	}
	select {
	case <-s.closed:
		return
	default:
	}
	go s.broadcastDeckShareHere()
}

func (s *Server) broadcastDeckShareHere() {
	s.shareMu.Lock()
	conn := s.shareBeaconConn
	s.shareMu.Unlock()
	if conn == nil {
		return
	}
	shadowBroadcast(conn, []byte(deckShareHereMagic+"\n"+s.board), deckShareBeaconPort)
}

func (s *Server) probeDeckSharePeers() {
	now := time.Now()
	s.shareMu.Lock()
	if now.Sub(s.shareLastProbe) < deckShareProbeCooldown {
		s.shareMu.Unlock()
		return
	}
	s.shareLastProbe = now
	conn := s.shareBeaconConn
	s.shareMu.Unlock()

	if conn != nil {
		go shadowBroadcast(conn, []byte(deckShareWhoMagic), deckShareBeaconPort)
	}
	if isDeckStateDir(s.cfg.StateDir) || s.rbLinkARPCandidates != nil || s.rbLinkPeerCandidates != nil {
		s.probeLANNeighborsAsync()
	}
}

func (s *Server) probeLANNeighborsAsync() {
	select {
	case <-s.closed:
		return
	default:
	}
	s.shareMu.Lock()
	if s.shareNeighborProbing {
		s.shareMu.Unlock()
		return
	}
	s.shareNeighborProbing = true
	s.shareMu.Unlock()

	s.lanProbeWG.Add(1)
	go func() {
		defer s.lanProbeWG.Done()
		defer func() {
			s.shareMu.Lock()
			s.shareNeighborProbing = false
			s.shareMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		seen := map[string]bool{}
		for _, ip := range s.discoverEngineLinkTCPPeers(ctx) {
			seen[ip] = true
		}
		for _, ip := range s.discoverARPLANPeers(ctx) {
			seen[ip] = true
		}
		for ip := range seen {
			if ctx.Err() != nil {
				return
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sharePeerURL(ip, "/peer/servers"), nil)
			if err != nil {
				continue
			}
			c := *copyPeerHTTP
			c.Timeout = 350 * time.Millisecond
			resp, err := c.Do(req)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				s.noteSharePeer(ip, time.Now())
				s.peerServers.dirty.Store(true)
			}
		}
	}()
}

func (s *Server) sharePeerURL(ip, p string) string {
	port := 0
	s.shareMu.Lock()
	if s.sharePeerPorts != nil {
		port = s.sharePeerPorts[ip]
	}
	s.shareMu.Unlock()
	if port == 0 && s.shadow != nil {
		port = s.shadow.peerPort
	}
	if port == 0 {
		port = 8080
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + p
}

func (s *Server) sharePeers() []string {
	seen := map[string]bool{}
	for _, ip := range s.copyPeers() {
		seen[ip] = true
	}
	now := time.Now()
	s.shareMu.Lock()
	for ip, t := range s.sharePeersMap {
		if now.Sub(t) < deckSharePeerTTL && net.ParseIP(ip) != nil {
			seen[ip] = true
		} else {
			delete(s.sharePeersMap, ip)
		}
	}
	s.shareMu.Unlock()
	peers := make([]string, 0, len(seen))
	for ip := range seen {
		peers = append(peers, ip)
	}
	sort.Strings(peers)
	return peers
}

func (s *Server) isKnownSharePeer(ip string) bool {
	if ip == "" {
		return false
	}
	return slices.Contains(s.sharePeers(), ip)
}

// ---- Source side (Deck A): /peer/servers* ----

func (s *Server) peerServerEntryByID(k serverKind, id string) (serverEntry, bool) {
	if !isRootID(id) {
		return serverEntry{}, false
	}
	for _, e := range s.servers(k) {
		if e.ID == id {
			return e, true
		}
	}
	return serverEntry{}, false
}

func (s *Server) handlePeerServersList(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	var list []peerServerMeta
	for _, k := range serverKinds {
		for _, e := range s.servers(k) {
			list = append(list, peerServerMeta{
				ID:         e.ID,
				Kind:       k.scope,
				Name:       e.Name,
				TargetHash: serverRoot(k, e.cfg.URL),
				Slot:       e.slot,
			})
		}
	}
	if list == nil {
		list = []peerServerMeta{}
	}
	respond(w, map[string]any{"model": s.board, "servers": list})
}

func (s *Server) handlePeerServerPlaylists(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	k, ok := kindOf(r.PathValue("kind"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown kind of server"))
		return
	}
	e, found := s.peerServerEntryByID(k, r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), serverListTimeout)
	defer cancel()
	lists, err := s.serverProvider(e).Playlists(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if lists == nil {
		lists = []providers.Playlist{}
	}
	respond(w, map[string]any{"playlists": lists})
}

func stripPeerTrackCredentials(tracks []providers.Track, serverName string) []providers.Track {
	out := make([]providers.Track, len(tracks))
	for i, t := range tracks {
		t.CoverURL = ""
		t.ThumbURL = ""
		t.Album = serverName
		out[i] = t
	}
	return out
}

func (s *Server) handlePeerServerTracks(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	k, ok := kindOf(r.PathValue("kind"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown kind of server"))
		return
	}
	e, found := s.peerServerEntryByID(k, r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	playlist := r.URL.Query().Get("playlist")
	if playlist == "" {
		fail(w, http.StatusBadRequest, errors.New("playlist required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tracks, err := s.serverProvider(e).PlaylistTracks(ctx, playlist)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if len(tracks) > maxPlaylistTracks {
		fail(w, http.StatusBadRequest, errors.New("playlist too large"))
		return
	}
	respond(w, map[string]any{"tracks": stripPeerTrackCredentials(tracks, e.Name)})
}

func (s *Server) handlePeerServerSearch(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	k, ok := kindOf(r.PathValue("kind"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown kind of server"))
		return
	}
	e, found := s.peerServerEntryByID(k, r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		fail(w, http.StatusBadRequest, errors.New("query required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tracks, err := s.serverProvider(e).Search(ctx, q)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	respond(w, map[string]any{"tracks": stripPeerTrackCredentials(tracks, e.Name)})
}

func (s *Server) acquirePeerTrackSlot(ctx context.Context) (func(), error) {
	s.shareMu.Lock()
	if s.peerTrackSem == nil {
		s.peerTrackSem = make(chan struct{}, peerTrackMaxSlots)
	}
	sem := s.peerTrackSem
	s.shareMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errors.New("too many peer track transfers")
	}
}

func safePeerTrackID(id string) bool {
	if id == "" || strings.ContainsRune(id, '\x00') || strings.ContainsRune(id, '\\') {
		return false
	}
	for _, seg := range strings.Split(id, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

func peerServeExtAndMIME(filePath string) (ext, mimeType string) {
	ext = strings.ToLower(filepath.Ext(filePath))
	if ext == ".m4a" || ext == ".aac" {
		ext = ".mp4"
	}
	switch ext {
	case ".flac":
		return ".flac", "audio/flac"
	case ".mp4":
		return ".mp4", "audio/mp4"
	case ".ogg":
		return ".ogg", "audio/ogg"
	case ".wav":
		return ".wav", "audio/wav"
	case ".aif", ".aiff", ".aifc":
		return ext, "audio/aiff"
	default:
		return ".mp3", "audio/mpeg"
	}
}

func (s *Server) handlePeerServerTrack(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	k, ok := kindOf(r.PathValue("kind"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown kind of server"))
		return
	}
	e, found := s.peerServerEntryByID(k, r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	rawID := r.URL.Query().Get("id")
	if !safePeerTrackID(rawID) {
		fail(w, http.StatusBadRequest, errors.New("invalid track id"))
		return
	}
	release, err := s.acquirePeerTrackSlot(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	defer release()

	http.NewResponseController(w).SetWriteDeadline(time.Time{})

	dlDir := ""
	tempDir := ""
	if s.jobs != nil && s.jobs.p.Deemix != nil && s.jobs.p.Deemix.DownloadDir != "" {
		dlDir = s.jobs.p.Deemix.DownloadDir
	} else {
		var merr error
		tempDir, merr = os.MkdirTemp("", "enginex-peer-track-*")
		if merr != nil {
			fail(w, http.StatusInternalServerError, merr)
			return
		}
		defer os.RemoveAll(tempDir)
		dlDir = tempDir
	}

	p, err := s.serverProvider(e).DownloadTrack(r.Context(), rawID, dlDir)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if tempDir == "" && s.jobs != nil && s.jobs.p.Deemix != nil {
		s.jobs.p.Deemix.Trim(p)
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > peerTrackMaxBytes {
		fail(w, http.StatusBadGateway, errors.New("peer track file unavailable"))
		return
	}
	ext, mimeType := peerServeExtAndMIME(p)
	w.Header().Set("X-Enginex-Ext", ext)
	w.Header().Set("Content-Type", mimeType)
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
}

// ---- Receiver side (Deck B): discovery cache, Gateway & Jobs ----

func (s *Server) localEnabledTargetHashesLocked(v uiSettings, f nasFile) map[string]bool {
	out := map[string]bool{}
	for _, k := range serverKinds {
		if !serversOn(k, v, f) {
			continue
		}
		for _, e := range serverEntries(k, f) {
			if nasConfigReady(e.cfg) {
				out[serverRoot(k, e.cfg.URL)] = true
			}
		}
	}
	return out
}

func (s *Server) setPeerLocalTargets(targets map[string]bool) {
	s.peerServers.mu.Lock()
	s.peerServers.localTargets = targets
	s.peerServers.mu.Unlock()
}

func (s *Server) collectLinkedServersLocked(localTargets map[string]bool, now time.Time) []linkedServerView {
	if s.peerServers.peers == nil {
		s.peerServers.peers = map[string]peerServerSnapshot{}
	}
	if localTargets != nil {
		s.peerServers.localTargets = localTargets
	} else {
		localTargets = s.peerServers.localTargets
	}
	var out []linkedServerView
	seenTargets := map[string]bool{}
	for k, v := range localTargets {
		seenTargets[k] = v
	}
	keys := make([]string, 0, len(s.peerServers.peers))
	for pk, snap := range s.peerServers.peers {
		if now.Sub(snap.FetchedAt) > peerServersEntryTTL {
			delete(s.peerServers.peers, pk)
			continue
		}
		keys = append(keys, pk)
	}
	sort.Strings(keys)
	for _, pk := range keys {
		snap := s.peerServers.peers[pk]
		for _, srv := range snap.Servers {
			k, ok := kindOf(srv.Kind)
			if !ok || !isRootID(srv.ID) {
				continue
			}
			if srv.TargetHash != "" && seenTargets[srv.TargetHash] {
				continue
			}
			if srv.TargetHash != "" {
				seenTargets[srv.TargetHash] = true
			}
			out = append(out, linkedServerView{
				ID:    srv.ID,
				Kind:  k.scope,
				Name:  srv.Name,
				Scope: peerServerScope(k, snap.PeerKey, srv.ID),
				Peer:  snap.IP,
			})
		}
	}
	return out
}

func (s *Server) cachedLinkedServerViews() []linkedServerView {
	s.peerServers.mu.Lock()
	defer s.peerServers.mu.Unlock()
	if len(s.peerServers.peers) == 0 {
		return nil
	}
	return s.collectLinkedServersLocked(nil, time.Now())
}

func (s *Server) linkedServerViews(localTargets map[string]bool) []linkedServerView {
	s.probeDeckSharePeers()
	now := time.Now()
	s.peerServers.mu.Lock()
	if s.peerServers.peers == nil {
		s.peerServers.peers = map[string]peerServerSnapshot{}
	}
	stale := s.peerServers.dirty.Swap(false) || now.Sub(s.peerServers.lastRefresh) >= peerServersRefreshCooldown
	if !stale && len(s.peerServers.peers) == 0 && len(s.sharePeers()) > 0 {
		stale = true
	}
	out := s.collectLinkedServersLocked(localTargets, now)
	s.peerServers.mu.Unlock()
	if stale {
		s.triggerPeerServersRefresh()
	}
	return out
}

func (s *Server) triggerPeerServersRefresh() {
	select {
	case <-s.closed:
		return
	default:
	}
	s.peerServers.mu.Lock()
	if s.peerServers.refreshing {
		s.peerServers.mu.Unlock()
		return
	}
	s.peerServers.refreshing = true
	s.peerServers.mu.Unlock()

	s.lanProbeWG.Add(1)
	go func() {
		defer s.lanProbeWG.Done()
		defer func() {
			s.peerServers.mu.Lock()
			s.peerServers.refreshing = false
			s.peerServers.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s.refreshPeerServers(ctx)
	}()
}

// refreshPeerServers queries every known LAN peer's /peer/servers synchronously.
func (s *Server) refreshPeerServers(ctx context.Context) {
	peers := s.sharePeers()
	type result struct {
		pk   string
		snap peerServerSnapshot
		ok   bool
	}
	results := make([]result, len(peers))
	var wg sync.WaitGroup
	for i, ip := range peers {
		wg.Go(func() {
			pk := peerKeyOf(ip)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sharePeerURL(ip, "/peer/servers"), nil)
			if err != nil {
				return
			}
			c := *copyPeerHTTP
			c.Timeout = 1500 * time.Millisecond
			resp, err := c.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				return
			}
			var wire struct {
				Model   string           `json:"model"`
				Servers []peerServerMeta `json:"servers"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wire); err != nil {
				return
			}
			results[i] = result{
				pk: pk,
				ok: true,
				snap: peerServerSnapshot{
					IP:        ip,
					PeerKey:   pk,
					Model:     wire.Model,
					FetchedAt: time.Now(),
					Servers:   wire.Servers,
				},
			}
		})
	}
	wg.Wait()

	now := time.Now()
	livePeers := map[string]bool{}
	for _, ip := range peers {
		livePeers[peerKeyOf(ip)] = true
	}
	s.peerServers.mu.Lock()
	if s.peerServers.peers == nil {
		s.peerServers.peers = map[string]peerServerSnapshot{}
	}
	for pk := range s.peerServers.peers {
		if !livePeers[pk] {
			delete(s.peerServers.peers, pk)
		}
	}
	for _, r := range results {
		if r.ok {
			s.peerServers.peers[r.pk] = r.snap
		}
	}
	s.peerServers.lastRefresh = now
	s.peerServers.mu.Unlock()
}

func (s *Server) peerServerEndpoint(k serverKind, peerKey, serverID string) (ip, name string, ok bool) {
	s.peerServers.mu.Lock()
	defer s.peerServers.mu.Unlock()
	snap, exists := s.peerServers.peers[peerKey]
	if !exists || time.Since(snap.FetchedAt) > peerServersEntryTTL {
		return "", "", false
	}
	for _, srv := range snap.Servers {
		if srv.Kind == k.scope && srv.ID == serverID {
			return snap.IP, srv.Name, true
		}
	}
	return "", "", false
}

func (s *Server) peerTrackLive(t providers.Track) bool {
	k, ok := kindOfPeerProvider(t.Provider)
	if !ok {
		return true
	}
	peerKey, serverID, _, ok := splitPeerServerScopedID(t.ID)
	if !ok {
		return false
	}
	_, _, alive := s.peerServerEndpoint(k, peerKey, serverID)
	return alive
}

func (s *Server) peerServerGetJSON(ctx context.Context, ip, pathAndQuery string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sharePeerURL(ip, pathAndQuery), nil)
	if err != nil {
		return http.StatusBadRequest, err
	}
	resp, err := copyPeerHTTP.Do(req)
	if err != nil {
		return http.StatusBadGateway, errors.New("could not reach peer deck")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		if e.Error != "" {
			return resp.StatusCode, errors.New(e.Error)
		}
		return resp.StatusCode, fmt.Errorf("peer returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return http.StatusBadGateway, errors.New("invalid response from peer deck")
	}
	return http.StatusOK, nil
}

func (g *tidalGateway) peerServerPlaylistsPage(w http.ResponseWriter, r *http.Request, scope string) {
	k, peerKey, serverID, ok := parsePeerServerScope(scope)
	if !ok {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	favorites := strings.HasSuffix(r.URL.Path, "/favorites/playlists")
	items := []any{}
	add := func(v map[string]any) {
		if favorites {
			flat := map[string]any{"item": v}
			for key, val := range v {
				flat[key] = val
			}
			v = flat
		}
		items = append(items, v)
	}
	if n := len(g.s.stemmed(scope)); n > 0 {
		add(playlistJSON(stemsListID(scope), "Stems", "", n))
	}
	ip, _, alive := g.s.peerServerEndpoint(k, peerKey, serverID)
	if !alive {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(k.scope, errors.New("not available")), "", 0))
		respond(w, page(r, 100, items))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), serverListTimeout)
	defer cancel()
	var wire struct {
		Playlists []providers.Playlist `json:"playlists"`
	}
	_, err := g.s.peerServerGetJSON(ctx, ip, "/peer/servers/"+k.scope+"/"+serverID+"/playlists", &wire)
	if err != nil && len(wire.Playlists) == 0 {
		add(playlistJSON(hintPlaylist, sourceFailedTitle(k.scope, err), "", 0))
		respond(w, page(r, 100, items))
		return
	}
	prov := peerProviderForKind(k)
	for _, pl := range wire.Playlists {
		pl.ID = peerServerScopedID(peerKey, serverID, pl.ID)
		ref := playlistRef{session: scope, provider: prov, list: pl}
		add(g.refJSON(g.addPlaylist(ref), ref))
	}
	respond(w, page(r, 100, items))
}

func (g *tidalGateway) peerServerTracks(w http.ResponseWriter, r *http.Request, id string, ref playlistRef, role, order string) {
	k, ok := kindOfPeerProvider(ref.provider)
	if !ok {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	peerKey, serverID, rawPlaylist, ok := splitPeerServerScopedID(ref.list.ID)
	if !ok {
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	ip, name, alive := g.s.peerServerEndpoint(k, peerKey, serverID)
	if !alive {
		g.logPlaylistOpenFailure(r, http.StatusNotFound, id, &ref, errServerGone)
		fail(w, http.StatusNotFound, errServerGone)
		return
	}
	var wire struct {
		Tracks []providers.Track `json:"tracks"`
	}
	status, err := g.s.peerServerGetJSON(r.Context(), ip, "/peer/servers/"+k.scope+"/"+serverID+"/tracks?playlist="+url.QueryEscape(rawPlaylist), &wire)
	if err != nil {
		if status == http.StatusNotFound {
			err = errServerGone
		}
		g.logPlaylistOpenFailure(r, status, id, &ref, err)
		fail(w, status, err)
		return
	}
	if len(wire.Tracks) > maxPlaylistTracks {
		err := errors.New("playlist too large")
		g.logPlaylistOpenFailure(r, http.StatusBadRequest, id, &ref, err)
		fail(w, http.StatusBadRequest, err)
		return
	}
	tracks := make([]providers.Track, len(wire.Tracks))
	for i, t := range wire.Tracks {
		t.Provider = ref.provider
		t.ID = peerServerScopedID(peerKey, serverID, t.ID)
		t.Album = cmp.Or(name, t.Album)
		t.CoverURL = ""
		t.ThumbURL = ""
		tracks[i] = t
	}
	g.s.cfg.BPM.Enrich(r.Context(), tracks)
	g.enrichRatingsFromDB(r.Context(), tracks)
	scope := peerServerScope(k, peerKey, serverID)
	refs := serverRefs(scope, tracks)
	if role != "" && role != "default" {
		sortStreamingTracks(refs, role, order)
	}
	respond(w, page(r, 500, g.refItems(refs)))
}

func (g *tidalGateway) peerServerSearchOne(ctx context.Context, query string, k serverKind, peerKey, serverID string) []trackRef {
	ip, name, alive := g.s.peerServerEndpoint(k, peerKey, serverID)
	if !alive {
		return nil
	}
	var wire struct {
		Tracks []providers.Track `json:"tracks"`
	}
	if _, err := g.s.peerServerGetJSON(ctx, ip, "/peer/servers/"+k.scope+"/"+serverID+"/search?q="+url.QueryEscape(query), &wire); err != nil {
		return nil
	}
	prov := peerProviderForKind(k)
	scope := peerServerScope(k, peerKey, serverID)
	tracks := make([]providers.Track, len(wire.Tracks))
	for i, t := range wire.Tracks {
		t.Provider = prov
		t.ID = peerServerScopedID(peerKey, serverID, t.ID)
		t.Album = cmp.Or(name, t.Album)
		t.CoverURL = ""
		t.ThumbURL = ""
		tracks[i] = t
	}
	g.s.cfg.BPM.Enrich(ctx, tracks)
	return serverRefs(scope, tracks)
}

func (g *tidalGateway) peerServerSearch(ctx context.Context, query, skipScope string, add func(pos, n int, refs []trackRef)) {
	views := g.s.cachedLinkedServerViews()
	var list []linkedServerView
	for _, lv := range views {
		if lv.Scope != skipScope {
			list = append(list, lv)
		}
	}
	if len(list) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i, lv := range list {
		wg.Go(func() {
			k, peerKey, serverID, ok := parsePeerServerScope(lv.Scope)
			if !ok {
				return
			}
			refs := g.peerServerSearchOne(ctx, query, k, peerKey, serverID)
			add(i, len(list), refs)
		})
	}
	wg.Wait()
}

func (s *Server) downloadPeerServerTrack(ctx context.Context, provider, trackID, destDir string) (string, error) {
	k, ok := kindOfPeerProvider(provider)
	if !ok {
		return "", errServerGone
	}
	peerKey, serverID, rawID, ok := splitPeerServerScopedID(trackID)
	if !ok {
		return "", errServerGone
	}
	ip, _, alive := s.peerServerEndpoint(k, peerKey, serverID)
	if !alive {
		return "", errServerGone
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sharePeerURL(ip, "/peer/servers/"+k.scope+"/"+serverID+"/track?id="+url.QueryEscape(rawID)), nil)
	if err != nil {
		return "", err
	}
	resp, err := peerStreamHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errServerGone
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("peer server download HTTP %d", resp.StatusCode)
	}
	ext, _ := peerServeExtAndMIME("x" + strings.TrimSpace(resp.Header.Get("X-Enginex-Ext")))
	sum := sha256.Sum256([]byte("peer\x00" + provider + "\x00" + peerKey + "\x00" + serverID + "\x00" + rawID))
	filename := "nas-" + hex.EncodeToString(sum[:]) + ext
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(destDir, ".part-peer-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, peerTrackMaxBytes+1))
	if err == nil && n > peerTrackMaxBytes {
		err = errors.New("peer track exceeds 1 GiB limit")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", err
	}
	dst := filepath.Join(destDir, filename)
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	cleanup = false
	return dst, nil
}

// peerTrackLiveAndTrim is referenced from Jobs/gateway; keep deemixclient imported for Trim/Inspect if needed.
var _ = (*deemixclient.Client)(nil)
