package stems

// Minimal mDNS (RFC 6762/6763) for the home renderer: it answers PTR/ANY
// queries for _enginex-stems._tcp.local. with PTR, SRV, TXT and A records,
// so DNS-SD browsers (dns-sd -B, avahi-browse) list it too, and the deck
// sends the same query next to its UDP 17891 probe. Only what that needs is
// parsed: no probing/conflict resolution, no known-answer suppression.

import (
	"encoding/binary"
	"net"
	"strconv"
	"strings"
)

const (
	mdnsPort    = 5353
	mdnsService = "_enginex-stems._tcp.local"
)

const typeA, typePTR, typeTXT, typeSRV, typeANY = 1, 12, 16, 33, 255

// MDNSGroup is the IPv4 mDNS multicast address sc5000-stemsd joins.
var MDNSGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// mdnsTargets is where the deck's mDNS query goes; tests point it at loopback.
var mdnsTargets = func() []*net.UDPAddr { return []*net.UDPAddr{MDNSGroup} }

func appendName(b []byte, labels ...string) []byte {
	for _, l := range labels {
		if len(l) > 63 {
			l = l[:63]
		}
		b = append(append(b, byte(len(l))), l...)
	}
	return append(b, 0)
}

var serviceLabels = strings.Split(mdnsService, ".")

// mdnsQuestion is a PTR query for the service in wire form.
func mdnsQuestion() []byte {
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(appendName(nil, serviceLabels...), typePTR), 1)
}

// mdnsQuery returns the query's id when msg asks for our service.
func mdnsQuery(msg []byte) (uint16, bool) {
	if len(msg) < 12 || msg[2]&0x80 != 0 { // a response
		return 0, false
	}
	off := 12
	for q := binary.BigEndian.Uint16(msg[4:]); q > 0; q-- {
		name, next, ok := readName(msg, off)
		if !ok || next+4 > len(msg) {
			return 0, false
		}
		t := binary.BigEndian.Uint16(msg[next:])
		if strings.EqualFold(name, mdnsService) && (t == typePTR || t == typeANY) {
			return binary.BigEndian.Uint16(msg), true
		}
		off = next + 4
	}
	return 0, false
}

// mdnsAnswer builds the response. A legacy unicast query (not from port
// 5353) gets its id and question echoed back, as RFC 6762 §6.7 requires.
func mdnsAnswer(id uint16, legacy bool, b Beacon, ip net.IP) []byte {
	instance := cleanLabel(b.Hostname)
	host := strings.SplitN(instance, ".", 2)[0]
	inst := append([]string{instance}, serviceLabels...)
	m := []byte{0, 0, 0x84, 0, 0, 0, 0, 0, 0, 0, 0, 0} // QR+AA
	if legacy {
		binary.BigEndian.PutUint16(m, id)
		m[5] = 1
		m = append(m, mdnsQuestion()...)
	}
	answers := uint16(0)
	rr := func(name []byte, typ, class uint16, rdata []byte) {
		answers++
		m = append(m, name...)
		m = binary.BigEndian.AppendUint16(m, typ)
		m = binary.BigEndian.AppendUint16(m, class)
		m = binary.BigEndian.AppendUint32(m, 120)
		m = binary.BigEndian.AppendUint16(m, uint16(len(rdata)))
		m = append(m, rdata...)
	}
	rr(appendName(nil, serviceLabels...), typePTR, 1, appendName(nil, inst...))
	srv := binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0}, uint16(b.Port))
	rr(appendName(nil, inst...), typeSRV, 0x8001, appendName(srv, host, "local"))
	var txt []byte
	for _, s := range []string{"service=" + beaconService, "version=" + strconv.Itoa(b.Version), "backend=" + b.Backend, "host=" + b.Hostname} {
		s = s[:min(len(s), 255)]
		txt = append(append(txt, byte(len(s))), s...)
	}
	rr(appendName(nil, inst...), typeTXT, 0x8001, txt)
	if ip.To4() != nil {
		rr(appendName(nil, host, "local"), typeA, 0x8001, ip.To4())
	}
	binary.BigEndian.PutUint16(m[6:], answers)
	return m
}

// parseMDNS reads a renderer's beacon out of an mDNS response for our service.
func parseMDNS(msg []byte) (Beacon, bool) {
	b, _, ok := parseMDNSWithIP(msg)
	return b, ok
}

// parseMDNSWithIP also extracts any IPv4 A record carried in the mDNS response,
// so reflected or multi-NIC mDNS answers resolve to the advertised listener IP.
func parseMDNSWithIP(msg []byte) (Beacon, net.IP, bool) {
	if len(msg) < 12 || msg[2]&0x80 == 0 {
		return Beacon{}, nil, false
	}
	off := 12
	for q := binary.BigEndian.Uint16(msg[4:]); q > 0; q-- {
		_, next, ok := readName(msg, off)
		if !ok {
			return Beacon{}, nil, false
		}
		off = next + 4
	}
	var b Beacon
	var srvTarget string
	var firstA, targetA net.IP
	ptr := false
	records := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	for ; records > 0; records-- {
		name, next, ok := readName(msg, off)
		if !ok || next+10 > len(msg) {
			return Beacon{}, nil, false
		}
		typ, size := binary.BigEndian.Uint16(msg[next:]), int(binary.BigEndian.Uint16(msg[next+8:]))
		data := next + 10
		if data+size > len(msg) {
			return Beacon{}, nil, false
		}
		rdata := msg[data : data+size]
		switch {
		case typ == typePTR && strings.EqualFold(name, mdnsService):
			ptr = true
		case typ == typeSRV && size >= 6 && strings.HasSuffix(strings.ToLower(name), "."+mdnsService):
			b.Port = int(binary.BigEndian.Uint16(rdata[4:]))
			if target, _, ok := readName(msg, data+6); ok {
				srvTarget = target
			}
		case typ == typeTXT && strings.HasSuffix(strings.ToLower(name), "."+mdnsService):
			for i := 0; i < len(rdata) && i+1+int(rdata[i]) <= len(rdata); i += 1 + int(rdata[i]) {
				k, v, _ := strings.Cut(string(rdata[i+1:i+1+int(rdata[i])]), "=")
				switch k {
				case "backend":
					b.Backend = v
				case "host":
					b.Hostname = v
				case "version":
					b.Version, _ = strconv.Atoi(v)
				}
			}
		case typ == typeA && size == 4:
			ip := net.IPv4(rdata[0], rdata[1], rdata[2], rdata[3])
			if firstA == nil {
				firstA = ip
			}
			if srvTarget != "" && strings.EqualFold(name, srvTarget) {
				targetA = ip
			}
		}
		off = data + size
	}
	if !ptr || b.Port == 0 {
		return Beacon{}, nil, false
	}
	b.Service = beaconService
	chosenIP := targetA
	if chosenIP == nil {
		chosenIP = firstA
	}
	return b, chosenIP, true
}

// readName decodes a possibly compressed name; next is the offset after it
// where it started.
func readName(msg []byte, off int) (string, int, bool) {
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

// cleanLabel keeps a hostname usable as a DNS-SD instance label.
func cleanLabel(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".local")
	if s == "" {
		return "enginex-stems"
	}
	return s
}

// localIPFor is this host's address on the subnet peer is on (the A record).
func localIPFor(peer net.IP) net.IP {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.Contains(peer) {
			return n.IP.To4()
		}
	}
	return nil
}
