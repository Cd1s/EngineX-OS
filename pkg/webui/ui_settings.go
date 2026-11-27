package webui

// Deck UI settings read by the QML in the UI packs (tools/ui-mod) and by
// engine_ui_hook.c, which only understands this key=value file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

// Build metadata injected at link time via -ldflags:
//
//	-X github.com/Cd1s/EngineX-OS/pkg/webui.BuildTag=...
//	-X github.com/Cd1s/EngineX-OS/pkg/webui.BuildTrack=...
//	-X github.com/Cd1s/EngineX-OS/pkg/webui.ReleaseBuild=1
var (
	BuildTag     string
	BuildTrack   = "dev"
	ReleaseBuild string
)

func currentBuildTag() string {
	if ReleaseBuild == "1" {
		return ""
	}
	if tag := strings.TrimSpace(BuildTag); tag != "" {
		return tag
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			rev = s.Value
			if len(rev) > 7 {
				rev = rev[:7]
			}
			break
		}
	}
	if rev == "" {
		return ""
	}
	track := strings.TrimSpace(os.Getenv("ENGINEX_BUILD_TRACK"))
	if track == "" {
		track = strings.TrimSpace(BuildTrack)
	}
	if track == "" {
		track = "dev"
	}
	return track + " " + rev
}

// uiCapabilities describes hardware-gated EngineX features derived from the
// device-tree board prefix (never Engine's Config.device.isPlayer).
type uiCapabilities struct {
	Mixer         bool `json:"mixer"`
	FourDeck      bool `json:"fourDeck"`
	AutoLoopKnob  bool `json:"autoLoopKnob"`
	BeatJumpPads  bool `json:"beatJumpPads"`
	TrackSkipKeys bool `json:"trackSkipKeys"`
	StemsEq       bool `json:"stemsEq"`
	LineInAux     bool `json:"lineInAux"`
	Gen2          bool `json:"gen2"`
	RemoteStems   bool `json:"remoteStems"`
	JogWheel      bool `json:"jogWheel"`
	SDHighSpeed   bool `json:"sdHighSpeed"`
}

func boardCapabilities(board string) uiCapabilities {
	if board == "" {
		return uiCapabilities{
			Mixer:         true,
			FourDeck:      true,
			AutoLoopKnob:  true,
			TrackSkipKeys: true,
			StemsEq:       true,
			RemoteStems:   true,
			JogWheel:      true,
			SDHighSpeed:   true,
		}
	}
	isPlayer := playerBoards[board]
	isMixstream := mixstreamBoards[board]
	isAZ04 := az04Boards[board]
	return uiCapabilities{
		Mixer:         !isPlayer,
		FourDeck:      fourDeckBoards[board],
		AutoLoopKnob:  !isMixstream,
		BeatJumpPads:  isMixstream,
		TrackSkipKeys: !noTrackSkipKeyBoards[board],
		StemsEq:       !isPlayer,
		LineInAux:     board == "SCLIVE4",
		Gen2:          dataBundleBoards[board],
		RemoteStems:   !isAZ04,
		JogWheel:      !isPlayer && !motorizedBoards[board],
		SDHighSpeed:   !isAZ04,
	}
}

const defaultUIDir = "/data/enginex/ui"

var (
	uiLanguages      = []string{"en", "zh-Hans", "zh-Hant", "es"}
	uiWaveforms      = []string{"denon", "rgb", "3band"}
	uiServices       = []string{"tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}
	uiQualities      = []string{qualityFLAC, quality320k}
	uiSpotifySources = []string{"auto", "deezer", "netease", "beatport", "applemusic", "soundcloud"}
	uiNeteaseModes   = []string{neteaseOfficial, neteaseDeemix}
	// TIDAL logs in through Engine's own flow against TIDAL's servers.
	loginServices = []string{"netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}

	uiStreamSortRoles  = []string{"default", "bpm", "key", "rating", "song", "artist", "length", "dateImported"}
	uiStreamSortOrders = []string{"asc", "desc"}
	uiSyncGuardPcts    = []int{0, 10, 15, 20}
	// UI Scale presets: the QML mod scales the loaded UI live (EmbeddedMain uiLoader).
	uiUIScales            = []int{110, 105, 100, 95, 90, 85, 80, 75}
	uiPioneerFxModes      = []string{"off", "spiral", "pitch_echo", "slip_roll", "dub_echo", "space"}
	uiShadowModes         = []string{"auto", "engine", "cdj"}
	uiRbLibraryViews      = []string{"folders", "merged", "device", "onelibrary"}
	uiFxTimeModes         = []string{"beats", "ms"}
	uiMixstreamFxDefaults = []string{"Echo", "Flanger", "Delay", "Phaser"}
	uiMixstreamFxChoices  = []string{"Echo", "Flanger", "Delay", "Phaser"}
)

func parseMixstreamFx(val string) ([]string, bool) {
	parts := strings.Split(val, ",")
	if len(parts) != len(uiMixstreamFxDefaults) {
		return nil, false
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if !slices.Contains(uiMixstreamFxChoices, p) {
			return nil, false
		}
		out[i] = p
	}
	return out, true
}

const (
	qualityFLAC = "flac"
	quality320k = "320k"

	// NetEase tracks download from NetEase itself, which works with nothing but a
	// NetEase login. Deemix mode tries the deck's Deemix first to spare the
	// account's monthly download quota, and falls back to NetEase.
	neteaseOfficial = "official"
	neteaseDeemix   = "deemix"
)

type usbPrepareStatus struct {
	State string `json:"state,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
	Error string `json:"error,omitempty"`
}

type uiSettings struct {
	SpotifySource  string            `json:"spotifySource"`
	EngineLighting bool              `json:"engineLighting"` // Stock lighting switch; opt in, saved independently of Engine shutdown.
	USBPreparing   bool              `json:"usbPreparing,omitempty"`
	USBPrepare     *usbPrepareStatus `json:"usbPrepare,omitempty"`
	Version        string            `json:"version,omitempty"`
	Quality        string            `json:"quality"`
	NeteaseMode    string            `json:"neteaseMode"`
	Playhead       float64           `json:"playhead"`
	// AutoAnalyze retains the user's preference (#55). The UI pack currently
	// fails closed: no automatic analysis until a bounded native adapter is verified.
	AutoAnalyze bool `json:"autoAnalyze"`
	// MasterLimiter turns on the soft clipper / peak limiter engine_ui_hook.c
	// applies to Engine's audio output (#58). Off by default.
	MasterLimiter bool `json:"masterLimiter"`
	// AutoGain raises quiet material toward 0 dBFS (at most +9 dB) ahead of the
	// limiter, which then always runs (#64). Off by default.
	AutoGain bool `json:"autoGain"`
	// HifiOutput compensates the fixed 48->96 kHz Hermite output roll-off on Gen-1
	// standalone players (JP07/JP08/JP13/JP14) ahead of the soft limiter (#172). Off by default.
	HifiOutput bool `json:"hifiOutput"`
	// OmniSource adds every mounted drive's playlists and tracks, and the other
	// services' playlists, to the streaming browser (#62). Off by default.
	OmniSource bool `json:"omniSource"`
	// RemoteLoad lets a LAN client load a track into an idle deck from an
	// external drive. Deck-only: loopback POST the legacy UI endpoint sets it, /api/ui
	// refuses it. Off by default.
	RemoteLoad bool `json:"remoteLoad"`
	// ProlinkShare enables Pro DJ Link: announces this deck, lets rb-bridge read
	// Pioneer/rekordbox libraries on the LAN, and exports this deck's drives (#59, #63).
	// Off by default.
	ProlinkShare bool `json:"prolinkShare"`
	// RekordboxShare is the folder of the computer with rekordbox that a mounted network
	// share shows (/Users/dj/Music), so its tracks are found inside the share. Empty:
	// the share shows the computer's whole disk.
	RekordboxShare   string   `json:"rekordboxShare"`
	RekordboxName    string   `json:"rekordboxName,omitempty"` // status only: the computer on the link, for the source's name
	Language         string   `json:"language"`
	Waveform         string   `json:"waveform"`
	Sources          []string `json:"sources"`
	Scope            string   `json:"scope"`
	GitHubRepo       string   `json:"githubRepo"`
	SafeMode         bool     `json:"safeMode"`
	LoggedIn         []string `json:"loggedIn"`
	TidalLoggedIn    bool     `json:"tidalLoggedIn"`
	TidalSynthetic   bool     `json:"tidalSynthetic"`
	FourDeckWaveform bool     `json:"fourDeckWaveform"`
	FourDeckRole     string   `json:"fourDeckRole"`
	ShadowMode       string   `json:"shadowMode"` // which path draws a shadow deck: auto = Engine loads every source whose file this player can have and stock components draw it (EngineX peer; CDJ playing from USB or SD), a CDJ without such a file goes to the display renderer, engine = Engine loads every source, cdj = display renderer for every source
	ProlinkWaveform  bool     `json:"prolinkWaveform"`
	BroadcastEnabled bool     `json:"broadcastEnabled"`
	UpfaderEcho      bool     `json:"upfaderEcho"`
	// CrossfaderReverse remembers the software mixer switch across Engine restarts.
	CrossfaderReverse bool `json:"crossfaderReverse"`
	// PairedView (#201): a standalone player in the 4-deck layout shows Engine's own
	// 2-deck view instead, this unit's active layer and the other player's active
	// deck (its layer followed). Needs FourDeckWaveform: setting it turns that on,
	// turning that off clears it.
	PairedView bool `json:"pairedView"`
	// ConsoleLineView (console linking, stage 1): on PRIME 4, PRIME 4+, SC LIVE 4 and PRIME 4 G2 a linked
	// player or CDJ is shown on a mixer channel while that channel's input is Line / AUX. Display only:
	// nothing is loaded, no deck is taken, FourDeckWaveform and the 4-deck role stay ignored on a console.
	// Off by default, honoured on those four boards only, never copied between units.
	ConsoleLineView bool `json:"consoleLineView"`
	// ConsoleDecks (console linking, stage 2, the row Show Console): on SC5000, SC5000M, SC6000 and SC6000M in
	// the 4-deck layout the decks of a linked console appear in the lanes this player has free, display only.
	// Off by default, needs FourDeckWaveform, honoured on those four boards only, never copied between units.
	ConsoleDecks bool `json:"consoleDecks"`
	// ShareDecks (the row Share Decks, #329): on SC5000, SC5000M, SC6000 and SC6000M in the Stock layout the
	// decks of this player are shown by a linked console's Line In view; the player keeps its stock screen and
	// controls (no 4-deck layout). It samples and sends only while such a console asks. Off by default, honoured
	// on those four boards only, never copied between units; without effect in the 4-deck layout, which shares anyway.
	ShareDecks bool `json:"shareDecks"`
	// DebugTelemetry logs every shadow deck update and decision (shadow_debug.go).
	// Development only: off by default, and nothing in the image turns it on.
	DebugTelemetry bool `json:"debugTelemetry"`
	VerboseLog     bool `json:"verboseLog"`

	StreamSortRole    string `json:"streamSortRole,omitempty"`
	StreamSortOrder   string `json:"streamSortOrder,omitempty"`
	BrowserDualColumn bool   `json:"browserDualColumn"`
	TracklistWaveform bool   `json:"tracklistWaveform"`
	RbLibraryView     string `json:"rbLibraryView"`
	CueCountdown      bool   `json:"cueCountdown"`
	BpmDecimals       int    `json:"bpmDecimals"`
	SyncGuardPct      int    `json:"syncGuardPct"`
	BtOut             int    `json:"btOut"` // output pair Bluetooth audio out sends (bt_out=, engine_ui_hook.c; 0 = Master / Layer A), named per model in bluetooth.go
	SwapLoopOrder     bool   `json:"swapLoopOrder"`
	LedBrightness     int    `json:"ledBrightness"`
	// UIScale enlarges or shrinks Engine's whole interface; it takes effect at the next
	// Engine start, so the Control Center row asks before applying it.
	UIScale             int      `json:"uiScale"`
	StemsEqMode         bool     `json:"stemsEqMode"`
	BeatJumpPads        bool     `json:"beatJumpPads"`
	PitchBendBeatJump   bool     `json:"pitchBendBeatJump"`
	RestoreBpmOnUnsync  bool     `json:"restoreBpmOnUnsync"` // #289: Sync off with the tempo control at 0 % returns the deck to the track's original BPM
	MixstreamFx         []string `json:"mixstreamFx"`
	JogInvertLeft       bool     `json:"jogInvertLeft"`
	JogInvertRight      bool     `json:"jogInvertRight"`
	SdHighSpeed         bool     `json:"sdHighSpeed"`
	SdHighSpeedPending  bool     `json:"sdHighSpeedPending,omitempty"`
	PioneerFx           string   `json:"pioneerFx"`
	FXSelectorAvailable bool     `json:"fxSelectorAvailable"`
	ColorFXAvailable    bool     `json:"colorFxAvailable"`
	FxTimeMode          string   `json:"fxTimeMode"`
	FxTimeMs            int      `json:"fxTimeMs"`

	RelayURL          string `json:"relayUrl"`
	DeemixPasswordSet bool   `json:"deemixPasswordSet"`

	// Status only, never saved: the export runs (ON waits for a Rekordbox USB), and
	// the board is a standalone player without channel faders (#52).
	ProlinkExportActive bool           `json:"prolinkExportActive"`
	Player              bool           `json:"player"`
	Board               string         `json:"board,omitempty"`
	BuildTag            string         `json:"buildTag,omitempty"`
	Capabilities        uiCapabilities `json:"capabilities"`
	// The role engine-ui-guard applied at Engine's last start (auto resolved).
	FourDeckRoleActive string `json:"fourDeckRoleActive,omitempty"`
	TimeZone           string `json:"timezone,omitempty"`
	DeckClock          bool   `json:"deckClock,omitempty"`
	ConsoleHTTPS       bool   `json:"consoleHttps"`
	HTTPSPort          int    `json:"httpsPort,omitempty"`

	// WebDAV and NAS (status only): the servers of each kind, without passwords,
	// with their Sources IDs; WebDAVOn and NASOn are the two rows in Settings ->
	// Services.
	WebDAV   []serverView `json:"webdav,omitempty"`
	NAS      []serverView `json:"nas,omitempty"`
	WebDAVOn bool         `json:"webdavOn,omitempty"`
	NASOn    bool         `json:"nasOn,omitempty"`
	// WebDAVSwitch and NASSwitch are those rows once the DJ used them: "1" on, "0"
	// off. Empty: on while a server of that kind is set up (a released deck's
	// carries over; an empty source is not put on every deck).
	WebDAVSwitch string `json:"-"`
	NASSwitch    string `json:"-"`
}

// uiFourDeckRoles are the decks a player plays in the 4-deck layout (#49/#160), RCA A then B.
// auto: 13 for an odd Engine player number, 24 for an even one (engine-ui-guard), or 12 on an all-in-one console.
var uiFourDeckRoles = []string{"auto", "13", "24", "12", "34", "1", "2"}

// fourDeckRolePath is where engine-ui-guard leaves the role it applied.
var fourDeckRolePath = "/run/enginex-assignments/role"

// uiScaleEnvPath records ENGINEX_UI_SCALE for Engine's unit (the
// engine.service.d/70-ui-scale.conf drop-in). It is deliberately not QT_SCALE_FACTOR,
// which leaves a black band under EGLFS. A package var so tests can point it at a temp dir.
var (
	uiScaleMu      sync.Mutex
	uiScaleEnvPath = "/data/enginex/ui-scale.env"
)

// writeUIScaleEnv records the scale chosen in Settings -> Device, and drops the file again at 100% so the unit keeps Denon's stock size. It sits
// outside ui/, which an update replaces wholesale.
func writeUIScaleEnv(pct int) error {
	uiScaleMu.Lock()
	defer uiScaleMu.Unlock()

	if !slices.Contains(uiUIScales, pct) || pct == 100 {
		if err := os.Remove(uiScaleEnvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	dir := filepath.Dir(uiScaleEnvPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	scale := strconv.FormatFloat(float64(pct)/100, 'f', -1, 64)
	text := "ENGINEX_UI_SCALE=" + scale + "\n"
	tmp, err := os.CreateTemp(dir, ".ui-scale-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(text); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), uiScaleEnvPath)
}

func (s *Server) uiDir() string {
	if s.cfg.UIDir != "" {
		return s.cfg.UIDir
	}
	return defaultUIDir
}

// uiServicesBeforeKnown are the services a settings file without "known=" (up to
// 5.3.0.1) was written against. 5.0.4.2 saved all five by default, so without this
// a service added later (Beatport) stayed hidden on every upgraded deck.
var uiServicesBeforeKnown = []string{"tidal", "netease", "spotify", "deezer", "soundcloud"}

// Only unsigned decimal integers are accepted by the UI and DSP readers.
func uiFxTimeInteger(text string) (int64, error) {
	if len(text) > 10 {
		return 0, errors.New("invalid milliseconds")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid milliseconds")
		}
	}
	n, err := strconv.ParseUint(text, 10, 32)
	return int64(n), err
}

// loadUI must be called with uiMu held. Bad or missing values get defaults.
func (s *Server) loadUI() uiSettings {
	known, saved, shadowModeSet := uiServicesBeforeKnown, false, false
	v := uiSettings{
		Language: "en", Waveform: "denon", Sources: slices.Clone(uiServices), Scope: "deezer", Quality: qualityFLAC, NeteaseMode: neteaseOfficial, SpotifySource: "auto", GitHubRepo: defaultGitHubRepo, FourDeckWaveform: false, FourDeckRole: "auto", ShadowMode: "auto", ProlinkWaveform: true,
		TracklistWaveform: true, RbLibraryView: "folders", CueCountdown: true, BpmDecimals: 2,
		StreamSortRole: "default", StreamSortOrder: "asc", LedBrightness: 100, PioneerFx: "off", UIScale: 100,
		FxTimeMode: "beats", FxTimeMs: 500, MixstreamFx: slices.Clone(uiMixstreamFxDefaults),
		// BeatJumpPads is off by default on every board, the four players' Shift + LOOP pad mode included (they have
		// physical Beat Jump buttons): only beat_jump_pads=1 turns it on.
	}
	b, _ := os.ReadFile(filepath.Join(s.uiDir(), "settings.conf"))
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "engine_lighting":
			v.EngineLighting = val == "1" || strings.EqualFold(val, "true")
		case "lang":
			if slices.Contains(uiLanguages, val) {
				v.Language = val
			}
		case "waveform":
			if slices.Contains(uiWaveforms, val) {
				v.Waveform = val
			}
		case "github_repo":
			if githubRepoRE.MatchString(val) {
				v.GitHubRepo = val
			}
		case "scope":
			if _, _, server := serverScopeOf(val); val == "" || server || slices.Contains(uiScopes, val) {
				v.Scope = val
			}
		case "quality":
			if slices.Contains(uiQualities, val) {
				v.Quality = val
			}
		case "spotify_source":
			if slices.Contains(uiSpotifySources, val) {
				v.SpotifySource = val
			}
		case "netease_mode":
			if slices.Contains(uiNeteaseModes, val) {
				v.NeteaseMode = val
			}
		case "playhead":
			if f, err := strconv.ParseFloat(val, 64); err == nil && validPlayhead(f) {
				v.Playhead = f
			}
		case "sources":
			if list, err := parseUISources(val); err == nil {
				v.Sources, saved = list, true
			}
		case "auto_analyze":
			v.AutoAnalyze = val == "1"
		case "master_limiter":
			v.MasterLimiter = val == "1"
		case "auto_gain":
			v.AutoGain = val == "1"
		case "hifi_output":
			v.HifiOutput = val == "1"
		case "omni_source":
			v.OmniSource = val == "1"
		case "remote_load":
			v.RemoteLoad = val == "1"
		case "rekordbox_share":
			if validRekordboxShare(val) {
				v.RekordboxShare = val
			}
		case "prolink_share", "prolink_export":
			if val == "1" {
				v.ProlinkShare = true
			}
		case "known":
			known = strings.Split(val, ",")
		case "four_deck_role":
			if slices.Contains(uiFourDeckRoles, val) {
				v.FourDeckRole = val
			}
		case "paired_view":
			v.PairedView = val == "1"
		case "console_line_view":
			v.ConsoleLineView = val == "1"
		case "console_decks":
			v.ConsoleDecks = val == "1"
		case "share_decks":
			v.ShareDecks = val == "1"
		case "four_deck_waveform":
			if val == "1" || strings.EqualFold(val, "true") {
				v.FourDeckWaveform = true
			} else if val == "0" || strings.EqualFold(val, "false") {
				v.FourDeckWaveform = false
			}
		case "shadow_mode":
			if slices.Contains(uiShadowModes, val) {
				v.ShadowMode, shadowModeSet = val, true
			}
		case "shadow_legacy_engine": // the key before shadow_mode; read, never written
			if b, ok := parseBoolParam(val); ok && !shadowModeSet {
				v.ShadowMode = map[bool]string{false: "cdj", true: "engine"}[b]
			}
		case "prolink_waveform":
			if val == "0" || strings.EqualFold(val, "false") {
				v.ProlinkWaveform = false
			} else if val == "1" || strings.EqualFold(val, "true") {
				v.ProlinkWaveform = true
			}
		case "upfader_echo":
			if val == "1" || strings.EqualFold(val, "true") {
				v.UpfaderEcho = true
			} else if val == "0" || strings.EqualFold(val, "false") {
				v.UpfaderEcho = false
			}
		case "crossfader_reverse":
			if b, ok := parseBoolParam(val); ok {
				v.CrossfaderReverse = b
			}
		case "verbose_log":
			v.VerboseLog = val == "1" || strings.EqualFold(val, "true")
		case "debug_telemetry":
			v.DebugTelemetry = val == "1" || strings.EqualFold(val, "true")
		case "stream_sort_role":
			if slices.Contains(uiStreamSortRoles, val) {
				v.StreamSortRole = val
			}
		case "stream_sort_order":
			if slices.Contains(uiStreamSortOrders, val) {
				v.StreamSortOrder = val
			}
		case "cue_countdown":
			v.CueCountdown = val != "0" && !strings.EqualFold(val, "false")
		case "bpm_decimals":
			if val == "1" {
				v.BpmDecimals = 1
			} else if val == "2" {
				v.BpmDecimals = 2
			}
		case "browser_dual_column":
			v.BrowserDualColumn = val == "1" || strings.EqualFold(val, "true")
		case "tracklist_waveform":
			if val == "0" || strings.EqualFold(val, "false") {
				v.TracklistWaveform = false
			} else if val == "1" || strings.EqualFold(val, "true") {
				v.TracklistWaveform = true
			}
		case "rb_library_view":
			if slices.Contains(uiRbLibraryViews, val) {
				v.RbLibraryView = val
			}
		case "bt_out":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 && n < 5 {
				v.BtOut = n
			}
		case "sync_guard_pct":
			if n, err := strconv.Atoi(val); err == nil && slices.Contains(uiSyncGuardPcts, n) {
				v.SyncGuardPct = n
			}
		case "swap_loop_order":
			v.SwapLoopOrder = val == "1" || strings.EqualFold(val, "true")
		case "led_brightness":
			if n, err := strconv.Atoi(val); err == nil && n >= 20 && n <= 100 {
				v.LedBrightness = n
			}
		case "ui_scale":
			if n, err := strconv.Atoi(val); err == nil && slices.Contains(uiUIScales, n) {
				v.UIScale = n
			}
		case "stems_eq_mode":
			v.StemsEqMode = val == "1" || strings.EqualFold(val, "true")
		case "beat_jump_pads":
			v.BeatJumpPads = val == "1" || strings.EqualFold(val, "true")
		case "pitch_bend_beat_jump":
			v.PitchBendBeatJump = val == "1" || strings.EqualFold(val, "true")
		case "restore_bpm_on_unsync":
			v.RestoreBpmOnUnsync = val == "1" || strings.EqualFold(val, "true")
		case "mixstream_fx":
			if fx, ok := parseMixstreamFx(val); ok {
				v.MixstreamFx = fx
			}
		case "jog_invert_left":
			v.JogInvertLeft = val == "1" || strings.EqualFold(val, "true")
		case "jog_invert_right":
			v.JogInvertRight = val == "1" || strings.EqualFold(val, "true")
		case "sd_high_speed":
			v.SdHighSpeed = val == "1" || strings.EqualFold(val, "true")
		case "pioneer_fx":
			if slices.Contains(uiPioneerFxModes, val) {
				v.PioneerFx = val
			}
		case "fx_time_mode":
			v.FxTimeMode = "beats"
			if val == "ms" {
				v.FxTimeMode = "ms"
			}
		case "fx_time_ms":
			v.FxTimeMs = 500
			if n, err := uiFxTimeInteger(val); err == nil {
				v.FxTimeMs = int(max(10, min(4000, n)))
			}
		case "console_https":
			v.ConsoleHTTPS = val == "1" || strings.EqualFold(val, "true")
		case "webdav":
			if val == "0" || val == "1" {
				v.WebDAVSwitch = val
			}
		case "nas":
			if val == "0" || val == "1" {
				v.NASSwitch = val
			}
		}
	}
	if saved { // services added since the file was written start enabled; ones turned off stay off
		for _, name := range uiServices {
			if !slices.Contains(known, name) && !slices.Contains(v.Sources, name) {
				v.Sources = append(v.Sources, name)
			}
		}
		v.Sources, _ = parseUISources(strings.Join(v.Sources, ",")) // fixed order
	}
	if !v.FourDeckWaveform {
		v.PairedView = false // #201: the paired view is a view of the 4-deck layout; a stray key alone is not it
	}
	if v.ConsoleLineView && !consoleLineBoards[imagePrefix(deviceCompatible())] {
		v.ConsoleLineView = false // a stored or copied value means nothing on any other board
	}
	if v.ConsoleDecks && (!v.FourDeckWaveform || !playerBoards[imagePrefix(deviceCompatible())]) {
		v.ConsoleDecks = false // the row belongs to the 4-deck layout of the standalone players; a stray key alone is nothing
	}
	if v.ShareDecks && !playerBoards[imagePrefix(deviceCompatible())] {
		v.ShareDecks = false // a stored or copied value means nothing on any other board
	}
	if slices.Contains(uiServices, v.Scope) && !slices.Contains(v.Sources, v.Scope) {
		v.Scope = ""
	}
	v.SafeMode = bootMarker(s.uiDir(), "safe-mode")
	return v
}

func parseUISources(val string) ([]string, error) {
	list := []string{}
	for _, name := range uiServices { // fixed order and no duplicates
		if slices.Contains(strings.Split(val, ","), name) {
			list = append(list, name)
		}
	}
	for _, name := range strings.Split(val, ",") {
		// rekordbox, NAS and WebDAV are in the status's list but are not switched
		// here: the deck sends that list back when the DJ flips a service.
		if _, _, server := serverScopeOf(name); server || name == rekordboxScope || name == nasScope || name == webdavScope {
			continue
		}
		if name != "" && !slices.Contains(uiServices, name) {
			return nil, errors.New("unknown source: " + name)
		}
	}
	return list, nil
}

func (s *Server) saveUI(v uiSettings, exportRequested ...bool) (err error) {
	defer func() {
		if err == nil {
			s.applyShadowFlags(v)
		}
	}()
	was := false
	if s.prolinkExport != nil && (v.ProlinkShare != s.loadUI().ProlinkShare || len(exportRequested) > 0 && exportRequested[0]) {
		was = s.prolinkExport.Status().Enabled
		if err = s.prolinkExport.SetEnabled(v.ProlinkShare); err != nil {
			if !v.ProlinkShare {
				return err
			}
			// ON stays ON without a Rekordbox USB; retryExport starts it once one is mounted.
			log.Printf("stream-sync: Pro DJ Link export waits: %v", err)
			err = nil
		}
		defer func() {
			if err != nil && was != v.ProlinkShare {
				_ = s.prolinkExport.SetEnabled(was)
			}
		}()
	}
	dir := s.uiDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	text := "lang=" + v.Language + "\nwaveform=" + v.Waveform + "\nsources=" + strings.Join(v.Sources, ",") + "\nknown=" + strings.Join(uiServices, ",") + "\nscope=" + v.Scope + "\nquality=" + v.Quality + "\nspotify_source=" + v.SpotifySource + "\nnetease_mode=" + v.NeteaseMode + "\nplayhead=" + strconv.FormatFloat(v.Playhead, 'f', -1, 64) + "\n"
	if v.EngineLighting {
		text += "engine_lighting=1\n"
	}
	if v.WebDAVSwitch != "" { // absent: on while a WebDAV server is set up
		text += "webdav=" + v.WebDAVSwitch + "\n"
	}
	if v.NASSwitch != "" { // absent: on while a NAS server is set up
		text += "nas=" + v.NASSwitch + "\n"
	}
	if v.FourDeckWaveform {
		text += "four_deck_waveform=1\n"
		if v.PairedView {
			text += "paired_view=1\n"
		}
	}
	if v.ShadowMode != "auto" {
		text += "shadow_mode=" + v.ShadowMode + "\n"
	}
	if v.ConsoleLineView {
		text += "console_line_view=1\n"
	}
	if v.ConsoleDecks && v.FourDeckWaveform {
		text += "console_decks=1\n"
	}
	if v.ShareDecks {
		text += "share_decks=1\n"
	}
	if v.FourDeckRole != "auto" {
		text += "four_deck_role=" + v.FourDeckRole + "\n"
	}
	if !v.ProlinkWaveform {
		text += "prolink_waveform=0\n"
	}
	if v.UpfaderEcho {
		text += "upfader_echo=1\n"
	}
	if v.CrossfaderReverse {
		text += "crossfader_reverse=1\n"
	}
	if v.AutoAnalyze {
		text += "auto_analyze=1\n"
	}
	if v.VerboseLog {
		text += "verbose_log=1\n"
	}
	if v.DebugTelemetry {
		text += "debug_telemetry=1\n"
	}
	if v.MasterLimiter {
		text += "master_limiter=1\n"
	}
	if v.AutoGain {
		text += "auto_gain=1\n"
	}
	if v.HifiOutput {
		text += "hifi_output=1\n"
	}
	if v.OmniSource {
		text += "omni_source=1\n"
	}
	if v.RemoteLoad {
		text += "remote_load=1\n"
	}
	if v.ProlinkShare {
		text += "prolink_share=1\n"
	}
	if v.RekordboxShare != "" {
		text += "rekordbox_share=" + v.RekordboxShare + "\n"
	}
	if v.StreamSortRole != "" && v.StreamSortRole != "default" {
		text += "stream_sort_role=" + v.StreamSortRole + "\n"
	}
	if v.StreamSortOrder != "" && v.StreamSortOrder != "asc" {
		text += "stream_sort_order=" + v.StreamSortOrder + "\n"
	}
	if !v.CueCountdown {
		text += "cue_countdown=0\n"
	}
	if v.BpmDecimals == 1 {
		text += "bpm_decimals=1\n"
	}
	if v.BrowserDualColumn {
		text += "browser_dual_column=1\n"
	}
	if !v.TracklistWaveform {
		text += "tracklist_waveform=0\n"
	}
	if slices.Contains(uiRbLibraryViews, v.RbLibraryView) && v.RbLibraryView != "folders" {
		text += "rb_library_view=" + v.RbLibraryView + "\n"
	}
	if v.BtOut > 0 && v.BtOut < 5 {
		text += "bt_out=" + strconv.Itoa(v.BtOut) + "\n"
	}
	if v.SyncGuardPct != 0 {
		text += "sync_guard_pct=" + strconv.Itoa(v.SyncGuardPct) + "\n"
	}
	if v.SwapLoopOrder {
		text += "swap_loop_order=1\n"
	}
	if v.LedBrightness != 100 && v.LedBrightness >= 20 && v.LedBrightness <= 100 {
		text += "led_brightness=" + strconv.Itoa(v.LedBrightness) + "\n"
	}
	if slices.Contains(uiUIScales, v.UIScale) && v.UIScale != 100 {
		text += "ui_scale=" + strconv.Itoa(v.UIScale) + "\n"
	}
	if v.StemsEqMode {
		text += "stems_eq_mode=1\n"
	}
	if v.BeatJumpPads {
		text += "beat_jump_pads=1\n"
	}
	if v.PitchBendBeatJump {
		text += "pitch_bend_beat_jump=1\n"
	}
	if v.RestoreBpmOnUnsync {
		text += "restore_bpm_on_unsync=1\n"
	}
	if len(v.MixstreamFx) == len(uiMixstreamFxDefaults) && !slices.Equal(v.MixstreamFx, uiMixstreamFxDefaults) {
		text += "mixstream_fx=" + strings.Join(v.MixstreamFx, ",") + "\n"
	}
	if v.JogInvertLeft {
		text += "jog_invert_left=1\n"
	}
	if v.JogInvertRight {
		text += "jog_invert_right=1\n"
	}
	if v.SdHighSpeed {
		text += "sd_high_speed=1\n"
	}
	if (!s.fxSelectorAvailable() || s.colorFXAvailable()) && v.PioneerFx != "" && v.PioneerFx != "off" {
		text += "pioneer_fx=" + v.PioneerFx + "\n"
	}
	if v.FxTimeMode == "ms" {
		text += "fx_time_mode=ms\n"
	}
	if v.FxTimeMs != 500 && v.FxTimeMs >= 10 && v.FxTimeMs <= 4000 {
		text += "fx_time_ms=" + strconv.Itoa(v.FxTimeMs) + "\n"
	}
	if v.GitHubRepo != defaultGitHubRepo {
		text += "github_repo=" + v.GitHubRepo + "\n"
	}
	if v.ConsoleHTTPS {
		text += "console_https=1\n"
	}
	path := filepath.Join(dir, "settings.conf")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == text {
		return writeUIScaleEnv(v.UIScale)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.WriteString(text); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return writeUIScaleEnv(v.UIScale)
}

// Caller holds uiMu. Preserve unrelated/unknown settings during migration.
func (s *Server) migratePioneerFX() error {
	path := filepath.Join(s.uiDir(), "settings.conf")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, "pioneer_fx=") {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(lines) {
		return nil
	}
	f, err := os.CreateTemp(s.uiDir(), ".fx-settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(strings.Join(kept, "\n"))
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *Server) deckClockWanted(now time.Time) bool {
	if s.shadow != nil {
		s.shadow.mu.Lock()
		polled := s.shadow.clockPolled
		s.shadow.mu.Unlock()
		if !polled.IsZero() && now.Sub(polled) < 15*time.Second {
			return true
		}
	}
	if preparing, _ := s.usbPreparingState(now); preparing {
		return true
	}
	return s.modsManager().IsModEnabled("dmx-studio")
}

// scope is the service the deck's native browser currently shows.
func (s *Server) scope() string {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().Scope
}

func (s *Server) deezerARLReady() bool {
	if strings.TrimSpace(s.savedDeezerOwner().ARL) == "" {
		return false
	}
	s.mu.Lock()
	deleted := s.ownerDeleted["deezer"]
	s.mu.Unlock()
	return !deleted
}

// scopeLoggedIn reports whether the DJ has an active login (or link source)
// for scope. Real TIDAL logs in via Engine's native OAuth flow after selecting
// TIDAL, so it is gated by the TIDAL switch in sources rather than here.
func (s *Server) scopeLoggedIn(scope string) bool {
	if scope == "" || scope == "tidal" {
		return true
	}
	if scope == rekordboxScope {
		return s.rekordboxHost() != nil || len(s.rbxPlayers()) > 0
	}
	if scope == nasScope {
		return s.nasAvailable()
	}
	if _, _, server := serverScopeOf(scope); server {
		_, ok := s.serverByScope(scope)
		return ok
	}
	if scope == "deezer" && s.deezerARLReady() {
		return true
	}
	if s.tidal == nil {
		return false
	}
	for _, session := range s.tidal.sourcesFor(scope) {
		if !session.Guest && session.Provider == scope && ready(session.p) {
			return true
		}
	}
	return false
}

// One metadata lookup only: never follow symlinks or read USB contents here.
func (s *Server) usbPreparingState(now time.Time) (bool, *usbPrepareStatus) {
	path := s.cfg.USBPreparingPath
	if path == "" {
		path = "/run/enginex-usb-preparing"
	}
	readStatus := func(p string, maxAge time.Duration) (string, bool) {
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			return "", false
		}
		age := now.Sub(info.ModTime())
		if age > maxAge || age < -5*time.Second {
			return "", false
		}
		if info.Size() <= 0 || info.Size() > 4096 {
			return "", true
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", true
		}
		return strings.TrimSpace(string(data)), true
	}

	if text, ok := readStatus(path, 30*time.Second); ok {
		if strings.HasPrefix(text, "{") {
			var st usbPrepareStatus
			if json.Unmarshal([]byte(text), &st) == nil {
				if st.State == "error" {
					return false, &st
				}
				if st.State == "" {
					st.State = "loading"
				}
				if st.Kind == "" {
					st.Kind = "usb"
				}
				return true, &st
			}
		}
		return true, nil
	}

	if text, ok := readStatus(path+".err", 8*time.Second); ok && strings.HasPrefix(text, "{") {
		var st usbPrepareStatus
		if json.Unmarshal([]byte(text), &st) == nil && st.Error != "" {
			st.State = "error"
			return false, &st
		}
	}
	return false, nil
}

func (s *Server) isLegacy504Pack() bool {
	enDir := filepath.Join(s.uiDir(), "packs", "en")
	if _, err := os.Stat(filepath.Join(enDir, "Skins", "EmbeddedCommon", "EmbeddedMain.qml")); err == nil {
		if _, jerr := os.Stat(filepath.Join(enDir, "ControlSurfaceModules", "ControlSurfaceModules", "JogWheel.qml")); errors.Is(jerr, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (s *Server) uiStatus(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	v := s.loadUI()
	nasReady := s.nasAvailableLocked()
	nf := s.loadNASFileLocked()
	v.WebDAV, v.NAS = serverViews(kindWebDAV, nf), serverViews(kindNAS, nf)
	v.WebDAVOn, v.NASOn = serversOn(kindWebDAV, v, nf), serversOn(kindNAS, v, nf)
	// Every enabled server of a kind that is on is its own entry in Sources (the
	// slot's SMB server is "nas", added below as before).
	var serverScopes []string
	for _, k := range serverKinds {
		if !serversOn(k, v, nf) {
			continue
		}
		for _, e := range serverEntries(k, nf) {
			if e.qualified() && nasConfigReady(e.cfg) {
				serverScopes = append(serverScopes, e.Scope())
			}
		}
	}
	v.FXSelectorAvailable = s.fxSelectorAvailable()
	v.ColorFXAvailable = s.colorFXAvailable()
	// pioneer_fx is the channel knobs' style now; only a PX board without them drops it.
	if v.FXSelectorAvailable && !v.ColorFXAvailable {
		if err := s.migratePioneerFX(); err != nil {
			log.Printf("FX selector settings migration: %v", err)
		}
		v.PioneerFx = "off"
	}
	s.uiMu.Unlock()
	v.TidalLoggedIn = s.tidal != nil && s.tidal.hasRealAuth()
	v.TidalSynthetic = s.tidal != nil && s.tidal.isSyntheticAccountOnly()
	v.BroadcastEnabled = s.BroadcastEnabled()
	if ver := s.installedVersion(); ver != "" && ver != "dev" {
		v.Version = ver
	}
	v.LoggedIn = []string{}
	for _, session := range s.tidal.sources() {
		if !session.Guest && ready(session.p) && !slices.Contains(v.LoggedIn, session.Provider) {
			v.LoggedIn = append(v.LoggedIn, session.Provider)
		}
	}
	if !slices.Contains(v.LoggedIn, "deezer") && s.deezerARLReady() {
		v.LoggedIn = append(v.LoggedIn, "deezer")
	}
	name := ""
	if host := s.rekordboxHost(); host != nil { // not saved: the source is there while the computer is
		name = host.Name
	} else if players := s.rbxPlayers(); len(players) > 0 { // or a player whose media it lists
		name = players[0].Name
	}
	if name != "" {
		v.Sources = append(slices.Clone(v.Sources), rekordboxScope)
		v.LoggedIn = append(v.LoggedIn, rekordboxScope)
		v.RekordboxName = name
	}
	if nasReady {
		v.Sources = append(slices.Clone(v.Sources), nasScope)
		v.LoggedIn = append(v.LoggedIn, nasScope)
	}
	v.Sources = append(slices.Clone(v.Sources), serverScopes...)
	v.LoggedIn = append(v.LoggedIn, serverScopes...)
	relay := s.relayStatusView()
	v.RelayURL, v.DeemixPasswordSet = relay.RelayURL, relay.PasswordSet
	v.ProlinkExportActive = s.prolinkExport != nil && s.prolinkExport.Status().Enabled
	board := imagePrefix(deviceCompatible())
	v.Board = board
	v.BuildTag = currentBuildTag()
	v.Player = playerBoards[board]
	v.Capabilities = boardCapabilities(board)
	if s.isLegacy504Pack() {
		v.Capabilities.JogWheel = false
	}
	if !v.Capabilities.JogWheel {
		v.JogInvertLeft = false
		v.JogInvertRight = false
	}
	if board != "" && !v.Player {
		v.HifiOutput = false
	}
	v.SdHighSpeedPending = s.sdHighSpeedPending(v.SdHighSpeed)
	v.FourDeckRoleActive = s.activeRole(v)
	v.TimeZone = s.savedTimeZone()
	now := time.Now()
	v.DeckClock = s.deckClockWanted(now)
	if v.DeckClock {
		s.rbPriorityQueue().RefreshDeckActivity(now)
	}
	v.USBPreparing, v.USBPrepare = s.usbPreparingState(now)
	httpsOn := s.httpsOfferedLocked(v.ConsoleHTTPS)
	if httpsOn {
		v.HTTPSPort = s.httpsPortNum()
	}
	consoleLink := "http://" + lanHost() + ":8080"
	if httpsOn {
		consoleLink = "https://" + net.JoinHostPort(lanHost(), s.httpsPortStr())
	}
	if public := s.publicConsoleURL(); public != "" {
		consoleLink = public
	}
	// The deck's own pack applies the answer to a setting it posts like a poll's: an answer
	// without what the poll has (the hook's state among it) took that away until the next
	// poll that applies, 3 to 30 s later, and a paused shadow copy played for that long.
	if r.URL.Path == "/v1/enginex/ui" {
		featuresOff := runningFeaturesOff()
		if featuresOff == nil {
			featuresOff = []string{}
		}
		pausedFollow := runningFeatureOn("shadow-paused-follow")
		if s.shadow != nil {
			s.shadow.pausedFollow.Store(pausedFollow)
		}
		body, err := json.Marshal(struct {
			uiSettings
			AccessCode string       `json:"accessCode"`
			ConsoleURL string       `json:"consoleUrl"`
			Tunnel     TunnelStatus `json:"tunnel"`
			// LANStems: the home legacy-stemsd last seen, for Settings -> About (#148).
			LANStems *stems.LANState `json:"lanStems,omitempty"`
			// FeaturesOff: hook features the running Engine has off; always sent
			// (empty [] when none or unknown) so a running UI clears stale entries.
			FeaturesOff []string `json:"featuresOff"`
			// ShadowPausedFollow: the running hook moves a paused shadow deck's line;
			// always sent (also false) so a running UI clears stale true state.
			ShadowPausedFollow bool `json:"shadowPausedFollow"`
		}{v, s.cfg.AccessToken, consoleLink, s.TunnelManager().Status(""), relay.LANStems, featuresOff, pausedFollow})
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		// The Engine UI polls this every 3 s; an unchanged digest spares it the
		// JSON.parse and ~40 property writes (V4 garbage on the GUI thread).
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-UI-Digest", strconv.FormatUint(fnvSum(string(body)), 16))
		w.Write(append(body, '\n'))
		return
	}
	respond(w, v)
}

func parseBoolParam(val string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	default:
		return false, false
	}
}

// uiUpdate changes the settings named in the query string. A language only
// takes effect when Engine starts, so restart=1 restarts it and also leaves
// safe mode: the user has asked for the UI to be tried again.
func (s *Server) uiUpdate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Body != nil && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]any
		r.Body = http.MaxBytesReader(w, r.Body, jsonBodyLimit)
		dec := json.NewDecoder(r.Body)
		// Exactly one value: anything but white space after it is a bad request too.
		if err := dec.Decode(&body); err != nil || dec.Decode(new(any)) != io.EOF {
			fail(w, http.StatusBadRequest, errors.New("invalid settings JSON"))
			return
		}
		for k, v := range body {
			if !q.Has(k) {
				q.Set(k, fmt.Sprint(v))
			}
		}
	}
	if q.Has("fxUnit") || q.Has("fxMode") {
		s.fxSelectorUpdate(w, r, q)
		return
	}
	if q.Has("timezone") && len(q) == 1 {
		s.timezoneUpdate(w, q.Get("timezone"))
		return
	}
	// The switch belongs to the deck: not from the LAN console API. Tunnels
	// never reach /v1 (the gateway refuses relayed requests, ForwardHandler).
	if (q.Has("remoteLoad") || q.Has("remote_load")) && strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, 403, errors.New("remoteLoad can only be changed on the deck"))
		return
	}
	if q.Has("relayUrl") && !validRelayURL(q.Get("relayUrl")) {
		fail(w, 400, errInvalidRelayURL)
		return
	}
	if q.Has("scope") {
		want := q.Get("scope")
		if _, _, server := serverScopeOf(want); !server && !slices.Contains(uiScopes, want) {
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		if !s.scopeLoggedIn(want) {
			fail(w, http.StatusConflict, errors.New("source is not logged in"))
			return
		}
	}
	s.uiMu.Lock()
	v := s.loadUI()
	prevShadowMode := v.ShadowMode
	prevConsoleHTTPS := v.ConsoleHTTPS
	prevSdHighSpeed := v.SdHighSpeed
	bad := func(field, val string, allowed []string) bool {
		return q.Has(field) && !slices.Contains(allowed, val)
	}
	if bad("spotifySource", q.Get("spotifySource"), uiSpotifySources) || bad("lang", q.Get("lang"), uiLanguages) || bad("waveform", q.Get("waveform"), uiWaveforms) || (bad("scope", q.Get("scope"), uiScopes) && !isServerScope(q.Get("scope"))) || bad("quality", q.Get("quality"), uiQualities) || bad("neteaseMode", q.Get("neteaseMode"), uiNeteaseModes) || bad("fourDeckRole", q.Get("fourDeckRole"), uiFourDeckRoles) || bad("streamSortRole", q.Get("streamSortRole"), uiStreamSortRoles) || bad("streamSortOrder", q.Get("streamSortOrder"), uiStreamSortOrders) || bad("pioneerFx", q.Get("pioneerFx"), uiPioneerFxModes) || bad("shadowMode", q.Get("shadowMode"), uiShadowModes) || bad("fxTimeMode", q.Get("fxTimeMode"), uiFxTimeModes) || bad("rbLibraryView", q.Get("rbLibraryView"), uiRbLibraryViews) {
		s.uiMu.Unlock()
		fail(w, 400, errors.New("invalid setting"))
		return
	}
	if q.Has("consoleHttps") || q.Has("console_https") {
		param := q.Get("consoleHttps")
		if !q.Has("consoleHttps") {
			param = q.Get("console_https")
		}
		b, ok := parseBoolParam(param)
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ConsoleHTTPS = b
	}
	if q.Has("engineLighting") || q.Has("engine_lighting") {
		param := q.Get("engineLighting")
		if !q.Has("engineLighting") {
			param = q.Get("engine_lighting")
		}
		b, ok := parseBoolParam(param)
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.EngineLighting = b
	}
	if q.Has("shadowMode") {
		v.ShadowMode = q.Get("shadowMode")
	}
	if q.Has("rbLibraryView") {
		v.RbLibraryView = q.Get("rbLibraryView")
	}
	if q.Has("verboseLog") {
		b, ok := parseBoolParam(q.Get("verboseLog"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.VerboseLog = b
	}
	if q.Has("fourDeckWaveform") {
		b, ok := parseBoolParam(q.Get("fourDeckWaveform"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.FourDeckWaveform = b
		if !b {
			v.PairedView = false // the paired view is a view of the 4-deck layout
			v.ConsoleDecks = false
		}
	}
	if q.Has("consoleLineView") {
		b, ok := parseBoolParam(q.Get("consoleLineView"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ConsoleLineView = b && consoleLineBoards[imagePrefix(deviceCompatible())]
	}
	if q.Has("consoleDecks") {
		b, ok := parseBoolParam(q.Get("consoleDecks"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ConsoleDecks = b && v.FourDeckWaveform && playerBoards[imagePrefix(deviceCompatible())]
	}
	if q.Has("shareDecks") {
		b, ok := parseBoolParam(q.Get("shareDecks"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ShareDecks = b && playerBoards[imagePrefix(deviceCompatible())]
	}
	if q.Has("pairedView") {
		b, ok := parseBoolParam(q.Get("pairedView"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.PairedView = b
		if b {
			v.FourDeckWaveform = true
		}
	}
	if q.Has("prolinkWaveform") {
		b, ok := parseBoolParam(q.Get("prolinkWaveform"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ProlinkWaveform = b
	}
	if q.Has("upfaderEcho") {
		b, ok := parseBoolParam(q.Get("upfaderEcho"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.UpfaderEcho = b
	}
	if q.Has("crossfaderReverse") {
		b, ok := parseBoolParam(q.Get("crossfaderReverse"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.CrossfaderReverse = b
	}
	if q.Has("cueCountdown") {
		b, ok := parseBoolParam(q.Get("cueCountdown"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.CueCountdown = b
	}
	if q.Has("bpmDecimals") || q.Has("bpm_decimals") {
		param := q.Get("bpmDecimals")
		if !q.Has("bpmDecimals") {
			param = q.Get("bpm_decimals")
		}
		n, err := strconv.Atoi(strings.TrimSpace(param))
		if err != nil || (n != 1 && n != 2) {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.BpmDecimals = n
	}
	if q.Has("browserDualColumn") {
		b, ok := parseBoolParam(q.Get("browserDualColumn"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.BrowserDualColumn = b
	}
	if q.Has("tracklistWaveform") || q.Has("libraryWaveform") {
		param := q.Get("tracklistWaveform")
		if !q.Has("tracklistWaveform") {
			param = q.Get("libraryWaveform")
		}
		b, ok := parseBoolParam(param)
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.TracklistWaveform = b
	}
	if q.Has("swapLoopOrder") {
		b, ok := parseBoolParam(q.Get("swapLoopOrder"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.SwapLoopOrder = b
	}
	if q.Has("stemsEqMode") {
		b, ok := parseBoolParam(q.Get("stemsEqMode"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.StemsEqMode = b
	}
	if q.Has("beatJumpPads") {
		b, ok := parseBoolParam(q.Get("beatJumpPads"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.BeatJumpPads = b
	}
	if q.Has("pitchBendBeatJump") {
		b, ok := parseBoolParam(q.Get("pitchBendBeatJump"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.PitchBendBeatJump = b
	}
	if q.Has("restoreBpmOnUnsync") {
		b, ok := parseBoolParam(q.Get("restoreBpmOnUnsync"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.RestoreBpmOnUnsync = b
	}
	if len(v.MixstreamFx) != len(uiMixstreamFxDefaults) {
		v.MixstreamFx = slices.Clone(uiMixstreamFxDefaults)
	} else {
		v.MixstreamFx = slices.Clone(v.MixstreamFx)
	}
	if q.Has("mixstreamFx") {
		fx, ok := parseMixstreamFx(q.Get("mixstreamFx"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.MixstreamFx = fx
	}
	for i := range uiMixstreamFxDefaults {
		key := "mixstreamFx" + strconv.Itoa(i+1)
		if q.Has(key) {
			fx := strings.TrimSpace(q.Get(key))
			if !slices.Contains(uiMixstreamFxChoices, fx) {
				s.uiMu.Unlock()
				fail(w, 400, errors.New("invalid setting"))
				return
			}
			v.MixstreamFx[i] = fx
		}
	}
	jogSupported := boardCapabilities(imagePrefix(deviceCompatible())).JogWheel && !s.isLegacy504Pack()
	if q.Has("jogInvertLeft") {
		b, ok := parseBoolParam(q.Get("jogInvertLeft"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		if jogSupported {
			v.JogInvertLeft = b
		} else {
			v.JogInvertLeft = false
		}
	}
	if q.Has("jogInvertRight") {
		b, ok := parseBoolParam(q.Get("jogInvertRight"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		if jogSupported {
			v.JogInvertRight = b
		} else {
			v.JogInvertRight = false
		}
	}
	if q.Has("sdHighSpeed") || q.Has("sd_high_speed") {
		param := q.Get("sdHighSpeed")
		if !q.Has("sdHighSpeed") {
			param = q.Get("sd_high_speed")
		}
		b, ok := parseBoolParam(param)
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.SdHighSpeed = b
	}
	if q.Has("syncGuardPct") {
		n, err := strconv.Atoi(q.Get("syncGuardPct"))
		if err != nil || !slices.Contains(uiSyncGuardPcts, n) {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.SyncGuardPct = n
	}
	if q.Has("ledBrightness") {
		n, err := strconv.Atoi(q.Get("ledBrightness"))
		if err != nil || n < 20 || n > 100 {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.LedBrightness = n
	}
	if q.Has("uiScale") {
		n, err := strconv.Atoi(q.Get("uiScale"))
		if err != nil || !slices.Contains(uiUIScales, n) {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.UIScale = n
	}
	if q.Has("streamSortRole") {
		v.StreamSortRole = q.Get("streamSortRole")
	}
	if q.Has("streamSortOrder") {
		v.StreamSortOrder = q.Get("streamSortOrder")
	}
	if q.Has("pioneerFx") {
		v.PioneerFx = q.Get("pioneerFx")
	}
	if q.Has("fxTimeMode") {
		v.FxTimeMode = q.Get("fxTimeMode")
	}
	if q.Has("fxTimeMs") {
		n, err := uiFxTimeInteger(q.Get("fxTimeMs"))
		if err != nil || n < 10 || n > 4000 {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.FxTimeMs = int(n)
	}
	var (
		hasBroadcast bool
		bcastOn      bool
	)
	if q.Has("broadcast") {
		b, ok := parseBoolParam(q.Get("broadcast"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		hasBroadcast = true
		bcastOn = b
	}
	if q.Has("lang") {
		v.Language = q.Get("lang")
	}
	if q.Has("waveform") {
		v.Waveform = q.Get("waveform")
	}
	if q.Has("quality") {
		v.Quality = q.Get("quality")
	}
	if q.Has("fourDeckRole") {
		v.FourDeckRole = q.Get("fourDeckRole")
	}
	if q.Has("spotifySource") {
		v.SpotifySource = q.Get("spotifySource")
	}
	if q.Has("neteaseMode") {
		v.NeteaseMode = q.Get("neteaseMode")
	}
	if q.Has("playhead") {
		f, err := strconv.ParseFloat(q.Get("playhead"), 64)
		if err != nil || !validPlayhead(f) {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.Playhead = f
	}
	if q.Has("autoAnalyze") {
		on := q.Get("autoAnalyze")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.AutoAnalyze = on == "1"
	}
	if q.Has("masterLimiter") {
		on := q.Get("masterLimiter")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.MasterLimiter = on == "1"
	}
	if q.Has("autoGain") {
		on := q.Get("autoGain")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.AutoGain = on == "1"
	}
	if q.Has("hifiOutput") || q.Has("hifi_output") {
		raw := q.Get("hifiOutput")
		if !q.Has("hifiOutput") {
			raw = q.Get("hifi_output")
		}
		on, ok := parseBoolParam(raw)
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		board := imagePrefix(deviceCompatible())
		v.HifiOutput = on && (board == "" || playerBoards[board])
	}
	if q.Has("rekordboxShare") {
		if !validRekordboxShare(q.Get("rekordboxShare")) {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.RekordboxShare = strings.TrimSpace(q.Get("rekordboxShare"))
	}
	if q.Has("omniSource") {
		on := q.Get("omniSource")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.OmniSource = on == "1"
	}
	if q.Has("remoteLoad") || q.Has("remote_load") {
		on := q.Get("remoteLoad")
		if !q.Has("remoteLoad") {
			on = q.Get("remote_load")
		}
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.RemoteLoad = on == "1"
	}
	if q.Has("webdav") {
		on := q.Get("webdav")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.WebDAVSwitch = on
		if k, _, _ := serverScopeOf(v.Scope); on == "0" && k == kindWebDAV {
			v.Scope = ""
		}
	}
	if q.Has("nas") {
		on := q.Get("nas")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.NASSwitch = on
		if k, _, _ := serverScopeOf(v.Scope); on == "0" && (k == kindNAS || v.Scope == nasScope) {
			v.Scope = ""
		}
	}
	if q.Has("prolinkShare") {
		on := q.Get("prolinkShare")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ProlinkShare = on == "1"
	}
	if q.Has("sources") {
		list, err := parseUISources(q.Get("sources"))
		if err != nil {
			s.uiMu.Unlock()
			fail(w, 400, err)
			return
		}
		v.Sources = list
	}
	if q.Has("scope") {
		want := q.Get("scope")
		if slices.Contains(uiServices, want) && !slices.Contains(v.Sources, want) {
			s.uiMu.Unlock()
			fail(w, http.StatusConflict, errors.New("source is disabled"))
			return
		}
		v.Scope = want
	}
	// Relay settings are written only once every field is known to be valid (#9).
	if q.Has("relayUrl") || q.Has("deemixPassword") {
		var relay, password *string
		if q.Has("relayUrl") {
			relay = ptrTo(q.Get("relayUrl"))
		}
		if q.Has("deemixPassword") {
			password = ptrTo(q.Get("deemixPassword"))
		}
		if err := s.updateRelayConfigLocked(relay, password); err != nil {
			s.uiMu.Unlock()
			fail(w, 500, errors.New("could not save the relay settings"))
			return
		}
	}
	previousVerbose := s.loadUI().VerboseLog
	if q.Has("verboseLog") {
		if err := writeVerboseLog(v.VerboseLog); err != nil {
			s.uiMu.Unlock()
			fail(w, 500, errors.New("could not save logging configuration"))
			return
		}
	}
	if slices.Contains(uiServices, v.Scope) && !slices.Contains(v.Sources, v.Scope) {
		v.Scope = ""
	}
	err := s.saveUI(v, q.Has("prolinkShare"))
	if err != nil && q.Has("verboseLog") {
		_ = writeVerboseLog(previousVerbose)
	}
	// A WebDAV or NAS row switched off cancels its servers' requests still running (a
	// browse, a search, a download): nothing of that kind answers after the switch. Under
	// uiMu, with the save: a search that lists its servers after it finds the kind off,
	// one that listed them before it has its connections cancelled (connectedServers).
	if err == nil && (q.Get("webdav") == "0" || q.Get("nas") == "0") {
		s.dropServerConns(func(_ string, k serverKind) bool {
			return (k == kindWebDAV && q.Get("webdav") == "0") || (k == kindNAS && q.Get("nas") == "0")
		})
	}
	if err == nil && v.ConsoleHTTPS != prevConsoleHTTPS {
		if herr := s.applyHTTPS(v.ConsoleHTTPS); herr != nil {
			v.ConsoleHTTPS = prevConsoleHTTPS
			_ = s.saveUI(v)
			err = herr
		}
	}
	s.uiMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if v.SdHighSpeed != prevSdHighSpeed && boardCapabilities(imagePrefix(deviceCompatible())).SDHighSpeed {
		if _, serr := s.applySDHighSpeed(v.SdHighSpeed); serr != nil {
			s.uiMu.Lock()
			v.SdHighSpeed = prevSdHighSpeed
			_ = s.saveUI(v)
			s.uiMu.Unlock()
			fail(w, 500, serr)
			return
		}
	}
	if v.ShadowMode != prevShadowMode {
		// Shadow Deck Mode switches live (#201): answer the held shadow long poll
		// now, in the new mode, instead of after its wait.
		s.shadow.mu.Lock()
		s.shadow.bump()
		s.shadow.mu.Unlock()
	}
	if hasBroadcast {
		if err := s.SetBroadcastEnabled(bcastOn); err != nil {
			fail(w, 400, err)
			return
		}
	}
	if q.Get("restart") == "1" {
		if err := s.restartEngine(); err != nil {
			log.Printf("stream-sync: restart Engine: %v", err)
			fail(w, 500, errors.New("settings saved, but Engine did not restart"))
			return
		}
	}
	s.uiStatus(w, r)
}

// quality is the download quality the owner chose. It applies from the next download.
func (s *Server) quality() string {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().Quality
}

// neteaseDirect reports whether NetEase tracks skip Deemix.
func (s *Server) neteaseDirect() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().NeteaseMode == neteaseOfficial
}

// validPlayhead takes Engine's normalised slider value.
func validPlayhead(f float64) bool { return f >= 0 && f <= 1 }

func ptrTo(v string) *string { return &v }

func (s *Server) restartEngine() error {
	if s.cfg.RestartEngine != nil {
		return s.cfg.RestartEngine()
	}
	if err := guardHostCommand("systemctl"); err != nil {
		return err
	}
	return exec.Command("systemctl", "restart", "--no-block", "engine.service").Run()
}

func (s *Server) remoteLoadEnabled() bool {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().RemoteLoad
}

func (s *Server) sdHighSpeedRunDir() string {
	if s.cfg.SDHighSpeedRunDir != "" {
		return s.cfg.SDHighSpeedRunDir
	}
	return "/run/enginex"
}

func (s *Server) sdHighSpeedPending(want bool) bool {
	runDir := s.sdHighSpeedRunDir()
	b, err := os.ReadFile(filepath.Join(runDir, "sd-highspeed.state"))
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(b)) != "deferred" {
		return false
	}
	meta, err := os.ReadFile(filepath.Join(runDir, "sd-highspeed.deferred"))
	if err != nil {
		return true
	}
	lines := strings.SplitN(strings.TrimSpace(string(meta)), "\n", 2)
	if len(lines) == 2 && lines[1] != "" {
		if _, err := os.Stat(lines[1]); err != nil && os.IsNotExist(err) {
			return false
		}
	}
	if len(lines) >= 1 && lines[0] != "" {
		if ios, err := os.ReadFile(filepath.Join("/sys/kernel/debug", lines[0], "ios")); err == nil {
			if strings.Contains(string(ios), "sd high-speed") == want {
				return false
			}
		}
	}
	return true
}

func (s *Server) anyDeckPlaying() bool {
	now := time.Now()
	if s.shadow != nil {
		s.shadow.mu.Lock()
		switch {
		case len(s.shadow.own) > 0 && now.Sub(s.shadow.ownAt) < 2*time.Second:
			for _, d := range s.shadow.own {
				if d.Playing {
					s.shadow.mu.Unlock()
					return true
				}
			}
		case len(s.shadow.clockDecks) > 0 && now.Sub(s.shadow.clockAt) < 2*time.Second:
			for _, d := range s.shadow.clockDecks {
				if d.Playing {
					s.shadow.mu.Unlock()
					return true
				}
			}
		}
		for _, p := range s.shadow.peerDecks {
			if !p.Stale && now.Sub(p.at) < 2*time.Second && p.Playing {
				s.shadow.mu.Unlock()
				return true
			}
		}
		s.shadow.mu.Unlock()
	}
	for _, dev := range s.mergedDevices() {
		if dev.Number < 1 || dev.Number > 4 || strings.HasPrefix(dev.Name, "EngineX") || now.Sub(dev.LastSeen) >= 2*time.Second {
			continue
		}
		if dev.Playing || dev.PlayState1 == cdjCuePlay {
			return true
		}
	}
	return false
}

func (s *Server) applySDHighSpeed(enabled bool) (bool, error) {
	playing := s.anyDeckPlaying()
	if s.cfg.ApplySDHighSpeed != nil {
		return s.cfg.ApplySDHighSpeed(enabled, playing)
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "enginex-sd-highspeed"))
	}
	candidates = append(candidates, "/data/enginex/bundle/bin/enginex-sd-highspeed", "/usr/local/bin/enginex-sd-highspeed")
	script := ""
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			script = c
			break
		}
	}
	if script == "" || guardHostCommand(script) != nil {
		return false, nil
	}
	want := "0"
	if enabled {
		want = "1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "apply", want)
	env := append(os.Environ(),
		"ENGINEX_UI_DIR="+s.uiDir(),
		"ENGINEX_RUN_ROOT="+s.sdHighSpeedRunDir(),
	)
	if playing {
		env = append(env, "ENGINEX_DECK_PLAYING=1")
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return false, errors.New(msg)
	}
	return s.sdHighSpeedPending(enabled), nil
}

// spotifySource returns the owner's selected matching service for Spotify audio.
func (s *Server) spotifySource() string {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().SpotifySource
}
