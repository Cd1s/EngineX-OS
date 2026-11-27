package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

const (
	rbLinkAnswerBudgetDefault = 750 * time.Millisecond
	rbLinkFilledRelayTimeout  = 2 * time.Second
	rbLinkPeerCacheTTL        = 5 * time.Minute
	rbLinkPeerNegTTL          = 60 * time.Second
	rbLinkMaxProbeConcurrency = 4
	rbLinkMaxConcurrentLAN    = 4
	rbLinkMaxQueuedLAN        = 16
	rbLinkMaxBodyBytes        = 4 << 20
)

var errLinkFillNotCommitted = errors.New("track fill not committed in library")

type rbLinkPeerEntry struct {
	endpoint string
	expires  time.Time
}

type rbPeerOnDemandRequest struct {
	UUID      string `json:"uuid"`
	Owner     string `json:"owner,omitempty"`
	VolHint   string `json:"volHint,omitempty"`
	Path      string `json:"path"`
	ID        int64  `json:"id,omitempty"`
	ImportKey int64  `json:"importKey"`
}

type rbPeerFilledRequest struct {
	UUID      string `json:"uuid"`
	Owner     string `json:"owner,omitempty"`
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
	peerUUID = normalizeLinkUUID(rest[:slash])
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

func isLANPeerRequest(r *http.Request) bool {
	if relayedFromOutside(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback())
}

var errLinkLANBusy = errors.New("too many concurrent peer requests")

func (s *Server) acquireLANLinkSlot(ctx context.Context) (func(), error) {
	for {
		cur := s.rbLinkLANQueued.Load()
		if cur >= rbLinkMaxQueuedLAN {
			return nil, errLinkLANBusy
		}
		if s.rbLinkLANQueued.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	s.rbLinkMu.Lock()
	if s.rbLinkLANSem == nil {
		s.rbLinkLANSem = make(chan struct{}, rbLinkMaxConcurrentLAN)
	}
	sem := s.rbLinkLANSem
	s.rbLinkMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() {
			<-sem
			s.rbLinkLANQueued.Add(-1)
		}, nil
	case <-ctx.Done():
		s.rbLinkLANQueued.Add(-1)
		return nil, ctx.Err()
	}
}

func (s *Server) ownLinkUUID() string {
	if id := normalizeLinkUUID(s.cfg.LinkUUID); id != "" {
		return id
	}
	if s.shadow == nil {
		return ""
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	for _, d := range s.shadow.own {
		if id := normalizeLinkUUID(d.Link); id != "" {
			return id
		}
	}
	for _, d := range s.shadow.clockDecks {
		if id := normalizeLinkUUID(d.Link); id != "" {
			return id
		}
	}
	return ""
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

func (s *Server) readProcFile(p string) ([]byte, error) {
	s.rbLinkMu.Lock()
	fn := s.rbLinkProcReadFile
	s.rbLinkMu.Unlock()
	if fn != nil {
		return fn(p)
	}
	return os.ReadFile(p)
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

// isEngineLinkTCPPort accepts ephemeral/StageLinQ service ports (>= 1024) and rejects
// well-known or non-Engine-Link services (SSH, HTTP, HTTPS, SMB, stream-sync).
func isEngineLinkTCPPort(port uint64) bool {
	return port >= 1024 && port != 8080 && port != 8443
}

// engineSocketInodes returns the socket inodes held open by running Engine processes.
// Checks ctx between /proc reads so discovery honors the shared answer budget.
func engineSocketInodes(ctx context.Context) map[string]bool {
	if ctx.Err() != nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	inodes := map[string]bool{}
	for _, ent := range entries {
		if ctx.Err() != nil {
			return nil
		}
		name := ent.Name()
		if len(name) == 0 || name[0] < '0' || name[0] > '9' {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", name, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "Engine" {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", name, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if ctx.Err() != nil {
				return nil
			}
			if target, err := os.Readlink(filepath.Join("/proc", name, "fd", fd.Name())); err == nil && strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
				inodes[target[len("socket:["):len(target)-1]] = true
			}
		}
	}
	return inodes
}

// parseProcNetTCPLANPeers extracts all unique private/link-local remote IPv4 peers from
// Linux /proc/net/tcp or /proc/net/tcp6 ESTABLISHED (01) Engine Link sockets.
// When engineInodes is non-nil, only sockets owned by Engine (matching column 9 inode)
// are accepted; non-Engine-Link ports (<1024, 8080, 8443) are always excluded.
func parseProcNetTCPLANPeers(content string, ownIPs map[string]bool, engineInodes map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "01" {
			continue
		}
		if engineInodes != nil && (len(fields) < 10 || !engineInodes[fields[9]]) {
			continue
		}
		_, locPortHex, locOK := strings.Cut(fields[1], ":")
		remHost, remPortHex, remOK := strings.Cut(fields[2], ":")
		if !locOK || !remOK {
			continue
		}
		locPort, err1 := strconv.ParseUint(locPortHex, 16, 16)
		remPort, err2 := strconv.ParseUint(remPortHex, 16, 16)
		if err1 != nil || err2 != nil || !isEngineLinkTCPPort(locPort) || !isEngineLinkTCPPort(remPort) {
			continue
		}
		var v4Hex string
		switch len(remHost) {
		case 8:
			v4Hex = remHost
		case 32:
			upper := strings.ToUpper(remHost)
			if strings.HasPrefix(upper, "0000000000000000FFFF0000") || strings.HasPrefix(upper, "00000000000000000000FFFF") {
				v4Hex = remHost[24:32]
			} else {
				continue
			}
		default:
			continue
		}
		v, err := strconv.ParseUint(v4Hex, 16, 32)
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
	}
	return out
}

// parseProcNetARPLANPeers extracts all unique complete (0x2) private/link-local IPv4
// neighbors from Linux /proc/net/arp.
func parseProcNetARPLANPeers(content string, ownIPs map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(content))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(fields[2]), "0x"), 16, 32)
		if err != nil || flags&0x2 == 0 || fields[3] == "00:00:00:00:00:00" {
			continue
		}
		ip := net.ParseIP(fields[0]).To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || (!ip.IsPrivate() && !ip.IsLinkLocalUnicast()) {
			continue
		}
		s := ip.String()
		if ownIPs[s] || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func (s *Server) discoverEngineLinkTCPPeers(ctx context.Context) []string {
	if ctx.Err() != nil {
		return nil
	}
	s.rbLinkMu.Lock()
	fn := s.rbLinkPeerCandidates
	s.rbLinkMu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	inodes := engineSocketInodes(ctx)
	if len(inodes) == 0 || ctx.Err() != nil {
		return nil
	}
	own := localIPv4Set()
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if ctx.Err() != nil {
			return out
		}
		raw, err := s.readProcFile(p)
		if err != nil {
			continue
		}
		for _, ip := range parseProcNetTCPLANPeers(string(raw), own, inodes) {
			if !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

func (s *Server) discoverARPLANPeers(ctx context.Context) []string {
	if ctx.Err() != nil {
		return nil
	}
	s.rbLinkMu.Lock()
	fn := s.rbLinkARPCandidates
	s.rbLinkMu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	raw, err := s.readProcFile("/proc/net/arp")
	if err != nil {
		return nil
	}
	return parseProcNetARPLANPeers(string(raw), localIPv4Set())
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

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "connection refused")
}

func replyMatchesOwner(h http.Header, peerUUID, libUUID string) bool {
	if strings.TrimSpace(h.Get("X-RB-UUID")) != libUUID {
		return false
	}
	if rawOwner := strings.TrimSpace(h.Get("X-RB-Owner")); rawOwner != "" {
		return normalizeLinkUUID(rawOwner) == peerUUID
	}
	return true
}

type rbLinkOwnerResult struct {
	endpoint    string
	verified    bool // true when endpoint is verified as peerUUID or verified owner of libUUID
	unsupported bool // true only when verified peerUUID does not support Link on-demand
	notFound    bool // true only when verified peerUUID confirmed stick is not mounted there
	timedOut    bool
}

// peerDecksHasLink asks an answering HTTP peer's GET /peer/decks to verify whether it
// actually owns Engine Link UUID peerUUID (used before negative-caching older EngineX peers).
func (s *Server) peerDecksHasLink(ctx context.Context, endpoint, peerUUID string) bool {
	if ctx.Err() != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.linkPeerURL(endpoint, "/peer/decks"), nil)
	if err != nil {
		return false
	}
	resp, err := s.rbLinkHTTPClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return false
	}
	var body struct {
		Decks []struct {
			Link string `json:"link"`
		} `json:"decks"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return false
	}
	for _, d := range body.Decks {
		if normalizeLinkUUID(d.Link) == peerUUID {
			return true
		}
	}
	return false
}

// probeLinkOwner checks whether candidate endpoint actually owns stick libUUID (and matches
// peerUUID when advertised) via a zero-build GET on RekordboxPeerOnDemandRoute.
func (s *Server) probeLinkOwner(ctx context.Context, endpoint, peerUUID, libUUID, volHint string) rbLinkOwnerResult {
	s.rbLinkPeerHTTPCount.Add(1)
	q := url.Values{}
	q.Set("uuid", libUUID)
	q.Set("owner", peerUUID)
	if volHint != "" {
		q.Set("vol", volHint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.linkPeerURL(endpoint, RekordboxPeerOnDemandRoute)+"?"+q.Encode(), nil)
	if err != nil {
		return rbLinkOwnerResult{}
	}
	resp, err := s.rbLinkHTTPClient().Do(req)
	if err != nil {
		return rbLinkOwnerResult{
			timedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	rbStatus := strings.TrimSpace(resp.Header.Get("X-RB-Status"))
	if resp.StatusCode == http.StatusOK && rbStatus == "owner" && replyMatchesOwner(resp.Header, peerUUID, libUUID) {
		return rbLinkOwnerResult{endpoint: endpoint, verified: true}
	}
	if rbStatus == "not_found" {
		if normalizeLinkUUID(resp.Header.Get("X-RB-Owner")) == peerUUID {
			return rbLinkOwnerResult{endpoint: endpoint, verified: true, notFound: true}
		}
		return rbLinkOwnerResult{}
	}
	if rbStatus == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden) {
		if s.peerDecksHasLink(ctx, endpoint, peerUUID) {
			return rbLinkOwnerResult{endpoint: endpoint, verified: true, unsupported: true}
		}
	}
	return rbLinkOwnerResult{}
}

// resolveLinkOwner is the single owner-resolution path shared by both track-data and
// track-filled: cached owner -> exact shadow peerDecks match -> known shadow peers ->
// 2-deck Engine Link TCP fallback -> ARP fallback (still read when earlier tiers found
// only unrelated peers).
func (s *Server) resolveLinkOwner(ctx context.Context, peerUUID, libUUID, volHint, excludeEP string) rbLinkOwnerResult {
	now := time.Now()
	if ep, ok := s.getCachedLinkPeer(peerUUID, now); ok && ep != excludeEP {
		return rbLinkOwnerResult{endpoint: ep, verified: true}
	}

	var shadowPeers []string
	if s.shadow != nil {
		s.shadow.mu.Lock()
		for _, p := range s.shadow.peerDecks {
			if p.ip != "" && p.ip != excludeEP && normalizeLinkUUID(p.Link) == peerUUID && now.Sub(p.at) < shadowPeerSeenTimeout {
				s.shadow.mu.Unlock()
				return rbLinkOwnerResult{endpoint: p.ip, verified: true}
			}
		}
		for ip, at := range s.shadow.peers {
			if ip != "" && ip != excludeEP && now.Sub(at) < shadowPeerSeenTimeout {
				shadowPeers = append(shadowPeers, ip)
			}
		}
		s.shadow.mu.Unlock()
	}

	tried := map[string]bool{}
	if excludeEP != "" {
		tried[excludeEP] = true
	}
	var anyTimedOut bool
	probeTier := func(cands []string) (rbLinkOwnerResult, bool) {
		var batch []string
		for _, ep := range cands {
			ep = strings.TrimSpace(ep)
			if ep == "" || tried[ep] {
				continue
			}
			tried[ep] = true
			batch = append(batch, ep)
		}
		if len(batch) == 0 || ctx.Err() != nil {
			return rbLinkOwnerResult{}, false
		}
		tierCtx, tierCancel := context.WithCancel(ctx)
		defer tierCancel()
		sem := make(chan struct{}, rbLinkMaxProbeConcurrency)
		results := make(chan rbLinkOwnerResult, len(batch))
		var wg sync.WaitGroup
		for _, ep := range batch {
			wg.Add(1)
			go func(target string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-tierCtx.Done():
					results <- rbLinkOwnerResult{timedOut: errors.Is(tierCtx.Err(), context.DeadlineExceeded)}
					return
				}
				defer func() { <-sem }()
				results <- s.probeLinkOwner(tierCtx, target, peerUUID, libUUID, volHint)
			}(ep)
		}
		go func() {
			wg.Wait()
			close(results)
		}()

		var best rbLinkOwnerResult
		for r := range results {
			if r.verified && !r.unsupported && !r.notFound {
				tierCancel()
				s.setCachedLinkPeer(peerUUID, r.endpoint, time.Now())
				return r, true
			}
			if r.verified && !best.verified {
				best = r
			}
			if r.timedOut {
				anyTimedOut = true
			}
		}
		if best.verified {
			return best, true
		}
		return rbLinkOwnerResult{}, false
	}

	// Tier 1: known shadow peers (without exact Link UUID match).
	if r, done := probeTier(shadowPeers); done {
		return r
	}
	// Tier 2: 2-deck Engine Link TCP fallback (always tried even if Tier 1 had unrelated shadow peers).
	if ctx.Err() == nil {
		if r, done := probeTier(s.discoverEngineLinkTCPPeers(ctx)); done {
			return r
		}
	}
	// Tier 3: ARP fallback (still read when TCP found nothing or only unrelated peers).
	if ctx.Err() == nil {
		if r, done := probeTier(s.discoverARPLANPeers(ctx)); done {
			return r
		}
	}
	return rbLinkOwnerResult{timedOut: anyTimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded)}
}

type rbLinkFetchOutcome struct {
	status      int
	rbStatus    string
	headers     http.Header
	body        []byte
	unsupported bool
	timedOut    bool
	err         error
}

func (s *Server) fetchFromLinkPeer(ctx context.Context, endpoint, peerUUID, libUUID string, payload []byte) rbLinkFetchOutcome {
	s.rbLinkPeerHTTPCount.Add(1)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.linkPeerURL(endpoint, RekordboxPeerOnDemandRoute), bytes.NewReader(payload))
	if err != nil {
		return rbLinkFetchOutcome{err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.rbLinkHTTPClient().Do(httpReq)
	if err != nil {
		return rbLinkFetchOutcome{
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
			return rbLinkFetchOutcome{
				timedOut: errors.Is(rerr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
				err:      rerr,
			}
		}
		if len(body) > rbLinkMaxBodyBytes || len(body) < 5 || !bytes.Equal(body[:5], []byte("RBXA1")) || !replyMatchesOwner(resp.Header, peerUUID, libUUID) {
			return rbLinkFetchOutcome{status: http.StatusBadGateway, err: errors.New("invalid or unverified peer RBXA1")}
		}
		return rbLinkFetchOutcome{status: http.StatusOK, headers: resp.Header.Clone(), body: body}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	unsupported := rbStatus == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden)
	return rbLinkFetchOutcome{status: resp.StatusCode, rbStatus: rbStatus, unsupported: unsupported}
}

func writeOnDemandProxyResponse(w http.ResponseWriter, out rbLinkFetchOutcome) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(out.body)))
	for _, h := range []string{
		"X-RB-UUID", "X-RB-Owner",
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
// by resolving the verified stick owner and fetching RBXA1 within one shared budget.
func (s *Server) serveRekordboxLinkRelay(w http.ResponseWriter, r *http.Request, req rbOnDemandRequest) {
	peerUUID, volHint, ok := parseNetDBPath(req.DB)
	libUUID := strings.TrimSpace(req.UUID)
	if !ok || !validRBStageUUID(libUUID) || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("invalid net:// library request"))
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
		Owner:     peerUUID,
		VolHint:   volHint,
		Path:      req.Path,
		ID:        req.ID,
		ImportKey: req.ImportKey,
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.rbLinkBudget())
	defer cancel()

	// Resolve verified owner (retrying once without stale cached endpoint if cache moved).
	excludeEP := ""
	for attempt := 0; attempt < 2; attempt++ {
		owner := s.resolveLinkOwner(ctx, peerUUID, libUUID, volHint, excludeEP)
		if owner.unsupported && owner.verified {
			s.setLinkPeerNegCached(peerUUID, time.Now())
			w.Header().Set("X-RB-Status", "peer_unsupported")
			fail(w, http.StatusNotFound, errors.New("peer does not support link on-demand"))
			return
		}
		if owner.notFound && owner.verified {
			w.Header().Set("X-RB-Status", "not_found")
			fail(w, http.StatusNotFound, bridge.ErrOnDemandNotFound)
			return
		}
		if owner.endpoint == "" {
			if owner.timedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeOnDemandError(w, context.DeadlineExceeded)
				return
			}
			writeOnDemandError(w, bridge.ErrOnDemandUnavailable)
			return
		}

		out := s.fetchFromLinkPeer(ctx, owner.endpoint, peerUUID, libUUID, peerPayload)
		if out.status == http.StatusOK {
			s.setCachedLinkPeer(peerUUID, owner.endpoint, time.Now())
			writeOnDemandProxyResponse(w, out)
			return
		}
		if out.timedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeOnDemandError(w, context.DeadlineExceeded)
			return
		}
		if out.unsupported && owner.verified {
			s.setLinkPeerNegCached(peerUUID, time.Now())
			w.Header().Set("X-RB-Status", "peer_unsupported")
			fail(w, http.StatusNotFound, errors.New("peer does not support link on-demand"))
			return
		}
		if out.rbStatus != "" && out.rbStatus != "not_found" {
			w.Header().Set("X-RB-Status", out.rbStatus)
			fail(w, out.status, errors.New("peer: "+out.rbStatus))
			return
		}
		// Stale cached endpoint or moved stick: clear cache and re-resolve once excluding it.
		s.clearCachedLinkPeer(peerUUID, owner.endpoint)
		if attempt == 0 && excludeEP == "" {
			excludeEP = owner.endpoint
			continue
		}
		if out.rbStatus == "not_found" {
			writeOnDemandError(w, bridge.ErrOnDemandNotFound)
			return
		}
		writeOnDemandError(w, bridge.ErrOnDemandUnavailable)
		return
	}
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
	payload, err := json.Marshal(rbPeerFilledRequest{
		UUID:      libUUID,
		Owner:     peerUUID,
		VolHint:   volHint,
		Path:      req.Path,
		ID:        req.ID,
		ImportKey: req.ImportKey,
	})
	if err == nil {
		s.rbOnDemandWG.Add(1)
		go func(body []byte) {
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
			owner := s.resolveLinkOwner(ctx, peerUUID, libUUID, volHint, "")
			if !owner.verified || owner.endpoint == "" || owner.unsupported || owner.notFound {
				return
			}
			httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.linkPeerURL(owner.endpoint, RekordboxPeerFilledRoute), bytes.NewReader(body))
			if err != nil {
				return
			}
			httpReq.Header.Set("Content-Type", "application/json")
			resp, err := s.rbLinkHTTPClient().Do(httpReq)
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
				resp.Body.Close()
			}
		}(payload)
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

func lookupLiveLibraryUUID(ctx context.Context, mount string) (string, error) {
	mdb := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return "", bridge.ErrOnDemandUnavailable
	}
	defer db.Close()
	var liveUUID string
	if err := db.QueryRowContext(ctx, `SELECT coalesce(uuid, '') FROM Information LIMIT 1`).Scan(&liveUUID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		if errors.Is(err, sql.ErrNoRows) {
			return "", bridge.ErrOnDemandUUIDMismatch
		}
		return "", bridge.ErrOnDemandUnavailable
	}
	return strings.TrimSpace(liveUUID), nil
}

// handlePeerRekordboxOnDemand answers LAN peers on RekordboxPeerOnDemandRoute:
//   - GET  (?uuid=&owner=&vol=): zero-build owner verification returning X-RB-Status: owner + X-RB-UUID (+ X-RB-Owner).
//   - POST (JSON): verified single-flight RBXA1 build keyed strictly by the owner's own m.db row values.
func (s *Server) handlePeerRekordboxOnDemand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.rbLinkBudget())
	defer cancel()
	release, err := s.acquireLANLinkSlot(ctx)
	if err != nil {
		if errors.Is(err, errLinkLANBusy) {
			w.Header().Set("X-RB-Status", "busy")
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		writeOnDemandError(w, err)
		return
	}
	defer release()

	ownLink := s.ownLinkUUID()
	if r.Method == http.MethodGet {
		libUUID := strings.TrimSpace(r.URL.Query().Get("uuid"))
		reqOwner := normalizeLinkUUID(r.URL.Query().Get("owner"))
		volHint := strings.TrimSpace(r.URL.Query().Get("vol"))
		if !validRBStageUUID(libUUID) {
			w.Header().Set("X-RB-Status", "bad_request")
			fail(w, http.StatusBadRequest, errors.New("valid uuid required"))
			return
		}
		if ownLink != "" {
			w.Header().Set("X-RB-Owner", ownLink)
			if reqOwner != "" && reqOwner != ownLink {
				writeOnDemandError(w, bridge.ErrOnDemandNotFound)
				return
			}
		}
		mount, ok := s.findMountedStickByUUID(libUUID, volHint)
		if !ok {
			writeOnDemandError(w, bridge.ErrOnDemandNotFound)
			return
		}
		liveUUID, err := lookupLiveLibraryUUID(ctx, mount)
		if err != nil {
			writeOnDemandError(w, err)
			return
		}
		if liveUUID != libUUID {
			writeOnDemandError(w, bridge.ErrOnDemandUUIDMismatch)
			return
		}
		w.Header().Set("X-RB-Status", "owner")
		w.Header().Set("X-RB-UUID", liveUUID)
		w.WriteHeader(http.StatusOK)
		return
	}

	var req rbPeerOnDemandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, err)
		return
	}
	libUUID := strings.TrimSpace(req.UUID)
	reqOwner := normalizeLinkUUID(req.Owner)
	if !validRBStageUUID(libUUID) || strings.TrimSpace(req.Path) == "" || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("uuid, path, and positive importKey required"))
		return
	}
	if ownLink != "" && reqOwner != "" && reqOwner != ownLink {
		w.Header().Set("X-RB-Owner", ownLink)
		writeOnDemandError(w, bridge.ErrOnDemandNotFound)
		return
	}
	mount, ok := s.findMountedStickByUUID(libUUID, req.VolHint)
	if !ok {
		writeOnDemandError(w, bridge.ErrOnDemandNotFound)
		return
	}

	// Never forward caller-supplied BPM/Key/Length: resolveOnDemandIdentityWithImportKey
	// keys and builds strictly from this owner's own m.db Track row.
	res, err := s.buildOnDemandCachedWithImportKey(ctx, bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       libUUID,
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  -1,
	}, true, req.ImportKey)
	if err != nil {
		writeOnDemandError(w, err)
		return
	}
	writeOnDemandRBXA1(w, res, libUUID, ownLink)
}

func (s *Server) rbStageRoot() (string, error) {
	if p := strings.TrimSpace(s.cfg.RBStageRoot); p != "" {
		return p, nil
	}
	return rbstate.StageRoot()
}

// linkWrittenDigestNoMain mirrors bridge.writtenDigestIgnoringMainCueTail so Unit A
// can verify a committed m.db row even if Engine already rewrote the unadjusted main-cue tail.
func linkWrittenDigestNoMain(beats, cues, loops []byte) []byte {
	h := sha256.New()
	for i, blob := range [][]byte{beats, cues} {
		raw, err := enginedb.QUncompress(blob, 8<<20)
		if err != nil {
			raw = blob
		}
		if i == 1 && len(raw) >= 8 && binary.BigEndian.Uint64(raw) == 8 {
			off := 8
			valid := true
			for j := 0; j < 8; j++ {
				if off >= len(raw) || len(raw)-off-1 < int(raw[off])+12 {
					valid = false
					break
				}
				off += 1 + int(raw[off]) + 12
			}
			if valid && off+17 == len(raw) {
				cp := append([]byte(nil), raw...)
				if cp[off+8] == 0 {
					clear(cp[off : off+8])
				}
				clear(cp[off+9:])
				raw = cp
			}
		}
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(raw)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(raw)
	}
	_, _ = h.Write(loops)
	return h.Sum(nil)[:16]
}

// verifyCommittedLinkFill confirms on Unit A's own m.db that the row has isAnalyzed=1,
// matching pdbImportKey, activeOnLoadLoops, and payload digest before recording filled.jsonl.
func verifyCommittedLinkFill(ctx context.Context, mount, libUUID string, engineID, importKey int64, res *bridge.OnDemandResult) error {
	if res == nil || len(res.Written) == 0 {
		return errLinkFillNotCommitted
	}
	mdb := filepath.Join(mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return bridge.ErrOnDemandUnavailable
	}
	defer db.Close()

	var liveUUID string
	if err := db.QueryRowContext(ctx, `SELECT coalesce(uuid, '') FROM Information LIMIT 1`).Scan(&liveUUID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return bridge.ErrOnDemandUnavailable
	}
	if strings.TrimSpace(liveUUID) != libUUID {
		return bridge.ErrOnDemandUUIDMismatch
	}

	var (
		rowID, rowImportKey int64
		analyzed            int
		activeLoops         uint32
		beats, cues, loops  []byte
	)
	err = db.QueryRowContext(ctx, `
		SELECT t.id, COALESCE(t.pdbImportKey, 0), COALESCE(t.isAnalyzed, 0),
		       p.beatData, p.quickCues, p.loops, COALESCE(p.activeOnLoadLoops, 0)
		FROM Track t
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.id = ?
		LIMIT 1`, engineID).Scan(&rowID, &rowImportKey, &analyzed, &beats, &cues, &loops, &activeLoops)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(err, sql.ErrNoRows) {
			return bridge.ErrOnDemandNotFound
		}
		return bridge.ErrOnDemandUnavailable
	}
	if rowID != engineID || rowImportKey != importKey {
		return bridge.ErrOnDemandNotFound
	}
	if analyzed == 0 || len(beats) == 0 || activeLoops != res.ActiveLoops {
		return errLinkFillNotCommitted
	}
	norm := enginedb.WrittenDigest(beats, cues, loops)
	raw := enginedb.RawWrittenDigest(beats, cues, loops)
	if bytes.Equal(norm, res.Written) || bytes.Equal(raw, res.Written) {
		return nil
	}
	if len(res.WrittenNoMain) > 0 && bytes.Equal(linkWrittenDigestNoMain(beats, cues, loops), res.WrittenNoMain) {
		return nil
	}
	return errLinkFillNotCommitted
}

func filledJournalDedupKey(uuid string, line rbFilledJournalLine) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s",
		uuid, line.ID, line.Key, line.WR, line.WRNM, line.AL, line.V, line.W)
}

func (s *Server) appendRemoteFilledJournal(uuid string, engineID int64, res *bridge.OnDemandResult) (bool, error) {
	if !validRBStageUUID(uuid) || engineID <= 0 || res == nil || res.Key == "" || len(res.Written) == 0 {
		return false, errors.New("invalid filled journal entry")
	}
	root, err := s.rbStageRoot()
	if err != nil {
		return false, err
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
	dedupKey := filledJournalDedupKey(uuid, line)

	s.rbLinkJournalMu.Lock()
	defer s.rbLinkJournalMu.Unlock()
	if s.rbLinkFilledSeen[dedupKey] {
		return false, nil
	}
	dir := filepath.Join(root, uuid)
	jpath := filepath.Join(dir, "filled.jsonl")
	if rf, err := os.Open(jpath); err == nil {
		sc := bufio.NewScanner(rf)
		sc.Buffer(make([]byte, 0, 8<<10), 256<<10)
		for sc.Scan() {
			var existing rbFilledJournalLine
			if json.Unmarshal(sc.Bytes(), &existing) == nil && filledJournalDedupKey(uuid, existing) == dedupKey {
				_ = rf.Close()
				if s.rbLinkFilledSeen == nil {
					s.rbLinkFilledSeen = map[string]bool{}
				}
				s.rbLinkFilledSeen[dedupKey] = true
				return false, nil
			}
		}
		_ = rf.Close()
	}

	raw, err := json.Marshal(line)
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(jpath, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	_, werr := f.Write(raw)
	cerr := f.Close()
	if werr != nil {
		return false, werr
	}
	if cerr != nil {
		return false, cerr
	}
	if s.rbLinkFilledSeen == nil {
		s.rbLinkFilledSeen = map[string]bool{}
	}
	s.rbLinkFilledSeen[dedupKey] = true
	return true, nil
}

// handlePeerRekordboxFilled verifies on this unit's mounted stick that the remote Link fill
// actually committed (isAnalyzed=1 + matching payload digest) and appends once to filled.jsonl.
func (s *Server) handlePeerRekordboxFilled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.rbLinkBudget())
	defer cancel()
	release, err := s.acquireLANLinkSlot(ctx)
	if err != nil {
		if errors.Is(err, errLinkLANBusy) {
			w.Header().Set("X-RB-Status", "busy")
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		writeOnDemandError(w, err)
		return
	}
	defer release()

	var req rbPeerFilledRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, err)
		return
	}
	libUUID := strings.TrimSpace(req.UUID)
	reqOwner := normalizeLinkUUID(req.Owner)
	if !validRBStageUUID(libUUID) || strings.TrimSpace(req.Path) == "" || req.ID <= 0 || req.ImportKey <= 0 {
		w.Header().Set("X-RB-Status", "bad_request")
		fail(w, http.StatusBadRequest, errors.New("uuid, path, id, and positive importKey required"))
		return
	}
	if ownLink := s.ownLinkUUID(); ownLink != "" && reqOwner != "" && reqOwner != ownLink {
		w.Header().Set("X-RB-Owner", ownLink)
		writeOnDemandError(w, bridge.ErrOnDemandNotFound)
		return
	}
	mount, ok := s.findMountedStickByUUID(libUUID, req.VolHint)
	if !ok {
		writeOnDemandError(w, bridge.ErrOnDemandNotFound)
		return
	}
	res, err := s.buildOnDemandCachedWithImportKey(ctx, bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       libUUID,
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  -1,
	}, true, req.ImportKey)
	if err != nil {
		writeOnDemandError(w, err)
		return
	}
	if err := verifyCommittedLinkFill(ctx, mount, libUUID, req.ID, req.ImportKey, res); err != nil {
		writeOnDemandError(w, err)
		return
	}
	appended, err := s.appendRemoteFilledJournal(libUUID, req.ID, res)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if appended {
		s.rbLinkFilledCount.Add(1)
	}
	respond(w, map[string]any{"ok": true, "duplicate": !appended})
}
