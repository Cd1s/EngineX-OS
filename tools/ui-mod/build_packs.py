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
	property string scUpdateRepo: "Cd1s/EngineX-OS"
	property string scUpdateText: ""
	property bool scFourDeckWave: false
	property bool scProlinkWave: true
	property bool scBroadcastOn: false
	property var scProlinkDecks: ({})
	property double scProlinkSyncMs: 0
	property double scProlinkNowMs: 0
	property string scLastBroadcastTrack: ""
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
		interval: root.scJog === "fast" ? 60 : root.scJog === "bpm" && root.scJogBPM > 0 ? Math.round(60000 / root.scJogBPM) : 100
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
			const step = root.scJog === "fast" ? 0.05 : root.scJog === "bpm" && root.scJogBPM > 0 ? 0.25 : 0.01
			root.scJogHue = (root.scJogHue + step) % 1
			root.scDeckList().forEach((d, i) => {
				const c = root.scDeckColor(d)
				if (c) c.color = Qt.hsva((root.scJogHue + i * 0.35) % 1, 1, 1, 1)
			})
		}
	}
	property var scSources: ["tidal", "netease", "spotify", "deezer", "soundcloud", "beatport"]
	property var scLoggedIn: []
	property string scScope: ""
	readonly property var scServices: [
		{ id: "tidal", name: "TIDAL" },
		{ id: "netease", name: scT("NetEase Cloud Music") },
		{ id: "spotify", name: "Spotify" },
		{ id: "deezer", name: "Deezer" },
		{ id: "soundcloud", name: "SoundCloud" },
		{ id: "beatport", name: "Beatport" }
	]
	readonly property var scDict: (@DICT@)
	function scT(s) { return scDict[s] || s }
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
		scJog = v.jog || "static"
		scKeepPlayhead(v.playhead || 0)
		scSources = v.sources
		scLoggedIn = v.loggedIn
		scScope = v.scope
		scFourDeckWave = !!v.fourDeckWaveform
		scProlinkWave = v.prolinkWaveform !== undefined ? !!v.prolinkWaveform : true
		scBroadcastOn = !!v.broadcastEnabled
		const p = Planck.getProperty("/Client/Preferences/Profile/Application/WaveformStyle")
		if (p.translator && (p.translator.index == 1) !== (v.waveform === "rgb"))
			p.translator.index = v.waveform === "rgb" ? 1 : 0
	}
	function scProlinkDeck(deck) {
		return scProlinkDecks && scProlinkDecks[String(deck)] ? scProlinkDecks[String(deck)] : null
	}
	function scHasProlinkDeck(deck) {
		const d = scProlinkDeck(deck)
		return !!(scProlinkWave && d && (d.track_id || d.bpm > 0 || (d.waveform && d.waveform.length > 0)))
	}
	function scProlinkPosMs(deck) {
		const d = scProlinkDeck(deck)
		if (!d) return 0
		const base = d.play_position_ms || 0
		if (!d.playing || !(scProlinkSyncMs > 0)) return base
		return base + Math.max(0, Date.now() - scProlinkSyncMs)
	}
	function scFetchProlink() {
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/prolink")
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			const v = JSON.parse(x.responseText)
			scProlinkDecks = v.decks || {}
			scProlinkSyncMs = Date.now()
			scProlinkNowMs = scProlinkSyncMs
		}
		x.send()
	}
	Timer {
		interval: 400; repeat: true
		running: root.scProlinkWave || root.scFourDeckWave
		onTriggered: root.scFetchProlink()
	}
	Timer {
		interval: 50; repeat: true
		running: (root.scProlinkWave || root.scFourDeckWave) && root.scProlinkSyncMs > 0
		onTriggered: root.scProlinkNowMs = Date.now()
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
	// A stemmed track opens its deck's pads in STEMS mode. Pads/View only steps
	// through modes; stop at STEMS, or back where it started if STEMS is not offered.
	// Engine renders Stems only for library tracks and silently skips streamed rows.
	// A streamed row dropped on the Stems list is imported by Engine itself from the
	// file stream-sync downloaded, onto a deck that is not playing, then queued.
	function scStemStream(uid) {
		const lib = Planck.librarianController.collectionController
		const m = /uri=streaming:\\/\\/TIDAL\\/Track\\/(\\d+)/.exec(lib.getTrackUrisFromUniversalIds([uid]) || "")
		if (!m) return false
		const deck = root.scDeckList().slice().reverse().find(d => !Planck.getProperty("/Engine/Deck%1/PlayState".arg(d)).translator.state)
		if (!deck) return true // all decks playing: never interrupt playback
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/stems/source?id=" + m[1])
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			const url = "file://" + encodeURI(JSON.parse(x.responseText).path)
			Planck.getProperty("/Client/Librarian/AddToDatabaseAndLoadToDeck%1".arg(deck)).translator.url = url
			scStemImport.url = url
			scStemImport.tries = 20
			scStemImport.restart()
		}
		x.send()
		return true
	}
	// Imports land in the deck's internal library, whose universal ids are plain
	// track ids. shortcut: scans ids 1-500; widen if that library ever grows past it.
	Timer {
		id: scStemImport
		property string url: ""
		property int tries: 0
		interval: 500; repeat: true
		onTriggered: {
			const ids = Array.from({ length: 500 }, (_, i) => i + 1)
			const lines = (Planck.librarianController.collectionController.getTrackUrisFromUniversalIds(ids) || "").split("\\n")
			const hit = lines.find(l => l.startsWith(url + "?"))
			const id = hit && /[?&]trackId=(\\d+)/.exec(hit)
			if (id) Planck.librarianController.stemsQueueManager.startRenderTracks(id[1])
			if (id || --tries <= 0) stop()
		}
	}
	function scPadsToStems(deck) {
		const p = Planck.getProperty("/Engine/Deck%1/Pads/View".arg(deck))
		const t = p && p.translator
		if (!t) return
		const start = t.string
		for (let i = 0; i < 16 && t.string !== "STEMS"; i++) {
			t.incValue(0, 1, 0, true)
			if (t.string === start) return
		}
	}
	Instantiator {
		model: root.decks && root.decks.length ? root.decks : [1, 2]
		delegate: QtObject {
			readonly property int deckIdx: parseInt(modelData)
			readonly property QObjProperty pStemmed: Planck.getProperty("/Engine/Deck%1/Track/Stemmed".arg(deckIdx))
			readonly property bool stemmed: !!pStemmed && !!pStemmed.translator && pStemmed.translator.state
			onStemmedChanged: if (stemmed) root.scPadsToStems(deckIdx)
			Component.onCompleted: if (stemmed) root.scPadsToStems(deckIdx)
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
						if (root.scLoginStatus === "error") return root.scT("Can't start login. Check the network.")
						if (root.scLoginStatus === "expired") return root.scT("The QR code expired.")
						if (root.scLoginStatus === "scanned") return root.scT("Confirm on your phone")
						if (root.scLogin === "netease") return root.scT("Scan with the NetEase Cloud Music app")
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

FOUR_DECK_STRIP = """
		Rectangle {
			id: scFourDeckRemoteStrip
			objectName: "FourDeckRemoteStrip"
			z: 900
			height: 76
			anchors {
				top: parent.top
				topMargin: 80
				left: parent.left
				leftMargin: 20
				right: parent.right
				rightMargin: 20
			}
			visible: root.scFourDeckWave && !root.dialogOpen && !root.messageBoxOpen
			color: "#e610141d"
			radius: 12
			border {
				width: 1
				color: "#2e384d"
			}

			function scDeckSong(d) {
				const pl = root.scProlinkDeck(d)
				if (pl && pl.title) return pl.title
				const p = Planck.getProperty("/Engine/Deck" + d + "/Track/SongName")
				if (p && p.translator && p.translator.string) return root.scPlainTitle(p.translator.string)
				return "DECK " + d
			}
			function scDeckArtist(d) {
				const pl = root.scProlinkDeck(d)
				if (pl && pl.artist) return pl.artist
				const p = Planck.getProperty("/Engine/Deck" + d + "/Track/ArtistName")
				if (p && p.translator && p.translator.string) return p.translator.string
				return ""
			}
			function scDeckBpm(d) {
				const pl = root.scProlinkDeck(d)
				if (pl && (pl.effective_bpm || pl.bpm)) return (pl.effective_bpm || pl.bpm).toFixed(1)
				let p = Planck.getProperty("/Engine/Deck" + d + "/Track/CurrentBPM")
				if (!p || !p.translator) p = Planck.getProperty("/Engine/Deck" + d + "/CurrentBPM")
				const val = p && p.translator ? (p.translator.unnormalized || p.translator.value || 0) : 0
				return val > 0 ? (Math.round(val * 10) / 10).toFixed(1) : "--.-"
			}
			function scDeckBeat(d) {
				const pl = root.scProlinkDeck(d)
				if (pl && pl.beat_in_bar >= 1 && pl.beat_in_bar <= 4) return pl.beat_in_bar
				const p = Planck.getProperty("/Engine/Deck" + d + "/CurrentBeatInBar")
				if (p && p.translator && p.translator.value) return p.translator.value
				return 1
			}
			function scDeckPlaying(d) {
				const pl = root.scProlinkDeck(d)
				if (pl && pl.playing) return true
				const p = Planck.getProperty("/Engine/Deck" + d + "/PlayState")
				return !!(p && p.translator && p.translator.state)
			}

			Row {
				anchors.fill: parent
				anchors.margins: 6
				spacing: 12

				Repeater {
					model: [3, 4]
					Rectangle {
						readonly property int deckNum: modelData
						readonly property bool isD3: deckNum === 3
						width: (parent.width - 12) / 2
						height: parent.height
						color: "#18202c"
						radius: 8
						border {
							width: 1
							color: isD3 ? "#1e3a5f" : "#3b234a"
						}

						// Top line: Badge, Title & Artist, BPM
						Item {
							id: headerRow
							anchors {
								top: parent.top
								topMargin: 4
								left: parent.left
								leftMargin: 8
								right: parent.right
								rightMargin: 8
							}
							height: 22

							Rectangle {
								id: dBadge
								width: 54
								height: 18
								radius: 4
								color: isD3 ? "#00c853" : "#ab47bc"
								anchors.verticalCenter: parent.verticalCenter
								Text {
									anchors.centerIn: parent
									text: "DECK " + deckNum
									font.bold: true
									font.pixelSize: 11
									color: isD3 ? "#000000" : "#ffffff"
								}
							}

							Text {
								id: dTitle
								anchors {
									left: dBadge.right
									leftMargin: 8
									right: dBpm.left
									rightMargin: 8
									verticalCenter: parent.verticalCenter
								}
								text: scFourDeckRemoteStrip.scDeckSong(deckNum) + (scFourDeckRemoteStrip.scDeckArtist(deckNum) ? " - " + scFourDeckRemoteStrip.scDeckArtist(deckNum) : "")
								color: "#ffffff"
								font.pixelSize: 13
								font.bold: true
								elide: Text.ElideRight
							}

							Text {
								id: dBpm
								anchors {
									right: parent.right
									verticalCenter: parent.verticalCenter
								}
								text: scFourDeckRemoteStrip.scDeckBpm(deckNum) + " BPM"
								color: "#00c3e3"
								font.pixelSize: 13
								font.bold: true
							}
						}

						// Bottom line: 4-Beat Phase indicators + Mini Waveform Strip
						Item {
							anchors {
								top: headerRow.bottom
								topMargin: 4
								left: parent.left
								leftMargin: 8
								right: parent.right
								rightMargin: 8
								bottom: parent.bottom
								bottomMargin: 4
							}

							// 4-beat phase bars
							Row {
								id: phaseBars
								anchors {
									left: parent.left
									verticalCenter: parent.verticalCenter
								}
								height: 26
								spacing: 3

								Repeater {
									model: 4
									Rectangle {
										readonly property bool activeBeat: (index + 1) === scFourDeckRemoteStrip.scDeckBeat(deckNum)
										width: 8
										height: 26
										radius: 2
										color: activeBeat ? (scFourDeckRemoteStrip.scDeckPlaying(deckNum) ? (isD3 ? "#00c853" : "#ab47bc") : "#ffffff") : "#2a3649"
									}
								}
							}

							// Mini waveform strip (32 bars)
							Row {
								anchors {
									left: phaseBars.right
									leftMargin: 8
									right: parent.right
									verticalCenter: parent.verticalCenter
								}
								height: 26
								spacing: 1

								readonly property real stepW: (width - 31) / 32.0
								readonly property var pDev: root.scProlinkDeck(deckNum)
								readonly property var pWf: pDev && pDev.waveform ? pDev.waveform : []
								readonly property bool isPlaying: scFourDeckRemoteStrip.scDeckPlaying(deckNum)
								readonly property real pPosMs: root.scProlinkPosMs(deckNum)
								readonly property real pDurMs: (pDev && pDev.duration_ms) ? pDev.duration_ms : 180000
								readonly property int pCenter: pWf && pWf.length > 0 ? Math.floor((pPosMs / Math.max(1, pDurMs)) * pWf.length) : 0

								Repeater {
									model: 32
									Item {
										width: Math.max(2, parent.stepW)
										height: 26

										readonly property int sIdx: parent.pWf && parent.pWf.length > 0 ? Math.max(0, Math.min(parent.pWf.length - 1, parent.pCenter + (index - 16) * 2)) : 0
										readonly property var sample: (parent.pWf && parent.pWf.length > 0) ? parent.pWf[sIdx] : null
										readonly property real animPhase: (index + Math.floor(root.scProlinkNowMs / 80)) % 16
										readonly property real hFactor: sample ? Math.max(0.15, (sample[0] + sample[1] + sample[2]) / (3.0 * 255.0)) : (parent.isPlaying ? (0.2 + 0.6 * Math.abs(Math.sin((index + animPhase) * 0.4))) : 0.2)

										Column {
											anchors.centerIn: parent
											width: parent.width
											spacing: 1

											Rectangle {
												width: parent.width
												height: Math.max(1, Math.round(8 * hFactor))
												color: root.scWave === "3band" ? "#ffffff" : root.scWave === "rgb" ? "#0078ff" : "#69f0ae"
											}
											Rectangle {
												width: parent.width
												height: Math.max(1, Math.round(9 * hFactor))
												color: root.scWave === "3band" ? "#f0b400" : root.scWave === "rgb" ? "#00e632" : "#00e676"
											}
											Rectangle {
												width: parent.width
												height: Math.max(1, Math.round(9 * hFactor))
												color: root.scWave === "3band" ? "#4a78ff" : root.scWave === "rgb" ? "#ff1428" : "#00c853"
											}
										}
									}
								}
							}
						}
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

EXTRA_DEVICE_ENTRIES = """
		EntryBase.ListEntry {
			text: "4-Deck Waveform"
			readonly property var ids: ["0", "1"]
			values: ["Off", "On"]
			currentValue: values[root.scFourDeckWave ? 1 : 0]
			onSelected: function(index) { root.scCall("fourDeckWaveform=" + ids[index]) }
		},
		EntryBase.ListEntry {
			text: "Pro DJ Link Waveform"
			readonly property var ids: ["0", "1"]
			values: ["Off", "On"]
			currentValue: values[root.scProlinkWave ? 1 : 0]
			onSelected: function(index) { root.scCall("prolinkWaveform=" + ids[index]) }
		},
		EntryBase.ListEntry {
			text: "Live Stream"
			readonly property var ids: ["0", "1"]
			values: ["Off", "On"]
			currentValue: values[root.scBroadcastOn ? 1 : 0]
			onSelected: function(index) { root.scCall("broadcast=" + ids[index]) }
		},
"""

LANGUAGE_ENTRY = """
		EntryBase.ListEntry {
			text: "Language"
			readonly property var ids: ["en", "zh-Hans", "zh-Hant"]
			values: ["English", "简体中文", "繁體中文"]
			currentValue: values[Math.max(0, ids.indexOf(root.scLang))]
			onSelected: function(index) {
				if (ids[index] === root.scLang) return
				const lang = ids[index]
				const prompt = PlanckGUI.views.prompts.post({
					type: Prompt.Warning,
					message: "切换语言需要重启 Engine，播放会中断。\nRestart Engine to change language? Playback will stop.",
					options: Prompt.Cancel | Prompt.Ok
				})
				prompt.finished.connect(function(role) {
					if (role === Prompt.AcceptRole) root.scCall("lang=" + lang + "&restart=1")
				})
			}
		},
		WaveformStyle { },
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

FOUR_DECK_TILE = """
		Rectangle {
			id: scFourDeckTile
			objectName: "ControlCenter_FourDeckWaveform"
			width: 256
			height: 104
			radius: 16
			color: Style.surface03Color
			Text {
				x: 14
				y: 8
				text: "4-Deck Waveform"
				font: Style.fontH4
				color: Style.font01Color
			}
			Row {
				x: 12
				y: 36
				Repeater {
					model: ["OFF", "ON"]
					Rectangle {
						readonly property bool selected: (index === 1) === root.scFourDeckWave
						width: (scFourDeckTile.width - 24) / 2
						height: 56
						color: selected ? "#00c3e3" : fourDeckTap.pressed ? "#6a6a6a" : "#4a4a4a"
						border {
							width: 1
							color: "#a8a8a8"
						}
						Text {
							anchors.centerIn: parent
							text: root.scT(index === 0 ? "Off" : "On").toUpperCase()
							font: Style.fontH5
							color: "#ffffff"
						}
						MouseArea {
							id: fourDeckTap
							anchors.fill: parent
							onClicked: root.scCall("fourDeckWaveform=" + index)
						}
					}
				}
			}
		}
"""

BROADCAST_TILE = """
		Rectangle {
			id: scBroadcastTile
			objectName: "ControlCenter_LiveStream"
			width: 256
			height: 104
			radius: 16
			color: Style.surface03Color
			Text {
				x: 14
				y: 8
				text: "Live Stream"
				font: Style.fontH4
				color: Style.font01Color
			}
			Row {
				x: 12
				y: 36
				Repeater {
					model: ["OFF", "ON"]
					Rectangle {
						readonly property bool selected: (index === 1) === root.scBroadcastOn
						width: (scBroadcastTile.width - 24) / 2
						height: 56
						color: selected ? "#00c3e3" : broadcastTap.pressed ? "#6a6a6a" : "#4a4a4a"
						border {
							width: 1
							color: "#a8a8a8"
						}
						Text {
							anchors.centerIn: parent
							text: root.scT(index === 0 ? "Off" : "On").toUpperCase()
							font: Style.fontH5
							color: "#ffffff"
						}
						MouseArea {
							id: broadcastTap
							anchors.fill: parent
							onClicked: root.scCall("broadcast=" + index)
						}
					}
				}
			}
		}
"""

WAVEFORM_LINE_IN_HINT = """import QtQuick 2.9
import airQuick 1.0
import StateMap 1.0
import EngineOS.UI.Style 1.0

Rectangle {
	id: lineInHintRoot
	anchors.fill: parent
	anchors.leftMargin: Config.device.isPlayer ? 36 : 24
	color: Style.surface01Color

	property int deckIndex
	readonly property QObjProperty pLine: Planck.getProperty(`/Engine/Mixer/Channel${deckIndex}/Line`)
	visible: deckIndex >= 1 && ((pLine && pLine.translator && pLine.translator.state) || root.scHasProlinkDeck(deckIndex))
	opacity: visible ? 1 : 0.0

	AIRMouseEventBlocker { }

	readonly property var d: root.scProlinkDeck(deckIndex)
	readonly property bool hasProlink: root.scHasProlinkDeck(deckIndex)

	Item {
		id: prolinkContent
		anchors.fill: parent
		visible: lineInHintRoot.hasProlink

		// Top metadata bar
		Rectangle {
			id: metaBar
			anchors {
				top: parent.top
				left: parent.left
				right: parent.right
			}
			height: 32
			color: "#cc0c1017"

			Row {
				anchors {
					left: parent.left
					leftMargin: 8
					verticalCenter: parent.verticalCenter
				}
				spacing: 8

				Rectangle {
					width: playerTag.implicitWidth + 12
					height: 22
					radius: 4
					color: (lineInHintRoot.d && lineInHintRoot.d.on_air) ? "#ff3b30" : "#00c3e3"
					anchors.verticalCenter: parent.verticalCenter
					Text {
						id: playerTag
						anchors.centerIn: parent
						text: (lineInHintRoot.d && lineInHintRoot.d.name) ? lineInHintRoot.d.name : "CDJ-3000"
						font.bold: true
						font.pixelSize: 12
						color: "#000000"
					}
				}

				Text {
					anchors.verticalCenter: parent.verticalCenter
					text: (lineInHintRoot.d && lineInHintRoot.d.title) ? lineInHintRoot.d.title : "Line-In Pro DJ Link"
					color: "#ffffff"
					font.pixelSize: 15
					font.bold: true
					elide: Text.ElideRight
					width: Math.min(implicitWidth, lineInHintRoot.width - 240)
				}

				Text {
					anchors.verticalCenter: parent.verticalCenter
					text: (lineInHintRoot.d && lineInHintRoot.d.artist) ? lineInHintRoot.d.artist : ""
					color: "#9caec4"
					font.pixelSize: 14
					elide: Text.ElideRight
					width: Math.min(implicitWidth, 180)
				}
			}

			Text {
				anchors {
					right: parent.right
					rightMargin: 12
					verticalCenter: parent.verticalCenter
				}
				text: ((lineInHintRoot.d && (lineInHintRoot.d.effective_bpm || lineInHintRoot.d.bpm)) ? (lineInHintRoot.d.effective_bpm || lineInHintRoot.d.bpm).toFixed(1) : "--.-") + " BPM"
				color: "#00c3e3"
				font.pixelSize: 15
				font.bold: true
			}
		}

		// Waveform area
		Item {
			id: waveArea
			anchors {
				top: metaBar.bottom
				left: parent.left
				right: parent.right
				bottom: parent.bottom
			}
			clip: true

			readonly property real posMs: root.scProlinkPosMs(lineInHintRoot.deckIndex)
			readonly property real durMs: (lineInHintRoot.d && lineInHintRoot.d.duration_ms) ? lineInHintRoot.d.duration_ms : 180000
			readonly property var wf: (lineInHintRoot.d && lineInHintRoot.d.waveform) ? lineInHintRoot.d.waveform : []
			readonly property int wfLen: wf ? wf.length : 0
			readonly property real centerIdx: wfLen > 0 ? (posMs / Math.max(1, durMs)) * wfLen : 0
			readonly property real colW: width / 64.0
			readonly property bool isPlaying: !!(lineInHintRoot.d && lineInHintRoot.d.playing)

			Row {
				anchors.fill: parent
				Repeater {
					model: 64
					Item {
						id: colItem
						width: waveArea.colW
						height: waveArea.height

						readonly property int sIdx: Math.round(waveArea.centerIdx + (index - 32) * (waveArea.wfLen > 0 ? Math.max(1, waveArea.wfLen / 128) : 1))
						readonly property var sample: (waveArea.wf && waveArea.wfLen > 0 && sIdx >= 0 && sIdx < waveArea.wfLen) ? waveArea.wf[sIdx] : null
						readonly property real animPhase: (index + Math.floor(root.scProlinkNowMs / 80)) % 16
						readonly property real hFactor: sample ? 1.0 : (waveArea.isPlaying ? (0.2 + 0.6 * Math.abs(Math.sin((index + animPhase) * 0.4))) : 0.15)
						readonly property real lowVal: sample ? sample[0] : Math.round(180 * hFactor)
						readonly property real midVal: sample ? sample[1] : Math.round(140 * hFactor)
						readonly property real highVal: sample ? sample[2] : Math.round(100 * hFactor)

						// Beatgrid tick mark at top
						Rectangle {
							anchors.top: parent.top
							anchors.horizontalCenter: parent.horizontalCenter
							width: 1
							height: ((index - 32) % 16 === 0) ? 10 : (((index - 32) % 4 === 0) ? 6 : 0)
							color: ((index - 32) % 16 === 0) ? "#ff3b30" : "#ffffff"
							visible: ((index - 32) % 4 === 0)
						}

						// Waveform bar centered vertically
						Column {
							anchors.centerIn: parent
							spacing: 1
							width: Math.max(2, colItem.width - 1)

							// High segment
							Rectangle {
								anchors.horizontalCenter: parent.horizontalCenter
								width: parent.width
								height: Math.max(1, Math.round((colItem.highVal / 255.0) * (colItem.height * 0.2)))
								color: root.scWave === "3band" ? "#ffffff" : root.scWave === "rgb" ? "#0078ff" : "#69f0ae"
							}
							// Mid segment
							Rectangle {
								anchors.horizontalCenter: parent.horizontalCenter
								width: parent.width
								height: Math.max(2, Math.round((colItem.midVal / 255.0) * (colItem.height * 0.35)))
								color: root.scWave === "3band" ? "#f0b400" : root.scWave === "rgb" ? "#00e632" : "#00e676"
							}
							// Low segment
							Rectangle {
								anchors.horizontalCenter: parent.horizontalCenter
								width: parent.width
								height: Math.max(2, Math.round((colItem.lowVal / 255.0) * (colItem.height * 0.4)))
								color: root.scWave === "3band" ? "#4a78ff" : root.scWave === "rgb" ? "#ff1428" : "#00c853"
							}
						}

						// Beatgrid tick mark at bottom
						Rectangle {
							anchors.bottom: parent.bottom
							anchors.horizontalCenter: parent.horizontalCenter
							width: 1
							height: ((index - 32) % 16 === 0) ? 10 : (((index - 32) % 4 === 0) ? 6 : 0)
							color: ((index - 32) % 16 === 0) ? "#ff3b30" : "#ffffff"
							visible: ((index - 32) % 4 === 0)
						}
					}
				}
			}

			// Crisp white center playhead bar
			Rectangle {
				id: playhead
				anchors {
					top: parent.top
					bottom: parent.bottom
					horizontalCenter: parent.horizontalCenter
				}
				width: 2
				color: "#ffffff"
				z: 10
			}
		}
	}
}
"""

# Smart Sync (5.3.0 only): a synced deck with a track widens its Pitch Range to
# the smallest step that reaches the master tempo. It never narrows the range.
# An empty deck still reports OriginalBPM 120, hence SongLoaded.
SMART_SYNC_MAIN = """
	Instantiator {
		model: root.decks && root.decks.length ? root.decks : [1, 2]
		delegate: QtObject {
			readonly property int deckIdx: parseInt(modelData)
			readonly property QObjProperty pSync: Planck.getProperty("/Engine/Deck%1/SyncMode".arg(deckIdx))
			readonly property QObjProperty pLoaded: Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(deckIdx))
			readonly property QObjProperty pOrigBpm: Planck.getProperty("/Engine/Deck%1/OriginalBPM".arg(deckIdx))
			readonly property QObjProperty pMasterBpm: Planck.getProperty("/Engine/Master/MasterTempo")
			readonly property real syncNeed: {
				const s = pSync && pSync.translator, l = pLoaded && pLoaded.translator, o = pOrigBpm && pOrigBpm.translator, m = pMasterBpm && pMasterBpm.translator
				if (!s || !l || !o || !m || s.string === "Off" || !l.state || !(o.unnormalized > 0)) return 0
				return Math.abs(m.unnormalized - o.unnormalized) / o.unnormalized * 100
			}
			onSyncNeedChanged: root.scWidenRange(deckIdx, syncNeed)
		}
	}
	function scWidenRange(deck, need) {
		const t = Planck.getProperty("/Engine/Deck%1/SpeedRange".arg(deck)).translator
		if (!t || need <= parseFloat(t.string)) return
		const i = t.entries.findIndex(e => parseFloat(e) >= need)
		t.index = i < 0 ? t.entries.length - 1 : i
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
							onClicked: scSyncTile.translator.index = index
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
        ROOT + "EmbeddedMain.qml": [
            (f"	Connections {{\n		target: {window()}\n", services + main + f"\n	Connections {{\n		target: {window()}\n"),
            ("		id: uiContainer\n", "		id: uiContainer\n" + LOGIN_DIALOG + FOUR_DECK_STRIP),
        ],
        v + "Common/Waveform/Waveform.qml": wave,
        v + "Common/Waveform/" + ("WaveformLineInHint.qml" if v53() else "LineInHint.qml"): WAVEFORM_LINE_IN_HINT,
        # Engine offers streamed rows only Prepare; add Stems (see scStemStream).
        v + "BrowseView/Components/Components/SwipeLeftItem.qml": [
            ("	readonly property list<string> actionLabels: leftActions.map( action => action.display.label )\n",
             "	readonly property bool scStem: String(row.modelData && row.modelData.uri || \"\").startsWith(\"streaming://TIDAL/Track/\")\n"
             "	readonly property list<string> actionLabels: leftActions.map( action => action.display.label ).concat(scStem ? [root.scT(\"Stems\")] : [])\n"),
            ("leftActions.map( action => action.display.commitMessage )", "leftActions.map( action => action.display.commitMessage ).concat(scStem ? [root.scT(\"Stems\")] : [])"),
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
            "	text: `EngineX OS ${root.scVersion || versionString} <font color=\\\"${grey}\\\">(github.com/${root.scUpdateRepo})</font>`")],
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
        v + "BrowseView/Components/Craylist.qml": [(
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
            ("		PlayerNumber { },\n", LANGUAGE_ENTRY + QUALITY_ENTRY + JOG_ENTRY + EXTRA_DEVICE_ENTRIES + "		PlayerNumber { },\n"),
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
            ("		DayModeEnable { }\n", "		DayModeEnable { }\n" + WAVE_TILE.replace("@LOW@", BAND_COLORS["Low"]).replace("@MID@", BAND_COLORS["Mid"]).replace("@HIGH@", BAND_COLORS["High"]) + FOUR_DECK_TILE + BROADCAST_TILE + (SYNC_TILE if v53() else "")),
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
        v + "Streaming/StreamingDialog.qml": [
            # This limit is for TIDAL's own qualities; DJ sets here run for hours.
            ("		function onShowTrackTooLong() {\n", "		function onShowTrackTooLong() {\n			return\n"),
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
        v + "BrowseView/Components/TrackList.qml": [
            (
                "	readonly property string selectedProviderDisplayName: Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName\n",
                "	readonly property string selectedProviderDisplayName: root.scBrand(Planck.librarianController.streamingInterfaceController.selectedProviderDisplayName)\n",
            ),
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
    }
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
            ("			onIsCurrentFocusAreaChanged: {\n				if(visible && !activeFocus && isCurrentFocusArea) {\n					forceActiveFocus()\n				}\n",
             "			function scRestoreFocusNow() {\n				" + restore_guard + "				forceActiveFocus()\n			}\n\n"
             "			onIsCurrentFocusAreaChanged: {\n				if(visible && !activeFocus && isCurrentFocusArea) {\n					Qt.callLater(scRestoreFocusNow)\n				}\n"),
            ("					if(visible && isCurrentFocusArea && !activeFocus) {\n						forceActiveFocus()\n					}\n",
             "					if(visible && isCurrentFocusArea && !activeFocus) {\n						Qt.callLater(scRestoreFocusNow)\n					}\n"),
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


def translate(text, table, skip):
    def literal(m):
        s, q = (m.group(1), '"') if m.group(1) is not None else (m.group(2), "`")
        return f"{q}{table[s]}{q}" if s in table and s not in skip else m.group(0)
    out = []
    for line in text.split("\n"):
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
    for lang in ("en", "zh-Hans", "zh-Hant"):
        table = {en: t[lang] for en, t in rows.items()} if lang != "en" else {}
        files = dict(base)
        if table:
            for f, rel in translated_qml(a.qrc):
                source = files.get(rel) or f.read_text(encoding="utf-8", errors="strict")
                changed = translate(source, table, skip)
                if changed != source:
                    files[rel] = changed
        for rel, text in files.items():
            text = text.replace("@DICT@", json.dumps(table, ensure_ascii=False))
            target = a.out / lang / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        print(lang, len(files), "files")
    copy_icons(a.out / "icons")
    with tarfile.open(a.out.with_suffix(".tar.gz"), "w:gz") as t:
        for lang in ("en", "zh-Hans", "zh-Hant"):
            t.add(a.out / lang, arcname=f"packs/{lang}")
        t.add(a.out / "icons", arcname="icons")


if __name__ == "__main__":
    main()
