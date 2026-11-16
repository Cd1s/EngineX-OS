package webui

// rekordbox on a computer as a deck source. With Pro DJ Link on and rekordbox's LINK
// seen on the network, the deck's browser lists "rekordbox (<computer name>)": its
// tracks and playlists, read over dbserver (prolink.RekordboxLink). A row's audio file
// is found next to the deck, then plays like a drive track (/v1/stream, with Range):
//   - a network share of the computer's music folder (mounts), with the setting
//     rekordbox_share naming which folder of the computer that share shows;
//   - else the computer's own NFS export, copied under the Pro DJ Link staging budget;
//   - else the deck says the computer does not share its files.

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
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

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
	"golang.org/x/text/unicode/norm"
)

const (
	rekordboxScope = "rekordbox" // the deck's source id, its playlists' and its tracks' provider
	rbxCacheDir    = ".rekordbox-cache"
	rbxMaxFile     = 1 << 30
	rbxProbeTime   = 3 * time.Second
	rbxCopyTime    = 15 * time.Minute
	rbxPortmap     = 50111
)

// uiScopes are the sources the deck's browser can show: the services, and rekordbox
// while a computer with rekordbox is on the link.
var uiScopes = append(slices.Clone(uiServices), rekordboxScope)

type rbxLink interface {
	Tracks() ([]prolink.RekordboxItem, error)
	Playlists(uint32) ([]prolink.RekordboxItem, error)
	PlaylistTracks(uint32) ([]prolink.RekordboxItem, error)
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
	"format":     {"This file type cannot be loaded from rekordbox (MP3, FLAC and AAC only)", "无法从 rekordbox 加载这种文件（仅支持 MP3、FLAC、AAC）", "無法從 rekordbox 載入這種檔案（僅支援 MP3、FLAC、AAC）", "Este tipo de archivo no se puede cargar desde rekordbox (solo MP3, FLAC y AAC)"},
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
		if prolink.IsRekordbox(d) && time.Since(d.LastSeen) < 15*time.Second && net.ParseIP(d.Address) != nil {
			return &rbxHost{d.Address, rbxName(d.Address)}
		}
	}
	return nil
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if names, err := lookupAddr(ctx, ip); err == nil && len(names) > 0 {
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
	host := g.s.rekordboxHost()
	if host == nil {
		notice("none")
		return
	}
	rbxSession.Lock()
	defer rbxSession.Unlock()
	link, err := dialRekordbox(ctx, net.ParseIP(host.IP), deckPlayer(g.s))
	if err != nil {
		notice("unreadable")
		return
	}
	defer link.Close()
	list := func(id, title string) {
		ref := playlistRef{provider: rekordboxScope, list: providers.Playlist{ID: rbxListID(host.IP, id), Title: title}}
		add(g.refJSON(g.addPlaylist(ref), ref))
	}
	list("all", g.s.rbxText("tracks"))
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
	walk(0, "", 0)
}

// rbxRows reads a list of the computer's tracks, and where the first one's file is.
func (g *tidalGateway) rbxRows(ctx context.Context, ip, list string) ([]prolink.RekordboxItem, string, error) {
	rbxSession.Lock()
	defer rbxSession.Unlock()
	link, err := dialRekordbox(ctx, net.ParseIP(ip), deckPlayer(g.s))
	if err != nil {
		return nil, "", err
	}
	defer link.Close()
	var rows []prolink.RekordboxItem
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
	_, rest, _ := strings.Cut(ref.list.ID, "\x00")
	ip, list, _ := strings.Cut(rest, "\x00")
	rows, first, err := g.rbxRows(r.Context(), ip, list)
	if err != nil && len(rows) == 0 {
		fail(w, 502, errors.New(g.s.rbxText("unreadable")))
		return
	}
	refs := make([]trackRef, len(rows))
	for i, it := range rows {
		refs[i] = trackRef{track: providers.Track{ID: "rbx\x00" + ip + "\x00" + strconv.FormatUint(uint64(it.ID), 10), Title: it.Label, Provider: rekordboxScope}}
	}
	items := g.refItems(refs)
	if first != "" {
		if _, _, _, err := g.rbxFind(r.Context(), ip, first); err != nil {
			notice := g.trackItems("", false, []providers.Track{{Provider: listNoticeProvider, ID: "rekordbox-files", Title: g.s.rbxText("noshare")}})
			items = append(notice, items...)
		}
	}
	respond(w, page(r, 500, items))
}

// rbxPath is a path of the computer as one slash-separated absolute path, whichever its system.
func rbxPath(p string) string {
	return path.Clean("/" + strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"))
}

// validRekordboxShare accepts the setting: a folder of the computer, or empty.
func validRekordboxShare(p string) bool {
	return len(p) <= 512 && !strings.ContainsFunc(p, unicode.IsControl)
}

// rbxShareFile finds the track's file inside a mounted network share, if the share shows
// the folder named by the setting (empty: the computer's whole disk).
func (s *Server) rbxShareFile(rbPath string) (file, root string) {
	s.uiMu.Lock()
	prefix := rbxPath(s.loadUI().RekordboxShare)
	s.uiMu.Unlock()
	p := rbxPath(rbPath)
	if prefix != "/" {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || !strings.HasPrefix(rest, "/") {
			return "", ""
		}
		p = rest
	}
	rel, ok := prolink.SafeRel(p)
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

// rbxFind says how the track's file can be had: in a share (file and root), or over the
// computer's NFS export (nfs); an error when neither reaches it.
func (g *tidalGateway) rbxFind(ctx context.Context, ip, rbPath string) (file, root string, nfs *prolink.NFSClient, err error) {
	if file, root = g.s.rbxShareFile(rbPath); file != "" {
		return file, root, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, rbxProbeTime)
	defer cancel()
	c := rbxNFS(net.ParseIP(ip))
	if _, _, err = c.Lookup(ctx, rbxExport, rbxPath(rbPath)); err != nil {
		return "", "", nil, err
	}
	return "", "", c, nil
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
	rbxSession.Lock()
	link, err := dialRekordbox(ctx, net.ParseIP(parts[1]), deckPlayer(g.s))
	var rbPath string
	if err == nil {
		rbPath, err = link.Path(uint32(tid))
		link.Close()
	}
	rbxSession.Unlock()
	if err != nil {
		return ref, err
	}
	if _, ok := localCodecs[strings.ToLower(path.Ext(rbxPath(rbPath)))]; !ok {
		return ref, errors.New(g.s.rbxText("format"))
	}
	file, root, nfs, err := g.rbxFind(ctx, parts[1], rbPath)
	if err != nil {
		log.Printf("stream-sync: load track=%d provider=rekordbox step=find result=unreachable nfs=%v", id, err)
		return ref, errors.New(g.s.rbxText("noshare"))
	}
	if nfs != nil {
		if file, root, err = g.rbxStage(ctx, nfs, parts[1], rbPath); err != nil {
			log.Printf("stream-sync: load track=%d provider=rekordbox step=copy result=failed error=%v", id, err)
			return ref, err
		}
	}
	log.Printf("stream-sync: load track=%d provider=rekordbox step=find result=ready via=%s", id, map[bool]string{true: "nfs", false: "share"}[nfs != nil])
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
	p := rbxPath(rbPath)
	_, info, err := c.Lookup(ctx, rbxExport, p)
	if err != nil {
		return "", "", err
	}
	if info.Size > rbxMaxFile {
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
		if skip[dir] || syscall.Statfs(dir, &fs) != nil || fs.Flags&1 != 0 { // ST_RDONLY
			continue
		}
		for rbxRoom(&fs, prolink.DirSize(cache)) < int64(info.Size) && dropOldest(cache) {
			_ = syscall.Statfs(dir, &fs)
		}
		if rbxRoom(&fs, prolink.DirSize(cache)) < int64(info.Size) {
			continue
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			continue
		}
		name := fmt.Sprintf("%x%s", sha1.Sum(fmt.Appendf(nil, "%s %s %d %d", ip, p, info.Size, info.MTime)), strings.ToLower(path.Ext(p)))
		file = filepath.Join(cache, name)
		// A copy that finished is a file; one that runs is joined. Both are decided under the lock
		// a finishing copy takes to leave the table, after it has moved its file into place.
		rbxCopies.Lock()
		if rbxCopies.m[file] == nil {
			if st, err := os.Stat(file); err == nil && st.Size() == int64(info.Size) {
				rbxCopies.Unlock()
				return file, dir, nil
			}
			tmp, err := os.CreateTemp(cache, ".fetch-*")
			if err != nil {
				rbxCopies.Unlock()
				continue
			}
			cp := &rbxCopy{file: file, tmp: tmp.Name(), size: int64(info.Size)}
			cp.more = sync.NewCond(&cp.mu)
			rbxCopies.m[file] = cp
			go cp.run(c, p, tmp)
		}
		rbxCopies.Unlock()
		return file, dir, nil
	}
	return "", "", errors.New(g.s.rbxText("noroom"))
}

// rbxCopy is a track being copied from the computer's NFS export into the cache. It goes on
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

func (cp *rbxCopy) run(c *prolink.NFSClient, p string, tmp *os.File) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), rbxCopyTime)
	defer cancel()
	_, err := c.FetchTo(ctx, rbxExport, p, rbxWriter{cp, tmp}, rbxMaxFile)
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
