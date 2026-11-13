package webui

// GET /api/prolink/diag (console login) and /v1/enginex/prolink/diag (the deck
// itself): a self-check for the PRIME 4 <-> CDJ-3000 beta test (#59, #63).
// It reads settings, interfaces, sockets and discovered peers only; no account
// file, token, cookie, Wi-Fi key, serial number or MAC address goes into it.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

// procNetDir is /proc/net; tests point it at fixtures.
var procNetDir = "/proc/net"

type diagIface struct {
	Name      string `json:"name"`
	Up        bool   `json:"up"`
	IPv4      string `json:"ipv4"`
	Netmask   string `json:"netmask"`
	LinkLocal bool   `json:"linkLocal"` // 169.254.x.x: a direct cable without DHCP
	Peers     int    `json:"peers"`     // discovered devices in this subnet
}

type diagPort struct {
	Proto     string `json:"proto"`
	Port      int    `json:"port"`
	Role      string `json:"role"`
	Listening bool   `json:"listening"`
}

type diagPeer struct {
	Number     byte    `json:"number"`
	Name       string  `json:"name"`
	Address    string  `json:"address"`
	AgeSec     float64 `json:"ageSec"` // since the last packet
	SameSubnet bool    `json:"sameSubnet"`
	BPM        float64 `json:"bpm,omitempty"`
	Playing    bool    `json:"playing"`
	Master     bool    `json:"master"`
	OnAir      bool    `json:"onAir"`
	Sync       bool    `json:"sync"`
	NFS        string  `json:"nfs,omitempty"` // CDJ/XDJ media server: "port N" or the error
}

type diagCheck struct {
	Level  string `json:"level"` // PASS, WARN, FAIL
	Item   string `json:"item"`
	Detail string `json:"detail"`
}

type prolinkDiag struct {
	Version  string    `json:"version"`
	Board    string    `json:"board"`
	Model    string    `json:"model"`
	Time     time.Time `json:"time"`
	Settings struct {
		ProlinkShare       bool   `json:"prolinkShare"`
		ProlinkExport      bool   `json:"prolinkExport"`
		FourDeckWaveform   bool   `json:"fourDeckWaveform"`
		FourDeckRoleActive string `json:"fourDeckRoleActive"`
	} `json:"settings"`
	Export      prolink.ExportStatus `json:"export"`
	ExportNFS   string               `json:"exportNfs,omitempty"`
	Interfaces  []diagIface          `json:"interfaces"`
	Ports       []diagPort           `json:"ports"`
	Peers       []diagPeer           `json:"peers"`
	ShadowDecks int                  `json:"shadowDecks"`
	Checks      []diagCheck          `json:"checks"`
}

func (s *Server) prolinkDiagGet(w http.ResponseWriter, r *http.Request) {
	d := s.prolinkDiag(r.Context(), time.Now())
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprint(w, d.markdown())
		return
	}
	respond(w, d)
}

func (s *Server) prolinkDiag(ctx context.Context, now time.Time) prolinkDiag {
	var d prolinkDiag
	d.Time = now.UTC().Truncate(time.Second)
	d.Version = s.installedVersion()
	for _, c := range deviceCompatible() {
		if p := imagePrefixes[c]; p != "" {
			d.Model, d.Board = p, strings.ToUpper(strings.TrimPrefix(c, "inmusic,"))
			break
		}
	}
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	d.Settings.ProlinkShare, d.Settings.ProlinkExport, d.Settings.FourDeckWaveform = v.ProlinkShare, v.ProlinkExport, v.FourDeckWaveform
	d.Settings.FourDeckRoleActive = s.activeRole(v)
	if s.prolinkExport != nil {
		d.Export = s.prolinkExport.Status()
	}
	if role := d.Settings.FourDeckRoleActive; role != "" {
		s.shadow.mu.Lock()
		lines := slices.Clone(s.shadow.lines)
		s.shadow.mu.Unlock()
		d.ShadowDecks = len(s.shadowCompose(role, now, lines))
	}

	var nets []*net.IPNet
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			nets = append(nets, n)
			d.Interfaces = append(d.Interfaces, diagIface{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0, IPv4: n.IP.String(), Netmask: net.IP(n.Mask).String(), LinkLocal: n.IP.IsLinkLocalUnicast()})
		}
	}

	for _, dev := range prolink.BuildSnapshot(s.mergedDevices()).Devices {
		p := diagPeer{Number: dev.Number, Name: dev.Name, Address: dev.Address, BPM: dev.EffectiveBPM, Playing: dev.Playing, Master: dev.Master, OnAir: dev.OnAir, Sync: dev.Sync}
		if !dev.LastSeen.IsZero() {
			p.AgeSec = now.Sub(dev.LastSeen).Round(100 * time.Millisecond).Seconds()
		}
		if ip := net.ParseIP(dev.Address); ip != nil {
			for i, n := range nets {
				if n.Contains(ip) {
					p.SameSubnet = true
					d.Interfaces[i].Peers++
				}
			}
		}
		d.Peers = append(d.Peers, p)
	}

	// Ask each Pioneer player's portmapper, and our own export's, where NFS listens.
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	probe := func(out *string, ip net.IP, pm int) {
		defer wg.Done()
		if port, err := (&prolink.NFSClient{IP: ip, Portmap: pm, Timeout: 500 * time.Millisecond}).NFSPort(ctx); err != nil {
			*out = "error: " + err.Error()
		} else {
			*out = "port " + strconv.Itoa(port)
		}
	}
	for i, p := range d.Peers {
		if ip := net.ParseIP(p.Address).To4(); ip != nil && pioneerPlayer(p.Name) {
			wg.Add(1)
			go probe(&d.Peers[i].NFS, ip, 0)
		}
	}
	if host, _, err := net.SplitHostPort(d.Export.DBAddress); d.Export.Enabled && err == nil {
		wg.Add(1)
		go probe(&d.ExportNFS, net.ParseIP(host), d.Export.PortmapPort)
	}
	wg.Wait()

	listening := listeningSockets(procNetDir)
	ports := []diagPort{{"udp", 50000, "announce / keepalive", false}, {"udp", 50001, "beat", false}, {"udp", 50002, "status", false},
		{"udp", 111, "portmapper (export)", false}, {"tcp", 12523, "dbserver port query (export)", false}}
	if _, p, err := net.SplitHostPort(d.Export.DBAddress); err == nil && p != "12523" {
		n, _ := strconv.Atoi(p)
		ports = append(ports, diagPort{"tcp", n, "dbserver (export)", false})
	}
	for i := range ports {
		ports[i].Listening = listening[ports[i].Proto+"/"+strconv.Itoa(ports[i].Port)]
	}
	d.Ports = ports
	d.Checks = d.checks()
	return d
}

func pioneerPlayer(name string) bool {
	n := strings.ToUpper(name)
	return strings.HasPrefix(n, "CDJ") || strings.HasPrefix(n, "XDJ")
}

// listeningSockets reads /proc/net/{tcp,udp}{,6}: "udp/50000" -> bound, "tcp/N" -> LISTEN.
func listeningSockets(dir string) map[string]bool {
	out := map[string]bool{}
	for _, name := range []string{"tcp", "tcp6", "udp", "udp6"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			// sl local_address rem_address st ...
			fields := strings.Fields(sc.Text())
			if len(fields) < 4 || !strings.Contains(fields[1], ":") {
				continue
			}
			if strings.HasPrefix(name, "tcp") && fields[3] != "0A" {
				continue
			}
			port, err := strconv.ParseUint(fields[1][strings.LastIndex(fields[1], ":")+1:], 16, 16)
			if err == nil {
				out[name[:3]+"/"+strconv.Itoa(int(port))] = true
			}
		}
		f.Close()
	}
	return out
}

func (d prolinkDiag) checks() []diagCheck {
	var c []diagCheck
	add := func(level, item, detail string) { c = append(c, diagCheck{level, item, detail}) }
	onOff := func(on bool, item, hint string) {
		if on {
			add("PASS", item, "On")
		} else {
			add("WARN", item, "Off: "+hint)
		}
	}
	onOff(d.Settings.ProlinkShare, "Pro DJ Link Library", "turn on in Settings -> Services to browse CDJ media")
	onOff(d.Settings.ProlinkExport, "Pro DJ Link Export to CDJ (Beta)", "turn on in Settings -> Services to show this deck's USB/SD on CDJs")
	if d.Settings.ProlinkExport {
		if d.Export.Enabled {
			add("PASS", "Export running", fmt.Sprintf("player %d, media %s, NFS %s", d.Export.Number, d.Export.Root, d.ExportNFS))
		} else {
			add("FAIL", "Export running", "switched on but not running: insert a USB/SD with a Rekordbox export or an Engine library, or another program holds UDP 111 / TCP 12523")
		}
	}
	if d.Settings.FourDeckWaveform && d.Settings.FourDeckRoleActive == "" {
		add("WARN", "4-deck role", "4-deck waveform is on but Engine has not applied a role yet: restart Engine")
	}

	up := 0
	for _, i := range d.Interfaces {
		if i.Up {
			up++
		}
	}
	if up == 0 {
		add("FAIL", "Network", "no interface has an IPv4 address: check the LAN cable")
	} else {
		add("PASS", "Network", fmt.Sprintf("%d interface(s) with IPv4", up))
	}

	for _, p := range d.Ports {
		switch {
		case p.Listening:
			add("PASS", fmt.Sprintf("%s %d", p.Proto, p.Port), p.Role)
		case p.Port >= 50000:
			add("FAIL", fmt.Sprintf("%s %d", p.Proto, p.Port), p.Role+": nothing listens, check rb-bridge / stream-sync")
		case d.Export.Enabled:
			add("FAIL", fmt.Sprintf("%s %d", p.Proto, p.Port), p.Role+": export runs but the socket is missing")
		}
	}

	if len(d.Peers) == 0 {
		add("WARN", "Discovery", "no other Pro DJ Link device seen: same switch/cable, CDJ powered on, LINK enabled")
	}
	numbers := map[byte]bool{}
	for _, p := range d.Peers {
		item := fmt.Sprintf("Peer %d %s", p.Number, p.Name)
		switch {
		case !p.SameSubnet:
			add("FAIL", item, p.Address+" is outside every local subnet: use one switch, DHCP everywhere or link-local everywhere")
		case p.AgeSec > 5:
			add("WARN", item, fmt.Sprintf("last packet %.0f s ago", p.AgeSec))
		default:
			add("PASS", item, p.Address)
		}
		if strings.HasPrefix(p.NFS, "error") {
			add("FAIL", item+" NFS", p.NFS)
		}
		if p.Number >= 1 && p.Number <= 6 {
			if numbers[p.Number] || d.Export.Enabled && p.Number == d.Export.Number {
				add("FAIL", "Player numbers", fmt.Sprintf("player %d is used twice: set the CDJ to 3 or 4", p.Number))
			}
			numbers[p.Number] = true
		}
	}
	return c
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// markdown is the report a tester pastes into a GitHub issue.
func (d prolinkDiag) markdown() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("### EngineX OS Pro DJ Link diagnostic\n\n")
	p("| | |\n|---|---|\n| Version | %s |\n| Model / board | %s / %s |\n| Time (UTC) | %s |\n", d.Version, d.Model, d.Board, d.Time.Format(time.RFC3339))
	p("| prolinkShare | %s |\n| prolinkExport | %s |\n| fourDeckWaveform | %s |\n| fourDeckRoleActive | %s |\n", yes(d.Settings.ProlinkShare), yes(d.Settings.ProlinkExport), yes(d.Settings.FourDeckWaveform), d.Settings.FourDeckRoleActive)
	p("| Export | running=%s player=%d NFS=%s |\n| Shadow decks | %d |\n\n", yes(d.Export.Enabled), d.Export.Number, d.ExportNFS, d.ShadowDecks)
	p("#### Checks\n\n| Result | Item | Detail |\n|---|---|---|\n")
	for _, c := range d.Checks {
		p("| %s | %s | %s |\n", c.Level, c.Item, c.Detail)
	}
	p("\n#### Interfaces\n\n| Name | Up | IPv4 | Netmask | Link-local | Peers |\n|---|---|---|---|---|---|\n")
	for _, i := range d.Interfaces {
		p("| %s | %s | %s | %s | %s | %d |\n", i.Name, yes(i.Up), i.IPv4, i.Netmask, yes(i.LinkLocal), i.Peers)
	}
	p("\n#### Pro DJ Link devices\n\n| # | Name | Address | Seen (s ago) | BPM | Playing | Master | On-Air | Sync | NFS |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, x := range d.Peers {
		p("| %d | %s | %s | %.1f | %.2f | %s | %s | %s | %s | %s |\n", x.Number, x.Name, x.Address, x.AgeSec, x.BPM, yes(x.Playing), yes(x.Master), yes(x.OnAir), yes(x.Sync), x.NFS)
	}
	return b.String()
}
