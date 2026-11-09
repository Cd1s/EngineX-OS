package webui

// A small mDNS responder for _spotify-connect._tcp.local. The deck's avahi-daemon
// loops on "Host name conflict" when eth0 and wlan0 sit on one subnet and then
// stops advertising, so the deck answers Spotify's queries itself, on every
// interface, and announces every 15 s. No probing, no known-answer suppression.

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	spotifyMDNSService  = "_spotify-connect._tcp.local"
	spotifyMDNSInstance = "EngineX OS"
	spotifyMDNSPort     = 8080
	spotifyMDNSEvery    = 15 * time.Second
)

const mdnsA, mdnsPTR, mdnsTXT, mdnsSRV, mdnsANY = 1, 12, 16, 33, 255

var spotifyMDNSGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

func mdnsLabels(b []byte, labels ...string) []byte {
	for _, l := range labels {
		b = append(append(b, byte(min(len(l), 63))), l[:min(len(l), 63)]...)
	}
	return append(b, 0)
}

// spotifyMDNSHost is the SRV target and A record name: never the system host
// name, which avahi-daemon owns. Every deck is "sc5000", and our cache-flush A
// record for sc5000.local made avahi on both decks loop on "Host name conflict"
// (every 20 s), which also broke Engine Link's discovery. One per deck, by MAC.
func spotifyMDNSHost() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback == 0 && len(i.HardwareAddr) == 6 {
			return fmt.Sprintf("enginex-spotify-%x", []byte(i.HardwareAddr[3:]))
		}
	}
	return "enginex-spotify"
}

// spotifyMDNSWants reports whether msg is a query for our service, instance or host name.
func spotifyMDNSWants(msg []byte, host string) bool {
	if len(msg) < 12 || msg[2]&0x80 != 0 { // too short, or a response
		return false
	}
	names := []string{spotifyMDNSService, spotifyMDNSInstance + "." + spotifyMDNSService, host + ".local"}
	off := 12
	for q := binary.BigEndian.Uint16(msg[4:]); q > 0; q-- {
		name, next, ok := mdnsReadName(msg, off)
		if !ok || next+4 > len(msg) {
			return false
		}
		switch binary.BigEndian.Uint16(msg[next:]) {
		case mdnsA, mdnsPTR, mdnsTXT, mdnsSRV, mdnsANY:
			for _, n := range names {
				if strings.EqualFold(name, n) {
					return true
				}
			}
		}
		off = next + 4
	}
	return false
}

// spotifyMDNSAnswer is the PTR, SRV, TXT and A records, in one multicast response.
func spotifyMDNSAnswer(host string, ip net.IP) []byte {
	service := strings.Split(spotifyMDNSService, ".")
	instance := append([]string{spotifyMDNSInstance}, service...)
	m := []byte{0, 0, 0x84, 0, 0, 0, 0, 0, 0, 0, 0, 0} // QR+AA
	n := uint16(0)
	rr := func(name []byte, typ, class uint16, rdata []byte) {
		n++
		m = append(m, name...)
		m = binary.BigEndian.AppendUint16(m, typ)
		m = binary.BigEndian.AppendUint16(m, class)
		m = binary.BigEndian.AppendUint32(m, 120)
		m = binary.BigEndian.AppendUint16(m, uint16(len(rdata)))
		m = append(m, rdata...)
	}
	rr(mdnsLabels(nil, service...), mdnsPTR, 1, mdnsLabels(nil, instance...))
	rr(mdnsLabels(nil, instance...), mdnsSRV, 0x8001, mdnsLabels(binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0}, spotifyMDNSPort), host, "local"))
	var txt []byte
	for _, s := range []string{"CPath=/spotify-connect", "VERSION=1.0", "Stack=SP"} {
		txt = append(append(txt, byte(len(s))), s...)
	}
	rr(mdnsLabels(nil, instance...), mdnsTXT, 0x8001, txt)
	rr(mdnsLabels(nil, host, "local"), mdnsA, 0x8001, ip.To4())
	binary.BigEndian.PutUint16(m[6:], n)
	return m
}

// mdnsReadName decodes a possibly compressed name; next is the offset after it.
func mdnsReadName(msg []byte, off int) (string, int, bool) {
	var labels []string
	next := -1
	for jumps := 0; jumps < 32; {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, "."), next, true
		case l&0xc0 == 0xc0:
			if off+1 >= len(msg) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3fff)
			jumps++
		case off+1+l > len(msg):
			return "", 0, false
		default:
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}

// serveSpotifyConnectMDNS answers and announces on every up, multicast-capable
// interface with an IPv4 address (re-read each round, so Wi-Fi coming up later is
// picked up) until stop is closed.
func serveSpotifyConnectMDNS(stop <-chan struct{}) {
	host := spotifyMDNSHost()
	conns := map[string]*net.UDPConn{} // by interface and address
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	tick := time.NewTicker(spotifyMDNSEvery)
	defer tick.Stop()
	for {
		want := map[string]net.IP{}
		ifaces, _ := net.Interfaces()
		for _, ifi := range ifaces {
			if ifi.Flags&(net.FlagUp|net.FlagMulticast|net.FlagLoopback) != net.FlagUp|net.FlagMulticast {
				continue
			}
			addrs, _ := ifi.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					want[ifi.Name+"/"+n.IP.String()] = n.IP.To4()
					break
				}
			}
		}
		for key, c := range conns {
			if _, ok := want[key]; !ok {
				c.Close()
				delete(conns, key)
			}
		}
		for key, ip := range want {
			if conns[key] != nil {
				continue
			}
			name, _, _ := strings.Cut(key, "/")
			ifi, err := net.InterfaceByName(name)
			if err != nil {
				continue
			}
			c, err := net.ListenMulticastUDP("udp4", ifi, spotifyMDNSGroup)
			if err != nil {
				continue
			}
			conns[key] = c
			go func() {
				buf := make([]byte, 9000)
				for {
					n, _, err := c.ReadFromUDP(buf)
					if err != nil {
						return
					}
					if spotifyMDNSWants(buf[:n], host) {
						c.WriteToUDP(spotifyMDNSAnswer(host, ip), spotifyMDNSGroup)
					}
				}
			}()
		}
		for key, c := range conns {
			c.WriteToUDP(spotifyMDNSAnswer(host, want[key]), spotifyMDNSGroup)
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
