package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/unix"
)

const (
	prolinkMagic = "Qspt1WmJOL"
	portAnnounce = 50000
	portStatus   = 50002
	portDBQuery  = 12523
	portmapRB    = 50111
	portmapStd   = 111

	rpcPortmap = 100000
	rpcMount   = 100005
	rpcNFS     = 100003
	fhSize     = 32

	kindSetup       = 0x0000
	kindRootMenu    = 0x1000
	kindAllTracks   = 0x1004
	kindPlaylist    = 0x1105
	kindMetadata    = 0x2002
	kindArtwork     = 0x2003
	kindWavePrev    = 0x2004
	kindTrackInfo   = 0x2102
	kindCueList     = 0x2104
	kindBeatGrid    = 0x2204
	kindWaveDetail  = 0x2904
	kindCueListExt  = 0x2b04
	kindANLZTag     = 0x2c04
	kindRender      = 0x3000
	kindSetupAck    = 0x4000
	kindRenderHdr   = 0x4001
	kindArtResp     = 0x4002
	kindUnavailable = 0x4003
	kindRenderItem  = 0x4101
	kindRenderFoot  = 0x4201
)

var dbMagic = []byte{0x11, 0x87, 0x23, 0x49, 0xae}

func main() {
	asPlayer := flag.Int("as", 1, "asking player number (1-4) for main dbserver browse")
	maxTracks := flag.Int("tracks", 5, "max sample tracks to inspect")
	specificTrack := flag.Uint("track", 0, "optional specific track ID to include in deep probe")
	listenDur := flag.Duration("listen", 2500*time.Millisecond, "passive UDP 50000/50002 listen duration (0 to skip)")
	srcPort := flag.Int("src", 0, "optional local UDP source port for RPC/MOUNT/NFS calls (0 = ephemeral; <1024 requires root)")
	wake := flag.Bool("wake", false, "opt-in: send virtual CDJ-3000 keepalive (0x06) to target:50000 and wait up to 10s for LINK")
	timeout := flag.Duration("timeout", 3*time.Second, "per-request network timeout")
	flag.Parse()

	if *asPlayer < 1 || *asPlayer > 255 {
		fmt.Fprintf(os.Stderr, "invalid -as player number %d\n", *asPlayer)
		os.Exit(2)
	}
	if *srcPort < 0 || *srcPort > 65535 {
		fmt.Fprintf(os.Stderr, "invalid -src port %d\n", *srcPort)
		os.Exit(2)
	}

	red := newRedactor()
	targets, ifNotes := discoverTargets(flag.Args(), red)

	fmt.Println("=== rbx-probe (read-only) ===")
	fmt.Printf("time: %s\n", time.Now().UTC().Format(time.RFC3339))
	for _, n := range ifNotes {
		fmt.Printf("iface: %s\n", n)
	}
	targetLabels := make([]string, len(targets))
	for i, ip := range targets {
		targetLabels[i] = red.ip(ip)
	}
	fmt.Printf("targets: %s\n", strings.Join(targetLabels, ", "))
	if *srcPort > 0 {
		fmt.Printf("rpc_src_port: %d\n", *srcPort)
	}

	if *wake {
		stopWake := wakeTargets(targets, byte(*asPlayer), red)
		defer stopWake()
	}

	var seenNums map[string]byte
	if *listenDur > 0 {
		seenNums = probeUDPPassive(targets, *listenDur, byte(*asPlayer), red)
	}

	for _, ip := range targets {
		fmt.Printf("\n========================================\n")
		fmt.Printf("TARGET: %s\n", red.ip(ip))
		fmt.Printf("========================================\n")
		paths := probeDBServer(ip, byte(*asPlayer), *maxTracks, uint32(*specificTrack), *timeout, seenNums[ip.String()] == 0, red)
		probeNFS(ip, paths, *srcPort, *timeout, red)
	}
}

// redactor masks usernames, home directories, hostname, LAN IPs, MACs, and personal filenames.
type redactor struct {
	mu      sync.Mutex
	user    string
	home    string
	host    string
	ips     map[string]string
	macs    map[string]string
	dirs    map[string]string
	files   map[string]string
	allowed map[string]bool
}

func newRedactor() *redactor {
	uName := os.Getenv("USER")
	if cur, err := user.Current(); err == nil && cur.Username != "" {
		uName = cur.Username
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	if idx := strings.IndexByte(host, '.'); idx > 0 {
		host = host[:idx]
	}
	return &redactor{
		user:  uName,
		home:  home,
		host:  host,
		ips:   map[string]string{},
		macs:  map[string]string{},
		dirs:  map[string]string{},
		files: map[string]string{},
		allowed: map[string]bool{
			".": true, "..": true,
			"Users": true, "home": true, "Volumes": true, "Music": true,
			"rekordbox": true, "PioneerDJ": true, "PIONEER": true,
			"USBANLZ": true, "Contents": true, "Artwork": true,
			"Desktop": true, "Documents": true, "Downloads": true,
			"Library": true, "Application Support": true,
		},
	}
}

func (r *redactor) ip(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return "<NON_V4>"
	}
	s := v4.String()
	if s == "127.0.0.1" {
		return "127.0.0.1"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if label, ok := r.ips[s]; ok {
		return label
	}
	label := fmt.Sprintf("<LAN_IP_%d>", len(r.ips)+1)
	r.ips[s] = label
	return label
}

func (r *redactor) mac(b []byte) string {
	if len(b) != 6 {
		return "<MAC_INVALID>"
	}
	s := net.HardwareAddr(b).String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if label, ok := r.macs[s]; ok {
		return label
	}
	label := fmt.Sprintf("<MAC_%d>", len(r.macs)+1)
	r.macs[s] = label
	return label
}

func (r *redactor) sanitizeGeneric(s string) string {
	if r.home != "" && len(r.home) >= 3 {
		s = strings.ReplaceAll(s, r.home, "/Users/<USER>")
	}
	if r.user != "" && len(r.user) >= 2 {
		s = replaceCaseInsensitive(s, r.user, "<USER>")
	}
	if r.host != "" && len(r.host) >= 3 {
		s = replaceCaseInsensitive(s, r.host, "<HOST>")
	}
	return s
}

func (r *redactor) text(s string) string {
	r.mu.Lock()
	ips := make(map[string]string, len(r.ips))
	for k, v := range r.ips {
		ips[k] = v
	}
	r.mu.Unlock()
	for rawIP, label := range ips {
		s = strings.ReplaceAll(s, rawIP, label)
	}
	return r.sanitizeGeneric(s)
}

func (r *redactor) err(err error) string {
	if err == nil {
		return "<nil>"
	}
	return r.text(err.Error())
}

func replaceCaseInsensitive(s, old, repl string) string {
	if old == "" {
		return s
	}
	lo := strings.ToLower(s)
	lold := strings.ToLower(old)
	var out strings.Builder
	i := 0
	for {
		idx := strings.Index(lo[i:], lold)
		if idx < 0 {
			out.WriteString(s[i:])
			break
		}
		out.WriteString(s[i : i+idx])
		out.WriteString(repl)
		i += idx + len(lold)
	}
	return out.String()
}

func (r *redactor) path(raw string) string {
	if raw == "" {
		return ""
	}
	sep := "/"
	norm := raw
	if strings.Contains(raw, `\`) && !strings.Contains(raw, "/") {
		sep = `\`
		norm = strings.ReplaceAll(raw, `\`, "/")
	}
	hasLead := strings.HasPrefix(norm, "/")
	hasTrail := len(norm) > 1 && strings.HasSuffix(norm, "/")
	parts := strings.Split(strings.Trim(norm, "/"), "/")
	out := make([]string, 0, len(parts))
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		isLast := i == len(parts)-1 && !hasTrail
		// Mask OS username right after Users / home.
		if i > 0 && (strings.EqualFold(parts[i-1], "Users") || strings.EqualFold(parts[i-1], "home")) {
			out = append(out, "<USER>")
			continue
		}
		// Keep drive prefix like C:
		if i == 0 && len(seg) == 2 && seg[1] == ':' {
			out = append(out, seg)
			continue
		}
		if r.allowed[seg] || (r.user != "" && strings.EqualFold(seg, r.user)) {
			if r.user != "" && strings.EqualFold(seg, r.user) {
				out = append(out, "<USER>")
			} else {
				out = append(out, seg)
			}
			continue
		}
		if isLast {
			ext := filepath.Ext(seg)
			if label, ok := r.files[seg]; ok {
				out = append(out, label)
			} else {
				label := fmt.Sprintf("<FILE_%d%s>", len(r.files)+1, ext)
				r.files[seg] = label
				out = append(out, label)
			}
			continue
		}
		if label, ok := r.dirs[seg]; ok {
			out = append(out, label)
		} else {
			label := fmt.Sprintf("<DIR_%d>", len(r.dirs)+1)
			r.dirs[seg] = label
			out = append(out, label)
		}
	}
	res := strings.Join(out, sep)
	if hasLead {
		res = sep + res
	}
	if hasTrail && res != sep {
		res += sep
	}
	return r.sanitizeGeneric(res)
}

func pathTraits(s string) string {
	hasSpace := strings.ContainsRune(s, ' ')
	nonASCII := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			nonASCII = true
			break
		}
	}
	return fmt.Sprintf("len=%d spaces=%v non_ascii=%v", len(s), hasSpace, nonASCII)
}

func discoverTargets(args []string, red *redactor) ([]net.IP, []string) {
	var notes []string
	if len(args) > 0 {
		var out []net.IP
		for _, a := range args {
			ip := net.ParseIP(strings.TrimSpace(a)).To4()
			if ip == nil {
				notes = append(notes, fmt.Sprintf("ignored non-IPv4 arg: %q", red.sanitizeGeneric(a)))
				continue
			}
			out = append(out, ip)
		}
		return out, notes
	}

	out := []net.IP{net.IPv4(127, 0, 0, 1)}
	seen := map[string]bool{"127.0.0.1": true}
	ifaces, err := net.Interfaces()
	if err != nil {
		notes = append(notes, fmt.Sprintf("net.Interfaces error: %v", err))
		return out, notes
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip tunnel/virtual interfaces by default to avoid probing Tailscale/VPNs.
		if strings.HasPrefix(ifc.Name, "utun") || strings.HasPrefix(ifc.Name, "awdl") ||
			strings.HasPrefix(ifc.Name, "llw") || strings.HasPrefix(ifc.Name, "feth") ||
			strings.HasPrefix(ifc.Name, "gif") || strings.HasPrefix(ifc.Name, "stf") ||
			strings.HasPrefix(ifc.Name, "anpi") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			v4 := ipNet.IP.To4()
			if v4 == nil || v4.IsLinkLocalUnicast() || !v4.IsPrivate() {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			notes = append(notes, fmt.Sprintf("%s addr=%s/%d", ifc.Name, red.ip(v4), ones))
			_ = bits
			if !seen[v4.String()] {
				seen[v4.String()] = true
				out = append(out, v4)
			}
		}
	}
	return out, notes
}

// --- Passive UDP 50000 / 50002 probe ---

func listenReuseUDP(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	p, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	return p.(*net.UDPConn), nil
}

func probeUDPPassive(targets []net.IP, dur time.Duration, asPlayer byte, red *redactor) map[string]byte {
	fmt.Printf("\n--- UDP Passive Probe (%s) ---\n", dur)
	allowed := map[string]bool{}
	for _, ip := range targets {
		if v4 := ip.To4(); v4 != nil {
			allowed[v4.String()] = true
		}
	}

	c50000, err50000 := listenReuseUDP(portAnnounce)
	if err50000 != nil {
		fmt.Printf("udp %d bind: %s (expected if rekordbox bound without SO_REUSEPORT)\n", portAnnounce, red.err(err50000))
	} else {
		defer c50000.Close()
		fmt.Printf("udp %d bind: OK\n", portAnnounce)
	}

	c50002, err50002 := listenReuseUDP(portStatus)
	if err50002 != nil {
		fmt.Printf("udp %d bind: %s (expected if rekordbox bound without SO_REUSEPORT)\n", portStatus, red.err(err50002))
	} else {
		defer c50002.Close()
		fmt.Printf("udp %d bind: OK\n", portStatus)
	}

	if c50000 == nil && c50002 == nil {
		return map[string]byte{}
	}

	type udpSeen struct {
		port int
		src  net.IP
		data []byte
	}
	var mu sync.Mutex
	var pkts []udpSeen
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	var wg sync.WaitGroup
	readLoop := func(port int, c *net.UDPConn) {
		defer wg.Done()
		buf := make([]byte, 2048)
		for {
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, addr, err := c.ReadFromUDP(buf)
			if ctx.Err() != nil {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if err != nil {
				return
			}
			v4 := addr.IP.To4()
			if v4 == nil || !allowed[v4.String()] {
				continue // strictly ignore non-target LAN hosts
			}
			cp := append([]byte(nil), buf[:n]...)
			mu.Lock()
			if len(pkts) < 32 {
				pkts = append(pkts, udpSeen{port: port, src: v4, data: cp})
			}
			mu.Unlock()
		}
	}

	if c50000 != nil {
		wg.Add(1)
		go readLoop(portAnnounce, c50000)
	}
	if c50002 != nil {
		wg.Add(1)
		go readLoop(portStatus, c50002)
	}
	wg.Wait()

	// Check if we observed a target device number on 50000; if 50002 bound, try unicast media query (0x05) directly to target.
	seenNums := map[string]byte{}
	uniqueAnn := map[string]bool{}
	for _, p := range pkts {
		if p.port == portAnnounce && len(p.data) >= 0x36 && string(p.data[:10]) == prolinkMagic && p.data[10] == 0x06 {
			seenNums[p.src.String()] = p.data[0x24]
		}
	}
	if c50002 != nil && len(seenNums) > 0 {
		for _, ip := range targets {
			num, ok := seenNums[ip.String()]
			if !ok {
				continue
			}
			for _, slot := range []byte{4, 5} {
				mq := buildMediaQuery(asPlayer, ip, num, slot)
				_, _ = c50002.WriteToUDP(mq, &net.UDPAddr{IP: ip, Port: portStatus})
			}
		}
		// Briefly collect any unicast 0x06 media responses.
		dead := time.Now().Add(600 * time.Millisecond)
		buf := make([]byte, 2048)
		for time.Now().Before(dead) {
			_ = c50002.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
			n, addr, err := c50002.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			v4 := addr.IP.To4()
			if v4 != nil && allowed[v4.String()] {
				pkts = append(pkts, udpSeen{port: portStatus, src: v4, data: append([]byte(nil), buf[:n]...)})
			}
		}
	}

	if len(pkts) == 0 {
		fmt.Println("udp packets from targets: none captured")
		return seenNums
	}
	for _, p := range pkts {
		if len(p.data) < 12 || string(p.data[:10]) != prolinkMagic {
			fmt.Printf("udp %d from %s: len=%d non-prolink\n", p.port, red.ip(p.src), len(p.data))
			continue
		}
		typ := p.data[10]
		if p.port == portAnnounce && typ == 0x06 && len(p.data) >= 0x36 {
			key := fmt.Sprintf("%s:%x", p.src, p.data)
			if uniqueAnn[key] {
				continue
			}
			uniqueAnn[key] = true
			nameRaw := p.data[0x0c:0x20]
			if idx := bytes.IndexByte(nameRaw, 0); idx >= 0 {
				nameRaw = nameRaw[:idx]
			}
			nameSafe := red.sanitizeGeneric(string(nameRaw))
			masked := append([]byte(nil), p.data...)
			copy(masked[0x26:0x2c], make([]byte, 6)) // zero MAC
			copy(masked[0x2c:0x30], make([]byte, 4)) // zero IP
			fmt.Printf("udp %d keepalive from %s: len=%d b[0x0b]=0x%02x name=%q name_len=%d b[0x20]=0x%02x b[0x21]=0x%02x number=%d(0x%02x) first=0x%02x mac=%s pkt_ip=%s peers=%d b[0x31:0x34]=%x b[0x34]=0x%02x b[0x35]=0x%02x tail_hex=%s\n",
				p.port, red.ip(p.src), len(p.data), p.data[0x0b], nameSafe, len(nameRaw), p.data[0x20], p.data[0x21],
				p.data[0x24], p.data[0x24], p.data[0x25], red.mac(p.data[0x26:0x2c]), red.ip(net.IP(p.data[0x2c:0x30])),
				p.data[0x30], p.data[0x31:0x34], p.data[0x34], p.data[0x35], hex.EncodeToString(masked[0x20:]))
		} else if p.port == portStatus && typ == 0x06 && len(p.data) >= 0xc0 {
			mName := decodeUTF16BEString(p.data[0x2c:0x6c])
			mDate := decodeUTF16BEString(p.data[0x6c:0x88])
			tracks := binary.BigEndian.Uint16(p.data[0xa6:0xa8])
			playlists := binary.BigEndian.Uint16(p.data[0xae:0xb0])
			fmt.Printf("udp %d media_resp from %s: target_player=%d slot=%d name_len=%d date=%q tracks=%d playlists=%d track_type=0x%02x color=0x%02x\n",
				p.port, red.ip(p.src), p.data[0x27], p.data[0x2b], len(mName), mDate, tracks, playlists, p.data[0xaa], p.data[0xa8])
		} else {
			fmt.Printf("udp %d pkt from %s: type=0x%02x len=%d\n", p.port, red.ip(p.src), typ, len(p.data))
		}
	}
	return seenNums
}

func probeMediaQueryForTarget(ip net.IP, asPlayer, targetPlayer byte, red *redactor) {
	c50002, err := listenReuseUDP(portStatus)
	if err != nil {
		fmt.Printf("udp %d media_query(%s, target=%d): bind %s\n", portStatus, red.ip(ip), targetPlayer, red.err(err))
		return
	}
	defer c50002.Close()
	for _, slot := range []byte{4, 5} {
		mq := buildMediaQuery(asPlayer, ip, targetPlayer, slot)
		_, _ = c50002.WriteToUDP(mq, &net.UDPAddr{IP: ip, Port: portStatus})
	}
	dead := time.Now().Add(600 * time.Millisecond)
	buf := make([]byte, 2048)
	got := 0
	for time.Now().Before(dead) {
		_ = c50002.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, addr, err := c50002.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		v4 := addr.IP.To4()
		if v4 == nil || !v4.Equal(ip.To4()) || n < 0xc0 || string(buf[:10]) != prolinkMagic || buf[10] != 0x06 {
			continue
		}
		got++
		mName := decodeUTF16BEString(buf[0x2c:0x6c])
		mDate := decodeUTF16BEString(buf[0x6c:0x88])
		tracks := binary.BigEndian.Uint16(buf[0xa6:0xa8])
		playlists := binary.BigEndian.Uint16(buf[0xae:0xb0])
		fmt.Printf("udp %d media_resp from %s: target_player=%d slot=%d name_len=%d date=%q tracks=%d playlists=%d track_type=0x%02x color=0x%02x\n",
			portStatus, red.ip(v4), buf[0x27], buf[0x2b], len(mName), mDate, tracks, playlists, buf[0xaa], buf[0xa8])
	}
	if got == 0 {
		fmt.Printf("udp %d media_query(%s, target=%d): no 0x06 reply\n", portStatus, red.ip(ip), targetPlayer)
	}
}

func buildMediaQuery(asPlayer byte, replyIP net.IP, targetPlayer, targetSlot byte) []byte {
	b := make([]byte, 0x30)
	copy(b[0:], prolinkMagic)
	b[0x0a] = 0x05
	copy(b[0x0b:], "rbx-probe")
	b[0x1f] = 0x01
	b[0x20] = 0x00
	b[0x21] = asPlayer
	binary.BigEndian.PutUint16(b[0x22:], 0x000c)
	copy(b[0x24:], replyIP.To4())
	b[0x2b] = targetPlayer
	b[0x2f] = targetSlot
	return b
}

func buildVirtualCDJKeepAlive(asPlayer byte, ip net.IP, mac [6]byte) []byte {
	b := make([]byte, 0x36)
	copy(b[0:], prolinkMagic)
	b[0x0a] = 0x06
	copy(b[0x0c:0x20], "CDJ-3000")
	b[0x20] = 0x01
	b[0x21] = 0x03 // generation 3 (CDJ-3000 class)
	binary.BigEndian.PutUint16(b[0x22:], 0x0036)
	b[0x24] = asPlayer
	b[0x25] = 0x01
	copy(b[0x26:0x2c], mac[:])
	if v4 := ip.To4(); v4 != nil {
		copy(b[0x2c:0x30], v4)
	}
	b[0x30] = 0x01 // peers
	b[0x34] = 0x01 // DeviceType::Cdj
	b[0x35] = 0x64 // CDJ-3000
	return b
}

func wakeTargets(targets []net.IP, asPlayer byte, red *redactor) func() {
	fmt.Printf("\n--- Wake (sending virtual CDJ-3000 0x06 keepalive as player %d to *:50000; waiting up to 10s for TCP %d) ---\n", asPlayer, portDBQuery)
	mac := [6]byte{0x02, 0x52, 0x42, 0x58, 0x00, asPlayer}
	var sameHostIPs []string
	sendOnce := func(recordSameHost bool) {
		for _, ip := range targets {
			c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: portAnnounce})
			if err != nil {
				continue
			}
			pktIP := ip
			if la, ok := c.LocalAddr().(*net.UDPAddr); ok && la.IP.To4() != nil {
				pktIP = la.IP.To4()
			}
			if recordSameHost && pktIP.Equal(ip) && !ip.IsLoopback() {
				sameHostIPs = append(sameHostIPs, red.ip(ip))
			}
			_, _ = c.Write(buildVirtualCDJKeepAlive(asPlayer, pktIP, mac))
			_ = c.Close()
		}
	}

	sendOnce(true)
	sameHostSummary := strings.Join(sameHostIPs, ", ")
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
				sendOnce(false)
			}
		}
	}()

	start := time.Now()
	deadline := start.Add(10 * time.Second)
	ready := false
	seenNotReady := map[string]bool{}
	for !ready && time.Now().Before(deadline) {
		for _, ip := range targets {
			if dbPort, err := queryDBPort(ip, 250*time.Millisecond); err == nil {
				if dbPort > 0 && dbPort < 0xffff {
					fmt.Printf("wake: %s TCP %d ready (RemoteDBServer port=%d) after %s\n",
						red.ip(ip), portDBQuery, dbPort, time.Since(start).Round(time.Millisecond))
					ready = true
					break
				}
				if !seenNotReady[ip.String()] {
					seenNotReady[ip.String()] = true
					fmt.Printf("wake: %s TCP %d returned port=%d (LINK not yet active)\n", red.ip(ip), portDBQuery, dbPort)
				}
			}
		}
		if !ready {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !ready {
		fmt.Printf("wake: TCP %d still closed/not ready after 10s (proceeding with probe anyway)\n", portDBQuery)
		if sameHostSummary != "" {
			fmt.Printf("wake: note: source IP equals target (%s); rekordbox may ignore own-IP 0x06 keepalives — test -wake from another LAN host/deck if LINK stays closed\n", sameHostSummary)
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-doneCh
		})
	}
}

// --- dbserver client ---

type dbField struct {
	tag  byte
	num  uint32
	str  string
	blob []byte
}

type dbMsg struct {
	tx   uint32
	kind uint16
	args []dbField
}

func encodeDBReq(tx uint32, kind uint16, args ...dbField) []byte {
	var b bytes.Buffer
	b.Write(dbMagic)
	b.WriteByte(0x11)
	_ = binary.Write(&b, binary.BigEndian, tx)
	b.WriteByte(0x10)
	_ = binary.Write(&b, binary.BigEndian, kind)
	b.WriteByte(0x0f)
	b.WriteByte(byte(len(args)))
	b.Write([]byte{0x14, 0, 0, 0, 12})
	var tags [12]byte
	for i, a := range args {
		if i < 12 {
			tags[i] = a.tag
		}
	}
	b.Write(tags[:])
	for _, a := range args {
		switch a.tag {
		case 0x06:
			b.WriteByte(0x11)
			_ = binary.Write(&b, binary.BigEndian, a.num)
		case 0x02:
			u := utf16.Encode([]rune(a.str))
			cu := uint32(len(u) + 1)
			b.WriteByte(0x26)
			_ = binary.Write(&b, binary.BigEndian, cu)
			for _, v := range u {
				_ = binary.Write(&b, binary.BigEndian, v)
			}
			b.Write([]byte{0, 0})
		case 0x03:
			b.WriteByte(0x14)
			_ = binary.Write(&b, binary.BigEndian, uint32(len(a.blob)))
			b.Write(a.blob)
		}
	}
	return b.Bytes()
}

func dbNum(v uint32) dbField { return dbField{tag: 0x06, num: v} }

func dmst(player, menu, slot, trackType byte) uint32 {
	return uint32(player)<<24 | uint32(menu)<<16 | uint32(slot)<<8 | uint32(trackType)
}

func tagLE(s string) uint32 {
	var b [4]byte
	copy(b[:], s)
	return binary.LittleEndian.Uint32(b[:])
}

const maxDBArgs = 32

func decodeDBMsg(r io.Reader) (*dbMsg, error) {
	var hdr [20]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(hdr[:5], dbMagic) || hdr[5] != 0x11 || hdr[10] != 0x10 || hdr[13] != 0x0f || hdr[15] != 0x14 {
		return nil, fmt.Errorf("bad dbserver header %x", hdr[:])
	}
	tx := binary.BigEndian.Uint32(hdr[6:10])
	kind := binary.BigEndian.Uint16(hdr[11:13])
	argc := int(hdr[14])
	tagLen := int(binary.BigEndian.Uint32(hdr[16:20]))
	if argc > maxDBArgs || tagLen < argc || tagLen > maxDBArgs {
		return nil, fmt.Errorf("bad dbserver header %x (argc=%d tagLen=%d)", hdr[:], argc, tagLen)
	}
	argTags := make([]byte, tagLen)
	if _, err := io.ReadFull(r, argTags); err != nil {
		return nil, err
	}
	msg := &dbMsg{tx: tx, kind: kind, args: make([]dbField, 0, argc)}
	for i := 0; i < argc; i++ {
		if argTags[i] == 0x03 && i > 0 && msg.args[i-1].tag == 0x06 && msg.args[i-1].num == 0 {
			// Protocol omits empty 0x14 blob fields when the preceding length field is 0.
			msg.args = append(msg.args, dbField{tag: 0x03})
			continue
		}
		var tag [1]byte
		if _, err := io.ReadFull(r, tag[:]); err != nil {
			return nil, err
		}
		var f dbField
		switch tag[0] {
		case 0x0f:
			var b [1]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			f = dbField{tag: 0x06, num: uint32(b[0])}
		case 0x10:
			var b [2]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			f = dbField{tag: 0x06, num: uint32(binary.BigEndian.Uint16(b[:]))}
		case 0x11:
			var b [4]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			f = dbField{tag: 0x06, num: binary.BigEndian.Uint32(b[:])}
		case 0x26:
			var b [4]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			val := binary.BigEndian.Uint32(b[:])
			if val > 65536 {
				return nil, fmt.Errorf("oversized string %d", val)
			}
			raw := make([]byte, 2*val)
			if _, err := io.ReadFull(r, raw); err != nil {
				return nil, err
			}
			f = dbField{tag: 0x02, str: decodeUTF16BEString(raw)}
		case 0x14:
			var b [4]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return nil, err
			}
			val := binary.BigEndian.Uint32(b[:])
			if val > 16<<20 {
				return nil, fmt.Errorf("oversized blob %d", val)
			}
			blob := make([]byte, val)
			if _, err := io.ReadFull(r, blob); err != nil {
				return nil, err
			}
			f = dbField{tag: 0x03, blob: blob}
		default:
			return nil, fmt.Errorf("unknown field marker 0x%02x", tag[0])
		}
		if f.tag != argTags[i] {
			return nil, fmt.Errorf("arg %d tag mismatch: header=0x%02x field=0x%02x", i, argTags[i], f.tag)
		}
		msg.args = append(msg.args, f)
	}
	return msg, nil
}

func setupServerPlayer(ack *dbMsg) uint32 {
	if len(ack.args) > 1 {
		return ack.args[1].num
	}
	if len(ack.args) > 0 {
		return ack.args[0].num
	}
	return 0
}

func decodeUTF16BEString(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

type dbConn struct {
	c  net.Conn
	r  *bufio.Reader
	tx uint32
}

func dialDB(ip net.IP, port int, asPlayer byte, timeout time.Duration) (*dbConn, uint32, error) {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, 0, err
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	hello := []byte{0x11, 0, 0, 0, 1}
	if _, err := c.Write(hello); err != nil {
		c.Close()
		return nil, 0, err
	}
	var got [5]byte
	if _, err := io.ReadFull(c, got[:]); err != nil || !bytes.Equal(got[:], hello) {
		c.Close()
		return nil, 0, fmt.Errorf("greeting err=%v got=%x", err, got)
	}
	d := &dbConn{c: c, r: bufio.NewReader(c)}
	if _, err := c.Write(encodeDBReq(0xfffffffe, kindSetup, dbNum(uint32(asPlayer)))); err != nil {
		c.Close()
		return nil, 0, err
	}
	ack, err := decodeDBMsg(d.r)
	if err != nil {
		c.Close()
		return nil, 0, err
	}
	if ack.kind != kindSetupAck {
		c.Close()
		return nil, 0, fmt.Errorf("setup got kind 0x%04x", ack.kind)
	}
	return d, setupServerPlayer(ack), nil
}

func (d *dbConn) Close() { _ = d.c.Close() }

func (d *dbConn) req(kind uint16, timeout time.Duration, args ...dbField) (*dbMsg, error) {
	d.tx++
	_ = d.c.SetDeadline(time.Now().Add(timeout))
	if _, err := d.c.Write(encodeDBReq(d.tx, kind, args...)); err != nil {
		return nil, err
	}
	return decodeDBMsg(d.r)
}

type menuRow struct {
	parent uint32
	id     uint32
	label  string
	sec    string
	kind   uint32
	flags  uint32
	artID  uint32
	pos    uint32
	arg12  uint32
}

func fmtCount(n uint32) string {
	if n == 0xffffffff {
		return "0xffffffff (none)"
	}
	return strconv.FormatUint(uint64(n), 10)
}

func (d *dbConn) renderMenu(player, menu, slot, trackType byte, dmstArg uint32, count uint32, limit uint32, timeout time.Duration) ([]menuRow, error) {
	if count == 0 || count == 0xffffffff {
		return nil, nil
	}
	var rows []menuRow
	_ = menu
	_ = player
	_ = slot
	_ = trackType
	for off := uint32(0); off < count && off < limit; {
		n := count - off
		if n > limit-off {
			n = limit - off
		}
		if n > 64 {
			n = 64
		}
		hdr, err := d.req(kindRender, timeout, dbNum(dmstArg), dbNum(off), dbNum(n), dbNum(0), dbNum(count), dbNum(0))
		if err != nil {
			return rows, err
		}
		if hdr.kind != kindRenderHdr {
			return rows, fmt.Errorf("render header got 0x%04x", hdr.kind)
		}
		for i := uint32(0); i < n; i++ {
			_ = d.c.SetDeadline(time.Now().Add(timeout))
			it, err := decodeDBMsg(d.r)
			if err != nil {
				return rows, err
			}
			if it.kind == kindRenderFoot {
				return rows, nil
			}
			if it.kind != kindRenderItem || len(it.args) < 7 {
				return rows, fmt.Errorf("render item got 0x%04x argc=%d", it.kind, len(it.args))
			}
			row := menuRow{
				parent: it.args[0].num,
				id:     it.args[1].num,
				label:  cleanMenuLabel(it.args[3].str),
				sec:    cleanMenuLabel(it.args[5].str),
				kind:   it.args[6].num,
			}
			if len(it.args) > 7 {
				row.flags = it.args[7].num
			}
			if len(it.args) > 8 {
				row.artID = it.args[8].num
			}
			if len(it.args) > 9 {
				row.pos = it.args[9].num
			}
			if len(it.args) > 12 {
				row.arg12 = it.args[12].num
			}
			rows = append(rows, row)
		}
		_ = d.c.SetDeadline(time.Now().Add(timeout))
		foot, err := decodeDBMsg(d.r)
		if err != nil {
			return rows, err
		}
		if foot.kind != kindRenderFoot {
			return rows, fmt.Errorf("render footer got 0x%04x", foot.kind)
		}
		off += n
	}
	return rows, nil
}

func cleanMenuLabel(s string) string {
	s = strings.TrimPrefix(s, "￺")
	s = strings.TrimSuffix(s, "￻")
	return s
}

var fallbackDBPorts = []int{55000, 55002, 50000}

func queryDBPortOn(ip net.IP, port int, timeout time.Duration) (int, error) {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(port)), timeout)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	ask := append([]byte{0, 0, 0, 15}, "RemoteDBServer\x00"...)
	if _, err := c.Write(ask); err != nil {
		return 0, err
	}
	var buf [64]byte
	n, err := c.Read(buf[:])
	if err == nil && n == 1 {
		var second [1]byte
		if _, err2 := io.ReadFull(c, second[:]); err2 == nil {
			buf[1] = second[0]
			n = 2
		}
	}
	if err != nil && n == 0 {
		return 0, err
	}
	if n != 2 {
		return 0, fmt.Errorf("unexpected %d-byte reply hex=%s ascii=%q",
			n, hex.EncodeToString(buf[:min(n, 16)]), printableASCII(buf[:min(n, 16)]))
	}
	return int(binary.BigEndian.Uint16(buf[:2])), nil
}

func queryDBPort(ip net.IP, timeout time.Duration) (int, error) {
	return queryDBPortOn(ip, portDBQuery, timeout)
}

func probeDirectDBPort(ip net.IP, port int, asPlayer byte, timeout time.Duration, red *redactor) (bool, bool) {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(port)), timeout)
	if err != nil {
		fmt.Printf("tcp %d direct dbserver: dial ERROR: %s\n", port, red.err(err))
		return false, false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	hello := []byte{0x11, 0, 0, 0, 1}
	if _, err := c.Write(hello); err != nil {
		fmt.Printf("tcp %d direct dbserver: write ERROR: %s\n", port, red.err(err))
		return false, true
	}
	var buf [64]byte
	n, err := c.Read(buf[:])
	if err == nil && n > 0 && n < len(hello) {
		if _, err2 := io.ReadFull(c, buf[n:len(hello)]); err2 == nil {
			n = len(hello)
		}
	}
	if n == 0 {
		fmt.Printf("tcp %d direct dbserver: greeting ERROR: %s\n", port, red.err(err))
		return false, true
	}
	if n != len(hello) || !bytes.Equal(buf[:len(hello)], hello) {
		fmt.Printf("tcp %d direct dbserver: unexpected greeting len=%d hex=%s ascii=%q\n",
			port, n, hex.EncodeToString(buf[:min(n, 16)]), red.sanitizeGeneric(printableASCII(buf[:min(n, 16)])))
		return false, true
	}
	d := &dbConn{c: c, r: bufio.NewReader(c)}
	if _, err := c.Write(encodeDBReq(0xfffffffe, kindSetup, dbNum(uint32(asPlayer)))); err != nil {
		fmt.Printf("tcp %d direct dbserver: greeting OK, setup write ERROR: %s\n", port, red.err(err))
		return true, true
	}
	ack, err := decodeDBMsg(d.r)
	if err != nil {
		fmt.Printf("tcp %d direct dbserver: greeting OK, setup ERROR: %s\n", port, red.err(err))
		return true, true
	}
	fmt.Printf("tcp %d direct dbserver: greeting OK, setup as=%d kind=0x%04x server_player=%d\n", port, asPlayer, ack.kind, setupServerPlayer(ack))
	return true, true
}

func probeDBFallback(ip net.IP, asPlayer byte, ports []int, timeout time.Duration, red *redactor) int {
	fbTimeout := timeout
	if fbTimeout > 750*time.Millisecond {
		fbTimeout = 750 * time.Millisecond
	}
	for _, cand := range ports {
		isDB, dialed := probeDirectDBPort(ip, cand, asPlayer, fbTimeout, red)
		if isDB {
			return cand
		}
		if !dialed {
			continue
		}
		qp, qerr := queryDBPortOn(ip, cand, fbTimeout)
		if qerr != nil {
			fmt.Printf("tcp %d RemoteDBServer query: ERROR: %s\n", cand, red.err(qerr))
			continue
		}
		fmt.Printf("tcp %d RemoteDBServer port=%d\n", cand, qp)
		if qp > 0 && qp < 0xffff {
			return qp
		}
	}
	return 0
}

func probeDBServer(ip net.IP, asPlayer byte, maxTracks int, specificTrack uint32, timeout time.Duration, needMediaQuery bool, red *redactor) []string {
	fmt.Printf("\n--- TCP dbserver Probe (%s) ---\n", red.ip(ip))
	dbPort, err := queryDBPort(ip, timeout)
	if err != nil {
		fmt.Printf("tcp %d RemoteDBServer query: ERROR: %s\n", portDBQuery, red.err(err))
	} else if dbPort == 0 || dbPort == 0xffff {
		fmt.Printf("tcp %d RemoteDBServer port=%d (LINK not active)\n", portDBQuery, dbPort)
		dbPort = 0
	} else {
		fmt.Printf("tcp %d RemoteDBServer port=%d\n", portDBQuery, dbPort)
	}
	if dbPort == 0 {
		dbPort = probeDBFallback(ip, asPlayer, fallbackDBPorts, timeout, red)
		if dbPort == 0 {
			return nil
		}
	}

	// Test asking player numbers 1, 4, 5, 7, 41.
	var okPlayers []byte
	var srvPlayer uint32
	for _, testAs := range []byte{1, 4, 5, 7, 41} {
		dc, srvNum, err := dialDB(ip, dbPort, testAs, timeout)
		if err != nil {
			fmt.Printf("setup as=%d: ERROR: %s\n", testAs, red.err(err))
		} else {
			fmt.Printf("setup as=%d: OK (server_player=%d)\n", testAs, srvNum)
			okPlayers = append(okPlayers, testAs)
			if srvPlayer == 0 && srvNum != 0 {
				srvPlayer = srvNum
			}
			dc.Close()
		}
	}

	if needMediaQuery && srvPlayer != 0 && srvPlayer <= 255 {
		probeMediaQueryForTarget(ip, asPlayer, byte(srvPlayer), red)
	}

	// Also check whether a >4 asking player works for actual 0x1000 browse (not just setup).
	for _, testAs := range okPlayers {
		if testAs <= 4 {
			continue
		}
		if hdc, _, err := dialDB(ip, dbPort, testAs, timeout); err == nil {
			for _, slot := range []byte{4, 5} {
				rm, rerr := hdc.req(kindRootMenu, timeout, dbNum(dmst(testAs, 1, slot, 1)), dbNum(0), dbNum(0xffffff))
				if rerr != nil {
					fmt.Printf("browse_check as=%d slot=%d 0x1000: ERROR: %s\n", testAs, slot, red.err(rerr))
					break
				}
				var cnt uint32
				if len(rm.args) >= 2 {
					cnt = rm.args[1].num
				}
				fmt.Printf("browse_check as=%d slot=%d 0x1000: kind=0x%04x count=%s\n", testAs, slot, rm.kind, fmtCount(cnt))
			}
			hdc.Close()
			break
		}
	}

	dc, _, err := dialDB(ip, dbPort, asPlayer, timeout)
	if err != nil {
		fmt.Printf("main setup as=%d: ERROR: %s\n", asPlayer, red.err(err))
		for _, cand := range okPlayers {
			if ndc, _, nerr := dialDB(ip, dbPort, cand, timeout); nerr == nil {
				fmt.Printf("main setup fallback as=%d: OK\n", cand)
				dc = ndc
				asPlayer = cand
				err = nil
				break
			}
		}
		if err != nil {
			return nil
		}
	}
	defer dc.Close()
	redial := func() bool {
		if dc != nil {
			dc.Close()
		}
		ndc, _, rerr := dialDB(ip, dbPort, asPlayer, timeout)
		if rerr != nil {
			fmt.Printf("  redial as=%d: ERROR: %s\n", asPlayer, red.err(rerr))
			dc = nil
			return false
		}
		dc = ndc
		return true
	}

	var realPaths []string
	var workingSlot byte
	for _, slot := range []byte{4, 5} {
		if dc == nil && !redial() {
			break
		}
		fmt.Printf("\n[slot=%d (as=%d)]\n", slot, asPlayer)
		d := dmst(asPlayer, 1, slot, 1)

		// 1. Root menu 0x1000
		rm, err := dc.req(kindRootMenu, timeout, dbNum(d), dbNum(0), dbNum(0xffffff))
		if err != nil {
			fmt.Printf("  0x1000 root_menu: ERROR: %s\n", red.err(err))
			if !redial() {
				break
			}
		} else if rm.kind != kindSetupAck || len(rm.args) < 2 {
			fmt.Printf("  0x1000 root_menu: reply kind=0x%04x argc=%d\n", rm.kind, len(rm.args))
		} else {
			cnt := rm.args[1].num
			fmt.Printf("  0x1000 root_menu: count=%s\n", fmtCount(cnt))
			items, err := dc.renderMenu(asPlayer, 1, slot, 1, d, cnt, 32, timeout)
			if err != nil {
				fmt.Printf("    render error: %s\n", red.err(err))
				_ = redial()
			}
			for _, it := range items {
				fmt.Printf("    id=%d kind=0x%04x label=%q\n", it.id, it.kind, red.sanitizeGeneric(it.label))
			}
		}

		// 2. All tracks 0x1004
		var trackIDs []uint32
		var trackArts = map[uint32]uint32{}
		seenTrack := map[uint32]bool{}
		addTrackID := func(id uint32) {
			if id != 0 && !seenTrack[id] && len(trackIDs) < maxTracks {
				seenTrack[id] = true
				trackIDs = append(trackIDs, id)
			}
		}
		if specificTrack != 0 {
			addTrackID(specificTrack)
		}
		tm, err := dc.req(kindAllTracks, timeout, dbNum(d), dbNum(0))
		if err != nil {
			fmt.Printf("  0x1004 all_tracks: ERROR: %s\n", red.err(err))
			_ = redial()
		} else if tm.kind != kindSetupAck || len(tm.args) < 2 {
			fmt.Printf("  0x1004 all_tracks: reply kind=0x%04x\n", tm.kind)
		} else {
			cnt := tm.args[1].num
			fmt.Printf("  0x1004 all_tracks: count=%s\n", fmtCount(cnt))
			if cnt > 0 && cnt != 0xffffffff && workingSlot == 0 {
				workingSlot = slot
			}
			limit := cnt
			if limit > 200 {
				limit = 200 // render up to 200 rows to sample start + end
			}
			rows, err := dc.renderMenu(asPlayer, 1, slot, 1, d, cnt, limit, timeout)
			if err != nil {
				fmt.Printf("    render error: %s\n", red.err(err))
				_ = redial()
			}
			if len(rows) > 0 {
				// Pick sample indices: first 2, one with artID > 0, and last 2.
				picks := []int{}
				seenIdx := map[int]bool{}
				addPick := func(idx int) {
					if idx >= 0 && idx < len(rows) && !seenIdx[idx] {
						seenIdx[idx] = true
						picks = append(picks, idx)
					}
				}
				for i := 0; i < 2; i++ {
					addPick(i)
				}
				for i, r := range rows {
					if r.artID != 0 {
						addPick(i)
						break
					}
				}
				for i := len(rows) - 2; i < len(rows); i++ {
					addPick(i)
				}
				for _, idx := range picks {
					r := rows[idx]
					trackArts[r.id] = r.artID
					addTrackID(r.id)
					fmt.Printf("    sample_track[%d]: id=%d parent=%d kind=0x%04x art=%d pos=%d title_len=%d artist_len=%d\n",
						idx, r.id, r.parent, r.kind, r.artID, r.pos, len(r.label), len(r.sec))
				}
			}
		}

		// 3. Root playlists & folders 0x1105
		if dc == nil && !redial() {
			break
		}
		pm, err := dc.req(kindPlaylist, timeout, dbNum(d), dbNum(0), dbNum(0), dbNum(1))
		if err != nil {
			fmt.Printf("  0x1105 playlists_root: ERROR: %s\n", red.err(err))
			_ = redial()
		} else if pm.kind != kindSetupAck || len(pm.args) < 2 {
			fmt.Printf("  0x1105 playlists_root: reply kind=0x%04x\n", pm.kind)
		} else {
			cnt := pm.args[1].num
			fmt.Printf("  0x1105 playlists_root: count=%s\n", fmtCount(cnt))
			rows, err := dc.renderMenu(asPlayer, 1, slot, 1, d, cnt, 16, timeout)
			if err != nil {
				fmt.Printf("    render error: %s\n", red.err(err))
				_ = redial()
			}
			var firstFolderID, firstPlaylistID uint32
			for _, r := range rows {
				fmt.Printf("    entry id=%d parent=%d kind=0x%04x pos=%d name_len=%d\n", r.id, r.parent, r.kind, r.pos, len(r.label))
				if r.kind == 1 && firstFolderID == 0 {
					firstFolderID = r.id
				}
				if r.kind == 8 && firstPlaylistID == 0 {
					firstPlaylistID = r.id
				}
			}
			if firstFolderID != 0 && dc != nil {
				fm, err := dc.req(kindPlaylist, timeout, dbNum(d), dbNum(0), dbNum(firstFolderID), dbNum(1))
				if err != nil {
					fmt.Printf("  0x1105 subfolder(%d): ERROR: %s\n", firstFolderID, red.err(err))
					_ = redial()
				} else if fm.kind == kindSetupAck && len(fm.args) >= 2 {
					fc := fm.args[1].num
					fmt.Printf("  0x1105 subfolder(%d): count=%s\n", firstFolderID, fmtCount(fc))
					frows, rerr := dc.renderMenu(asPlayer, 1, slot, 1, d, fc, 8, timeout)
					if rerr != nil {
						fmt.Printf("    render error: %s\n", red.err(rerr))
						_ = redial()
					}
					for _, r := range frows {
						fmt.Printf("    subentry id=%d parent=%d kind=0x%04x pos=%d name_len=%d\n", r.id, r.parent, r.kind, r.pos, len(r.label))
						if r.kind == 8 && firstPlaylistID == 0 {
							firstPlaylistID = r.id
						}
					}
				}
			}
			if firstPlaylistID != 0 && dc != nil {
				plm, err := dc.req(kindPlaylist, timeout, dbNum(d), dbNum(0), dbNum(firstPlaylistID), dbNum(0))
				if err != nil {
					fmt.Printf("  0x1105 playlist_tracks(%d): ERROR: %s\n", firstPlaylistID, red.err(err))
					_ = redial()
				} else if plm.kind == kindSetupAck && len(plm.args) >= 2 {
					pc := plm.args[1].num
					fmt.Printf("  0x1105 playlist_tracks(%d): count=%s\n", firstPlaylistID, fmtCount(pc))
					prows, rerr := dc.renderMenu(asPlayer, 1, slot, 1, d, pc, 5, timeout)
					if rerr != nil {
						fmt.Printf("    render error: %s\n", red.err(rerr))
						_ = redial()
					}
					for _, r := range prows {
						fmt.Printf("    pl_track id=%d parent=%d kind=0x%04x pos=%d title_len=%d artist_len=%d\n",
							r.id, r.parent, r.kind, r.pos, len(r.label), len(r.sec))
						if len(trackIDs) == 0 {
							if _, ok := trackArts[r.id]; !ok {
								trackArts[r.id] = r.artID
							}
							addTrackID(r.id)
						}
					}
				}
			}
		}

		// 4. Deep track probes for sampled tracks (if slot returned tracks; for slot!=workingSlot, test only 1 track if any).
		if len(trackIDs) > 0 {
			probeIDs := trackIDs
			if workingSlot != 0 && slot != workingSlot && len(probeIDs) > 1 {
				probeIDs = probeIDs[:1]
			}
			for i, tid := range probeIDs {
				paths := probeTrackDetails(ip, dbPort, asPlayer, slot, tid, trackArts[tid], i == 0, timeout, red)
				if i < 3 {
					realPaths = append(realPaths, paths...)
				}
			}
		}
	}

	return realPaths
}

func probeTrackDetails(ip net.IP, dbPort int, asPlayer, slot byte, tid, hintArt uint32, checkAlt bool, timeout time.Duration, red *redactor) []string {
	fmt.Printf("\n  -- Track id=%d (slot=%d) --\n", tid, slot)
	// Use a dedicated connection per track so unanswered requests never stall later tracks.
	dc, _, err := dialDB(ip, dbPort, asPlayer, timeout)
	if err != nil {
		fmt.Printf("    dialDB error: %s\n", red.err(err))
		return nil
	}
	defer func() {
		if dc != nil {
			dc.Close()
		}
	}()
	redial := func() bool {
		if dc != nil {
			dc.Close()
		}
		ndc, _, derr := dialDB(ip, dbPort, asPlayer, timeout)
		if derr != nil {
			fmt.Printf("    redial error: %s\n", red.err(derr))
			dc = nil
			return false
		}
		dc = ndc
		return true
	}

	d1 := dmst(asPlayer, 1, slot, 1)
	d8 := dmst(asPlayer, 8, slot, 1)

	// 0x2002 Track Metadata
	artID := hintArt
	mm, err := dc.req(kindMetadata, timeout, dbNum(d1), dbNum(tid))
	if err != nil {
		fmt.Printf("    0x2002 metadata: ERROR: %s\n", red.err(err))
		if !redial() {
			return nil
		}
	} else if mm.kind != kindSetupAck || len(mm.args) < 2 {
		fmt.Printf("    0x2002 metadata: kind=0x%04x\n", mm.kind)
	} else {
		rows, rerr := dc.renderMenu(asPlayer, 1, slot, 1, d1, mm.args[1].num, 32, timeout)
		var kinds []string
		for _, r := range rows {
			extra := ""
			if r.arg12 != 0 {
				extra = fmt.Sprintf(",a12=%d", r.arg12)
			}
			kinds = append(kinds, fmt.Sprintf("0x%04x(id=%d,art=%d%s,len=%d)", r.kind, r.id, r.artID, extra, len(r.label)))
			if r.kind == 0x04 && r.artID != 0 {
				artID = r.artID
			}
		}
		fmt.Printf("    0x2002 metadata: count=%s err=%s items=[%s]\n", fmtCount(mm.args[1].num), red.err(rerr), strings.Join(kinds, ", "))
		if rerr != nil && !redial() {
			return nil
		}
	}

	// 0x2102 Track File Path (test m=1, and m=8 on first track or if m=1 yields no path)
	var paths []string
	probe2102 := func(menu byte, dval uint32) {
		if dc == nil && !redial() {
			return
		}
		lbl := fmt.Sprintf("0x2102 track_info(m=%d)", menu)
		im, ierr := dc.req(kindTrackInfo, timeout, dbNum(dval), dbNum(tid))
		if ierr != nil {
			fmt.Printf("    %s: ERROR: %s\n", lbl, red.err(ierr))
			_ = redial()
			return
		}
		if im.kind != kindSetupAck || len(im.args) < 2 {
			fmt.Printf("    %s: kind=0x%04x\n", lbl, im.kind)
			return
		}
		rows, rerr := dc.renderMenu(asPlayer, menu, slot, 1, dval, im.args[1].num, 16, timeout)
		fmt.Printf("    %s: count=%s err=%s\n", lbl, fmtCount(im.args[1].num), red.err(rerr))
		for _, r := range rows {
			secInfo := fmt.Sprintf("sec_len=%d", len(r.sec))
			if strings.ContainsAny(r.sec, "/\\") {
				secInfo = fmt.Sprintf("sec=%q (%s)", red.path(r.sec), pathTraits(r.sec))
			}
			fmt.Printf("      item parent=%d id=%d kind=0x%04x path=%q (%s) %s\n",
				r.parent, r.id, r.kind, red.path(r.label), pathTraits(r.label), secInfo)
			if r.kind&0xffff == 0 && r.label != "" {
				paths = append(paths, r.label)
			}
		}
		if rerr != nil {
			_ = redial()
		}
	}
	probe2102(1, d1)
	if len(paths) == 0 || checkAlt {
		probe2102(8, d8)
	}

	// Single-request analysis/cue/artwork probes; reopen connection on timeout so framing stays intact.
	short := 1200 * time.Millisecond
	if timeout < short {
		short = timeout
	}
	probeBlob := func(label string, kind uint16, args ...dbField) bool {
		if dc == nil && !redial() {
			return false
		}
		resp, err := dc.req(kind, short, args...)
		if err != nil {
			fmt.Printf("    %s: ERROR: %s (reopening conn)\n", label, red.err(err))
			_ = redial()
			return false
		}
		var parts []string
		hasBlob := false
		for idx, a := range resp.args {
			switch a.tag {
			case 0x06:
				parts = append(parts, fmt.Sprintf("a%d=num(%d/0x%x)", idx, a.num, a.num))
			case 0x02:
				parts = append(parts, fmt.Sprintf("a%d=str(len=%d)", idx, len(a.str)))
			case 0x03:
				if len(a.blob) > 0 {
					hasBlob = true
				}
				head := a.blob
				if len(head) > 16 {
					head = head[:16]
				}
				parts = append(parts, fmt.Sprintf("a%d=blob(len=%d,head=%s)", idx, len(a.blob), hex.EncodeToString(head)))
			}
		}
		fmt.Printf("    %s: kind=0x%04x [%s]\n", label, resp.kind, strings.Join(parts, " "))
		return resp.kind != kindUnavailable && hasBlob
	}

	probeBlob("0x2204 beat_grid(m=8)", kindBeatGrid, dbNum(d8), dbNum(tid))
	probeBlob("0x2004 wave_prev(m=8,arg=4)", kindWavePrev, dbNum(d8), dbNum(4), dbNum(tid), dbNum(0))
	probeBlob("0x2004 wave_prev(m=8,arg=1)", kindWavePrev, dbNum(d8), dbNum(1), dbNum(tid), dbNum(0))
	probeBlob("0x2904 wave_detail(m=1)", kindWaveDetail, dbNum(d1), dbNum(tid), dbNum(0))
	probeBlob("0x2904 wave_detail(m=8)", kindWaveDetail, dbNum(d8), dbNum(tid), dbNum(0))
	if !probeBlob("0x2104 cue_list(m=8)", kindCueList, dbNum(d8), dbNum(tid)) && checkAlt {
		probeBlob("0x2104 cue_list(m=1)", kindCueList, dbNum(d1), dbNum(tid))
	}
	if !probeBlob("0x2b04 cue_list_ext(m=8)", kindCueListExt, dbNum(d8), dbNum(tid), dbNum(0)) && checkAlt {
		probeBlob("0x2b04 cue_list_ext(m=1)", kindCueListExt, dbNum(d1), dbNum(tid), dbNum(0))
	}

	anlzPairs := [][2]string{
		{"PQTZ", "DAT"},
		{"PCOB", "DAT"},
		{"PCO2", "EXT"},
		{"PWV3", "DAT"},
		{"PWV4", "EXT"},
		{"PWV5", "EXT"},
		{"PWV6", "2EX"},
		{"PWV7", "2EX"},
		{"PSSI", "EXT"},
	}
	for idx, p := range anlzPairs {
		lbl := fmt.Sprintf("0x2c04 anlz(%s.%s)", p[0], p[1])
		ok := probeBlob(lbl, kindANLZTag, dbNum(d1), dbNum(tid), dbNum(tagLE(p[0])), dbNum(tagLE(p[1])))
		if idx == 0 && !ok && checkAlt {
			probeBlob(lbl+"[m=8]", kindANLZTag, dbNum(d8), dbNum(tid), dbNum(tagLE(p[0])), dbNum(tagLE(p[1])))
		}
	}
	if artID != 0 {
		ok1 := probeBlob(fmt.Sprintf("0x2003 artwork(%d)", artID), kindArtwork, dbNum(d8), dbNum(artID))
		ok2 := probeBlob(fmt.Sprintf("0x2003 artwork_hi(%d)", artID), kindArtwork, dbNum(d8), dbNum(artID), dbNum(1))
		if !ok1 && !ok2 && checkAlt {
			probeBlob(fmt.Sprintf("0x2003 artwork(m=1,%d)", artID), kindArtwork, dbNum(d1), dbNum(artID))
		}
	}

	return paths
}

// --- Read-only SunRPC / NFSv2 client ---

type xdrBuf struct{ bytes.Buffer }

func (x *xdrBuf) u32(v uint32) *xdrBuf {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	x.Write(b[:])
	return x
}

func (x *xdrBuf) opaque(b []byte) *xdrBuf {
	x.u32(uint32(len(b)))
	x.Write(b)
	x.Write(make([]byte, (4-len(b)%4)%4))
	return x
}

type xdrReader struct {
	b   []byte
	err error
}

func (r *xdrReader) u32() uint32 {
	if r.err != nil || len(r.b) < 4 {
		r.err = errors.New("short RPC/XDR reply")
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *xdrReader) fixed(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errors.New("short RPC/XDR reply")
		return nil
	}
	v := r.b[:n]
	pad := (n + 3) &^ 3
	if pad > len(r.b) {
		pad = len(r.b)
	}
	r.b = r.b[pad:]
	return v
}

func (r *xdrReader) opaque(max int) []byte {
	n := r.u32()
	if r.err == nil && (max < 0 || uint64(n) > uint64(max)) {
		r.err = fmt.Errorf("oversized opaque %d", n)
	}
	if r.err != nil {
		return nil
	}
	pad := (int(n) + 3) &^ 3
	if len(r.b) < pad {
		if len(r.b) < int(n) {
			r.err = errors.New("short RPC opaque")
			return nil
		}
		pad = int(n)
	}
	v := r.b[:n]
	r.b = r.b[pad:]
	return v
}

func encodeUTF16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return b
}

func decodeWireName(b []byte) (string, string) {
	if len(b) >= 2 && len(b)%2 == 0 {
		// Check if UTF-16LE (either odd bytes are zero for ASCII, or NUL-free UTF-16LE).
		asciiLE := true
		for i := 1; i < len(b); i += 2 {
			if b[i] != 0 {
				asciiLE = false
				break
			}
		}
		if asciiLE {
			u := make([]uint16, len(b)/2)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(b[2*i:])
			}
			return strings.TrimRight(string(utf16.Decode(u)), "\x00"), "utf16le"
		}
	}
	return string(bytes.TrimRight(b, "\x00")), "utf8"
}

func rpcCallUDP(ip net.IP, port, srcPort int, prog, vers, proc uint32, authUnix bool, args []byte, timeout time.Duration) (*xdrReader, error) {
	var id [4]byte
	_, _ = rand.Read(id[:])
	xid := binary.BigEndian.Uint32(id[:])
	msg := &xdrBuf{}
	msg.u32(xid).u32(0).u32(2).u32(prog).u32(vers).u32(proc)
	if authUnix {
		cred := &xdrBuf{}
		cred.u32(0).opaque(nil).u32(0).u32(0).u32(0)
		msg.u32(1).opaque(cred.Bytes()).u32(0).u32(0)
	} else {
		msg.u32(0).u32(0).u32(0).u32(0)
	}
	msg.Write(args)

	var laddr *net.UDPAddr
	if srcPort > 0 {
		laddr = &net.UDPAddr{IP: net.IPv4zero, Port: srcPort}
	}
	conn, err := net.DialUDP("udp4", laddr, &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	buf := make([]byte, 64<<10)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := conn.Write(msg.Bytes()); err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return nil, err
			}
			r := &xdrReader{b: append([]byte(nil), buf[:n]...)}
			if r.u32() != xid || r.err != nil {
				continue
			}
			if mtype := r.u32(); mtype != 1 {
				return nil, fmt.Errorf("rpc unexpected mtype %d", mtype)
			}
			rstat := r.u32()
			if rstat != 0 {
				why := r.u32()
				detail := r.u32()
				return nil, fmt.Errorf("rpc MSG_DENIED stat=%d why=%d detail=%d", rstat, why, detail)
			}
			r.u32()          // verf flavor
			r.opaque(400)    // verf body
			astat := r.u32() // accept_stat
			if r.err != nil {
				return nil, r.err
			}
			if astat != 0 {
				return nil, fmt.Errorf("rpc accept_stat=%d (prog=%d vers=%d proc=%d)", astat, prog, vers, proc)
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("no UDP reply from %s:%d", ip, port)
}

func probeRPCNull(ip net.IP, label string, port, srcPort int, prog, vers uint32, authUnix bool, timeout time.Duration, red *redactor) bool {
	if _, err := rpcCallUDP(ip, port, srcPort, prog, vers, 0, authUnix, nil, timeout); err != nil {
		if authUnix {
			if _, err2 := rpcCallUDP(ip, port, srcPort, prog, vers, 0, false, nil, timeout); err2 == nil {
				fmt.Printf("%s NULL (proc 0) udp/%d: OK ONLY with AUTH_NULL!\n", label, port)
				return true
			}
		}
		fmt.Printf("%s NULL (proc 0) udp/%d: ERROR: %s\n", label, port, red.err(err))
		return false
	}
	fmt.Printf("%s NULL (proc 0) udp/%d (authUnix=%v): OK\n", label, port, authUnix)
	return true
}

func probeNFS(ip net.IP, trackPaths []string, srcPort int, timeout time.Duration, red *redactor) {
	fmt.Printf("\n--- UDP Portmap / MOUNT / NFSv2 Probe (%s) ---\n", red.ip(ip))

	// Check standard portmap 111 briefly, and rekordbox portmap 50111.
	short := 400 * time.Millisecond
	if r111, err := rpcCallUDP(ip, portmapStd, srcPort, rpcPortmap, 2, 4, true, nil, short); err != nil {
		fmt.Printf("portmap udp/%d DUMP: %s\n", portmapStd, red.err(err))
	} else {
		fmt.Printf("portmap udp/%d DUMP: responded (%d bytes)\n", portmapStd, len(r111.b))
	}

	pmPort := portmapRB
	authUnix := true
	rDump, err := rpcCallUDP(ip, pmPort, srcPort, rpcPortmap, 2, 4, true, nil, timeout)
	if err != nil {
		fmt.Printf("portmap udp/%d DUMP (AUTH_UNIX): %s\n", pmPort, red.err(err))
		rDump, err = rpcCallUDP(ip, pmPort, srcPort, rpcPortmap, 2, 4, false, nil, timeout)
		if err != nil {
			fmt.Printf("portmap udp/%d DUMP (AUTH_NULL): %s\n", pmPort, red.err(err))
		} else {
			authUnix = false
			fmt.Printf("portmap udp/%d DUMP: succeeded ONLY with AUTH_NULL!\n", pmPort)
		}
	} else {
		fmt.Printf("portmap udp/%d DUMP (AUTH_UNIX): OK\n", pmPort)
	}
	if err == nil && rDump != nil {
		for rDump.err == nil && rDump.u32() == 1 {
			prog := rDump.u32()
			vers := rDump.u32()
			proto := rDump.u32()
			port := rDump.u32()
			fmt.Printf("  pmap entry: prog=%d vers=%d proto=%d port=%d\n", prog, vers, proto, port)
		}
	}

	getPort := func(prog, vers uint32) (int, error) {
		args := (&xdrBuf{}).u32(prog).u32(vers).u32(17).u32(0).Bytes()
		r, err := rpcCallUDP(ip, pmPort, srcPort, rpcPortmap, 2, 3, authUnix, args, timeout)
		if err != nil {
			return 0, err
		}
		p := r.u32()
		if r.err != nil || p == 0 || p > 65535 {
			return 0, fmt.Errorf("not registered (port=%d err=%v)", p, r.err)
		}
		return int(p), nil
	}

	mountPort, err := getPort(rpcMount, 1)
	if err != nil {
		fmt.Printf("GETPORT mountd(100005,v1): ERROR: %s\n", red.err(err))
		return
	}
	nfsPort, err := getPort(rpcNFS, 2)
	if err != nil {
		fmt.Printf("GETPORT nfsd(100003,v2): ERROR: %s\n", red.err(err))
		return
	}
	fmt.Printf("GETPORT: mountd=%d nfsd=%d (authUnix=%v)\n", mountPort, nfsPort, authUnix)

	// MOUNTPROC_EXPORT (proc 5)
	type exportEntry struct {
		raw     []byte
		decoded string
		enc     string
	}
	var exports []exportEntry
	rExp, err := rpcCallUDP(ip, mountPort, srcPort, rpcMount, 1, 5, authUnix, nil, timeout)
	if err != nil {
		fmt.Printf("MOUNT EXPORT (proc 5, authUnix=%v): ERROR: %s\n", authUnix, red.err(err))
		if authUnix {
			if r2, err2 := rpcCallUDP(ip, mountPort, srcPort, rpcMount, 1, 5, false, nil, timeout); err2 == nil {
				rExp = r2
				err = nil
				authUnix = false
				fmt.Printf("MOUNT EXPORT (proc 5): succeeded ONLY with AUTH_NULL!\n")
			} else {
				fmt.Printf("MOUNT EXPORT (proc 5, AUTH_NULL): ERROR: %s\n", red.err(err2))
			}
		}
	}
	if err != nil {
		nullTimeout := timeout
		if nullTimeout > 800*time.Millisecond {
			nullTimeout = 800 * time.Millisecond
		}
		okM := probeRPCNull(ip, "MOUNT", mountPort, srcPort, rpcMount, 1, authUnix, nullTimeout, red)
		okN := probeRPCNull(ip, "NFS", nfsPort, srcPort, rpcNFS, 2, authUnix, nullTimeout, red)
		if !okM && !okN {
			fmt.Println("  note: mountd/nfsd gave no UDP reply while portmap answered (rekordbox may ignore loopback/self-IP or require another LAN peer / -src <1024)")
		}
	} else {
		for rExp.err == nil && rExp.u32() == 1 {
			raw := append([]byte(nil), rExp.opaque(4096)...)
			dec, enc := decodeWireName(raw)
			var groups []string
			for rExp.err == nil && rExp.u32() == 1 {
				g := string(rExp.opaque(1024))
				if gip := net.ParseIP(strings.TrimSpace(g)); gip != nil {
					groups = append(groups, red.ip(gip))
				} else {
					groups = append(groups, red.text(g))
				}
			}
			exports = append(exports, exportEntry{raw: raw, decoded: dec, enc: enc})
			nulTerm := len(raw) > 0 && raw[len(raw)-1] == 0
			fmt.Printf("  export: path=%q enc=%s raw_len=%d nul_term=%v groups=%v\n",
				red.path(dec), enc, len(raw), nulTerm, groups)
		}
	}

	// MOUNTPROC_MNT (proc 1) - read-only FH lookup, never changes OS mounts.
	mntCandidates := []struct {
		label   string
		decoded string
		raw     []byte
	}{}
	for _, e := range exports {
		mntCandidates = append(mntCandidates, struct {
			label   string
			decoded string
			raw     []byte
		}{label: fmt.Sprintf("%s(%s)", red.path(e.decoded), e.enc), decoded: e.decoded, raw: e.raw})
	}
	if len(mntCandidates) == 0 {
		mntCandidates = append(mntCandidates,
			struct {
				label   string
				decoded string
				raw     []byte
			}{label: "/(utf16le)", decoded: "/", raw: encodeUTF16LE("/")},
			struct {
				label   string
				decoded string
				raw     []byte
			}{label: "/(utf8)", decoded: "/", raw: []byte("/")},
		)
	}

	var rootFH []byte
	var mountedExport string
	for _, cand := range mntCandidates {
		args := (&xdrBuf{}).opaque(cand.raw).Bytes()
		rMnt, err := rpcCallUDP(ip, mountPort, srcPort, rpcMount, 1, 1, authUnix, args, timeout)
		if err != nil {
			fmt.Printf("MOUNT MNT %s (authUnix=%v): ERROR: %s\n", cand.label, authUnix, red.err(err))
			if authUnix {
				if r2, err2 := rpcCallUDP(ip, mountPort, srcPort, rpcMount, 1, 1, false, args, timeout); err2 == nil {
					rMnt = r2
					authUnix = false
					fmt.Printf("MOUNT MNT %s retried with AUTH_NULL!\n", cand.label)
				} else {
					fmt.Printf("MOUNT MNT %s (AUTH_NULL): ERROR: %s\n", cand.label, red.err(err2))
				}
			}
		}
		if rMnt == nil {
			continue
		}
		st := rMnt.u32()
		if st != 0 {
			fmt.Printf("MOUNT MNT %s: NFS/MOUNT status=%d\n", cand.label, st)
			continue
		}
		fh := rMnt.fixed(fhSize)
		if rMnt.err != nil {
			fmt.Printf("MOUNT MNT %s: short fh: %s\n", cand.label, red.err(rMnt.err))
			continue
		}
		rootFH = append([]byte(nil), fh...)
		mountedExport = cand.decoded
		fmt.Printf("MOUNT MNT %s: OK (fh_nonzero=%v)\n", cand.label, !bytes.Equal(rootFH, make([]byte, fhSize)))
		break
	}
	if rootFH == nil {
		return
	}

	// NFSPROC_READDIR (proc 16) on rootFH (single packet, read-only).
	rdArgs := (&xdrBuf{}).u32(0) // placeholder
	rdArgs.Reset()
	rdArgs.Write(rootFH)
	rdArgs.u32(0).u32(1400)
	if rDir, err := rpcCallUDP(ip, nfsPort, srcPort, rpcNFS, 2, 16, authUnix, rdArgs.Bytes(), timeout); err != nil {
		fmt.Printf("NFS READDIR /: ERROR: %s\n", red.err(err))
	} else if st := rDir.u32(); st != 0 {
		fmt.Printf("NFS READDIR /: status=%d\n", st)
	} else {
		var entries []string
		for rDir.err == nil && rDir.u32() == 1 {
			_ = rDir.u32() // fileid
			nameRaw := rDir.opaque(1024)
			_ = rDir.fixed(4) // cookie
			dec, enc := decodeWireName(nameRaw)
			if len(entries) < 10 {
				entries = append(entries, fmt.Sprintf("%q(%s)", red.path("/"+dec), enc))
			}
		}
		fmt.Printf("NFS READDIR / (first %d): [%s]\n", len(entries), strings.Join(entries, ", "))
	}

	lookupPath := func(p string, utf16le bool) ([]byte, uint32, uint32, uint32, error) {
		fh := append([]byte(nil), rootFH...)
		var size, mtime uint32
		parts := strings.FieldsFunc(strings.ReplaceAll(p, `\`, "/"), func(r rune) bool { return r == '/' })
		if len(parts) == 0 {
			return nil, 0, 0, 0, errors.New("empty path")
		}
		for _, comp := range parts {
			if comp == "." || comp == ".." {
				return nil, 0, 0, 0, errors.New("unsafe path")
			}
			args := &xdrBuf{}
			args.Write(fh)
			if utf16le {
				args.opaque(encodeUTF16LE(comp))
			} else {
				args.opaque([]byte(comp))
			}
			r, err := rpcCallUDP(ip, nfsPort, srcPort, rpcNFS, 2, 4, authUnix, args.Bytes(), timeout)
			if err != nil {
				return nil, 0, 0, 0, err
			}
			if st := r.u32(); r.err != nil || st != 0 {
				return nil, 0, 0, st, fmt.Errorf("NFS status=%d err=%v", st, r.err)
			}
			fh = append([]byte(nil), r.fixed(fhSize)...)
			attr := r.fixed(68)
			if r.err != nil {
				return nil, 0, 0, 0, r.err
			}
			size = binary.BigEndian.Uint32(attr[20:])
			mtime = binary.BigEndian.Uint32(attr[52:])
		}
		return fh, size, mtime, 0, nil
	}

	// Probe whether export.pdb exists over rekordbox NFS.
	for _, pdbCand := range []string{"PIONEER/rekordbox/export.pdb", "export.pdb"} {
		_, sz, _, st16, err16 := lookupPath(pdbCand, true)
		if err16 == nil {
			fmt.Printf("NFS LOOKUP %s (utf16le): FOUND size=%d\n", pdbCand, sz)
		} else {
			_, sz8, _, st8, err8 := lookupPath(pdbCand, false)
			fmt.Printf("NFS LOOKUP %s: utf16le=(st=%d %s) utf8=(st=%d sz=%d %s)\n",
				pdbCand, st16, red.err(err16), st8, sz8, red.err(err8))
		}
	}

	// Deduplicate and probe up to 3 track paths from 0x2102.
	seenPaths := map[string]bool{}
	probed := 0
	for _, tp := range trackPaths {
		if tp == "" || seenPaths[tp] || probed >= 3 {
			continue
		}
		seenPaths[tp] = true
		probed++

		candidates := []struct {
			mode string
			path string
		}{{mode: "full", path: tp}}
		if mountedExport != "" && mountedExport != "/" {
			normExp := strings.TrimRight(strings.ReplaceAll(mountedExport, `\`, "/"), "/")
			normTP := strings.ReplaceAll(tp, `\`, "/")
			if strings.HasPrefix(normTP, normExp+"/") {
				candidates = append(candidates, struct {
					mode string
					path string
				}{mode: "rel_to_export", path: strings.TrimPrefix(normTP, normExp)})
			}
		}

		var fh []byte
		var sz, mtime uint32
		var encUsed, modeUsed string
		for _, c := range candidates {
			if fh16, sz16, mt16, st16, err16 := lookupPath(c.path, true); err16 == nil {
				fh, sz, mtime, encUsed, modeUsed = fh16, sz16, mt16, "utf16le", c.mode
				break
			} else if fh8, sz8, mt8, st8, err8 := lookupPath(c.path, false); err8 == nil {
				fh, sz, mtime, encUsed, modeUsed = fh8, sz8, mt8, "utf8", c.mode
				break
			} else {
				fmt.Printf("NFS LOOKUP track (%s) %s: utf16le=(st=%d %s) utf8=(st=%d %s)\n",
					c.mode, red.path(tp), st16, red.err(err16), st8, red.err(err8))
			}
		}
		if fh == nil {
			continue
		}
		fmt.Printf("NFS LOOKUP track (%s) %s: OK enc=%s size=%d mtime=%d\n", modeUsed, red.path(tp), encUsed, sz, mtime)

		// Read only first 64 bytes to confirm audio data transfer works.
		readArgs := &xdrBuf{}
		readArgs.Write(fh)
		readArgs.u32(0).u32(64).u32(0)
		rr, err := rpcCallUDP(ip, nfsPort, srcPort, rpcNFS, 2, 6, authUnix, readArgs.Bytes(), timeout)
		if err != nil {
			fmt.Printf("NFS READ(0, 64): ERROR: %s\n", red.err(err))
			continue
		}
		if st := rr.u32(); st != 0 {
			fmt.Printf("NFS READ(0, 64): status=%d\n", st)
			continue
		}
		_ = rr.fixed(68)
		data := rr.opaque(128)
		if rr.err != nil {
			fmt.Printf("NFS READ(0, 64): decode error: %s\n", red.err(rr.err))
			continue
		}
		head := data[:min(len(data), 8)]
		fmt.Printf("NFS READ(0, 64): OK bytes=%d magic=%q head_hex=%s\n", len(data), printableASCII(head), hex.EncodeToString(head))
	}
}

func printableASCII(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c <= 0x7e {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}
