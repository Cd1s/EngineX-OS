package stems

// Zero-config home renderer. sc5000-stemsd on a computer in the deck's LAN
// answers a UDP broadcast probe and mDNS (mdns.go); a deck with no relay (or an unreachable one)
// offloads Stems there and renders locally when nothing answers in time.
// Discovered renderers never receive the deck's relay password: a LAN
// renderer serves without a credential (Backend with an empty Secret).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
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
	Checked  time.Time `json:"checked"`
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

// discoverTargets is where probes go; tests point it at loopback.
var discoverTargets = broadcastTargets

// broadcastTargets: the limited broadcast plus each IPv4 interface's directed
// broadcast, so a deck with several interfaces probes every LAN.
func broadcastTargets() []*net.UDPAddr {
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

// DiscoverLANServer probes the LAN once and returns the base URL of the first
// renderer that answers within timeout.
func DiscoverLANServer(ctx context.Context, timeout time.Duration) (string, Beacon, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return "", Beacon{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	conn.SetDeadline(deadline)
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })()
	for _, t := range discoverTargets() {
		conn.WriteToUDP([]byte(discoverProbe), t)
	}
	query := append([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, mdnsQuestion()...)
	for _, t := range mdnsTargets() {
		conn.WriteToUDP(query, t)
	}
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if ctx.Err() != nil || time.Now().After(deadline) || (errors.As(err, &ne) && ne.Timeout()) {
				return "", Beacon{}, err
			}
			continue
		}
		var b Beacon
		if json.Unmarshal(buf[:n], &b) != nil {
			b, _ = parseMDNS(buf[:n])
		}
		if b.Service != beaconService || b.Port < 1 || b.Port > 65535 || !lanIP(from.IP) {
			continue
		}
		return "http://" + net.JoinHostPort(from.IP.String(), strconv.Itoa(b.Port)), b, nil
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
	st, ok := LANState{}, false
	if last.Endpoint != "" && validEndpoint(last.Endpoint) {
		st, ok = lanHealth(ctx, last.Endpoint, timeout)
	}
	if !ok {
		if endpoint, _, err := DiscoverLANServer(ctx, timeout); err == nil {
			st, ok = lanHealth(ctx, endpoint, timeout)
		}
	}
	if !ok {
		st = last // keep the last renderer for the console and the next quick check
	}
	st.Online, st.Checked = ok, time.Now().UTC()
	if b, err := json.Marshal(st); err == nil && os.WriteFile(statePath+".tmp", b, 0o600) == nil {
		os.Rename(statePath+".tmp", statePath)
	}
	return st.Endpoint, ok
}

// lanHealth checks a renderer that needs no credential and has a free slot.
func lanHealth(ctx context.Context, endpoint string, timeout time.Duration) (LANState, bool) {
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
		OK      bool   `json:"ok"`
		Backend string `json:"backend"`
		Device  string `json:"device"`
		Busy    bool   `json:"busy"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h) != nil || !h.OK || h.Busy {
		return LANState{}, false
	}
	return LANState{Endpoint: endpoint, Hostname: h.Device, Backend: h.Backend}, true
}
