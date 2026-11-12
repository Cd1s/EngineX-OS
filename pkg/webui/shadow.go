package webui

// Shadow decks (#49). In the 4-deck layout a player plays the decks of its
// role and shows the other two. By default QML renders source display assets
// (shadow_display.go), keeping the corresponding local Engine slots empty.
// The opt-in legacy mode drives silent local Engine decks and copies audio.
// Sources are the other EngineX player, whose QML posts its own
// decks to POST /v1/sc5000/decks, or a CDJ on Pro DJ Link, by player number.
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
	"errors"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

const shadowBeaconPort = 50199 // EngineX players find each other here

// Where Engine's volumes live, and the beacon; New copies them, tests change them.
var shadowDataRoot, shadowMediaRoot, shadowBeaconOn = "/data", "/media", true

// deckState is one deck as a shadow deck follows it.
type deckState struct {
	baseKey      string  // source-only file identity; Key also identifies the loaded session
	Deck         int     `json:"deck"`
	Path         string  `json:"path,omitempty"` // a file on this deck
	Net          string  `json:"net,omitempty"`  // Engine's TrackNetworkPath, posted by the QML
	Key          string  `json:"key,omitempty"`  // changes with the track
	Ext          string  `json:"ext,omitempty"`
	Name         string  `json:"name,omitempty"`  // the file's name, which the copy keeps
	Title        string  `json:"title,omitempty"` // a streaming track's real title (QML or stm.db): the copy is named after it
	Artist       string  `json:"artist,omitempty"`
	KeyName      string  `json:"keyName,omitempty"`      // the key as the source's header writes it ("-": none); the shadow header shows it, not the copy's own analysis
	ID           string  `json:"id,omitempty"`           // Engine mode, what the source has loaded: its Key (a copy), its Net (this player's own file), a CDJ's slot and track id. The QML loads again when it changes
	DurationMS   float64 `json:"durationMs,omitempty"`   // the track's length on the source: tells the QML whether Engine holds this track or another
	Loading      bool    `json:"loading,omitempty"`      // shadow deck: the peer has a track whose audio is still being fetched
	Progress     float64 `json:"progress,omitempty"`     // while loading, 0..1: how much of the copy (Engine mode) or of the source's detail waveform is done. Absent: not known, the QML shows a ring that turns
	Error        string  `json:"error,omitempty"`        // Engine mode: why the copy failed (shadowCopy.err); it is tried again
	Stale        bool    `json:"stale,omitempty"`        // frozen display sample; not an eject/goodbye
	MetadataOnly bool    `json:"metadataOnly,omitempty"` // CDJ state has no peer display asset endpoint
	LineIn       bool    `json:"lineIn,omitempty"`       // all-in-one console: shadow source mapped to Line In channel
	DisplayOnly  bool    `json:"displayOnly,omitempty"`  // this slot goes to the display renderer; Engine must not load it
	Fallback     string  `json:"fallback,omitempty"`     // auto mode, a CDJ on the display renderer: why Engine cannot have its file (cdjFallback); the QML says so on the lane
	Playing      bool    `json:"playing"`
	PositionMS   float64 `json:"positionMs"`
	Pitch        float64 `json:"pitch"` // percent
	// Rate is how fast PositionMS runs on the receiver's clock, track ms per ms,
	// measured (peerDeck.skew). Absent: 1 + Pitch/100.
	Rate float64 `json:"rate,omitempty"`
	// Range is the source deck's pitch range in percent (Engine's SpeedRange) and
	// KeyLock its key lock: the shadow Engine deck is set to both. Absent from a
	// CDJ and an older sender: the shadow deck keeps its own.
	Range   float64 `json:"range,omitempty"`
	KeyLock *bool   `json:"keylock,omitempty"`
	Master  *bool   `json:"master,omitempty"`
	// Lead is the deck's own DeckIsMaster, what its header shows as LEAD, for the
	// shadow header only. Master also marks the player that holds the network
	// master with no master deck, and is cleared for a standalone player's copy.
	Lead *bool `json:"lead,omitempty"`
	// Loop, KeyOffset, Stems and Slip are what the source's header and overview show
	// beside the track: an active loop, the key shift in semitones, the Stems icon,
	// and the slip timeline's position in samples while slip is active (0: it is
	// not). For the shadow deck's display only: the copy takes none of them.
	Loop      bool  `json:"loop,omitempty"`
	KeyOffset int   `json:"keyOffset,omitempty"`
	Stems     bool  `json:"stems,omitempty"`
	Slip      int64 `json:"slip,omitempty"`
	// Active: this deck is on the sender's active layer (its Layer A/B), what the
	// paired 2-deck view (#201) shows of that player. An empty deck says it too.
	Active bool `json:"active,omitempty"`
	// ShadowMaster is the sender's shadow deck (a copy of the receiver's deck of
	// that number) that Engine Link left holding master: the receiver's QML takes
	// master back to a real deck. On every deck the sender posts; 0: none.
	ShadowMaster int     `json:"shadowMaster,omitempty"`
	Sync         bool    `json:"sync,omitempty"`       // Sync on: a CDJ's BEAT SYNC becomes Engine's Sync on the shadow deck
	BPM          float64 `json:"bpm,omitempty"`        // the track's
	CurrentBPM   float64 `json:"currentBpm,omitempty"` // an EngineX deck's played tempo (Pitch is derived from it)
	Beat         float64 `json:"beat,omitempty"`       // Engine's BeatNumber: beats into the bar, 0..4
	// At is when PositionMS was read, Unix ms on this machine's clock: the QML's
	// GUI thread and our niced HTTP handler both lag by tens of ms, so the
	// position is extrapolated from the sample time, not from arrival.
	At int64 `json:"at,omitempty"`
	// Sent is when this update left for the peer, Unix ms on the sender's clock,
	// with PositionMS carried to it: the receiver adds what the trip took over
	// the quickest one (peerLate).
	Sent int64 `json:"sent,omitempty"`
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
	// DisplayDetail (display compose only): the detail waveform of this deck's
	// display data is ready here, so the QML re-reads it once instead of polling.
	DisplayDetail bool `json:"displayDetail,omitempty"`
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
	// The playhead line as of the last version bump: what the QML extrapolates
	// from until the next bump. Updates on this line do not bump (see takePeer).
	anchorPos float64
	anchorAt  time.Time
	// skew is how far the deck runs off its pitch on this machine's clock, as a
	// fraction: the peer's audio clock against our system clock (-165 ppm on two
	// SC5000) and its pitch, which is rounded to 0.01 %. Measured from ref, the
	// present line once it has settled, and kept across tracks and pitches.
	skew   float64
	refPos float64
	refAt  time.Time
	local  string // this deck's own copy of the track, see localPeerTrack
	head   bool   // Engine mode: only the head of the display data (cover, key); the source computes no detail waveform for it
}

// speed is how fast the deck's position runs on this machine's clock.
func (p peerDeck) speed() float64 { return (1 + p.Pitch/100) * (1 + p.skew) }

// carried is the deck's position d ago moved on to now, and the Rate to carry
// it further with once that is measured.
func (p peerDeck) carried(d time.Duration) (pos, rate float64) {
	pos = p.PositionMS + float64(d.Microseconds())/1000*p.speed()
	if p.skew != 0 {
		rate = math.Round(p.speed()*1e6) / 1e6
	}
	return pos, rate
}

type peerLocalFile struct {
	file, key string
	at        time.Time
}

// Tolerances of samePeerPicture: a playing deck's position may run this far off
// the line the QML extrapolates before the long poll is woken. The sender posts a
// filtered line (a few ms of noise) and reports a slip of 15 ms at once (jog
// nudge, its audio clock against the nominal pitch); the QML steers its scroll
// to the line it has, so more than that must reach it.
// A paused deck must not move at all.
const (
	shadowPlayingDriftMS = 12
	shadowPausedDriftMS  = 0.5
)

// A deck's speed is measured over at least shadowSkewSpan of one line, from
// updates that came within shadowSkewLateMS of the quickest trip (each is a few
// ms of noise: 30 s gives ~50 ppm, and the span grows with the line). The line
// counts from shadowSkewSettle on: the sender's first post of a line is up to
// 12 ms behind, its next one (5 s) is not. The speed in use stays until the
// line it gives misses an update by shadowSkewOffMS, which is beyond the noise.
// A result beyond shadowSkewMax is a nudge inside the tolerance, not a clock.
const (
	shadowSkewSettle = 6 * time.Second
	shadowSkewSpan   = 30 * time.Second
	shadowSkewLateMS = 2
	shadowSkewOffMS  = 2
	shadowSkewMax    = 500e-6
)

func sameOptionalBool(a, b *bool) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// samePeerPicture is true when d shows what old already shows: the same track,
// state, tempo and markers, and a playhead on old's anchored line.
// Seq, At, Beat and Dbg do not count: the display QML does not use them.
func samePeerPicture(old peerDeck, d deckState, now time.Time) bool {
	o := old.deckState
	if o.Deck != d.Deck || o.Path != d.Path || o.Net != d.Net || o.Key != d.Key || o.Ext != d.Ext || o.Name != d.Name ||
		o.Title != d.Title || o.Artist != d.Artist || o.KeyName != d.KeyName || o.Loading != d.Loading || o.Active != d.Active || o.Stale != d.Stale || o.MetadataOnly != d.MetadataOnly ||
		o.Playing != d.Playing || o.Pitch != d.Pitch || !sameOptionalBool(o.Master, d.Master) || !sameOptionalBool(o.Lead, d.Lead) || o.Loop != d.Loop || o.KeyOffset != d.KeyOffset || o.Stems != d.Stems || o.Slip != d.Slip || o.ShadowMaster != d.ShadowMaster || o.Sync != d.Sync || o.BPM != d.BPM ||
		o.CurrentBPM != d.CurrentBPM || o.Range != d.Range || (o.KeyLock == nil) != (d.KeyLock == nil) || (o.KeyLock != nil && *o.KeyLock != *d.KeyLock) ||
		o.Jumps != d.Jumps || o.Cue != d.Cue || o.Link != d.Link || (o.CuePos == nil) != (d.CuePos == nil) ||
		(o.CuePos != nil && *o.CuePos != *d.CuePos) || (o.Cues == nil) != (d.Cues == nil) || !slices.Equal(o.Cues, d.Cues) {
		return false
	}
	if !o.Playing {
		return math.Abs(old.anchorPos-d.PositionMS) <= shadowPausedDriftMS
	}
	expect := old.anchorPos + float64(now.Sub(old.anchorAt).Microseconds())/1000*old.speed()
	return math.Abs(expect-d.PositionMS) <= shadowPlayingDriftMS
}

// precisePos is a CDJ-3000's playhead from its absolute position packets (0x0b,
// every 30 ms): where its line (observe) was when the last one arrived; the
// status packet's playhead only moves on beats, and following that made the
// shadow deck jump (#49).
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
	// The playhead line (see observe): where the deck is at at, as its quickest
	// packets say, and the rate it runs at.
	line, rate float64
	late       float64 // the least a packet was behind the line, of the n since the line last moved
	n          int
	pitch      float64 // from the status packet (0x0a), exact
	fine       bool
}

// exact is the CDJ's pitch in percent. Position packets round it to 0.01 %,
// which alone slips 3 ms a minute and, taking turns with the status packet's
// exact value, moved the shadow deck's fader back and forth; so the status
// packet's value counts while the rounded one agrees with it.
func (c cdjSeen) exact(pitch float64) float64 {
	if c.fine && math.Abs(c.pitch-pitch) <= 0.0051 {
		return c.pitch
	}
	return pitch
}

type shadowState struct {
	fetchCalls    atomic.Uint64
	fetchCDJCalls atomic.Uint64
	mu            sync.Mutex
	precise       map[byte]precisePos
	cdjPortmap    int              // tests: the players' portmap port; 0 is UDP 111
	cdj           map[byte]cdjSeen // per CDJ player number: its last edges and jumps
	own           []deckState
	ownMaster     bool
	ownAt         time.Time          // the newest post or alive ping
	ownExport     prolink.ExportDeck // what the newest post told the Pro DJ Link export
	ownPostAt     int64              // the QML's clock on the newest post
	clockDecks    []deckState        // #209: lightweight local deck clock feed when 4-deck shadow is off
	clockAt       time.Time
	clockPostAt   int64
	clockPolled   time.Time
	peers         map[string]time.Time // EngineX players heard on the beacon
	peerDecks     map[int]peerDeck
	peerLocal     map[string]peerLocalFile // by TrackNetworkPath
	peerSeq       map[string]int64         // newest Seq per peer (Engine Link id, else address)
	ver           uint64                   // bumped on every new peer update
	notify        chan struct{}            // closed on the bump: wakes the long polls
	conn          *net.UDPConn             // the beacon socket while shadowLoop runs
	files         map[string]string        // track key -> local copy
	busy          map[string]bool
	copies        map[string]*shadowCopy // track key -> the copy on its way, or how it failed
	copyRetry     time.Duration          // after a copy's first failure; doubles up to 15 times this
	copyTimeout   time.Duration
	used          map[string]time.Time // when a shadow deck last wanted a copy
	polled        time.Time
	running       bool
	peerPort      int // tests only; 0 is 8080
	pushPort      int // tests only; 0 is shadowBeaconPort

	dataRoot, mediaRoot string
	beacon              bool
	unresolved          [5]string              // per deck: the last TrackNetworkPath logged as having no file
	streamFiles         map[string]streamTrack // streaming uri -> Engine's download (zero = none)
	peerTaken           map[string]time.Time   // when takePeer last accepted an update per peer (as peerSeq)
	peerClock           map[string]peerClock   // per peer (as peerSeq)
	dbg                 shadowDebug
	metaOnlyPoll        bool // the newest compose had a CDJ deck, including cached display assets: 120 ms polls
	display             shadowDisplayCache
	sourceDisplay       shadowSourceCache
	lines               []int // Line/Phono mixer channel inputs on all-in-one consoles
}

func newShadowState() *shadowState {
	return &shadowState{precise: map[byte]precisePos{}, cdj: map[byte]cdjSeen{}, peers: map[string]time.Time{}, peerDecks: map[int]peerDeck{}, peerSeq: map[string]int64{}, notify: make(chan struct{}), files: map[string]string{}, busy: map[string]bool{}, copies: map[string]*shadowCopy{}, copyRetry: time.Second, copyTimeout: 2 * time.Minute, used: map[string]time.Time{}, streamFiles: map[string]streamTrack{}, peerTaken: map[string]time.Time{}, peerClock: map[string]peerClock{}, dataRoot: shadowDataRoot, mediaRoot: shadowMediaRoot, beacon: shadowBeaconOn}
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
		d.Key, d.Ext, d.Loading, d.ID = "", "", false, ""
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
		if d.Key != "" {
			d.baseKey = d.Key
			d.Key = fmt.Sprintf("%s-%x", d.baseKey, seq)
			for _, previous := range s.shadow.own {
				if previous.Deck == d.Deck && previous.baseKey == d.baseKey && previous.Path == d.Path && previous.Key != "" {
					d.Key = previous.Key
					break
				}
			}
		}
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
		master := d.Master != nil && *d.Master
		if master || (d.Playing && !ex.Playing && !ex.Master) {
			ex.BPM, ex.Pitch, ex.Beat = d.BPM, d.Pitch, d.Beat
		}
		ex.Playing = ex.Playing || d.Playing
		ex.Master = ex.Master || master
	}
	s.shadow.mu.Lock()
	nowMS = time.Now().UnixMilli()
	if postAt > nowMS || (postAt != 0 && postAt < s.shadow.ownPostAt && s.shadow.ownPostAt <= nowMS) {
		// Reject older samples, including a pre-rollback handler that finished
		// late. A previous baseline now in the future belongs to the old clock:
		// the first current-clock post replaces it instead of waiting to catch up.
		s.shadow.mu.Unlock()
		respond(w, map[string]any{"ok": true})
		return
	}
	s.shadow.ownPostAt = postAt
	s.shadow.own, s.shadow.ownAt, s.shadow.ownExport = own, time.Now(), ex
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

// decksAlive is the QML's heartbeat between posts: nothing changed on its decks
// since the last post, so it sends no JSON (each serialisation is garbage on
// Engine's GUI-thread heap). Its decks stay fresh for the peer, which extrapolates
// them; 409 asks the QML for a full post (we restarted, or never had one).
func (s *Server) decksAlive(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	sh := s.shadow
	sh.mu.Lock()
	if sh.ownAt.IsZero() || now.Sub(sh.ownAt) >= 2*time.Second {
		sh.mu.Unlock()
		w.WriteHeader(http.StatusConflict) // stale or unknown: the post brings the state back
		return
	}
	sh.ownAt = now
	seq := now.UnixNano()
	for i := range sh.own {
		sh.own[i].Seq = seq // a new update for the peer's order check (UDP and HTTP fallback)
	}
	ex := sh.ownExport.Carried(now)
	sh.ownExport = ex
	sh.mu.Unlock()
	if s.prolinkExport != nil {
		s.prolinkExport.SetDeck(ex)
	}
	s.shadowWake()
	s.shadowPush()
	w.WriteHeader(http.StatusNoContent)
}

func deckEffectiveBPM(d deckState) float64 {
	if d.CurrentBPM > 0 {
		return d.CurrentBPM
	}
	if d.BPM > 0 {
		return d.BPM * (1 + d.Pitch/100)
	}
	return 0
}

func wrapBarBeat(b float64) float64 {
	b = math.Mod(b, 4.0)
	if b < 0 {
		b += 4.0
	}
	return b
}

// ownForPeer is this player's decks for the other one, positions carried to
// now (its clock means nothing there). Called with sh.mu held.
func (sh *shadowState) ownForPeer(now time.Time) []deckState {
	var out []deckState
	if len(sh.own) != 0 {
		for _, d := range sh.own {
			d.Path = ""
			d.Stale = now.Sub(sh.ownAt) >= 2*time.Second
			if d.Playing {
				// The QML posts only on a change and pings in between: carry the
				// position and beat phase on while it is alive, at most 2 s past its last word.
				until := min(now.UnixMilli(), sh.ownAt.UnixMilli()+2000)
				dtMS := max(int64(0), until-d.At)
				d.PositionMS += float64(dtMS) * (1 + d.Pitch/100)
				if tempo := deckEffectiveBPM(d); tempo > 0 && d.Beat >= 0 {
					d.Beat = wrapBarBeat(d.Beat + float64(dtMS)/1000.0*tempo/60.0)
				}
			}
			d.At, d.Sent = 0, now.UnixMilli()
			out = append(out, d)
		}
	}
	return out
}

type clockDeck struct {
	Deck       int     `json:"deck"`
	Source     string  `json:"source"`
	SourceID   string  `json:"sourceId,omitempty"`
	Master     bool    `json:"master,omitempty"`
	Lead       bool    `json:"lead,omitempty"`
	Playing    bool    `json:"playing"`
	Cue        bool    `json:"cue,omitempty"`
	BPM        float64 `json:"bpm,omitempty"`
	CurrentBPM float64 `json:"currentBpm,omitempty"`
	Pitch      float64 `json:"pitch,omitempty"`
	Beat       float64 `json:"beat"`
	PositionMS float64 `json:"positionMs,omitempty"`
	At         int64   `json:"at"`
}

// deckClockPost takes lightweight local deck beat samples from Engine's UI pack
// when 4-deck shadow mode is off (#209). Memory only; never starts shadow beacons.
func (s *Server) deckClockPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
	if err != nil {
		fail(w, 400, err)
		return
	}
	var decks []deckState
	if err := json.Unmarshal(body, &decks); err != nil {
		var env struct {
			Decks []deckState `json:"decks"`
		}
		if e2 := json.Unmarshal(body, &env); e2 != nil {
			fail(w, 400, err)
			return
		}
		decks = env.Decks
	}
	now := time.Now()
	nowMS := now.UnixMilli()
	var postAt int64
	var seen [5]bool
	clean := decks[:0]
	for _, d := range decks {
		if d.Deck < 1 || d.Deck > 4 || seen[d.Deck] {
			continue
		}
		seen[d.Deck] = true
		if d.At > nowMS || d.At < nowMS-2000 {
			d.At = nowMS
		}
		postAt = max(postAt, d.At)
		clean = append(clean, d)
	}
	sh := s.shadow
	sh.mu.Lock()
	nowMS = time.Now().UnixMilli()
	if postAt > nowMS || (postAt != 0 && postAt < sh.clockPostAt && sh.clockPostAt <= nowMS) {
		sh.mu.Unlock()
		respond(w, map[string]any{"ok": true})
		return
	}
	sh.clockPostAt = postAt
	sh.clockDecks = clean
	sh.clockAt = now
	sh.mu.Unlock()
	respond(w, map[string]any{"ok": true})
}

func carriedClockDeck(d deckState, source, sourceID string, lastSeen time.Time, now time.Time) clockDeck {
	pos := d.PositionMS
	beat := d.Beat
	at := d.At
	if at <= 0 {
		at = lastSeen.UnixMilli()
	}
	if d.Playing {
		until := min(now.UnixMilli(), lastSeen.UnixMilli()+2000)
		dtMS := max(int64(0), until-at)
		rate := 1 + d.Pitch/100
		if d.Pitch == 0 && d.BPM > 0 && d.CurrentBPM > 0 {
			rate = d.CurrentBPM / d.BPM
		}
		pos += float64(dtMS) * rate
		if tempo := deckEffectiveBPM(d); tempo > 0 && beat >= 0 {
			beat = wrapBarBeat(beat + float64(dtMS)/1000.0*tempo/60.0)
		}
	}
	master := d.Master != nil && *d.Master
	lead := d.Lead != nil && *d.Lead
	return clockDeck{
		Deck:       d.Deck,
		Source:     source,
		SourceID:   sourceID,
		Master:     master,
		Lead:       lead,
		Playing:    d.Playing || d.Cue,
		Cue:        d.Cue,
		BPM:        d.BPM,
		CurrentBPM: d.CurrentBPM,
		Pitch:      d.Pitch,
		Beat:       beat,
		PositionMS: pos,
		At:         now.UnixMilli(),
	}
}

// deckClockGet exposes the current local, peer, and Pro DJ Link deck beat state
// over loopback for Mods such as dmx-studio (#209), without starting shadow beacons.
func (s *Server) deckClockGet(w http.ResponseWriter, r *http.Request) {
	devices := s.mergedDevices()
	now := time.Now()
	nowMS := now.UnixMilli()
	sh := s.shadow
	sh.mu.Lock()
	sh.clockPolled = now
	var out []clockDeck
	switch {
	case len(sh.own) > 0 && now.Sub(sh.ownAt) < 2*time.Second:
		for _, d := range sh.own {
			out = append(out, carriedClockDeck(d, "local", fmt.Sprintf("local:%d", d.Deck), sh.ownAt, now))
		}
	case len(sh.clockDecks) > 0 && now.Sub(sh.clockAt) < 2*time.Second:
		for _, d := range sh.clockDecks {
			out = append(out, carriedClockDeck(d, "local", fmt.Sprintf("local:%d", d.Deck), sh.clockAt, now))
		}
	}
	var peerKeys []int
	for slot := range sh.peerDecks {
		peerKeys = append(peerKeys, slot)
	}
	slices.Sort(peerKeys)
	for _, slot := range peerKeys {
		p := sh.peerDecks[slot]
		if p.Stale || now.Sub(p.at) >= 2*time.Second {
			continue
		}
		srcID := p.ip
		if p.Link != "" {
			srcID = p.Link
		}
		d := p.deckState
		d.At = p.at.UnixMilli()
		out = append(out, carriedClockDeck(d, "peer", fmt.Sprintf("peer:%s:%d", srcID, d.Deck), p.at, now))
	}
	sh.mu.Unlock()

	for _, dev := range devices {
		if dev.Number < 1 || dev.Number > 4 || strings.HasPrefix(dev.Name, "EngineX") || now.Sub(dev.LastSeen) >= 2*time.Second {
			continue
		}
		tempo := dev.EffectiveBPM
		if tempo <= 0 && dev.BPM > 0 {
			tempo = dev.BPM * (1 + dev.Pitch/100)
		}
		if tempo <= 0 {
			continue
		}
		beat := dev.BeatPhase
		if dev.BeatInBar >= 1 && dev.BeatInBar <= 4 {
			beat = float64(dev.BeatInBar-1) + dev.BeatPhase
		}
		cue := dev.PlayState1 == cdjCuePlay
		playing := dev.Playing || cue
		if playing {
			beat = wrapBarBeat(beat + now.Sub(dev.LastSeen).Seconds()*tempo/60.0)
		} else {
			beat = wrapBarBeat(beat)
		}
		out = append(out, clockDeck{
			Deck:       int(dev.Number),
			Source:     "prolink",
			SourceID:   fmt.Sprintf("prolink:%d", dev.Number),
			Master:     dev.Master,
			Lead:       dev.Master,
			Playing:    playing,
			Cue:        cue,
			BPM:        dev.BPM,
			CurrentBPM: tempo,
			Pitch:      dev.Pitch,
			Beat:       beat,
			PositionMS: float64(dev.PlayPositionMS),
			At:         nowMS,
		})
	}
	respond(w, map[string]any{"clockApi": true, "now": nowMS, "decks": out})
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

// peerClock is the smallest (arrival - Sent) a peer's updates have shown, ms:
// its clock's offset from ours plus the quickest trip.
type peerClock struct {
	off float64
	at  time.Time
}

// peerLate is how many ms longer than the peer's quickest update this one took
// to get here (the network, or either suite waiting for the CPU): a playing
// deck has moved on by that much, and anchoring it at arrival showed the wait
// as a slip of the picture. The quickest trip may rise 1 ms/s, which follows
// two clocks drifting apart or one being slewed; over 300 ms late is a stepped
// clock and starts over. Called with sh.mu held.
func (sh *shadowState) peerLate(id string, sent int64, now time.Time) float64 {
	off := float64(now.UnixMilli() - sent)
	c, ok := sh.peerClock[id]
	c.off += now.Sub(c.at).Seconds()
	if !ok || off < c.off || off > c.off+300 {
		c.off = off
	}
	c.at = now
	sh.peerClock[id] = c
	return off - c.off
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
		delete(sh.peerClock, id)
	}
	// A clock stepped back by more than 10 s (NTP) restarts the order.
	if last := sh.peerSeq[id]; seq != 0 && seq <= last && seq > last-int64(10*time.Second) {
		sh.dbg.event("peer-stale", map[string]any{"ip": ip, "seq": seq, "last": last})
		return
	}
	late := 0.0
	if len(decks) > 0 && decks[0].Sent != 0 {
		late = sh.peerLate(id, decks[0].Sent, now)
	}
	sh.dbg.event("peer", map[string]any{"ip": ip, "seq": seq, "late": late, "decks": slimDecks(decks)})
	sh.peerSeq[id] = seq
	changed := false
	for slot, v := range sh.peerDecks {
		if (v.ip == ip || v.Link != "" && v.Link == id) && !slices.ContainsFunc(decks, func(d deckState) bool { return d.Deck == v.Deck }) {
			delete(sh.peerDecks, slot) // the peer ejected it, or restarted empty
			changed = true
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
		if cur, ok := sh.peerDecks[slot]; ok && !(cur.Deck == d.Deck && (cur.ip == ip || cur.Link != "" && cur.Link == id)) {
			slot = 0
			for s := 1; s <= 4; s++ {
				if c, taken := sh.peerDecks[s]; !taken || c.Deck == d.Deck && (c.ip == ip || c.Link != "" && c.Link == id) {
					slot = s
					break
				}
			}
		}
		if slot == 0 {
			continue
		} // all slots belong to other tracks; never overwrite a sibling
		if d.Playing && !d.Stale {
			d.PositionMS += late * (1 + d.Pitch/100)
		}
		d.Sent, d.Rate = 0, 0
		p := peerDeck{deckState: d, ip: ip, at: now, anchorPos: d.PositionMS, anchorAt: now}
		old, ok := sh.peerDecks[slot]
		if ok = ok && (old.ip == ip || old.Link != "" && old.Link == d.Link); ok {
			p.skew = old.skew
		}
		if ok && samePeerPicture(old, d, now) {
			p.anchorPos, p.anchorAt, p.refPos, p.refAt = old.anchorPos, old.anchorAt, old.refPos, old.refAt
			// The line's own slope, once it is long enough to tell: anchored anew
			// with the new speed, which reaches the QML as a few ms to steer over.
			if d.Playing && late <= shadowSkewLateMS {
				if p.refAt.IsZero() {
					if now.Sub(p.anchorAt) >= shadowSkewSettle {
						p.refPos, p.refAt = d.PositionMS, now
					}
				} else if span := now.Sub(p.refAt); span >= shadowSkewSpan {
					run := float64(span.Microseconds()) / 1000 * (1 + d.Pitch/100)
					skew := (d.PositionMS-p.refPos)/run - 1
					if math.Abs(skew) <= shadowSkewMax && math.Abs(skew-p.skew)*run >= shadowSkewOffMS {
						p.skew, p.anchorPos, p.anchorAt = skew, d.PositionMS, now
						changed = true
					}
				}
			}
		} else {
			changed = true
		}
		sh.peerDecks[slot] = p
	}
	// Heartbeats and steady playback refresh the stored decks (their liveness and
	// extrapolation) without a new version: the display QML then skips the reply
	// body (X-Shadow-State unchanged) instead of parsing it on its GUI thread.
	if changed {
		sh.bump()
	}
}

// bump marks new state and wakes the long polls. Called with sh.mu held.
func (sh *shadowState) bump() {
	sh.ver++
	close(sh.notify)
	sh.notify = make(chan struct{})
}

// shadowQuietPoll bounds a long poll whose answer cannot change by itself.
// Each poll is a QML XMLHttpRequest and its strings on V4's heap: 120 ms
// holds were ~8 requests/s, the largest steady allocation of the display.
var shadowQuietPoll = time.Second

// pollWait is how long a long poll may hold an unchanged answer: a quiet poll
// unless a CDJ deck's positions refresh, cut short when a peer deck would turn
// stale (compose derives that from time, so no bump announces it).
func (sh *shadowState) pollWait(legacy bool, now time.Time) time.Duration {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if legacy || sh.metaOnlyPoll {
		return 120 * time.Millisecond
	}
	d := shadowQuietPoll
	for _, p := range sh.peerDecks {
		if left := p.at.Add(2 * time.Second).Sub(now); !p.Stale && left > 0 && left < d {
			d = left
		}
	}
	return d
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
	title = strings.TrimRight(title, ". ")
	if strings.EqualFold(filepath.Ext(title), ext) {
		return title // a tagless file is titled by its name: no "x.mp3.mp3"
	}
	return title + ext
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
		FROM Track t JOIN PerformanceData p ON p.trackId = t.id WHERE t.path = ? AND length(p.trackData)>4 AND length(p.beatData)>4 AND length(p.overviewWaveFormData)>4`, rel).
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
func (sh *shadowState) observe(d prolink.Device) { sh.observeAt(d, time.Now()) }

func (sh *shadowState) observeAt(d prolink.Device, now time.Time) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	c := sh.cdj[d.Number]
	edge := false
	switch {
	case d.PacketType == 0x0b && d.PrecisePosition:
		ms := float64(d.PlayPositionMS)
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
		// The time base is when a packet arrives, and a packet is only ever late
		// (the network, this process waiting for the CPU): anchored at each arrival,
		// the shadow deck steered after that wait. So the line runs at the pitch from
		// the quickest packet; eight late ones in a row (240 ms: the deck was slowed
		// by hand) move it back by the least of them.
		rate := 1 + c.exact(d.Pitch)/100
		expect := c.line + float64(now.Sub(c.at).Microseconds())/1000*c.rate
		late := expect - ms
		moving := !jump && !c.at.IsZero() && ms > c.ms
		if !moving || !c.moving || math.Abs(rate-c.rate) > 2e-6 || late <= 0 {
			c.line, c.n = ms, 0
		} else if c.line, c.n = expect, c.n+1; c.n == 1 || late < c.late {
			c.late = late
		}
		if c.n >= 8 {
			c.line, c.n = c.line-c.late, 0
		}
		c.rate, c.moving = rate, moving
		c.ms, c.at = ms, now
		sh.precise[d.Number] = precisePos{c.line, now}
	case d.PacketType == 0x0a:
		playing := d.Playing || d.PlayState1 == cdjCuePlay
		edge = playing != c.playing || d.TrackID != c.track
		c.playing, c.track = playing, d.TrackID
		c.pitch, c.fine = d.Pitch, true
	}
	sh.cdj[d.Number] = c
	if edge {
		sh.bump()
	}
}

func parseLineChannels(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n >= 1 && n <= 4 {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// shadowGet is what the QML applies to the decks this player does not play.
// With ?ver=N it is a long poll: it answers at once when the peer state moved
// past N, else as soon as it does, at the latest after pollWait (120 ms while
// CDJ decks refresh their positions, else up to a second).
func (s *Server) shadowGet(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	v := s.loadUI()
	s.uiMu.Unlock()
	role := s.activeRole(v)
	debug := s.shadow.dbg.set(v.DebugTelemetry || fileExists(shadowDebugFlag))
	query := r.URL.Query()
	seen, seenErr := strconv.ParseUint(query.Get("ver"), 10, 64)
	out := []deckState{}
	var ver uint64
	if role != "" {
		s.shadow.mu.Lock()
		s.shadow.lines = parseLineChannels(query.Get("line"))
		s.shadow.mu.Unlock()
		s.shadowWake()
		if seenErr == nil {
			s.shadow.wait(r.Context(), seen, s.shadow.pollWait(v.ShadowMode != "cdj", time.Now()))
			// The wait can span a Shadow Deck Mode switch (#201): the answer, its
			// headers and displayOnly all follow the settings as they are now.
			s.uiMu.Lock()
			v = s.loadUI()
			s.uiMu.Unlock()
		}
		s.shadow.mu.Lock()
		ver = s.shadow.ver
		lines := slices.Clone(s.shadow.lines)
		s.shadow.mu.Unlock()
		out = s.shadowComposeMode(v.ShadowMode, role, time.Now(), lines)
	}
	metaOnly := slices.ContainsFunc(out, func(d deckState) bool { return d.MetadataOnly || strings.HasPrefix(d.Key, "cdj:") })
	s.shadow.mu.Lock()
	s.shadow.metaOnlyPoll = metaOnly
	s.shadow.mu.Unlock()
	// The peer version alone says nothing about copy readiness, retries, or
	// extrapolation. Skip only empty Engine slots after the client applied one.
	if role != "" && v.ShadowMode != "cdj" && !debug && !metaOnly && !slices.ContainsFunc(out, func(d deckState) bool {
		return d.Link == "" || d.Path != "" || d.ID != "" || d.Loading || d.Playing ||
			d.ShadowMaster != 0 || d.Master != nil && *d.Master || d.LineIn
	}) {
		if seenErr == nil && seen > 0 && ver == seen && query.Get("idle") == "1" {
			w.Header().Set("X-Shadow-Unchanged", "1")
			respond(w, struct {
				Unchanged bool `json:"unchanged"`
			}{true})
			return
		}
		w.Header().Set("X-Shadow-Idle", "1")
	}
	stale := slices.ContainsFunc(out, func(d deckState) bool { return d.Stale })
	// The version covers peer updates; what compose derives from time and the
	// display cache (stale, loading, the placed slot and key) goes in a digest.
	h := fnv.New64a()
	for _, d := range out {
		fmt.Fprintf(h, "%d|%s|%t|%t|%t|%t|%.2f;", d.Deck, d.Key, d.Loading, d.Stale, d.MetadataOnly, d.DisplayDetail, d.Progress)
	}
	// CDJ continuous positions do not advance the EngineX peer version. Those
	// responses, like responses with an Engine-loaded deck, must always reach QML.
	if v.ShadowMode == "cdj" && !metaOnly {
		w.Header().Set("X-Shadow-State", fmt.Sprintf("%d:%t:%s:%t:%t:%x", ver, true, role, stale, debug, h.Sum64()))

	}
	// displayOnly is what a deck without its own flag follows (an empty slot too).
	respond(w, map[string]any{"role": role, "decks": out, "ver": ver, "debug": debug, "displayOnly": v.ShadowMode == "cdj"})
}

// shadowCompose picks the path per shadow slot. In auto mode every source goes
// through Engine, drawn by the stock components, when this player can have the
// file: an EngineX peer's track, and a CDJ's from a USB stick or SD card on the
// link. A CDJ whose file it cannot have (cdjFallback) goes to the display
// renderer (displayOnly on its deck) and says why. The two forced modes send
// every source down one path.
func (s *Server) shadowCompose(role string, now time.Time, lines ...[]int) []deckState {
	s.uiMu.Lock()
	mode := s.loadUI().ShadowMode
	s.uiMu.Unlock()
	return s.shadowComposeMode(mode, role, now, lines...)
}

// shadowComposeMode is shadowCompose for a mode the caller already read, so one
// answer never mixes two modes.
func (s *Server) shadowComposeMode(mode, role string, now time.Time, lines ...[]int) []deckState {
	switch mode {
	case "cdj":
		return s.shadowComposeDisplay(role, now, lines...)
	case "engine":
		return s.shadowComposeLegacy(role, now, lines...)
	}
	out, free, lineIn, display := s.shadowLegacy(role, now, false, lines...)
	if len(display) > 0 {
		out = append(out, s.shadowDisplayCDJs(free, lineIn, now, display)...)
		slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	}
	return out
}

func (s *Server) shadowComposeLegacy(role string, now time.Time, lines ...[]int) []deckState {
	out, _, _, _ := s.shadowLegacy(role, now, true, lines...)
	return out
}

// CDJ status packet, the slot a track was loaded from (Deep Symmetry's
// analysis): 1 CD, 2 SD, 3 USB, 4 rekordbox on a computer, 7 the second USB of
// an all-in-one, the others seen are streaming services.
const (
	cdjSlotCD     = 1
	cdjSlotLink   = 4
	cdjSlotSecond = 7
)

// cdjFallback tells why auto mode cannot put a CDJ's track through Engine, ""
// when it can. Only a file a player serves over NFS (SD, USB) can be copied:
// a streamed track, one rekordbox sends from a computer and a CD have none.
// failed is the kind of the copy's last failure (shadowCopy.err) while no
// attempt has succeeded: the copy is still tried again, and Engine takes the
// deck once one does.
func cdjFallback(dev prolink.Device, host net.IP, failed string) string {
	_, err := prolink.SlotExport(dev.SourceSlot)
	switch {
	case dev.SourceSlot == cdjSlotLink:
		return "link"
	case dev.SourceSlot == cdjSlotCD || dev.SourceSlot == cdjSlotSecond:
		return "media"
	case err != nil:
		return "streaming"
	case host == nil || failed != "" && failed != "timeout":
		return "network"
	}
	return failed
}

// shadowLegacy is the Engine path. With all (Engine mode) every CDJ takes it.
// Without (auto mode), display names the CDJ decks left to the display renderer
// and why (cdjFallback; no reason on a console's Line In channel, lineIn, where
// only that renderer draws), out of the decks free for CDJs.
func (s *Server) shadowLegacy(role string, now time.Time, all bool, lines ...[]int) (out []deckState, free []int, lineIn bool, display map[int]string) {
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
	// free: shadow decks the other EngineX player does not fill
	var shadowDecks []int
	usedPeer := map[int]bool{}
	placed := map[int]peerDeck{}
	pfx := imagePrefix(deviceCompatible())
	isConsole := pfx != "" && !playerBoards[pfx]
	var consoleLines []int
	if isConsole && len(lines) > 0 && len(lines[0]) > 0 {
		consoleLines = slices.Clone(lines[0])
		slices.Sort(consoleLines)
	}
	for deck := 1; deck <= 4; deck++ {
		if strings.Contains(role, strconv.Itoa(deck)) {
			continue
		}
		shadowDecks = append(shadowDecks, deck)
		if p, ok := sh.peerDecks[deck]; ok && !p.Stale && now.Sub(p.at) < 2*time.Second {
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
			if p, ok := sh.peerDecks[src]; ok && !p.Stale && now.Sub(p.at) < 2*time.Second {
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
		d.LineIn, d.DisplayOnly = false, false
		if d.Lead == nil {
			d.Lead = d.Master // only older senders omit the deck's own LEAD state
		}
		// Two standalone players share master natively over Engine Link; an
		// all-in-one console linked to an SC5000/SC6000 (#160) does not, so keep
		// the peer's master flag when this unit is not a standalone player.
		if pfx := imagePrefix(deviceCompatible()); pfx == "" || playerBoards[pfx] {
			d.Master = new(false)
		}
		if d.Playing {
			d.PositionMS, d.Rate = p.carried(now.Sub(p.at))
		}
		d.Progress, d.Error = 0, "" // ours to say, not the peer's
		// A file of this player's own for a track the peer loaded from this player's
		// drive: both then read one library. Not for a track on the peer's own drive
		// (the net path names the peer itself): the same path here is another
		// library's track with its own analysis, whose waveform peaks drew the low
		// band at half the source's height. That one is copied with the source's.
		d.Path = ""
		if host, _, _ := strings.Cut(strings.TrimPrefix(d.Net, "net://"), "/"); d.Link == "" || !strings.EqualFold(host, strings.Trim(d.Link, "{}")) {
			d.Path = resolveNetPathFile(d.Net, sh.dataRoot, sh.mediaRoot, false)
		}
		// A file of this player's own is named by its path. Not by the peer's key
		// then: that appears once the peer's Engine has cached the file, mid-track.
		d.ID = d.Net
		if d.Path == "" && d.Key != "" {
			// Not by address: a player on eth0 and wlan0 is heard from both, and the
			// track was copied and loaded (and analysed) twice.
			key := "peer:" + d.Key
			d.ID = d.Key
			wanted[key] = now
			d.Path = sh.files[key]
			if d.Path == "" {
				s.shadowFetchPeer(key, p)
				d.Progress, d.Error = sh.copyShown(key)
			}
		}
		// Audio still on its way: the QML shows the deck as loading. No track on
		// the peer at all: nothing to hold, so the QML unloads the shadow deck.
		d.Loading = d.Path == "" && d.Key != ""
		if p.Net == "" && p.Key == "" {
			d.Path, d.Loading = "", false
		}
		// The key of the source's display data: the QML covers the deck with the
		// source's cover and key until Engine holds the track.
		d.Key, d.Ext, d.Net = displayTrackKey(p), "", ""
		d.At = now.UnixMilli()
		out = append(out, d)
	}
	var lineFree []int
	if len(consoleLines) > 0 {
		free = nil
		for _, deck := range consoleLines {
			if _, occupied := placed[deck]; !occupied {
				free = append(free, deck)
			}
		}
		lineFree = free
	}
	lineIn = len(consoleLines) > 0
	display = map[int]string{}
	for deck, dev := range cdjDecks(devices, free, now, lineFree) {
		if lineIn && !all {
			display[deck] = ""
			continue
		}
		cue := dev.PlayState1 == cdjCuePlay
		d := deckState{Deck: deck, Playing: dev.Playing || cue, Cue: cue, Jumps: sh.cdj[dev.Number].jumps, PositionMS: float64(dev.PlayPositionMS), Pitch: sh.cdj[dev.Number].exact(dev.Pitch), Master: new(dev.Master), Lead: new(dev.Master), Sync: dev.Sync, BPM: dev.BPM, DurationMS: float64(dev.DurationMS), Title: dev.Title, Artist: dev.Artist}
		if len(consoleLines) > 0 {
			d.LineIn = true
		}
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
			d.ID = fmt.Sprintf("cdj:own:%d", dev.TrackID)
		} else if dev.TrackID != 0 {
			host := sourceHost(export, devices, dev.SourcePlayer)
			why := ""
			if !all {
				why = cdjFallback(dev, host, "")
			}
			if host != nil && why == "" {
				key := fmt.Sprintf("cdj:%s:%d:%d", host, dev.SourceSlot, dev.TrackID)
				wanted[key] = now
				d.Path, d.ID = sh.files[key], key
				if d.Path == "" {
					s.shadowFetchCDJ(key, host, dev.SourceSlot, dev.TrackID)
					d.Loading = true
					d.Progress, d.Error = sh.copyShown(key)
					if c := sh.copies[key]; !all && c != nil && c.fails > 0 {
						why = cdjFallback(dev, host, c.err)
					}
				}
			}
			if why != "" {
				display[deck] = why
				continue
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b deckState) int { return a.Deck - b.Deck })
	return out, free, lineIn, display
}

// cdjDecks puts the CDJs on the free shadow decks: on their own player number
// when it is free, the rest on what is left, Deck 2 first. A lone CDJ next to
// a 1+3 player then shows beside Deck 1 also in the 2-waveform layout.
// On an all-in-one console with Line channels, CDJs are mapped to the Line
// channels in player-number order.
func cdjDecks(devices []prolink.Device, free []int, now time.Time, lines ...[]int) map[int]prolink.Device {
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
	if len(lines) > 0 && len(lines[0]) > 0 {
		lineSlots := slices.Clone(lines[0])
		slices.Sort(lineSlots)
		for i := 0; i < len(cdjs) && i < len(lineSlots); i++ {
			out[lineSlots[i]] = cdjs[i]
		}
		return out
	}
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
	var host string
	for _, d := range devices {
		if d.Number == n && d.Address != "" && (host == "" || d.Address < host) {
			host = d.Address
		}
	}
	if host != "" {
		return net.ParseIP(host)
	}
	return nil
}

// shadowFetch* run with sh.mu held; the copy itself runs unlocked.
func (s *Server) shadowFetchCDJ(key string, host net.IP, slot byte, id uint32) {
	s.shadow.fetchCDJCalls.Add(1)
	s.shadowFetch(key, func(ctx context.Context, cp *shadowCopy) (string, error) {
		c := &prolink.NFSClient{IP: host, Portmap: s.shadow.cdjPortmap}
		local, err := prolink.FetchTrack(ctx, c, slot, id, s.shadowDir(), func(done, total int64) {
			cp.total.Store(total)
			cp.done.Store(done)
		})
		if err != nil {
			return "", err
		}
		var perf []byte
		if t, a, err := prolink.FetchAnalysis(ctx, c, slot, id); err == nil {
			if info, err := compat.AudioHeader(local); err == nil && info.SampleRate > 0 {
				t.SampleRate = info.SampleRate
				if info.Samples > 0 {
					t.SampleCount = info.Samples
				}
				t.StartOffsetSamples = info.StartOffsetSamples
			}
			if p, err := enginedb.EncodePerformance(t, a); err == nil && p.Complete {
				perf, _ = perfFromBlobs(t.BPM, p.TrackData, p.Beats, p.Overview)
			}
		}
		savePerf(local, perf)
		return local, nil
	})
}

func copyShadowAudio(dst io.Writer, resp *http.Response, limit int64) error {
	if resp.ContentLength > limit {
		return fmt.Errorf("peer audio exceeds %d byte limit", limit)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("copy peer audio: %w", err)
	}
	if n > limit {
		return fmt.Errorf("peer audio exceeds %d byte limit", limit)
	}
	return nil
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
	s.shadowFetch(key, func(ctx context.Context, cp *shadowCopy) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("peer audio: %s: %w", resp.Status, errShadowUnavailable)
		}
		cp.total.Store(max(0, resp.ContentLength)) // -1: the peer did not say
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(local), ".fetch-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		err = copyShadowAudio(io.MultiWriter(f, countWriter{&cp.done}), resp, 1<<30)
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

// shadowCopy is one track's copy on its way to this player: how far it is, and
// after a failure why it failed and when it is tried again.
type shadowCopy struct {
	done, total atomic.Int64 // bytes; total 0: the source did not say
	fails       int
	retry       time.Time // no new attempt before this
	err         string    // the last failure, by kind: never the error's text, which can hold addresses
}

var errShadowUnavailable = errors.New("the source does not have the track")

type countWriter struct{ n *atomic.Int64 }

func (w countWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// copyShown is what the QML shows of a copy (sh.mu held): how far it is, 0 while
// the length is not known, or the failure it is retried after. The failure stays
// until bytes of the next attempt arrive, so the line does not blink.
func (sh *shadowState) copyShown(key string) (float64, string) {
	c := sh.copies[key]
	if c == nil {
		return 0, ""
	}
	done, total := c.done.Load(), c.total.Load()
	if done <= 0 {
		return 0, c.err
	}
	if total <= 0 {
		return 0, ""
	}
	return min(1, max(0.01, math.Round(float64(done)/float64(total)*100)/100)), ""
}

func (s *Server) shadowFetch(key string, fetch func(context.Context, *shadowCopy) (string, error)) {
	s.shadow.fetchCalls.Add(1)
	sh := s.shadow
	c := sh.copies[key]
	// A copy that failed is not asked for again on every compose (8 a second).
	if sh.busy[key] || (c != nil && time.Now().Before(c.retry)) {
		return
	}
	if c == nil {
		c = &shadowCopy{}
		sh.copies[key] = c
	}
	c.done.Store(0)
	c.total.Store(0)
	sh.busy[key] = true
	s.shadowCleanCopies(key)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), sh.copyTimeout)
		defer cancel()
		path, err := fetch(ctx, c)
		sh.mu.Lock()
		defer sh.mu.Unlock()
		delete(sh.busy, key)
		for k := range sh.copies {
			if k != key && !sh.busy[k] && time.Since(sh.used[k]) > 2*time.Minute {
				delete(sh.copies, k)
			}
		}
		if err != nil {
			var timeout net.Error
			switch {
			case errors.Is(err, errShadowUnavailable):
				c.err = "unavailable"
			case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()): // ctx: a read cut off by the deadline fails with another error
				c.err = "timeout"
			case c.done.Load() > 0:
				c.err = "interrupted"
			default:
				c.err = "failed"
			}
			c.fails++
			c.retry = time.Now().Add(min(sh.copyRetry<<min(c.fails-1, 4), 15*sh.copyRetry))
			c.done.Store(0)
			fmt.Fprintf(os.Stderr, "stream-sync: shadow deck copy %s: %v\n", key, err)
			return
		}
		delete(sh.copies, key)
		sh.files[key] = path
		s.shadowCleanCopies(key)
	}()
}

// Called with sh.mu held, before a download too: ENOSPC must not prevent cleanup.
func (s *Server) shadowCleanCopies(keep string) {
	sh := s.shadow
	// Keep copies wanted in the last 2 minutes and those still being fetched.
	for k, f := range sh.files {
		if k != keep && !sh.busy[k] && time.Since(sh.used[k]) > 2*time.Minute {
			os.Remove(f)
			os.Remove(f + ".perf")
			for dir := filepath.Dir(f); dir != s.shadowDir() && strings.HasPrefix(dir, s.shadowDir()); dir = filepath.Dir(dir) {
				os.Remove(dir) // the copy's own directories, once empty
			}
			delete(sh.files, k)
			delete(sh.used, k)
		}
	}
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
	// Retain older caches for an explicit rollback; starting display discovery
	// must not delete files from an earlier mode.
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
