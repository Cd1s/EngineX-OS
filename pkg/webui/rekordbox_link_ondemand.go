package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

const (
	rbLinkAnswerBudgetDefault = 750 * time.Millisecond
	rbLinkFilledRelayTimeout  = 2 * time.Second
	rbLinkPeerCacheTTL        = 5 * time.Minute
	rbLinkPeerNegTTL          = 60 * time.Second
	rbLinkMaxProbePeers       = 4
	rbLinkMaxBodyBytes        = 4 << 20
)

type rbLinkPeerEntry struct {
	endpoint string
	expires  time.Time
}

type rbPeerOnDemandRequest struct {
	UUID      string  `json:"uuid"`
	VolHint   string  `json:"volHint,omitempty"`
	Path      string  `json:"path"`
	ID        int64   `json:"id,omitempty"`
	ImportKey int64   `json:"importKey"`
	Key       *int    `json:"key,omitempty"`
	BPM       float64 `json:"bpm,omitempty"`
	Length    int     `json:"length,omitempty"`
}

type rbPeerFilledRequest struct {
	UUID      string `json:"uuid"`
	VolHint   string `json:"volHint,omitempty"`
	Path      string `json:"path"`
	ID        int64  `json:"id"`
	ImportKey int64  `json:"importKey"`
}

type rbFilledJournalLine struct {
	ID   int64  `json:"id"`
	Key  string `json:"k"`
	WR   string `json:"wr"`
	WRNM string `json:"wrnm,omitempty"`
	AL   uint32 `json:"al"`
	V    int    `json:"v"`
	W    string `json:"w"`
	At   int64  `json:"at"`
}

// parseNetDBPath parses Engine Link's airNfs library URI:
//
//	net://<peer-uuid>/<vol>/Engine Library/Database2/m.db
func parseNetDBPath(raw string) (peerUUID, volName string, ok bool) {
	const suffix = "/Engine Library/Database2/m.db"
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "net://") {
		return "", "", false
	}
	rest := raw[len("net://"):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash >= 128 {
		return "", "", false
	}
	host := rest[:slash]
	peerUUID = normalizeLinkUUID(host)
	if peerUUID == "" {
		return "", "", false
	}
	tail := rest[slash:]
	if len(tail) <= len(suffix)+1 || !strings.HasSuffix(tail, suffix) {
		return "", "", false
	}
	volName = tail[1 : len(tail)-len(suffix)]
	if volName == "" || volName == "." || volName == ".." || strings.ContainsAny(volName, "/\\") {
		return "", "", false
	}
	return peerUUID, volName, true
}

func normalizeLinkUUID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) >= 128 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '{' || c == '}':
			continue
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c == '-':
			b.WriteByte(c)
		case c >= 'A' && c <= 'F':
			b.WriteByte(c + ('a' - 'A'))
		default:
			return ""
		}
	}
	return b.String()
}

func validRBStageUUID(u string) bool {
	if u == "" || len(u) > 128 || u[0] == '.' {
		return false
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// isLANPeerRequest requires a direct LAN (RFC1918, link-local, or loopback in tests)
// request that was not relayed through an external tunnel or proxy header.
func isLANPeerRequest(r *http.Request) bool {
	if relayedFromOutside(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()
}

func (s *Server) rbLinkBudget() time.Duration {
	s.rbLinkMu.Lock()
	b := s.rbLinkAnswerBudget
	s.rbLinkMu.Unlock()
	if b > 0 {
		return b
	}
	return rbLinkAnswerBudgetDefault
}

func (s *Server) rbLinkHTTPClient() *http.Client {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	if s.rbLinkPeerClient == nil {
		s.rbLinkPeerClient = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 350 * time.Millisecond}).DialContext,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     30 * time.Second,
			},
		}
	}
	return s.rbLinkPeerClient
}

func (s *Server) linkPeerURL(endpoint, route string) string {
	if _, _, err := net.SplitHostPort(endpoint); err == nil {
		return "http://" + endpoint + route
	}
	port := 8080
	if s.shadow != nil && s.shadow.peerPort > 0 {
		port = s.shadow.peerPort
	}
	return "http://" + net.JoinHostPort(endpoint, strconv.Itoa(port)) + route
}

func localIPv4Set() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil {
				out[ip4.String()] = true
			}
		}
	}
	return out
}

// parseProcNetTCPLANPeers extracts up to maxPeers unique private/link-local remote
// IPv4 endpoints from Linux /proc/net/tcp ESTABLISHED (01) sockets. Used strictly on
// demand when 4-deck shadow discovery has no peer entry (2-deck mode).
func parseProcNetTCPLANPeers(content string, ownIPs map[string]bool, maxPeers int) []string {
	if maxPeers <= 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "01" { // 01 = TCP_ESTABLISHED
			continue
		}
		remHost, _, ok := strings.Cut(fields[2], ":")
		if !ok || len(remHost) != 8 {
			continue
		}
		v, err := strconv.ParseUint(remHost, 16, 32)
		if err != nil {
			continue
		}
		ip := net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		if ip.IsLoopback() || ip.IsUnspecified() || (!ip.IsPrivate() && !ip.IsLinkLocalUnicast()) {
			continue
		}
		s := ip.String()
		if ownIPs[s] || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= maxPeers {
			break
		}
	}
	return out
}

func (s *Server) discoverLinkPeerCandidates(peerUUID string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ep string) {
		ep = strings.TrimSpace(ep)
		if ep == "" || seen[ep] || len(out) >= rbLinkMaxProbePeers {
			return
		}
		seen[ep] = true
		out = append(out, ep)
	}

	// 1. Exact Engine Link UUID match from 4-deck shadow peerDecks (0 ms, no scan).
	now := time.Now()
	if s.shadow != nil {
		s.shadow.mu.Lock()
		for _, p := range s.shadow.peerDecks {
			if p.ip != "" && normalizeLinkUUID(p.Link) == peerUUID && now.Sub(p.at) < shadowPeerSeenTimeout {
				add(p.ip)
			}
		}
		// 2. Other live discovered EngineX peers (4-deck mode before peer loads a deck).
		for ip, at := range s.shadow.peers {
			if ip != "" && now.Sub(at) < shadowPeerSeenTimeout {
				add(ip)
			}
		}
		s.shadow.mu.Unlock()
	}

	// 3. On-demand fallback for 2-deck (shadow OFF) mode: active LAN TCP peers from /proc/net/tcp.
	if len(out) == 0 {
		s.rbLinkMu.Lock()
		fn := s.rbLinkPeerCandidates
		s.rbLinkMu.Unlock()
		if fn != nil {
			for _, ep := range fn() {
				add(ep)
			}
		} else if raw, err := os.ReadFile("/proc/net/tcp"); err == nil {
			for _, ip := range parseProcNetTCPLANPeers(string(raw), localIPv4Set(), rbLinkMaxProbePeers) {
				add(ip)
			}
		}
	}
	return out
}

func (s *Server) getCachedLinkPeer(peerUUID string, now time.Time) (string, bool) {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	if e, ok := s.rbLinkPeers[peerUUID]; ok && now.Before(e.expires) && e.endpoint != "" {
		return e.endpoint, true
	}
	return "", false
}

func (s *Server) setCachedLinkPeer(peerUUID, endpoint string, now time.Time) {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	if s.rbLinkPeers == nil {
		s.rbLinkPeers = map[string]rbLinkPeerEntry{}
	}
	s.rbLinkPeers[peerUUID] = rbLinkPeerEntry{endpoint: endpoint, expires: now.Add(rbLinkPeerCacheTTL)}
	delete(s.rbLinkPeerNeg, peerUUID)
}

func (s *Server) clearCachedLinkPeer(peerUUID, endpoint string) {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	if e, ok := s.rbLinkPeers[peerUUID]; ok && (endpoint == "" || e.endpoint == endpoint) {
		delete(s.rbLinkPeers, peerUUID)
	}
}

func (s *Server) isLinkPeerNegCached(peerUUID string, now time.Time) bool {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	exp, ok := s.rbLinkPeerNeg[peerUUID]
	if !ok {
		return false
	}
	if now.Before(exp) {
		return true
	}
	delete(s.rbLinkPeerNeg, peerUUID)
	return false
}

func (s *Server) setLinkPeerNegCached(peerUUID string, now time.Time) {
	s.rbLinkMu.Lock()
	defer s.rbLinkMu.Unlock()
	if s.rbLinkPeerNeg == nil {
		s.rbLinkPeerNeg = map[string]time.Time{}
	}
	s.rbLinkPeerNeg[peerUUID] = now.Add(rbLinkPeerNegTTL)
	delete(s.rbLinkPeers, peerUUID)
}

type rbLinkProbeOutcome struct {
	endpoint    string
	status      int
	rbStatus    string
	headers     http.Header
	body        []byte
	unsupported bool
	timedOut    bool
	err         error
}

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "connection refused")
}

func (s *Server) fetchFromLinkPeer(ctx context.Context, endpoint string, payload []byte) rbLinkProbeOutcome {
	s.rbLinkPeerHTTPCount.Add(1)
	u := s.linkPeerURL(endpoint, RekordboxPeerOnDemandRoute)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return rbLinkProbeOutcome{endpoint: endpoint, err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.rbLinkHTTPClient().Do(httpReq)
	if err != nil {
		return rbLinkProbeOutcome{
			endpoint:    endpoint,
			unsupported: isConnRefused(err),
			timedOut:    errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
			err:         err,
		}
	}
	defer resp.Body.Close()

	rbStatus := strings.TrimSpace(resp.Header.Get("X-RB-Status"))
	if resp.StatusCode == http.StatusOK {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, rbLinkMaxBodyBytes+1))
		if rerr != nil {
			return rbLinkProbeOutcome{
				endpoint: endpoint,
				timedOut: errors.Is(rerr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
				err:      rerr,
			}
		}
		if len(body) > rbLinkMaxBodyBytes || len(body) < 5 || !bytes.Equal(body[:5], []byte("RBXA1")) {
			return rbLinkProbeOutcome{endpoint: endpoint, status: http.StatusBadGateway, err: errors.New("invalid peer RBXA1")}
		}
		return rbLinkProbeOutcome{
			endpoint: endpoint,
			status:   http.StatusOK,
			headers:  resp.Header.Clone(),
			body:     body,
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	// Older EngineX (404/405 without X-RB-Status) or non-EngineX HTTP service: peer does not support Link on-demand.
	unsupported := rbStatus == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden)
	return rbLinkProbeOutcome{
		endpoint:    endpoint,
		status:      resp.StatusCode,
		rbStatus:    rbStatus,
		unsupported: unsupported,
	}
}

func writeOnDemandProxyResponse(w http.ResponseWriter, out rbLinkProbeOutcome) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(out.body)))
	for _, h := range []string{
		"X-RB-Key", "X-RB-Written", "X-RB-Written-NoMain",
		"X-RB-Row", "X-RB-Active-Loops", "X-RB-Rules", "X-RB-Timing-Ms",
	} {
		if v := out.headers.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.body)
}

// serveRekordboxLinkRelay handles Unit B's local hook request for a net:// library
// by asking the stick-owning EngineX peer over the LAN within a fixed answer budget.
func (s *Server) serveRekordboxLinkRelay(w http.ResponseWriter, r *http.Request, req rbOnDemandRequest) {
	peerUUID, volHint, ok := parseNetDBPath(req.DB)
	if !ok {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("invalid net:// library path"))
		return
	}
	libUUID := strings.TrimSpace(req.UUID)
	if !validRBStageUUID(libUUID) || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("missing library uuid or importKey"))
		return
	}

	now := time.Now()
	if s.isLinkPeerNegCached(peerUUID, now) {
		w.Header().Set("X-RB-Status", "peer_unsupported")
		fail(w, http.StatusNotFound, errors.New("peer unsupported (cached)"))
		return
	}

	peerPayload, err := json.Marshal(rbPeerOnDemandRequest{
		UUID:      libUUID,
		VolHint:   volHint,
		Path:      req.Path,
		ID:        req.ID,
		ImportKey: req.ImportKey,
		Key:       req.Key,
		BPM:       req.BPM,
		Length:    req.Length,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.rbLinkBudget())
	defer cancel()

	// Fast path: cached peer endpoint for this Engine Link UUID.
	cachedEP, hadCached := s.getCachedLinkPeer(peerUUID, now)
	if hadCached {
		out := s.fetchFromLinkPeer(ctx, cachedEP, peerPayload)
		if out.status == http.StatusOK {
			writeOnDemandProxyResponse(w, out)
			return
		}
		if out.rbStatus != "" && out.rbStatus != "not_found" {
			w.Header().Set("X-RB-Status", out.rbStatus)
			fail(w, out.status, errors.New("peer: "+out.rbStatus))
			return
		}
		if out.timedOut {
			w.Header().Set("X-RB-Status", "timeout")
			fail(w, http.StatusGatewayTimeout, errors.New("peer answer budget exceeded"))
			return
		}
		s.clearCachedLinkPeer(peerUUID, cachedEP)
		if out.unsupported {
			// Re-discover only if other candidates exist; if cachedEP was the only one, fall through.
		}
	}

	cands := s.discoverLinkPeerCandidates(peerUUID)
	if len(cands) == 0 {
		w.Header().Set("X-RB-Status", "peer_unsupported")
		fail(w, http.StatusNotFound, errors.New("no LAN peer found"))
		return
	}

	// Probe up to rbLinkMaxProbePeers candidates in parallel; first 200 OK wins.
	probeCtx, probeCancel := context.WithCancel(ctx)
	defer probeCancel()
	results := make(chan rbLinkProbeOutcome, len(cands))
	var wg sync.WaitGroup
	for _, ep := range cands {
		if ep == cachedEP {
			continue
		}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			results <- s.fetchFromLinkPeer(probeCtx, target, peerPayload)
		}(ep)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		anyCapable      bool
		anyTimedOut     bool
		anyTransientErr bool
		unsupportedCnt  int
		bestStatus      int
		bestRBStat      string
	)
	for out := range results {
		if out.status == http.StatusOK {
			probeCancel()
			s.setCachedLinkPeer(peerUUID, out.endpoint, time.Now())
			writeOnDemandProxyResponse(w, out)
			return
		}
		if out.rbStatus != "" {
			anyCapable = true
			if bestStatus == 0 || out.rbStatus != "not_found" {
				bestStatus, bestRBStat = out.status, out.rbStatus
			}
		} else if out.timedOut {
			anyTimedOut = true
		} else if out.unsupported {
			unsupportedCnt++
		} else {
			anyTransientErr = true
		}
	}

	if anyTimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		w.Header().Set("X-RB-Status", "timeout")
		fail(w, http.StatusGatewayTimeout, errors.New("peer answer budget exceeded"))
		return
	}
	if anyCapable {
		w.Header().Set("X-RB-Status", bestRBStat)
		fail(w, bestStatus, errors.New("peer: "+bestRBStat))
		return
	}
	if anyTransientErr {
		w.Header().Set("X-RB-Status", "unavailable")
		fail(w, http.StatusServiceUnavailable, errors.New("peer transient error"))
		return
	}
	if unsupportedCnt > 0 || hadCached {
		s.setLinkPeerNegCached(peerUUID, time.Now())
	}
	w.Header().Set("X-RB-Status", "peer_unsupported")
	fail(w, http.StatusNotFound, errors.New("peer does not support link on-demand"))
}

func (s *Server) handleRekordboxOnDemandFilled(w http.ResponseWriter, r *http.Request) {
	s.serveRekordboxFilledNotify(w, r, false)
}

func (s *Server) serveRekordboxFilledNotify(w http.ResponseWriter, r *http.Request, fromUnix bool) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !fromUnix && !writeBackFromDeck(r) {
		fail(w, http.StatusForbidden, errors.New("loopback only"))
		return
	}
	var req rbOnDemandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	peerUUID, volHint, ok := parseNetDBPath(req.DB)
	libUUID := strings.TrimSpace(req.UUID)
	if !ok || !validRBStageUUID(libUUID) || strings.TrimSpace(req.Path) == "" || req.ID <= 0 || req.ImportKey <= 0 {
		fail(w, http.StatusBadRequest, errors.New("invalid filled notification"))
		return
	}

	ep, ok := s.getCachedLinkPeer(peerUUID, time.Now())
	if !ok {
		if cands := s.discoverLinkPeerCandidates(peerUUID); len(cands) > 0 {
			ep = cands[0]
		}
	}
	if ep != "" {
		payload, err := json.Marshal(rbPeerFilledRequest{
			UUID:      libUUID,
			VolHint:   volHint,
			Path:      req.Path,
			ID:        req.ID,
			ImportKey: req.ImportKey,
		})
		if err == nil {
			s.rbOnDemandWG.Add(1)
			go func(target string, body []byte) {
				defer s.rbOnDemandWG.Done()
				ctx, cancel := context.WithTimeout(context.Background(), rbLinkFilledRelayTimeout)
				defer cancel()
				go func() {
					select {
					case <-s.closed:
						cancel()
					case <-ctx.Done():
					}
				}()
				u := s.linkPeerURL(target, RekordboxPeerFilledRoute)
				httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
				if err != nil {
					return
				}
				httpReq.Header.Set("Content-Type", "application/json")
				resp, err := s.rbLinkHTTPClient().Do(httpReq)
				if err == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
					resp.Body.Close()
				}
			}(ep, payload)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// findMountedStickByUUID locates the mounted stick under mediaRoot whose bridge snapshot
// LibraryUUID matches uuid. Never authorizes a mount by volume name or path alone.
func (s *Server) findMountedStickByUUID(uuid, volHint string) (string, bool) {
	if !validRBStageUUID(uuid) {
		return "", false
	}
	root := filepath.Clean(strings.TrimSpace(s.onDemandMediaRoot()))
	if root == "" {
		return "", false
	}
	checkDir := func(name string) (string, bool) {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			return "", false
		}
		mount := filepath.Join(root, name)
		st, err := os.Lstat(mount)
		if err != nil || !st.Mode().IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
		hdr, err := rbstate.ReadHeader(filepath.Join(mount, "Engine Library", "Database2"))
		if err != nil || hdr.LibraryUUID != uuid {
			return "", false
		}
		return mount, true
	}
	if volHint != "" {
		if m, ok := checkDir(volHint); ok {
			return m, true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, ent := range entries {
		if ent.Name() == volHint {
			continue
		}
		if m, ok := checkDir(ent.Name()); ok {
			return m, true
		}
	}
	return "", false
}

// handlePeerRekordboxOnDemand answers LAN peers strictly for tracks on this unit's own
// mounted stick matching BOTH Information.uuid AND Track.pdbImportKey.
func (s *Server) handlePeerRekordboxOnDemand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	var req rbPeerOnDemandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, err)
		return
	}
	libUUID := strings.TrimSpace(req.UUID)
	if !validRBStageUUID(libUUID) || strings.TrimSpace(req.Path) == "" || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("uuid, path, and positive importKey required"))
		return
	}
	mount, ok := s.findMountedStickByUUID(libUUID, req.VolHint)
	if !ok {
		w.Header().Set("X-RB-Status", "not_found")
		fail(w, http.StatusNotFound, errors.New("stick uuid not mounted here"))
		return
	}

	s.rbPriorityQueue().NoteLoading(time.Now())

	engineKey := -1
	if req.Key != nil {
		engineKey = *req.Key
	}
	res, err := s.buildOnDemandCachedWithImportKey(r.Context(), bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       libUUID,
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  engineKey,
		EngineBPM:  req.BPM,
		EngineLen:  req.Length,
	}, true, req.ImportKey)
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrOnDemandNotFound):
			w.Header().Set("X-RB-Status", "not_found")
			fail(w, http.StatusNotFound, err)
		case errors.Is(err, bridge.ErrOnDemandUUIDMismatch):
			w.Header().Set("X-RB-Status", "uuid_mismatch")
			fail(w, http.StatusConflict, err)
		case errors.Is(err, bridge.ErrOnDemandDegraded), errors.Is(err, bridge.ErrOnDemandLimit):
			w.Header().Set("X-RB-Status", "degraded")
			fail(w, http.StatusUnprocessableEntity, err)
		case errors.Is(err, bridge.ErrOnDemandUnavailable):
			w.Header().Set("X-RB-Status", "unavailable")
			fail(w, http.StatusServiceUnavailable, err)
		default:
			w.Header().Set("X-RB-Status", "error")
			fail(w, http.StatusServiceUnavailable, err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.RBXA1)))
	w.Header().Set("X-RB-Key", res.Key)
	w.Header().Set("X-RB-Written", base64.StdEncoding.EncodeToString(res.Written))
	w.Header().Set("X-RB-Written-NoMain", base64.StdEncoding.EncodeToString(res.WrittenNoMain))
	w.Header().Set("X-RB-Row", res.RowHash)
	w.Header().Set("X-RB-Active-Loops", strconv.FormatUint(uint64(res.ActiveLoops), 10))
	w.Header().Set("X-RB-Rules", strconv.Itoa(res.Rules))
	w.Header().Set("X-RB-Timing-Ms", fmt.Sprintf("%.2f", res.Timing.TotalMS))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.RBXA1)
}

func (s *Server) rbStageRoot() (string, error) {
	if p := strings.TrimSpace(s.cfg.RBStageRoot); p != "" {
		return p, nil
	}
	return rbstate.StageRoot()
}

func (s *Server) appendRemoteFilledJournal(uuid string, engineID int64, res *bridge.OnDemandResult) error {
	if !validRBStageUUID(uuid) || engineID <= 0 || res == nil || res.Key == "" || len(res.Written) == 0 {
		return errors.New("invalid filled journal entry")
	}
	root, err := s.rbStageRoot()
	if err != nil {
		return err
	}
	line := rbFilledJournalLine{
		ID:  engineID,
		Key: res.Key,
		WR:  base64.StdEncoding.EncodeToString(res.Written),
		AL:  res.ActiveLoops,
		V:   res.Rules,
		W:   res.RowHash,
		At:  time.Now().Unix(),
	}
	if len(res.WrittenNoMain) > 0 {
		line.WRNM = base64.StdEncoding.EncodeToString(res.WrittenNoMain)
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	s.rbLinkJournalMu.Lock()
	defer s.rbLinkJournalMu.Unlock()
	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "filled.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// handlePeerRekordboxFilled appends to this unit's filled.jsonl after a LAN peer commits
// an on-demand fill on our mounted stick. Digests are always derived locally on this unit
// from the verified stick + Track row, never trusted from the peer request.
func (s *Server) handlePeerRekordboxFilled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	var req rbPeerFilledRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, err)
		return
	}
	libUUID := strings.TrimSpace(req.UUID)
	if !validRBStageUUID(libUUID) || strings.TrimSpace(req.Path) == "" || req.ID <= 0 || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("uuid, path, id, and positive importKey required"))
		return
	}
	mount, ok := s.findMountedStickByUUID(libUUID, req.VolHint)
	if !ok {
		w.Header().Set("X-RB-Status", "not_found")
		fail(w, http.StatusNotFound, errors.New("stick uuid not mounted here"))
		return
	}
	res, err := s.buildOnDemandCachedWithImportKey(r.Context(), bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       libUUID,
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  -1,
	}, true, req.ImportKey)
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrOnDemandNotFound):
			w.Header().Set("X-RB-Status", "not_found")
			fail(w, http.StatusNotFound, err)
		case errors.Is(err, bridge.ErrOnDemandUUIDMismatch):
			w.Header().Set("X-RB-Status", "uuid_mismatch")
			fail(w, http.StatusConflict, err)
		default:
			w.Header().Set("X-RB-Status", "unavailable")
			fail(w, http.StatusServiceUnavailable, err)
		}
		return
	}
	if err := s.appendRemoteFilledJournal(libUUID, req.ID, res); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.rbLinkFilledCount.Add(1)
	respond(w, map[string]any{"ok": true})
}
