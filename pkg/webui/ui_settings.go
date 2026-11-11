package webui

// Deck UI settings read by the QML in the UI packs (tools/ui-mod) and by
// engine_ui_hook.c, which only understands this key=value file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"

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
		StemsEq:       !isPlayer && board != "PRIME4G2",
		LineInAux:     board == "SCLIVE4",
		Gen2:          dataBundleBoards[board],
		RemoteStems:   !isAZ04,
	}
}

const defaultUIDir = "/data/enginex/ui"

var (
	uiLanguages    = []string{"en", "zh-Hans", "zh-Hant"}
	uiWaveforms    = []string{"denon", "rgb", "3band"}
	uiServices     = []string{"tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}
	uiQualities    = []string{qualityFLAC, quality320k}
	uiNeteaseModes = []string{neteaseOfficial, neteaseDeemix}
	// TIDAL logs in through Engine's own flow against TIDAL's servers.
	loginServices = []string{"netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}

	uiStreamSortRoles  = []string{"default", "bpm", "key", "rating", "song", "artist", "length"}
	uiStreamSortOrders = []string{"asc", "desc"}
	uiSyncGuardPcts    = []int{0, 10, 15, 20}
	// UI Scale presets: the QML mod scales the loaded UI live (EmbeddedMain uiLoader).
	uiUIScales       = []int{110, 105, 100, 95, 90, 85, 80, 75}
	uiPioneerFxModes = []string{"off", "spiral", "pitch_echo", "slip_roll", "dub_echo", "space"}
	uiShadowModes    = []string{"auto", "engine", "cdj"}
	uiFxTimeModes    = []string{"beats", "ms"}
)

const (
	qualityFLAC = "flac"
	quality320k = "320k"

	// NetEase tracks download from NetEase itself, which works with nothing but a
	// NetEase login. Deemix mode tries the deck's Deemix first to spare the
	// account's monthly download quota, and falls back to NetEase.
	neteaseOfficial = "official"
	neteaseDeemix   = "deemix"
)

type uiSettings struct {
	Version     string  `json:"version,omitempty"`
	Quality     string  `json:"quality"`
	NeteaseMode string  `json:"neteaseMode"`
	Playhead    float64 `json:"playhead"`
	// AutoAnalyze retains the user's preference (#55). The UI pack currently
	// fails closed: no automatic analysis until a bounded native adapter is verified.
	AutoAnalyze bool `json:"autoAnalyze"`
	// MasterLimiter turns on the soft clipper / peak limiter engine_ui_hook.c
	// applies to Engine's audio output (#58). Off by default.
	MasterLimiter bool `json:"masterLimiter"`
	// AutoGain raises quiet material toward 0 dBFS (at most +9 dB) ahead of the
	// limiter, which then always runs (#64). Off by default.
	AutoGain bool `json:"autoGain"`
	// OmniSource adds every mounted drive's playlists and tracks, and the other
	// services' playlists, to the streaming browser (#62). Off by default.
	OmniSource bool `json:"omniSource"`
	// ProlinkShare lets rb-bridge read the Rekordbox library of Pioneer
	// players on the LAN (#59). Off by default.
	ProlinkShare     bool     `json:"prolinkShare"`
	ProlinkExport    bool     `json:"prolinkExport"` // experimental Denon -> CDJ export; default OFF
	Language         string   `json:"language"`
	Waveform         string   `json:"waveform"`
	Sources          []string `json:"sources"`
	Scope            string   `json:"scope"`
	GitHubRepo       string   `json:"githubRepo"`
	SafeMode         bool     `json:"safeMode"`
	LoggedIn         []string `json:"loggedIn"`
	FourDeckWaveform bool     `json:"fourDeckWaveform"`
	FourDeckRole     string   `json:"fourDeckRole"`
	ShadowMode       string   `json:"shadowMode"` // which path draws a shadow deck: auto = Engine loads every source whose file this player can have and stock components draw it (EngineX peer; CDJ playing from USB or SD), a CDJ without such a file goes to the display renderer, engine = Engine loads every source, cdj = display renderer for every source
	ProlinkWaveform  bool     `json:"prolinkWaveform"`
	BroadcastEnabled bool     `json:"broadcastEnabled"`
	UpfaderEcho      bool     `json:"upfaderEcho"`
	// PairedView (#201): a standalone player in the 4-deck layout shows Engine's own
	// 2-deck view instead, this unit's active layer and the other player's active
	// deck (its layer followed). Needs FourDeckWaveform: setting it turns that on,
	// turning that off clears it.
	PairedView bool `json:"pairedView"`
	// DebugTelemetry logs every shadow deck update and decision (shadow_debug.go).
	// Development only: off by default, and nothing in the image turns it on.
	DebugTelemetry bool `json:"debugTelemetry"`
	VerboseLog     bool `json:"verboseLog"`

	StreamSortRole    string `json:"streamSortRole,omitempty"`
	StreamSortOrder   string `json:"streamSortOrder,omitempty"`
	BrowserDualColumn bool   `json:"browserDualColumn"`
	TracklistWaveform bool   `json:"tracklistWaveform"`
	CueCountdown      bool   `json:"cueCountdown"`
	SyncGuardPct      int    `json:"syncGuardPct"`
	SwapLoopOrder     bool   `json:"swapLoopOrder"`
	LedBrightness     int    `json:"ledBrightness"`
	// UIScale enlarges or shrinks Engine's whole interface; it takes effect at the next
	// Engine start, so the Control Center row asks before applying it.
	UIScale             int    `json:"uiScale"`
	StemsEqMode         bool   `json:"stemsEqMode"`
	BeatJumpPads        bool   `json:"beatJumpPads"`
	PioneerFx           string `json:"pioneerFx"`
	FXSelectorAvailable bool   `json:"fxSelectorAvailable"`
	FxTimeMode          string `json:"fxTimeMode"`
	FxTimeMs            int    `json:"fxTimeMs"`

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
}

// uiFourDeckRoles are the decks a player plays in the 4-deck layout (#49/#160), RCA A then B.
// auto: 13 for an odd Engine player number, 24 for an even one (engine-ui-guard), or 12 on an all-in-one console.
var uiFourDeckRoles = []string{"auto", "13", "24", "12", "34", "1", "2"}

// fourDeckRolePath is where engine-ui-guard leaves the role it applied.
var fourDeckRolePath = "/run/sc5000-assignments/role"

// uiScaleEnvPath records the scale as SC5000_UI_SCALE for Engine's unit (the
// engine.service.d/70-ui-scale.conf drop-in). It is deliberately not QT_SCALE_FACTOR,
// which leaves a black band under EGLFS. A package var so tests can point it at a temp dir.
var (
	uiScaleMu      sync.Mutex
	uiScaleEnvPath = "/data/sc5000-custom/ui-scale.env"
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
	text := "SC5000_UI_SCALE=" + strconv.FormatFloat(float64(pct)/100, 'f', -1, 64) + "\n"
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
	if _, err := os.Stat(defaultUIDir); os.IsNotExist(err) {
		if _, legacyErr := os.Stat("/data/sc5000-custom/ui"); legacyErr == nil {
			return "/data/sc5000-custom/ui"
		}
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
		Language: "en", Waveform: "denon", Sources: slices.Clone(uiServices), Scope: "deezer", Quality: qualityFLAC, NeteaseMode: neteaseOfficial, GitHubRepo: defaultGitHubRepo, FourDeckWaveform: false, FourDeckRole: "auto", ShadowMode: "auto", ProlinkWaveform: true,
		TracklistWaveform: true, CueCountdown: true,
		StreamSortRole: "default", StreamSortOrder: "asc", LedBrightness: 100, PioneerFx: "off", UIScale: 100,
		FxTimeMode: "beats", FxTimeMs: 500,
	}
	b, _ := os.ReadFile(filepath.Join(s.uiDir(), "settings.conf"))
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
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
			if slices.Contains(uiServices, val) {
				v.Scope = val
			}
		case "quality":
			if slices.Contains(uiQualities, val) {
				v.Quality = val
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
		case "omni_source":
			v.OmniSource = val == "1"
		case "prolink_share":
			v.ProlinkShare = val == "1"
		case "prolink_export":
			v.ProlinkExport = val == "1"
		case "known":
			known = strings.Split(val, ",")
		case "four_deck_role":
			if slices.Contains(uiFourDeckRoles, val) {
				v.FourDeckRole = val
			}
		case "paired_view":
			v.PairedView = val == "1"
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
		case "browser_dual_column":
			v.BrowserDualColumn = val == "1" || strings.EqualFold(val, "true")
		case "tracklist_waveform":
			if val == "0" || strings.EqualFold(val, "false") {
				v.TracklistWaveform = false
			} else if val == "1" || strings.EqualFold(val, "true") {
				v.TracklistWaveform = true
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
		if name != "" && !slices.Contains(uiServices, name) {
			return nil, errors.New("unknown source: " + name)
		}
	}
	return list, nil
}

func (s *Server) saveUI(v uiSettings, exportRequested ...bool) (err error) {
	was := false
	if s.prolinkExport != nil && (v.ProlinkExport != s.loadUI().ProlinkExport || len(exportRequested) > 0 && exportRequested[0]) {
		was = s.prolinkExport.Status().Enabled
		if err = s.prolinkExport.SetEnabled(v.ProlinkExport); err != nil {
			if !v.ProlinkExport {
				return err
			}
			// ON stays ON without a Rekordbox USB; retryExport starts it once one is mounted.
			log.Printf("stream-sync: Pro DJ Link export waits: %v", err)
			err = nil
		}
		defer func() {
			if err != nil && was != v.ProlinkExport {
				_ = s.prolinkExport.SetEnabled(was)
			}
		}()
	}
	dir := s.uiDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	text := "lang=" + v.Language + "\nwaveform=" + v.Waveform + "\nsources=" + strings.Join(v.Sources, ",") + "\nknown=" + strings.Join(uiServices, ",") + "\nscope=" + v.Scope + "\nquality=" + v.Quality + "\nnetease_mode=" + v.NeteaseMode + "\nplayhead=" + strconv.FormatFloat(v.Playhead, 'f', -1, 64) + "\n"
	if v.FourDeckWaveform {
		text += "four_deck_waveform=1\n"
		if v.PairedView {
			text += "paired_view=1\n"
		}
	}
	if v.ShadowMode != "auto" {
		text += "shadow_mode=" + v.ShadowMode + "\n"
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
	if v.OmniSource {
		text += "omni_source=1\n"
	}
	if v.ProlinkShare {
		text += "prolink_share=1\n"
	}
	if v.ProlinkExport {
		text += "prolink_export=1\n"
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
	if v.BrowserDualColumn {
		text += "browser_dual_column=1\n"
	}
	if !v.TracklistWaveform {
		text += "tracklist_waveform=0\n"
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
	if !s.fxSelectorAvailable() && v.PioneerFx != "" && v.PioneerFx != "off" {
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
	if err = os.Rename(tmp.Name(), filepath.Join(dir, "settings.conf")); err != nil {
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

// scope is the service the deck's native browser currently shows.
func (s *Server) scope() string {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	return s.loadUI().Scope
}

func (s *Server) uiStatus(w http.ResponseWriter, r *http.Request) {
	s.uiMu.Lock()
	v := s.loadUI()
	v.FXSelectorAvailable = s.fxSelectorAvailable()
	if v.FXSelectorAvailable {
		if err := s.migratePioneerFX(); err != nil {
			log.Printf("FX selector settings migration: %v", err)
		}
		v.PioneerFx = "off"
	}
	s.uiMu.Unlock()
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
	relay := s.relayStatusView()
	v.RelayURL, v.DeemixPasswordSet = relay.RelayURL, relay.PasswordSet
	v.ProlinkExportActive = s.prolinkExport != nil && s.prolinkExport.Status().Enabled
	board := imagePrefix(deviceCompatible())
	v.Board = board
	v.BuildTag = currentBuildTag()
	v.Player = playerBoards[board]
	v.Capabilities = boardCapabilities(board)
	v.FourDeckRoleActive = s.activeRole(v)
	v.TimeZone = s.savedTimeZone()
	if r.Method == http.MethodGet && r.URL.Path == "/v1/sc5000/ui" {
		body, err := json.Marshal(struct {
			uiSettings
			AccessCode string `json:"accessCode"`
			ConsoleURL string `json:"consoleUrl"`
			// LANStems: the home sc5000-stemsd last seen, for Settings -> About (#148).
			LANStems *stems.LANState `json:"lanStems,omitempty"`
		}{v, s.cfg.AccessToken, "http://" + lanHost() + ":8080", relay.LANStems})
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
	if q.Has("fxUnit") || q.Has("fxMode") {
		s.fxSelectorUpdate(w, r, q)
		return
	}
	if q.Has("timezone") && len(q) == 1 {
		s.timezoneUpdate(w, q.Get("timezone"))
		return
	}
	if r.Body != nil && strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for k, v := range body {
				if !q.Has(k) {
					q.Set(k, fmt.Sprint(v))
				}
			}
		}
	}
	if q.Has("relayUrl") && !validRelayURL(q.Get("relayUrl")) {
		fail(w, 400, errInvalidRelayURL)
		return
	}
	s.uiMu.Lock()
	v := s.loadUI()
	prevShadowMode := v.ShadowMode
	bad := func(field, val string, allowed []string) bool {
		return q.Has(field) && !slices.Contains(allowed, val)
	}
	if bad("lang", q.Get("lang"), uiLanguages) || bad("waveform", q.Get("waveform"), uiWaveforms) || bad("scope", q.Get("scope"), uiServices) || bad("quality", q.Get("quality"), uiQualities) || bad("neteaseMode", q.Get("neteaseMode"), uiNeteaseModes) || bad("fourDeckRole", q.Get("fourDeckRole"), uiFourDeckRoles) || bad("streamSortRole", q.Get("streamSortRole"), uiStreamSortRoles) || bad("streamSortOrder", q.Get("streamSortOrder"), uiStreamSortOrders) || bad("pioneerFx", q.Get("pioneerFx"), uiPioneerFxModes) || bad("shadowMode", q.Get("shadowMode"), uiShadowModes) || bad("fxTimeMode", q.Get("fxTimeMode"), uiFxTimeModes) {
		s.uiMu.Unlock()
		fail(w, 400, errors.New("invalid setting"))
		return
	}
	if q.Has("shadowMode") {
		v.ShadowMode = q.Get("shadowMode")
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
		}
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
	if q.Has("cueCountdown") {
		b, ok := parseBoolParam(q.Get("cueCountdown"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.CueCountdown = b
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
	if q.Has("scope") {
		v.Scope = q.Get("scope")
	}
	if q.Has("quality") {
		v.Quality = q.Get("quality")
	}
	if q.Has("fourDeckRole") {
		v.FourDeckRole = q.Get("fourDeckRole")
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
	if q.Has("omniSource") {
		on := q.Get("omniSource")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.OmniSource = on == "1"
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
	if q.Has("prolinkExport") {
		on := q.Get("prolinkExport")
		if on != "0" && on != "1" {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.ProlinkExport = on == "1"
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
	err := s.saveUI(v, q.Has("prolinkExport"))
	if err != nil && q.Has("verboseLog") {
		_ = writeVerboseLog(previousVerbose)
	}
	s.uiMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
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
	return exec.Command("systemctl", "restart", "--no-block", "engine.service").Run()
}
