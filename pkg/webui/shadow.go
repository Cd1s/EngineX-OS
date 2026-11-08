package webui

// Shadow decks (#49). In the 4-deck layout a player plays the decks of its
// role and shows the other two. Its QML polls GET /v1/sc5000/shadow and drives
// those decks (silent: the hook routes only the role's decks to the outputs)
// from whoever plays them: the other EngineX player, whose QML posts its own
// decks to POST /v1/sc5000/decks, or a CDJ on Pro DJ Link, by player number.
// Track audio is copied to this deck.
//
// Latency: the sender's QML posts on every edge (play, CUE, jump, pitch) within
// 20 ms, Go pushes the post to the other player over UDP at once, and there it
// wakes the QML's long poll (GET /v1/sc5000/shadow?ver=N). GET /peer/decks,
// polled every 250 ms, is the fallback for lost datagrams.

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

const shadowBeaconPort = 50199 // EngineX players find each other here

// Where Engine's volumes live, and the beacon; New copies them, tests change them.
var shadowDataRoot, shadowMediaRoot, shadowBeaconOn = "/data", "/media", true

// deckState is one deck as a shadow deck follows it.
type deckState struct {
	Deck       int     `json:"deck"`
	Path       string  `json:"path,omitempty"` // a file on this deck
	Net        string  `json:"net,omitempty"`  // Engine's TrackNetworkPath, posted by the QML
	Key        string  `json:"key,omitempty"`  // changes with the track
	Ext        string  `json:"ext,omitempty"`
	Name       string  `json:"name,omitempty"`  // the file's name, which the copy keeps
	Title      string  `json:"title,omitempty"` // a streaming track's real title (QML or stm.db): the copy is named after it
	Artist     string  `json:"artist,omitempty"`
	Loading    bool    `json:"loading,omitempty"` // shadow deck: the peer has a track whose audio is still being fetched
	Playing    bool    `json:"playing"`
	PositionMS float64 `json:"positionMs"`
	Pitch      float64 `json:"pitch"` // percent
	Master     bool    `json:"master,omitempty"`
	Sync       bool    `json:"sync,omitempty"`       // Sync on: a CDJ's BEAT SYNC becomes Engine's Sync on the shadow deck
	BPM        float64 `json:"bpm,omitempty"`        // the track's
	CurrentBPM float64 `json:"currentBpm,omitempty"` // an EngineX deck's played tempo (Pitch is derived from it)
	Beat       float64 `json:"beat,omitempty"`       // Engine's BeatNumber: beats into the bar, 0..4
	// At is when PositionMS was read, Unix ms on this machine's clock: the QML's
	// GUI thread and our niced HTTP handler both lag by tens of ms, so the
	// position is extrapolated from the sample time, not from arrival.
	At int64 `json:"at,omitempty"`
	// Jumps counts the sender's position jumps (hot cue, beat jump, loop, CUE
	// while playing): a count, not a flag, so a lost or merged update still shows it.
	Jumps int `json:"jumps,omitempty"`
	// Cue: Playing only because CUE is held (Engine's PlayState stays false).
	Cue bool `json:"cue,omitempty"`
	// Link is the sender Engine's /Network/NetworkId: the receiver also follows
	// that Engine's PlayState over Engine Link, which arrives ~80 ms sooner.
	Link string `json:"link,omitempty"`
	// Seq orders one sender's updates (its Unix ns at the post): UDP and the
	// HTTP fallback can arrive out of order.
	Seq int64  `json:"seq,omitempty"`
	Dbg string `json:"dbg,omitempty"` // shortcut: Engine's raw master state, until verified on hardware
	// CuePos and Cues are the sender's CUE point and hot cues in track samples;
	// the receiver's QML sets them through Engine's own deck properties. Cues is
	// null when the sender does not say (a CDJ), [] when the track has none.
	CuePos *float64    `json:"cuePos,omitempty"`
	Cues   []shadowCue `json:"cues"`
}

// shadowCue is one hot cue (I 1..8); the receiver checks it before use.
type shadowCue struct {
	I int     `json:"i"`
	P float64 `json:"p"`
	N string  `json:"n,omitempty"`
	C string  `json:"c,omitempty"`
}

type peerDeck struct {
	deckState
	ip string
	at time.Time
}

// precisePos is a CDJ-3000's playhead from its absolute position packet (0x0b,
// every 30 ms) and when it arrived; the status packet's playhead only moves on
// beats, and following that made the shadow deck jump (#49).
type precisePos struct {
	ms float64
	at time.Time
}

// cdjSeen is what observe last saw of a CDJ, to find its edges as they arrive.
type cdjSeen struct {
	playing bool // playing, or previewing with CUE held
	track   uint32
	ms      float64 // the last absolute position (0x0b) and when it came
	at      time.Time
	moving  bool
	jumps   int
}

type shadowState struct {
	mu        sync.Mutex
	precise   map[byte]precisePos
	cdj       map[byte]cdjSeen // per CDJ player number: its last edges and jumps
	own       []deckState
	ownMaster bool
	ownAt     time.Time
	ownPostAt int64                // the QML's clock on the newest post
	peers     map[string]time.Time // EngineX players heard on the beacon
	peerDecks map[int]peerDeck
	peerSeq   map[string]int64  // newest Seq per peer (Engine Link id, else address)
	ver       uint64            // bumped on every new peer update
	notify    chan struct{}     // closed on the bump: wakes the long polls
	conn      *net.UDPConn      // the beacon socket while shadowLoop runs
	files     map[string]string // track key -> local copy
	busy      map[string]bool
	used      map[string]time.Time // when a shadow deck last wanted a copy
	polled    time.Time
	running   bool
	peerPort  int // tests only; 0 is 8080
	pushPort  int // tests only; 0 is shadowBeaconPort

	dataRoot, mediaRoot string
	beacon              bool
	unresolved          [5]string              // per deck: the last TrackNetworkPath logged as having no file
	streamFiles         map[string]streamTrack // streaming uri -> Engine's download (zero = none)
	peerTaken           map[string]time.Time   // when takePeer last accepted an update per peer (as peerSeq)
	dbg                 shadowDebug
}

func newShadowState() *shadowState {
	return &shadowState{precise: map[byte]precisePos{}, cdj: map[byte]cdjSeen{}, peers: map[string]time.Time{}, peerDecks: map[int]peerDeck{}, peerSeq: map[string]int64{}, notify: make(chan struct{}), files: map[string]string{}, busy: map[string]bool{}, used: map[string]time.Time{}, streamFiles: map[string]streamTrack{}, peerTaken: map[string]time.Time{}, dataRoot: shadowDataRoot, mediaRoot: shadowMediaRoot, beacon: shadowBeaconOn}
}

// shadow-v3: copies under the old "shadow" were analysed by Engine itself,
// and Engine keeps a path's analysis; new paths get the source's (#49).
func (s *Server) shadowDir() string { return filepath.Join(filepath.Dir(s.uiDir()), "shadow-v3") }

// activeRole is the role engine-ui-guard applied ("13", "24", "12", "34", "1", "2"),
// or "" without the 4-deck layout.
func (s *Server) activeRole(v uiSettings) string {
	if !v.FourDeckWaveform {
		return ""
	}
	b, err := os.ReadFile(fourDeckRolePath)
	if role := strings.TrimSpace(string(b)); slices.Contains(uiFourDeckRoles[1:], role) {
		return role
	}
	// #160: on an all-in-one console (PRIME 4 / PRIME 4+ / PRIME 4 G2 / SC LIVE 4)
	// Engine already has its decks and needs no player assignment bind-mount; when
	// engine-ui-guard has not written fourDeckRolePath yet, resolve the role directly.
	if err != nil && os.IsNotExist(err) {
		if p := imagePrefix(deviceCompatible()); p != "" && !playerBoards[p] {
			if slices.Contains(uiFourDeckRoles[1:], v.FourDeckRole) {
				return v.FourDeckRole
			}
			return "12"
		}
	}
	return ""
}

func (s *Server) role() string {
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	return s.activeRole(v)
}

// netPathFile maps Engine's TrackNetworkPath ("net://<uuid>/<volume>/<path>")
// to the file on this deck: "(Unknown)" is /data, other volumes are /media/<volume>.
func netPathFile(np, dataRoot, mediaRoot string) string {
	rest, ok := strings.CutPrefix(np, "net://")
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return ""
	}
	base := filepath.Join(mediaRoot, parts[1])
	if parts[1] == "(Unknown)" {
		base = dataRoot
	}
	rel, ok := prolink.SafeRel(parts[2])
	if !ok || strings.Contains(parts[1], "..") {
		return ""
	}
	return filepath.Join(base, rel)
}

var shadowAudioExt = []string{".mp3", ".flac", ".wav", ".aif", ".aiff", ".m4a", ".mp4", ".aac", ".ogg", ".alac"}

func regularAudioFile(f string) bool {
	if !slices.Contains(shadowAudioExt, strings.ToLower(filepath.Ext(f))) {
		return false
	}
	st, err := os.Stat(f)
	return err == nil && st.Mode().IsRegular()
}

// resolveNetPathFile finds the file behind Engine's TrackNetworkPath. Only the
// player that loaded the track (engineCache) may answer with Engine's own copy
// in inMusicTempFiles: a shadow deck loading such a copy made Engine write a new
// one under a new name, the path changed, and the deck reloaded every ~8 s,
// stalling the GUI (#49 stutter).
func resolveNetPathFile(np, dataRoot, mediaRoot string, engineCache bool) string {
	f := netPathFile(np, dataRoot, mediaRoot)
	temp := filepath.Join(dataRoot, "inMusicTempFiles") + string(filepath.Separator)
	if !engineCache && strings.HasPrefix(f, temp) {
		return ""
	}
	if f == "" || regularAudioFile(f) {
		return f
	}
	parts := strings.SplitN(strings.TrimPrefix(np, "net://"), "/", 3)
	rel, ok := prolink.SafeRel(parts[2])
	if !ok {
		return ""
	}
	if i := strings.LastIndex(parts[1], " ("); i > 0 && strings.HasSuffix(parts[1], ")") {
		candidate := filepath.Join(mediaRoot, parts[1][:i], rel)
		if regularAudioFile(candidate) {
			return candidate
		}
	}
	entries, _ := os.ReadDir(mediaRoot)
	for _, entry := range entries {
		if entry.IsDir() {
			candidate := filepath.Join(mediaRoot, entry.Name(), rel)
			if regularAudioFile(candidate) {
				return candidate
			}
		}
	}
	if !engineCache {
		return ""
	}
	entries, _ = os.ReadDir(filepath.Join(dataRoot, "inMusicTempFiles"))
	var cached string
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), "_"+filepath.Base(f)) {
			continue
		}
		candidate := filepath.Join(dataRoot, "inMusicTempFiles", entry.Name())
		if regularAudioFile(candidate) {
			if cached != "" {
				return "" // ambiguous names cannot identify a track
			}
			cached = candidate
		}
	}
	return cached
}

// decksPost takes this deck's own decks from its QML (loopback only: /v1/).
func (s *Server) decksPost(w http.ResponseWriter, r *http.Request) {
	var decks []deckState
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&decks); err != nil {
		fail(w, 400, err)
		return
	}
	s.setDMXDecks(decks)
	own := decks[:0]
	nowMS, seq := time.Now().UnixMilli(), time.Now().UnixNano()
	var postAt int64
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 {
			continue
		}
		d.Seq = seq
		if d.At > nowMS || d.At < nowMS-2000 {
			d.At = nowMS // missing or a clock we cannot trust
		}
		postAt = max(postAt, d.At) // after clamping: one future timestamp must not freeze ownPostAt
		d.Path, d.Name = "", ""
		d.Key, d.Ext, d.Loading = "", "", false
		np := d.Net
		f := resolveNetPathFile(np, s.shadow.dataRoot, s.shadow.mediaRoot, true)
		if netPathFile(np, s.shadow.dataRoot, s.shadow.mediaRoot) == "" {
			d.Net = ""
		}
		if f == "" && strings.HasPrefix(np, "streaming://") {
			st := s.shadow.streamingDownload(r.Context(), np) // Engine's own download, analysed in stm.db
			f = st.path
			if d.Title == "" {
				d.Title, d.Artist = st.title, st.artist
			}
		}
		if f == "" && s.tidal != nil {
			f = s.tidal.streamedFile(np) // loaded from the streaming browser (OmniSource too)
		}
		if f != "" && slices.Contains(shadowAudioExt, strings.ToLower(filepath.Ext(f))) {
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
				d.Path, d.Ext, d.Name = f, strings.ToLower(filepath.Ext(f)), filepath.Base(f)
				if n := displayFileName(d.Artist, d.Title, d.Ext); n != "" {
					d.Name = n // Engine shows a tagless streaming cache file by its name
				}
				d.Key = fmt.Sprintf("%x", fnvSum(fmt.Sprintf("%s %d %d", f, st.Size(), st.ModTime().UnixNano())))
			}
		}
		s.shadow.mu.Lock()
		logIt := np != "" && d.Path == "" && s.shadow.unresolved[d.Deck] != np
		if logIt {
			s.shadow.unresolved[d.Deck] = np
		}
		s.shadow.mu.Unlock()
		if logIt {
			log.Printf("stream-sync: shadow: deck %d track %q has no local file", d.Deck, np)
		}
		own = append(own, d)
	}
	// Pro DJ Link players see these decks in our status: playing, tempo and
	// master. Taking master asks a CDJ that still holds it to hand it over.
	ex := prolink.ExportDeck{At: time.Now()}
	for _, d := range own {
		if d.Master || (d.Playing && !ex.Playing && !ex.Master) {
			ex.BPM, ex.Pitch, ex.Beat = d.BPM, d.Pitch, d.Beat
		}
		ex.Playing = ex.Playing || d.Playing
		ex.Master = ex.Master || d.Master
	}
	s.shadow.mu.Lock()
	if postAt != 0 && postAt < s.shadow.ownPostAt {
		// The QML's XHRs race on separate connections: an older post lost.
		s.shadow.mu.Unlock()
		respond(w, map[string]any{"ok": true})
		return
	}
	s.shadow.ownPostAt = postAt
	s.shadow.own, s.shadow.ownAt = own, time.Now()
	s.shadow.dbg.event("post", map[string]any{"decks": slimDecks(own)})
	claimed := ex.Master && !s.shadow.ownMaster
	s.shadow.ownMaster = ex.Master
	s.shadow.mu.Unlock()
	if s.prolinkExport != nil {
		s.prolinkExport.SetDeck(ex)
		if claimed {
			asked := map[string]bool{} // an all-in-one is one address for several players
			for _, dev := range s.mergedDevices() {
				if ip := net.ParseIP(dev.Address); dev.Master && ip != nil && !asked[dev.Address] && !strings.HasPrefix(dev.Name, "EngineX") && time.Since(dev.LastSeen) < 5*time.Second {
					asked[dev.Address] = true
					s.prolinkExport.RequestMaster(ip)
				}
			}
		}
	}
	s.shadowWake()
	s.shadowPush()
	respond(w, map[string]any{"ok": true})
}

const shadowDecksMagic = "EngineX-decks 1\n"

// ownForPeer is this player's decks for the other one, positions carried to
// now (its clock means nothing there). Called with sh.mu held.
func (sh *shadowState) ownForPeer(now time.Time) []deckState {
	var out []deckState
	if now.Sub(sh.ownAt) < 2*time.Second {
		for _, d := range sh.own {
			d.Path = ""
			if d.Playing {
				d.PositionMS += float64(now.UnixMilli()-d.At) * (1 + d.Pitch/100)
			}
			d.At = 0
			out = append(out, d)
		}
	}
	return out
}

// shadowPush sends this player's decks to the other EngineX players at once
// (unicast, under a millisecond on a LAN).
func (s *Server) shadowPush() {
	sh := s.shadow
	sh.mu.Lock()
	conn, port := sh.conn, sh.pushPort
	var ips []net.IP
	for ip, seen := range sh.peers {
		if time.Since(seen) < 6*time.Second {
			ips = append(ips, net.ParseIP(ip))
		}
	}
	b, err := json.Marshal(sh.ownForPeer(time.Now()))
	sh.mu.Unlock()
	if conn == nil || err != nil {
		return
	}
	if port == 0 {
		port = shadowBeaconPort
	}
	pkt := append([]byte(shadowDecksMagic), b...)
	for _, ip := range ips {
		conn.WriteToUDP(pkt, &net.UDPAddr{IP: ip, Port: port})
	}
}

// takePeer stores a peer's decks unless an update with a newer Seq came first,
// and wakes the long polls. Called with sh.mu held.
func (sh *shadowState) takePeer(ip string, decks []deckState, now time.Time) {
	var seq int64
	for _, d := range decks {
		seq = max(seq, d.Seq)
	}
	// A player on eth0 and wlan0 sends every update from both addresses: order
	// by its Engine Link id, or each copy was taken and woke the QML's poll twice.
	id := ip
	if len(decks) > 0 && decks[0].Link != "" {
		id = decks[0].Link
	}
	// A peer silent for over 2 s may have restarted with a clock behind its old Seq.
	if t, ok := sh.peerTaken[id]; !ok || now.Sub(t) > 2*time.Second {
		delete(sh.peerSeq, id)
	}
	// A clock stepped back by more than 10 s (NTP) restarts the order.
	if last := sh.peerSeq[id]; seq != 0 && seq <= last && seq > last-int64(10*time.Second) {
		sh.dbg.event("peer-stale", map[string]any{"ip": ip, "seq": seq, "last": last})
		return
	}
	sh.dbg.event("peer", map[string]any{"ip": ip, "seq": seq, "decks": slimDecks(decks)})
	sh.peerSeq[id] = seq
	for slot, v := range sh.peerDecks {
		if (v.ip == ip || v.Link != "" && v.Link == id) && !slices.ContainsFunc(decks, func(d deckState) bool { return d.Deck == v.Deck }) {
			delete(sh.peerDecks, slot) // the peer ejected it, or restarted empty
		}
	}
	sh.peerTaken[id] = now
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 {
			continue
		}
		slot := d.Deck
		for s, v := range sh.peerDecks {
			if (v.ip == ip || v.Link != "" && v.Link == id) && v.Deck == d.Deck {
				slot = s
				break
			}
		}
		if cur, ok := sh.peerDecks[slot]; ok && cur.ip != "" && cur.ip != ip && (cur.Link == "" || cur.Link != id) && now.Sub(cur.at) < 2*time.Second {
			for s := 1; s <= 4; s++ {
				if c, taken := sh.peerDecks[s]; !taken || c.ip == ip || c.Link != "" && c.Link == id || now.Sub(c.at) >= 2*time.Second {
					slot = s
					break
				}
			}
		}
		sh.peerDecks[slot] = peerDeck{deckState: d, ip: ip, at: now}
	}
	sh.bump()
}

// bump marks new state and wakes the long polls. Called with sh.mu held.
func (sh *shadowState) bump() {
	sh.ver++
	close(sh.notify)
	sh.notify = make(chan struct{})
}

// wait returns once the peer state is newer than ver, or after d.
func (sh *shadowState) wait(ctx context.Context, ver uint64, d time.Duration) {
	sh.mu.Lock()
	ch, cur := sh.notify, sh.ver
	sh.mu.Unlock()
	if cur != ver {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
}

func fnvSum(v string) uint64 { h := fnv.New64a(); h.Write([]byte(v)); return h.Sum64() }

// peerDecksGet serves this deck's own decks to the other EngineX player (LAN,
// read-only: deck state and the loaded track, nothing else).
func (s *Server) peerDecksGet(w http.ResponseWriter, r *http.Request) {
	s.shadow.mu.Lock()
	out := s.shadow.ownForPeer(time.Now())
	s.shadow.mu.Unlock()
	respond(w, map[string]any{"decks": out})
}

// ownDeckFile is the file on this player's deck {n} if ?key= still names it.
func (s *Server) ownDeckFile(r *http.Request) string {
	n, _ := strconv.Atoi(r.PathValue("n"))
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	for _, d := range s.shadow.own {
		if d.Deck == n && d.Key != "" && d.Key == r.URL.Query().Get("key") {
			return d.Path
		}
	}
	return ""
}

func (s *Server) peerAudioGet(w http.ResponseWriter, r *http.Request) {
	path := s.ownDeckFile(r)
	if path == "" {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "gone", http.StatusConflict)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// peerPerfGet serves the analysis of the track on this player's deck {n} as
// EXPF (#49), read from the library Engine keeps it in; unanalysed is 404.
func (s *Server) peerPerfGet(w http.ResponseWriter, r *http.Request) {
	path := s.ownDeckFile(r)
	if path == "" {
		http.Error(w, "not loaded", http.StatusConflict)
		return
	}
	perf, err := libraryPerf(r.Context(), path, s.shadow.dataRoot, s.shadow.mediaRoot)
	if err != nil {
		http.Error(w, "not analysed", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(perf)
}

// streamingDownload is Engine's own download of a streaming track (Engine
// 5.3.0's TrackNetworkPath for one is just "net://<uuid>/(Unknown)", its
// TrackUri "streaming://TIDAL/Track/<id>"): stm.db's row for the uri, read-only
// (#49). Hits are cached per uri (the QML posts every 120 ms); misses are not,
// since Engine may post before its stm.db row exists.
func (sh *shadowState) streamingDownload(ctx context.Context, uri string) streamTrack {
	sh.mu.Lock()
	t, ok := sh.streamFiles[uri]
	sh.mu.Unlock()
	if ok {
		return t
	}
	lib := filepath.Join(sh.dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
	if db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(lib, "Database2", "stm.db")}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)"); err == nil {
		var rel string
		if db.QueryRowContext(ctx, `SELECT path, COALESCE(title,''), COALESCE(artist,'') FROM Track WHERE uri = ? ORDER BY id DESC LIMIT 1`, uri).Scan(&rel, &t.title, &t.artist) == nil {
			t.path = filepath.Join(lib, filepath.FromSlash(rel))
			if !strings.HasPrefix(t.path, filepath.Clean(sh.dataRoot)+string(filepath.Separator)) || !regularAudioFile(t.path) {
				t = streamTrack{}
			}
		}
		db.Close()
	}
	if t.path != "" {
		sh.mu.Lock()
		sh.streamFiles[uri] = t
		sh.mu.Unlock()
	}
	return t
}

// streamTrack is a streaming track's Engine download and its tags in stm.db.
type streamTrack struct{ path, title, artist string }

// displayFileName is "Artist - Title<ext>" without the characters a file name
// cannot carry; "" when there is no title.
func displayFileName(artist, title, ext string) string {
	clean := func(v string) string {
		v = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
				return -1
			}
			return r
		}, v)
		return strings.TrimSpace(v)
	}
	title, artist = clean(title), clean(artist)
	if title == "" {
		return ""
	}
	if artist != "" {
		title = artist + " - " + title
	}
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120])
	}
	return strings.TrimRight(title, ". ") + ext
}

// engineLibrary is the "Engine Library" folder whose m.db lists f: the
// volume's for /media/<volume>/..., else the internal one.
func engineLibrary(f, dataRoot, mediaRoot string) string {
	if rel, err := filepath.Rel(mediaRoot, f); err == nil && filepath.IsLocal(rel) {
		vol, _, _ := strings.Cut(rel, string(filepath.Separator))
		return filepath.Join(mediaRoot, vol, "Engine Library")
	}
	return filepath.Join(dataRoot, "AppDataUser/AIR Music Technology/Engine/Engine Library")
}

// libraryPerf reads f's analysis from its m.db. Read-only: Engine owns that
// database, and writing it from another process corrupted it (#49).
func libraryPerf(ctx context.Context, f, dataRoot, mediaRoot string) ([]byte, error) {
	lib := engineLibrary(f, dataRoot, mediaRoot)
	rel, err := filepath.Rel(lib, f)
	if err != nil {
		return nil, err
	}
	// Streaming downloads (/data/downloads/<service>/) are in stm.db, not m.db.
	for _, name := range []string{"m.db", "stm.db"} {
		var perf []byte
		if perf, err = dbPerf(ctx, filepath.Join(lib, "Database2", name), filepath.ToSlash(rel)); err == nil {
			return perf, nil
		}
	}
	return nil, err
}

func dbPerf(ctx context.Context, mdb, rel string) ([]byte, error) {
	if _, err := os.Stat(mdb); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var bpm sql.NullFloat64
	var track, beats, overview []byte
	err = db.QueryRowContext(ctx, `SELECT coalesce(t.bpmAnalyzed, t.bpm), p.trackData, p.beatData, p.overviewWaveFormData
		FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.path = ? AND t.isAnalyzed = 1`, rel).
		Scan(&bpm, &track, &beats, &overview)
	if err != nil {
		return nil, err
	}
	return perfFromBlobs(bpm.Float64, track, beats, overview)
}

// perfFromBlobs turns a track's qCompress'd PerformanceData blobs into EXPF v1,
// what engine_ui_hook.c's OfflineAnalyzer hook replays: "EXPF", u32 1, f64 bpm,
// then raw trackData, beatData and overviewWaveFormData, each u32 length +
// bytes, little-endian. trackData gets 5.3.0's waveform peaks if it lacks them.
func perfFromBlobs(bpm float64, track, beats, overview []byte) ([]byte, error) {
	if !(bpm > 0 && bpm < 1000) {
		return nil, fmt.Errorf("perf: bpm %v", bpm)
	}
	var raw [3][]byte
	for i, blob := range [][]byte{track, beats, overview} {
		var err error
		if raw[i], err = enginedb.QUncompress(blob, 8<<20); err != nil {
			return nil, err
		}
	}
	raw[0] = enginedb.TrackDataPeaks(raw[0], raw[2])
	var b bytes.Buffer
	b.WriteString("EXPF")
	binary.Write(&b, binary.LittleEndian, uint32(1))
	binary.Write(&b, binary.LittleEndian, bpm)
	for _, part := range raw {
		binary.Write(&b, binary.LittleEndian, uint32(len(part)))
		b.Write(part)
	}
	return b.Bytes(), nil
}

// savePerf puts the source's analysis next to a copy, or removes a stale one;
// without it Engine analyses the copy itself.
func savePerf(local string, perf []byte) {
	if perf == nil {
		os.Remove(local + ".perf")
		return
	}
	if os.WriteFile(local+".perf.tmp", perf, 0o644) != nil || os.Rename(local+".perf.tmp", local+".perf") != nil {
		os.Remove(local + ".perf.tmp")
	}
}

func fetchPerf(ctx context.Context, url string) []byte {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 || err != nil {
		return nil // a source without /perf: Engine analyses the copy
	}
	return b
}

// cdjCuePlay is a status packet's play state while CUE is held (P1 0x07).
const cdjCuePlay = 7

// observe keeps the absolute positions of Pro DJ Link players and wakes the
// long polls on a CDJ's edge (play, CUE held or released, track, a jump), so
// the shadow deck gets it at once instead of on the next 120 ms refresh.
func (sh *shadowState) observe(d prolink.Device) {
	now := time.Now()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	c := sh.cdj[d.Number]
	edge := false
	switch {
	case d.PacketType == 0x0b && d.PrecisePosition:
		ms := float64(d.PlayPositionMS)
		sh.precise[d.Number] = precisePos{ms, now}
		// A jump: hot cue, CUE back to the cue point, beat jump, loop. The packets
		// come every 30 ms, so a playing deck is off its line by arrival jitter only.
		jump := false
		if !c.at.IsZero() {
			expect := c.ms
			if c.moving {
				expect += float64(now.Sub(c.at).Milliseconds()) * (1 + d.Pitch/100)
			}
			jump = math.Abs(ms-expect) > 80
		}
		if jump {
			c.jumps++
			edge = true
		}
		c.moving = !jump && !c.at.IsZero() && ms > c.ms
		c.ms, c.at = ms, now
	case d.PacketType == 0x0a:
		playing := d.Playing || d.PlayState1 == cdjCuePlay
		edge = playing != c.playing || d.TrackID != c.track
		c.playing, c.track = playing, d.TrackID
	}
	sh.cdj[d.Number] = c
	if edge {
		sh.bump()
	}
}

// shadowGet is what the QML applies to the decks this player does not play.
// With ?ver=N it is a long poll: it answers at once when the peer state moved
// past N, else as soon as it does, at the latest after 120 ms (the CDJ decks
// and the steady positions refresh at that rate).
func (s *Server) shadowGet(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	role := s.activeRole(v)
	debug := s.shadow.dbg.set(v.DebugTelemetry || fileExists(shadowDebugFlag))
	out := []deckState{}
	var ver uint64
	if role != "" {
		s.shadowWake()
		if v, err := strconv.ParseUint(r.URL.Query().Get("ver"), 10, 64); err == nil {
			s.shadow.wait(r.Context(), v, 120*time.Millisecond)
		}
		s.shadow.mu.Lock()
		ver = s.shadow.ver
		s.shadow.mu.Unlock()
		out = s.shadowCompose(role, time.Now())
	}
	respond(w, map[string]any{"role": role, "decks": out, "ver": ver, "debug": debug})
}

func (s *Server) shadowCompose(role string, now time.Time) []deckState {
	devices := s.mergedDevices()
	// The exporter's lock is taken before ours, never under it: closing the
	// exporter waits for its observer, which takes ours.
	var export prolink.ExportStatus
	exported := map[uint32]string{} // track id -> this deck's own file
	if s.prolinkExport != nil {
		if export = s.prolinkExport.Status(); export.Enabled {
			for _, dev := range devices {
				if dev.TrackID != 0 && dev.SourcePlayer == export.Number && dev.SourceSlot == 3 {
					if f := s.prolinkExport.TrackPath(dev.TrackID); f != "" && regularAudioFile(f) {
						exported[dev.TrackID] = f
					}
				}
			}
		}
	}
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	wanted := sh.used
	var out []deckState
	var free []int // shadow decks the other EngineX player does not fill
	var shadowDecks []int
	usedPeer := map[int]bool{}
	placed := map[int]peerDeck{}
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		shadowDecks = append(shadowDecks, deck)
		if p, ok := sh.peerDecks[deck]; ok && now.Sub(p.at) < 2*time.Second {
			placed[deck] = p
			usedPeer[deck] = true
		}
	}
	// #160: a linked player whose deck number overlaps with our own role (e.g. an
	// SC5000 playing on Deck 1 while a PRIME 4 owns Decks 1+2) fills the next free
	// shadow deck (Decks 3/4), while shadowFetchPeer still fetches from p.Deck.
	for _, deck := range shadowDecks {
		if _, ok := placed[deck]; ok {
			continue
		}
		for src := 1; src <= 4; src++ {
			if usedPeer[src] {
				continue
			}
			if p, ok := sh.peerDecks[src]; ok && now.Sub(p.at) < 2*time.Second {
				placed[deck] = p
				usedPeer[src] = true
				break
			}
		}
	}
	for _, deck := range shadowDecks {
		p, ok := placed[deck]
		if !ok {
			free = append(free, deck)
			continue
		}
		d := p.deckState
		d.Deck = deck
		// Two standalone players share master natively over Engine Link; an
		// all-in-one console linked to an SC5000/SC6000 (#160) does not, so keep
		// the peer's master flag when this unit is not a standalone player.
		if pfx := imagePrefix(deviceCompatible()); pfx == "" || playerBoards[pfx] {
			d.Master = false
		}
		if d.Playing {
			d.PositionMS += float64(now.Sub(p.at).Milliseconds()) * (1 + d.Pitch/100)
		}
		d.Path = resolveNetPathFile(d.Net, sh.dataRoot, sh.mediaRoot, false)
		if d.Path == "" && d.Key != "" {
			// Not by address: a player on eth0 and wlan0 is heard from both, and the
			// track was copied and loaded (and analysed) twice.
			key := "peer:" + d.Key
			wanted[key] = now
			d.Path = sh.files[key]
			if d.Path == "" {
				s.shadowFetchPeer(key, p)
			}
		}
		// Audio still on its way: the QML shows the deck as loading. No track on
		// the peer at all: nothing to hold, so the QML unloads the shadow deck.
		d.Loading = d.Path == "" && d.Key != ""
		if p.Net == "" && p.Key == "" {
			d.Path, d.Loading = "", false
		}
		d.Key, d.Ext, d.Net = "", "", ""
		d.At = now.UnixMilli()
		out = append(out, d)
	}
	for deck, dev := range cdjDecks(devices, free, now) {
		cue := dev.PlayState1 == cdjCuePlay
		d := deckState{Deck: deck, Playing: dev.Playing || cue, Cue: cue, Jumps: sh.cdj[dev.Number].jumps, PositionMS: float64(dev.PlayPositionMS), Pitch: dev.Pitch, Master: dev.Master, Sync: dev.Sync, BPM: dev.BPM}
		at := dev.LastSeen
		if p, ok := sh.precise[dev.Number]; ok && now.Sub(p.at) < time.Second {
			d.PositionMS, at = p.ms, p.at
		}
		if d.Playing {
			d.PositionMS += float64(now.Sub(at).Milliseconds()) * (1 + d.Pitch/100)
		}
		d.At = now.UnixMilli()
		if f := exported[dev.TrackID]; f != "" && dev.SourcePlayer == export.Number {
			d.Path = f // the CDJ plays from this deck's own drive (#63): no copy
		} else if dev.TrackID != 0 {
			if host := sourceHost(export, devices, dev.SourcePlayer); host != nil {
				key := fmt.Sprintf("cdj:%s:%d:%d", host, dev.SourceSlot, dev.TrackID)
				wanted[key] = now
				d.Path = sh.files[key]
				if d.Path == "" {
					s.shadowFetchCDJ(key, host, dev.SourceSlot, dev.TrackID)
				}
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	return out
}

// cdjDecks puts the CDJs on the free shadow decks: on their own player number
// when it is free, the rest on what is left, Deck 2 first. A lone CDJ next to
// a 1+3 player then shows beside Deck 1 also in the 2-waveform layout.
func cdjDecks(devices []prolink.Device, free []int, now time.Time) map[int]prolink.Device {
	// A CDJ that changed its number keeps its old entry for a few seconds, which
	// became a second (master) CDJ. An all-in-one (XDJ-XZ, XDJ-AZ) is several
	// players on one address, all sending every 200 ms or so. So keep only the
	// numbers an address sent from within 1.5 s of its newest packet.
	var live []prolink.Device
	newest := map[string]time.Time{}
	for _, dev := range devices {
		if dev.Number >= 1 && dev.Number <= 6 && !strings.HasPrefix(dev.Name, "EngineX") && now.Sub(dev.LastSeen) <= 5*time.Second {
			live = append(live, dev)
			if dev.LastSeen.After(newest[dev.Address]) {
				newest[dev.Address] = dev.LastSeen
			}
		}
	}
	var cdjs []prolink.Device
	for _, dev := range live {
		if newest[dev.Address].Sub(dev.LastSeen) <= 1500*time.Millisecond {
			cdjs = append(cdjs, dev)
		}
	}
	slices.SortFunc(cdjs, func(a, b prolink.Device) int { return int(a.Number) - int(b.Number) })
	out := map[int]prolink.Device{}
	var rest []prolink.Device
	for _, dev := range cdjs {
		if deck := int(dev.Number); slices.Contains(free, deck) {
			out[deck] = dev
		} else {
			rest = append(rest, dev)
		}
	}
	for _, deck := range []int{2, 4, 1, 3} {
		if _, taken := out[deck]; len(rest) > 0 && !taken && slices.Contains(free, deck) {
			out[deck], rest = rest[0], rest[1:]
		}
	}
	return out
}

// sourceHost is the player whose media holds a CDJ's track: another player,
// or this deck's own export (#63).
func sourceHost(st prolink.ExportStatus, devices []prolink.Device, n byte) net.IP {
	if st.Enabled && st.Number == n {
		if host, _, err := net.SplitHostPort(st.DBAddress); err == nil {
			return net.ParseIP(host)
		}
	}
	for _, d := range devices {
		if d.Number == n {
			return net.ParseIP(d.Address)
		}
	}
	return nil
}

// shadowFetch* run with sh.mu held; the copy itself runs unlocked.
func (s *Server) shadowFetchCDJ(key string, host net.IP, slot byte, id uint32) {
	s.shadowFetch(key, func(ctx context.Context) (string, error) {
		c := &prolink.NFSClient{IP: host}
		local, err := prolink.FetchTrack(ctx, c, slot, id, s.shadowDir())
		if err != nil {
			return "", err
		}
		var perf []byte
		if t, a, err := prolink.FetchAnalysis(ctx, c, slot, id); err == nil {
			if p, err := enginedb.EncodePerformance(t, a); err == nil && p.Complete {
				perf, _ = perfFromBlobs(t.BPM, p.TrackData, p.Beats, p.Overview)
			}
		}
		savePerf(local, perf)
		return local, nil
	})
}

func (s *Server) shadowFetchPeer(key string, p peerDeck) {
	port := s.shadow.peerPort
	if port == 0 {
		port = 8080
	}
	url := fmt.Sprintf("http://%s/peer/decks/%d/audio?key=%s", net.JoinHostPort(p.ip, strconv.Itoa(port)), p.Deck, url.QueryEscape(p.Key))
	ext := ".mp3"
	if slices.Contains(shadowAudioExt, p.Ext) {
		ext = p.Ext
	}
	// The copy keeps the peer's file name (Engine shows it for a file without tags).
	name := filepath.Base(p.Name)
	if name == "." || name == ".." || strings.ToLower(filepath.Ext(name)) != ext {
		name = "track" + ext
	}
	dir := filepath.Join(s.shadowDir(), fmt.Sprintf("peer-%x", fnvSum(key)))
	// The UI stretches a rekordbox export's low overview by its Contents/ path
	// (scOverviewScale): a copy of such a track keeps it, so it draws as it does there.
	if strings.Contains(p.Net, "/Contents/") {
		dir = filepath.Join(dir, "Contents")
	}
	local := filepath.Join(dir, name)
	s.shadowFetch(key, func(ctx context.Context) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("peer audio: %s", resp.Status)
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, err = io.Copy(f, io.LimitReader(resp.Body, 1<<30))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		if err := os.Rename(f.Name(), local); err != nil {
			return "", err
		}
		// Engine loads the copy only after this returns, so it sees the .perf.
		savePerf(local, fetchPerf(ctx, strings.Replace(url, "/audio?", "/perf?", 1)))
		return local, nil
	})
}

func (s *Server) shadowFetch(key string, fetch func(context.Context) (string, error)) {
	sh := s.shadow
	if sh.busy[key] {
		return
	}
	sh.busy[key] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		path, err := fetch(ctx)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		delete(sh.busy, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream-sync: shadow deck copy %s: %v\n", key, err)
			return
		}
		sh.files[key] = path
		// Keep copies a shadow deck wanted in the last 2 minutes: deleting the
		// previous track at once failed Engine's load when the CDJ switched fast.
		for k, f := range sh.files {
			if k != key && time.Since(sh.used[k]) > 2*time.Minute {
				os.Remove(f)
				os.Remove(f + ".perf")
				for dir := filepath.Dir(f); dir != s.shadowDir() && strings.HasPrefix(dir, s.shadowDir()); dir = filepath.Dir(dir) {
					os.Remove(dir) // the copy's own directories, once empty
				}
				delete(sh.files, k)
				delete(sh.used, k)
			}
		}
	}()
}

// shadowWake starts the beacon and peer polling while a shadow deck is in use;
// they stop 10 s after the last poll.
func (s *Server) shadowWake() {
	sh := s.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.polled = time.Now()
	if sh.running {
		return
	}
	os.RemoveAll(filepath.Join(filepath.Dir(s.uiDir()), "shadow")) // before shadow-v3
	sh.running = true
	go s.shadowLoop()
}

func (s *Server) shadowLoop() {
	sh := s.shadow
	var conn *net.UDPConn
	if sh.beacon {
		conn = shadowBeacon()
	}
	if conn != nil {
		sh.mu.Lock()
		sh.conn = conn
		sh.mu.Unlock()
		defer func() {
			sh.mu.Lock()
			sh.conn = nil
			sh.mu.Unlock()
			conn.Close()
		}()
		go func() {
			b := make([]byte, 64<<10)
			for {
				n, addr, err := conn.ReadFromUDP(b)
				if err != nil {
					return
				}
				if !isLocalIP(addr.IP) {
					sh.receive(b[:n], addr.IP.String(), time.Now())
				}
			}
		}()
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.closed:
			sh.mu.Lock()
			sh.running = false
			ips, port := slices.Collect(maps.Keys(sh.peers)), cmp.Or(sh.pushPort, shadowBeaconPort)
			sh.mu.Unlock()
			if conn != nil {
				// Goodbye (no decks): the other player unloads its shadow decks at once.
				bye := []byte(shadowDecksMagic + "[]")
				conn.WriteToUDP(bye, &net.UDPAddr{IP: net.IPv4bcast, Port: port})
				for _, ip := range ips {
					conn.WriteToUDP(bye, &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
				}
			}
			return
		case <-tick.C:
		}
		sh.mu.Lock()
		if time.Since(sh.polled) > 10*time.Second {
			sh.running = false
			sh.mu.Unlock()
			return
		}
		sh.mu.Unlock()
		if conn != nil && i%8 == 0 {
			shadowSendBeacons(conn)
		}
		s.shadowPollPeers()
	}
}

// shadowSendBeacons sends the discovery beacon on the default route and from
// each broadcast-capable IPv4 interface (so eth0 is always discovered even when
// the default route is on wlan0).
func shadowSendBeacons(conn *net.UDPConn) {
	msg := []byte("EngineX-shadow 1")
	conn.WriteToUDP(msg, &net.UDPAddr{IP: net.IPv4bcast, Port: shadowBeaconPort})
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagBroadcast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			mask := ipn.Mask
			if ip4 == nil || len(mask) != 4 {
				continue
			}
			bcast := net.IPv4(ip4[0]|^mask[0], ip4[1]|^mask[1], ip4[2]|^mask[2], ip4[3]|^mask[3])
			lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
				c.Control(func(fd uintptr) {
					_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
				})
				return nil
			}}
			if pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(ip4.String(), "0")); err == nil {
				_, _ = pc.WriteTo(msg, &net.UDPAddr{IP: bcast, Port: shadowBeaconPort})
				pc.Close()
			}
		}
	}
}

// receive takes a datagram on the beacon port: a beacon or a peer's decks.
func (sh *shadowState) receive(b []byte, ip string, now time.Time) {
	var decks []deckState
	beacon := string(b) == "EngineX-shadow 1"
	if !beacon {
		rest, ok := bytes.CutPrefix(b, []byte(shadowDecksMagic))
		if !ok || json.Unmarshal(rest, &decks) != nil {
			return
		}
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.peers[ip] = now
	if !beacon {
		sh.takePeer(ip, decks, now)
	}
}

func shadowBeacon() *net.UDPConn {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}
		})
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":"+strconv.Itoa(shadowBeaconPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "stream-sync: shadow deck beacon: %v\n", err)
		return nil
	}
	return pc.(*net.UDPConn)
}

func isLocalIP(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return ip.IsLoopback()
}

var peerClient = &http.Client{Timeout: 400 * time.Millisecond}

// shadowPollPeers reads the other EngineX players' decks.
func (s *Server) shadowPollPeers() {
	sh := s.shadow
	sh.mu.Lock()
	var ips []string
	for ip, seen := range sh.peers {
		if time.Since(seen) < 6*time.Second {
			ips = append(ips, ip)
		}
	}
	port := sh.peerPort
	sh.mu.Unlock()
	if port == 0 {
		port = 8080
	}
	for _, ip := range ips {
		resp, err := peerClient.Get("http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/peer/decks")
		if err != nil {
			continue
		}
		var body struct{ Decks []deckState }
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		sh.mu.Lock()
		sh.takePeer(ip, body.Decks, time.Now())
		sh.mu.Unlock()
	}
}
