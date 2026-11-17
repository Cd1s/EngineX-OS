package webui

// rekordbox on a computer as an Engine Remote Library. Engine finds remote libraries
// ("Engine as a Service", what Engine DJ Desktop offers to players) by broadcasting
// "EAAS" 01 00 to UDP 11224 every 2 s; a library answers with its gRPC and HTTP
// addresses. While Pro DJ Link is on and rekordbox is on the link, the deck answers for
// the computer itself: the library lists rekordbox's collection and playlists (dbserver),
// and a track comes with rekordbox's beat grid, cues, loops and waveform, so Engine
// loads it without analysing it again and nothing writes Engine's databases from outside.
//
// Engine ignores an answer from the address it asked from, so the deck answers its own
// Engine from 127.0.0.1 and serves on 127.0.0.1 only: this deck's Engine is the only
// one offered the library. Engine's own Remote Library server holds 50010/50020.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"github.com/Cd1s/EngineX-OS/third_party/stagelinq-eaas/enginelibrary"
	"github.com/Cd1s/EngineX-OS/third_party/stagelinq-eaas/networktrust"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Variables for the tests.
var (
	eaasDiscoveryPort = 11224
	eaasGRPCAddr      = "127.0.0.1:50011"
	eaasHTTPAddr      = "127.0.0.1:50021"
	eaasFromHere      = fromThisDeck
)

var eaasRequest = []byte("EAAS\x01\x00")

type rbxEaas struct {
	s        *Server
	ip, name string
	token    uuid.UUID
	udp      *net.UDPConn
	grpc     *grpc.Server
	http     *http.Server
	mu       sync.Mutex
	files    map[uint32]string // track id -> file GetTrack found, for /download
}

// eaasLoop offers the rekordbox computer as a Remote Library while it is on the link.
func (s *Server) eaasLoop() {
	var cur *rbxEaas
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		host := s.rekordboxHost()
		if cur != nil && (host == nil || host.IP != cur.ip) {
			cur.stop()
			cur = nil
		}
		if cur == nil && host != nil {
			e, err := startEaas(s, host.IP, rbxName(host.IP))
			if err != nil {
				log.Printf("stream-sync: rekordbox remote library: %v", err)
			}
			cur = e
		}
		select {
		case <-s.closed:
			if cur != nil {
				cur.stop()
			}
			return
		case <-t.C:
		}
	}
}

func startEaas(s *Server, ip, name string) (*rbxEaas, error) {
	e := &rbxEaas{s: s, ip: ip, name: name, token: uuid.NewSHA1(uuid.NameSpaceOID, []byte("rekordbox "+ip)), files: map[uint32]string{}}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { // Engine's own beacon may hold the port too
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", eaasDiscoveryPort))
	if err != nil {
		return nil, err
	}
	answer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		pc.Close()
		return nil, err
	}
	gl, err := net.Listen("tcp4", eaasGRPCAddr)
	if err != nil {
		pc.Close()
		answer.Close()
		return nil, err
	}
	hl, err := net.Listen("tcp4", eaasHTTPAddr)
	if err != nil {
		pc.Close()
		answer.Close()
		gl.Close()
		return nil, err
	}
	e.udp = pc.(*net.UDPConn)
	e.grpc = grpc.NewServer(grpc.MaxSendMsgSize(32 << 20))
	enginelibrary.RegisterEngineLibraryServiceServer(e.grpc, &eaasLibrary{e: e})
	networktrust.RegisterNetworkTrustServiceServer(e.grpc, eaasTrust{})
	mux := http.NewServeMux()
	mux.HandleFunc("/download/", e.download)
	e.http = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go e.grpc.Serve(gl)
	go e.http.Serve(hl)
	go e.beacon(answer, "grpc://"+gl.Addr().String(), "http://"+hl.Addr().String())
	log.Printf("stream-sync: rekordbox remote library %q (%s) offered on %s", name, ip, gl.Addr())
	return e, nil
}

func (e *rbxEaas) stop() {
	e.udp.Close()
	e.grpc.Stop()
	e.http.Close()
	log.Printf("stream-sync: rekordbox remote library %s withdrawn", e.ip)
}

// beacon answers this deck's Engine: one datagram per service address, from 127.0.0.1.
func (e *rbxEaas) beacon(answer *net.UDPConn, grpcURL, httpURL string) {
	defer answer.Close()
	answers := [][]byte{
		eaasAnswer(e.token, "rekordbox ("+e.name+")", grpcURL, "4.0.0"),
		eaasAnswer(e.token, "rekordbox ("+e.name+")", httpURL, "4.0.0"),
	}
	b := make([]byte, 64)
	for {
		n, from, err := e.udp.ReadFromUDP(b)
		if err != nil {
			return
		}
		if !bytes.Equal(b[:n], eaasRequest) || !eaasFromHere(from.IP) {
			continue
		}
		for _, a := range answers {
			_, _ = answer.WriteToUDP(a, from)
		}
	}
}

// fromThisDeck reports whether ip is one of this deck's own addresses.
func fromThisDeck(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// eaasAnswer is a discovery answer as Engine reads it (QDataStream version 18, big-endian):
// "EAAS" 01 01, source id, name, service URL, version, type 1 (library), "_".
func eaasAnswer(id uuid.UUID, name, url, version string) []byte {
	var b bytes.Buffer
	b.WriteString("EAAS\x01\x01")
	b.Write(id[:])
	qString(&b, name)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(url))) // QUrl: its encoded bytes
	b.WriteString(url)
	qString(&b, version)
	b.WriteByte(1)
	qString(&b, "_")
	return b.Bytes()
}

func qString(b *bytes.Buffer, s string) {
	u := utf16.Encode([]rune(s))
	_ = binary.Write(b, binary.BigEndian, uint32(2*len(u)))
	_ = binary.Write(b, binary.BigEndian, u)
}

type eaasTrust struct {
	networktrust.UnimplementedNetworkTrustServiceServer
}

// CreateTrust: only this deck's Engine reaches the library, so there is nothing to ask.
func (eaasTrust) CreateTrust(_ context.Context, r *networktrust.CreateTrustRequest) (*networktrust.CreateTrustResponse, error) {
	log.Printf("stream-sync: rekordbox remote library: trust for %q", r.GetDeviceName())
	return &networktrust.CreateTrustResponse{Response: &networktrust.CreateTrustResponse_Granted{Granted: &networktrust.CreateTrustGranted{}}}, nil
}

type eaasLibrary struct {
	enginelibrary.UnimplementedEngineLibraryServiceServer
	e *rbxEaas
}

// link runs f in a dbserver session with the computer (one at a time, as rekordbox serves).
func (l *eaasLibrary) link(ctx context.Context, f func(rbxLink) error) error {
	rbxSession.Lock()
	defer rbxSession.Unlock()
	link, err := dialRekordbox(ctx, net.ParseIP(l.e.ip), deckPlayer(l.e.s))
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer link.Close()
	return f(link)
}

func (l *eaasLibrary) GetLibraries(context.Context, *enginelibrary.GetLibrariesRequest) (*enginelibrary.GetLibrariesResponse, error) {
	id, title, logo := l.e.ip, "rekordbox ("+l.e.name+")", enginelibrary.LibraryLogo_LIBRARY_LOGO_REKORDBOX
	return &enginelibrary.GetLibrariesResponse{Libraries: []*enginelibrary.Library{{Id: &id, Title: &title, Logo: &logo}}}, nil
}

// GetLibrary is the playlist tree: every track first, then rekordbox's folders and playlists.
func (l *eaasLibrary) GetLibrary(ctx context.Context, _ *enginelibrary.GetLibraryRequest) (*enginelibrary.GetLibraryResponse, error) {
	list := func(id, title string, sub []*enginelibrary.PlaylistMetadata) *enginelibrary.PlaylistMetadata {
		kind := enginelibrary.ListType_LIST_TYPE_PLAY
		return &enginelibrary.PlaylistMetadata{Id: &id, Title: &title, ListType: &kind, Playlists: sub}
	}
	out := []*enginelibrary.PlaylistMetadata{list("all", l.e.s.rbxText("tracks"), nil)}
	err := l.link(ctx, func(link rbxLink) error {
		var walk func(folder uint32, depth int) ([]*enginelibrary.PlaylistMetadata, error)
		walk = func(folder uint32, depth int) ([]*enginelibrary.PlaylistMetadata, error) {
			items, err := link.Playlists(folder)
			var lists []*enginelibrary.PlaylistMetadata
			for _, it := range items {
				id := strconv.FormatUint(uint64(it.ID), 10)
				switch {
				case it.Kind == prolink.RekordboxPlaylist:
					lists = append(lists, list(id, it.Label, nil))
				case it.Kind == prolink.RekordboxFolder && depth < 4:
					sub, _ := walk(it.ID, depth+1)
					lists = append(lists, list("folder:"+id, it.Label, sub))
				}
			}
			return lists, err
		}
		lists, err := walk(0, 0)
		out = append(out, lists...)
		return err
	})
	if err != nil && len(out) == 1 {
		return nil, err
	}
	return &enginelibrary.GetLibraryResponse{Playlists: out}, nil
}

func (l *eaasLibrary) rows(ctx context.Context, list string) (rows []prolink.RekordboxTrack, err error) {
	err = l.link(ctx, func(link rbxLink) error {
		if list == "" || list == "all" {
			rows, err = link.Tracks()
			return err
		}
		id, perr := strconv.ParseUint(list, 10, 32)
		if perr != nil {
			return status.Error(codes.NotFound, "no such playlist")
		}
		rows, err = link.PlaylistTracks(uint32(id))
		return err
	})
	if err != nil && len(rows) > 0 {
		err = nil // what was read
	}
	return rows, err
}

func eaasListTrack(t prolink.RekordboxTrack) *enginelibrary.ListTrack {
	id, title, artist, key := strconv.FormatUint(uint64(t.ID), 10), t.Title, t.Artist, t.Key
	m := &enginelibrary.TrackMetadata{Id: &id, Title: &title, Artist: &artist, Key: &key}
	if t.BPM > 0 {
		m.Bpm = &t.BPM
	}
	return &enginelibrary.ListTrack{Metadata: m}
}

func eaasPages[T interface{ Send(*R) error }, R any](rows []prolink.RekordboxTrack, size uint32, s T, wrap func([]*enginelibrary.ListTrack) *R) error {
	if size == 0 || size > 500 {
		size = 100
	}
	for i := 0; i < len(rows) || i == 0; i += int(size) {
		page := make([]*enginelibrary.ListTrack, 0, size)
		for _, t := range rows[i:min(len(rows), i+int(size))] {
			page = append(page, eaasListTrack(t))
		}
		if err := s.Send(wrap(page)); err != nil {
			return err
		}
	}
	return nil
}

func (l *eaasLibrary) GetTracks(r *enginelibrary.GetTracksRequest, s grpc.ServerStreamingServer[enginelibrary.GetTracksResponse]) error {
	rows, err := l.rows(s.Context(), r.GetPlaylistId())
	if err != nil {
		return err
	}
	return eaasPages(rows, r.GetPageSize(), s, func(p []*enginelibrary.ListTrack) *enginelibrary.GetTracksResponse {
		return &enginelibrary.GetTracksResponse{Tracks: p}
	})
}

func (l *eaasLibrary) SearchTracks(r *enginelibrary.SearchTracksRequest, s grpc.ServerStreamingServer[enginelibrary.SearchTracksResponse]) error {
	rows, err := l.rows(s.Context(), "all")
	if err != nil {
		return err
	}
	q := strings.ToLower(strings.TrimSpace(r.GetQuery()))
	var hits []prolink.RekordboxTrack
	for _, t := range rows {
		if q == "" || strings.Contains(strings.ToLower(t.Title+"\x00"+t.Artist+"\x00"+t.Key), q) {
			hits = append(hits, t)
		}
	}
	return eaasPages(hits, r.GetPageSize(), s, func(p []*enginelibrary.ListTrack) *enginelibrary.SearchTracksResponse {
		return &enginelibrary.SearchTracksResponse{Tracks: p}
	})
}

func (l *eaasLibrary) GetSearchFilters(context.Context, *enginelibrary.GetSearchFiltersRequest) (*enginelibrary.GetSearchFiltersResponse, error) {
	return &enginelibrary.GetSearchFiltersResponse{SearchFilters: &enginelibrary.SearchFilterOptions{}}, nil
}

// GetTrack: where Engine downloads the file, its facts, and rekordbox's analysis as
// Engine's own PerformanceData (the same blobs rb-bridge writes into a drive's m.db).
func (l *eaasLibrary) GetTrack(ctx context.Context, r *enginelibrary.GetTrackRequest) (*enginelibrary.GetTrackResponse, error) {
	id64, err := strconv.ParseUint(r.GetTrackId(), 10, 32)
	if err != nil {
		return nil, status.Error(codes.NotFound, "no such track")
	}
	id := uint32(id64)
	var rbPath string
	var meta []prolink.RekordboxItem
	var a *rekordbox.Analysis
	err = l.link(ctx, func(link rbxLink) error {
		var err error
		if rbPath, err = link.Path(id); err != nil {
			return err
		}
		if m, ok := link.(interface {
			Metadata(uint32) ([]prolink.RekordboxItem, error)
		}); ok {
			meta, _ = m.Metadata(id)
		}
		a = eaasAnalysis(link, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	ext := strings.ToLower(path.Ext(rbxPath(rbPath)))
	if _, ok := localCodecs[ext]; !ok {
		return nil, status.Error(codes.FailedPrecondition, l.e.s.rbxText("format"))
	}
	g := l.e.s.tidal
	file, _, nfs, err := g.rbxFind(ctx, l.e.ip, rbPath)
	if err == nil && nfs != nil {
		file, _, err = g.rbxStage(ctx, nfs, l.e.ip, rbPath)
	}
	if err != nil {
		log.Printf("stream-sync: rekordbox remote library track=%d step=find result=unreachable error=%v", id, err)
		return nil, status.Error(codes.Unavailable, l.e.s.rbxText("noshare"))
	}
	l.e.mu.Lock()
	l.e.files[id] = file
	l.e.mu.Unlock()
	size := uint32(0)
	if cp := rbxRunning(file); cp != nil {
		size = uint32(cp.size)
	} else if st, err := os.Stat(file); err == nil {
		size = uint32(st.Size())
	}
	t := eaasTrackFacts(meta, id, rbPath)
	eaasHeader(file, int64(size), &t)
	p, err := enginedb.EncodePerformance(t, a)
	if err != nil {
		log.Printf("stream-sync: rekordbox remote library track=%d analysis: %v", id, err)
		p = enginedb.Performance{Cues: enginedb.CueSlots{Main: -1}}
	}
	url := fmt.Sprintf("<%d%s>", id, ext)
	idStr, title, artist, album, key := strconv.FormatUint(id64, 10), t.Title, t.Artist, t.Album, t.Key
	length := uint32(t.Duration)
	md := &enginelibrary.TrackMetadata{Id: &idStr, Title: &title, Artist: &artist, Album: &album, Key: &key, LengthSeconds: &length}
	if t.BPM > 0 {
		md.Bpm = &t.BPM
	}
	log.Printf("stream-sync: rekordbox remote library track=%d %q grid=%d cues=%d overview=%d", id, t.Title, len(p.Beats), len(a.Cues), len(p.Overview))
	return &enginelibrary.GetTrackResponse{
		Blob:            &enginelibrary.TrackBlob{Type: &enginelibrary.TrackBlob_Url{Url: &enginelibrary.TrackBlobUrl{Url: &url, FileSize: &size}}},
		PerformanceData: eaasPerformance(idStr, t.BPM, p),
		Metadata:        md,
	}, nil
}

// eaasPerformance is p as Engine's TrackPerformanceData: the grid and overview as the
// blobs Engine stores, cues and loops as values in samples (Engine encodes those itself).
func eaasPerformance(id string, bpm float64, p enginedb.Performance) *enginelibrary.TrackPerformanceData {
	color := func(c [4]byte) *enginelibrary.Color {
		r, g, b, a := uint32(c[1]), uint32(c[2]), uint32(c[3]), uint32(c[0])
		return &enginelibrary.Color{R: &r, G: &g, B: &b, A: &a}
	}
	src := enginelibrary.InitialImportSource_INITIAL_IMPORT_SOURCE_REKORDBOX
	main, set := p.Cues.Main, p.Cues.Main >= 0
	out := &enginelibrary.TrackPerformanceData{
		Id: &id, BeatGrid: p.Beats, OverviewWaveform: p.Overview, InitialImportSource: &src,
		MainCue: &enginelibrary.MainCue{Position: &main, IsSetManually: &set, InitialPosition: &main},
	}
	if bpm > 0 {
		out.Bpm = &bpm
	}
	for i, c := range p.Cues.Hot {
		if c != nil {
			out.QuickCues = append(out.QuickCues, &enginelibrary.TrackPerformanceData_QuickCuesEntry{Key: eaasU32(uint32(i)), Value: &enginelibrary.QuickCue{Name: &c.Name, Position: &c.Position, Color: color(c.Color)}})
		}
	}
	for i, l := range p.Cues.Loops {
		if l != nil {
			out.Loops = append(out.Loops, &enginelibrary.TrackPerformanceData_LoopsEntry{Key: eaasU32(uint32(i)), Value: &enginelibrary.Loop{Name: &l.Name, LoopIn: l.In, LoopOut: l.Out, Color: color(l.Color), ActiveOnLoad: &l.OnLoad}})
		}
	}
	return out
}

// eaasTrackFacts reads a track's metadata rows (kind 4 title, 7 artist, 2 album, 0xb
// length in seconds, 0xd tempo times 100, 0xf key).
func eaasTrackFacts(rows []prolink.RekordboxItem, id uint32, rbPath string) rekordbox.Track {
	t := rekordbox.Track{ID: id, Path: rbPath, Title: path.Base(rbxPath(rbPath))}
	for _, it := range rows {
		switch it.Kind & 0xff {
		case 4:
			t.Title = it.Label
		case 7:
			t.Artist = it.Label
		case 2:
			t.Album = it.Label
		case 0xb:
			t.Duration = float64(it.ID)
		case 0xd:
			t.BPM = float64(it.ID) / 100
		case 0xf:
			t.Key = it.Label
		}
	}
	return t
}

const eaasHeadBytes = 512 << 10

// eaasHeader takes the sample rate and length from the file's header: the first bytes
// (waited for while a copy runs), in a file of the real size so the header reads as whole.
func eaasHeader(file string, size int64, t *rekordbox.Track) {
	var r io.ReaderAt
	if cp := rbxRunning(file); cp != nil {
		f, err := os.Open(cp.tmp)
		if err != nil {
			f, err = os.Open(file)
		}
		if err != nil {
			return
		}
		defer f.Close()
		want := min(size, eaasHeadBytes)
		deadline := time.Now().Add(rbxProbeTime * 5)
		cp.mu.Lock()
		for cp.got < want && !cp.done && time.Now().Before(deadline) {
			cp.mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			cp.mu.Lock()
		}
		cp.mu.Unlock()
		r = f
	} else {
		f, err := os.Open(file)
		if err != nil {
			return
		}
		defer f.Close()
		r = f
	}
	head := make([]byte, min(size, eaasHeadBytes))
	n, _ := r.ReadAt(head, 0)
	tmp, err := os.CreateTemp("", "rbx-head-*"+filepath.Ext(file))
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(head[:n])
	if err == nil {
		err = tmp.Truncate(size)
	}
	tmp.Close()
	if err != nil {
		return
	}
	info, err := compat.AudioHeader(tmp.Name())
	if err != nil || info.SampleRate <= 0 {
		return
	}
	t.SampleRate, t.StartOffsetSamples = info.SampleRate, info.StartOffsetSamples
	if info.Duration > 0 {
		t.Duration = info.Duration
	}
	if info.Samples > 0 {
		t.SampleCount = info.Samples
	}
}

// eaasAnalysis is the track's rekordbox analysis over dbserver: each ANLZ tag rekordbox
// hands out alone, read as a one-section analysis file and merged.
func eaasAnalysis(link rbxLink, id uint32) *rekordbox.Analysis {
	a := &rekordbox.Analysis{}
	l, ok := link.(interface {
		Analysis(uint32, string, string) ([]byte, error)
	})
	if !ok {
		return a
	}
	for _, tf := range [][2]string{{"PQTZ", "DAT"}, {"PCO2", "EXT"}, {"PCOB", "DAT"}, {"PWV7", "2EX"}, {"PWV5", "EXT"}, {"PWAV", "DAT"}} {
		b, err := l.Analysis(id, tf[0], tf[1])
		if err != nil || len(b) < 12 {
			continue
		}
		file := make([]byte, 28, 28+len(b))
		copy(file, "PMAI")
		binary.BigEndian.PutUint32(file[4:], 28)
		binary.BigEndian.PutUint32(file[8:], uint32(28+len(b)))
		if s, err := rekordbox.ParseAnalysis(append(file, b...)); err == nil {
			a.Merge(s)
		}
	}
	return a
}

// download serves /download/<id.ext>: the file GetTrack found, with Range.
func (e *rbxEaas) download(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/download/"), "<>")
	id, err := strconv.ParseUint(strings.TrimSuffix(name, path.Ext(name)), 10, 32)
	e.mu.Lock()
	file := e.files[uint32(id)]
	e.mu.Unlock()
	if err != nil || file == "" {
		http.NotFound(w, r)
		return
	}
	if cp := rbxRunning(file); cp != nil {
		cp.serve(w, r)
		return
	}
	http.ServeFile(w, r, file)
}

func eaasU32(v uint32) *uint32 { return &v }
