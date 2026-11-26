package webui

// rekordbox on a computer as a deck source. With Pro DJ Link on and rekordbox's LINK
// seen on the network, the deck's browser lists "rekordbox (<computer name>)": its
// tracks and playlists, read over dbserver (prolink.RekordboxLink). A row's audio file
// is found next to the deck, then plays like a drive track (/v1/stream, with Range):
//   - a network share of the computer's music folder (mounts), with the setting
//     rekordbox_share naming which folder of the computer that share shows;
//   - else that configured share over SMB2/3 in Go, copied under the same staging budget;
//   - else the computer's own NFS export, copied under the Pro DJ Link staging budget;
//   - else the deck says the computer does not share its files.

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/Cd1s/EngineX-OS/internal/smbclient"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"golang.org/x/text/unicode/norm"
)

// rbxAnalysisWait is how long a load's answer waits for its analysis. A variable for tests.
var rbxAnalysisWait = 3 * time.Second

const (
	rekordboxScope   = "rekordbox" // the deck's source id, its playlists' and its tracks' provider
	rbxCacheDir      = ".rekordbox-cache"
	rbxMaxFile       = 1 << 30
	rbxProbeTime     = 3 * time.Second
	rbxPresenceEvery = 2 * time.Second // shorter than the UI poll, so timer jitter never skips a check
	rbxCopyTime      = 15 * time.Minute
	rbxPortmap       = 50111
	rbxArtTime       = 3 * time.Second
)

// uiScopes are the sources the deck's browser can show: the services, and rekordbox
// while a computer with rekordbox is on the link.
var uiScopes = append(slices.Clone(uiServices), rekordboxScope)

type rbxLink interface {
	Tracks() ([]prolink.RekordboxTrack, error)
	Playlists(uint32) ([]prolink.RekordboxItem, error)
	PlaylistTracks(uint32) ([]prolink.RekordboxTrack, error)
	Path(uint32) (string, error)
	Close()
}

// Hooks for the tests.
var (
	dialRekordbox = func(ctx context.Context, ip net.IP, player byte) (rbxLink, error) {
		return prolink.DialRekordbox(ctx, ip, player)
	}
	// deckPlayer is the deck's own Pro DJ Link player number, so two decks ask the computer as
	// two players; 0 while it has none (not announcing), and the client then asks as 5.
	deckPlayer = func(s *Server) byte { return s.prolinkExport.Status().Number }
	lookupAddr = net.DefaultResolver.LookupAddr
	rbxExport  = "/"
	rbxRoom    = prolink.StageRoom
	rbxNFS     = func(ip net.IP) *prolink.NFSClient {
		c := &prolink.NFSClient{IP: ip, Portmap: rbxPortmap, Timeout: 400 * time.Millisecond}
		if os.Geteuid() == 0 {
			c.SourcePort = 1023 // some NFS servers only answer privileged ports
		}
		return c
	}
	rbxSession sync.Mutex // the computer serves one session per asking player: ours run one at a time
	rbxNames   = struct {
		sync.Mutex
		m map[string]string
	}{m: map[string]string{}}
)

// rbxTexts are the deck's words for this source, by uiLanguages order: en, zh-Hans, zh-Hant, es.
var rbxTexts = map[string][4]string{
	"tracks":     {"Tracks", "曲目", "曲目", "Pistas"},
	"none":       {"No computer with rekordbox found on Pro DJ Link", "Pro DJ Link 上没有找到运行 rekordbox 的电脑", "Pro DJ Link 上沒有找到執行 rekordbox 的電腦", "No se encontró ningún ordenador con rekordbox en Pro DJ Link"},
	"unreadable": {"Could not read the rekordbox library", "无法读取 rekordbox 资料库", "無法讀取 rekordbox 資料庫", "No se pudo leer la biblioteca de rekordbox"},
	"noshare":    {"This computer does not share files with EngineX; set up a network share of your music folder", "这台电脑没有向 EngineX 共享文件；请把音乐文件夹设为网络共享", "這台電腦沒有向 EngineX 分享檔案；請把音樂資料夾設為網路共用", "Este ordenador no comparte archivos con EngineX; configura una carpeta de red compartida con tu música"},
	"format":     {"This file type cannot be loaded from rekordbox (MP3, FLAC, AAC, WAV and AIFF only)", "无法从 rekordbox 加载这种文件（仅支持 MP3、FLAC、AAC、WAV、AIFF）", "無法從 rekordbox 載入這種檔案（僅支援 MP3、FLAC、AAC、WAV、AIFF）", "Este tipo de archivo no se puede cargar desde rekordbox (solo MP3, FLAC, AAC, WAV y AIFF)"},
	"noroom":     {"No room on the drive to copy this track", "存储空间不足，无法复制这首曲目", "儲存空間不足，無法複製這首曲目", "No hay espacio en la unidad para copiar esta pista"},
}

func (s *Server) rbxText(key string) string {
	s.uiMu.Lock()
	lang := s.loadUI().Language
	s.uiMu.Unlock()
	return rbxTexts[key][max(0, slices.Index(uiLanguages, lang))]
}

type rbxHost struct{ IP, Name string }

// rekordboxHost is the computer with rekordbox on the link, once Pro DJ Link is on.
func (s *Server) rekordboxHost() *rbxHost {
	s.uiMu.Lock()
	on := s.loadUI().ProlinkShare
	s.uiMu.Unlock()
	if !on {
		return nil
	}
	for _, d := range s.mergedDevices() {
		if prolink.IsRekordbox(d) && time.Since(d.LastSeen) <= prolink.DeviceTimeout && net.ParseIP(d.Address) != nil {
			if !s.rekordboxReady(d.Address) {
				continue
			}
			return &rbxHost{d.Address, rbxName(d.Address)}
		}
	}
	return nil
}

func (s *Server) rekordboxAvailable(ip string) bool {
	host := s.rekordboxHost()
	return host != nil && host.IP == ip
}

// A computer keeps announcing even with LINK off. Only a successful library-port
// query confirms the source; this memory is never restored from a snapshot at boot.
type rbxPresence struct {
	checked, seen time.Time
	checking      bool
}

// rekordboxReady never waits on the network in a UI/browse request. Polls refresh
// one bounded query, without opening a dbserver session or disturbing a load.
func (s *Server) rekordboxReady(ip string) bool {
	s.rbxPresenceMu.Lock()
	defer s.rbxPresenceMu.Unlock()
	now := time.Now()
	// Drop abandoned IPs without discarding a recent reply or an in-flight query.
	for addr, p := range s.rbxPresence {
		if !p.checking && now.Sub(p.checked) > prolink.DeviceTimeout && now.Sub(p.seen) > prolink.DeviceTimeout {
			delete(s.rbxPresence, addr)
		}
	}
	if s.rbxPresence == nil {
		s.rbxPresence = make(map[string]*rbxPresence)
	}
	p := s.rbxPresence[ip]
	if p == nil {
		p = &rbxPresence{}
		s.rbxPresence[ip] = p
	}
	if !p.checking && now.Sub(p.checked) >= rbxPresenceEvery {
		p.checked, p.checking = now, true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), rbxProbeTime)
			defer cancel()
			_, err := prolink.RekordboxPort(ctx, net.ParseIP(ip))
			s.rbxPresenceMu.Lock()
			defer s.rbxPresenceMu.Unlock()
			if err == nil {
				p.seen = time.Now()
			}
			p.checking = false
		}()
	}
	return !p.seen.IsZero() && now.Sub(p.seen) <= prolink.DeviceTimeout
}

// rbxName is the computer's name, from the network's reverse lookup of its address. The
// address stands in until that answers: the status poll never waits for it.
func rbxName(ip string) string {
	rbxNames.Lock()
	defer rbxNames.Unlock()
	if name, ok := rbxNames.m[ip]; ok {
		return name
	}
	rbxNames.m[ip] = ip
	lookup := lookupAddr
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if names, err := lookup(ctx, ip); err == nil && len(names) > 0 {
			rbxNames.Lock()
			rbxNames.m[ip], _, _ = strings.Cut(strings.TrimSuffix(names[0], "."), ".")
			rbxNames.Unlock()
		}
	}()
	return ip
}

func rbxListID(ip, list string) string { return "rbx\x00" + ip + "\x00" + list }

// rbxPlaylistsPage answers a playlists request while the deck shows rekordbox.
func (g *tidalGateway) rbxPlaylistsPage(w http.ResponseWriter, r *http.Request) {
	favorites := strings.HasSuffix(r.URL.Path, "/favorites/playlists")
	items := []any{}
	g.rbxPlaylists(r.Context(), func(v map[string]any) {
		if favorites { // as for the services: the playlist wrapped in "item", its fields repeated flat
			flat := map[string]any{"item": v}
			for k, f := range v {
				flat[k] = f
			}
			v = flat
		}
		items = append(items, v)
	})
	respond(w, page(r, 100, items))
}

// rbxPlaylists lists the computer's tracks and playlists. Folders are flattened:
// "Folder / Playlist".
func (g *tidalGateway) rbxPlaylists(ctx context.Context, add func(map[string]any)) {
	notice := func(key string) {
		ref := playlistRef{provider: listNoticeProvider, list: providers.Playlist{ID: rekordboxScope, Title: g.s.rbxText(key)}}
		add(g.refJSON(g.addPlaylist(ref), ref))
	}
	host, players := g.s.rekordboxHost(), g.s.rbxPlayers()
	if host == nil && len(players) == 0 {
		notice("none")
		return
	}
	defer rbxClaim()()
	rbxSession.Lock()
	defer rbxSession.Unlock()
	library := func(key, prefix string, link rbxLink) {
		list := func(id, title string) {
			ref := playlistRef{provider: rekordboxScope, list: providers.Playlist{ID: rbxListID(key, id), Title: title}}
			add(g.refJSON(g.addPlaylist(ref), ref))
		}
		list("all", prefix+g.s.rbxText("tracks"))
		var walk func(folder uint32, prefix string, depth int)
		walk = func(folder uint32, prefix string, depth int) {
			items, err := link.Playlists(folder)
			for _, it := range items {
				switch {
				case it.Kind == prolink.RekordboxPlaylist:
					list(strconv.FormatUint(uint64(it.ID), 10), prefix+it.Label)
				case it.Kind == prolink.RekordboxFolder && depth < 4:
					walk(it.ID, prefix+it.Label+" / ", depth+1)
				}
			}
			if err != nil {
				notice("unreadable")
			}
		}
		walk(0, prefix, 0)
	}
	if host != nil {
		link, err := g.s.rbxDial(ctx, host.IP)
		if err != nil {
			notice("unreadable")
		} else {
			library(host.IP, "", link)
			link.Close()
		}
	}
	// The players' media: what a CDJ would browse over Pro DJ Link.
	for _, d := range players {
		for _, slot := range []byte{3, 2} {
			probe, cancel := context.WithTimeout(ctx, rbxProbeTime)
			key := rbxMediaKey(d.Address, slot)
			if link, err := g.s.rbxDial(probe, key); err == nil {
				library(key, rbxMediaName(d, slot)+" / ", link)
			}
			cancel()
		}
	}
}

// rbxRows reads a list of the computer's tracks, and where the first one's file is.
func (g *tidalGateway) rbxRows(ctx context.Context, ip, list string) ([]prolink.RekordboxTrack, string, error) {
	rbxSession.Lock()
	defer rbxSession.Unlock()
	link, err := g.s.rbxDial(ctx, ip)
	if err != nil {
		return nil, "", err
	}
	defer link.Close()
	var rows []prolink.RekordboxTrack
	if list == "all" {
		rows, err = link.Tracks()
	} else {
		id, _ := strconv.ParseUint(list, 10, 32)
		rows, err = link.PlaylistTracks(uint32(id))
	}
	if len(rows) == 0 {
		return nil, "", err
	}
	first, _ := link.Path(rows[0].ID)
	return rows, first, err
}

// rbxTracks answers a rekordbox list's tracks. When none of the computer's files can be
// reached, the first row says so: better than a load that fails.
func (g *tidalGateway) rbxTracks(w http.ResponseWriter, r *http.Request, ref playlistRef) {
	defer rbxClaim()() // the rows, the file check and the analysis prefetch go before covers
	_, rest, _ := strings.Cut(ref.list.ID, "\x00")
	ip, list, _ := strings.Cut(rest, "\x00")
	prefetch := g.rbxBrowse.begin()
	rows, first, err := g.rbxRows(r.Context(), ip, list)
	if err != nil && len(rows) == 0 {
		openErr := errors.New(g.s.rbxText("unreadable"))
		g.logPlaylistOpenFailure(r, http.StatusBadGateway, ref.list.ID, &ref, openErr)
		fail(w, http.StatusBadGateway, openErr)
		return
	}
	refs := make([]trackRef, len(rows))
	_, slot := rbxMedia(ip)
	for i, t := range rows {
		// The list shows the key as the wheel notation or not at all: its tempo column reads nothing else.
		key := ""
		if n, mode, ok := enginedb.Camelot(t.Key); ok {
			key = fmt.Sprintf("%d%c", n, mode)
		}
		refs[i] = trackRef{track: providers.Track{ID: "rbx\x00" + ip + "\x00" + strconv.FormatUint(uint64(t.ID), 10), Title: t.Title, Artist: t.Artist, BPM: t.BPM, Key: key, DurationMS: t.DurationMS, Provider: rekordboxScope}}
		if slot == 0 && t.Art != 0 { // a computer's cover; a player's medium is left as it was
			refs[i].track.CoverURL = rbxCoverScheme + ip + "/" + strconv.FormatUint(uint64(t.Art), 10)
		}
	}
	items := g.refItems(refs)
	noticeCount := 0
	if first != "" {
		if _, _, _, err := g.rbxFind(r.Context(), ip, first); err != nil {
			notice := g.trackItems("", false, []providers.Track{{Provider: listNoticeProvider, ID: "rekordbox-files", Title: g.s.rbxText("noshare")}})
			items = append(notice, items...)
			noticeCount = len(notice)
		}
	}
	result := page(r, 500, items)
	shown := result["items"].([]any)
	offset := result["offset"].(int) - noticeCount
	if offset < 0 {
		shown = shown[min(-offset, len(shown)):]
		offset = 0
	}
	front, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(prefetch, cancel)
	if g.rbxPrefetch(front, ip, rows, shown, offset) {
		rbxMarkLoading()
	}
	stop()
	cancel()
	respond(w, result)
	if _, slot := rbxMedia(ip); slot == 0 && len(rows) != 0 {
		g.rbxBrowse.enqueue(prefetch, func() {
			release := rbxClaim()
			defer release()
			g.rbxPrefetchContinue(prefetch, ip, rows, items[noticeCount:], offset)
		})
	}
}

// rbxCoverScheme marks a cover the computer's rekordbox holds: rekordbox-art://<ip>/<artwork id>.
// The gateway's art cache and /v1/enginex/art serve it like a service's cover link.
const rbxCoverScheme = "rekordbox-art://"

// rbxArtTurn lets covers use the dbserver session only when nothing else wants it: a list,
// a load or an analysis read claims it, which ends a cover being read at once (its context is
// cancelled, closing the connection) and keeps new ones out until released.
var rbxArtTurn struct {
	sync.Mutex
	claims int
	cancel context.CancelFunc
}

func rbxClaim() (release func()) {
	rbxArtTurn.Lock()
	rbxArtTurn.claims++
	if rbxArtTurn.cancel != nil {
		rbxArtTurn.cancel()
	}
	rbxArtTurn.Unlock()
	return func() {
		rbxArtTurn.Lock()
		rbxArtTurn.claims--
		rbxArtTurn.Unlock()
	}
}

// rbxArtTake takes the session for a cover when it is free and unclaimed.
func rbxArtTake(cancel context.CancelFunc) bool {
	rbxArtTurn.Lock()
	defer rbxArtTurn.Unlock()
	if rbxArtTurn.claims > 0 || !rbxSession.TryLock() {
		return false
	}
	rbxArtTurn.cancel = cancel
	return true
}

func rbxArtRelease() {
	rbxArtTurn.Lock()
	rbxArtTurn.cancel = nil
	rbxArtTurn.Unlock()
	rbxSession.Unlock()
}

// rbxArtPixels bounds a cover's decoded size; rekordbox's own are far smaller.
const rbxArtPixels = 1600 * 1600

// rbxArt reads a cover over dbserver (0x2003) from the computer on the link now, waiting at
// most rbxArtTime for a free session. Only a JPEG, or a PNG it re-encodes as JPEG (Engine's
// stm.db covers are JPEG), within maxArtBytes and rbxArtPixels is a cover.
func (g *tidalGateway) rbxArt(ctx context.Context, address string) []byte {
	ip, n, _ := strings.Cut(strings.TrimPrefix(address, rbxCoverScheme), "/")
	art, err := strconv.ParseUint(n, 10, 32)
	if host := g.s.rekordboxHost(); err != nil || art == 0 || host == nil || host.IP != ip {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, rbxArtTime)
	defer cancel()
	for !rbxArtTake(cancel) {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(20 * time.Millisecond):
		}
	}
	link, err := g.s.rbxDial(ctx, ip)
	var data []byte
	if err == nil {
		if a, ok := link.(interface{ Artwork(uint32) ([]byte, error) }); ok {
			data, err = a.Artwork(uint32(art))
		}
		link.Close()
	}
	rbxArtRelease()
	if err != nil || len(data) > maxArtBytes {
		return nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width*cfg.Height > rbxArtPixels || format != "jpeg" && format != "png" {
		return nil
	}
	img, err := png.Decode(bytes.NewReader(data))
	if format == "jpeg" {
		_, err = jpeg.Decode(bytes.NewReader(data)) // a truncated picture is no cover
		img = nil
	}
	if err != nil {
		return nil
	}
	if img != nil {
		var b bytes.Buffer
		if jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}) != nil || b.Len() > maxArtBytes {
			return nil
		}
		data = b.Bytes()
	}
	return data
}

// rbxPath is a path of the computer as one slash-separated absolute path, whichever its system.
func rbxPath(p string) string {
	return path.Clean("/" + strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
}

// validRekordboxShare accepts the setting: a folder of the computer, or empty.
func validRekordboxShare(p string) bool {
	return len(p) <= 512 && !strings.ContainsFunc(p, unicode.IsControl)
}

// rbxShareRel maps a computer path into the folder shown by its configured share.
func (s *Server) rbxShareRel(rbPath string) (string, bool) {
	s.uiMu.Lock()
	prefix := rbxPath(s.loadUI().RekordboxShare)
	s.uiMu.Unlock()
	p := rbxPath(rbPath)
	if prefix != "/" {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || !strings.HasPrefix(rest, "/") {
			return "", false
		}
		p = rest
	}
	return prolink.SafeRel(p)
}

func (s *Server) rbxShareFile(rbPath string) (file, root string) {
	rel, ok := s.rbxShareRel(rbPath)
	if !ok {
		return "", ""
	}
	for _, m := range s.loadMounts() {
		if !mountedAt(m.MountPoint) {
			continue
		}
		// macOS keeps names in either Unicode form; the share may show the other one.
		for _, name := range []string{rel, norm.NFC.String(rel), norm.NFD.String(rel)} {
			if st, err := os.Stat(filepath.Join(m.MountPoint, name)); err == nil && st.Mode().IsRegular() {
				return filepath.Join(m.MountPoint, name), m.MountPoint
			}
		}
	}
	return "", ""
}

type rbxRemote struct {
	nfs   *prolink.NFSClient
	mount RemoteMount
	path  string
	info  os.FileInfo
}

// rbxFind checks a kernel-mounted share, Go SMB, then the computer's NFS export.
func (g *tidalGateway) rbxFind(ctx context.Context, ip, rbPath string) (file, root string, remote *rbxRemote, err error) {
	if host, slot := rbxMedia(ip); slot != 0 { // a player's medium: its own NFS export
		export, err := prolink.SlotExport(slot)
		if err != nil {
			return "", "", nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, rbxProbeTime)
		defer cancel()
		c := rbxPlayerNFS(net.ParseIP(host))
		if _, _, err = c.Lookup(ctx, export, rbxPath(rbPath)); err != nil {
			return "", "", nil, err
		}
		return "", "", &rbxRemote{nfs: c}, nil
	}
	if file, root = g.s.rbxShareFile(rbPath); file != "" {
		return file, root, nil, nil
	}
	if smb := g.rbxFindSMB(ctx, rbPath); smb != nil {
		return "", "", smb, nil
	}
	ctx, cancel := context.WithTimeout(ctx, rbxProbeTime)
	defer cancel()
	c := rbxNFS(net.ParseIP(ip))
	if _, _, err = c.Lookup(ctx, rbxExport, rbxPath(rbPath)); err != nil {
		return "", "", nil, err
	}
	return "", "", &rbxRemote{nfs: c}, nil
}

func (g *tidalGateway) rbxFindSMB(ctx context.Context, rbPath string) *rbxRemote {
	if rel, ok := g.s.rbxShareRel(rbPath); ok {
		probe, cancel := context.WithTimeout(ctx, rbxProbeTime)
		defer cancel()
		for _, m := range g.s.loadMounts() {
			if !m.Enabled || (m.Protocol != "smb" && m.Protocol != "cifs") || mountedAt(m.MountPoint) {
				continue
			}
			c, e := smbclient.Dial(probe, m.smbURL(), m.Username, m.Password, nil)
			if e != nil {
				log.Printf("stream-sync: provider=rekordbox step=find via=smb result=failed error=%v", e)
				continue
			}
			for _, name := range []string{rel, norm.NFC.String(rel), norm.NFD.String(rel)} {
				p := path.Join(c.Root, name)
				if info, e := c.Share.Stat(p); e == nil && info.Mode().IsRegular() {
					c.Close()
					return &rbxRemote{mount: m, path: p, info: info}
				}
			}
			c.Close()
		}
	}
	return nil
}

// rbxPrepare gives a rekordbox row the audio file it plays from, as a drive row has one.
func (g *tidalGateway) rbxPrepare(ctx context.Context, id int64, ref trackRef) (trackRef, error) {
	if st, err := os.Stat(ref.file); err == nil && st.Mode().IsRegular() {
		return ref, nil
	}
	parts := strings.Split(ref.track.ID, "\x00")
	tid, err := strconv.ParseUint(parts[len(parts)-1], 10, 32)
	if err != nil || len(parts) != 3 {
		return ref, errors.New("rekordbox: bad track")
	}
	rbxMarkLoading()
	release := rbxClaim()
	rbxSession.Lock()
	link, err := g.s.rbxDial(ctx, parts[1])
	var rbPath string
	if err == nil {
		rbPath, err = link.Path(uint32(tid))
		link.Close()
	}
	rbxSession.Unlock()
	release()
	if err != nil {
		return ref, err
	}
	if _, ok := localCodecs[strings.ToLower(path.Ext(rbxPath(rbPath)))]; !ok {
		return ref, errors.New(g.s.rbxText("format"))
	}
	file, root, remote, err := g.rbxFind(ctx, parts[1], rbPath)
	if err != nil {
		log.Printf("stream-sync: load track=%d provider=rekordbox step=find result=unreachable error=%v", id, err)
		return ref, errors.New(g.s.rbxText("noshare"))
	}
	via := "share"
	if remote != nil {
		if remote.nfs != nil {
			via = "nfs"
			file, root, err = g.rbxStage(ctx, remote.nfs, parts[1], rbPath)
		} else {
			via = "smb"
			file, root, err = g.rbxStageCopy(remote.mount.smbURL()+" "+remote.mount.Username, rbPath, remote.info.Size(), remote.info.ModTime().Unix(), func(ctx context.Context, w io.Writer) error {
				c, err := smbclient.Dial(ctx, remote.mount.smbURL(), remote.mount.Username, remote.mount.Password, nil)
				if err != nil {
					return err
				}
				defer c.Close()
				f, err := c.Share.Open(remote.path)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = io.CopyN(w, f, remote.info.Size())
				return err
			})
		}
		if err != nil {
			log.Printf("stream-sync: load track=%d provider=rekordbox step=copy result=failed error=%v", id, err)
			return ref, err
		}
	}
	log.Printf("stream-sync: load track=%d provider=rekordbox step=find result=ready via=%s", id, via)
	if _, slot := rbxMedia(parts[1]); slot == 0 {
		rbxIndexReplay(id, rbPath, file)
	}
	// Engine reads the track's row as soon as it has this answer, and the hook fills the row
	// then and not again: an analysis written later left the deck without overview and with
	// Engine's own grid until the track was loaded once more. The list's prefetch has it
	// ready for most tracks; a load it did not reach waits for its own, within bounds.
	rbxAnalysisJobs.Add(1)
	written := make(chan struct{})
	go func() {
		defer close(written)
		g.rbxWriteAnalysis(id, parts[1], uint32(tid), rbPath, file)
	}()
	select {
	case <-written:
	case <-time.After(rbxAnalysisWait):
	case <-ctx.Done():
	}
	ref.file, ref.drive = file, root
	g.mu.Lock()
	g.tracks[id] = ref
	g.mu.Unlock()
	return ref, nil
}

// rbxStage starts the copy of a track from the computer's NFS export into a drive's cache
// folder, within the Pro DJ Link staging budget (the oldest copies make room), and returns
// the file it will be: the copy goes on while the manifest and stream requests are answered.
func (g *tidalGateway) rbxStage(ctx context.Context, c *prolink.NFSClient, ip, rbPath string) (file, root string, err error) {
	p, export := rbxPath(rbPath), rbxExport
	if _, slot := rbxMedia(ip); slot != 0 {
		export, _ = prolink.SlotExport(slot)
	}
	_, info, err := c.Lookup(ctx, export, p)
	if err != nil {
		return "", "", err
	}
	return g.rbxStageCopy(ip, p, int64(info.Size), int64(info.MTime), func(ctx context.Context, w io.Writer) error {
		_, err := c.FetchTo(ctx, export, p, w, rbxMaxFile)
		return err
	})
}

func (g *tidalGateway) rbxStageCopy(ip, p string, size, mtime int64, fetch func(context.Context, io.Writer) error) (file, root string, err error) {
	if size < 0 || size > rbxMaxFile {
		return "", "", errors.New(g.s.rbxText("noroom"))
	}
	skip := map[string]bool{}
	for _, m := range g.s.loadMounts() {
		skip[m.MountPoint] = true
	}
	entries, _ := os.ReadDir(g.s.mediaRoot())
	for _, e := range entries {
		dir := filepath.Join(g.s.mediaRoot(), e.Name())
		cache := filepath.Join(dir, rbxCacheDir)
		var fs syscall.Statfs_t
		if skip[dir] || syscall.Statfs(dir, &fs) != nil || fs.Flags&1 != 0 || !rbxOwnCache(dir, cache) { // ST_RDONLY
			continue
		}
		// Budget for on-demand rekordbox playback copies (.rekordbox-cache). Leftover
		// .prolink-cache directories are untouched and already reflected in fs.Bavail.
		room := func() int64 {
			return rbxRoom(&fs, prolink.DirSize(cache))
		}
		for room() < size && dropOldest(cache) {
			_ = syscall.Statfs(dir, &fs)
		}
		if room() < size {
			continue
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			continue
		}
		name := fmt.Sprintf("%x%s", sha1.Sum(fmt.Appendf(nil, "%s %s %d %d", ip, p, size, mtime)), strings.ToLower(path.Ext(p)))
		file = filepath.Join(cache, name)
		// A copy that finished is a file; one that runs is joined. Both are decided under the lock
		// a finishing copy takes to leave the table, after it has moved its file into place.
		rbxCopies.Lock()
		if rbxCopies.m[file] == nil {
			if st, err := os.Stat(file); err == nil && st.Size() == size {
				rbxCopies.Unlock()
				return file, dir, nil
			}
			tmp, err := os.CreateTemp(cache, ".fetch-*")
			if err != nil {
				rbxCopies.Unlock()
				continue
			}
			cp := &rbxCopy{file: file, tmp: tmp.Name(), size: size}
			cp.more = sync.NewCond(&cp.mu)
			rbxCopies.m[file] = cp
			go cp.run(fetch, tmp)
		}
		rbxCopies.Unlock()
		return file, dir, nil
	}
	return "", "", errors.New(g.s.rbxText("noroom"))
}

// rbxOwnCache reports whether cache is not there yet or is a folder of the drive's own
// volume. A link, a file or another volume in its place could lead anywhere: nothing is
// listed, removed or written through it.
func rbxOwnCache(dir, cache string) bool {
	st, err := os.Lstat(cache)
	if os.IsNotExist(err) {
		return true
	}
	root, rerr := os.Stat(dir)
	return err == nil && rerr == nil && st.IsDir() && st.Sys().(*syscall.Stat_t).Dev == root.Sys().(*syscall.Stat_t).Dev
}

// rbxCopy is a track being copied over SMB or NFS into the cache. It goes on
// when the request that started it gives up (the next try finds the file there), and a stream
// request is answered from what has arrived while the rest comes.
type rbxCopy struct {
	file, tmp string
	size      int64
	mu        sync.Mutex
	more      *sync.Cond
	got       int64
	done      bool
	err       error
}

var rbxCopies = struct {
	sync.Mutex
	m map[string]*rbxCopy
}{m: map[string]*rbxCopy{}}

// rbxRunning is the copy still running into file, or nil.
func rbxRunning(file string) *rbxCopy {
	rbxCopies.Lock()
	defer rbxCopies.Unlock()
	return rbxCopies.m[file]
}

// rbxWriter counts what reached the file, for the readers waiting on it.
type rbxWriter struct {
	cp *rbxCopy
	f  *os.File
}

func (w rbxWriter) Write(b []byte) (int, error) {
	n, err := w.f.Write(b)
	w.cp.mu.Lock()
	w.cp.got += int64(n)
	w.cp.mu.Unlock()
	w.cp.more.Broadcast()
	return n, err
}

func (cp *rbxCopy) run(fetch func(context.Context, io.Writer) error, tmp *os.File) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), rbxCopyTime)
	defer cancel()
	err := fetch(ctx, rbxWriter{cp, tmp})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(cp.tmp, cp.file)
	}
	if err != nil {
		os.Remove(cp.tmp)
	}
	cp.mu.Lock()
	cp.done, cp.err = true, err
	got := cp.got
	cp.mu.Unlock()
	cp.more.Broadcast()
	log.Printf("stream-sync: provider=rekordbox step=copy ok=%t bytes=%d of=%d elapsed=%s", err == nil, got, cp.size, time.Since(started).Round(time.Millisecond))
	rbxCopies.Lock()
	delete(rbxCopies.m, cp.file)
	rbxCopies.Unlock()
}

// serve answers a stream request, Range included, as bytes arrive.
func (cp *rbxCopy) serve(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(cp.tmp)
	if err != nil { // it finished and moved into place meanwhile
		http.ServeFile(w, r, cp.file)
		return
	}
	defer f.Close()
	defer context.AfterFunc(r.Context(), func() {
		cp.mu.Lock() // a reader is before its check or already waiting, never between
		cp.mu.Unlock()
		cp.more.Broadcast()
	})()
	http.ServeContent(w, r, "", time.Time{}, &rbxReader{cp: cp, f: f, ctx: r.Context()})
}

// rbxReader reads a copy that is still running: a read past what has arrived waits for it.
type rbxReader struct {
	cp  *rbxCopy
	f   *os.File
	ctx context.Context
	pos int64
}

func (r *rbxReader) Read(p []byte) (int, error) {
	cp := r.cp
	if r.pos >= cp.size {
		return 0, io.EOF
	}
	cp.mu.Lock()
	for r.pos >= cp.got && !cp.done && r.ctx.Err() == nil {
		cp.more.Wait()
	}
	got, err := cp.got, cp.err
	cp.mu.Unlock()
	if r.pos >= got {
		if err == nil {
			err = r.ctx.Err()
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	n, err := r.f.ReadAt(p[:min(int64(len(p)), got-r.pos)], r.pos)
	r.pos += int64(n)
	if n > 0 {
		err = nil
	}
	return n, err
}

func (r *rbxReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.cp.size
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = off
	return off, nil
}

// dropOldest removes the oldest copy in cache; false when there is none.
func dropOldest(cache string) bool {
	entries, _ := os.ReadDir(cache)
	var oldest string
	var at time.Time
	for _, e := range entries {
		// A copy still running is not the oldest of anything.
		if st, err := e.Info(); err == nil && e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".fetch-") && (oldest == "" || st.ModTime().Before(at)) {
			oldest, at = filepath.Join(cache, e.Name()), st.ModTime()
		}
	}
	return oldest != "" && os.Remove(oldest) == nil
}
