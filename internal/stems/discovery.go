package stems

// Zero-config home renderer. sc5000-stemsd on a computer in the deck's LAN
// answers a UDP broadcast probe and mDNS (mdns.go); a deck with no relay (or an unreachable one)
// offloads Stems there and renders locally when nothing answers in time.
// Discovered renderers never receive the deck's relay password: a LAN
// renderer serves without a credential (Backend with an empty Secret).

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DiscoveryPort is the UDP port sc5000-stemsd answers probes on.
	DiscoveryPort = 17891
	discoverProbe = "ENGINEX_STEMS_DISCOVER_V1"
	beaconService = "enginex-stemsd"
	// LANStateName is the file, next to deemix-config.json, where
	// stems-processor records the home renderer it last checked.
	LANStateName    = "stems-lan.json"
	DefaultLANState = "/data/sc5000-custom/state/" + LANStateName
)

// Beacon is a renderer's answer to a discovery probe.
type Beacon struct {
	Service  string `json:"service"`
	Version  int    `json:"version"`
	Port     int    `json:"port"`
	Backend  string `json:"backend"`
	Hostname string `json:"hostname"`
}

// LANState is what the deck console shows about the home renderer.
type LANState struct {
	Endpoint string    `json:"endpoint,omitempty"`
	Hostname string    `json:"hostname,omitempty"`
	Backend  string    `json:"backend,omitempty"`
	Online   bool      `json:"online"`
	Relay    bool      `json:"relay,omitempty"`
	Checked  time.Time `json:"checked"`
}

// lanEndpoint reports whether raw is a valid HTTP(S) URL whose host is a LAN
// address (or .local/.lan mDNS name) suitable for passwordless LAN Stems.
func lanEndpoint(raw string) bool {
	if !validEndpoint(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return lanIP(ip)
	}
	lh := strings.ToLower(strings.TrimSuffix(host, "."))
	return lh != "" && (!strings.Contains(lh, ".") || strings.HasSuffix(lh, ".local") || strings.HasSuffix(lh, ".lan"))
}

func configuredRelayEndpoint(statePath string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(statePath), "deemix-config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		RelayURL string `json:"relayUrl"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(cfg.RelayURL), "/")
}

func sameEndpoint(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
}

// lanIP: addresses a home renderer or deck can have (private, loopback,
// link-local, tailnet CGNAT).
func lanIP(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip))
}

// Announce answers discovery probes on conn until ctx ends: the UDP 17891
// probe with JSON, and mDNS queries for _enginex-stems._tcp.local. (mdns.go).
// Only LAN senders get a reply, so a UDP port open to the internet cannot
// reflect traffic.
func Announce(ctx context.Context, conn net.PacketConn, b Beacon) error {
	b.Service, b.Version = beaconService, 2
	reply, _ := json.Marshal(b)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok && (ua.Port == DiscoveryPort || ua.Port == mdnsPort) {
		beaconStop := make(chan struct{})
		defer close(beaconStop)
		go func() {
			emit := func() {
				if ua.Port == DiscoveryPort {
					for _, t := range broadcastOnlyTargets() {
						conn.WriteTo(reply, t)
					}
				} else {
					for _, ifIP := range localLANIPv4s() {
						conn.WriteTo(mdnsAnswer(0, false, b, ifIP), MDNSGroup)
					}
				}
			}
			emit()
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-beaconStop:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					emit()
				}
			}
		}()
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		u, ok := from.(*net.UDPAddr)
		if !ok || !lanIP(u.IP) {
			continue
		}
		if string(buf[:n]) == discoverProbe {
			conn.WriteTo(reply, from)
		} else if id, ok := mdnsQuery(buf[:n]); ok {
			// Port 5353 means a full mDNS querier: answer the group. Others are legacy unicast.
			legacy, to := u.Port != mdnsPort, net.Addr(MDNSGroup)
			if legacy {
				to = from
			}
			conn.WriteTo(mdnsAnswer(id, legacy, b, localIPFor(u.IP)), to)
		}
	}
}

var (
	arpTablePath = "/proc/net/arp"
	peerHintsMu  sync.Mutex
	peerHints    []net.IP
)

// RememberPeerHint records a recent LAN client IPv4 address (for example a PC
// browser opening the deck Web Console) so unicast UDP discovery can reach it
// even when Wi-Fi AP client isolation drops broadcast and multicast frames.
func RememberPeerHint(hostOrAddr string) {
	host := strings.TrimSpace(hostOrAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return
	}
	ip = ip.To4()
	if ip == nil || ip.IsLoopback() || !lanIP(ip) {
		return
	}
	peerHintsMu.Lock()
	defer peerHintsMu.Unlock()
	for i, existing := range peerHints {
		if existing.Equal(ip) {
			peerHints = append(append(peerHints[:i], peerHints[i+1:]...), ip)
			return
		}
	}
	if len(peerHints) >= 16 {
		peerHints = peerHints[1:]
	}
	peerHints = append(peerHints, append(net.IP(nil), ip...))
}

func localLANIPv4Nets() []*net.IPNet {
	var out []*net.IPNet
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && lanIP(n.IP) {
				out = append(out, &net.IPNet{IP: n.IP.To4(), Mask: n.Mask})
			}
		}
	}
	return out
}

func localLANIPv4s() []net.IP {
	nets := localLANIPv4Nets()
	out := make([]net.IP, len(nets))
	for i, n := range nets {
		out[i] = n.IP
	}
	return out
}

func unicastPeerTargets() []*net.UDPAddr {
	localSet := map[string]bool{}
	for _, ip := range localLANIPv4s() {
		localSet[ip.String()] = true
	}
	seen := map[string]bool{}
	var out []*net.UDPAddr
	add := func(ip net.IP) {
		ip4 := ip.To4()
		if ip4 == nil || !lanIP(ip4) || localSet[ip4.String()] || seen[ip4.String()] || len(out) >= 32 {
			return
		}
		seen[ip4.String()] = true
		out = append(out, &net.UDPAddr{IP: append(net.IP(nil), ip4...), Port: DiscoveryPort})
	}
	peerHintsMu.Lock()
	for i := len(peerHints) - 1; i >= 0; i-- {
		add(peerHints[i])
	}
	peerHintsMu.Unlock()
	if raw, err := os.ReadFile(arpTablePath); err == nil {
		for idx, line := range strings.Split(string(raw), "\n") {
			if idx == 0 {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[2] == "0x0" {
				continue
			}
			if ip := net.ParseIP(fields[0]); ip != nil {
				add(ip)
			}
		}
	}
	return out
}

func broadcastOnlyTargets() []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: DiscoveryPort}}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			if len(n.Mask) < 4 {
				continue // point-to-point / unnumbered interfaces have no broadcast mask (#40)
			}
			ip, mask, b := n.IP.To4(), n.Mask[len(n.Mask)-4:], make(net.IP, 4)
			for k := range b {
				b[k] = ip[k] | ^mask[k]
			}
			targets = append(targets, &net.UDPAddr{IP: b, Port: DiscoveryPort})
		}
	}
	return targets
}

// discoverTargets is where probes go; tests point it at loopback.
var discoverTargets = broadcastTargets

// broadcastTargets: the limited broadcast, each IPv4 interface's directed
// broadcast, plus known ARP/peer-hint unicast targets so Wi-Fi AP broadcast
// isolation does not block discovery.
func broadcastTargets() []*net.UDPAddr {
	return append(broadcastOnlyTargets(), unicastPeerTargets()...)
}

// DiscoverLANServer probes the LAN once and returns the base URL of the first
// renderer that answers within timeout.
func DiscoverLANServer(ctx context.Context, timeout time.Duration) (string, Beacon, error) {
	var gotEndpoint string
	var gotBeacon Beacon
	err := discoverLANServers(ctx, timeout, func(endpoint string, b Beacon) bool {
		gotEndpoint, gotBeacon = endpoint, b
		return true
	})
	if err != nil {
		return "", Beacon{}, err
	}
	return gotEndpoint, gotBeacon, nil
}

func validMDNSRecordIP(mdnsIP, fromIP net.IP, localNets []*net.IPNet) bool {
	ip4 := mdnsIP.To4()
	if ip4 == nil || ip4.IsLoopback() || !lanIP(ip4) {
		return false
	}
	if from4 := fromIP.To4(); from4 != nil && ip4.Equal(from4) {
		return true
	}
	for _, n := range localNets {
		if n != nil && !n.IP.IsLoopback() && n.Contains(ip4) {
			return true
		}
	}
	return false
}

func discoverLANServers(ctx context.Context, timeout time.Duration, accept func(endpoint string, b Beacon) bool) error {
	primary, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return err
	}
	type probeSock struct {
		conn *net.UDPConn
		net  *net.IPNet // nil on primary wildcard socket
	}
	socks := []probeSock{{conn: primary}}
	localNets := localLANIPv4Nets()
	for _, ifNet := range localNets {
		if len(socks) >= 8 {
			break
		}
		if c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: ifNet.IP, Port: 0}); e == nil {
			socks = append(socks, probeSock{conn: c, net: ifNet})
		}
	}
	closeAll := func() {
		for _, s := range socks {
			s.conn.Close()
		}
	}
	defer closeAll()
	deadline := time.Now().Add(timeout)
	for _, s := range socks {
		s.conn.SetDeadline(deadline)
	}
	defer context.AfterFunc(ctx, func() {
		now := time.Now()
		for _, s := range socks {
			s.conn.SetDeadline(now)
		}
	})()

	canSendOn := func(s probeSock, dst *net.UDPAddr) bool {
		if dst == nil {
			return false
		}
		if s.net == nil {
			return true
		}
		return dst.IP.Equal(net.IPv4bcast) || dst.IP.IsMulticast() || s.net.Contains(dst.IP)
	}

	query := append([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, mdnsQuestion()...)
	dTargets := discoverTargets()
	mTargets := mdnsTargets()
	sendProbes := func() {
		for _, s := range socks {
			for _, t := range dTargets {
				if canSendOn(s, t) {
					s.conn.WriteToUDP([]byte(discoverProbe), t)
				}
			}
			for _, t := range mTargets {
				if canSendOn(s, t) {
					s.conn.WriteToUDP(query, t)
				}
			}
		}
	}
	sendProbes()

	retryStop := make(chan struct{})
	var retryWG sync.WaitGroup
	defer func() {
		close(retryStop)
		retryWG.Wait()
	}()
	if timeout >= 40*time.Millisecond {
		retryWG.Add(1)
		go func() {
			defer retryWG.Done()
			timer := time.NewTimer(timeout / 3)
			defer timer.Stop()
			select {
			case <-retryStop:
			case <-ctx.Done():
			case <-timer.C:
				sendProbes()
			}
		}()
	}

	type rxPkt struct {
		data []byte
		from *net.UDPAddr
	}
	pktCh := make(chan rxPkt, 16)
	errCh := make(chan error, len(socks))
	var wg sync.WaitGroup
	for _, s := range socks {
		wg.Add(1)
		go func(conn *net.UDPConn) {
			defer wg.Done()
			buf := make([]byte, 1500)
			for {
				n, from, rerr := conn.ReadFromUDP(buf)
				if rerr != nil {
					errCh <- rerr
					return
				}
				pkt := make([]byte, n)
				copy(pkt, buf[:n])
				select {
				case pktCh <- rxPkt{data: pkt, from: from}:
				case <-retryStop:
					return
				}
			}
		}(s.conn)
	}
	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	seen := map[string]bool{}
	var lastErr error
	for {
		select {
		case pkt := <-pktCh:
			var b Beacon
			var mdnsIP net.IP
			if json.Unmarshal(pkt.data, &b) != nil {
				b, mdnsIP, _ = parseMDNSWithIP(pkt.data)
			}
			peerIP := pkt.from.IP
			if validMDNSRecordIP(mdnsIP, pkt.from.IP, localNets) {
				peerIP = mdnsIP
			}
			if b.Service != beaconService || b.Port < 1 || b.Port > 65535 || !lanIP(peerIP) {
				continue
			}
			endpoint := "http://" + net.JoinHostPort(peerIP.String(), strconv.Itoa(b.Port))
			if seen[endpoint] {
				continue
			}
			seen[endpoint] = true
			if accept(endpoint, b) {
				return nil
			}
		case <-doneCh:
			select {
			case lastErr = <-errCh:
			default:
				lastErr = net.ErrClosed
			}
			return lastErr
		}
	}
}

// FindLAN returns a home renderer that is up and free: first the one recorded
// in statePath if its /healthz still answers, else whatever answers a probe.
// It records the outcome in statePath and spends at most about 3×timeout.
func FindLAN(ctx context.Context, statePath string, timeout time.Duration) (string, bool) {
	var last LANState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &last)
	}
	relayCfg := configuredRelayEndpoint(statePath)
	isRelay := last.Relay || (relayCfg != "" && last.Endpoint != "" && sameEndpoint(last.Endpoint, relayCfg))
	st, ok := LANState{}, false
	if !isRelay && last.Endpoint != "" && lanEndpoint(last.Endpoint) {
		st, ok = lanHealth(ctx, last.Endpoint, timeout)
	}
	if !ok {
		_ = discoverLANServers(ctx, timeout, func(endpoint string, _ Beacon) bool {
			if relayCfg != "" && sameEndpoint(endpoint, relayCfg) {
				return false
			}
			st, ok = lanHealth(ctx, endpoint, timeout)
			return ok
		})
	}
	if !ok {
		if isRelay || (last.Endpoint != "" && !lanEndpoint(last.Endpoint)) {
			return "", false
		}
		st = last // keep the last renderer for the console and the next quick check
	}
	st.Online, st.Checked = ok, time.Now().UTC()
	if ok || st.Endpoint != "" {
		if b, err := json.Marshal(st); err == nil && os.WriteFile(statePath+".tmp", b, 0o600) == nil {
			os.Rename(statePath+".tmp", statePath)
		}
	}
	return st.Endpoint, ok
}

// ProbeLANState discovers or refreshes the LAN Stems renderer for the deck UI
// and Web Console when no online cloud-relay Stems worker is active.
func ProbeLANState(ctx context.Context, statePath string, timeout time.Duration) *LANState {
	FindLAN(ctx, statePath, timeout)
	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil
	}
	var st LANState
	if json.Unmarshal(b, &st) != nil || (st.Endpoint == "" && !st.Online) {
		return nil
	}
	return &st
}

// lanHealth checks a renderer that needs no credential and has a free slot.
func lanHealth(ctx context.Context, endpoint string, timeout time.Duration) (LANState, bool) {
	if !lanEndpoint(endpoint) {
		return LANState{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/healthz", nil)
	if err != nil {
		return LANState{}, false
	}
	// One-shot probe: no keep-alive, so no idle socket outlives it (#42).
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return LANState{}, false
	}
	defer resp.Body.Close()
	var h struct {
		OK       bool   `json:"ok"`
		Protocol string `json:"protocol"`
		Backend  string `json:"backend"`
		Device   string `json:"device"`
		Busy     bool   `json:"busy"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h) != nil || !h.OK || h.Protocol != Protocol || h.Busy {
		return LANState{}, false
	}
	return LANState{Endpoint: endpoint, Hostname: h.Device, Backend: h.Backend}, true
}
