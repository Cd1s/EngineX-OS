#!/usr/bin/env python3
"""Build the UI packs engine_ui_hook.c loads into Engine (see README.md)."""
import argparse
import json
import re
import shutil
import tarfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
# The deck skin: Skins/EmbeddedCommon up to 5.0.4, Skins/Embedded from 5.3.0 (set in main).
ROOT = "Skins/EmbeddedCommon/"
# Translated, and scanned for compared literals: the trees the 5.0.4 packs always covered.
TRANSLATED = ("Skins/EmbeddedCommon/", "Skins/Embedded/", "Skins/JP22SideScreen/", "UIKit.1/", "Skins/UIKit/")
DEV_HARNESS = """
	Timer {
		interval: 500; running: true; repeat: true
		property string last: ""
		onTriggered: {
			const x = new XMLHttpRequest()
			x.open("GET", "file:///tmp/ui-eval.js")
			x.onreadystatechange = function() {
				if (x.readyState !== 4 || !x.responseText) return
				const n = x.responseText.indexOf("\\n")
				const id = x.responseText.slice(0, n)
				if (id === last) return
				last = id
				try { console.warn("SCEVAL " + id + " = " + eval(x.responseText.slice(n + 1))) }
				catch (e) { console.warn("SCEVAL " + id + " ERR " + e + " " + e.lineNumber) }
			}
			x.send()
		}
	}
"""


def anchor_pattern(old: str) -> str:
    """old as a regex that ignores layout only: 5.3.0 ran qmlformat over every file
    (tabs -> spaces, `if(` -> `if (`, wrapped lines). Tokens and their order must
    still match, and words that were apart stay apart."""
    out, prev, gap = [], "", False
    for tok in re.findall(r"\s+|\w+|[^\w\s]", old):
        if tok.isspace():
            gap = True
            continue
        if out:
            out.append(r"\s+" if gap and (prev[-1:].isalnum() or prev[-1:] == "_") and (tok[0].isalnum() or tok[0] == "_") else r"\s*")
        out.append(re.escape(tok))
        prev, gap = tok, False
    head = r"(?m:^)[ \t]*" if old[:1] in " \t" else ""
    tail = r"[ \t]*\n" if old.endswith("\n") else ""
    return head + "".join(out) + tail


def patch(text: str, old: str, new: str, path: str) -> str:
    if text.count(old) == 1:
        return text.replace(old, new)
    found = list(re.finditer(anchor_pattern(old), text))
    if text.count(old) or len(found) != 1:
        raise SystemExit(f"{path}: expected exactly one match for {old!r}, got {max(text.count(old), len(found))}")
    m = found[0]
    return text[:m.start()] + new + text[m.end():]


SERVICES_MAIN = """
	// SC5000 UI mod: settings owned by stream-sync (see pkg/webui/ui_settings.go).
	property string scVersion: ""
	property string scLang: "en"
	property string scWave: "denon"
	property string scQuality: "flac"
	property string scNeteaseMode: "official"
	property string scJog: "static"
	property bool scMasterLimiter: false // #58, applied by engine_ui_hook.c
	property bool scAutoGain: false // #64, applied by engine_ui_hook.c
	// #55: once a drive is in, Engine's own analysis queue takes every unanalysed
	// track (rb-bridge's quick imports included). Its OfflineAnalyzer processes and
	// queue threads run at idle priority (engine_ui_hook.c), so playback keeps priority.
	property bool scAutoAnalyze: false
	property bool scOmniSource: false // #62, served by the streaming gateway
	readonly property var scDrives: Planck.librarianController.devicesController.deviceModelCountObserver
	readonly property int scDriveCount: scDrives ? scDrives.count : 0
	onScDriveCountChanged: scAutoAnalyzeTimer.restart()
	onScAutoAnalyzeChanged: scAutoAnalyzeTimer.restart()
	Timer {
		id: scAutoAnalyzeTimer
		interval: 15000 // after rb-bridge and Engine have attached the drive's library
		onTriggered: {
			const p = Planck.getProperty("/Client/Librarian/Analyzer/AnalyzeUnanalyzed")
			if (root.scAutoAnalyze && root.scDriveCount > 0 && p && p.translator) p.translator.state = true
		}
	}
	property bool scProlinkShare: false // #59, read by rb-bridge
	property bool scProlinkExport: false // #63, experimental export to CDJs
	// #49: the decks this player owns in the 4-deck layout ("13", "24", "12", "1", "2";
	// empty: no 4-deck layout). Engine refuses loading the others (LoadToDeckFilter).
	property string scFourDeckRole: ""
	// The board (device tree), not Config.device.isPlayer: the 4-deck layout turns a
	// player into a non-player inside Engine (#49).
	property bool scPlayer: false
	function scDeckLocked(deck) { return scFourDeckRole !== "" && scFourDeckRole.indexOf(String(deck)) < 0 }
	// Swipe left loads the role's first deck, swipe right its second (0: stock).
	function scSwipeDeck(right) { return scFourDeckRole === "" ? 0 : parseInt(scFourDeckRole[right ? scFourDeckRole.length - 1 : 0]) }
	property bool scFourDeckOn: false // the saved switch; Engine applies it on restart
	property string scFourDeckRoleSetting: "auto"
	// #99: five Layout Manager views. Engine has three and forgets the selected one
	// at every start, and the 4-deck layout used to force its lanes over the user's
	// view. stream-sync keeps the active view and each one's layout (views.json);
	// Engine starts on the active view. A view never saved has the performance pads
	// off, and in the 4-deck layout four decks and four waveform lanes. Changes in
	// the Layout Manager go to the active view; Save View stores it at once.
	property bool scUiLoaded: false // scApply ran: the 4-deck role is known
	property int scViewActive: 1
	property var scViews: ({})
	property bool scViewsRead: false
	property bool scViewReady: false // the active view applied after this start
	property real scViewQuiet: 0 // no autosave until an applied view has settled
	property string scViewLast: ""
	function scViewControls() {
		const c = PlanckGUI.views.performanceViewController
		if (!c || !c.deckController || !c.waveformController) return null
		return {deckStyle: c.deckController.deckStyleSetting, deckLayout: c.layoutController ? c.layoutController.layoutSetting : null,
			deckCount: c.deckController.deckCountSetting, waveformCount: c.waveformController.waveformCountSetting,
			waveformOrientation: c.waveformController.waveformOrientationSetting}
	}
	function scViewSnapshot() {
		const m = scViewControls()
		if (!m) return null
		const c = PlanckGUI.views.performanceViewController
		const s = {}
		for (const k in m) if (m[k] && m[k].currentName) s[k] = String(m[k].currentName)
		for (const [k, path] of [["pads", "/GUI/PadsViewEnabled"], ["fx", "/GUI/FxViewEnabled"]]) {
			const p = Planck.getProperty(path)
			if (p && p.translator) s[k] = !!p.translator.state
		}
		if (c.libraryView) s.library = !!c.libraryView.visibleSettingValue
		if (PlanckGUI.views.isPortrait) s.albumArt = !!c.deckController.albumArtVisible
		for (const [k] of scViewPrefs) s[k] = typeof root[k] === "boolean" ? root[k] : String(root[k])
		return s
	}
	// EngineX settings a view keeps too: [snapshot key = root property, /v1/sc5000/ui query].
	readonly property var scViewPrefs: [["scWave", "waveform"], ["scJog", "jog"], ["scUIScale", "uiScale"],
		["scDualColumn", "browserDualColumn"], ["scBeatJumpPads", "beatJumpPads"], ["scSwapLoopOrder", "swapLoopOrder"]]
	function scViewApply(s) {
		const m = scViewControls()
		if (!m || (scFourDeckRole !== "" && !(m.deckCount && m.deckCount.settingNames && m.deckCount.settingNames.length >= 2))) return false
		const c = PlanckGUI.views.performanceViewController
		for (const k in m) {
			const set = m[k], i = set && set.settingNames && typeof s[k] === "string" ? set.settingNames.indexOf(s[k]) : -1
			if (i >= 0 && set.currentName !== s[k]) set.setCurrentIndex(i)
		}
		for (const [k, path] of [["pads", "/GUI/PadsViewEnabled"], ["fx", "/GUI/FxViewEnabled"]]) {
			const p = Planck.getProperty(path)
			if (typeof s[k] === "boolean" && p && p.translator && !!p.translator.state !== s[k]) p.translator.state = s[k]
		}
		if (typeof s.library === "boolean" && c.libraryView && !!c.libraryView.visibleSettingValue !== s.library) c.libraryView.performanceLibraryEnableClicked()
		if (typeof s.albumArt === "boolean" && PlanckGUI.views.isPortrait && !!c.deckController.albumArtVisible !== s.albumArt) c.deckController.onDisplayAlbumArtClicked()
		const q = [] // a view saved before these keys keeps the current settings
		for (const [k, key] of scViewPrefs) {
			const want = s[k]
			if (typeof want === "boolean" && want !== !!root[k]) q.push(key + "=" + (want ? 1 : 0))
			else if (typeof want === "string" && want !== String(root[k])) q.push(key + "=" + encodeURIComponent(want))
		}
		if (q.length) scCall(q.join("&"))
		scViewQuiet = Date.now() + 3000
		scViewLast = ""
		return true
	}
	function scViewOf(n) {
		return scViews[String(n)] || (scFourDeckRole !== "" ? {pads: false, deckCount: "4 Decks", waveformCount: "4 Waveforms"} : {pads: false})
	}
	function scViewStore() {
		const x = new XMLHttpRequest()
		x.open("POST", "http://127.0.0.1:8080/v1/sc5000/views")
		x.send(JSON.stringify({active: scViewActive, views: scViews}))
	}
	function scViewSave() {
		const s = scViewSnapshot()
		if (!s) return
		const v = Object.assign({}, scViews)
		v[String(scViewActive)] = s
		scViews = v
		scViewLast = JSON.stringify(s)
		scViewStore()
	}
	function scViewSelect(n) {
		if (n === scViewActive) return
		scViewActive = n
		scViewApply(scViewOf(n))
		scViewStore()
	}
	Timer {
		interval: 2000; repeat: true; running: true
		onTriggered: {
			if (!root.scUiLoaded) return
			if (!root.scViewsRead) {
				root.scViewsRead = true
				const x = new XMLHttpRequest()
				x.open("GET", "http://127.0.0.1:8080/v1/sc5000/views")
				x.onreadystatechange = function() {
					if (x.readyState !== 4) return
					try {
						const v = JSON.parse(x.responseText)
						if (v.active >= 1 && v.active <= 5) root.scViewActive = v.active
						root.scViews = v.views || {}
					} catch (err) {
						root.scViewsRead = x.status === 200 // retry when stream-sync was not up
					}
				}
				x.send()
				return
			}
			if (!root.scViewReady) {
				if (root.scFourDeckRole !== "") {
					const lb = Planck.getProperty("/GUI/ShowLayerB")
					if (!lb || !lb.translator) return
					if (!lb.translator.state) lb.translator.state = true
				}
				root.scViewReady = root.scViewApply(root.scViewOf(root.scViewActive))
				return
			}
			if (Date.now() < root.scViewQuiet) return
			const s = root.scViewSnapshot()
			const j = s ? JSON.stringify(s) : ""
			if (!j) return
			if (root.scViewLast === "") root.scViewLast = j // what the applied view settled to
			else if (j !== root.scViewLast) root.scViewSave()
		}
	}
	// #49 shadow decks: post this player's decks for the other EngineX player and
	// drive the decks it does not play from GET /v1/sc5000/shadow (a CDJ or the
	// other player); the hook keeps them silent. Units measured on a JP07:
	// TrackPosition/TrackLength are track samples, Speed is the fader (0.5 = 0 %,
	// 0..1 = the SpeedRange), Play is the play state. A seek flushes Engine's
	// decoder and waveform, and every fader write re-tunes the time stretcher, so
	// a playing shadow deck only takes the source's pitch and runs on its own audio
	// clock (crystals agree to ~20 ppm).
	// Latency: the sender samples its decks every 40 ms and posts at once on an
	// edge (play, CUE, a jump, pitch, track), else every 120 ms; its Go pushes the
	// post over UDP and the receiver's Go wakes the long poll below. The receiver
	// applies a play or stop edge on the first update, never behind a lock.
	property var scShadowAt: ({})
	property var scShadowPrev: ({}) // own decks at the last 40 ms sample
	property real scShadowPostAt: 0
	property real scShadowPollAt: 0
	property real scShadowVer: 0
	property int scShadowGen: 0
	// Planck.getProperty is a C++ lookup; the shadow code runs it thousands of times a second.
	property var scPropCache: ({})
	function scTr(path) {
		let t = scPropCache[path]
		if (t === undefined) {
			const p = Planck.getProperty(path)
			t = p ? p.translator : null
			if (t) scPropCache[path] = t
		}
		return t
	}
	// #151: CDJ-style BARS.BEATS countdown to the next hot cue ahead of the playhead.
	// scTr values are not bindings, so the capsules re-read on this tick.
	property int scCueTick: 0
	Timer {
		interval: 120; repeat: true; running: true
		onTriggered: root.scCueTick = (root.scCueTick + 1) & 0xffff
	}
	function scNextCueInfo(d, tick) {
		const t = p => (root.scTr("/Engine/Deck" + d + p) || {})
		if (!t("/Track/SongLoaded").state) return null
		const pos = t("/Track/TrackPosition").unnormalized || 0
		const sr = t("/Track/SampleRate").value || 44100
		const bpm = t("/OriginalBPM").unnormalized || t("/Track/CurrentBPM").unnormalized || 0
		if (!(sr > 0 && bpm > 0)) return null
		let best = null
		for (let i = 1; i <= 8; i++) {
			if (!t("/Track/QuickCue" + i + "Exists").state) continue
			const cp = t("/Track/QuickCue" + i + "Position").value
			if (!(cp > pos + sr * 0.05) || (best && cp >= best.pos)) continue
			best = {idx: i, pos: cp}
		}
		if (!best) return null
		const totalBeats = (best.pos - pos) / sr * (bpm / 60.0)
		if (totalBeats <= 0 || totalBeats > 2048) return null
		const bars = Math.floor(totalBeats / 4)
		const nm = t("/Track/QuickCue" + best.idx + "Name").string || ("CUE " + best.idx)
		return {
			text: (bars < 10 ? "0" + bars : String(bars)) + "." + ((Math.floor(totalBeats) % 4) + 1) + " BARS",
			label: nm.slice(0, 8),
			color: String(t("/Track/QuickCue" + best.idx + "Color").color || "#26a6ff"),
			urgent: bars < 4
		}
	}
	property var scShadowCueCache: ({})
	Timer {
		interval: 40; repeat: true
		running: root.scFourDeckRole !== ""
		onTriggered: root.scShadowTick()
	}
	Timer {
		id: scShadowPollTimer
		onTriggered: if (root.scFourDeckRole !== "") root.scShadowPoll()
	}
	property bool scShadowCdjMaster: false // a CDJ on a shadow deck holds master
	// Engine's buttons (RequestMaster, Sync, UnSync) act on false -> true and only
	// see the value between event-loop turns: release, press, release, 120 ms
	// apart. /Engine/DeckN/Play is not one of them: it is the play state itself
	// (true plays, false pauses), so it is written, not pressed.
	property var scPressQueue: []
	function scPress(path) {
		scPressQueue = scPressQueue.concat([{path: path, step: 0}])
		scPressTimer.restart()
	}
	Timer {
		id: scPressTimer
		interval: 120
		onTriggered: {
			const next = []
			for (const e of root.scPressQueue) {
				const t = root.scTr(e.path)
				if (!t) continue
				t.state = e.step === 1
				if (e.step < 2) next.push({path: e.path, step: e.step + 1})
			}
			root.scPressQueue = next
			if (next.length) restart()
		}
	}
	function scShadowTick() {
		const g = (d, p) => root.scTr("/Engine/Deck" + d + p)
		const now = Date.now()
		if (now - scShadowPollAt > 3000) scShadowPoll() // starts the chain, or restarts a hung one
		const mine = scFourDeckRole.split("").map(c => parseInt(c))
		let edge = now - scShadowPostAt >= 120
		for (const d of mine) {
			const sr = g(d, "/Track/SampleRate").value
			const s = {ps: !!g(d, "/PlayState").state, posMs: sr > 0 ? g(d, "/Track/TrackPosition").unnormalized / sr * 1000 : 0,
				pitch: parseFloat(g(d, "/SpeedState").string) || 0, at: now, jumps: 0,
				// A streaming track's TrackNetworkPath is only "net://<uuid>/(Unknown)" (5.3.0); its TrackUri names it.
				net: g(d, "/Track/SongLoaded").state ? (g(d, "/Track/TrackUri").string || g(d, "/Track/TrackNetworkPath").string) : ""}
			const p = scShadowPrev[d]
			// Holding CUE previews with PlayState false (measured on a JP07): CUE's
			// LED is on and the position runs. The LED alone also blinks while paused.
			s.cue = !s.ps && !!(g(d, "/Track/CueLEDState") || {}).state && !!p && s.net === p.net && (s.posMs > p.posMs + 0.5 || p.cue)
			s.playing = s.ps || s.cue
			if (p) {
				// A jump: hot cue, beat jump, loop, CUE while playing, jog search while
				// paused. TrackPosition moves in ~33 ms steps, so a playing deck sits
				// off its line by that plus timer jitter; a paused one does not move.
				s.jumps = p.jumps
				const expect = p.posMs + (p.playing ? (now - p.at) * (1 + p.pitch / 100) : 0)
				if (s.playing === p.playing && s.net === p.net && Math.abs(s.posMs - expect) > (p.playing ? 80 : 5)) s.jumps++
				if (s.jumps !== p.jumps || s.playing !== p.playing || s.net !== p.net || Math.abs(s.pitch - p.pitch) > 0.02) edge = true
			}
			scShadowPrev[d] = s
		}
		if (edge) scShadowPost(mine, now)
	}
	function scShadowPost(mine, now) {
		const g = (d, p) => root.scTr("/Engine/Deck" + d + p)
		// A networked player's MASTER LED is the unit's /Engine/Sync/Network/MasterStatus
		// (ControlSurfaceModules/Sync.qml); DeckIsMaster is per deck. The unit's
		// master is this player's unless a shadow deck (a CDJ) holds it.
		const deckMaster = [1, 2, 3, 4].map(i => !!g(i, "/DeckIsMaster").state)
		const pNet = root.scTr("/Engine/Sync/Network/MasterStatus")
		const net = !!(pNet && pNet.state)
		const shadowLead = deckMaster.some((m, i) => m && mine.indexOf(i + 1) < 0)
		let lead = 0
		if (net && !shadowLead && !scShadowCdjMaster && !mine.some(d => deckMaster[d - 1]))
			lead = mine.find(d => g(d, "/PlayState").state) || mine[0]
		const link = (root.scTr("/Network/NetworkId") || {}).string || ""
		const own = []
		for (const d of mine) {
			const s = scShadowPrev[d]
			const t = p => root.scTr("/Engine/Deck" + d + "/Track/" + p)
			// 32 property reads: rescan at most every 500 ms per deck, or at once on a new track.
			let cc = scShadowCueCache[d]
			if (!cc || cc.net !== s.net || now - cc.at >= 500) {
				const cues = []
				for (let i = 1; s.net && i <= 8; i++) {
					const q = "QuickCue" + i
					if ((t(q + "Exists") || {}).state)
						cues.push({i: i, p: t(q + "Position").value, n: t(q + "Name").string, c: String(t(q + "Color").color)})
				}
				cc = scShadowCueCache[d] = {net: s.net, at: now, cues: cues}
			}
			const cues = cc.cues
			own.push({deck: d, net: s.net, cues: s.net ? cues : null, cuePos: s.net ? (t("CuePosition") || {}).value : undefined, playing: s.playing, cue: s.cue, link: link, positionMs: s.posMs, at: s.at, jumps: s.jumps,
				pitch: s.pitch, master: deckMaster[d - 1] || d === lead,
				bpm: (root.scTr("/Engine/Deck" + d + "/OriginalBPM") || {}).unnormalized || 0,
				beat: (root.scTr("/Engine/Deck" + d + "/BeatNumber") || {}).unnormalized || 0,
				// the gateway names a streamed shadow copy after the real track, not its numeric id
				title: s.net ? ((root.scTr("/Engine/Deck" + d + "/Track/SongName") || root.scTr("/Engine/Deck" + d + "/Track/TrackName") || {}).string || "") : "",
				artist: s.net ? ((root.scTr("/Engine/Deck" + d + "/Track/ArtistName") || {}).string || "") : "",
				dbg: ""})
		}
		scShadowPostAt = now
		const post = new XMLHttpRequest()
		post.open("POST", "http://127.0.0.1:8080/v1/sc5000/decks")
		post.send(JSON.stringify(own))
	}
	// One request at a time: Go holds it until the other player's state changes
	// (at most 120 ms), so an edge arrives here within a millisecond of its UDP.
	function scShadowPoll() {
		const gen = ++scShadowGen
		scShadowPollAt = Date.now()
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/shadow?ver=" + scShadowVer)
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || gen !== root.scShadowGen) return
			let wait = 500
			try {
				if (x.status === 200) {
					const r = JSON.parse(x.responseText)
					root.scShadowApplyAll(r.decks || [])
					// An older suite answers at once (no ver): keep its old pace.
					wait = !r.role ? 500 : r.ver === undefined ? 120 : 25
					if (r.ver !== undefined) root.scShadowVer = r.ver
				}
			} catch (err) {
				console.warn("EngineX shadow decks: " + err)
			}
			scShadowPollTimer.interval = wait
			scShadowPollTimer.restart()
		}
		x.send()
	}
	function scShadowApplyAll(decks) {
		root.scShadowCdjMaster = decks.some(e => e.master)
		for (const e of decks) root.scShadowApply(e)
		const now = Date.now()
		for (const d of [1, 2, 3, 4]) {
			if (scFourDeckRole.indexOf(String(d)) >= 0) continue
			const e = decks.find(x => x.deck === d)
			if (e && (e.path || e.loading)) { // loading: Go is still copying the track, wait for it
				if (scShadowAt[d]) scShadowAt[d].emptyAt = 0
				continue
			}
			root.scShadowEmpty(d, now)
		}
	}
	// The other player went away (off, rebooting) or ejected its track: stop this
	// shadow deck and, after 350 ms (one dropped update must not eject), unload it
	// so no stale track or waveform stays, handing master back to our own deck.
	function scShadowEmpty(d, now) {
		const pPlay = root.scTr("/Engine/Deck" + d + "/Play")
		if (pPlay && pPlay.state) pPlay.state = false
		const at = scShadowAt[d] || (scShadowAt[d] = {path: ""})
		if (!at.emptyAt) at.emptyAt = now
		if (now - at.emptyAt < 350 || !(at.path || (root.scTr("/Engine/Deck" + d + "/Track/SongLoaded") || {}).state)) return
		if ((root.scTr("/Engine/Deck" + d + "/SyncMode") || {}).string !== "Off") scPress("/Engine/Deck" + d + "/UnSync")
		if ((root.scTr("/Engine/Deck" + d + "/DeckIsMaster") || {}).state) {
			const m0 = scFourDeckRole.split("")[0]
			if (m0) scPress("/Engine/Deck" + m0 + "/RequestMaster")
		}
		const u1 = root.scTr("/Engine/Deck" + d + "/Unload")
		if (u1) u1.state = true
		const u2 = root.scTr("/Engine/Deck" + d + "/Track/Unload")
		if (u2) u2.state = true
		delete scShadowAt[d]
	}
	function scShadowApply(e) {
		const d = e.deck
		if (scFourDeckRole.indexOf(String(d)) >= 0 || !e.path) return
		const g = p => root.scTr("/Engine/Deck" + d + p)
		const now = Date.now()
		const at = scShadowAt[d] || {path: ""}
		if (at.path !== e.path) {
			root.scTr("/Client/Librarian/AddToDatabaseAndLoadToDeck" + d).url = "file://" + encodeURI(e.path)
			scShadowAt[d] = {path: e.path, loadUntil: now + 1200, seekAt: 0, seeks: 0, loads: (at.loads || 0) + 1, jumps: e.jumps}
			return
		}
		if (now < at.loadUntil || !g("/Track/SongLoaded").state) return
		at.ready = true
		at.link = e.link || ""
		// no scShadowLink(): a RemoteState/Register subscription made Engine spam "service has disappeared" and block the Librarian when the other player went off; our UDP push already carries the edges
		scShadowCues(d, at, e, now)
		// Updates sent before an Engine Link edge arrive after it: skip them, or
		// they would undo the edge for a moment.
		if (now - (at.linkAt || 0) < 300 && (e.playing && !e.cue) !== at.linkPS) return
		if (e.cue && at.linkCueOff && now - at.linkCueOff < 300) return
		const wasPreview = at.preview
		at.preview = !!e.cue
		if (!e.playing) at.pausedAt = e.positionMs
		// The CDJ took master: so does its shadow deck, and Engine drops it on ours
		// (one master). Only after the change (for 5 s), so pressing MASTER here
		// afterwards wins it back.
		if (e.master && !at.master) at.wantMaster = now + 5000
		if (!e.master) at.wantMaster = 0
		at.master = !!e.master
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		if (!(sr > 0 && len > 0)) return
		const seek = ms => root.scShadowSeek(d, at, ms)
		// e.at is when Go extrapolated e.positionMs; this runs later on a busy GUI
		// thread, so carry the target to now before comparing.
		const target = e.positionMs + (e.playing && e.at ? Math.max(0, now - e.at) * (1 + e.pitch / 100) : 0)
		const drift = target - g("/Track/TrackPosition").unnormalized / sr * 1000 // > 0: behind
		at.drift = drift
		const pSync = g("/SyncMode")
		const synced = !!pSync && pSync.string !== "" && pSync.string !== "Off"
		const range = parseFloat(g("/SpeedRange").string) || 8
		const speed = Math.max(0, Math.min(1, 0.5 + e.pitch / (2 * range)))
		if (!(e.sync && synced) && Math.abs(g("/Speed").value - speed) > 0.001) g("/Speed").value = speed
		// Play, CUE held, pause, CUE released: on the first update that shows it.
		// at.cmd only stops rewriting the same state while Engine follows (a frame
		// or two); a change of the source's state always goes through.
		const local = !!g("/PlayState").state
		if (e.playing !== local) {
			if (at.cmd !== e.playing || now >= at.cmdUntil) {
				if (e.playing) {
					if (Math.abs(drift) > 25) seek(target)
					g("/Play").state = true
				} else {
					g("/Play").state = false
					// CUE released: back on the cue point. After a preview the sender's
					// TrackPosition may not have jumped back yet; the preview started there.
					const back = wasPreview && at.pausedAt !== undefined ? at.pausedAt : e.positionMs
					seek(back)
					at.target = back
					at.check = now + 60 // and where it landed once Engine stopped
				}
				at.cmd = e.playing
				at.cmdUntil = now + 250
				at.jumps = e.jumps
			}
			return
		}
		at.cmd = undefined
		// Engine ignores RequestMaster on a stopped deck: press it once this one
		// plays, again until it is master.
		if (at.wantMaster && now < at.wantMaster && !g("/DeckIsMaster").state && local && now >= (at.pressed || 0)) {
			scPress("/Engine/Deck" + d + "/RequestMaster")
			at.pressed = now + 800
		}
		if (g("/DeckIsMaster").state) at.wantMaster = 0
		// The CDJ's BEAT SYNC is Engine's own Sync on the shadow deck: Engine then
		// holds its tempo and phase to the master, so no fader and few seeks here.
		if (!!e.sync !== synced && now >= (at.syncAt || 0)) {
			scPress("/Engine/Deck" + d + (e.sync ? "/Sync" : "/UnSync"))
			at.syncAt = now + 800
		}
		if (!e.playing) {
			// Paused: seek once per source position (the cue point moved, jog
			// search) and check the landing once. It is off by a decoder frame, or
			// a beat with Quantize, and seeking again every tick replayed it.
			at.jumps = e.jumps
			const fresh = at.target === undefined || at.target === null || Math.abs(e.positionMs - at.target) > 5
			const check = at.check && now >= at.check
			if ((fresh || check) && Math.abs(drift) > 10 && now - at.seekAt >= 40) {
				seek(e.positionMs)
				at.target = e.positionMs
				at.check = fresh ? now + 60 : 0
			} else if (check) at.check = 0
			scShadowBeat(d, at, e, drift, now)
			return
		}
		at.target = null
		// Playing: seek at once when the source jumped, after 1 s on a large gap
		// (a CDJ has no jump count), and on steady drift past the limit on three
		// updates at most every 2 s. at.wide: two steady seeks within 10 s did not
		// land (Quantize snaps to a beat), so this track then only follows jumps.
		const lim = e.sync && synced ? 2000 : at.wide ? 800 : 150
		const big = Math.max(350, lim)
		const jump = e.jumps !== at.jumps && Math.abs(drift) > 60
		if (!jump) at.jumps = e.jumps
		at.over = Math.abs(drift) > lim ? (at.over || 0) + 1 : 0
		if ((jump && now - at.seekAt >= 80) || (Math.abs(drift) > big && now - at.seekAt >= 1000) || (at.over >= 3 && now - at.seekAt >= 2000)) {
			if (!jump && Math.abs(drift) <= big) {
				at.wide = at.wide || now - (at.lastSteady || 0) < 10000
				at.lastSteady = now
			}
			seek(target)
			at.jumps = e.jumps
			at.over = 0
			at.seeks = (at.seeks || 0) + 1
		}
	}
	// The source's hot cues and CUE point (track samples), set through Engine's
	// own deck properties: Engine shows them on the waveform, the overview and
	// the pads, and stores them itself. Nothing else may write its m.db (it
	// holds it with an exclusive lock and its own page cache; a write broke it).
	function scShadowCues(d, at, e, now) {
		if (!Array.isArray(e.cues)) return // a CDJ: no cues sent
		const sig = JSON.stringify(e.cues) + "/" + e.cuePos
		if (now < (at.cueAt || 0) + (sig === at.cueSig ? 1000 : 250)) return
		at.cueAt = now
		at.cueSig = sig
		const t = p => root.scTr("/Engine/Deck" + d + "/Track/" + p)
		const want = {}
		for (const c of e.cues)
			if (c && c.i >= 1 && c.i <= 8 && isFinite(c.p) && c.p >= 0) want[c.i] = c
		for (let i = 1; i <= 8; i++) {
			const q = "QuickCue" + i, w = want[i], has = !!t(q + "Exists").state
			if (!w) {
				if (has) scPress("/Engine/Deck" + d + "/Track/DeleteQuickCue" + i)
				continue
			}
			if (!has || Math.abs(t(q + "Position").value - w.p) > 0.5) t(q + "SetPosition").value = w.p
			const n = String(w.n || "").slice(0, 64)
			if (t(q + "Name").string !== n) t(q + "Name").string = n
			if (/^#[0-9a-f]{6,8}$/i.test(w.c || "") && String(t(q + "Color").color) !== w.c) t(q + "Color").color = w.c
		}
		if (isFinite(e.cuePos) && e.cuePos >= 0 && Math.abs(t("CuePosition").value - e.cuePos) > 0.5) t("CuePosition").value = e.cuePos
	}
	// The shadow deck's grid is Engine's own analysis of the copy. A whole beat
	// off (another downbeat) is fixed with Engine's OffsetPhase buttons, one
	// press per check: Forward moves BeatNumber back by one (measured on a
	// JP07). Part of a beat has no control short of the on-screen grid editor.
	function scShadowBeat(d, at, e, drift, now) {
		if ((at.beatTries || 0) >= 4 || !(e.bpm > 0) || e.beat === undefined || Math.abs(drift) > 15 || now < (at.beatAt || 0)) return
		const local = (root.scTr("/Engine/Deck" + d + "/BeatNumber") || {}).unnormalized
		if (local === undefined) return
		const k = ((Math.round(e.beat - local) % 4) + 4) % 4
		const step = k === 3 ? -1 : k
		if (!step) {
			at.beatTries = 4
			return
		}
		scPress("/Engine/Deck" + d + "/Track/OffsetPhase" + (step > 0 ? "Backward" : "Forward"))
		at.beatTries = (at.beatTries || 0) + 1
		at.beatAt = now + 600
	}
	function scShadowSeek(d, at, ms) {
		const g = p => root.scTr("/Engine/Deck" + d + p)
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		if (!(sr > 0 && len > 0)) return
		g("/Track/SetSongPosition").value = Math.max(0, Math.min(1, ms / (len / sr * 1000)))
		at.seekAt = Date.now()
	}
	// Engine Link already mirrors the other Engine's properties here, within a
	// few ms of that Engine changing them (measured: our own update arrives
	// 60-130 ms later, two GUI-thread XHR hops). So play/stop and the CUE LED
	// come from Engine Link; positions, jumps and pitch still from our updates.
	// Register takes one path per event-loop turn: one path per update.
	property var scShadowLinks: ({})
	function scShadowLink(d, uuid) {
		const k = uuid + d, n = scShadowLinks[k] || 0
		const paths = ["/Engine/Deck" + d + "/PlayState", "/Engine/Deck" + d + "/Track/CueLEDState"]
		if (n >= paths.length) return
		const reg = Planck.getProperty("/RemoteState/" + uuid + "/Register")
		if (!reg || !reg.translator) return
		reg.translator.string = paths[n]
		const p = Planck.getProperty("/RemoteState/" + uuid + "/RemoteProperties" + paths[n])
		if (!p || !p.translator) return
		scShadowLinks[k] = n + 1
		const kind = n ? "cue" : "play"
		p.translator.stringChanged.connect(() => root.scShadowLinkEdge(d, uuid, kind, p.translator.string))
	}
	function scShadowLinkEdge(d, uuid, kind, json) {
		const at = scShadowAt[d]
		if (!at || !at.ready || at.link !== uuid) return
		let on
		try { on = !!JSON.parse(json).state } catch (err) { return }
		const g = p => root.scTr("/Engine/Deck" + d + p)
		const now = Date.now()
		const local = !!g("/PlayState").state
		if (kind === "cue") {
			at.linkCueOff = on ? 0 : now
			if (!on && at.preview && local) { // CUE released after a preview
				g("/Play").state = false
				if (at.pausedAt !== undefined) scShadowSeek(d, at, at.pausedAt)
				at.target = at.pausedAt
				at.check = now + 60
				at.preview = false
				at.cmd = false
				at.cmdUntil = now + 250
			}
			return
		}
		at.linkAt = now
		at.linkPS = on
		if (on === local) return
		g("/Play").state = on
		at.cmd = on
		at.cmdUntil = now + 250
		at.target = null // the next update says where it stopped
	}
	function scRestartCall(query) {
		const prompt = PlanckGUI.views.prompts.post({
			type: Prompt.Warning,
			message: "需要重启 Engine，播放会中断。\nRestart Engine to apply? Playback will stop.",
			options: Prompt.Cancel | Prompt.Ok
		})
		prompt.finished.connect(function(r) { if (r === Prompt.AcceptRole) scCall(query + "&restart=1") })
	}
	property string scUpdateRepo: "Cd1s/EngineX-OS"
	// #79: only GET /v1/sc5000/ui (loopback) carries these; shown in Settings -> About.
	property string scConsoleUrl: ""
	property string scAccessCode: ""
	// #148: home sc5000-stemsd last seen by stems-processor; "" = never checked.
	property string scLanStems: ""
	property string scUpdateText: ""
	property bool scBroadcastOn: false
	property bool scUpfaderEcho: false
	property string scLastBroadcastTrack: ""
	property bool scDualColumn: false
	property int scSyncGuardPct: 0
	property bool scSwapLoopOrder: false
	property int scLedBrightness: 100
	property int scUIScale: 100
	property bool scStemsEqMode: false
	property bool scBeatJumpPads: false
	property string scPioneerFx: "off"
	// Offers this model's release image to EngineX's online updater.
	property ListModel scUpdateModel: ListModel {}
	property bool scUpdating: false
	Timer {
		id: scUpdatePollTimer
		interval: 500
		repeat: true
		running: root.scUpdating
		onTriggered: root.scFetchUpdate(false)
	}
	function scConfirmUpdate(ver) {
		if (scUpdating) return
		try {
			let msg = {
				id: "FirmwareUpdateAvailable",
				type: Prompt.Info,
				title: scT("Update Now"),
				options: [
					{text: scT("Remind Later"), role: Prompt.RejectRole},
					{text: scT("Update"), role: Prompt.AcceptRole}
				]
			}
			let p = PlanckGUI.views.prompts.post(msg)
			p.message = scT("New release available:") + " EngineX OS " + (ver || "")
			p.finished.connect(function(role) {
				if (role === Prompt.AcceptRole)
					root.scApplyUpdate()
			})
		} catch (e) {
			root.scApplyUpdate()
		}
	}
	function scApplyUpdate() {
		if (scUpdating) return
		scUpdating = true
		scUpdateText = scT("Updating…")
		if (scUpdateModel.count > 0) {
			scUpdateModel.setProperty(0, "isEnabled", false)
			scUpdateModel.setProperty(0, "buttonLabel", scT("Updating…"))
			scUpdateModel.setProperty(0, "downloadProgress", 0.05)
		}
		scFetchUpdate(true)
	}
	// EngineX checks its own GitHub repository, not Denon's firmware server.
	function scCheckUpdate() {
		if (scUpdating) return
		scUpdateText = scT("Checking GitHub…")
		scUpdateModel.clear()
		scFetchUpdate(false)
	}
	function scFetchUpdate(apply) {
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/update" + (apply ? "?apply=1" : ""))
		x.onreadystatechange = function() {
			if (x.readyState !== 4) return
			if (x.status !== 200) {
				scUpdating = false
				scUpdateText = scT("GitHub unreachable")
				return
			}
			const v = JSON.parse(x.responseText)
			if (v.currentVersion && v.currentVersion !== "dev") scVersion = v.currentVersion
			scUpdating = !!v.updating
			scUpdateText = v.error ? scT(v.error) : v.updating ? scT("Updating…") + " " + Math.round((v.progress || 0.05) * 100) + "%" : v.updateAvailable ? scT("New release available:") + " " + v.latestVersion : scT("Up to date") + " (" + v.currentVersion + ")"
			if (v.updateAvailable && v.downloadUrl) {
				const lbl = v.updating ? Math.round((v.progress || 0.05) * 100) + "%" : scT("Update")
				const prog = v.updating ? (v.progress || 0.05) : 0
				if (scUpdateModel.count < 1)
					scUpdateModel.append({ displayVersion: v.latestVersion, imageUrl: v.downloadUrl, buttonLabel: lbl, isEnabled: !v.updating, downloadProgress: prog })
				else {
					scUpdateModel.setProperty(0, "buttonLabel", lbl)
					scUpdateModel.setProperty(0, "isEnabled", !v.updating)
					scUpdateModel.setProperty(0, "downloadProgress", prog)
				}
			}
		}
		x.send()
	}
	property bool scPlayheadRestored: false
	// Engine resets this preference on every start, so settings.conf keeps it.
	function scKeepPlayhead(saved) {
		const p = Planck.getProperty("/Client/Preferences/PlayheadPosition")
		if (!p || !p.translator) return
		if (!scPlayheadRestored) {
			scPlayheadRestored = true
			if (saved > 0) p.translator.value = saved
		} else if (Math.abs(p.translator.value - saved) > 0.001) {
			scCall("playhead=" + p.translator.value)
		}
	}
	// Jog light modes write only the decks' live colours. The profile's colours stay
	// as chosen and are not written ten times a second to flash.
	property real scJogHue: 0
	property var scJogSaved: null
	function scDeckList() { return (root.decks && root.decks.length ? root.decks : [1, 2]).map(d => parseInt(d)) }
	function scDeckColor(deck) { const p = Planck.getProperty("/Engine/Deck" + deck + "/Color"); return p ? p.translator : null }
	// BPM sync: the tempo of the deck that is playing, the active one first; 0 when
	// nothing plays. Rounded so the pitch fader does not restart the timer constantly.
	function scPlayingBPM() {
		const decks = (root.activeDecks || []).map(d => parseInt(d)).concat(root.scDeckList())
		for (const d of decks) {
			const play = Planck.getProperty("/Engine/Deck" + d + "/PlayState")
			let bpm = Planck.getProperty("/Engine/Deck" + d + "/Track/CurrentBPM")
			if (!bpm || !bpm.translator) bpm = Planck.getProperty("/Engine/Deck" + d + "/CurrentBPM")
			const isPlaying = play && play.translator && play.translator.state
			const bpmVal = bpm && bpm.translator ? (bpm.translator.unnormalized || bpm.translator.value || 0) : 0
			if (isPlaying && bpmVal > 40)
				return Math.round(bpmVal * 10) / 10
		}
		return 0
	}
	property real scJogBPM: 0
	Timer {
		interval: 500; repeat: true
		running: root.scJog === "bpm"
		onTriggered: root.scJogBPM = root.scPlayingBPM()
		onRunningChanged: root.scJogBPM = running ? root.scPlayingBPM() : 0
	}
	Timer {
		// bpm: one colour step per beat, a full turn of the colour wheel every four beats.
		interval: root.scJog === "fast" ? 60 : root.scJog === "bpm" && root.scJogBPM > 0 ? Math.round(60000 / root.scJogBPM) : 250
		repeat: true
		running: root.scJog === "rainbow" || root.scJog === "fast" || root.scJog === "bpm"
		onRunningChanged: {
			const decks = root.scDeckList()
			if (running) {
				root.scJogSaved = decks.map(d => { const c = root.scDeckColor(d); return c ? String(c.color) : "" })
			} else if (root.scJogSaved) {
				decks.forEach((d, i) => { const c = root.scDeckColor(d); if (c && root.scJogSaved[i]) c.color = root.scJogSaved[i] })
				root.scJogSaved = null
			}
		}
		onTriggered: {
			const step = root.scJog === "fast" ? 0.05 : root.scJog === "bpm" && root.scJogBPM > 0 ? 0.25 : 0.025
			const hue = (root.scJogHue + step) % 1
			// a colour change under 0.015 of the wheel is invisible: skip the writes
			if (Math.min(Math.abs(hue - root.scJogHue), 1 - Math.abs(hue - root.scJogHue)) < 0.015) return
			root.scJogHue = hue
			root.scDeckList().forEach((d, i) => {
				const c = root.scDeckColor(d)
				if (c) c.color = Qt.hsva((root.scJogHue + i * 0.35) % 1, 1, 1, 1)
			})
		}
	}
	property var scSources: ["tidal", "netease", "spotify", "deezer", "soundcloud", "beatport", "applemusic"]
	property var scLoggedIn: []
	property string scScope: ""
	readonly property var scServices: [
		{ id: "tidal", name: "TIDAL" },
		{ id: "netease", name: scT("NetEase Cloud Music") },
		{ id: "spotify", name: "Spotify" },
		{ id: "deezer", name: "Deezer" },
		{ id: "soundcloud", name: "SoundCloud" },
		{ id: "beatport", name: "Beatport" },
		{ id: "applemusic", name: "Apple Music" }
	]
	// Every row is keyed by all three spellings, so a string baked into the pack in
	// one language maps to any other: changing scLang re-evaluates the bindings.
	readonly property var scDicts: (@DICTS@)
	readonly property var scDict: scDicts[scLang] || ({})
	function scT(s) { const d = scDicts[scLang]; return (d && d[s]) || s }
	// Translated literals reach this through Math.scI18n: ids here are not visible from every file.
	QtObject {
		id: scI18n
		function t(s) { return root.scT(s) }
	}
	// Evaluated while this file's bindings are enabled, before any Loader below creates its content.
	readonly property bool scI18nShared: { Math.scI18n = scI18n; return true }
	function scIcon(id) {
		if (id === "tidal") return "qrc:/@ICONS@/SourceTIDALColored.png"
		if (id === "soundcloud") return "qrc:/@ICONS@/SourceSoundCloudColored.png"
		if (id === "beatport") return "qrc:/@ICONS@/SourceBeatport%20LINKColored.png"
		return "file:///data/sc5000-custom/ui/icons/" + id + ".png"
	}
	// The gateway appends " [128 BPM 8A]" to a row's label; see labelWithBPM. A value
	// Engine analysed itself always wins, so these only fill empty columns.
	function scPreview(label) {
		return /\\s\\[(\\d+) BPM(?: ([0-9]{1,2}[AB]))?\\]$/.exec(label || "") || ["", "", ""]
	}
	function scPlainTitle(song) { return (song || "").replace(/\\s\\[\\d+ BPM(?: [0-9]{1,2}[AB])?\\]$/, "") }
	// Engine's messages name its one streaming client; say which service is showing.
	function scBrand(s) {
		const service = scServices.find(v => v.id === scScope)
		return s && service && scScope !== "tidal" ? String(s).replace(/TIDAL/gi, service.name) : s
	}
	function scCall(query, done) {
		const x = new XMLHttpRequest()
		x.open(query ? "POST" : "GET", "http://127.0.0.1:8080/v1/sc5000/ui" + (query ? "?" + query : ""))
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			scApply(JSON.parse(x.responseText))
			if (done) done()
		}
		x.send()
	}
	function scApply(v) {
		scEnsureTidal()
		if (v.version) scVersion = v.version
		scLang = v.language
		scWave = v.waveform
		scQuality = v.quality || "flac"
		scNeteaseMode = v.neteaseMode || "official"
		scUpdateRepo = v.githubRepo || scUpdateRepo
		if (v.accessCode) { scAccessCode = v.accessCode; scConsoleUrl = v.consoleUrl || "" }
		scLanStems = v.lanStems ? (v.lanStems.online ? scT("Stems LAN renderer online:") : scT("Stems LAN renderer offline:")) + " " + (v.lanStems.hostname || v.lanStems.endpoint || "") : ""
		scJog = v.jog || "static"
		scMasterLimiter = !!v.masterLimiter
		scAutoGain = !!v.autoGain
		scAutoAnalyze = !!v.autoAnalyze
		scOmniSource = !!v.omniSource
		scProlinkShare = !!v.prolinkShare
		scProlinkExport = !!v.prolinkExport
		scPlayer = !!v.player
		scFourDeckRole = v.player && v.fourDeckWaveform ? (v.fourDeckRoleActive || "") : ""
		scFourDeckOn = !!v.fourDeckWaveform
		scFourDeckRoleSetting = v.fourDeckRole || "auto"
		scKeepPlayhead(v.playhead || 0)
		scSources = v.sources
		scLoggedIn = v.loggedIn
		scScope = v.scope
		scBroadcastOn = !!v.broadcastEnabled
		scUpfaderEcho = !!v.upfaderEcho
		if (typeof v === "string") scParseSettingsConf(v)
		else if (v) {
			if (v.browserDualColumn !== undefined) scDualColumn = !!v.browserDualColumn
			else if (v.browser_dual_column !== undefined) scDualColumn = v.browser_dual_column === "1" || v.browser_dual_column === 1 || v.browser_dual_column === true
			if (v.syncGuardPct !== undefined) scSyncGuardPct = parseInt(v.syncGuardPct) || 0
			else if (v.sync_guard_pct !== undefined) scSyncGuardPct = parseInt(v.sync_guard_pct) || 0
			if (v.swapLoopOrder !== undefined) scSwapLoopOrder = !!v.swapLoopOrder
			else if (v.swap_loop_order !== undefined) scSwapLoopOrder = v.swap_loop_order === "1" || v.swap_loop_order === 1 || v.swap_loop_order === true
			if (v.ledBrightness !== undefined) scLedBrightness = parseInt(v.ledBrightness) || 100
			else if (v.led_brightness !== undefined) scLedBrightness = parseInt(v.led_brightness) || 100
			if (v.uiScale !== undefined) scUIScale = parseInt(v.uiScale) || 100
			else if (v.ui_scale !== undefined) scUIScale = parseInt(v.ui_scale) || 100
			if (v.stemsEqMode !== undefined) scStemsEqMode = !!v.stemsEqMode
			else if (v.stems_eq_mode !== undefined) scStemsEqMode = v.stems_eq_mode === "1" || v.stems_eq_mode === 1 || v.stems_eq_mode === true
			if (v.beatJumpPads !== undefined) scBeatJumpPads = !!v.beatJumpPads
			else if (v.beat_jump_pads !== undefined) scBeatJumpPads = v.beat_jump_pads === "1" || v.beat_jump_pads === 1 || v.beat_jump_pads === true
			if (v.pioneerFx !== undefined) scPioneerFx = v.pioneerFx || "off"
			else if (v.pioneer_fx !== undefined) scPioneerFx = v.pioneer_fx || "off"
		}
		const p = Planck.getProperty("/Client/Preferences/Profile/Application/WaveformStyle")
		if (p.translator && (p.translator.index == 1) !== (v.waveform === "rgb"))
			p.translator.index = v.waveform === "rgb" ? 1 : 0
		// Unswapped, the Auto Loop knob resizes the loop: Beat Jump size follows it.
		if (!scSwapLoopOrder) {
			const lbj = root.scTr("/Client/Preferences/Profile/Application/LinkBeatJumpLoopSize")
			if (lbj && !lbj.state) lbj.state = true
		}
		scUiLoaded = true
	}
	function scParseSettingsConf(text) {
		for (const line of (text || "").split("\n")) {
			const eq = line.indexOf("=")
			if (eq < 0) continue
			const key = line.slice(0, eq).trim()
			const val = line.slice(eq + 1).trim()
			if (key === "browser_dual_column") root.scDualColumn = val === "1"
			else if (key === "sync_guard_pct") root.scSyncGuardPct = parseInt(val) || 0
			else if (key === "swap_loop_order") root.scSwapLoopOrder = val === "1"
			else if (key === "led_brightness") root.scLedBrightness = parseInt(val) || 100
			else if (key === "ui_scale") root.scUIScale = parseInt(val) || 100
			else if (key === "stems_eq_mode") root.scStemsEqMode = val === "1"
			else if (key === "beat_jump_pads") root.scBeatJumpPads = val === "1"
			else if (key === "pioneer_fx") root.scPioneerFx = val || "off"
		}
	}
	function scSyncBroadcastMeta() {
		if (!scBroadcastOn) return
		const decks = (root.activeDecks || []).map(d => parseInt(d)).concat(root.scDeckList())
		for (const d of decks) {
			const play = Planck.getProperty("/Engine/Deck" + d + "/PlayState")
			if (!(play && play.translator && play.translator.state)) continue
			const pTitle = Planck.getProperty("/Engine/Deck" + d + "/Track/SongName")
			const pArtist = Planck.getProperty("/Engine/Deck" + d + "/Track/ArtistName")
			const title = scPlainTitle(pTitle && pTitle.translator ? pTitle.translator.string : "")
			const artist = pArtist && pArtist.translator ? pArtist.translator.string : ""
			if (!title && !artist) continue
			const key = artist + " - " + title
			if (key === scLastBroadcastTrack) return
			scLastBroadcastTrack = key
			const x = new XMLHttpRequest()
			x.open("POST", "http://127.0.0.1:8080/v1/sc5000/broadcast/metadata?artist=" + encodeURIComponent(artist) + "&title=" + encodeURIComponent(title))
			x.send()
			return
		}
	}
	Timer {
		interval: 2000; repeat: true
		running: root.scBroadcastOn
		onTriggered: root.scSyncBroadcastMeta()
	}
	// Only a rekordbox USB export's quick-imported overview is low and needs the
	// stretch. Its audio sits under the export's Contents folder; streams, Stream
	// downloads and Engine Library music never do, so they keep stock height.
	function scOverviewScale(track, rgb) {
		const path = track && track.path ? String(track.path) : ""
		if (!/(^|\\/)Contents\\//.test(path)) return 1
		return rgb ? @RGB_SCALE@ : @BANDS_SCALE@
	}
	// Settings and the Control Center selector both switch the palette here.
	function scSetWave(id) {
		if (id === "rgb" && scWave !== "rgb") {
			PlanckGUI.views.prompts.post({
				type: Prompt.Default,
				title: "RGB Waveform Creation",
				message: "RGB Waveforms and Track Overviews will be built when loading to deck.",
				options: Prompt.Ok
			})
		}
		scCall("waveform=" + id)
	}
	function scSetSource(id, on) {
		const list = scSources.filter(s => s !== id)
		if (on) list.push(id)
		scCall("sources=" + list.join(","))
	}
	// Every service is backed by Engine's TIDAL provider; the gateway follows the scope.
	function scOpen(id) {
		scAskedAt = Date.now()
		scScope = id // before Engine's TIDAL client can raise a prompt; scApply confirms it
		scEnsureTidal()
		scCall("scope=" + id, function() {
			Planck.librarianController.streamingPlaylistsController.invalidateUI()
			scSelectPending = !scSelectTidal()
			scEnterTries = 8
			scEnterTimer.start()
			if (id !== "tidal" && scLoggedIn.indexOf(id) < 0) scLoginBegin(id)
		})
	}
	// Engine's own login and pairing prompts for that provider belong to a tap on a
	// service in the source list. Engine also raises them by itself, for instance
	// after a restart with TIDAL last shown or for a track from an old TIDAL login,
	// and then asked to log in to TIDAL again and again (#48). Close those as the
	// cross would.
	property double scAskedAt: 0
	// The local gateway logs Engine's TIDAL client in at once for every other service,
	// so its login, pairing and "Connecting to" boxes would only flash up with the
	// TIDAL name. They stay closed unless TIDAL itself is open.
	function scQuiet() { return scScope !== "" && scScope !== "tidal" }
	function scUnasked(provider) {
		if (Date.now() - scAskedAt < 120000) return false
		const c = Planck.librarianController.streamingInterfaceController
		if (provider && !provider.loggedIn && c.selectedProvider === provider) c.cancelPairing()
		c.restorePreviouslySelectedSource()
		return true
	}
	property string scLogin: ""
	property string scLoginStatus: ""
	property bool scLoginLocal: false
	property int scLoginSerial: 0
	function scLoginRequest(method, id, done) {
		const x = new XMLHttpRequest()
		x.open(method, "http://127.0.0.1:8080/v1/sc5000/login?provider=" + id)
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || scLogin !== id) return
			if (x.status === 200) done(JSON.parse(x.responseText))
			else scLoginStatus = "error"
		}
		x.send()
	}
	function scLoginBegin(id) {
		scLogin = id
		scLoginStatus = "starting"
		scLoginRequest("POST", id, function(v) {
			scLoginSerial++
			scLoginStatus = v.status
			scLoginLocal = /:8080\\/m\\//.test(v.target || "")
		})
	}
	function scLoginDone() {
		scLogin = ""
		scCall("", function() {
			Planck.librarianController.streamingPlaylistsController.invalidateUI()
			scEnterTries = 8
			scEnterTimer.start()
		})
	}
	Timer {
		interval: 2000; repeat: true
		// An expired code waits for a tap: renewing it here redrew the code every
		// two seconds whenever NetEase kept calling fresh ones expired.
		running: scLogin !== "" && ["starting", "error", "expired"].indexOf(scLoginStatus) < 0
		onTriggered: {
			const id = scLogin
			scLoginRequest("GET", id, function(v) {
				if (v.status === "done") scLoginDone()
				else scLoginStatus = v.status
			})
		}
	}
	// The playlists are one level down; open them so a source never looks empty.
	property int scEnterTries: 0
	property bool scSelectPending: false
	// The row may not exist yet right after the provider is re-enabled, and a
	// delegate can be recycled, so find it in the model rather than the view.
	function scSelectTidal() {
		const sources = Planck.librarianController.browserSourceControllerPtr()
		const model = sources.model()
		for (let i = 0; i < model.rowCount(); i++) {
			const index = model.index(i, 0)
			if (["TIDAL", "Tidal"].indexOf(model.data(index, scSourceNameRole)) >= 0) {
				sources.select(index)
				return true
			}
		}
		return false
	}
	readonly property int scSourceNameRole: 257
	// Every service depends on Engine's TIDAL provider. A saved profile can turn it
	// off, which would leave all four service rows doing nothing.
	function scEnsureTidal() {
		for (let i = 0; i < scProviders.count; i++) {
			const watcher = scProviders.objectAt(i)
			if (watcher) watcher.ensure()
		}
	}
	function scFindMyPlaylists(item) {
		if (item.objectName === "Browser_Collection_StackView_ViewDelegate" && item.visible && item.modelData && item.modelData.display === "My Playlists")
			return item
		for (let i = 0; item.children && i < item.children.length; i++) {
			const found = scFindMyPlaylists(item.children[i])
			if (found) return found
		}
		return null
	}
	Timer {
		id: scEnterTimer
		interval: 300; repeat: true
		onTriggered: {
			if (scSelectPending) scSelectPending = !scSelectTidal()
			const row = scFindMyPlaylists(uiContainer)
			if (row) row.clicked(null)
			if (row || --scEnterTries <= 0) stop()
		}
	}
	Timer {
		interval: 3000; running: true; repeat: true; triggeredOnStart: true
		onTriggered: scCall("")
	}
	// Keep Engine's own Stems list in the library sidebar (the hook enables it on JP07)
	// and its Performance Pads bar on screen.
	Timer {
		interval: 2000; running: true
		onTriggered: {
			for (const path of ["/Client/Preferences/Profile/Application/ShowStemsList", "/GUI/PadsViewEnabled"]) {
				const p = Planck.getProperty(path)
				if (p && p.translator && !p.translator.state) p.translator.state = true
			}
			const a = Planck.getProperty("/Client/AnalyticsEnabled")
			if (a && a.translator && a.translator.state) a.translator.state = false
		}
	}
	// Engine renders Stems only for library tracks and silently skips streamed rows.
	// A streamed row dropped on the Stems list is imported by Engine itself from the
	// file stream-sync downloaded, onto a deck that is not playing, then queued.
	function scStemStream(uid) {
		const lib = Planck.librarianController.collectionController
		const m = /uri=streaming:\\/\\/TIDAL\\/Track\\/(\\d+)/.exec(lib.getTrackUrisFromUniversalIds([uid]) || "")
		if (!m) return false
		const deck = root.scDeckList().slice().reverse().find(d => !root.scDeckLocked(d) && !Planck.getProperty("/Engine/Deck%1/PlayState".arg(d)).translator.state)
		if (!deck) return true // all decks playing: never interrupt playback
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/stems/source?id=" + m[1])
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			if (Planck.getProperty("/Engine/Deck%1/PlayState".arg(deck)).translator.state) return // started playing while we waited
			const url = "file://" + encodeURI(JSON.parse(x.responseText).path)
			// Engine imports a file only through a deck load, so load it on an empty deck
			// and eject it again once the Stems render is queued (scStemUnload).
			const song = Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(deck))
			scStemUnload.deck = deck
			scStemUnload.armed = !(song && song.translator && song.translator.state)
			Planck.getProperty("/Client/Librarian/AddToDatabaseAndLoadToDeck%1".arg(deck)).translator.url = url
			scStemImport.url = url
			scStemImport.path = JSON.parse(x.responseText).path
			scStemImport.tries = 20
			scStemImport.restart()
		}
		x.send()
		return true
	}
	// The import lands in the internal m.db, but while a streaming collection is open
	// collectionController is that collection and cannot see it. So the gateway looks
	// the id up by file name; the controller scan (ids 1-2000) is only the fallback.
	Timer {
		id: scStemImport
		property string url: ""
		property string path: ""
		property int tries: 0
		interval: 500; repeat: true
		function render(id) {
			if (!running) return // a slower reply after the render already started
			Planck.librarianController.stemsQueueManager.startRenderTracks(String(id))
			if (scStemUnload.armed) {
				scStemUnload.name = path.slice(path.lastIndexOf("/") + 1)
				scStemUnload.tries = 20
				scStemUnload.restart()
			}
			stop()
		}
		onTriggered: {
			if (tries-- <= 0) { stop(); return }
			const x = new XMLHttpRequest()
			x.open("GET", "http://127.0.0.1:8080/v1/sc5000/stems/track-id?path=" + encodeURIComponent(path))
			x.onreadystatechange = function() {
				if (x.readyState !== 4 || x.status !== 200) return
				const r = JSON.parse(x.responseText)
				if (r.trackId > 0) render(r.trackId)
			}
			x.send()
			const ids = Array.from({ length: 2000 }, (_, i) => i + 1)
			const lines = (Planck.librarianController.collectionController.getTrackUrisFromUniversalIds(ids) || "").split("\\n")
			const hit = lines.find(l => l.indexOf(encodeURI(path.replace("/data/", "/")) + "?") >= 0)
			const id = hit && /[?&]trackId=(\\d+)/.exec(hit)
			if (id) render(id[1])
		}
	}
	// Ejects the deck that scStemStream loaded only to get the file into the library,
	// but only while it still holds that file and is not playing.
	// shortcut: one pending import at a time; a second Stems tap re-arms the same deck.
	Timer {
		id: scStemUnload
		property int deck: 0
		property bool armed: false
		property string name: ""
		property int tries: 0
		interval: 500; repeat: true
		onTriggered: {
			if (--tries <= 0 || !armed) { stop(); return }
			const song = Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(deck))
			const uri = Planck.getProperty("/Engine/Deck%1/Track/TrackUri".arg(deck))
			if (!song.translator.state || !uri.translator) return
			let u = uri.translator.string
			try { u = decodeURIComponent(u) } catch (e) { }
			if (u.indexOf(name) < 0) return
			if (!Planck.getProperty("/Engine/Deck%1/PlayState".arg(deck)).translator.state)
				Planck.getProperty("/Engine/Deck%1/Track/Unload".arg(deck)).translator.state = true
			armed = false
			stop()
		}
	}
	Instantiator {
		model: root.decks && root.decks.length ? root.decks : [1, 2]
		delegate: QtObject {
			readonly property int deckIdx: parseInt(modelData)
			// Pads stay in Engine's own mode (HOT CUE) when a track loads (#71): they
			// used to jump to STEMS for every stemmed track, which is every track.
			readonly property QObjProperty pUri: Planck.getProperty("/Engine/Deck%1/Track/TrackUri".arg(deckIdx))
			readonly property string uri: pUri && pUri.translator ? pUri.translator.string : ""
			onUriChanged: root.scSwapStemmed(deckIdx, uri)
		}
	}
	// A streamed row with a stemmed local copy (the services' own Stems lists)
	// is swapped for that copy as it loads, so the deck has its Stems. Engine's
	// own import finds the track it already imported; a playing deck is left alone.
	function scSwapStemmed(deck, uri) {
		const m = /^streaming:\\/\\/TIDAL\\/Track\\/(\\d+)$/.exec(uri)
		if (!m) return
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/stems/local?id=" + m[1])
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			if (Planck.getProperty("/Engine/Deck%1/PlayState".arg(deck)).translator.state) return
			const cur = Planck.getProperty("/Engine/Deck%1/Track/TrackUri".arg(deck)) // another track may have been loaded meanwhile
			if (!cur || !cur.translator || cur.translator.string !== uri) return
			Planck.getProperty("/Client/Librarian/AddToDatabaseAndLoadToDeck%1".arg(deck)).translator.url = "file://" + encodeURI(JSON.parse(x.responseText).path)
		}
		x.send()
	}
	Instantiator {
		id: scProviders
		model: Planck.librarianController.streamingInterfaceController.providerModel
		delegate: QtObject {
			function ensure() { if (["TIDAL", "Tidal"].indexOf(provider.name) >= 0 && !provider.integrated) provider.integrated = true }
			readonly property Connections watcher: Connections {
				target: provider
				function onIntegratedChanged() { ensure() }
			}
			Component.onCompleted: ensure()
		}
	}
"""

LOGIN_DIALOG = """
		Rectangle {
			anchors.fill: parent
			z: 1000000
			visible: root.scLogin !== ""
			color: "#e0000000"
			readonly property var service: root.scServices.find(s => s.id === root.scLogin)
			MouseArea { anchors.fill: parent }
			Rectangle {
				anchors.centerIn: parent
				width: 1040; height: 660
				radius: 24
				color: Style.surface02Color
				Rectangle {
					x: 40; y: 60
					width: 540; height: 540
					radius: 16
					color: "white"
					readonly property bool renew: root.scLoginStatus === "expired" || root.scLoginStatus === "error"
					Image {
						anchors.centerIn: parent
						width: 508; height: 508
						smooth: false
						cache: false
						opacity: parent.renew ? 0.08 : 1
						visible: root.scLoginSerial > 0
						source: root.scLogin && root.scLoginSerial > 0 ? "http://127.0.0.1:8080/v1/sc5000/login/qr?provider=" + root.scLogin + "&n=" + root.scLoginSerial : ""
					}
					Text {
						anchors.centerIn: parent
						width: 460
						visible: parent.renew
						horizontalAlignment: Text.AlignHCenter
						wrapMode: Text.WordWrap
						text: root.scT("Tap for a new QR code")
						color: "black"
						font: Style.fontH0
					}
					MouseArea {
						anchors.fill: parent
						enabled: parent.renew
						onClicked: root.scLoginBegin(root.scLogin)
					}
				}
				Image {
					x: 620; y: 70
					width: 88; height: 88
					mipmap: true
					source: root.scLogin ? root.scIcon(root.scLogin) : ""
				}
				Text {
					x: 620; y: 180
					width: 380
					text: parent.parent.service ? parent.parent.service.name : ""
					color: Style.font02Color
					font: Style.fontH0
				}
				Text {
					x: 620; y: 240
					width: 380
					wrapMode: Text.WordWrap
					color: Style.font02Color
					font: Style.fontH1
					text: {
						if (root.scLoginStatus === "error") return root.scT("Couldn't load the login code. Tap the code to retry.")
						if (root.scLoginStatus === "expired") return root.scT("The QR code expired.")
						if (root.scLoginStatus === "scanned") return root.scT("Confirm on your phone")
						if (root.scLogin === "netease") return root.scT("Scan in the NetEase Cloud Music app, not with the camera or WeChat") + "\n\n" + root.scT("Only one phone? Take a photo of this code, then in the app tap Scan and pick the photo from Album.")
						return root.scT(root.scLoginLocal ? "Scan with your phone camera. Phone and deck must be on the same network." : "Scan with your phone camera.")
					}
				}
				Rectangle {
					x: 620; y: 520
					width: 380; height: 80
					radius: 16
					color: closeArea.pressed ? Style.focus02Color : Style.surface03Color
					Text {
						anchors.centerIn: parent
						text: root.scT("Not now")
						color: Style.font02Color
						font: Style.fontH1
					}
					MouseArea {
						id: closeArea
						anchors.fill: parent
						onClicked: root.scLogin = ""
					}
				}
			}
		}
"""

SOURCE_FOOTER = """
	property var scTidalRow: null
	footer: Column {
		width: connectedSource.width
		Repeater {
			model: root.scServices.filter(s => root.scSources.indexOf(s.id) >= 0)
			MouseArea {
				width: connectedSource.width
				height: 108
				onClicked: root.scOpen(modelData.id)
				Rectangle {
					anchors.fill: parent
					radius: 16
					color: parent.pressed ? Style.focus02Color : Style.surface02Color
					border.color: Style.icon02Color
					border.width: connectedSource.scTidalRow && connectedSource.scTidalRow.isCurrent && root.scScope === modelData.id ? 2 : 0
				}
				Image {
					x: connectedSource.delegateLeftPadding + 8
					anchors.verticalCenter: parent.verticalCenter
					width: 48; height: 48
					mipmap: true
					source: root.scIcon(modelData.id)
				}
				Text {
					x: connectedSource.delegateLeftPadding + 80
					anchors.verticalCenter: parent.verticalCenter
					text: modelData.name
					color: Style.font02Color
					font: Style.fontH1
				}
				MouseArea {
					anchors { right: parent.right; top: parent.top; bottom: parent.bottom }
					width: 140
					visible: modelData.id !== "tidal"
					onClicked: root.scLoginBegin(modelData.id)
					Text {
						anchors.centerIn: parent
						text: root.scT(root.scLoggedIn.indexOf(modelData.id) >= 0 ? "Switch account" : "Scan to log in")
						color: Style.font02Color
						font: Style.fontH3
					}
				}
			}
		}
	}
"""

# Connect New Sources, minus stock TIDAL (kept integrated for the custom services; 5.3.0
# names it "Tidal"), SoundCloud and Beatport LINK, which stream-sync replaces. A GridView keeps a blank cell for a hidden delegate; Grid skips it.
# The GridView properties SourceScreen.qml sets stay declared.
AVAILABLE_SOURCE = """import QtQuick
import Planck

Grid {
	id: availableSource
	objectName: "AvailableSource"

	property int columnCount: 5
	property int cellPadding: 16
	readonly property real cellWidth: width / columnCount
	readonly property real cellHeight: cellWidth
	readonly property real contentHeight: implicitHeight
	property bool interactive: false
	property int currentIndex: -1
	readonly property Item currentItem: currentIndex >= 0 && currentIndex < scRepeater.count ? scRepeater.itemAt(currentIndex) : null

	columns: columnCount
	Repeater {
		id: scRepeater
		model: Planck.librarianController.browserSourceControllerPtr().availableSourceModel()
		delegate: AvailableSourceDelegate {
			visible: ["TIDAL", "Tidal", "SoundCloud", "Beatport LINK"].indexOf(model.name) < 0
			width: availableSource.cellWidth
			height: availableSource.cellHeight
			cellContentWidth: availableSource.cellWidth - availableSource.cellPadding * 2
			cellCcontentHeight: availableSource.cellHeight - availableSource.cellPadding * 2
		}
	}
}
"""

STREAM_SWITCHES = """
	Repeater {
		model: root.scServices
		SwitchEntry {
			text: modelData.name
			icon.source: root.scIcon(modelData.id)
			icon.useOriginal: true
			checked: root.scSources.indexOf(modelData.id) >= 0
			onClicked: root.scSetSource(modelData.id, !(root.scSources.indexOf(modelData.id) >= 0))
		}
	}
"""

WAVEFORM_ENTRY = """import QtQuick 2.12
import StateMap 1.0
import Planck 1.0
import "Base"
ListEntry {
	text: "Waveform Style"
	readonly property var ids: ["denon", "rgb", "3band"]
	values: ["Denon", "RGB", "3-Band"]
	currentValue: values[Math.max(0, ids.indexOf(root.scWave))]
	onSelected: function(index) { root.scSetWave(ids[index]) }
}
"""

QUALITY_ENTRY = """
		EntryBase.ListEntry {
			text: "Streaming Quality"
			readonly property var ids: ["flac", "320k"]
			values: ["Lossless", "320kbps"]
			currentValue: values[Math.max(0, ids.indexOf(root.scQuality))]
			onSelected: function(index) { root.scCall("quality=" + ids[index]) }
		},
		EntryBase.ListEntry {
			text: "NetEase Download"
			readonly property var ids: ["official", "deemix"]
			values: ["Official", "Deemix First"]
			currentValue: values[Math.max(0, ids.indexOf(root.scNeteaseMode))]
			onSelected: function(index) { root.scCall("neteaseMode=" + ids[index]) }
		},
"""

JOG_ENTRY = """
		EntryBase.ListEntry {
			text: "Jog Light Mode"
			readonly property var ids: ["static", "rainbow", "fast", "bpm"]
			values: ["Static", "Psy Rainbow", "Psy Fast", "Psy BPM Sync"]
			currentValue: values[Math.max(0, ids.indexOf(root.scJog))]
			onSelected: function(index) { root.scCall("jog=" + ids[index]) }
		},
"""

# Settings -> Services: streaming, live stream and Pro DJ Link. The gate probe looks
# for objectName ControlCenter_LiveStream (it was a Control Center tile up to #50).
SERVICES_ENTRIES = QUALITY_ENTRY + """
		EntryBase.ListEntry {
			text: "Live Stream"
			objectName: "ControlCenter_LiveStream"
			values: ["Off", "On"]
			currentValue: values[root.scBroadcastOn ? 1 : 0]
			onSelected: function(index) { root.scCall("broadcast=" + index) }
		},
		EntryBase.ListEntry {
			text: "Pro DJ Link Library"
			values: ["Off", "On"]
			currentValue: values[root.scProlinkShare ? 1 : 0]
			onSelected: function(index) { root.scCall("prolinkShare=" + index) }
		},
		EntryBase.ListEntry {
			text: "Pro DJ Link Export to CDJ (Beta)"
			values: ["Off", "On"]
			currentValue: values[root.scProlinkExport ? 1 : 0]
			onSelected: function(index) { root.scCall("prolinkExport=" + index) }
		},
"""

# User Profile -> Playback: audio DSP and playback.
PLAYBACK_ENTRIES = """
		EntryBase.ListEntry {
			text: "Master Soft Clipper"
			values: ["Off", "On"]
			currentValue: values[root.scMasterLimiter ? 1 : 0]
			onSelected: function(index) { root.scCall("masterLimiter=" + index) }
		},
		EntryBase.ListEntry {
			text: "Auto-Gain (0 dB Normalize)"
			values: ["Off", "On"]
			currentValue: values[root.scAutoGain ? 1 : 0]
			onSelected: function(index) { root.scCall("autoGain=" + index) }
		},
		EntryBase.ListEntry {
			text: "Echo Out"
			values: ["Off", "On"]
			currentValue: values[root.scUpfaderEcho ? 1 : 0]
			onSelected: function(index) { root.scCall("upfaderEcho=" + index) }
		},
		EntryBase.ListEntry {
			text: "Stems EQ Mode"
			values: ["Off", "On"]
			currentValue: values[root.scStemsEqMode ? 1 : 0]
			onSelected: function(index) { root.scCall("stemsEqMode=" + index) }
		},
		EntryBase.ListEntry {
			text: "Color FX Style"
			readonly property var ids: ["off", "spiral", "pitch_echo", "slip_roll", "dub_echo", "space"]
			values: ["Off", "Spiral", "Pitch Echo", "Slip Roll", "Dub Echo", "Space"]
			currentValue: values[Math.max(0, ids.indexOf(root.scPioneerFx))]
			onSelected: function(index) { root.scCall("pioneerFx=" + ids[index]) }
		},
"""

BEAT_JUMP_ENTRIES = """
		EntryBase.ListEntry {
			text: "Beat Jump Pad Bank"
			values: ["Off", "On"]
			currentValue: values[root.scBeatJumpPads ? 1 : 0]
			onSelected: function(index) { root.scCall("beatJumpPads=" + index) }
		},
		EntryBase.ListEntry {
			text: "Swap Loop / Beat Jump"
			values: ["Off", "On"]
			currentValue: values[root.scSwapLoopOrder ? 1 : 0]
			onSelected: function(index) { root.scCall("swapLoopOrder=" + index) }
		},
"""

SAFETY_ENTRIES = """
		EntryBase.ListEntry {
			text: "Sync Guard Tolerance"
			readonly property var ids: [0, 10, 15, 20]
			values: ["Off", "10%", "15%", "20%"]
			currentValue: values[Math.max(0, ids.indexOf(root.scSyncGuardPct))]
			onSelected: function(index) { root.scCall("syncGuardPct=" + ids[index]) }
		},
"""

DUAL_COLUMN_ENTRY = """
		EntryBase.ListEntry {
			text: "Dual-Column Browser"
			values: ["Off", "On"]
			currentValue: values[root.scDualColumn ? 1 : 0]
			onSelected: function(index) { root.scCall("browserDualColumn=" + index) }
		},
"""

LIBRARY_ENTRIES = """
		EntryBase.ListEntry {
			text: "Auto-Analyze Drives"
			values: ["Off", "On"]
			currentValue: values[root.scAutoAnalyze ? 1 : 0]
			onSelected: function(index) { root.scCall("autoAnalyze=" + index) }
		},
		EntryBase.ListEntry {
			text: "OmniSource (Drives in Streaming)"
			values: ["Off", "On"]
			currentValue: values[root.scOmniSource ? 1 : 0]
			onSelected: function(index) { root.scCall("omniSource=" + index) }
		},
""" + DUAL_COLUMN_ENTRY

UI_SCALE_ENTRY = """
		EntryBase.ListEntry {
			text: "UI Scale"
			readonly property var ids: ["100", "95", "90", "85", "80", "75"]
			values: ["100%", "95%", "90%", "85%", "80%", "75%"]
			currentValue: values[Math.max(0, ids.indexOf(String(root.scUIScale)))]
			onSelected: function(index) { if (ids[index] !== String(root.scUIScale)) root.scCall("uiScale=" + ids[index]) }
		},
"""

LED_ENTRY = """
		EntryBase.ListEntry {
			text: "Pad LED Brightness"
			readonly property var ids: [100, 80, 60, 40, 20]
			values: ["100%", "80%", "60%", "40%", "20%"]
			currentValue: values[Math.max(0, ids.indexOf(root.scLedBrightness))]
			onSelected: function(index) { root.scCall("ledBrightness=" + ids[index]) }
		},
"""

FOUR_DECK_ENTRIES = """
		EntryBase.ListEntry {
			text: "4-Deck Waveform"
			visible: root.scPlayer // #49
			values: ["Off", "On"]
			currentValue: root.scFourDeckOn ? "On" : "Off"
			onSelected: function(index) { if ((index === 1) !== root.scFourDeckOn) root.scRestartCall("fourDeckWaveform=" + index) }
		},
		EntryBase.ListEntry {
			text: "4-Deck Role"
			visible: root.scPlayer && root.scFourDeckOn
			readonly property var ids: ["auto", "13", "24", "12", "1", "2"]
			values: ["Auto", "1+3", "2+4", "1+2", "1", "2"]
			currentValue: values[Math.max(0, ids.indexOf(root.scFourDeckRoleSetting))]
			onSelected: function(index) { if (ids[index] !== root.scFourDeckRoleSetting) root.scRestartCall("fourDeckRole=" + ids[index]) }
		},
"""

# #99 Layout Manager: what a View 1..5 keeps besides the stock layout (scViewSnapshot).
VIEW_ENTRIES = """
		@HEADLINE@ { text: "ENGINEX VIEW PRESET" },
		WaveformStyle { },
""" + JOG_ENTRY + UI_SCALE_ENTRY + DUAL_COLUMN_ENTRY + BEAT_JUMP_ENTRIES + FOUR_DECK_ENTRIES

# #78: a deck in a booth can be switched off without reaching for the recessed rear
# button. Engine's own quit path: the "Turn Off" confirmation, recording and library
# migration guards, then the power-off.
POWER_ENTRY = """
		EntryBase.PrefsButtonEntry {
			text: "Power"
			buttonText: "Turn Off"
			onClicked: Planck.quit(QuitReasons.Poweroff, false)
		},
"""

LANGUAGE_ENTRY = """
		EntryBase.ListEntry {
			text: "Language"
			readonly property var ids: ["en", "zh-Hans", "zh-Hant"]
			values: ["English", "简体中文", "繁體中文"]
			currentValue: values[Math.max(0, ids.indexOf(root.scLang))]
			onSelected: function(index) {
				if (ids[index] !== root.scLang) root.scCall("lang=" + ids[index])
			}
		},
"""

# Control Center: the CDJ-3000 "WAVEFORM COLOR" control, in a stock-size tile
# (#45): a title over three joined segments, each with a colour swatch and a
# name; the selected one is filled cyan. A tap switches at once, no RGB prompt.
WAVE_TILE = """
		Rectangle {
			id: scWaveTile
			objectName: "ControlCenter_WaveformStyle"
			readonly property var ids: ["denon", "rgb", "3band"]
			readonly property string current: ids.indexOf(root.scWave) >= 0 ? root.scWave : "denon"
			width: 256
			height: 104
			radius: 16
			color: Style.surface03Color
			Text {
				x: 14
				y: 8
				text: "Waveform Style"
				font: Style.fontH4
				color: Style.font01Color
			}
			Row {
				x: 12
				y: 36
				Repeater {
					model: ["DENON", "RGB", "3BAND"]
					Rectangle {
						id: seg
						readonly property string waveId: scWaveTile.ids[index]
						readonly property bool selected: scWaveTile.current === waveId
						objectName: "ControlCenter_WaveformStyle_" + waveId
						width: (scWaveTile.width - 24) / 3
						height: 56
						color: selected ? "#00c3e3" : segTap.pressed ? "#6a6a6a" : "#4a4a4a"
						border {
							width: 1
							color: "#a8a8a8"
						}
						Rectangle {
							id: swatch
							anchors.horizontalCenter: parent.horizontalCenter
							y: 10
							width: 50
							height: 12
							color: "#00c853"
							border {
								width: 1
								color: "#202020"
							}
							gradient: seg.waveId === "rgb" ? rainbow : null
							Gradient {
								id: rainbow
								orientation: Gradient.Horizontal
								GradientStop { position: 0.0; color: "#ff2020" }
								GradientStop { position: 0.2; color: "#ffd000" }
								GradientStop { position: 0.4; color: "#20e040" }
								GradientStop { position: 0.6; color: "#00d8ff" }
								GradientStop { position: 0.8; color: "#3050ff" }
								GradientStop { position: 1.0; color: "#e040ff" }
							}
							Row {
								visible: seg.waveId === "3band"
								x: 1
								y: 1
								Rectangle { width: 14; height: 10; color: "@HIGH@" }
								Rectangle { width: 14; height: 10; color: "@MID@" }
								Rectangle { width: 20; height: 10; color: "@LOW@" }
							}
						}
						Text {
							anchors.horizontalCenter: parent.horizontalCenter
							y: 28
							text: modelData
							font: Style.fontH5
							color: "#ffffff"
						}
						MouseArea {
							id: segTap
							anchors.fill: parent
							onClicked: if (!seg.selected) root.scCall("waveform=" + seg.waveId)
						}
					}
				}
			}
		}
"""

# Live Stream moved to Settings -> Services; the Control Center keeps performance tiles.
JOG_TILE = """
		Rectangle {
			id: scJogTile
			objectName: "ControlCenter_JogLight"
			readonly property var ids: ["static", "rainbow", "fast", "bpm"]
			width: 256
			height: 104
			radius: 16
			color: Style.surface03Color
			Text {
				x: 14
				y: 8
				text: "Jog Light"
				font: Style.fontH4
				color: Style.font01Color
			}
			Row {
				x: 12
				y: 36
				Repeater {
					model: ["Static", "Rainbow", "Fast", "BPM"]
					Rectangle {
						readonly property bool selected: root.scJog === scJogTile.ids[index]
						width: (scJogTile.width - 24) / 4
						height: 56
						color: selected ? "#00c3e3" : jogTap.pressed ? "#6a6a6a" : "#4a4a4a"
						border {
							width: 1
							color: "#a8a8a8"
						}
						Text {
							anchors.centerIn: parent
							text: root.scT(modelData).toUpperCase()
							font: Style.fontH5
							color: "#ffffff"
						}
						MouseArea {
							id: jogTap
							anchors.fill: parent
							onClicked: if (!parent.selected) root.scCall("jog=" + scJogTile.ids[index])
						}
					}
				}
			}
		}
"""

UPFADER_ECHO_TILE = """
			Rectangle {
				id: scUpfaderEchoTile
				objectName: "ControlCenter_UpfaderEcho"
				width: 256
				height: 104
				radius: 16
				color: Style.surface03Color
				Text {
					x: 14
					y: 8
					text: "Fader Echo"
					font: Style.fontH4
					color: Style.font01Color
				}
				Row {
					x: 12
					y: 36
					Repeater {
						model: ["Off", "All", "Upfader"]
						Rectangle {
							readonly property var vfx: PlanckGUI.views.transitionFx
							readonly property bool selected: vfx ? (index === 0 ? !vfx.enabled : (vfx.enabled && (index === 2 ? vfx.triggerSource === 1 : vfx.triggerSource !== 1))) : (index === 0 ? !root.scUpfaderEcho : (index === 2 && root.scUpfaderEcho))
							width: (scUpfaderEchoTile.width - 24) / 3
							height: 56
							color: selected ? "#00c3e3" : upfaderEchoTap.pressed ? "#6a6a6a" : "#4a4a4a"
							border {
								width: 1
								color: "#a8a8a8"
							}
							Text {
								anchors.centerIn: parent
								text: root.scT(modelData).toUpperCase()
								font: Style.fontH5
								color: "#ffffff"
							}
							MouseArea {
								id: upfaderEchoTap
								anchors.fill: parent
								onClicked: {
									const vfx = PlanckGUI.views.transitionFx
									if (vfx) {
										if (index === 0) vfx.enabled = false
										else {
											vfx.enabled = true
											vfx.setTriggerSource(index === 2 ? 1 : 2)
										}
									}
									root.scCall("upfaderEcho=" + (index === 0 ? "0" : "1"))
								}
							}
						}
					}
				}
			}
"""

# Smart Sync (5.3.0 only): half/double-time (0.5x, 2x, 0.25x, 4x) harmonic tempo matching.
# Automatically widens Pitch Range to the smallest step needed for the best harmonic match,
# and restores the user's original Pitch Range when Sync is turned off.
SMART_SYNC_MAIN = """
	function scHarmonicSyncNeed(masterBpm, origBpm) {
		if (origBpm <= 0 || masterBpm <= 0) return 0
		const multiples = [1.0, 0.5, 2.0, 0.25, 4.0]
		let minNeed = 1e9
		for (let i = 0; i < multiples.length; i++) {
			const harmonic = origBpm * multiples[i]
			const need = Math.abs(masterBpm - harmonic) / harmonic * 100
			if (need < minNeed) minNeed = need
		}
		return minNeed
	}
	Instantiator {
		model: root.decks && root.decks.length ? root.decks : [1, 2]
		delegate: QtObject {
			readonly property int deckIdx: parseInt(modelData)
			readonly property QObjProperty pSync: Planck.getProperty("/Engine/Deck%1/SyncMode".arg(deckIdx))
			readonly property QObjProperty pLoaded: Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(deckIdx))
			readonly property QObjProperty pOrigBpm: Planck.getProperty("/Engine/Deck%1/OriginalBPM".arg(deckIdx))
			readonly property QObjProperty pMasterBpm: Planck.getProperty("/Engine/Master/MasterTempo")
			readonly property QObjProperty pRange: Planck.getProperty("/Engine/Deck%1/SpeedRange".arg(deckIdx))
			property int savedRangeIndex: -1
			readonly property bool syncActive: {
				const s = pSync && pSync.translator, l = pLoaded && pLoaded.translator
				return !!(s && l && s.string !== "Off" && l.state)
			}
			readonly property real syncNeed: {
				const s = pSync && pSync.translator, l = pLoaded && pLoaded.translator, o = pOrigBpm && pOrigBpm.translator, m = pMasterBpm && pMasterBpm.translator
				if (!s || !l || !o || !m || s.string === "Off" || !l.state || !(o.unnormalized > 0) || !(m.unnormalized > 0)) return 0
				return root.scHarmonicSyncNeed(m.unnormalized, o.unnormalized)
			}
			onSyncActiveChanged: {
				const r = pRange && pRange.translator
				if (syncActive) {
					if (savedRangeIndex < 0 && r) savedRangeIndex = r.index
					if (syncNeed > 0) root.scWidenRange(deckIdx, syncNeed)
				} else {
					if (savedRangeIndex >= 0 && r) {
						r.index = savedRangeIndex
						savedRangeIndex = -1
					}
				}
			}
			onSyncNeedChanged: {
				if (syncActive) {
					const r = pRange && pRange.translator
					if (savedRangeIndex < 0 && r) savedRangeIndex = r.index
					if (syncNeed > 0) root.scWidenRange(deckIdx, syncNeed)
				}
			}
		}
	}
	function scWidenRange(deck, need) {
		const t = Planck.getProperty("/Engine/Deck%1/SpeedRange".arg(deck)).translator
		if (!t || need <= parseFloat(t.string)) return
		const i = t.entries.findIndex(e => parseFloat(e) >= need)
		t.index = i < 0 ? t.entries.length - 1 : i
	}
	// The Smart Sync tile calls this so a tap takes effect without waiting for a tempo change.
	function scWidenAll() {
		const m = Planck.getProperty("/Engine/Master/MasterTempo").translator
		for (const d of root.decks && root.decks.length ? root.decks : [1, 2]) {
			const o = Planck.getProperty("/Engine/Deck%1/OriginalBPM".arg(d)).translator
			const s = Planck.getProperty("/Engine/Deck%1/SyncMode".arg(d)).translator
			const l = Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(d)).translator
			if (m && o && s && l && l.state && s.string !== "Off" && o.unnormalized > 0 && m.unnormalized > 0) scWidenRange(d, scHarmonicSyncNeed(m.unnormalized, o.unnormalized))
		}
	}
"""

# Control Center (5.3.0 only): Engine's own Sync Mode preference (Bar / Beat /
# Tempo) as a tile in the WAVE_TILE style, next to SMART_SYNC_MAIN.
SYNC_TILE = """
		Rectangle {
			id: scSyncTile
			objectName: "ControlCenter_SmartSync"
			readonly property var translator: Planck.getProperty("/Client/Preferences/Profile/Application/SyncMode").translator
			width: 256
			height: 104
			radius: 16
			color: Style.surface03Color
			Text {
				x: 14
				y: 8
				text: root.scT("Smart Sync")
				font: Style.fontH4
				color: Style.font01Color
			}
			Row {
				x: 12
				y: 36
				Repeater {
					model: scSyncTile.translator.entries
					Rectangle {
						objectName: "ControlCenter_SmartSync_" + modelData
						width: (scSyncTile.width - 24) / 3
						height: 56
						color: scSyncTile.translator.index === index ? "#00c3e3" : syncTap.pressed ? "#6a6a6a" : "#4a4a4a"
						border {
							width: 1
							color: "#a8a8a8"
						}
						Text {
							anchors.centerIn: parent
							text: modelData.toUpperCase()
							font: Style.fontH5
							color: "#ffffff"
						}
						MouseArea {
							id: syncTap
							anchors.fill: parent
							onClicked: {
								scSyncTile.translator.index = index
								root.scWidenAll() // show the effect at once
							}
						}
					}
				}
			}
		}
"""

# CDJ-3000 3-Band palette (blue lows, gold mids, white highs): played,
# fading and past shades of each band.
BAND_COLORS = {"Low": "#4a78ff", "Mid": "#f0b400", "High": "#ffffff"}
BAND_FADE = {"Low": "#3a62dd", "Mid": "#d89e00", "High": "#f0f4ff"}
BAND_PAST = {"Low": "#24408f", "Mid": "#906c00", "High": "#999999"}
# rekordbox's saturated RGB primaries, in place of Engine's muted ones.
RGB_COLORS = {"Low": "#ff1428", "Mid": "#00e632", "High": "#0078ff"}
RGB_PAST = {"Low": "#7f0a14", "Mid": "#007319", "High": "#003c80"}


# Vertical stretch of the deck overview waveform for tracks on a rekordbox USB
# export (1.0 = stock height; every other track keeps 1.0). RGB draws
# each column at full amplitude and fills the strip quickly; Denon and 3-Band
# stack the bands as separate, lower layers and need more.
OVERVIEW_Y_SCALE_RGB = 1.3
OVERVIEW_Y_SCALE_BANDS = 2.2


def three_band(style_name, band, colors=BAND_COLORS):
    return f'(root.scWave === "3band" ? "{colors[band]}" : Style.{style_name})'


def v53():
    """5.3.0 renamed the skin root, ran qmlformat and moved or rewrote a few views."""
    return ROOT == "Skins/Embedded/"


def window():
    # The QML name of Engine's window: MainWindow up to 5.0.4, planckQuickView in 5.3.0.
    return "planckQuickView" if v53() else "MainWindow"


ZOOM_STEP = """
	// 10 waveform zoom steps, geometric (about x1.4 per step, like a pinch) from 1 s to the
	// limit however far #77 raised it: stock steps 0.5 s per detent, 60 detents out to 30 s.
	// A linear step jumped 1 s -> 4 s at once and redrew 4x the vertices in one frame.
	// Detents that arrive within one frame collapse into one write.
	property int scZoomPending: 0
	readonly property QObjProperty scZoomProp: Planck.getProperty("/Engine/Master/WaveformZoom")
	Timer {
		id: scZoomTimer
		interval: 16
		onTriggered: {
			const t = root.scZoomProp && root.scZoomProp.translator
			const n = root.scZoomPending
			root.scZoomPending = 0
			if (!t || !t.editable || !n) return
			const lo = Math.max(1, t.minimum !== undefined ? t.minimum : 1), hi = t.maximum !== undefined ? t.maximum : 30
			const ratio = Math.pow(hi / lo, 1 / 9) // lo..hi in 10 levels
			const cur = Math.max(lo, Math.min(hi, t.unnormalized))
			const level = Math.round(Math.log(cur / lo) / Math.log(ratio)) + n
			t.unnormalized = Math.max(lo, Math.min(hi, lo * Math.pow(ratio, Math.max(0, Math.min(9, level)))))
		}
	}
	function scZoom(dir) {
		scZoomPending += dir
		if (!scZoomTimer.running) scZoomTimer.start()
	}
	readonly property var scZoomProps: [Planck.createProperty("ZoomIn", true, 3), Planck.createProperty("ZoomOut", true, 3)]
	ActionTranslatorStateChangeNotifier { prop: scZoomProps && Planck.getProperty("/GUI/Scripted/ZoomIn"); enabled: true; onChanged: root.scZoom(1) }
	ActionTranslatorStateChangeNotifier { prop: scZoomProps && Planck.getProperty("/GUI/Scripted/ZoomOut"); enabled: true; onChanged: root.scZoom(-1) }
"""


def cue_capsule(ind: str, deck_expr: str, h: int, px: int) -> str:
    """#151: Pioneer-style `● DROP  16.3 BARS` capsule; hidden (no space) without a cue ahead."""
    q = f"""Rectangle {{
    objectName: "EngineX_CueCountdown"
    readonly property var info: typeof root !== "undefined" && root && root.scNextCueInfo ? root.scNextCueInfo({deck_expr}, root.scCueTick) : null
    visible: info !== null
    Layout.alignment: Qt.AlignVCenter
    Layout.leftMargin: 8
    Layout.preferredHeight: {h}
    Layout.preferredWidth: cueRow.implicitWidth + 16
    radius: 5
    color: "#cc141820"
    border.width: 1
    border.color: info ? (info.urgent ? "#ff9f1c" : info.color) : "transparent"
    Row {{
        id: cueRow
        anchors.centerIn: parent
        spacing: 6
        Rectangle {{ width: 8; height: 8; radius: 4; anchors.verticalCenter: parent.verticalCenter; color: parent.parent.info ? parent.parent.info.color : "transparent" }}
        Text {{ anchors.verticalCenter: parent.verticalCenter; font: Style.fontH1; color: Style.font02Color; text: parent.parent.info ? parent.parent.info.label : "" }}
        Text {{ anchors.verticalCenter: parent.verticalCenter; font: Style.makeFont(Style.fontDisplay.family, {px}, Font.Bold); color: parent.parent.info && parent.parent.info.urgent ? "#ffb703" : "#ffffff"; text: parent.parent.info ? parent.parent.info.text : "" }}
    }}
}}
"""
    return "".join(ind + l + "\n" if l else "\n" for l in q.splitlines())


def edits(dev: bool):
    v = ROOT + "Views/"
    prefs = v + "ControlCenter/Tabs/Preferences/"
    wave = [(f"rgbMode ? Style.waveformRGB{b}Color : Style.waveform{b}FadeColor", f'rgbMode ? "{RGB_COLORS[b]}" : ' + three_band(f"waveform{b}FadeColor", b, BAND_FADE)) for b in BAND_COLORS]
    wave += [(f"color{b}Color: Style.waveform{b}Color", f"color{b}Color: {three_band(f'waveform{b}Color', b)}") for b in BAND_COLORS]
    overview = [(f"rgbMode ? Style.waveformRGB{b}Color : Style.waveform{b}Color", f'rgbMode ? "{RGB_COLORS[b]}" : ' + three_band(f"waveform{b}Color", b)) for b in BAND_COLORS]
    overview += [(f"elapsed{b}Color: Style.waveform{b}PastColor", f'elapsed{b}Color: rgbMode ? "{RGB_PAST[b]}" : ' + three_band(f"waveform{b}PastColor", b, BAND_PAST)) for b in BAND_COLORS]
    # Deck overview: drop the 7 px gap under the waveform so it uses the full bar,
    # and stretch the amplitude of tracks quick-imported from a rekordbox USB, whose
    # converted overview looks flat in the strip (scOverviewScale). The default style
    # (Praline) grows up from the bottom edge, so scale from there; only y changes,
    # so needle drop and markers keep their x. The container clips what the
    # stretch pushes past the top. transform must sit outside the anchors group.
    deck_overview = list(overview) + [
        ("				bottomMargin: 7\n", "				bottomMargin: 0\n"),
        ("		objectName: \"Deck_WaveformOverview_ViewContainer\"\n",
         "		objectName: \"Deck_WaveformOverview_ViewContainer\"\n		clip: true\n"),
        ("			visible: waveformOverview.songLoaded && !waveformOverview.isRemoteDeck\n",
         "			transform: Scale { origin.y: overview.height; yScale: root.scOverviewScale(overview.trackData, overview.rgbMode) }\n"
         "			visible: waveformOverview.songLoaded && !waveformOverview.isRemoteDeck\n"),
    ]
    main = DEV_HARNESS if dev else ""
    services = SERVICES_MAIN.replace("@ICONS@", "Skins/UIKit/Icons" if v53() else "UIKit.1/UIKit/Icons").replace("@RGB_SCALE@", str(OVERVIEW_Y_SCALE_RGB)).replace("@BANDS_SCALE@", str(OVERVIEW_Y_SCALE_BANDS)) + (SMART_SYNC_MAIN if v53() else "")
    out = {
        # The flag is still cleared, which is what pressing "Ok" would do.
        ROOT + "Items/MessageBoxes/ImproperDeviceEjectMessageBox.qml": [(
            "      let messageObject = {", "      Planck.librarianController.deviceEjectedImproperly = false\n      return\n      let messageObject = {")],
        # The hook already admits every filesystem type (0x1897dcc / 0x1a28e04), yet an
        # exFAT card still got "reformat the drive to exFAT or FAT32" (#48), likely for
        # a partition with no filesystem (EFI, hidden) next to the one that mounted.
        # Nothing here can repair a drive; one that does not mount stays off the list.
        ROOT + "Items/MessageBoxes/UnsupportedStorageDeviceDetectedMessageBox.qml": [(
            "function onUnsupportedStorageDeviceDetected(name) {\n", "function onUnsupportedStorageDeviceDetected(name) {\n			return\n")],
        # Browse encoder in the performance views: 10 zoom steps instead of stock's 60 (see ZOOM_STEP).
        **({ROOT + f"StateNodes/States/{n}.qml": [
            ('"/Engine/Master/DecrementWaveformZoom"', '"/GUI/Scripted/ZoomOut"'),
            ('"/Engine/Master/IncrementWaveformZoom"', '"/GUI/Scripted/ZoomIn"')]
           for n in ("PerformanceViewState", "PerformanceViewLibraryViewState", "PerformanceViewRemoteHostScreenState", "PerformanceViewStreamingState", "PerformanceViewNoLibraryState")} if v53() else {}),
        ROOT + "EmbeddedMain.qml": [
            (f"	Connections {{\n		target: {window()}\n", services + main + ZOOM_STEP + "\n	Rectangle { anchors.fill: parent; z: -1000; color: \"#000000\" } // UI Scale: no window colour round the edges\n" + f"\n	Connections {{\n		target: {window()}\n"),
            # The overlay (Control Center, popups) fills uiContainer's unscaled box: scale it
            # the same way about the same centre, or its top bar sits above the screen.
            ("		Overlay.overlay.anchors.fill = uiContainer\n",
             "		Overlay.overlay.anchors.fill = uiContainer\n		Overlay.overlay.scale = Qt.binding(function() { return uiContainer.scale })\n"),
            ("		id: uiContainer\n", "		id: uiContainer\n" + LOGIN_DIALOG),
            # UI Scale: QT_SCALE_FACTOR leaves a black band under EGLFS (nativeScreenSize and
            # defaultScreenSize drift apart), so scale uiContainer itself: it is centred in its
            # parent and scales about its centre, so width*scale stays the screen size and
            # everything under it (main view, Source, Control Center, Settings) scales together.
            (f"		width: {window()}.defaultScreenSize.width\n		height: {window()}.defaultScreenSize.height\n		anchors.centerIn: parent\n",
             f"		width: {window()}.defaultScreenSize.width * 100 / (root.scUIScale || 100)\n"
             f"		height: {window()}.defaultScreenSize.height * 100 / (root.scUIScale || 100)\n"
             "		scale: (root.scUIScale || 100) / 100\n"
             "		anchors.centerIn: parent\n"),
        ],
        v + "Common/Waveform/Waveform.qml": wave,
        # Engine offers streamed rows only Prepare; add Stems (see scStemStream).
        v + "BrowseView/Components/Components/SwipeLeftItem.qml": [
            ("	readonly property list<string> actionLabels: leftActions.map( action => action.display.label )\n",
             "	readonly property bool scStem: String(row.modelData && row.modelData.uri || \"\").startsWith(\"streaming://TIDAL/Track/\") && !leftActions.some(action => /stem/i.test(String(action.display && action.display.label || \"\")))\n"
             "	readonly property list<string> actionLabels: leftActions.map( action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : action.display.label ).concat(scStem ? [root.scT(\"Stems\")] : [])\n"),
            ("leftActions.map( action => action.display.commitMessage )", "leftActions.map( action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : action.display.commitMessage ).concat(scStem ? [root.scT(\"Stems\")] : [])"),
            ("leftActions.map( action => action.display.color )", "leftActions.map( action => action.display.color ).concat(scStem ? [Style.systemBlue01Color] : [])"),
            ("				  leftActions[actionIndex].trigger(row.index)\n",
             "				  if (actionIndex < leftActions.length) leftActions[actionIndex].trigger(row.index)\n"
             "				  else root.scStemStream(row.modelData.trackId)\n"),
        ],
        # EngineX: Denon firmware would replace the whole mod, so its prompt never
        # shows; the Update page checks the GitHub repository instead.
        ROOT + "Items/MessageBoxes/FirmwareUpdateAvailableMessageBox.qml": [
            ("	onUpdateAvailableChanged: {\n", "	onUpdateAvailableChanged: {\n		return\n")],
        prefs + "Entries/Version.qml": [(
            "	text: `Engine OS ${versionString} <font color=\\\"${grey}\\\">(${gitHashString})</font>`",
            "	text: `EngineX OS ${root.scVersion || versionString} <font color=\\\"${grey}\\\">(github.com/${root.scUpdateRepo})</font>${root.scAccessCode ? `<br><small>${root.scT(\"Web Console\")} ${root.scConsoleUrl}<br>${root.scT(\"Access Code\")} ${root.scAccessCode}${root.scLanStems ? `<br>${root.scLanStems}` : \"\"}</small>` : \"\"}`")],
        prefs + "Entries/Analytics.qml": [(
            "\t\tchecked = pAnalyticsEnabled.translator.state\n\t\tPlanck.quit(QuitReasons.AnalyticsChange, false)",
            "\t\tpAnalyticsEnabled.translator.state = !pAnalyticsEnabled.translator.state")],
        prefs + "Entries/Update.qml": [
            ("    model: PlanckGUI.views.updaterController.releaseVersionsModel\n", "    model: root.scUpdateModel\n"),
            ("        text: `Engine OS ${model.displayVersion} is available`\n",
             "        text: root.scT(\"New release available:\") + \" EngineX OS \" + model.displayVersion\n"),
            ("        onClicked: PlanckGUI.views.updaterController.updateOnlineForUrl(model.displayVersion, model.imageUrl)\n",
             "        onClicked: root.scConfirmUpdate(model.displayVersion)\n"),
            ('        text: "You\'re running the latest version"\n        visible: control.count < 1\n',
             "        text: root.scUpdateText\n        visible: true\n"),
            # Keep Denon's call: it also rescans USB/SD for <PREFIX>-*-Update.img, otherwise
            # only a replug finds a stick that was in before boot (Firmware Update Mode dialog).
            ("            PlanckGUI.views.updaterController.checkForUpdates()\n",
             "            PlanckGUI.views.updaterController.checkForUpdates()\n            root.scCheckUpdate()\n"),
        ],
        # RA002: stepping the clock while a deck plays disturbs the audio output.
        prefs + "Entries/Time.qml": [(
            '		text: "Sync Time"\n',
            '		text: "Sync Time"\n		enabled: !root.scDeckList().some(d => { const p = Planck.getProperty("/Engine/Deck" + d + "/PlayState"); return !!(p && p.translator && p.translator.state) })\n')],
        v + "BrowseView/Components/Craylist.qml": [
            ("Config.device.hasStemsProEmbeddedRendering && !isCurrentDeviceNetworkDrive" if v53() else "Config.device.hasStemsProEmbeddedRendering", "true"),
            (
                "				var trackInfo = info.payload[key]\n				tracks.push(trackInfo.uniqueID)\n			}\n			Planck.librarianController.stemsQueueManager.startRenderTracks(",
                "				var trackInfo = info.payload[key]\n				if (!root.scStemStream(trackInfo.uniqueID)) tracks.push(trackInfo.uniqueID)\n			}\n			if (tracks.length) Planck.librarianController.stemsQueueManager.startRenderTracks(")],
        v + "Common/WaveformOverview/WaveformOverview.qml": deck_overview,
        v + "Common/WaveformOverview/WaveformOverviewLibrary.qml": overview,
        # Bindings, not aliases. WaveformOverviewRotatable aliases these two
        # properties and must stay untouched: "property alias x: 0" is not a valid
        # alias, and that one error makes the whole JP07 main view unavailable.
        v + "Common/WaveformOverview/WaveformOverviewWithNeedleLock.qml": [
            ("invisibleActiveAreaWidthAtBeginning: isInGridEditor ? 0 : isRightDeck ? 15 : 25", "invisibleActiveAreaWidthAtBeginning: 0"),
            ("invisibleActiveAreaWidthAtEnd: isInGridEditor ? 0 : isRightDeck ? 25 : 15", "invisibleActiveAreaWidthAtEnd: 0"),
        ],
        prefs + "Entries/WaveformStyle.qml": WAVEFORM_ENTRY,
        # Same gate as above: show the Stems List switch and the per-track stems icon.
        prefs + "Entries/StemsList.qml": [("active: Planck.hasStemsPreRenderedPlayback()", "active: true")],
        v + "BrowseView/Components/Components/StemsIcon.qml": [(
            "visible: (Planck.hasStemsPreRenderedPlayback() || (!!row.modelData.isDemoTrack && Config.device.hasDemoContentPreRenderedStems) ) && row.modelData.stems !== 0",
            "visible: row.modelData.stems !== 0")],
        prefs + "Entries/Base/ListEntry.qml": [
            ("function isSelected(text) {", "function isSelected(value) {"),
            ("return entry.currentValue === text\n", "return entry.currentValue === value\n"),
            ("entry.currentValue.includes(text)", "entry.currentValue.includes(value)"),
            ("					text: modelData\n", "					text: root.scT(modelData)\n"),
            ("down: pressed || isSelected(text)", "down: pressed || isSelected(modelData)"),
            ("if(isSelected(text) && entry.defaultIndex", "if(isSelected(modelData) && entry.defaultIndex"),
        ],
        # 5.3.0 moved it up from TabContents/, so its imports lost the "../".
        prefs + ("" if v53() else "TabContents/") + "DeviceTabContent.qml": [
            (f'import "{"" if v53() else "../"}Entries"\n', f'import "{"" if v53() else "../"}Entries"\nimport "{"" if v53() else "../"}Entries/Base" as EntryBase\nimport Planck 1.0\n'),
            ("		PlayerNumber { },\n", LANGUAGE_ENTRY + UI_SCALE_ENTRY + LED_ENTRY + FOUR_DECK_ENTRIES + JOG_ENTRY + (POWER_ENTRY if v53() else "") + "		PlayerNumber { },\n"),
        ],
        **{prefs + ("" if v53() else "TabContents/") + f"{name}TabContent.qml": [
            (f'import "{"" if v53() else "../"}Entries"\n', f'import "{"" if v53() else "../"}Entries"\nimport "{"" if v53() else "../"}Entries/Base" as EntryBase\nimport Planck 1.0\n'),
            (f"{last} {{}}", f"{last} {{}}," + entries.rstrip().rstrip(",")),
        ] for name, last, entries in [
            ("Services", "SpotifyEnable" if v53() else "CloudServices", SERVICES_ENTRIES),
            ("Playback", "KeySyncMode", PLAYBACK_ENTRIES),
            ("CuesLoops", "PausedSavedLoopBehavior", BEAT_JUMP_ENTRIES),
            ("Safety", "KeyAdjust", SAFETY_ENTRIES),
            ("Library", "CollectionBrowseBehavior", LIBRARY_ENTRIES),
        ]},
        v + "ControlCenter/Tabs/LayoutManagerTab.qml": [
            ('import "Preferences/Entries/Base"\n', 'import "Preferences/Entries/Base"\nimport "Preferences/Entries/Base" as EntryBase\n'),
            ("DeckSwap {\n id: deckSwap\n }", "DeckSwap { id: deckSwap }," + VIEW_ENTRIES.replace("@HEADLINE@", "PrefsHeadline" if v53() else "Headline").rstrip().rstrip(",")),
        ],
        prefs + ("" if v53() else "TabContents/") + "MixerTabContent.qml": [
            (f'import "{"" if v53() else "../"}Entries"\n',
             f'import "{"" if v53() else "../"}Entries"\nimport "{"Entries/Base" if v53() else "../Entries/Base"}" as EntryBase\nimport Planck 1.0\n'),
            ("CrossfaderCalibration {}" if v53() else "CrossfaderCalibration { }",
             """EntryBase.ListEntry {
\t\t\t\ttext: "Fader Echo"
\t\t\t\treadonly property var vfx: PlanckGUI.views.transitionFx
\t\t\t\tvalues: ["Off", "All Faders", "Upfader Only"]
\t\t\t\tcurrentValue: !vfx || !vfx.enabled ? (root.scUpfaderEcho ? "Upfader Only" : "Off") : (vfx.triggerSource === 1 ? "Upfader Only" : "All Faders")
\t\t\t\tonSelected: function(index) {
\t\t\t\t\tif (vfx) {
\t\t\t\t\t\tif (index === 0) vfx.enabled = false
\t\t\t\t\t\telse { vfx.enabled = true; vfx.setTriggerSource(index === 2 ? 1 : 2) }
\t\t\t\t\t}
\t\t\t\t\troot.scCall("upfaderEcho=" + (index === 0 ? "0" : "1"))
\t\t\t\t}
\t\t\t},
\t\t\t""" + ("CrossfaderCalibration {}" if v53() else "CrossfaderCalibration { }")),
        ],
        prefs + "Entries/StreamingServices.qml": [
            ("		visible: !isCloudService && !isEaasService\n", '		visible: !isCloudService && !isEaasService && ["TIDAL", "Tidal", "SoundCloud", "Beatport LINK"].indexOf(provider.name) < 0\n'),
            ('import "Base"\n', 'import QtQuick.Layouts 1.12\nimport "Base"\n'),
            ('	label: "Streaming"\n', '	label: "Streaming"\n' + STREAM_SWITCHES),
        ],
        v + "BrowserSource/ConnectedSource.qml": [
            ("	currentIndex: -1\n", "	currentIndex: -1\n" + SOURCE_FOOTER),
            ("		height: 108\n		leftPadding", '		height: ["TIDAL", "Tidal", "SoundCloud", "Beatport LINK"].indexOf(model.name) >= 0 ? 0 : 108\n		visible: ["TIDAL", "Tidal", "SoundCloud", "Beatport LINK"].indexOf(model.name) < 0\n		Component.onCompleted: if (model.name === "TIDAL" || model.name === "Tidal") connectedSource.scTidalRow = this\n		leftPadding'),
        ],
        v + "ControlCenter/Tabs/ControlCenterTab.qml": [
            ("import QtQuick.Layouts 1.12\n", "import QtQuick.Layouts 1.12\nimport EngineOS.UI.Style 1.0\n"),
            ("		DayModeEnable { }\n", "		DayModeEnable { }\n" + WAVE_TILE.replace("@LOW@", BAND_COLORS["Low"]).replace("@MID@", BAND_COLORS["Mid"]).replace("@HIGH@", BAND_COLORS["High"]) + JOG_TILE + UPFADER_ECHO_TILE + (SYNC_TILE if v53() else "")),
        ],
        v + "ControlCenter/ControlCenterTabView.qml": [("text: tabView.hideTabButtonLabel ? \"\" : modelData", "text: tabView.hideTabButtonLabel ? \"\" : root.scT(modelData)")],
        prefs + "PreferencesTabView.qml": [("					text: modelData\n", "					text: root.scT(modelData)\n")],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbViewHeader.qml": [
            ("text: breadcrumbViewHeader.title\n", 'text: ["My Playlists", "My Collection"].indexOf(breadcrumbViewHeader.title) >= 0 ? root.scT(breadcrumbViewHeader.title) : breadcrumbViewHeader.title\n'),
        ],
        # Services switched off in Settings > Streaming stay hidden here, as stock ones do.
        v + "BrowserSource/AvailableSource.qml": AVAILABLE_SOURCE,
        v + "BrowserSource/AvailableSourceDelegate.qml": [
            ("GridView.view.model.index(model.index, 0)", "Planck.librarianController.browserSourceControllerPtr().availableSourceModel().index(model.index, 0)"),
            ("GridView.isCurrentItem || cellContent.pressed", "availableSource.currentIndex === model.index || cellContent.pressed"),
        ],
        # #142: Suppress non-interactive background Wi-Fi and network modal pop-ups
        # ("Wi-Fi Login Error: Failed to connect / Lost connection") that disrupt live gigs
        # and reset browse session / focus state.
        v + "WifiSelect/WifiLoginFailed.qml": [(
            "		function onLoginFailed(reason) {\n",
            "		function onLoginFailed(reason) {\n"
            "			const pShow = Planck.getProperty(\"/GUI/ShowWifiView\")\n"
            "			const wifiVisible = !!((pShow && pShow.translator && pShow.translator.state) ||\n"
            "			                       (target && (target.wifiSelectionVisible || target.passwordDialogVisible)) ||\n"
            "			                       (typeof onBoardingWizardVisible !== \"undefined\" && onBoardingWizardVisible))\n"
            "			const isPlaying = [1, 2, 3, 4].some(d => {\n"
            "				const p = Planck.getProperty(\"/Engine/Deck\" + d + \"/PlayState\")\n"
            "				return !!(p && p.translator && p.translator.state)\n"
            "			})\n"
            "			if (!wifiVisible || isPlaying || reason === \"lost-connection\") return\n",
        )],
        v + "Streaming/StreamingDialog.qml": [
            # This limit is for TIDAL's own qualities; DJ sets here run for hours.
            ("		function onShowTrackTooLong() {\n", "		function onShowTrackTooLong() {\n			return\n"),
            # #142: Suppress background network alerts that disrupt live playback and reset GUI focus
            ("		function onShowNoNetwork() {\n", "		function onShowNoNetwork() {\n			return\n"),
            ("		function onShowLostConnection() {\n", "		function onShowLostConnection() {\n			return\n"),
            ("		function onShowLoginFailed(provider, message) {\n",
             "		function onShowLoginFailed(provider, message) {\n"
             "			const pShowLogin = Planck.getProperty(\"/Client/Librarian/Streaming/ShowLoginPanel\")\n"
             "			const isLoginVisible = !!(pShowLogin && pShowLogin.translator && pShowLogin.translator.state)\n"
             "			const isPlaying = [1, 2, 3, 4].some(d => {\n"
             "				const p = Planck.getProperty(\"/Engine/Deck\" + d + \"/PlayState\")\n"
             "				return !!(p && p.translator && p.translator.state)\n"
             "			})\n"
             "			if (!isLoginVisible || isPlaying) {\n"
             "				Planck.librarianController.streamingInterfaceController.cancelLogoutSpinner()\n"
             "				return\n"
             "			}\n"),
            ("		function onShowGenericError(errorMessage, title) {\n", "		function onShowGenericError(errorMessage, title) {\n			title = root.scBrand(title)\n			errorMessage = root.scBrand(errorMessage)\n"),
            ("		function onShowGenericMessage(message, title) {\n", "		function onShowGenericMessage(message, title) {\n			title = root.scBrand(title)\n			message = root.scBrand(message)\n"),
            ("`${provider.displayName}: ${message}`", "root.scBrand(`${provider.displayName}: ${message}`)"),
            ("`This may be another PRIME player downloading or another ${Planck.librarianController.streamingInterfaceController.selectedProviderName} client playing a track.`", "root.scBrand(`This may be another PRIME player downloading or another ${Planck.librarianController.streamingInterfaceController.selectedProviderName} client playing a track.`)"),
            ("`This will remove all saved waveform data, cues and loops for ${provider.displayName} tracks from the selected source drive. Continue?`", "root.scBrand(`This will remove all saved waveform data, cues and loops for ${provider.displayName} tracks from the selected source drive. Continue?`)"),
            ("`${provider.displayName} session is no longer valid. This may be due to a recent password change. In order to continue, please pair with ${provider.displayName} again.`", "root.scBrand(`${provider.displayName} session is no longer valid. This may be due to a recent password change. In order to continue, please pair with ${provider.displayName} again.`)"),
            ("`Are you sure you want to log out from ${provider.displayName}?`", "root.scBrand(`Are you sure you want to log out from ${provider.displayName}?`)"),
            ("`Please log out of your Engine DJ profile to log out of ${provider.displayName}.`", "root.scBrand(`Please log out of your Engine DJ profile to log out of ${provider.displayName}.`)"),
            ("`Logging out will remove all ${provider.displayName} tracks from deck. Are you sure you want to log out?`", "root.scBrand(`Logging out will remove all ${provider.displayName} tracks from deck. Are you sure you want to log out?`)"),
            ("`${provider.displayName} - ${brief}`", "root.scBrand(`${provider.displayName} - ${brief}`)"),
        ],
        v + "Streaming/StreamingAcceptDenyDialog.qml": [
            (
                '						width: 400\n						height: (src.height / src.width) * width\n						color: Style.icon01Color\n						source: service.provider ? `qrc:/streaming/${service.provider.providerType}/Logo.png` : ""\n',
                '						width: root.scScope && root.scScope !== "tidal" ? 120 : 400\n						height: root.scScope && root.scScope !== "tidal" ? 120 : (src.height / src.width) * width\n						useOriginal: !!(root.scScope && root.scScope !== "tidal")\n						color: Style.icon01Color\n						source: service.provider ? (root.scScope && root.scScope !== "tidal" ? root.scIcon(root.scScope) : `qrc:/streaming/${service.provider.providerType}/Logo.png`) : ""\n',
            ),
            (
                '						text: service.provider ? `Connecting to ${service.provider.displayName}` : ""\n',
                '						text: service.provider ? root.scT("Connecting to") + " " + root.scBrand(service.provider.displayName) : ""\n',
            ),
            ("function onShowAcceptDenyDialog(provider) {\n", "function onShowAcceptDenyDialog(provider) {\n				if (root.scQuiet() || root.scUnasked(provider)) return\n"),
        ],
        v + "Streaming/StreamingUtilityDialog.qml": [(
            '            color: Style.icon01Color\n'
            '            source: dialog.provider ? "qrc:/streaming/%1/Logo_Horizontal.png".arg(dialog.provider.providerType) : ""\n',
            '            Layout.preferredWidth: root.scScope && root.scScope !== "tidal" ? 96 : -1\n'
            '            Layout.preferredHeight: root.scScope && root.scScope !== "tidal" ? 96 : -1\n'
            '            useOriginal: !!(root.scScope && root.scScope !== "tidal")\n'
            '            color: Style.icon01Color\n'
            '            source: dialog.provider ? (root.scScope && root.scScope !== "tidal" ? root.scIcon(root.scScope) : "qrc:/streaming/%1/Logo_Horizontal.png".arg(dialog.provider.providerType)) : ""\n',
        )] if v53() else [
            (
                '			width: 400\n			height: (src.height / src.width) * width\n			color: Style.icon01Color\n			source: dialog.provider ? "qrc:/streaming/%1/Logo.png".arg(dialog.provider.providerType) : ""\n',
                '			width: root.scScope && root.scScope !== "tidal" ? 96 : 400\n			height: root.scScope && root.scScope !== "tidal" ? 96 : (src.height / src.width) * width\n			useOriginal: !!(root.scScope && root.scScope !== "tidal")\n			color: Style.icon01Color\n			source: dialog.provider ? (root.scScope && root.scScope !== "tidal" ? root.scIcon(root.scScope) : "qrc:/streaming/%1/Logo.png".arg(dialog.provider.providerType)) : ""\n',
            ),
        ],
        v + "Streaming/StreamingLoginDialog.qml": [
            (
                '					text: `Log in to ${loginDialog.provider ? loginDialog.provider.displayName: ""} to access your tracks and playlists`\n',
                '					text: root.scBrand(`Log in to ${loginDialog.provider ? loginDialog.provider.displayName: ""} to access your tracks and playlists`)\n',
            ),
            ("function onShowLoginDialog(provider) {\n", "function onShowLoginDialog(provider) {\n			if (root.scQuiet() || root.scUnasked(provider)) return\n"),
        ],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbViewContentContainer.qml": [
            (
                "		providerName: Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName\n",
                "		providerName: root.scBrand(Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName)\n",
            ),
        ],
        # Engine asks TIDAL's image server for a row's artwork; the other services' come
        # from the gateway. The row's provider label marks a streaming row.
        v + "BrowseView/Components/Components/AlbumArt.qml": [(
            'source: row.modelData.albumArt ? row.modelData.albumArt : ""',
            'source: row.streamingProviderName && root.scScope !== "tidal" && row.modelData.song'
            ' ? "http://127.0.0.1:8080/v1/sc5000/art?song=" + encodeURIComponent(root.scPlainTitle(row.modelData.song)) + "&artist=" + encodeURIComponent(row.modelData.artist || "")'
            ' : (row.modelData.albumArt ? row.modelData.albumArt : "")',
        )],
        v + "BrowseView/Components/Components/" + ("BrowseViewBpm.qml" if v53() else "Bpm.qml"): [('text: row.modelData.bpm || ""', 'text: row.modelData.bpm || root.scPreview(row.modelData.providerName)[1]')],
        v + "BrowseView/Components/Components/Key.qml": [('text: row.modelData.key || ""', 'text: row.modelData.key || root.scPreview(row.modelData.providerName)[2] || ""')],
        v + "BrowseView/Components/Components/Song.qml": [('title: row.modelData.song || ""', 'title: root.scPlainTitle(row.modelData.song)')],
        # Selecting a drive that carries an Engine profile asked "Load drive profile to
        # device?" on every insert. Answer "No" unasked: the deck keeps its own settings.
        prefs + "Entries/LoadPreferencesDialog.qml": [(
            "		if(show && !pQuitting) {\n			let messageObject = {\n",
            "		if(show && !pQuitting) {\n"
            "			Qt.callLater(() => { pShow.translator.state = false; pFinshedLoadingProfile.translator.state = true })\n"
            "			return\n"
            "			let messageObject = {\n")],
        # rb-bridge converts rekordbox media at mount. Engine's own slow import of a
        # large library corrupted m.db, so always take the prompt's "Ignore" branch.
        ROOT + "Items/MessageBoxes/RekordBoxImportDialog.qml": [(
            "		function onRequestChanged(devicePath) {\n",
            "		function onRequestChanged(devicePath) {\n"
            "			Planck.librarianController.allowDeviceForNfs(devicePath)\n"
            "			Planck.librarianController.devicesController.triggerDeviceAutoSelection()\n"
            "			return\n",
        )],
        v + "BrowseView/Components/RetryingHint.qml": [
            (
                '		text: "WAITING FOR " + providerName.toUpperCase()\n',
                '		text: root.scT("WAITING FOR") + " " + root.scBrand(providerName).toUpperCase()\n',
            ),
        ],
        # 5.3.0 keeps the old sidebar as LegacySidebar for the legacy browse view.
        v + "Common/Sidebar/" + ("LegacySidebar.qml" if v53() else "Sidebar.qml"): [
            ("source: browserSource.sourceIconUrl\n", 'source: browserSource.sourceIconUrl.indexOf("TIDAL") >= 0 && root.scScope ? root.scIcon(root.scScope) : browserSource.sourceIconUrl\n'),
            ("useOriginal: browserSource.useOriginalIcon\n", 'useOriginal: browserSource.useOriginalIcon || browserSource.sourceIconUrl.indexOf("TIDAL") >= 0\n'),
            # "History" is also compared in Historylist.qml, so translate() leaves it alone everywhere.
            ('						"History"\n', '						root.scT("History")\n'),
        ],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml": [
            ('				objectName: "Browser_Collection_StackView_ViewDelegate"\n',
             '				objectName: "Browser_Collection_StackView_ViewDelegate"\n'
             '				readonly property bool scHidden: breadcrumbView.isStreamingContent && root.scScope !== "tidal" && contentView.StackView.index === 0 && !!modelData && (root.scScope === "netease" ? ["Featured", "Genres", "Moods", "TIDAL Rising"] : ["My Collection", "Featured", "Genres", "Moods", "TIDAL Rising"]).indexOf(modelData.display) >= 0\n'
             # The view assigns visible itself and would undo a binding. These rows
             # end the list, so what stays is blank space below My Playlists.
             '				opacity: scHidden ? 0 : 1\n'),
            ("				enabled: !stackView.busy\n				isDragged:", "				enabled: !stackView.busy && !scHidden\n				isDragged:"),
            ("						return modelData.display\n					}\n					return \"\"", "						return breadcrumbView.isStreamingContent && contentView.StackView.index === 0 ? root.scT(modelData.display) : modelData.display\n					}\n					return \"\""),
        ],
        v + "BrowseView/Components/SortMenu/SortMenu.qml": [
            (
                "    readonly property bool isStreamingProviderWithSort: collectionController.isCurrentCollectionStreaming &&\n"
                "        Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                "    readonly property bool isStreamingProviderWithSort: collectionController.isCurrentCollectionStreaming\n",
            ),
            (
                "    onSortRoleChanged: {\n"
                "        if (dialog) {\n"
                "            dialog.sortRole = sortRole\n"
                "        }\n"
                "        responsiveBrowseView.sortRole = sortRole\n"
                "    }\n",
                "    onSortRoleChanged: {\n"
                "        if (dialog) {\n"
                "            dialog.sortRole = sortRole\n"
                "        }\n"
                "        responsiveBrowseView.sortRole = sortRole\n"
                "        try {\n"
                "            var scReq = new XMLHttpRequest()\n"
                "            scReq.open(\"POST\", \"http://127.0.0.1:8080/v1/sc5000/ui\")\n"
                "            scReq.setRequestHeader(\"Content-Type\", \"application/json\")\n"
                "            scReq.send(JSON.stringify({\n"
                '                "streamSortRole": sortRole,\n'
                '                "streamSortOrder": sortOrder === Qt.DescendingOrder ? "desc" : "asc"\n'
                "            }))\n"
                "        } catch (e) {}\n"
                "    }\n",
            ),
        ],
        v + "BrowseView/Components/Components/SortRole.qml": [(
            "        text: parent.displayData\n",
            '        text: root.scDualColumn && (row.modelData.bpm || "") !== "" ? ((row.modelData.bpm || "") + (parent.displayData ? " · " + parent.displayData : "")) : parent.displayData\n',
        )],
    }
    if v53():
        # The stock "Stems Support" row downloads Denon's stems-child-process; EngineX OS ships
        # and mounts its own, and an Install there could replace that mount.
        out[prefs + "Entries/StemsDownload.qml"] = [("active: manager && manager.canModifyInstall", "active: false")]
        out[v + "RecordingView/RecMainView.qml"] = [(
            "    readonly property bool isBlockedByStreaming: pBlockedByStreaming && pBlockedByStreaming.translator\n"
            "        ? pBlockedByStreaming.translator.state\n"
            "        : false\n",
            "    readonly property bool isBlockedByStreaming: false\n"
            "    Connections {\n"
            "        target: pBlockedByStreaming ? pBlockedByStreaming.translator : null\n"
            "        function onStateChanged() {\n"
            "            if (pBlockedByStreaming.translator.state) pBlockedByStreaming.translator.state = false\n"
            "        }\n"
            "    }\n",
        )]
        # #99: five views kept by stream-sync (scView* in EmbeddedMain) and Save View.
        out[ROOT + "Views/ControlCenter/Tabs/Preferences/Entries/LayoutViewTab.qml"] = [
            ("        model: [\n            ViewStateControllerInterface.View.View1,\n            ViewStateControllerInterface.View.View2,\n            ViewStateControllerInterface.View.View3,\n        ]\n", "        model: 5\n"),
            ("            Layout.preferredWidth: 120\n", "            Layout.preferredWidth: 100\n"),
            ("            property bool isSelected: modelData === control.currentView\n", "            property bool isSelected: index + 1 === root.scViewActive\n"),
            ("            onClicked: control.currentView = modelData\n", "            onClicked: root.scViewSelect(index + 1)\n"),
            ('    FlatButton {\n        objectName: "ResetCurrentViewButton"\n', '    FlatButton {\n        objectName: "SaveCurrentViewButton"\n        Layout.preferredHeight: 50\n        Layout.alignment: Qt.AlignVCenter | Qt.AlignRight\n        text: "Save View"\n        onClicked: root.scViewSave()\n        background: Rectangle {\n            color: Style.surface03Color\n        }\n    }\n\n    FlatButton {\n        objectName: "ResetCurrentViewButton"\n'),
        ]
        # #49: Engine's own load filter (all load paths: menu, direct, browse knob) also
        # refuses decks that belong to the other player in the 4-deck layout.
        out[ROOT + "PropertyFilter/LoadToDeckFilter.qml"] = [
            ("        active: songLoading || lockLine\n", "        active: songLoading || lockLine || root.scDeckLocked(deckIndex)\n")]
        # #49: a networked player's MASTER LED shows the unit's network master, so in
        # the 4-deck layout it stayed lit while a shadow deck (a CDJ) held master.
        # There (SC5000/SC6000 boards with isPlayer off, i.e. 4-deck; Config.device.name
        # is the board, "JP07", and isJP is undefined) it shows the deck's own
        # master; every other device keeps stock.
        out["ControlSurfaceModules/ControlSurfaceModules/Sync.qml"] = [(
            "            readonly property string isMasterPath: Config.network.hasSyncing\n",
            "            readonly property string isMasterPath: Config.network.hasSyncing &&\n"
            "                    !(!Config.device.isPlayer && [\"JP07\", \"JP08\", \"JP13\", \"JP14\"].indexOf(Config.device.name) >= 0)\n",
        ), (
            # Stock dims a non-master LED (1), which reads as lit; here it is off.
            "                        } else {\n                            1\n                        }\n",
            "                        } else {\n                            !Config.device.isPlayer && [\"JP07\", \"JP08\", \"JP13\", \"JP14\"].indexOf(Config.device.name) >= 0 ? 0 : 1\n                        }\n",
        )]
        # #49: a swipe loads the role's own decks (1+3: left 1, right 3), not the stock
        # left/right layer deck. Only "Deck N" actions; Prepare List etc. stay stock.
        for side, right in (("left", "false"), ("right", "true")):
            a = side + "Actions"
            if side == "left":
                out[ROOT + f"Views/BrowseView/Components/Components/Swipe{side.title()}Item.qml"] = [
                    (f"    readonly property list<string> actionLabels: {a}.map(action => action.display.label)\n",
                     "    readonly property bool scStem: !!(row.isStreamingTrack || row.streamingProviderName || String(row.modelData && row.modelData.uri || \"\").startsWith(\"streaming://TIDAL/Track/\") || (row.modelData && row.modelData.trackId > 0)) && !leftActions.some(action => /stem/i.test(String(action.display && action.display.label || \"\")))\n"
                     f"    readonly property int scDeck: root.scSwipeDeck({right})\n"
                     "    function scLabel(s) { return scDeck > 0 ? String(s).replace(/\\d+/, scDeck) : s }\n"
                     f"    readonly property list<string> actionLabels: {a}.map(action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : scLabel(action.display.label)).concat(scStem ? [root.scT(\"Stems\")] : [])\n"),
                    (f"    readonly property list<string> actionCommitMessages: {a}.map(action => action.display.commitMessage)\n",
                     f"    readonly property list<string> actionCommitMessages: {a}.map(action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : scLabel(action.display.commitMessage)).concat(scStem ? [root.scT(\"Stems\")] : [])\n"),
                    (f"    readonly property list<color> actionColors: {a}.map(action => action.display.color)\n",
                     f"    readonly property list<color> actionColors: {a}.map(action => action.display.color).concat(scStem ? [Style.systemBlue01Color] : [])\n"),
                    (f"        {a}[actionIndex].trigger(row.index)\n",
                     "        if (actionIndex >= leftActions.length || /stem/i.test(String(leftActions[actionIndex].display && leftActions[actionIndex].display.label || \"\"))) {\n"
                     "            // streamed rows go through the gateway; local USB/SD tracks are rendered by Engine itself (also via the stock Stem entry)\n"
                     "            if (!root.scStemStream(row.modelData.trackId)) Planck.librarianController.stemsQueueManager.startRenderTracks(String(row.modelData.trackId))\n"
                     f"        }} else if (scDeck > 0 && /\\d/.test({a}[actionIndex].display.label)) {{\n"
                     "            trackList.pCurrentIndex.translator.unnormalized = row.index\n"
                     "            Planck.getProperty(\"/Client/Librarian/LoadToDeck%1\".arg(scDeck)).translator.state = true\n"
                     f"        }} else {a}[actionIndex].trigger(row.index)\n"),
                    ("    textSwipeInvalid: ((readyToCommit", "    textSwipeInvalid: scDeck > 0 ? \"\" : ((readyToCommit"),
                ]
            else:
                out[ROOT + f"Views/BrowseView/Components/Components/Swipe{side.title()}Item.qml"] = [
                    (f"    readonly property list<string> actionLabels: {a}.map(action => action.display.label)\n",
                     f"    readonly property int scDeck: root.scSwipeDeck({right})\n"
                     "    function scLabel(s) { return scDeck > 0 ? String(s).replace(/\\d+/, scDeck) : s }\n"
                     f"    readonly property list<string> actionLabels: {a}.map(action => scLabel(action.display.label))\n"),
                    (f"    readonly property list<string> actionCommitMessages: {a}.map(action => action.display.commitMessage)\n",
                     f"    readonly property list<string> actionCommitMessages: {a}.map(action => scLabel(action.display.commitMessage))\n"),
                    (f"        {a}[actionIndex].trigger(row.index)\n",
                     f"        if (scDeck > 0 && /\\d/.test({a}[actionIndex].display.label)) {{\n"
                     "            trackList.pCurrentIndex.translator.unnormalized = row.index\n"
                     "            Planck.getProperty(\"/Client/Librarian/LoadToDeck%1\".arg(scDeck)).translator.state = true\n"
                     f"        }} else {a}[actionIndex].trigger(row.index)\n"),
                    ("    textSwipeInvalid: ((readyToCommit", "    textSwipeInvalid: scDeck > 0 ? \"\" : ((readyToCommit"),
                ]
    if v53():
        # Stock settings a board flag hides although Engine registers the property on every
        # board measured (JP07, JP11, JP13, JC11, NH08 in the #77 sandbox dump). Each gate
        # keeps its flag and adds the property check, so a board without it stays stock.
        silent = '!!(Planck.getProperty("/Engine/Deck1/SilentCue") && Planck.getProperty("/Engine/Deck1/SilentCue").translator)'
        for rel, fixes in {
            # Silent Cue: the Censor button mutes the deck instead of reversing it.
            prefs + "Entries/CensorBehavior.qml": [
                ("active: Config.device.hasSilentCue\n", f"active: Config.device.hasSilentCue || {silent}\n")],
            "ControlSurfaceModules/ControlSurfaceModules/DeckAssignmentConfig.qml": [(
                "readonly property bool censorIsSilentCue: Config.device.hasSilentCue &&\n",
                'readonly property QObjProperty pSilentCueProbe: Planck.getProperty(activeDeckPath + "SilentCue")\n'
                "    readonly property bool censorIsSilentCue: (Config.device.hasSilentCue ||\n"
                "        !!(pSilentCueProbe && pSilentCueProbe.translator)) &&\n")],
            # PRIME GO and Mixstream Pro have Engine's limiter (Config.audio.hasLimiter,
            # Off / 0-18 dB) but not its setting.
            prefs + "Entries/MasterLimiter.qml": [
                ("active: Config.ui.preferences.hasMasterLimiter\n",
                 "active: Config.ui.preferences.hasMasterLimiter || Config.audio.hasLimiter\n")],
            # #104: Fader Echo / TransitionFx 3-state cycling: Off -> All Faders -> Upfader Only -> Off
            v + "ControlCenter/Tabs/ControlCenter/Entries/TransitionFxEnable.qml": [
                ('text: "Fader Echo"\n        icon.source: "qrc:/Skins/UIKit/Icons/Faderecho.png"\n        checked: view.enabled\n        onClicked: view.enabled = !view.enabled',
                 'text: !view.enabled ? "Fader Echo" : (view.triggerSource === 1 ? "Fader Echo: Up" : "Fader Echo: All")\n        icon.source: "qrc:/Skins/UIKit/Icons/Faderecho.png"\n        checked: view.enabled\n        onClicked: { if (!view.enabled) { view.setTriggerSource(2); view.enabled = true } else if (view.triggerSource === 2) { view.setTriggerSource(1) } else { view.enabled = false } }'),
            ],
            v + "StatusBar/Buttons/TransitionFxButton.qml": [
                ('    checked: active\n    onClicked: transitionFx.enabled = !transitionFx.enabled',
                 '    checked: active\n    onClicked: { if (!transitionFx.enabled) { transitionFx.setTriggerSource(2); transitionFx.enabled = true } else if (transitionFx.triggerSource === 2) { transitionFx.setTriggerSource(1) } else { transitionFx.enabled = false } }'),
            ],
            prefs + "Entries/TransitionFxSection.qml": [
                ('        TransitionFxSelectEffect {\n            view: transitionFx.view\n        }',
                 '        TransitionFxSelectEffect {\n            view: transitionFx.view\n        }\n        ListEntry {\n            text: "Trigger Source"\n            readonly property var vfx: transitionFx.view\n            values: ["Off", "All Faders", "Upfader Only"]\n            currentValue: !vfx || !vfx.enabled ? "Off" : (vfx.triggerSource === 1 ? "Upfader Only" : "All Faders")\n            onSelected: function(index) {\n                if (!vfx) return\n                if (index === 0) vfx.enabled = false\n                else { vfx.enabled = true; vfx.setTriggerSource(index === 2 ? 1 : 2) }\n            }\n        }'),
            ],
        }.items():
            assert not isinstance(out.get(rel), str), rel
            out.setdefault(rel, []).extend(fixes)
    # UI Scale < 100 %: uiContainer and the Overlay holding Control Center and popups are
    # defaultScreenSize * 100 / scale; views sized to defaultScreenSize covered only its
    # top-left part and the Drawer's modal dim showed as a bare band right and below.
    # These files run in main.qml's context (productLayoutComponent: EmbeddedView {}),
    # which sees `root` but not EmbeddedMain's ids: use its stock alias for uiContainer.
    W, H = f"{window()}.defaultScreenSize.width", f"{window()}.defaultScreenSize.height"
    ui = 'typeof root !== "undefined" && root && root.transformed'
    UW, UH = f"({ui} ? root.transformed.width : {W})", f"({ui} ? root.transformed.height : {H})"
    size = [(f"width: {W}\n", f"width: {UW}\n"), (f"height: {H}\n", f"height: {UH}\n")]
    scaled = {
        v + "ControlCenter/ControlCenter.qml": size + [(f"height: {H} +", f"height: {UH} +")],
        v + "ControlCenter/ControlCenterTabView.qml": [(f"{H} - height", f"{UH} - height")],
        v + "StatusBar/AbstractStatusBar.qml": [(f"dragPosition * {H}", f"dragPosition * {UH}")],
        v + "EmbeddedBase.qml": [(f"implicitWidth: {W}\n", f"implicitWidth: {UW}\n"), (f"implicitHeight: {H}\n", f"implicitHeight: {UH}\n")],
        v + "StorageMode/StorageModeOverlay.qml": size,
        ROOT + "Items/Pads/PadModeSelectDialogue.qml": size,
        prefs + "Entries/private/ColorWheelDialog.qml": size,
        # Settings -> Device -> Pad LED Brightness also dims the on-screen pads.
        ROOT + "Items/Pads/Pads.qml": [("id: pads\n", "id: pads\n    opacity: typeof root !== \"undefined\" && root && root.scLedBrightness ? Math.max(0.2, Math.min(1, root.scLedBrightness / 100)) : 1\n")],
    }
    if v53():
        scaled[v + "MainView/Fx/DJFxSelectCarousel.qml"] = size
        # Pad LED Brightness on the hardware pads. The controller QML runs in its own engine
        # (no `root`), so its one GlobalAssignmentConfig reads ledBrightness from stream-sync
        # and the pad LEDs scale what ColorOutputAssignment sends.
        scaled["ControlSurfaceModules/ControlSurfaceModules/GlobalAssignmentConfig.qml"] = [(
            '    objectName: "Global Assignment Config"\n',
            '    id: scGlobalConfig\n'
            '    objectName: "Global Assignment Config"\n'
            '    property real scPadBrightness: 1\n'
            '    property bool scSwapLoopOrder: false\n'
            '    Timer {\n'
            '        interval: 10000; running: true; repeat: true; triggeredOnStart: true\n'
            '        onTriggered: {\n'
            '            const x = new XMLHttpRequest()\n'
            '            x.onreadystatechange = function() {\n'
            '                if (x.readyState !== XMLHttpRequest.DONE || x.status !== 200) return\n'
            '                try {\n'
            '                    const j = JSON.parse(x.responseText)\n'
            '                    const n = parseInt(j.ledBrightness)\n'
            '                    if (n >= 20 && n <= 100) scGlobalConfig.scPadBrightness = n / 100\n'
            '                    scGlobalConfig.scSwapLoopOrder = !!j.swapLoopOrder\n'
            '                } catch (e) {}\n'
            '            }\n'
            '            x.open("GET", "http://127.0.0.1:8080/v1/sc5000/ui")\n'
            '            x.send()\n'
            '        }\n'
            '    }\n')]
        pad_brt = '    scBrightness: typeof globalConfig !== "undefined" && globalConfig && globalConfig.scPadBrightness ? globalConfig.scPadBrightness : 1\n'
        # #135: Swap Loop / Beat Jump: the Auto Loop knob beat-jumps unshifted and resizes the loop with Shift.
        scaled["ControlSurfaceModules/ControlSurfaceModules/private/AutoLoopKnob.qml"] = [(
            "        output: globalConfig.shift ? shiftTurnOutput : normalTurnOutput\n",
            '        output: (globalConfig.shift !== (typeof globalConfig !== "undefined" && globalConfig && !!globalConfig.scSwapLoopOrder)) ? shiftTurnOutput : normalTurnOutput\n'),
            # JP07's AutoLoop has no loopInactiveShiftTurnAction (default None), so the swapped turn did nothing.
            ("        readonly property bool beatJumpEnabled: module.loopInactiveShiftTurnAction === Action.BeatJump &&",
             '        readonly property bool beatJumpEnabled: (module.loopInactiveShiftTurnAction === Action.BeatJump || (typeof globalConfig !== "undefined" && globalConfig && !!globalConfig.scSwapLoopOrder)) &&')]
        # #135: touch Beat Jump on the deck screen: icon = jump back, size text = next size
        # (cycles through 16/32/64 phrase jumps), » = jump forward. BeatJump gets its deck
        # from deckIndex; the five users pass theirs (default 1).
        bj = "Skins/EngineOS/UI/CompositeControls/"
        def bj_tap(action, call="p.translator.state = true"):
            return ('        MouseArea {\n            anchors.fill: parent\n            anchors.margins: -6\n'
                    f'            onClicked: {{ const p = Planck.getProperty("/Engine/Deck" + beatJump.deckIndex + "/Track/BeatJump/{action}"); if (p && p.translator) {call} }}\n'
                    '        }\n')
        scaled[bj + "Deck/DeckItem/BeatJump.qml"] = [
            ("    required property BeatJumpViewInterface view\n",
             "    required property BeatJumpViewInterface view\n    property int deckIndex: 1\n"),
            ("        SequentialAnimation {\n            id: beatJumpImageAnimation\n",
             bj_tap("JumpBack") + "\n        SequentialAnimation {\n            id: beatJumpImageAnimation\n"),
            ("        SequentialAnimation {\n            id: beatJumpTextAnimation\n",
             bj_tap("BeatJumpIndex", "p.translator.incValue(0, 1, 0, true)") + "\n        SequentialAnimation {\n            id: beatJumpTextAnimation\n"),
            ("                to: Style.font01Color\n                duration: Style.colorAnimationDuration\n            }\n        }\n    }\n",
             "                to: Style.font01Color\n                duration: Style.colorAnimationDuration\n            }\n        }\n    }\n\n"
             '    Text {\n        objectName: "Deck_BeatJump_Forward"\n        verticalAlignment: Text.AlignVCenter\n'
             '        font: beatJumpText.font\n        color: Style.font01Color\n        text: "\\u00bb"\n\n'
             + bj_tap("JumpForward") + "    }\n"),
        ]
        scaled[bj + "Deck/DeckSharedItems/DeckSharedItems.qml"] = [
            ("            view: deckSharedItems.deckView.beatJumpView\n",
             "            view: deckSharedItems.deckView.beatJumpView\n            deckIndex: deckSharedItems.deckData.deckIndex\n")]
        scaled[bj + "DeckPerformanceData/DeckPerformanceDataContent.qml"] = [
            ("        view: deckPerformanceDataContent.deckView.beatJumpView\n",
             "        view: deckPerformanceDataContent.deckView.beatJumpView\n        deckIndex: deckPerformanceDataContent.deckView.deckData.deckIndex\n"),
            ("        visible: Config.audio.deck.hasBeatJump\n    }\n",
             "        visible: Config.audio.deck.hasBeatJump\n    }\n\n" + cue_capsule("    ", "deckPerformanceDataContent.deckData.deckIndex", 30, 20))]
        scaled[bj + "DeckPerformanceData/PortraitDeckPerformanceDataContent.qml"] = [
            ("                view: deckPerformanceDataContent.deckView.beatJumpView\n",
             "                view: deckPerformanceDataContent.deckView.beatJumpView\n                deckIndex: deckPerformanceDataContent.deckView.deckData.deckIndex\n")]
        scaled[bj + "DeckOverview/DeckOverview.qml"] = [
            ("                    view: control.deckView.beatJumpView\n",
             "                    view: control.deckView.beatJumpView\n                    deckIndex: control.deckView.deckData.deckIndex\n")]
        scaled[bj + "DeckTrackData/GroupedMiniDeckTrackDataContent.qml"] = [
            ("                    view: deckTrackDataContent.deckView.beatJumpView\n",
             "                    view: deckTrackDataContent.deckView.beatJumpView\n                    deckIndex: deckTrackDataContent.deckIndex\n"),
            ("                    visible: Config.audio.deck.hasBeatJump\n                }\n",
             "                    visible: Config.audio.deck.hasBeatJump\n                }\n\n" + cue_capsule("                ", "deckTrackDataContent.deckIndex", 24, 16))]
        scaled["ControlSurfaceModules/ControlSurfaceModules/private/ActionPadRGBLed.qml"] =[("    enabled: deckConfig.inDJMode\n", "    enabled: deckConfig.inDJMode\n" + pad_brt)]
        scaled["ControlSurfaceModules/ControlSurfaceModules/private/QuadPadRGBLed.qml"] = [("    idx: note\n", "    idx: note\n" + pad_brt)]
        scaled["airAssignments/OutputAssignment/ColorOutputAssignment.qml"] = [
            ("	property bool debug: false;\n",
             "	property bool debug: false;\n"
             "	// EngineX: 0.2-1, set by the pad LEDs (Pad LED Brightness); 1 sends the stock colour.\n"
             "	property real scBrightness: 1\n"
             "	onScBrightnessChanged: if (enabled) device.sendColor(channel, idx, privatePart.scScaled())\n"),
            ("				device.sendColor(channel, idx, outputColor);\n", "				device.sendColor(channel, idx, scScaled());\n"),
            ("		id: privatePart\n",
             "		id: privatePart\n"
             "		function scScaled() { const b = scBrightness, c = outputColor; return b >= 0.99 ? c : Qt.rgba(c.r * b, c.g * b, c.b * b, c.a) }\n"),
        ]
        # Stock 5.3.0 defects that surface mid-set when the deck booted offline (path is
        # /Engine/DeckN/PlayState, as PausePlayingDeckHint.qml reads it).
        playing = '(() => { for (let i = 1; i <= 4; i++) { const p = Planck.getProperty("/Engine/Deck" + i + "/PlayState"); if (p && p.translator && p.translator.state) return true } return false })()'
        scaled[v + "WifiSelect/WifiLoginFailed.qml"] = [
            # wpa_supplicant's background retries fail 30-60 s after boot; only report while the Wi-Fi screen is open.
            ("        function onLoginFailed(reason) {\n", "        function onLoginFailed(reason) {\n            if (!target.wifiSelectionVisible) return\n"),
        ]
        scaled[v + "EmbeddedView.qml"] = [
            # A background cloud/profile source timing out cleared currentDevice and threw the DJ out of the browser.
            ("                if (!failToLoginEaasSource) {\n                    pShowBrowseView.translator.state = false\n",
             "                if (numberOfConnectedDevices > 0) Planck.librarianController.devicesController.autoSelectDevice()\n"
             "                if (currentDevice.length > 0) { pShowBrowseView.translator.state = true; return }\n"
             f"                if (!failToLoginEaasSource && !{playing}) {{\n                    pShowBrowseView.translator.state = false\n"),
        ]
        # Stock forgot "= true": the Wi-Fi button of these two dialogs did nothing.
        scaled[v + "Streaming/StreamingDialog.qml"] = [
            ('                    Planck.getProperty("/GUI/ShowWifiView").translator.state\n                }\n            })\n        }\n        function onSessionInvalidated',
             '                    Planck.getProperty("/GUI/ShowWifiView").translator.state = true\n                }\n            })\n        }\n        function onSessionInvalidated'),
            ('                    Planck.getProperty("/GUI/ShowWifiView").translator.state\n                }\n            })\n        }\n\n        function onShowLogoutMessage',
             '                    Planck.getProperty("/GUI/ShowWifiView").translator.state = true\n                }\n            })\n        }\n\n        function onShowLogoutMessage'),
        ]
        scaled[ROOT + "Items/MessageBoxes/TimeSyncDialog.qml"] = [
            ("        function onNetworkNotConnected() {\n", "        function onNetworkNotConnected() {\n            if (" + playing + ") return\n"),
        ]
        # 4-deck (#49): the library deck bar lists Deck 1-4 left to right (stock groups
        # by side: 1, 3, 2, 4), lights only this player's decks, and names the deck of a
        # loaded track. Elsewhere `root` is not EmbeddedMain or scFourDeckRole is "": stock.
        role = 'typeof root !== "undefined" && root && root.scFourDeckRole'
        tabs = "Skins/EngineOS/UI/CompositeControls/Decks/DeckTabs.qml"
        scaled[tabs] = [
            ("                beatGridEditor ? beatGridEditor.deckLabels() : []\n",
             "                const labels = beatGridEditor ? beatGridEditor.deckLabels() : []\n"
             "                const deck = l => { const m = viewController.getDeckTabViewModel(l); return m ? m.deckNumber : 0 }\n"
             f"                return {role} ? labels.slice().sort((a, b) => deck(a) - deck(b)) : labels\n"),
            ("                    activeDeck: microDeckLoader.viewModel.activeDeck\n",
             f"                    activeDeck: {role} ? !root.scDeckLocked(microDeckLoader.viewModel.deckNumber) : microDeckLoader.viewModel.activeDeck\n"),
            ("                    greyOut: !microDeckLoader.viewModel.activeDeck && !deckTabs.isTouchable\n",
             f"                    greyOut: {role} ? root.scDeckLocked(microDeckLoader.viewModel.deckNumber) : !microDeckLoader.viewModel.activeDeck && !deckTabs.isTouchable\n"),
        ]
        scaled["Skins/EngineOS/UI/CompositeControls/Deck/MicroDeck.qml"] = [(
            "                text: microDeck.loadedDeckSongName\n",
            f"                text: ({role} ? microDeck.deckIndex + \" · \" : \"\") + microDeck.loadedDeckSongName\n")]
        # In the 4-deck layout the deck colour came out white (measured on a 1+3 deck with
        # blue PlayerColor1/2), which has no gradient and draws a black strip, and an unset
        # PlayerColor3/4 is black: use the deck's PlayerColor preference, then that of the
        # deck on the same side (1 or 2). White and black count as unset.
        scaled[v + "MainView/DeckInfo/DeckColor/DeckColor.qml"] = [(
            '    readonly property color playerColor: deckView ? deckView.deckData.deckColor : "transparent"\n',
            '    readonly property color scOwnColor: deckView ? deckView.deckData.deckColor : "transparent"\n'
            '    function scPref(n) { const p = Planck.getProperty("/Client/Preferences/Profile/Application/PlayerColor" + n); return p && p.translator ? p.translator.color : "transparent" }\n'
            '    function scBlank(c) { return c.a === 0 || (c.r === c.g && c.g === c.b && (c.r === 0 || c.r === 1)) }\n'
            '    readonly property color playerColor: !deckView || !scBlank(scOwnColor) ? scOwnColor\n'
            '        : !scBlank(scPref(deckIndex)) ? scPref(deckIndex)\n'
            '        : deckIndex > 2 && !scBlank(scPref(deckIndex - 2)) ? scPref(deckIndex - 2) : scOwnColor\n')]
    for rel, fixes in scaled.items():
        assert not isinstance(out.get(rel), str), rel
        out.setdefault(rel, []).extend(fixes)
    for rel, fixes in legacy_qml_fixes().items():
        assert not isinstance(out.get(rel), str), rel  # a whole-file replacement already owns it
        out.setdefault(rel, []).extend(fixes)
    return out


def legacy_qml_fixes():
    """Fixes for stock Engine 5.0.4 QML defects, applied before translation (so
    the LOG IN / LOG OUT labels below are translated too). Every anchor must
    match exactly once; patch() stops the build otherwise."""
    v = ROOT + "Views/"
    # Focus comes back on the next event-loop turn, and never while a dialog or
    # a text field (on-screen keyboard) holds it.
    restore_guard = (
        "if(!visible || !isCurrentFocusArea || activeFocus || root.dialogOpen || root.messageBoxOpen) return\n"
        f"				const item = {window()}.activeFocusItem\n"
        "				if(item && item.inputMethodHints !== undefined) return\n"
    )
    return {
        # CF008: Back/Forward on the Source screen fired an action nothing handles
        # there; /GUI/CloseDialogs is what SourceScreen.qml listens to. Arrow keys
        # and Enter/Escape drive it like the encoder.
        ROOT + "StateNodes/States/SourceScreenState.qml": [(
            '		back: "/Qml/CloseCurrentDialog"\n		forward: "/Qml/CloseCurrentDialog"\n',
            '		back: "/GUI/CloseDialogs"\n		forward: "/GUI/CloseDialogs"\n',
        )],
        v + "BrowserSource/SourceScreen.qml": [(
            "	readonly property alias expandTransitionRunning: expandTransition.running\n",
            "	readonly property alias expandTransitionRunning: expandTransition.running\n"
            "	Keys.enabled: sourceScreen.visible && !root.messageBoxOpen\n"
            "	Keys.onUpPressed: controller.navigation().decrementIndex()\n"
            "	Keys.onDownPressed: controller.navigation().incrementIndex()\n"
            "	Keys.onReturnPressed: controller.navigation().enter()\n"
            "	Keys.onEnterPressed: controller.navigation().enter()\n"
            "	Keys.onEscapePressed: controller.close()\n",
        )],
        # CF009 / CF010 / H143: after a view switch or Instant Double the track list
        # and breadcrumb stack grabbed focus mid-transition and the encoder lost it.
        v + "BrowseView/Components/TrackList.qml": [
            (
                "	readonly property string selectedProviderDisplayName: Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName\n",
                "	readonly property string selectedProviderDisplayName: root.scBrand(Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName)\n",
            ),
            ("			onIsCurrentFocusAreaChanged: {\n				if(visible && !activeFocus && isCurrentFocusArea) {\n					forceActiveFocus()\n				}\n",
             "			function scRestoreFocusNow() {\n				" + restore_guard + "				forceActiveFocus()\n			}\n\n"
             "			onIsCurrentFocusAreaChanged: {\n				if(visible && !activeFocus && isCurrentFocusArea) {\n					Qt.callLater(scRestoreFocusNow)\n				}\n"),
            ("					if(visible && isCurrentFocusArea && !activeFocus) {\n						forceActiveFocus()\n					}\n",
             "					if(visible && isCurrentFocusArea && !activeFocus) {\n						Qt.callLater(scRestoreFocusNow)\n					}\n"),
            (
                "!(streamingFlags & StreamingTrackFlags.BlocksRecording)",
                "true",
            ),
            (
                "id: disableRecordOfStreaming\n            visible: trackList.recording && trackList.isStreamingCollection\n",
                "id: disableRecordOfStreaming\n            visible: false\n",
            ),
        ],
        v + "BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml": [(
            "						contentView.currentRowIndex = index\n					}\n					breadcrumbView.push()\n				}\n",
            "						contentView.currentRowIndex = index\n					}\n					breadcrumbView.push()\n"
            "					Qt.callLater(function() {\n"
            "						if(!breadcrumbView.visible || !breadcrumbView.isCurrentFocusArea || root.dialogOpen || root.messageBoxOpen) return\n"
            f"						const item = {window()}.activeFocusItem\n"
            "						if(item && item.inputMethodHints !== undefined) return\n"
            "						breadcrumbView.forceActiveFocus()\n"
            "					})\n"
            "				}\n",
        )],
        # CF013: after logging out of a cloud drive the Account button still said
        # LOG OUT and could not log back in. Only an explicit loggedIn === false
        # switches it: if the provider type has no such property the stock
        # logout keeps working.
        v + "Streaming/CloudUtilityDialog.qml": [(
            '            label: "Account"\n'
            '            buttons: ["Log Out"]\n'
            "            onClicked: label => {\n"
            '                Planck.getProperty("/Client/Librarian/Streaming/LogoutSpinner").translator.state = true\n'
            "                Planck.librarianController.streamingInterfaceController.displayLogout(dialog.provider)\n"
            "            }\n"
            "            attachment: Elements.BusyIndicator {\n"
            '                running: Planck.getProperty("/Client/Librarian/Streaming/LogoutSpinner").translator.state\n',
            "            id: scAccount\n"
            "            readonly property bool scLoggedOut: !!dialog.provider && dialog.provider.loggedIn === false\n"
            '            label: "Account"\n'
            '            buttons: [scLoggedOut ? "Log In" : "Log Out"]\n'
            "            onClicked: label => {\n"
            "                const service = dialog.provider\n"
            "                if (!service) return\n"
            "                const controller = Planck.librarianController.streamingInterfaceController\n"
            "                if (scLoggedOut) {\n"
            "                    dialog.close()\n"
            "                    controller.showAddAccountDialog(service)\n"
            "                } else {\n"
            '                    Planck.getProperty("/Client/Librarian/Streaming/LogoutSpinner").translator.state = true\n'
            "                    controller.displayLogout(service)\n"
            "                }\n"
            "            }\n"
            "            attachment: Elements.BusyIndicator {\n"
            '                running: !scAccount.scLoggedOut && Planck.getProperty("/Client/Librarian/Streaming/LogoutSpinner").translator.state\n',
        )] if v53() else [(
            '			buttonText: "LOG OUT"\n'
            "			showSpinner: pLogoutSpinner.translator.state\n"
            "			onClicked: {\n"
            "				pLogoutSpinner.translator.state = true\n"
            "				Planck.librarianController.streamingInterfaceController.displayLogout(provider)\n"
            "			}\n",
            # The comparison sits on its own line: translate() skips lines holding "==".
            "			readonly property bool scLoggedOut: !!provider && provider.loggedIn === false\n"
            '			buttonText: scLoggedOut ? "LOG IN" : "LOG OUT"\n'
            "			showSpinner: !scLoggedOut && !!(pLogoutSpinner && pLogoutSpinner.translator && pLogoutSpinner.translator.state)\n"
            "			onClicked: {\n"
            "				const service = provider\n"
            "				if(!service) return\n"
            "				const controller = Planck.librarianController.streamingInterfaceController\n"
            "				if(scLoggedOut) {\n"
            "					dialog.close()\n"
            "					controller.showAddAccountDialog(service)\n"
            "				} else {\n"
            "					pLogoutSpinner.translator.state = true\n"
            "					controller.displayLogout(service)\n"
            "				}\n"
            "			}\n",
        )],
        # H166: closing a login left the provider half-paired; only cancel the
        # pairing that belongs to the provider still selected.
        v + "Streaming/StreamingLoginDialog.qml": [(
            "	onCrossClicked: {\n		controller.restorePreviouslySelectedSource()\n	}\n",
            "	onCrossClicked: {\n"
            "		if(provider && !provider.loggedIn && controller.selectedProvider === provider) {\n"
            "			controller.cancelPairing()\n"
            "		}\n"
            "		controller.restorePreviouslySelectedSource()\n"
            "	}\n",
        )],
        v + "Streaming/StreamingPairingDialog.qml": [(
            "		if(!visible && service.provider && !service.provider.loggedIn) {\n",
            "		if(!visible && service.provider && !service.provider.loggedIn && controller.selectedProvider === service.provider) {\n",
        ), ("function onShowPairingDialog(provider) {\n", "function onShowPairingDialog(provider) {\n				if (root.scQuiet()) return\n")],
        # RA001: leaving the Load Playlist menu left the deck picker open.
        v + "SelectPlaylistDeckMenu/SelectPlaylistDeckMenuLayoutBase.qml": [(
            '	pShow: Planck.getProperty("/GUI/ShowSelectPlaylistDeckMenu")\n',
            '	pShow: Planck.getProperty("/GUI/ShowSelectPlaylistDeckMenu")\n'
            "	onShownChanged: {\n"
            "		const selection = PlanckGUI.views.playlistDeck ? PlanckGUI.views.playlistDeck.selectDeck : null\n"
            "		if(!shown && selection && selection.isOpen) selection.close()\n"
            "	}\n",
        )],
        # RA008: Hide Title/Artist also holds on the main view's title and artist.
        ROOT + "Items/Utils/TrackDisplayText.qml": [
            ("	property bool forceFilename: false\n",
             "	property bool forceFilename: false\n"
             "	property bool respectHideTitleArtist: false\n"
             '	readonly property QObjProperty pHideTitleArtist: Planck.getProperty("/Client/Preferences/HideTitleArtist")\n'
             "	readonly property bool hideTitleArtist: !!(respectHideTitleArtist && pHideTitleArtist && pHideTitleArtist.translator && pHideTitleArtist.translator.state)\n"),
            ("	readonly property string display: shouldShowFilename && canShowFilename ? filename : title\n",
             '	readonly property string display: hideTitleArtist ? "" : (shouldShowFilename && canShowFilename ? filename : title)\n'),
        ],
        v + "MainView/ArtistSong.qml": [
            ("				providerName: artistSong.trackSource\n",
             "				providerName: artistSong.trackSource\n				respectHideTitleArtist: true\n"),
            ("			visible: !artistSong.onlyFileName && text.length > 0\n",
             "			visible: !artistSong.onlyFileName && !displayText.hideTitleArtist && text.length > 0\n"),
            # #146: PRIME GO / Mixstream have no physical Previous/Next Track keys (every other
            # model does), so the title bar gets on-screen ‹ ›. Never while the deck plays.
            ("	Column {\n		id: layout\n",
             "	readonly property bool scNoSkipKeys: [\"JP11\", \"JP11S\", \"NH08\", \"NH08S\", \"NH10\"].indexOf(Config.device.name) >= 0\n"
             "	readonly property QObjProperty pScPlay: Planck.getProperty(`/Engine/Deck${deckIndex}/PlayState`)\n"
             "	readonly property bool scPlaying: !!(pScPlay && pScPlay.translator && pScPlay.translator.state)\n"
             "	Row {\n"
             "		id: scSkipKeys\n"
             "		visible: artistSong.scNoSkipKeys\n"
             "		anchors { right: parent.right; top: parent.top; bottom: parent.bottom }\n"
             "		opacity: artistSong.scPlaying ? 0.3 : 1\n"
             "		Repeater {\n"
             "			model: [{g: \"\\u2039\", n: \"PreviousTrack\"}, {g: \"\\u203a\", n: \"NextTrack\"}]\n"
             "			Item {\n"
             "				width: 48; height: scSkipKeys.height\n"
             "				Text { anchors.centerIn: parent; text: modelData.g; color: Style.font02Color; font.pixelSize: 40 }\n"
             "				MouseArea {\n"
             "					anchors.fill: parent\n"
             "					onClicked: {\n"
             "						if (artistSong.scPlaying) return\n"
             "						const t = Planck.getProperty(`/Engine/Deck${artistSong.deckIndex}/Track/${modelData.n}`)\n"
             "						if (t && t.translator) t.translator.state = true\n"
             "					}\n"
             "				}\n"
             "			}\n"
             "		}\n"
             "	}\n"
             "	Column {\n		id: layout\n"),
            ("			right: parent.right\n			margins: parent.margins\n		}\n\n		spacing: -15\n",
             "			right: scSkipKeys.visible ? scSkipKeys.left : parent.right\n			margins: parent.margins\n		}\n\n		spacing: -15\n"),
        ],
    }


# Only these properties hold display text. Ids such as StateMap names never match.
TEXT_LINE = re.compile(r'^(\s*(?:[\w.]*\.)?(?:text|label|title|message|headline|info|display|description|missingContentHint|buttonText|value)\s*:)(.*)$')
BARE_LITERAL = re.compile(r'^(\s*)"([^"\\]+)"(\s*)$')
# Template literals match whole, ${...} included (`Version: ${version}`).
LITERAL = re.compile(r'"([^"\\\n]+)"|`([^`\\\n]+)`')


def load_dict():
    rows = {}
    for line in (HERE / "i18n.tsv").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            en, hans, hant = line.split("\t")
            rows[en] = {"zh-Hans": hans, "zh-Hant": hant}
    return rows


def translated_qml(qrc: Path):
    for f in sorted(qrc.rglob("*.qml")):
        rel = f.relative_to(qrc).as_posix()
        if rel.startswith(TRANSLATED) and "/Demo/" not in rel:
            yield f, rel


def compared_strings(qrc: Path):
    """Literals used in comparisons drive logic, so they must stay in English."""
    found = set()
    for f, _ in translated_qml(qrc):
        found |= set(re.findall(r'[=!]==?\s*"([^"\n]+)"', f.read_text(encoding="utf-8", errors="replace")))
    return found


def live_literal(t):
    """A literal that follows scLang at run time. EmbeddedMain's ids are not visible from
    every file, but the engine's Math object is (Qt refuses new properties): EmbeddedMain hangs scI18n on it (see SERVICES_MAIN). Before
    that, or if it never happens, the baked text shows."""
    t = json.dumps(t, ensure_ascii=False)
    return f'(Math.scI18n ? Math.scI18n.t({t}) : {t})'


def translate(text, table, skip, live=False):
    # ListElement values must be constants, so lines inside such a block keep static text.
    in_element = 0
    static = False

    def literal(m):
        s, q = (m.group(1), '"') if m.group(1) is not None else (m.group(2), "`")
        if s not in table or s in skip:
            return m.group(0)
        if live and not static and "${" not in s:
            return live_literal(table[s])
        return f"{q}{table[s]}{q}"
    out = []
    for line in text.split("\n"):
        static = in_element > 0 or "ListElement" in line
        if static:
            in_element = max(0, in_element + line.count("{") - line.count("}"))
        m = TEXT_LINE.match(line)
        if m and "==" not in m.group(2) and "!=" not in m.group(2) and not m.group(2).lstrip().startswith("//"):
            line = m.group(1) + LITERAL.sub(literal, m.group(2))
        else:
            b = BARE_LITERAL.match(line)
            if b and b.group(2) in table and b.group(2) not in skip:
                line = f'{b.group(1)}"{table[b.group(2)]}"{b.group(3)}'
        out.append(line)
    return "\n".join(out)


def copy_icons(out: Path):
    shutil.copytree(HERE / "icons", out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--qrc", type=Path, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--dev", action="store_true", help="add the eval harness (never ship this)")
    a = ap.parse_args()
    global ROOT
    ROOT = next((r for r in ("Skins/EmbeddedCommon/", "Skins/Embedded/") if (a.qrc / r / "EmbeddedMain.qml").is_file()), None)
    if not ROOT:
        raise SystemExit(f"{a.qrc}: no Skins/EmbeddedCommon or Skins/Embedded EmbeddedMain.qml")
    shutil.rmtree(a.out, ignore_errors=True)
    base = {}
    for rel, change in edits(a.dev).items():
        if isinstance(change, str):
            base[rel] = change
            continue
        text = (a.qrc / rel).read_text(encoding="utf-8")
        for old, new in change:
            text = patch(text, old, new, rel)
        base[rel] = text
    # A qrc path this base does not ship renders as a blank icon (5.3.0 has no UIKit.1/).
    for rel, text in base.items():
        for ref in re.findall(r'"qrc:/([^"$%{}+]+\.(?:png|svg|jpg|qml))"', text.replace("%20", " ")):
            if not (a.qrc / ref).is_file():
                raise SystemExit(f"{rel}: qrc:/{ref} is not in {a.qrc}")
    rows = load_dict()
    skip = compared_strings(a.qrc)
    # dicts[target][any spelling] = target spelling, so the language can change without a restart.
    dicts = {}
    for lang in ("en", "zh-Hans", "zh-Hant"):
        d = {}
        for en, t in rows.items():
            to = en if lang == "en" else t[lang]
            for spelling in (en, t["zh-Hans"], t["zh-Hant"]):
                d[spelling] = to
        dicts[lang] = d
    for lang in ("en", "zh-Hans", "zh-Hant"):
        table = {en: (en if lang == "en" else t[lang]) for en, t in rows.items()}
        files = dict(base)
        for f, rel in translated_qml(a.qrc):
            source = files.get(rel) or f.read_text(encoding="utf-8", errors="strict")
            changed = translate(source, table, skip, live=rel.startswith(("Skins/EmbeddedCommon/", "Skins/Embedded/")))
            if changed != source:
                files[rel] = changed
        for rel, text in files.items():
            text = text.replace("@DICTS@", json.dumps(dicts, ensure_ascii=False))
            target = a.out / lang / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        # Engine's start-up screen. Only the 1280x800 one: _16x9 is 1366x768 and would stretch.
        splash = HERE.parent.parent / "docs/assets/boot-splash-enginex.png"
        if ROOT == "Skins/Embedded/" and splash.is_file():
            (a.out / lang / ROOT / "img").mkdir(parents=True, exist_ok=True)
            shutil.copyfile(splash, a.out / lang / ROOT / "img/SplashScreen.png")
        print(lang, len(files), "files")
    copy_icons(a.out / "icons")
    with tarfile.open(a.out.with_suffix(".tar.gz"), "w:gz") as t:
        for lang in ("en", "zh-Hans", "zh-Hant"):
            t.add(a.out / lang, arcname=f"packs/{lang}")
        t.add(a.out / "icons", arcname="icons")


if __name__ == "__main__":
    main()
