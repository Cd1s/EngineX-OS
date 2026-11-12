package prolink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// ExportConfig selects one mounted Rekordbox USB. Empty Root selects the first
// mounted export under /media. Address overrides allow isolated loopback tests.
type ExportConfig struct {
	Root                                                         string
	IP                                                           net.IP
	MAC                                                          net.HardwareAddr
	DBAddr, PortmapAddr, DiscoveryAddr, StatusAddr, AnnounceAddr string
	ProbeDuration                                                time.Duration
	// Observe receives valid status packets captured on the shared status port.
	Observe    func(Device)
	MediaCache *MediaCache // optional incoming status enrichment; allocated only while ON
	// BeatAddr also feeds Observe with CDJ-3000 absolute position packets (0x0b,
	// every 30 ms) for #49 shadow decks; default 0.0.0.0:50001, shared with rb-bridge.
	BeatAddr string
	// Peers lists Pro DJ Link devices seen on the LAN; the export binds to the
	// interface that reaches them (see exportIdentity).
	Peers func() []net.IP
}

type ExportStatus struct {
	Enabled          bool   `json:"enabled"`
	Number           byte   `json:"number"`
	Root             string `json:"root,omitempty"`
	DBAddress        string `json:"dbAddress,omitempty"`
	PortmapPort      int    `json:"portmapPort,omitempty"`
	DiscoveryAddress string `json:"discoveryAddress,omitempty"`
	StatusAddress    string `json:"statusAddress,omitempty"`
}

// Exporter is inert until SetEnabled(true): constructing it, asking its status,
// or disabling it binds no sockets and starts no goroutines or timers.
type Exporter struct {
	mu   sync.Mutex
	cfg  ExportConfig
	run  *exportRun
	deck ExportDeck
}

// ExportDeck is what the status packets say about the decks this player plays
// (#49): playing, tempo, and whether one of them is tempo master. Without it
// the export looks like an idle player.
type ExportDeck struct {
	Playing, Master bool
	BPM, Pitch      float64 // the track's BPM; pitch in percent
	// Beat is Engine's BeatNumber when read at At: beats into the bar, 0..4
	// (the integer part is the beat, the fraction the phase). As master and
	// playing, the export sends a beat packet (0x28) on every beat from it.
	Beat float64
	At   time.Time
}

// beatAt is where the master deck is in its bar at now: the beat (0..3) and
// how far into it (0..1). ok is false when it is not a playing master.
func (d ExportDeck) beatAt(now time.Time) (beat int, frac float64, ok bool) {
	bpm := d.BPM * (1 + d.Pitch/100)
	if !d.Master || !d.Playing || bpm <= 0 || d.At.IsZero() || d.Beat < 0 || now.Sub(d.At) > 2*time.Second {
		return 0, 0, false
	}
	pos := d.Beat + now.Sub(d.At).Seconds()*bpm/60
	whole := math.Floor(pos)
	return int(whole) % 4, pos - whole, true
}

// Carried is d read again at now: the beat moved on with the tempo, so a deck
// that has not changed since At keeps its beat packets (beatAt gives up after 2 s).
func (d ExportDeck) Carried(now time.Time) ExportDeck {
	if d.At.IsZero() {
		return d
	}
	if bpm := d.BPM * (1 + d.Pitch/100); d.Playing && bpm > 0 && d.Beat >= 0 {
		d.Beat = math.Mod(d.Beat+now.Sub(d.At).Seconds()*bpm/60, 4)
	}
	d.At = now
	return d
}

// beatPacket is a Pro DJ Link beat (0x28): beat in bar (1..4) just hit, the
// time to the following beats and bars, pitch and BPM.
func (r *exportRun) beatPacket(d ExportDeck, inBar int, frac float64) []byte {
	b := linkPacket(0x28, 0x60, 11)
	b[0x21], b[0x5f] = r.player, r.player
	binary.BigEndian.PutUint16(b[0x22:], 0x3c)
	ms := 60000 / (d.BPM * (1 + d.Pitch/100))
	next := (1 - frac) * ms
	for i, beats := range []float64{0, 1, float64(3 - inBar), 3, float64(7 - inBar), 7} { // next beat, 2nd beat, next bar, 4th beat, 2nd bar, 8th beat
		binary.BigEndian.PutUint32(b[0x24+4*i:], uint32(math.Round(next+beats*ms)))
	}
	for i := 0x3c; i < 0x54; i++ {
		b[i] = 0xff
	}
	binary.BigEndian.PutUint32(b[0x54:], uint32(math.Round(0x100000*(1+d.Pitch/100))))
	binary.BigEndian.PutUint16(b[0x5a:], uint16(math.Round(d.BPM*100)))
	b[0x5c] = byte(inBar + 1)
	return b
}

// beatLoop sends a beat packet to the players each time the master deck
// crosses a beat.
func (r *exportRun) beatLoop() {
	defer r.wg.Done()
	// Idle: poll slowly. Only a deck playing as master needs beat-accurate 5 ms polling.
	const idlePoll, playPoll = 50 * time.Millisecond, 5 * time.Millisecond
	tick := time.NewTicker(idlePoll)
	defer tick.Stop()
	fast := false
	last := -1
	for {
		select {
		case <-r.done:
			return
		case now := <-tick.C:
			r.mu.Lock()
			beat, frac, ok := r.deck.beatAt(now)
			if ok != fast {
				fast = ok
				if ok {
					tick.Reset(playPoll)
				} else {
					tick.Reset(idlePoll)
				}
			}
			if !ok {
				last = -1
			} else if beat != last {
				if last >= 0 && r.player != 0 { // not on the first sample: that is mid-beat
					pkt := r.beatPacket(r.deck, beat, frac)
					for _, ip := range r.peerIPsLocked() {
						r.sendLocked(r.beatConn(), pkt, &net.UDPAddr{IP: ip, Port: 50001})
					}
				}
				last = beat
			}
			r.mu.Unlock()
		}
	}
}

// SetDeck updates the status packets. Losing master ends a handoff we granted.
func (e *Exporter) SetDeck(d ExportDeck) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deck = d
	if e.run != nil {
		e.run.mu.Lock()
		e.run.deck = d
		if !d.Master {
			e.run.handoff = 0
		}
		e.run.mu.Unlock()
	}
}

// CachedAnalysis exposes only the already observed track; it cannot start export
// or perform network I/O, and never takes the exporter lifecycle mutex.
func (e *Exporter) CachedAnalysis(dev Device, host net.IP) (*rekordbox.Analysis, string, string) {
	return e.cfg.MediaCache.CachedAnalysis(dev, host)
}

// DisplayMediaCache is immutable configuration. Display polling never takes the
// lifecycle mutex, which may be waiting for network workers during shutdown.
func (e *Exporter) DisplayMediaCache() *MediaCache { return e.cfg.MediaCache }

// RequestMaster asks the tempo master at ip to hand master over to this player
// (Pro DJ Link master handoff request, type 0x26, to its beat port).
func (e *Exporter) RequestMaster(ip net.IP) error {
	e.mu.Lock()
	r := e.run
	e.mu.Unlock()
	if r == nil {
		return errors.New("prolink: export is off")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.player == 0 {
		return errors.New("prolink: no player number yet")
	}
	r.sendLocked(r.beatConn(), r.handoffPacket(0x26, 0x28), &net.UDPAddr{IP: ip, Port: 50001})
	return nil
}

// handoffPacket is a master handoff request (0x26) or response (0x27, yes).
func (r *exportRun) handoffPacket(kind byte, length int) []byte {
	b := linkPacket(kind, length, 11)
	b[0x21], b[0x27] = r.player, r.player
	binary.BigEndian.PutUint16(b[0x22:], 8) // as Beat Link sends both
	if kind == 0x27 {
		b[0x2b] = 1
	}
	return b
}

func (r *exportRun) beatConn() *net.UDPConn {
	if r.beat != nil {
		return r.beat
	}
	return r.status
}

func NewExporter(cfg ExportConfig) *Exporter { return &Exporter{cfg: cfg} }
func (e *Exporter) SetEnabled(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !on {
		if e.run != nil {
			e.run.close()
			e.run = nil
		}
		return nil
	}
	if e.run != nil {
		return nil
	}
	r, err := startExport(e.cfg)
	if err != nil {
		return err
	}
	r.deck = e.deck
	e.run = r
	return nil
}
func (e *Exporter) Close() { _ = e.SetEnabled(false) }

// Rebind restarts a running export when the players moved to another interface.
func (e *Exporter) Rebind() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return nil
	}
	ip, _, _, _, err := exportIdentity(e.cfg)
	if err != nil || ip.Equal(e.run.ip) {
		return nil
	}
	e.run.close()
	e.run = nil
	r, err := startExport(e.cfg)
	if err != nil {
		return err
	}
	e.run = r
	return nil
}
func (e *Exporter) Status() ExportStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return ExportStatus{}
	}
	r := e.run
	return ExportStatus{Enabled: true, Number: r.number(), Root: r.nfs.Root, DBAddress: r.db.Addr().String(), PortmapPort: r.nfs.PortmapPort(), DiscoveryAddress: r.discovery.LocalAddr().String(), StatusAddress: r.status.LocalAddr().String()}
}

// TrackPath is the file behind track id of the export this deck serves (the id
// a CDJ reports after loading from it), or "" when it is not exported.
func (e *Exporter) TrackPath(id uint32) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.run == nil {
		return ""
	}
	for _, t := range e.run.lib.Tracks {
		if t.ID == id {
			if f := StreamFile(t.Path); f != "" {
				return f
			}
			if rel, ok := SafeRel(t.Path); ok {
				return filepath.Join(e.run.nfs.Root, rel)
			}
			return ""
		}
	}
	return ""
}

type exportPeer struct {
	number byte
	name   string
	seen   time.Time
	ip     net.IP
	first  time.Time // its first keepalive
	heard  bool      // it has sent us a status packet
}

// exportFirmware is the least firmware version the status packets announce.
const exportFirmware = "3.30"

// firmwareWait is how long a new player gets no status from us while we have
// not read its own (it sends one every 200 ms once it has our keepalive).
const firmwareWait = 500 * time.Millisecond

type exportRun struct {
	nfs               *NFSServer
	db                *DBServer
	discovery, status *net.UDPConn
	beat              *net.UDPConn // nil without Observe or when the port is taken
	deck              ExportDeck
	handoff           byte // the player we agreed to hand master to
	ip                net.IP
	mac               net.HardwareAddr
	announce          *net.UDPAddr
	media             *net.UDPAddr
	lib               *rekordbox.Library
	pdb               string
	pdbInfo           os.FileInfo
	capacity, free    uint64
	mu                sync.Mutex
	peers             map[string]exportPeer
	player            byte
	probed            bool
	mounted           bool
	first             bool
	statusCounter     uint32
	firmware          string    // the newest firmware a player on the link reported, "" before any
	announced         time.Time // since when the status loop has a player number
	floodUntil        time.Time
	observe           func(Device)
	observer          *Discovery
	updates           chan Device
	cancel            context.CancelFunc
	mediaCache        *MediaCache
	network           *net.IPNet
	done              chan struct{}
	wake              chan struct{}
	wg                sync.WaitGroup
}

// MountedExports does no polling and accepts read-only mounts too. Exporting
// never changes Engine's database or the Rekordbox drive.
func MountedExports() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var roots []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "/media/") {
			continue
		}
		root := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(fields[1])
		pdb, pdbErr := os.Stat(filepath.Join(root, "PIONEER", "rekordbox", "export.pdb"))
		mdb, mdbErr := os.Stat(filepath.Join(root, "Engine Library", "Database2", "m.db"))
		// A rekordbox export, or an Engine library the bridge exports for CDJs (#63).
		if pdbErr == nil && pdb.Mode().IsRegular() || mdbErr == nil && mdb.Mode().IsRegular() {
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	// No USB library: still export the streaming downloads (created by startExport).
	if len(roots) == 0 && len(scanStreams()) > 0 {
		roots = []string{StreamExportRoot}
	}
	return roots
}

// Both the existing passive observer and the optional exporter need broadcast
// discovery. SO_REUSEADDR delivers broadcasts to both; no export socket exists
// until enabled. Unicast media requests go to the export listener.
func listenLinkUDP(address string, broadcast bool) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if e := c.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if err == nil && broadcast {
				err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			}
		}); e != nil {
			return e
		}
		return err
	}}
	p, err := lc.ListenPacket(context.Background(), "udp4", address)
	if err != nil {
		return nil, err
	}
	return p.(*net.UDPConn), nil
}

// exportIdentity picks the interface to export on: the one whose subnet holds a
// Pro DJ Link device we have seen (cfg.Peers, our own addresses ignored), else
// the first usable one. A deck on Wi-Fi with a bare link-local eth0 then follows
// the players to Wi-Fi, and a booth cable keeps eth0.
func exportIdentity(cfg ExportConfig) (net.IP, net.HardwareAddr, net.IP, *net.IPNet, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	explicit := cfg.IP != nil && !cfg.IP.IsUnspecified()
	var peers []net.IP
	if cfg.Peers != nil && !explicit {
		peers = cfg.Peers()
	}
	var first func() (net.IP, net.HardwareAddr, net.IP, *net.IPNet, error)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			n, ok := address.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip := n.IP.To4()
			if explicit {
				if !ip.Equal(cfg.IP) {
					// Explicit loopback identities can use isolated 127/8 test
					// addresses without adding system interface aliases.
					if len(cfg.MAC) == 6 && cfg.IP.IsLoopback() && ip.IsLoopback() && n.Contains(cfg.IP) {
						ip = cfg.IP.To4()
					} else {
						continue
					}
				}
			} else if ip.IsLoopback() || len(iface.HardwareAddr) != 6 {
				continue
			}
			identity := func() (net.IP, net.HardwareAddr, net.IP, *net.IPNet, error) {
				mac := iface.HardwareAddr
				if len(cfg.MAC) != 0 {
					mac = cfg.MAC
				}
				if len(mac) != 6 {
					return nil, nil, nil, nil, errors.New("prolink: export needs a six-byte interface MAC")
				}
				broadcast := append(net.IP(nil), ip...)
				mask := n.Mask
				if len(mask) == 16 {
					mask = mask[12:]
				}
				for i := range broadcast {
					broadcast[i] |= ^mask[i]
				}
				return append(net.IP(nil), ip...), append(net.HardwareAddr(nil), mac...), broadcast, &net.IPNet{IP: ip.Mask(mask), Mask: mask}, nil
			}
			if len(peers) == 0 {
				return identity()
			}
			for _, p := range peers {
				if p = p.To4(); p != nil && !p.Equal(ip) && n.Contains(p) {
					return identity()
				}
			}
			if first == nil {
				first = identity
			}
		}
	}
	if first != nil {
		return first()
	}
	return nil, nil, nil, nil, errors.New("prolink: no IPv4 interface for export")
}

func startExport(cfg ExportConfig) (*exportRun, error) {
	root := cfg.Root
	if root == "" {
		roots := MountedExports()
		if len(roots) == 0 {
			return nil, errors.New("prolink: mount a Rekordbox USB export first")
		}
		root = roots[0]
		if root == StreamExportRoot {
			if err := os.MkdirAll(root, 0755); err != nil {
				return nil, err
			}
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if st, err := os.Lstat(root); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("prolink: export root must be a real directory")
	}
	// An Engine-only drive gets an export.pdb for the CDJs; a real rekordbox export is left alone.
	if err := SyncEngineToPioneerExport(root); err != nil {
		return nil, err
	}
	nfs := &NFSServer{Root: root, StreamDirs: StreamDirs}
	rel := ""
	for _, part := range []string{"PIONEER", "rekordbox", "export.pdb"} {
		var status uint32
		rel, status = nfs.child(rel, part)
		if status != nfsOK {
			return nil, errors.New("prolink: invalid export.pdb path")
		}
		if _, status = nfs.stat(rel); status != nfsOK {
			return nil, errors.New("prolink: export.pdb must not follow symlinks")
		}
	}
	pdb := filepath.Join(root, rel)
	fs, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer fs.Close()
	f, err := fs.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("prolink: export.pdb must be a regular file")
	}
	lib, err := rekordbox.ParsePDB(f, st.Size())
	if err != nil {
		return nil, err
	}
	if err := lib.Validate(); err != nil {
		return nil, err
	}
	for i := range lib.Tracks {
		t := &lib.Tracks[i]
		for _, p := range []*string{&t.Path, &t.AnalysisPath} {
			if *p == "" {
				continue
			}
			rel, ok := SafeRel(*p)
			if !ok {
				return nil, fmt.Errorf("prolink: unsafe path in track %d", t.ID)
			}
			*p = "/" + filepath.ToSlash(rel)
		}
	}
	ip, mac, broadcast, network, err := exportIdentity(cfg)
	if err != nil {
		return nil, err
	}
	address := func(given string, port string) string {
		if given != "" {
			return given
		}
		return net.JoinHostPort(ip.String(), port)
	}
	r := &exportRun{network: network, observe: cfg.Observe, nfs: nfs, ip: ip, mac: mac, lib: lib, pdb: pdb, pdbInfo: st, peers: make(map[string]exportPeer), mounted: true, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	var disk syscall.Statfs_t
	if syscall.Statfs(root, &disk) == nil {
		r.capacity = uint64(disk.Blocks) * uint64(disk.Bsize)
		r.free = uint64(disk.Bavail) * uint64(disk.Bsize)
	}
	nfs.Available = r.mediaAvailable
	discoveryAddress := cfg.DiscoveryAddr
	if discoveryAddress == "" {
		discoveryAddress = "0.0.0.0:50000"
	}
	r.discovery, err = listenLinkUDP(discoveryAddress, true)
	if err != nil {
		return nil, err
	}
	r.status, err = listenLinkUDP(address(cfg.StatusAddr, "50002"), true)
	if err != nil {
		r.discovery.Close()
		return nil, err
	}
	announce := cfg.AnnounceAddr
	if announce == "" {
		announce = net.JoinHostPort(broadcast.String(), "50000")
	}
	r.announce, err = net.ResolveUDPAddr("udp4", announce)
	if err != nil {
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	r.media = &net.UDPAddr{IP: r.announce.IP, Port: 50002}
	if err = nfs.Listen(address(cfg.PortmapAddr, "111")); err != nil {
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	r.db = &DBServer{Library: lib, Number: r.number}
	if err = r.db.Listen(address(cfg.DBAddr, "12523")); err != nil {
		nfs.Close()
		r.status.Close()
		r.discovery.Close()
		return nil, err
	}
	probe := cfg.ProbeDuration
	if probe <= 0 {
		probe = 2500 * time.Millisecond // captured keepalive cadence can exceed two seconds
	}
	if cfg.Observe != nil {
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		if cfg.MediaCache != nil {
			cache := MediaCache{Context: ctx, Dir: cfg.MediaCache.Dir, Enabled: cfg.MediaCache.Enabled, Portmap: cfg.MediaCache.Portmap}
			r.mediaCache = &cache
			r.observer = NewDiscovery()
			r.observer.SetMediaCache(&cache)
		}
		r.updates = make(chan Device, 16)
		r.wg.Add(1)
		go r.observeLoop()
		if r.beat, err = listenLinkUDP(address(cfg.BeatAddr, "50001"), true); err != nil {
			log.Printf("prolink: no absolute positions on the beat port: %v", err)
			r.beat, err = nil, nil
		} else {
			r.wg.Add(1)
			go r.receiveBeats()
		}
	}
	r.wg.Add(4)
	go r.receivePeers()
	go r.receiveMedia()
	go r.announceLoop(probe)
	go r.beatLoop()
	return r, nil
}
func (r *exportRun) number() byte         { r.mu.Lock(); defer r.mu.Unlock(); return r.player }
func (r *exportRun) mediaAvailable() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.mounted }
func (r *exportRun) close() {
	close(r.done)
	if r.cancel != nil {
		r.cancel()
	}
	r.discovery.Close()
	r.status.Close()
	if r.beat != nil {
		r.beat.Close()
	}
	r.mu.Lock()
	r.player = 0
	r.mu.Unlock()
	r.db.Close()
	r.nfs.Close()
	r.wg.Wait()
}
func (r *exportRun) chooseLocked(now time.Time) {
	used := [5]bool{}
	for key, p := range r.peers {
		if now.Sub(p.seen) > 10*time.Second {
			delete(r.peers, key)
		} else {
			if p.number <= 4 {
				used[p.number] = true
			}
		}
	}
	old := r.player
	r.player = 0
	if !r.probed || !r.mounted || now.Before(r.floodUntil) {
		return
	}
	if old > 0 && !used[old] {
		r.player = old
		return
	}
	for n := byte(4); n > 0; n-- {
		if !used[n] {
			r.player = n
			return
		}
	}
}
func (r *exportRun) receivePeers() {
	defer r.wg.Done()
	b := make([]byte, 2048)
	for {
		n, addr, err := r.discovery.ReadFromUDP(b)
		if err != nil {
			return
		}
		d, err := ParsePacket(b[:n], 50000, addr.IP)
		if err != nil || d.Number < 1 || d.Number > 6 || !r.network.Contains(addr.IP) {
			continue
		}
		mac := b[0x26:0x2c]
		// Our own announcement, whichever interface it looped back on: a deck with
		// eth0 and wlan0 on one subnet hears it from its Wi-Fi address too, and
		// yielding to itself flipped numbers in a keepalive/media storm (#100).
		if bytes.Equal(mac, r.mac) && d.Name == "EngineX Export Beta" {
			continue
		}
		key := addr.IP.String() + "/" + net.HardwareAddr(mac).String()
		r.mu.Lock()
		if _, ok := r.peers[key]; !ok && len(r.peers) >= 64 {
			r.player = 0
			r.floodUntil = time.Now().Add(10 * time.Second) // resume after the bounded peer window expires
			r.mu.Unlock()
			r.db.closeClients()
			continue
		}
		p := exportPeer{number: d.Number, name: d.Name, seen: time.Now(), ip: append(net.IP(nil), addr.IP...)}
		if old, ok := r.peers[key]; ok {
			p.first, p.heard = old.first, old.heard
		} else {
			p.first = p.seen
		}
		r.peers[key] = p
		old := r.player
		// No disk, menu work, or periodic timer on the collision path.
		if old == d.Number {
			r.chooseLocked(time.Now())
		}
		changed := old != r.player
		r.mu.Unlock()
		if changed {
			r.db.closeClients()
			select {
			case r.wake <- struct{}{}:
			default:
			}
		}
	}
}
func (r *exportRun) receiveMedia() {
	defer r.wg.Done()
	b := make([]byte, 2048)
	for {
		n, addr, err := r.status.ReadFromUDP(b)
		if err != nil {
			return
		}
		if r.observe != nil {
			if d, err := ParsePacket(b[:n], 50002, addr.IP); err == nil && d.Number >= 1 && d.Number <= 6 && r.network.Contains(addr.IP) {
				select {
				case r.updates <- d:
				default:
				}
			}
		}
		if n < 0x30 || string(b[:10]) != magic {
			continue
		}
		r.mu.Lock()
		if r.network.Contains(addr.IP) {
			r.heardLocked(addr.IP, b[:n])
		}
		if b[10] == 5 && r.player != 0 && b[0x2b] == r.player && b[0x2f] == 3 {
			r.sendLocked(r.status, r.mediaPacket(), addr)
		}
		r.mu.Unlock()
	}
}

// heardLocked notes a packet from a player on the status port: it now knows us,
// and its status carries its firmware version (bytes 0x7c..0x7f, "1.44"). A
// CDJ-3000 shows "Linked to player with older firmware" for as long as a player
// announces a lower version than its own (#217), so ours is never below the
// newest one on the link. Caller holds r.mu.
func (r *exportRun) heardLocked(ip net.IP, b []byte) {
	for key, p := range r.peers {
		if !p.heard && p.ip.Equal(ip) {
			p.heard = true
			r.peers[key] = p
		}
	}
	if len(b) < 0x80 || b[10] != 0x0a {
		return
	}
	fw := b[0x7c:0x80]
	for i, c := range fw {
		if digit := c >= '0' && c <= '9'; i == 1 && c != '.' || i != 1 && !digit {
			return
		}
	}
	if string(fw) > r.firmware {
		r.firmware = string(fw)
	}
}

// receiveBeats passes CDJ-3000 absolute position packets straight to Observe:
// at 30 ms they filled the status queue and status packets (play/pause) were
// dropped, so a paused CDJ still looked like it was playing.
func (r *exportRun) receiveBeats() {
	defer r.wg.Done()
	b := make([]byte, 512)
	for {
		n, addr, err := r.beat.ReadFromUDP(b)
		if err != nil {
			return
		}
		if n >= 0x28 && string(b[:10]) == magic && b[10] == 0x26 && r.network.Contains(addr.IP) {
			// Another player asks for master: agree, and name it in our status
			// until Engine moves master to its shadow deck (SetDeck).
			r.mu.Lock()
			if r.player != 0 {
				r.sendLocked(r.beat, r.handoffPacket(0x27, 0x2c), &net.UDPAddr{IP: addr.IP, Port: 50001})
				r.handoff = b[0x21]
			}
			r.mu.Unlock()
			continue
		}
		if d, err := ParsePacket(b[:n], 50001, addr.IP); err == nil && d.PacketType == 0x0b && d.Number >= 1 && d.Number <= 6 && r.network.Contains(addr.IP) {
			r.observe(d)
		}
	}
}

func (r *exportRun) observeLoop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case d := <-r.updates:
			if r.observer != nil && (r.mediaCache.Enabled == nil || r.mediaCache.Enabled()) {
				ready := true
				if d.SourcePlayer != d.Number {
					r.mu.Lock()
					var source net.IP
					for _, p := range r.peers {
						if p.number == d.SourcePlayer {
							source = p.ip
							break
						}
					}
					r.mu.Unlock()
					if source == nil && d.SourcePlayer != 0 {
						ready = false
					}
					if source != nil {
						r.observer.Observe(Device{Number: d.SourcePlayer, Address: source.String(), PacketType: 6})
					}
				}
				if ready {
					d = r.observer.Observe(d)
				}
			}
			select {
			case <-r.done:
				return
			default:
			}
			r.observe(d)
		}
	}
}
func (r *exportRun) sendLocked(c *net.UDPConn, b []byte, to *net.UDPAddr) {
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	_, _ = c.WriteToUDP(b, to)
}
func (r *exportRun) announceLoop(probe time.Duration) {
	defer r.wg.Done()
	timer := time.NewTimer(probe)
	defer timer.Stop()
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	statusTicker := time.NewTicker(200 * time.Millisecond)
	defer statusTicker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-timer.C:
			r.mu.Lock()
			r.probed = true
			r.first = len(r.peers) == 0
			r.chooseLocked(time.Now())
			r.mu.Unlock()
		case <-statusTicker.C:
			r.sendStatus()
			continue
		case <-ticker.C:
			st, err := os.Stat(r.pdb)
			r.mu.Lock()
			old := r.player
			r.mounted = err == nil && os.SameFile(r.pdbInfo, st) && r.pdbInfo.ModTime().Equal(st.ModTime()) && r.pdbInfo.Size() == st.Size()
			r.chooseLocked(time.Now())
			changed := old != r.player
			r.mu.Unlock()
			if changed {
				r.db.closeClients()
			}
		case <-r.wake:
		}
		// Each send holds the identity lock for at most its 10ms deadline, so a
		// received claim cannot be followed by a queued old-number announcement.
		r.mu.Lock()
		if r.player != 0 {
			r.sendLocked(r.discovery, r.announcement(), r.announce)
		}
		r.mu.Unlock()
		r.mu.Lock()
		if r.player != 0 {
			r.sendLocked(r.status, r.mediaPacket(), r.media)
		}
		r.mu.Unlock()
	}
}
func linkPacket(kind byte, length, nameOffset int) []byte {
	b := make([]byte, length)
	copy(b, magic)
	b[10] = kind
	copy(b[nameOffset:nameOffset+20], "EngineX Export Beta")
	b[nameOffset+20] = 1
	return b
}
func (r *exportRun) announcement() []byte {
	b := linkPacket(6, 0x36, 12)
	b[0x21] = 2
	binary.BigEndian.PutUint16(b[0x22:], 0x36)
	b[0x24], b[0x25] = r.player, 1
	if r.first {
		b[0x25] = 2
	}
	copy(b[0x26:], r.mac)
	copy(b[0x2c:], r.ip.To4())
	b[0x30], b[0x34] = byte(min(len(r.peers)+1, 6)), 1
	return b
}
func (r *exportRun) mediaPacket() []byte {
	b := linkPacket(6, 0xc0, 11)
	b[0x21] = r.player
	binary.BigEndian.PutUint16(b[0x22:], 0x9c)
	b[0x27], b[0x2b] = r.player, 3
	u := utf16.Encode([]rune(filepath.Base(r.nfs.Root)))
	limit := min(len(u), 31)
	if limit > 0 && u[limit-1] >= 0xd800 && u[limit-1] <= 0xdbff {
		limit--
	}
	for i, n := range u[:limit] {
		binary.BigEndian.PutUint16(b[0x2c+2*i:], n)
	}
	binary.BigEndian.PutUint16(b[0xa6:], uint16(min(len(r.lib.Tracks), 65535)))
	b[0xaa] = 1
	binary.BigEndian.PutUint16(b[0xae:], uint16(min(len(r.lib.Playlists), 65535)))
	binary.BigEndian.PutUint64(b[0xb0:], r.capacity)
	binary.BigEndian.PutUint64(b[0xb8:], r.free)
	return b
}

// Real captures and Beat Link use 0x11c bytes for the extended legacy status
// layout (not 0x110, which is the offset of its final row). Advertise this
// common layout; native CDJ-3000 status extensions are not implemented.
func (r *exportRun) statusPacket() []byte {
	b := linkPacket(0x0a, 0x11c, 11)
	b[0x20], b[0x21], b[0x24], b[0x26] = 4, r.player, r.player, 1
	binary.BigEndian.PutUint16(b[0x22:], uint16(len(b)-0x24))
	b[0x6a], b[0x6b], b[0x6f], b[0x73] = 4, 4, 0, 4             // USB mounted, no SD
	b[0x75] = 1                                                 // LINK available while the exported USB is mounted
	binary.BigEndian.PutUint16(b[0x90:], 0x7fff)                // no loaded track / meaningful master BPM
	copy(b[0x7c:0x80], max(exportFirmware, r.firmware))         // never older than a player on the link (heardLocked)
	b[0x89], b[0x8b], b[0x9f], b[0xcc] = 0x84, 0x7e, 0xff, 0x0f // idle, no master/sync
	for _, off := range []int{0x8c, 0x98, 0xc0, 0xc4} {
		binary.BigEndian.PutUint32(b[off:], 0x00100000)
	}
	binary.BigEndian.PutUint16(b[0x92:], 0xffff)
	binary.BigEndian.PutUint32(b[0xa0:], 0xffffffff)
	if d := r.deck; d.Master || d.Playing {
		if d.Playing {
			b[0x89] |= 0x40
		}
		if d.Master {
			b[0x89] |= 0x20
		}
		if d.BPM > 0 {
			binary.BigEndian.PutUint16(b[0x90:], 0x8000)
			binary.BigEndian.PutUint16(b[0x92:], uint16(math.Round(d.BPM*100)))
		}
		if f := 1 + d.Pitch/100; f > 0 {
			binary.BigEndian.PutUint32(b[0x8c:], uint32(math.Round(0x100000*f)))
			binary.BigEndian.PutUint32(b[0x98:], uint32(math.Round(0x100000*f)))
		}
	}
	if r.handoff != 0 {
		b[0x9f] = r.handoff
	}
	if beat, _, ok := r.deck.beatAt(time.Now()); ok {
		b[0xa6] = byte(beat + 1)
	}
	r.statusCounter++
	binary.BigEndian.PutUint32(b[0xc8:], r.statusCounter)
	return b
}

// peerIPsLocked lists the players seen in the last 10 s (at most 6). For
// status packets (announced: since when we have a player number), only those
// whose firmware we could have read: a status packet with a lower version than
// the player's own would raise its warning. Caller holds r.mu.
func (r *exportRun) peerIPsLocked(announced ...time.Time) []net.IP {
	peers := make([]net.IP, 0, len(r.peers))
	seen := make(map[string]bool)
	for _, p := range r.peers {
		if len(announced) > 0 && !p.heard && time.Since(p.first) < firmwareWait+max(0, announced[0].Sub(p.first)) {
			continue
		}
		if time.Since(p.seen) <= 10*time.Second && !seen[p.ip.String()] && len(peers) < 6 {
			peers = append(peers, p.ip)
			seen[p.ip.String()] = true
		}
	}
	return peers
}

func (r *exportRun) sendStatus() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.player == 0 {
		r.announced = time.Time{}
		return
	}
	if r.announced.IsZero() {
		r.announced = time.Now()
	}
	pkt := r.statusPacket() // identical for every peer
	for _, ip := range r.peerIPsLocked(r.announced) {
		r.sendLocked(r.status, pkt, &net.UDPAddr{IP: ip, Port: 50002})
	}
}
