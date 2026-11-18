package webui

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Hook features (tools/ui-mod/hook_features.h): EngineX parts of the Engine hook a deck
// can switch off. The console's feature menu writes hook-disable.conf; the hook reads it
// when Engine starts and publishes what it really runs with in hookFeatureStatePath.

// hookFeature: Name as in hook_features.h, Group for the menu, Reboot when the change
// takes effect only after the deck restarts (the edisksd preload, boot-activate.sh).
// Service: an EngineX service the boot script leaves out, not a hook site (the hook
// ignores the name). Needs: features it is useless without; off with any of them.
type hookFeature struct {
	Name    string
	Group   string
	Reboot  bool
	Gen2    bool // only second-generation bundles know the switch
	Service bool
	Needs   []string
	About   string
}

// hookFeatures lists every name of ENGINEX_HOOK_FEATURES in hook_features.h
// (TestHookFeatureNamesMatchTheHook), grouped as the menu shows them.
var hookFeatures = []hookFeature{
	{Name: "sso", Group: "streaming", About: "Engine believes an Engine DJ profile is connected (#159); needed with gateway"},
	{Name: "gateway", Group: "streaming", Needs: []string{"sso"}, About: "Engine's TIDAL addresses point at stream-sync (NetEase/Spotify/Deezer/SoundCloud/Beatport/Apple Music shown as TIDAL)"},
	{Name: "deemixd", Group: "streaming", Reboot: true, Service: true, About: "EngineX service: Deezer download queue for the console's Deezer jobs and Deezer tracks loaded through the gateway"},
	{Name: "apple-music", Group: "streaming", About: "unlocks Engine's built-in Apple Music source"},
	{Name: "stems", Group: "engine", About: "Stems unlocked, Stems list, EngineX stems-processor (local/LAN/relay)"},
	{Name: "bluetooth", Group: "engine", About: "config: hasBluetooth / send Bluetooth to assignment"},
	{Name: "stems-eq", Group: "engine", About: "#152: 3-band Stems level EQ (ChannelStrip)"},
	{Name: "sweep-fx", Group: "engine", About: "#166: full Sweep FX choice on Mixstream consoles"},
	{Name: "zoom30", Group: "engine", About: "#77: waveform zooms out to 30 s (stock all-in-ones 10 s)"},
	{Name: "rec-streaming", Group: "engine", About: "#206: load and record streaming tracks during a recording"},
	{Name: "aux-input", Group: "engine", About: "#171: AUX/MIC2 input and mixer menu of the SC LIVE 4"},
	{Name: "four-deck", Group: "engine", About: "#49: 4-deck layout on players, MASTER rule for shadow decks (Gen-1 5.3.0)"},
	{Name: "limiter-menu", Group: "engine", About: "shows the hidden stock Master Limiter menu"},
	{Name: "filesystems", Group: "drives", About: "unknown file systems and HFS+ allowed, config disableHfsPlus=0"},
	{Name: "readonly-sticks", Group: "drives", About: "write-protected sticks are not ejected or reported read-only"},
	{Name: "mount-hook", Group: "drives", Reboot: true, Gen2: true, About: "edisksd preload: converts rekordbox exports and repairs Engine libraries on sticks"},
	{Name: "internal-heal", Group: "drives", About: "engine-guardian before each Engine start: the internal library's m.db without reserved page bytes (Engine 5.3.0 cannot attach other libraries otherwise)"},
	{Name: "dsp-guards", Group: "audio", About: "21 DSP guards and the master chain: Echo Out, Pioneer FX, Hi-Fi Output, Auto-Gain, Soft Clipper, Live Stream tap"},
	{Name: "hifi-output", Group: "audio", Needs: []string{"dsp-guards"}, About: "#172: 48->96 kHz Hermite output roll-off compensation before the soft limiter on Gen-1 players (SC5000/SC6000)"},
	{Name: "reverb-tail", Group: "audio", About: "#207: the stock Reverb (Classic) decays up to about 28 s above 82 % of its knob, all-in-ones only"},
	{Name: "alsa-timestamp", Group: "audio", About: "ALSA time stamps GETTIMEOFDAY -> MONOTONIC_RAW"},
	{Name: "thread-tuning", Group: "audio", About: "Engine background threads and OfflineAnalyzer at lower priority, off CPU 1/2"},
	{Name: "sqlite-tuning", Group: "data", About: "busy_timeout, synchronous=NORMAL, temp_store=MEMORY, cache_size for every Engine database"},
	{Name: "telemetry-block", Group: "data", About: "PostHog/events databases in RAM, crash reports to /tmp, no Crashpad"},
	{Name: "vendor-update-block", Group: "data", About: "autoupdate.airmusictech.com is not resolved (no inMusic firmware offers)"},
	{Name: "ui-packs", Group: "ui", About: "EngineX interface (QML packs): all EngineX menus, About, Remote Load; off = stock interface"},
	{Name: "shadow-paused-follow", Group: "ui", About: "a shadow deck stays paused and its waveform line follows the source deck (first-generation Engine 5.3.0); off = the shadow deck plays muted as before"},
	{Name: "rb-bridge", Group: "services", Reboot: true, Service: true, About: "EngineX service: Pro DJ Link (players' announcements and status for CDJ shadow decks, library sharing); CDJ export is the console's, rekordbox Zero-Scan mount-hook's"},
}

func hookFeatureNamed(name string) (hookFeature, bool) {
	i := slices.IndexFunc(hookFeatures, func(f hookFeature) bool { return f.Name == name })
	if i < 0 {
		return hookFeature{}, false
	}
	return hookFeatures[i], true
}

// hookFeaturesOffWithNeeds adds every feature whose need is off; hookFeatures lists a
// need before the features that need it, so one pass in that order is enough.
func hookFeaturesOffWithNeeds(off []string) []string {
	out := slices.Clone(off)
	for _, f := range hookFeatures {
		if slices.Contains(out, f.Name) {
			continue
		}
		for _, n := range f.Needs {
			if slices.Contains(out, n) {
				out = append(out, f.Name)
				break
			}
		}
	}
	return out
}

// hookFeatureDepth: how far the menu indents a feature, one step per need it hangs under.
func hookFeatureDepth(f hookFeature) int {
	d := 0
	for _, n := range f.Needs {
		if g, ok := hookFeatureNamed(n); ok {
			d = max(d, hookFeatureDepth(g)+1)
		}
	}
	return d
}

// Vars so tests do not touch the deck's files.
var (
	hookDisablePath      = "/data/enginex/hook-disable.conf"
	hookFeatureStatePath = "/run/enginex/hook-features"
	// engineAlive tells whether pid is the Engine that wrote the state file; a stale
	// file from an Engine that has since stopped (safe mode, stock Engine) says nothing.
	engineAlive = func(pid int) bool { return engineAliveIn("/proc", pid) }
)

// engineAliveIn: pid runs Engine's executable. Not by comm: Engine 5.3.0 names its main
// thread EMain, so comm reads EMain there. Under qemu-user (binfmt, the x86 gate host)
// the interpreter comes first and the program second.
func engineAliveIn(proc string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	argv0, rest, _ := strings.Cut(string(b), "\x00")
	if strings.HasPrefix(filepath.Base(argv0), "qemu-") {
		argv0, _, _ = strings.Cut(rest, "\x00")
	}
	return filepath.Base(argv0) == "Engine"
}

// HookFeatureOff reports whether hook-disable.conf switches name off. stream-sync
// asks it for the services it starts itself (first generation: deemixd).
func HookFeatureOff(name string) bool {
	b, err := os.ReadFile(hookDisablePath)
	return err == nil && slices.Contains(hookFeaturesOffWithNeeds(parseHookFeatureNames(string(b))), name)
}

func knownHookFeature(name string) bool {
	return slices.ContainsFunc(hookFeatures, func(f hookFeature) bool { return f.Name == name })
}

// parseHookFeatureNames reads names the way the hook does: comma, space or line
// separated, '#' starts a comment. Unknown names are dropped.
func parseHookFeatureNames(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, name := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' }) {
			if knownHookFeature(name) && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// hookRunState is what the running Engine's hook published; ok is false when no
// Engine with the hook runs (or the hook predates the state file).
type hookRunState struct {
	build, off, on []string
	ok             bool
}

func readHookRunState() hookRunState {
	b, err := os.ReadFile(hookFeatureStatePath)
	if err != nil {
		return hookRunState{}
	}
	var st hookRunState
	pid := 0
	for _, line := range strings.Split(string(b), "\n") {
		key, val, _ := strings.Cut(line, "=")
		switch key {
		case "pid":
			pid, _ = strconv.Atoi(val)
		case "build":
			st.build = parseHookFeatureNames(val)
		case "off":
			st.off = parseHookFeatureNames(val)
		case "on":
			st.on = parseHookFeatureNames(val)
		}
	}
	st.build = hookFeaturesOffWithNeeds(st.build)
	st.off = hookFeaturesOffWithNeeds(st.off)
	st.ok = pid > 0 && engineAlive(pid)
	return st
}

// runningFeatureOn reports that the running Engine's hook says it does name. A hook of
// before a feature existed does not list it as off either, so not being off is no proof;
// the paused shadow follow needs one (a UI pack that pauses the copy under a hook that does
// not move its line shows a standing waveform).
func runningFeatureOn(name string) bool {
	st := readHookRunState()
	return st.ok && slices.Contains(st.on, name) && !slices.Contains(st.off, name)
}

// runningFeaturesOff is the list QML hides rows by; nil when unknown, so every row stays.
func runningFeaturesOff() []string {
	st := readHookRunState()
	if !st.ok {
		return nil
	}
	off := slices.Clone(st.off)
	if off == nil {
		off = []string{}
	}
	// Services are not hook sites: the file says whether the boot script started them.
	if b, err := os.ReadFile(hookDisablePath); err == nil {
		for _, name := range parseHookFeatureNames(string(b)) {
			if f, _ := hookFeatureNamed(name); f.Service && !slices.Contains(off, name) {
				off = append(off, name)
			}
		}
	}
	return off
}

type hookFeatureView struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	Reboot bool   `json:"reboot,omitempty"`
	// Off: hook-disable.conf names it. Build: this build left it out, the file cannot
	// bring it back. Running: on or off in the Engine that runs now, "" when unknown.
	Off     bool   `json:"off"`
	Build   bool   `json:"build,omitempty"`
	Running string `json:"running"`
	// Needs: the features it needs; Blocked: one of them is off, so this one is too
	// and the menu greys it out; Depth: how far the menu indents it under its need.
	Needs   []string `json:"needs,omitempty"`
	Blocked bool     `json:"blocked,omitempty"`
	Depth   int      `json:"depth,omitempty"`
	// Along: off only because its need is off; it comes back on with the need.
	Along bool `json:"along,omitempty"`
}

type hookFeaturesView struct {
	Features []hookFeatureView `json:"features"`
	// EngineKnown: an Engine with the hook runs and said what it runs with.
	EngineKnown bool `json:"engineKnown"`
	// RestartNeeded: the file differs from the running Engine (restart Engine to apply).
	RestartNeeded bool `json:"restartNeeded"`
	// RebootNeeded: a reboot-required feature differs from Engine's start snapshot.
	RebootNeeded bool `json:"rebootNeeded"`
}

func (s *Server) hookFeaturesView() hookFeaturesView {
	var file, along []string
	if b, err := os.ReadFile(hookDisablePath); err == nil {
		file, along = parseHookFeatureNames(string(b)), hookFeaturesAlong(string(b))
		for _, name := range hookFeaturesOffWithNeeds(file) {
			if !slices.Contains(file, name) {
				along = append(along, name)
			}
		}
		file = hookFeaturesOffWithNeeds(file)
	}
	st := readHookRunState()
	gen2 := dataBundleBoards[imagePrefix(deviceCompatible())] || !onDeck()
	v := hookFeaturesView{Features: []hookFeatureView{}, EngineKnown: st.ok}
	for _, f := range hookFeatures {
		if f.Gen2 && !gen2 {
			continue
		}
		fv := hookFeatureView{Name: f.Name, Group: f.Group, Reboot: f.Reboot,
			Off: slices.Contains(file, f.Name), Build: slices.Contains(st.build, f.Name),
			Needs: f.Needs, Depth: hookFeatureDepth(f), Along: slices.Contains(along, f.Name)}
		for _, n := range f.Needs {
			if slices.Contains(file, n) || slices.Contains(st.build, n) {
				fv.Blocked = true
			}
		}
		if st.ok && !f.Service {
			runningOff := slices.Contains(st.off, f.Name)
			fv.Running = "on"
			if runningOff {
				fv.Running = "off"
			}
			if !f.Reboot && runningOff != (fv.Off || fv.Build) {
				v.RestartNeeded = true
			}
		}
		if st.ok && f.Reboot && slices.Contains(st.off, f.Name) != (fv.Off || fv.Build) {
			v.RebootNeeded = true
		}
		v.Features = append(v.Features, fv)
	}
	return v
}

// hookAlongMark starts the comment of a feature that is off only with its need.
const hookAlongMark = "# off with "

// hookFeaturesAlong: the names switched off in text whose line carries hookAlongMark.
func hookFeaturesAlong(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && knownHookFeature(name) && strings.HasPrefix(strings.TrimSpace(rest), hookAlongMark) {
			names = append(names, name)
		}
	}
	return names
}

// hookFeaturesNeedsOff: whether one of the features f needs is in off.
func hookFeaturesNeedsOff(f hookFeature, off []string) bool {
	return slices.ContainsFunc(f.Needs, func(n string) bool { return slices.Contains(off, n) })
}

// hookDisableFile is the whole hook-disable.conf for these names: every feature is
// listed with what it does, the ones switched off as plain lines.
func hookDisableFile(off []string) string { return hookDisableFileAlong(off, nil) }

// hookDisableFileAlong also marks the features in along as off only with their need.
func hookDisableFileAlong(off, along []string) string {
	var b strings.Builder
	b.WriteString("# hook-disable.conf: EngineX features switched off on this deck.\n")
	b.WriteString("# Written by the web console (Settings -> Features); edit it there or by hand.\n")
	b.WriteString("# One name per line switches that feature off; '#' starts a comment.\n")
	b.WriteString("# Engine features apply after an Engine restart, mount-hook after a reboot.\n")
	b.WriteString("# Features the build left out (ENGINEX_HOOK_DISABLE) cannot be switched on here.\n")
	group := ""
	for _, f := range hookFeatures {
		if f.Group != group {
			group = f.Group
			fmt.Fprintf(&b, "#\n# --- %s\n", group)
		}
		line := fmt.Sprintf("%-20s # %s", f.Name, f.About)
		if slices.Contains(along, f.Name) {
			line = fmt.Sprintf("%-20s %s%s: %s", f.Name, hookAlongMark, strings.Join(f.Needs, ", "), f.About)
		}
		if !slices.Contains(off, f.Name) {
			line = "# " + line
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func writeHookDisableFile(off []string) error { return writeHookDisableFileAlong(off, nil) }

func writeHookDisableFileAlong(off, along []string) error {
	if err := os.MkdirAll(filepath.Dir(hookDisablePath), 0o755); err != nil {
		return err
	}
	tmp := hookDisablePath + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(hookDisableFileAlong(off, along))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, hookDisablePath)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

type hookFeaturesPost struct {
	Off     []string `json:"off"`
	Restart bool     `json:"restart"`
}

// handleHookFeatures: GET the menu, POST {"off":[names],"restart":true} to write
// hook-disable.conf and restart Engine with it.
func (s *Server) handleHookFeatures(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		respond(w, s.hookFeaturesView())
		return
	}
	var in hookFeaturesPost
	if !input(w, r, &in) {
		return
	}
	off := []string{}
	for _, name := range in.Off {
		if !knownHookFeature(name) {
			fail(w, http.StatusBadRequest, fmt.Errorf("unknown feature %q", name))
			return
		}
		if !slices.Contains(off, name) {
			off = append(off, name)
		}
	}
	s.uiMu.Lock()
	// A feature that went off only with its need comes back with it, even when the
	// client still lists it; one switched off by hand stays off.
	var before, previous []string
	if b, err := os.ReadFile(hookDisablePath); err == nil {
		before = hookFeaturesAlong(string(b))
		previous = hookFeaturesOffWithNeeds(parseHookFeatureNames(string(b)))
	}
	off = slices.DeleteFunc(off, func(name string) bool {
		f, _ := hookFeatureNamed(name)
		return slices.Contains(before, name) && !hookFeaturesNeedsOff(f, off)
	})
	all := hookFeaturesOffWithNeeds(off)
	var along []string
	for _, name := range all {
		f, _ := hookFeatureNamed(name)
		if hookFeaturesNeedsOff(f, all) && (!slices.Contains(off, name) || slices.Contains(before, name)) {
			along = append(along, name)
		}
	}
	off = all
	rebootChanged := slices.ContainsFunc(hookFeatures, func(f hookFeature) bool {
		return f.Reboot && slices.Contains(previous, f.Name) != slices.Contains(off, f.Name)
	})
	err := writeHookDisableFileAlong(off, along)
	s.uiMu.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("stream-sync: hook features off: %s", strings.Join(off, ","))
	v := s.hookFeaturesView()
	v.RebootNeeded = v.RebootNeeded || rebootChanged
	if in.Restart && !v.RebootNeeded {
		if err := s.restartEngine(); err != nil {
			log.Printf("stream-sync: restart Engine: %v", err)
			fail(w, http.StatusInternalServerError, errors.New("features saved, but Engine did not restart"))
			return
		}
	}
	respond(w, v)
}
