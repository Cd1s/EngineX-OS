package webui

// Deck UI settings read by the QML in the UI packs (tools/ui-mod) and by
// engine_ui_hook.c, which only understands this key=value file.

import (
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const defaultUIDir = "/data/sc5000-custom/ui"

var (
	uiLanguages    = []string{"en", "zh-Hans", "zh-Hant"}
	uiWaveforms    = []string{"denon", "rgb", "3band"}
	uiServices     = []string{"tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}
	uiQualities    = []string{qualityFLAC, quality320k}
	uiJogModes     = []string{"static", "rainbow", "fast", "bpm"}
	uiNeteaseModes = []string{neteaseOfficial, neteaseDeemix}
	// TIDAL logs in through Engine's own flow against TIDAL's servers.
	loginServices = []string{"netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"}
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
	Jog         string  `json:"jog"`
	NeteaseMode string  `json:"neteaseMode"`
	Playhead    float64 `json:"playhead"`
	// AutoAnalyze has the UI pack start Engine's own analysis of every
	// unanalysed track whenever a drive is inserted (#55). Off by default.
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
	ProlinkWaveform  bool     `json:"prolinkWaveform"`
	BroadcastEnabled bool     `json:"broadcastEnabled"`
	UpfaderEcho      bool     `json:"upfaderEcho"`

	RelayURL          string `json:"relayUrl"`
	DeemixPasswordSet bool   `json:"deemixPasswordSet"`

	// Status only, never saved: the export runs (ON waits for a Rekordbox USB), and
	// the board is a standalone player without channel faders (#52).
	ProlinkExportActive bool `json:"prolinkExportActive"`
	Player              bool `json:"player"`
	// The role engine-ui-guard applied at Engine's last start (auto resolved).
	FourDeckRoleActive string `json:"fourDeckRoleActive,omitempty"`
}

// uiFourDeckRoles are the decks a player plays in the 4-deck layout (#49), RCA A then B.
// auto: 13 for an odd Engine player number, 24 for an even one (engine-ui-guard).
var uiFourDeckRoles = []string{"auto", "13", "24", "12", "1", "2"}

// fourDeckRolePath is where engine-ui-guard leaves the role it applied.
var fourDeckRolePath = "/run/sc5000-assignments/role"

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

// loadUI must be called with uiMu held. Bad or missing values get defaults.
func (s *Server) loadUI() uiSettings {
	known, saved := uiServicesBeforeKnown, false
	v := uiSettings{Language: "en", Waveform: "denon", Sources: slices.Clone(uiServices), Scope: "deezer", Quality: qualityFLAC, Jog: "static", NeteaseMode: neteaseOfficial, GitHubRepo: defaultGitHubRepo, FourDeckWaveform: false, FourDeckRole: "auto", ProlinkWaveform: true}
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
		case "jog":
			if slices.Contains(uiJogModes, val) {
				v.Jog = val
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
		case "four_deck_waveform":
			if val == "1" || strings.EqualFold(val, "true") {
				v.FourDeckWaveform = true
			} else if val == "0" || strings.EqualFold(val, "false") {
				v.FourDeckWaveform = false
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
	_, err := os.Stat(filepath.Join(s.uiDir(), "safe-mode"))
	v.SafeMode = err == nil
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
	text := "lang=" + v.Language + "\nwaveform=" + v.Waveform + "\nsources=" + strings.Join(v.Sources, ",") + "\nknown=" + strings.Join(uiServices, ",") + "\nscope=" + v.Scope + "\nquality=" + v.Quality + "\njog=" + v.Jog + "\nnetease_mode=" + v.NeteaseMode + "\nplayhead=" + strconv.FormatFloat(v.Playhead, 'f', -1, 64) + "\n"
	if v.FourDeckWaveform {
		text += "four_deck_waveform=1\n"
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
	return os.Rename(tmp.Name(), filepath.Join(dir, "settings.conf"))
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
	v.Player = playerBoards[imagePrefix(deviceCompatible())]
	v.FourDeckRoleActive = s.activeRole(v)
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
	if q.Has("relayUrl") && !validRelayURL(q.Get("relayUrl")) {
		fail(w, 400, errInvalidRelayURL)
		return
	}
	s.uiMu.Lock()
	v := s.loadUI()
	bad := func(field, val string, allowed []string) bool {
		return q.Has(field) && !slices.Contains(allowed, val)
	}
	if bad("lang", q.Get("lang"), uiLanguages) || bad("waveform", q.Get("waveform"), uiWaveforms) || bad("scope", q.Get("scope"), uiServices) || bad("quality", q.Get("quality"), uiQualities) || bad("jog", q.Get("jog"), uiJogModes) || bad("neteaseMode", q.Get("neteaseMode"), uiNeteaseModes) || bad("fourDeckRole", q.Get("fourDeckRole"), uiFourDeckRoles) {
		s.uiMu.Unlock()
		fail(w, 400, errors.New("invalid setting"))
		return
	}
	if q.Has("fourDeckWaveform") {
		b, ok := parseBoolParam(q.Get("fourDeckWaveform"))
		if !ok {
			s.uiMu.Unlock()
			fail(w, 400, errors.New("invalid setting"))
			return
		}
		v.FourDeckWaveform = b
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
	if q.Has("jog") {
		v.Jog = q.Get("jog")
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
	err := s.saveUI(v, q.Has("prolinkExport"))
	s.uiMu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	if hasBroadcast {
		if err := s.SetBroadcastEnabled(bcastOn); err != nil {
			fail(w, 400, err)
			return
		}
	}
	if q.Get("restart") == "1" {
		os.Remove(filepath.Join(s.uiDir(), "safe-mode"))
		os.Remove(filepath.Join(s.uiDir(), "boot-count"))
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
