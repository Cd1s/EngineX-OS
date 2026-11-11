#!/usr/bin/env python3
"""Build the UI packs engine_ui_hook.c loads into Engine (see README.md)."""
import argparse
import json
import os
import re
import shutil
import subprocess
import tarfile
from pathlib import Path

BOOT_READY = r'''
                    id: scBootSplash
                    Timer {
                        id: scBootReadyTimer
                        property bool sent: false
                        property var request: null
                        property double requestAt: 0
                        interval: 1000; repeat: true
                        running: !scBootSplash.active && planckQuickView.isMainScreen && !sent
                        onTriggered: {
                            if (request) {
                                if (Date.now() - requestAt > 5000) {
                                    const old = request; request = null; old.abort()
                                }
                                return
                            }
                            const x = new XMLHttpRequest()
                            request = x; requestAt = Date.now()
                            x.onreadystatechange = function() {
                                if (x.readyState !== XMLHttpRequest.DONE || request !== x) return
                                request = null
                                if (x.status !== 200 || scBootSplash.active) return
                                let token = ""
                                try { token = JSON.parse(x.responseText).token || "" } catch (e) { return }
                                if (!token) return
                                const ack = new XMLHttpRequest()
                                request = ack; requestAt = Date.now()
                                ack.onreadystatechange = function() {
                                    if (ack.readyState !== XMLHttpRequest.DONE || request !== ack) return
                                    request = null
                                    if (ack.status === 200 || ack.status === 204) sent = true
                                }
                                ack.open("POST", "http://127.0.0.1:8080/v1/sc5000/boot/ready", true)
                                ack.setRequestHeader("Content-Type", "application/json")
                                ack.send(JSON.stringify({token: token}))
                            }
                            x.open("GET", "http://127.0.0.1:8080/v1/sc5000/boot", true)
                            x.send()
                        }
                    }
'''

def verify_boot_ready_loader(text):
    """Fail a new vendor base before emitting a duplicate QML id."""
    blocks = [m.group(1) for m in re.finditer(
        r'\bSplashScreen\s*\{(.*?)\n[ \t]*\}', text, re.S)
        if 'objectName: "SplashScreenLoader"' in m.group(1)]
    if len(blocks) != 1 or re.search(r'(?m)^\s*id\s*:', blocks[0]):
        raise ValueError("boot-ready: stock splash loader is missing or already declares an id")


FX_CONTROLLER = r'''
    property var scFxControllers: []
    property bool scFxMsSending: false
    property int scFxMsRequest: 0
    Timer { id: scFxMsTimeout; interval: 2000; onTriggered: root.scFxMsExpire() }
    function scFxMsExpire() {
        ++scFxMsRequest
        scFxMsSending = false; scFxMsWanted = -1; scFxMsSent = -1
    }
    property int scFxMsWanted: -1
    property int scFxMsSent: -1
    function scFxMsFlush() {
        if (scFxMsSending) return
        if (scFxTimeMode !== "ms") { scFxMsWanted = -1; return }
        if (scFxMsWanted < 10) return
        const ms = scFxMsWanted
        scFxMsWanted = -1; scFxMsSent = ms; scFxMsSending = true
        const request = ++scFxMsRequest
        scFxMsTimeout.restart()
        scCall("fxTimeMs=" + ms, function() {
            if (request !== scFxMsRequest) return
            scFxMsTimeout.stop()
            scFxMsSending = false; scFxMsSent = -1
            if (scFxMsWanted === scFxTimeMs) scFxMsWanted = -1
            scFxMsFlush()
        })
    }
    function scFxMsSet(ms) {
        scFxMsWanted = ms
        scFxMsFlush()
    }
    function scFxMsController(parameter) {
        if (scFxTimeMode !== "ms" || !parameter) return null
        for (let i = 0; i < scFxControllers.length; ++i) {
            const c = scFxControllers[i]
            if (!c || !c.djFxView || c.scFxBusy) continue
            const mode = c.scFxSelected - c.scFxStock.length + 1
            if ((mode === 1 || mode === 2 || mode === 4) && c.djFxView.variantParameters[0] === parameter) return c
        }
        return null
    }
    function scFxMsStep(parameter, delta) {
        if (!scFxMsController(parameter)) return false
        const base = scFxMsWanted >= 10 ? scFxMsWanted : (scFxMsSending ? scFxMsSent : scFxTimeMs)
        scFxMsSet(Math.max(10, Math.min(4000, base + delta * 10)))
        return true
    }
    function scFxMsKnob(parameter) {
        if (!scFxMsController(parameter)) return
        // The verified native Rate dispatcher has exactly eleven positions.
        const rates = [1/16, 1/8, 1/4, 1/2, 3/4, 1, 2, 4, 8, 16, 32]
        const milliseconds = [10, 18, 33, 60, 110, 200, 364, 663, 1207, 2197, 4000]
        const index = rates.indexOf(parameter.value)
        if (index >= 0) scFxMsSet(milliseconds[index])
    }
    Repeater {
        model: root.scFxSelectorAvailable ? 6 : 0
        delegate: Item {
            id: scCtrl
            width: 0; height: 0; visible: false

    // Scripted actions cross QQmlEngine/context boundaries through Planck.
    readonly property int scFxUnit: index + 1
    property var djFxView: null
    readonly property string scFxPath: Config.audio.fx.sendEffectSlots > 0
        ? "/Engine/Mixer/DJFxUnit" + scFxUnit + "/"
        : "/Engine/Mixer/Channel" + (scFxUnit - 2) + "/DJFx/"
    readonly property var scFxType: Planck.getProperty(scFxPath + "Type")
    readonly property string scFxTypeName: scFxType && scFxType.translator ? scFxType.translator.string : ""
    onScFxTypeNameChanged: {
        if (scFxSelected >= 0 && !scFxBusy && scFxTypeName !== "Flanger") {
            scFxSelected = -1; scFxIndex = djFxView ? djFxView.selectionIndex : -1
            root.scCall("fxUnit=" + scFxUnit + "&fxMode=0")
        }
    }
    Connections {
        target: scCtrl.djFxView
        function onSelectionIndexChanged() {
            if (scCtrl.scFxSelected < 0 && !scCtrl.scFxBusy && !scCtrl.scFxBrowsing) scCtrl.scFxIndex = scCtrl.djFxView.selectionIndex
        }
    }
    readonly property string scFxBus: "EngineXFx" + scFxUnit
    readonly property var scFxProps: [
        Planck.createProperty(scFxBus + "Plus", false, 3),
        Planck.createProperty(scFxBus + "Minus", false, 3),
        Planck.createProperty(scFxBus + "Accept", false, 3),
        Planck.createProperty(scFxBus + "Ready", false, 1)]
    readonly property var scFxReady: scFxProps && Planck.getProperty("/GUI/Scripted/" + scFxBus + "Ready")
    readonly property var scFxStock: djFxView ? Array.from(djFxView.effectsNames) : []
    readonly property bool scFxEnabled: !!root.scFxSelectorAvailable && scFxStock.indexOf("Flanger") >= 0
    readonly property var scFxNames: scFxEnabled ? scFxStock.concat([
        "Spiral (PX)", "Pitch Echo (PX)", "Slip Roll (PX)", "Dub Echo (PX)", "Space (PX)"]) : scFxStock
    readonly property var scFxTimeParameter: djFxView && djFxView.variantParameters.length ? djFxView.variantParameters[0] : null
    Connections {
        target: scCtrl.scFxTimeParameter
        function onValueChanged() { root.scFxMsKnob(scCtrl.scFxTimeParameter) }
    }
    property int scFxIndex: -1
    property int scFxSelected: -1
    property int scFxRequest: 0
    property bool scFxBusy: false
    property bool scFxBrowsing: false
    property int scFxQueued: -1
    Timer {
        interval: 2000; running: scCtrl.scFxBusy
        onTriggered: {
            ++scCtrl.scFxRequest
            scCtrl.scFxBusy = false; scCtrl.scFxQueued = -1
            scCtrl.scFxIndex = scCtrl.scFxSelected
        }
    }
    function scFxSetReady() {
        if (scFxReady && scFxReady.translator) scFxReady.translator.state = scFxEnabled
    }
    onScFxEnabledChanged: scFxSetReady()
    Component.onCompleted: {
        const next = root.scFxControllers.slice(); next[index] = scCtrl
        root.scFxControllers = next
        scFxSetReady()
    }
    Component.onDestruction: {
        if (scFxHandshakePhase && scFxMix && scFxMix.translator) scFxMix.translator.value = scFxSavedWet
        if (scFxReady && scFxReady.translator) scFxReady.translator.state = false
    }
    function scFxTurn(delta) {
        if (!scFxEnabled || !djFxView) return
        let at = scFxIndex >= 0 ? scFxIndex : (scFxSelected >= 0 ? scFxSelected : djFxView.selectionIndex)
        const next = Math.max(0, Math.min(scFxNames.length - 1, at + delta))
        if (next === at) return
        scFxIndex = next
        if (Config.device.hasDJFxContinuousKnobs) scFxAccept(scFxIndex)
        else { scFxBrowsing = true; djFxView.selecting() }
    }
    function scFxAccept(index) {
        if (!djFxView || index < 0 || index >= scFxNames.length) return
        if (!scFxEnabled) { djFxView.selectEffect(index); return }
        if (scFxBusy) { scFxQueued = index; return }
        const mode = index >= scFxStock.length ? index - scFxStock.length + 1 : 0
        const request = ++scFxRequest
        scFxIndex = index
        scFxBusy = true
        // The hook acknowledgement is a barrier: mapping ALWAYS precedes Type.
        root.scCall("fxUnit=" + scFxUnit + "&fxMode=" + mode, function(reply) {
            if (request !== scFxRequest || !djFxView) return
            scFxSelected = mode ? index : -1
            scFxBrowsing = false
            if (!mode || reply.fxBind) djFxView.selectEffect(mode ? scFxStock.indexOf("Flanger") : index)
            if (mode && reply.fxBind) {
                scFxMix = Planck.getProperty(scFxPath + "Mix")
                if (!scFxMix || !scFxMix.translator) { scFxBusy = false; return }
                scFxSavedWet = scFxMix.translator.value
                scFxChallengeB = reply.fxChallengeB
                scFxMix.translator.value = reply.fxChallengeA
                scFxHandshakePhase = 1
                scFxHandshake.start()
            } else scFxFinish()
            djFxView.abortSelecting()
        })
    }
    property var scFxMix: null
    property real scFxSavedWet: 0
    property real scFxChallengeB: 0
    property int scFxHandshakePhase: 0
    Connections {
        target: scCtrl.scFxMix ? scCtrl.scFxMix.translator : null
        function onValueChanged() {
            const value = target.value
            if (scCtrl.scFxHandshakePhase && (value === 0 || value > 0.0001)) scCtrl.scFxSavedWet = value
        }
    }
    Timer {
        id: scFxHandshake
        interval: 40; repeat: true
        onTriggered: {
            if (!scCtrl.scFxMix || !scCtrl.scFxMix.translator) { stop(); scCtrl.scFxFinish(); return }
            if (scCtrl.scFxHandshakePhase === 1) {
                scCtrl.scFxMix.translator.value = scCtrl.scFxChallengeB
                scCtrl.scFxHandshakePhase = 2
            } else {
                scCtrl.scFxMix.translator.value = scCtrl.scFxSavedWet
                stop(); scCtrl.scFxFinish()
            }
        }
    }
    function scFxFinish() {
        scFxHandshakePhase = 0
        scFxBusy = false
        if (scFxQueued >= 0) { const next = scFxQueued; scFxQueued = -1; scFxAccept(next) }
    }
    ActionTranslatorStateChangeNotifier {
        prop: scCtrl.scFxProps && Planck.getProperty("/GUI/Scripted/" + scCtrl.scFxBus + "Plus")
        enabled: scCtrl.scFxEnabled
        onChanged: scCtrl.scFxTurn(1)
    }
    ActionTranslatorStateChangeNotifier {
        prop: scCtrl.scFxProps && Planck.getProperty("/GUI/Scripted/" + scCtrl.scFxBus + "Minus")
        enabled: scCtrl.scFxEnabled
        onChanged: scCtrl.scFxTurn(-1)
    }
    ActionTranslatorStateChangeNotifier {
        prop: scCtrl.scFxProps && Planck.getProperty("/GUI/Scripted/" + scCtrl.scFxBus + "Accept")
        enabled: scCtrl.scFxEnabled
        onChanged: {
            if (!scCtrl.djFxView) return
            if (!scCtrl.djFxView.selectionPopoutVisible) {
                if (scCtrl.scFxIndex < 0) scCtrl.scFxIndex = scCtrl.scFxSelected >= 0 ? scCtrl.scFxSelected : scCtrl.djFxView.selectionIndex
                scCtrl.scFxBrowsing = true
                scCtrl.djFxView.selecting()
            } else if (scCtrl.scFxIndex >= 0) scCtrl.scFxAccept(scCtrl.scFxIndex)
        }
    }

        }
    }
'''

FX_SELECTOR = r'''
    readonly property int scFxUnit: Config.audio.fx.sendEffectSlots > 0 ? (indicator === "2" ? 2 : 1) : 3
    readonly property var scFxController: root.scFxControllers[scFxUnit - 1] || null
    readonly property var scFxNames: scFxController ? scFxController.scFxNames : (djFxView ? Array.from(djFxView.effectsNames) : [])
    readonly property int scFxIndex: scFxController ? scFxController.scFxIndex : -1
    readonly property int scFxSelected: scFxController ? scFxController.scFxSelected : -1
    function scFxRegister() { if (scFxController && djFxView) scFxController.djFxView = djFxView }
    onScFxControllerChanged: scFxRegister()
    onDjFxViewChanged: scFxRegister()
    Component.onCompleted: scFxRegister()
    function scFxAccept(index) {
        if (scFxController && scFxController.scFxEnabled) scFxController.scFxAccept(index)
        else if (djFxView) djFxView.selectEffect(index)
    }
    function scFxAbort() {
        if (scFxController) {
            scFxController.scFxBrowsing = false
            scFxController.scFxIndex = scFxController.scFxSelected
        }
        if (djFxView) djFxView.abortSelecting()
    }
'''

FX_KNOB = r'''
            readonly property int scFxUnit: fxConfig.hasSendStyleEffects ? Number(modelData) : 3 + Math.max(0, Number(String(modelData).replace("Channel", "")) - 1)
            readonly property string scFxBus: "/GUI/Scripted/EngineXFx" + scFxUnit
            readonly property var scFxReady: Planck.getProperty(scFxBus + "Ready")
            property bool scFxPushOwned: false
            function scFxCanUse() {
                const ready = scFxReady && scFxReady.translator
                return ready && ready.state && Planck.getProperty(scFxBus + "Plus").translator &&
                    Planck.getProperty(scFxBus + "Minus").translator && Planck.getProperty(scFxBus + "Accept").translator
            }
            function scFxAction(suffix) { Planck.getProperty(scFxBus + suffix).translator.state = true }
            function scFxMove(direction, amount) {
                if (!(amount > 0)) return // MIDI zero means no movement.
                // JogOutput normalizes one detent to 1/63; preserve its decoded
                // magnitude rather than changing continuous encoder behaviour.
                const steps = Math.max(1, Math.min(63, Math.round(amount * 63)))
                for (let n = 0; n < steps; ++n) scFxAction(direction > 0 ? "Plus" : "Minus")
            }
'''

FX_JOG_TARGET = r'''
                target: QtObject {
                    property QtObject px: QtObject {
                        readonly property bool editable: true
                        function incValue(amount) { scFxMove(1, amount) }
                        function decValue(amount) { scFxMove(-1, amount) }
                    }
                    function get(channel, value) {
                        return scFxCanUse() ? px : fxConfig.channel[modelData].pEffectIndex.translator
                    }
                }
'''

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
	property string scLang: "@LANG@"
	property string scWave: "denon"
	property string scQuality: "flac"
	property string scNeteaseMode: "official"
	property bool scMasterLimiter: false // #58, applied by engine_ui_hook.c
	property bool scAutoGain: false // #64, applied by engine_ui_hook.c
	// #55: retain the saved preference, but fail closed until a bounded native
	// per-track adapter is verified. See AUTO_ANALYZE.md. No drive/toggle handler
	// may submit a whole-library scan or guess an ITrackDataPtr payload.
	property bool scAutoAnalyze: false
	readonly property bool scAutoAnalyzeAvailable: false
	property bool scOmniSource: false // #62, served by the streaming gateway
	property bool scProlinkShare: false // #59, read by rb-bridge
	property bool scProlinkExport: false // #63, experimental export to CDJs
	// #49: the decks this player owns in the 4-deck layout ("13", "24", "12", "1", "2";
	// empty: no 4-deck layout). Engine refuses loading the others (LoadToDeckFilter).
	property string scFourDeckRole: ""
	// The board (device tree), not Config.device.isPlayer: the 4-deck layout turns a
	// player into a non-player inside Engine (#49).
	property bool scPlayer: false
	readonly property string scBoard: typeof Config !== "undefined" && Config && Config.device ? String(Config.device.name || "") : ""
	readonly property bool scIsPlayerBoard: scPlayer || ["JP07", "JP08", "JP13", "JP14"].indexOf(scBoard) >= 0
	readonly property bool scHasMixer: !scIsPlayerBoard
	readonly property bool scCanFourDeck: scIsPlayerBoard || ["JC11", "JC11S", "JP21", "JP24"].indexOf(scBoard) >= 0
	readonly property bool scHasAutoLoopKnob: typeof Config !== "undefined" && Config && Config.device && Config.device.hasAutoLoopKnob !== undefined ? !!Config.device.hasAutoLoopKnob : ["NH08", "NH08S", "NH10"].indexOf(scBoard) < 0
	readonly property bool scIsMixstream: typeof Config !== "undefined" && Config && Config.audio && Config.audio.fx && Config.audio.fx.hasNHFx !== undefined ? !!Config.audio.fx.hasNHFx : ["NH08", "NH08S", "NH10"].indexOf(scBoard) >= 0
	readonly property bool scHasStemsEq: scHasMixer && (typeof Config !== "undefined" && Config && Config.audio ? !!Config.audio.hasStemsLevel : false)
	readonly property bool scIsAZ04: ["JP24", "RMZ2"].indexOf(scBoard) >= 0
	readonly property var scTransitionFxView: typeof PlanckGUI !== "undefined" && PlanckGUI && PlanckGUI.views ? (PlanckGUI.views.transitionFx || null) : null
	readonly property bool scHasStockTransitionTile: typeof Config !== "undefined" && Config && Config.audio && Config.audio.fx && Config.device
		? (!!Config.audio.fx.hasTransitionFx && !Config.device.hasHardwareFaderFxButtons && !!scTransitionFxView)
		: false
	Connections {
		target: root.scTransitionFxView
		ignoreUnknownSignals: true
		function onEnabledChanged() {
			if (!root.scUiLoaded || !root.scHasMixer || !root.scTransitionFxView) return
			const on = !!root.scTransitionFxView.enabled
			if (on !== root.scUpfaderEcho) {
				root.scUpfaderEcho = on
				root.scCall("upfaderEcho=" + (on ? "1" : "0"))
			}
		}
	}
	function scSetTransitionFx(vfx, on, upfaderOnly) {
		if (!vfx) return
		if (!on) {
			vfx.enabled = false
			return
		}
		const opts = vfx.triggerSourceOptions
		if (opts && opts.length > 0) {
			const idx = upfaderOnly ? 1 : 2
			const val = opts[idx] !== undefined ? opts[idx] : opts[opts.length - 1]
			if (val !== undefined) vfx.setTriggerSource(String(val))
		}
		vfx.enabled = true
	}
	readonly property string scPackBuildTag: @BUILD_TAG@
	property string scBuildTag: ""
	function scAboutBuildTag() {
		if (scBuildTag && scPackBuildTag && scBuildTag !== scPackBuildTag) return scBuildTag + " / ui " + scPackBuildTag
		return scBuildTag || scPackBuildTag
	}
	function scDeckLocked(deck) { const isLine = !!(scShadowDisplay[deck] && scShadowDisplay[deck].lineIn); return scFourDeckRole !== "" && (isLine || scFourDeckRole.indexOf(String(deck)) < 0) && (scPlayer || !!scShadowData(deck) || (((scShadowEngineMask >> deck) & 1) === 1 && !!(scShadowAt[deck] && scShadowAt[deck].path))) }
	// Swipe left loads the role's first deck, swipe right its second (0: stock).
	function scSwipeDeck(right) { return scFourDeckRole === "" ? 0 : parseInt(scFourDeckRole[right ? scFourDeckRole.length - 1 : 0]) }
	property bool scFourDeckOn: false // the saved switch; Engine applies it on restart
	property string scFourDeckRoleSetting: "auto"
	property string scShadowMode: "auto" // from /v1/sc5000/ui: auto (each slot by its source), engine or cdj (every source down one path)
	property int scShadowLineMask: 0 // Engine mode on a console: bit N = a CDJ is on Deck N's Line In, where only the display renderer draws its waveform
	property bool scShadowDisplayOnly: true // the /shadow response is authoritative; missing means the safe default. What a deck without its own displayOnly follows
	property int scShadowEngineMask: 0 // per slot: bit N = Deck N goes through Engine (load, seek, speed); clear = the display renderer draws it and Engine is left alone
	property var scShadowDisplay: ({})
	property var scShadowDisplayRequests: ({})
	property var scShadowGridCache: ({})
	property var scShadowLiveCueCache: ({})
	property int scShadowFrame: 0
	readonly property int scShadowExpiryMs: 3000
	// When stream-sync last confirmed the shadow state (an unchanged version counts):
	// a plain object, so no binding re-evaluates on every long-poll reply.
	readonly property var scShadowLive: ({seen: 0})
	// The display renderer draws deck d: a display slot, or an Engine slot it covers while the track is on its way (scShadowCovers).
	function scShadowVisible(d) { return d >= 1 && d <= 4 && (((scShadowEngineMask >> d) & 1) === 0 || !!scShadowDisplay[d]) && scDeckLocked(d) }
	function scShadowDeckFor(item) {
		// Stock skins put the deck on different wrappers (deckIndex / deckData / deckView).
		for (let p = item; p; p = p.parent) {
			for (const data of [p, p.deckData, p.deckView && p.deckView.deckData, p.view && p.view.deckData]) {
				const d = data && Number(data.deckIndex)
				if (d >= 1 && d <= 4) return d
			}
		}
		return 0
	}
	function scShadowData(d) { return scShadowDisplay[d] || null }
	// How fast a shadow deck's position runs on this player's clock: the rate stream-sync
	// measured (the other player's audio clock, its pitch beyond 0.01 %), else by the pitch.
	function scShadowRate(s) { return s.rate > 0 ? s.rate : Math.max(0, 1 + s.pitch / 100) }
	function scShadowPosition(s, now) {
		if (!s) return 0
		// Unchanged replies keep a sample alive: carry it on until 3 s after the last one.
		const alive = Math.max(s.at, scShadowLive.seen) + scShadowExpiryMs
		const elapsed = s.stale ? 0 : Math.max(0, Math.min(now, alive) - s.at)
		const pos = Math.max(0, s.positionMs + (s.playing ? elapsed * scShadowRate(s) : 0))
		return s.meta && s.meta.durationMs > 0 ? Math.min(s.meta.durationMs, pos) : pos
	}
	// Binary searches keep the source grid and #151 independent of Engine's empty slots.
	function scShadowBeatIndex(beats, ms) {
		let lo = 0, hi = beats.length
		while (lo < hi) { const mid = (lo + hi) >> 1; if (beats[mid].ms < ms) lo = mid + 1; else hi = mid }
		return lo
	}
	function scShadowBeatAt(beats, ms) {
		if (beats.length < 2) return NaN
		const i = Math.max(0, Math.min(beats.length - 2, scShadowBeatIndex(beats, ms) - 1))
		const a = beats[i], b = beats[i + 1]
		if (!(b.ms > a.ms)) return NaN
		return a.number + (ms - a.ms) / (b.ms - a.ms) * (b.number - a.number)
	}
	function scShadowGrid(d, meta, start, end) {
		const beats = meta && meta.beats || []
		const a = scShadowBeatIndex(beats, start), b = Math.min(a + 512, scShadowBeatIndex(beats, end))
		const c = scShadowGridCache[d]
		if (c && c.beats === beats && c.a === a && c.b === b) return c.rows
		const rows = beats.slice(a, b)
		scShadowGridCache[d] = {beats: beats, a: a, b: b, rows: rows}
		return rows
	}
	function scShadowNextCue(d) {
		const s = scShadowData(d), m = s && s.meta
		if (!m) return null
		const pos = scShadowPosition(s, Date.now())
		let best = null
		const cues = scShadowCuesFor(d, s) // indexed: for...of allocates an iterator per 200 ms tick
		for (let i = 0; i < cues.length; i++) { const c = cues[i]; if (c.i > 0 && c.ms > pos + 50 && (!best || c.ms < best.ms)) best = c }
		if (!best) return null
		const beats = m.beats || []
		const totalBeats = scShadowBeatAt(beats, best.ms) - scShadowBeatAt(beats, pos)
		if (!(totalBeats > 0 && totalBeats <= 2048)) return null
		const bars = Math.floor(totalBeats / 4)
		return {text: String(bars) + "." + ((Math.floor(totalBeats) % 4) + 1),
			label: String(best.n || "").slice(0, 32), color: best.c || "#26a6ff", urgent: bars < 4}
	}
	function scShadowCuesFor(d, s) {
		const m = s && s.meta
		if (!m) return scShadowEmptyArray
		if (!Array.isArray(s.liveCues) || !(m.sampleRate > 0)) return m.cues || scShadowEmptyArray
		const c = scShadowLiveCueCache[d], raw = s.liveCues
		let same = !!c && c.key === s.key && c.rate === m.sampleRate && c.cuePos === s.cuePos && c.raw.length === raw.length
		if (same) for (let i = 0; i < raw.length; i++) {
			const a = c.raw[i], b = raw[i]
			if (a.i !== b.i || a.p !== b.p || a.n !== b.n || a.c !== b.c) { same = false; break }
		}
		if (same) return c.rows
		const rows = []
		for (let i = 0; i < raw.length; i++) {
			const q = raw[i], ms = q.p / m.sampleRate * 1000
			if (q.i >= 1 && q.i <= 8 && isFinite(ms) && ms >= 0 && ms <= m.durationMs)
				rows.push({i: q.i, ms: ms, n: q.n || "", c: q.c || "#26a6ff"})
		}
		if (isFinite(s.cuePos) && s.cuePos >= 0 && s.cuePos / m.sampleRate * 1000 <= m.durationMs)
			rows.push({i: 0, ms: s.cuePos / m.sampleRate * 1000, n: "CUE", c: "#ffffff"})
		else if (s.cuePos === undefined) for (const q of m.cues || scShadowEmptyArray) if (q.i === 0) rows.push(q)
		for (const q of m.cues || scShadowEmptyArray) if (q.i === 9) rows.push(q)
		scShadowLiveCueCache[d] = {key: s.key, rate: m.sampleRate, cuePos: s.cuePos, raw: raw, rows: rows}
		return rows
	}
	function scShadowAsset(url) {
		// Assets are display resources served by the gateway, never Engine track paths.
		if (!url) return ""
		return url[0] === "/" ? "http://127.0.0.1:8080" + url : /^https?:/.test(url) ? url : ""
	}
	property var scShadowDisplayText: ({}) // per deck: the newest display reply applied
	function scShadowDisplayFetch(d, key) {
		const s = scShadowData(d)
		if (!scShadowVisible(d) || !s || !key || s.key !== key || s.metadataOnly) return
		const old = scShadowDisplayRequests[d]
		if (old && old.key === key && ((old.busy && Date.now() - old.started < 10000) || Date.now() < old.next)) return
		if (s.meta && !s.meta.detailPending) return // completed overview-only is a valid fallback
		const req = {key: key, busy: true, started: Date.now(), next: 0, tries: old && old.key === key ? old.tries + 1 : 1}
		scShadowDisplayRequests[d] = req
		const x = new XMLHttpRequest()
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/shadow/display?deck=" + d + "&key=" + encodeURIComponent(key))
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || root.scShadowDisplayRequests[d] !== req) return
			req.busy = false
			req.next = Date.now() + Math.min(8000, 500 * Math.pow(2, Math.min(req.tries - 1, 4)))
			const current = root.scShadowData(d)
			if (!root.scShadowVisible(d) || !current || current.key !== key) return
			try {
				if (x.status !== 200) return // including 404: retry this key on the metadata timer
				// A safety retry that brings the same pending reply: keep the meta object
				// (a new one re-creates every tile, grid and cue delegate for nothing).
				const text = x.responseText, last = root.scShadowDisplayText[d]
				if (current.meta && last && last.key === key && last.text === text) {
					if (current.meta.detailPending) req.next = Date.now() + Math.min(30000, 2000 * Math.pow(2, Math.min(req.tries - 1, 4)))
					return
				}
				const m = JSON.parse(text)
				if (m.key !== key) return
				root.scShadowDisplayText[d] = {key: key, text: text}
				// Pending detail: each reply is a 10+ kB JSON with every beat (hundreds of
				// objects). stream-sync says when the detail is ready (displayDetail),
				// which re-reads it at once; until then only a slow safety retry.
				if (m.detailPending) req.next = Date.now() + Math.min(30000, 2000 * Math.pow(2, Math.min(req.tries - 1, 4)))
				const next = Object.assign({}, root.scShadowDisplay)
				next[d] = Object.assign({}, current, {meta: m})
				root.scShadowDisplay = next
				root.scGcLoadSeq++ // the parse garbage of a 10+ kB display reply
			} catch (err) { console.warn("EngineX shadow display: " + err) }
		}
		x.send()
	}
	// The shadow decks' cue countdown clock: only while one of them plays. Every
	// tick re-evaluates each deck view's countdown (~36 V4 allocations).
	function scShadowAnyPlaying(m) {
		for (let d = 1; d <= 4; d++) if (m[d] && m[d].playing && !m[d].stale) return true
		return false
	}
	Timer {
		interval: 200; repeat: true
		running: root.scFourDeckRole !== "" && root.scShadowEngineMask !== 30 && root.scShadowAnyPlaying(root.scShadowDisplay)
		onTriggered: root.scShadowFrame = (root.scShadowFrame + 1) & 0xffff
	}
	Timer {
		interval: 500; repeat: true; running: root.scFourDeckRole !== "" && root.scShadowEngineMask !== 30
		onTriggered: {
			for (let d = 1; d <= 4; d++) {
				const s = root.scShadowData(d)
				if (s && s.key) root.scShadowDisplayFetch(d, s.key)
			}
		}
	}
	// V4 collects stop-the-world on the GUI thread: 100-180 ms on SC5000 for the
	// stock heap plus ours, i.e. a visible hitch of every moving waveform. Start
	// each playback on a clean heap instead: collect once when every deck on this
	// screen (own and shadow) has stopped, and once after a track or its display
	// data finished loading while nothing plays. Never while a deck plays.
	property int scGcLoadSeq: 0
	property int scGcRuns: 0
	property real scGcLastMs: 0
	property var scGcProps: null
	property var scGcState: ({armed: true, quietSince: 0, lastRun: -Infinity, loadSig: 0, loadSeq: 0, loadAt: 0, followAt: 0}) // start-up garbage counts as a stop
	function scGcCheck() {
		const now = Date.now(), g = scGcState
		let p = scGcProps
		if (!p) {
			p = []
			for (let d = 1; d <= 4; d++) p.push({play: scTr("/Engine/Deck" + d + "/PlayState"), cmd: scTr("/Engine/Deck" + d + "/Play"), length: scTr("/Engine/Deck" + d + "/Track/TrackLength")})
			if (!p[0].play || !p[0].cmd || !p[0].length) return false // wait for deck 1 to build
			scGcProps = p
		}
		let playing = false, sig = 0
		for (let d = 1; d <= 4; d++) {
			// Playing also means a held CUE preview: PlayState stays false while it
			// sounds and scrolls (scShadowTick's sample, own decks), or the Play command.
			const x = p[d - 1], s = scShadowDisplay[d], own = scShadowPrev[d]
			if ((x.play && x.play.state) || (x.cmd && x.cmd.state) || (own && own.playing) || (s && s.playing && !s.stale)) playing = true
			sig = sig * 31 + (x.length ? x.length.unnormalized || 0 : 0)
		}
		if (sig !== g.loadSig || scGcLoadSeq !== g.loadSeq) { g.loadSig = sig; g.loadSeq = scGcLoadSeq; g.loadAt = now }
		if (playing) { g.armed = true; g.quietSince = 0; g.loadAt = 0; g.followAt = 0; return false }
		if (!g.quietSince) g.quietSince = now
		// 1.5 s settle: a cue-play tap or a quick pause/play must not collect.
		const stopped = g.armed && now - g.quietSince >= 1500
		const loaded = g.loadAt > 0 && now - g.loadAt >= 1500
		// Engine keeps loading (waveform, analysis, library) for seconds after the
		// length is known: collect once more 6 s on if the deck still rests.
		const follow = g.followAt > 0 && now >= g.followAt
		// 3 s apart: a load and its display data arriving a moment later both count.
		if (!(stopped || loaded || follow) || now - g.lastRun < 3000) return false
		g.followAt = loaded ? now + 6000 : 0
		g.armed = false; g.loadAt = 0; g.lastRun = now
		gc()
		scGcRuns++; scGcLastMs = Date.now() - now
		return true
	}
	Timer { interval: 500; repeat: true; running: true; onTriggered: root.scGcCheck() }
	// True when e shows exactly what s shows: the peer's 500 ms heartbeats and
	// steady playback then only move the extrapolation anchor (read imperatively
	// by the displays) instead of re-running every shadow binding per reply.
	function scShadowSameSample(s, key, e) {
		if (s.key !== key || s.name !== (e.name || "") || s.title !== (e.title || "") || s.artist !== (e.artist || "") || s.keyName !== (e.keyName || "") ||
			s.playing !== !!e.playing || s.stale !== !!e.stale || s.metadataOnly !== !!e.metadataOnly || s.lineIn !== !!e.lineIn || s.pitch !== (Number(e.pitch) || 0) ||
			s.cuePos !== (typeof e.cuePos === "number" ? e.cuePos : undefined) || s.master !== !!e.master ||
			s.lead !== (e.lead !== undefined && e.lead !== null ? !!e.lead : !!e.master) || s.sync !== !!e.sync || s.keylock !== !!e.keylock ||
			s.bpm !== (Number(e.bpm) || 0) || s.currentBpm !== (Number(e.currentBpm) || 0) || s.range !== (Number(e.range) || 0) ||
			s.loading !== !!e.loading || s.displayDetail !== !!e.displayDetail || s.progress !== (Number(e.progress) || 0) || s.error !== (e.error || "") ||
			s.engine !== !!e.engine || s.copying !== !!e.copying || s.durationMs !== (Number(e.durationMs) || 0) || s.fallback !== (e.fallback || "")) return false
		if (Array.isArray(e.cues)) {
			const a = s.liveCues, b = e.cues
			if (!Array.isArray(a) || a.length !== b.length) return false
			for (let i = 0; i < b.length; i++) if (a[i].i !== b[i].i || a[i].p !== b[i].p || a[i].n !== b[i].n || a[i].c !== b[i].c) return false
		}
		return true
	}
	function scShadowApplyDisplay(decks) {
		const now = Date.now()
		let next = null // copied on the first real change only
		for (let d = 1; d <= 4; d++) {
			let e = null
			for (let i = 0; i < decks.length; i++) if (Number(decks[i].deck) === d) { e = decks[i]; break }
			const old = scShadowData(d)
			const isLine = !!(e && e.lineIn)
			if (!e || scFourDeckRole === "" || (!isLine && scFourDeckRole.indexOf(String(d)) >= 0) || !(e.key || e.loading)) {
				if (old) { if (!next) next = Object.assign({}, scShadowDisplay); delete next[d] }
				delete scShadowDisplayRequests[d]; delete scShadowGridCache[d]; delete scShadowLiveCueCache[d]
				continue
			}
			const key = String(e.key || ""), pos = Number(e.positionMs) || 0, at = Number(e.at) || now
			const same = !!old && scShadowSameSample(old, key, e)
			if (same) {
				// Same picture: a paused deck at the same spot, or a playing one within
				// 12 ms of its line (the scroll keeps running within 12 ms too).
				// Refresh the anchor in place well before the 3 s expiry.
				const drift = old.playing ? old.positionMs + (at - old.at) * scShadowRate(old) - pos : old.positionMs - pos
				if (Math.abs(drift) <= (old.playing ? 12 : 0)) {
					old.positionMs = pos; old.at = at; old.rate = Number(e.rate) || 0; old.beat = Number(e.beat) || 0
					continue
				}
			}
			const sample = {key: key, name: e.name || "", title: e.title || "", artist: e.artist || "", keyName: e.keyName || "",
				playing: !!e.playing, stale: !!e.stale, metadataOnly: !!e.metadataOnly, positionMs: pos, at: at,
				pitch: Number(e.pitch) || 0, rate: Number(e.rate) || 0, lineIn: isLine,
				cuePos: typeof e.cuePos === "number" ? e.cuePos : undefined, master: !!e.master,
				lead: e.lead !== undefined && e.lead !== null ? !!e.lead : !!e.master, sync: !!e.sync, keylock: !!e.keylock,
				beat: Number(e.beat) || 0, bpm: Number(e.bpm) || 0, currentBpm: Number(e.currentBpm) || 0, range: Number(e.range) || 0, loading: !!e.loading, displayDetail: !!e.displayDetail,
				jumps: Number(e.jumps) || 0,
				progress: Number(e.progress) || 0, error: e.error || "", engine: !!e.engine, copying: !!e.copying, durationMs: Number(e.durationMs) || 0,
				fallback: e.fallback || "",
				meta: old && old.key === key ? old.meta : null,
				liveCues: Array.isArray(e.cues) ? (same ? old.liveCues : e.cues) : old && old.key === key ? old.liveCues : null}
			if (!next) next = Object.assign({}, scShadowDisplay)
			next[d] = sample
			if (!old || old.key !== key) delete scShadowDisplayRequests[d]
		}
		if (next) scShadowDisplay = next
		for (let d = 1; d <= 4; d++) {
			const s = scShadowDisplay[d]
			if (!s || !s.key || (s.meta && !s.meta.detailPending)) continue
			const r = scShadowDisplayRequests[d]
			if (s.meta && s.displayDetail && r && r.key === s.key && !r.busy) r.next = 0 // ready here now
			scShadowDisplayFetch(d, s.key)
		}
	}
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
	readonly property var scViewPrefs: [["scWave", "waveform"], ["scUIScale", "uiScale"],
		["scTracklistWaveform", "tracklistWaveform"], ["scDualColumn", "browserDualColumn"], ["scCueCountdown", "cueCountdown"], ["scBeatJumpPads", "beatJumpPads"], ["scSwapLoopOrder", "swapLoopOrder"]]
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
		scViewLast = ""; scViewDirty = true
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
			root.scViewCheck(Date.now())
		}
	}
	// Autosave compares a snapshot of the view (setting names, Engine and EngineX
	// switches, JSON) with the last one: ~230 V4 allocations, every 2 s. Take it
	// only after a change signal, plus every 30 s should one be missed.
	property bool scViewDirty: true
	property bool scViewHooked: false
	property real scViewCheckAt: 0
	function scViewMark() { scViewDirty = true }
	onScWaveChanged: scViewDirty = true
	onScUIScaleChanged: scViewDirty = true
	onScTracklistWaveformChanged: scViewDirty = true
	onScDualColumnChanged: scViewDirty = true
	onScCueCountdownChanged: scViewDirty = true
	onScBeatJumpPadsChanged: scViewDirty = true
	onScSwapLoopOrderChanged: scViewDirty = true
	function scViewHook() {
		const m = scViewControls()
		if (!m) return
		for (const k in m) if (m[k] && m[k].settingChanged) m[k].settingChanged.connect(root.scViewMark)
		for (const path of ["/GUI/PadsViewEnabled", "/GUI/FxViewEnabled"]) {
			const p = Planck.getProperty(path)
			if (p && p.translator && p.translator.stateChanged) p.translator.stateChanged.connect(root.scViewMark)
		}
		const c = PlanckGUI.views.performanceViewController
		if (c.libraryView && c.libraryView.visibleSettingValueChanged) c.libraryView.visibleSettingValueChanged.connect(root.scViewMark)
		if (c.deckController && c.deckController.albumArtVisibleChanged) c.deckController.albumArtVisibleChanged.connect(root.scViewMark)
		scViewHooked = true
	}
	function scViewCheck(now) {
		if (now < scViewQuiet) return
		if (!scViewHooked) scViewHook()
		if (!scViewDirty && now - scViewCheckAt < 30000) return
		scViewDirty = false; scViewCheckAt = now
		const s = scViewSnapshot()
		const j = s ? JSON.stringify(s) : ""
		if (!j) return
		if (scViewLast === "") scViewLast = j // what the applied view settled to
		else if (j !== scViewLast) scViewSave()
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
	// edge (play, CUE, a jump, pitch, track), else every 500 ms; its Go pushes the
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
	// A paused deck must not invalidate the other decks' cue bindings.
	property int scCueProbe: 0
	function scCuePlaying(d, probe) {
		const t = root.scTr("/Engine/Deck" + d + "/PlayState")
		return !!t && t.state
	}
	function scProbeCueClocks() {
		if (!root.scCueCountdown) return
		// Reuse the existing settings poll to retry properties absent at startup.
		// Once all translators are cached this does nothing; no extra polling Timer.
		if (!scPropCache["/Engine/Deck1/PlayState"] || !scPropCache["/Engine/Deck2/PlayState"] ||
			!scPropCache["/Engine/Deck3/PlayState"] || !scPropCache["/Engine/Deck4/PlayState"])
			root.scCueProbe = (root.scCueProbe + 1) & 0xffff
	}
	Timer {
		id: scCueClock1
		property int tick: 0
		interval: 200; repeat: true
		running: root.scCueCountdown && root.scCuePlaying(1, root.scCueProbe)
		onTriggered: tick = (tick + 1) & 0xffff
	}
	Timer {
		id: scCueClock2
		property int tick: 0
		interval: 200; repeat: true
		running: root.scCueCountdown && root.scCuePlaying(2, root.scCueProbe)
		onTriggered: tick = (tick + 1) & 0xffff
	}
	Timer {
		id: scCueClock3
		property int tick: 0
		interval: 200; repeat: true
		running: root.scCueCountdown && root.scCuePlaying(3, root.scCueProbe)
		onTriggered: tick = (tick + 1) & 0xffff
	}
	Timer {
		id: scCueClock4
		property int tick: 0
		interval: 200; repeat: true
		running: root.scCueCountdown && root.scCuePlaying(4, root.scCueProbe)
		onTriggered: tick = (tick + 1) & 0xffff
	}
	function scCueTickFor(d) {
		// A resting shadow deck's countdown does not move: no clock dependency.
		if (scShadowVisible(d)) return scShadowDisplay[d] && scShadowDisplay[d].playing && !scShadowDisplay[d].stale ? scShadowFrame : 0
		return d === 1 ? scCueClock1.tick : d === 2 ? scCueClock2.tick : d === 3 ? scCueClock3.tick : scCueClock4.tick
	}
	// Same object while text/label/color/urgent are unchanged, so bindings do not re-layout every tick.
	property var scCueInfoCache: ({})
	// Every deck view (and layout variant) evaluates its countdown on the same
	// clock tick: compute once per deck and tick, the rest share the answer.
	property var scCueInfoMemo: ({})
	function scNextCueInfo(d, tick) {
		// Same tick within 50 ms: the same clock edge. A later re-evaluation (a load
		// or cue edit while paused) computes afresh.
		const memo = scCueInfoMemo[d], now = Date.now()
		if (typeof tick === "number" && tick !== 0 && memo && memo.tick === tick && now - memo.at < 50) return memo.info
		const info = scNextCueCompute(d)
		if (typeof tick === "number" && tick !== 0) {
			if (memo) { memo.tick = tick; memo.info = info; memo.at = now } else scCueInfoMemo[d] = {tick: tick, info: info, at: now}
		}
		return info
	}
	function scNextCueCompute(d) {
		if (scShadowVisible(d)) {
			const info = scShadowNextCue(d)
			if (!info) { delete scCueInfoCache[d]; return null }
			const c = scCueInfoCache[d]
			if (c && c.text === info.text && c.label === info.label && c.color === info.color && c.urgent === info.urgent) return c
			scCueInfoCache[d] = info
			return info
		}
		const p = scCueProps(d)
		if (!p.loaded || !p.loaded.state) return null
		const pos = p.position ? p.position.unnormalized || 0 : 0
		const sr = p.sampleRate ? p.sampleRate.value || 44100 : 44100
		const bpm = (p.bpm ? p.bpm.unnormalized : 0) || (p.trackBpm ? p.trackBpm.unnormalized : 0) || 0
		if (!(sr > 0 && bpm > 0)) return null
		let best = -1, bestPos = Infinity
		for (let i = 0; i < p.cues.length; i++) {
			const cue = p.cues[i]
			if (!cue.exists || !cue.exists.state || !cue.position) continue
			const cp = cue.position.value
			if (!(cp > pos + sr * 0.05) || cp >= bestPos) continue
			best = i; bestPos = cp
		}
		if (best < 0) return null
		const totalBeats = (bestPos - pos) / sr * (bpm / 60.0)
		if (totalBeats <= 0 || totalBeats > 2048) return null
		const bars = Math.floor(totalBeats / 4)
		const cue = p.cues[best]
		const text = String(bars) + "." + ((Math.floor(totalBeats) % 4) + 1)
		const label = (cue.name ? cue.name.string || "" : "").slice(0, 32)
		const color = String((cue.color && cue.color.color) || "#26a6ff")
		const urgent = bars < 4
		const c = scCueInfoCache[d]
		if (c && c.text === text && c.label === label && c.color === color && c.urgent === urgent) return c
		const info = {text: text, label: label, color: color, urgent: urgent}
		scCueInfoCache[d] = info
		return info
	}
	property var scCuePropCache: ({})
	function scCueProps(d) {
		if (scCuePropCache[d]) return scCuePropCache[d]
		const prefix = "/Engine/Deck" + d
		const t = p => root.scTr(prefix + p)
		const p = {loaded: t("/Track/SongLoaded"), position: t("/Track/TrackPosition"), sampleRate: t("/Track/SampleRate"),
			bpm: t("/OriginalBPM"), trackBpm: t("/Track/CurrentBPM"), cues: []}
		for (let i = 1; i <= 8; i++) {
			const key = "/Track/QuickCue" + i
			p.cues.push({exists: t(key + "Exists"), position: t(key + "Position"), name: t(key + "Name"), color: t(key + "Color")})
		}
		// Retry discovery if the Engine deck has not been constructed yet.
		if (p.loaded && p.position && p.cues.every(c => c.exists && c.position && c.name && c.color)) scCuePropCache[d] = p
		return p
	}
	property var scShadowCueCache: ({})
	// 25 Hz around every edge and a CUE preview, 12.5 Hz in steady playback (the
	// sender posts the filtered line twice a second anyway), 5 Hz while all own
	// decks rest. Each tick reads ~20 Engine properties, and every read allocates
	// on V4's heap: at 25 Hz this was a third of the idle allocation (GC trigger).
	// Play and CUE presses wake it at once.
	Timer {
		id: scShadowTickTimer
		interval: 40; repeat: true
		running: root.scFourDeckRole !== ""
		onTriggered: root.scShadowTick()
	}
	property real scShadowBusyAt: 0
	property bool scShadowRoleDirty: true
	onScFourDeckRoleChanged: scShadowRoleDirty = true
	function scShadowWake() {
		scShadowBusyAt = Date.now()
		if (scShadowTickTimer.interval !== 40) scShadowTickTimer.interval = 40
	}
	Timer {
		id: scShadowPollTimer
		onTriggered: if (root.scFourDeckRole !== "") root.scShadowPoll()
	}
	property bool scShadowCdjMaster: false // a CDJ on a shadow deck holds master
	// Engine Link hands master to a deck that still plays when the master stops, and
	// a copy plays on for the update's trip: it is left holding master. A copy is
	// never master: a real deck is, as on stock (where a stopped master keeps it).
	property int scShadowLead: 0 // our copy that holds master, told to the peer (shadowMaster)
	property int scShadowPeerLead: 0 // our deck whose copy on the peer holds master
	property bool scShadowPeerPlaying: false
	property real scShadowMasterAt: 0
	// The real deck that plays takes master, the copy's own first; none plays: the copy's.
	function scShadowMasterBack(mine, now) {
		if (now < scShadowMasterAt || (!scShadowLead && mine.indexOf(scShadowPeerLead) < 0)) return
		let playing = 0
		for (let n = 0; n < mine.length; n++) if (scShadowPrev[mine[n]].ps) { playing = mine[n]; break }
		const n = scShadowPeerLead, p = scShadowPrev[n]
		const want = scShadowLead ? (scShadowProperties(scShadowLead).play.state ? 0 : playing) : p.ps ? n : playing || (scShadowPeerPlaying || !p.loaded ? 0 : n)
		if (!want || scShadowPrev[want].master) return
		scPress("/Engine/Deck" + want + "/RequestMaster")
		scShadowMasterAt = now + 800
	}
	// Engine's buttons (RequestMaster, Sync, UnSync) act on false -> true and only
	// see the value between event-loop turns: release, press, release, 120 ms
	// apart. /Engine/DeckN/Play is not one of them: it is the play state itself
	// (true plays, false pauses), so it is written, not pressed.
	property var scPressQueue: []
	function scPress(path) {
		const remote = /^[/]Engine[/]Deck([1-4])[/]/.exec(path)
		if (remote && ((scShadowEngineMask >> Number(remote[1])) & 1) === 0 && scShadowVisible(Number(remote[1]))) return
		scPressQueue = scPressQueue.concat([{path: path, step: 0}])
		scPressTimer.restart()
	}
	Timer {
		id: scPressTimer
		interval: 120
		onTriggered: {
			const next = []
			for (const e of root.scPressQueue) {
				const remote = /^[/]Engine[/]Deck([1-4])[/]/.exec(e.path)
				if (remote && ((root.scShadowEngineMask >> Number(remote[1])) & 1) === 0 && root.scShadowVisible(Number(remote[1]))) continue
				const t = root.scTr(e.path)
				if (!t) continue
				t.state = e.step === 1
				if (e.step < 2) next.push({path: e.path, step: e.step + 1})
			}
			root.scPressQueue = next
			if (next.length) restart()
		}
	}
	// Reuse the sampling buffers, translators and HTTP object after warm-up.
	readonly property var scShadowEmptyArray: []
	readonly property var scShadowEmptyObject: ({})
	readonly property var scShadowDecks: [1, 2, 3, 4]
	property string scShadowMineRole: "?"
	property var scShadowMine: []
	property var scShadowSampleSpare: ({})
	property var scShadowDeckProperties: ({})
	property var scShadowOwn: []
	property var scShadowPostRequest: null
	property var scShadowPollRequest: null
	property string scShadowResponseVersion: ""
	property bool scShadowIdle: false
	readonly property var scShadowLinePaths: ["/Engine/Mixer/Channel1/Line", "/Engine/Mixer/Channel2/Line", "/Engine/Mixer/Channel3/Line", "/Engine/Mixer/Channel4/Line"]
	property bool scShadowPostPending: false
	property string scShadowLinkId: ""
	property var scShadowSent: [] // per own deck: what the newest full post told the peer
	property real scShadowFullAt: 0
	property bool scShadowPostFull: true
	property real scShadowLinkAt: -Infinity
	property var scShadowTextAt: ({})
	function scShadowProperties(d) {
		let p = scShadowDeckProperties[d]
		if (p) return p
		p = {}
		const prefix = "/Engine/Deck" + d
		for (const pair of [["rate", "/Track/SampleRate"], ["shown", "/SpeedState"], ["original", "/OriginalBPM"],
			["current", "/CurrentBPM"], ["play", "/PlayState"], ["position", "/Track/TrackPosition"],
			["loaded", "/Track/SongLoaded"], ["uri", "/Track/TrackUri"], ["net", "/Track/TrackNetworkPath"],
			["cueLED", "/Track/CueLEDState"], ["master", "/DeckIsMaster"], ["sync", "/SyncMode"],
			["beat", "/BeatNumber"], ["cuePosition", "/Track/CuePosition"], ["title", "/Track/SongName"],
			["trackName", "/Track/TrackName"], ["artist", "/Track/ArtistName"], ["length", "/Track/TrackLength"],
			["range", "/SpeedRange"], ["keylock", "/Track/KeyLock"], ["currentKey", "/Track/CurrentKey"],
			["loop", "/Track/LoopEnableState"], ["keyOffset", "/Track/KeyOffset"], ["stemmed", "/Track/Stemmed"],
			["slip", "/Track/SlipModeActive"], ["slipPosition", "/Track/SlipModePosition"]])
			p[pair[0]] = root.scTr(prefix + pair[1]) || scShadowEmptyObject
		p.cues = []
		for (let i = 1; i <= 8; i++) {
			const q = prefix + "/Track/QuickCue" + i
			p.cues.push({exists: root.scTr(q + "Exists") || scShadowEmptyObject,
				position: root.scTr(q + "Position") || scShadowEmptyObject, name: root.scTr(q + "Name") || scShadowEmptyObject,
				color: root.scTr(q + "Color") || scShadowEmptyObject})
		}
		// Rare signals: a play or CUE press restores the 25 Hz sampling at once.
		for (const t of [p.play, p.cueLED]) if (t.stateChanged) t.stateChanged.connect(root.scShadowWake)
		scShadowDeckProperties[d] = p
		return p
	}
	function scShadowTick() {
		const now = Date.now()
		if (now - scShadowPollAt > 3000) scShadowPoll()
		scShadowLine(now)
		scShadowLandingTick(now)
		if (scShadowRoleDirty) {
			scShadowRoleDirty = false
			scShadowMineRole = scFourDeckRole
			scShadowMine = scFourDeckRole.split("").map(c => parseInt(c))
			scShadowOwn.length = scShadowMine.length
		}
		const mine = scShadowMine
		let edge = scShadowPostPending, busy = false, moving = false
		for (let n = 0; n < mine.length; n++) {
			const d = mine[n], g = scShadowProperties(d), p = scShadowPrev[d]
			const s = scShadowSampleSpare[d] || {}
			const sr = g.rate.value
			const ob = g.original.unnormalized || 0, cb = g.current.unnormalized || 0
			// 25 Hz: every .string read allocates a JS string (and counts toward V4's
			// GC trigger). Re-read the strings only when they can have changed.
			if (p && p.bpm === ob && p.currentBpm === cb && now - p.pitchAt < 1000) { s.pitch = p.pitch; s.pitchAt = p.pitchAt }
			else {
				const shown = parseFloat(g.shown.string) || 0
				const exact = ob > 0 && cb > 0 ? Math.round(((cb / ob - 1) * 100) * 100) / 100 : NaN
				// SpeedState is capped at +/-100 even when native Sync runs faster.
				s.pitch = Math.abs(exact - shown) < 0.15 || (shown === 100 && exact > 100) ? exact : shown; s.pitchAt = now
			}
			s.ps = !!g.play.state
			s.posMs = sr > 0 ? g.position.unnormalized / sr * 1000 : 0
			s.rawPos = s.posMs
			s.bpm = ob; s.currentBpm = cb; s.at = now; s.jumps = p ? p.jumps : 0
			s.loaded = !!g.loaded.state
			if (p && p.loaded === s.loaded && now - p.netAt < 500) { s.net = p.net; s.netAt = p.netAt }
			else { s.net = s.loaded ? g.uri.string || g.net.string || "" : ""; s.netAt = now }
			s.cue = !s.ps && !!g.cueLED.state && !!p && s.net === p.net && (s.posMs > p.posMs + 0.5 || p.cue)
			s.playing = s.ps || s.cue
			s.master = !!g.master.state
			if (p && p.bpm === ob && p.currentBpm === cb && now - p.syncAt < 200) { s.sync = p.sync; s.syncAt = p.syncAt; s.range = p.range; s.rangeIndex = p.rangeIndex; s.keylock = p.keylock }
			else {
				s.sync = (g.sync.string || "Off") !== "Off"; s.syncAt = now; s.keylock = !!g.keylock.state
				// .string allocates: the range is read again only when its index moved.
				s.rangeIndex = g.range.index
				s.range = p && p.rangeIndex === s.rangeIndex ? p.range : parseFloat(g.range.string) || 0
			}
			const syncChange = p && s.sync && (!p.sync || Math.abs(s.pitch - p.pitch) > 0.02)
			s.syncUntil = !s.sync ? 0 : syncChange ? now + 2000 : p ? p.syncUntil || 0 : 0
			const syncMoving = s.ps && now < s.syncUntil
			const freshRaw = p && s.rawPos - p.rawPos > (now - p.at) * (1 + s.pitch / 100) * 0.5
			s.rawStillAt = freshRaw || !p || s.ps !== p.ps || s.net !== p.net ? undefined :
				isFinite(p.rawStillAt) ? p.rawStillAt : now
			if (syncChange) {
				// Keep the measured audio-block bias, not the old phase line.
				const bias = isFinite(p.env) && isFinite(p.avg) ? p.env - p.avg : 0
				s.syncBias = Math.max(0, Math.min(35 * (1 + s.pitch / 100), bias))
			} else s.syncBias = p ? p.syncBias || 0 : 0
			if (p) {
				const run = p.playing ? (now - p.at) * (1 + p.pitch / 100) : 0, expect = p.posMs + run
				// A late tick (GC, a busy GUI thread) can run before Engine's queued position
				// update and read the position of before the stall (zs15-b: 340 ms behind,
				// counted as a jump back and a jump forward, each a seek on the peer).
				// Nothing is known then: the deck is where its speed puts it.
				// A delayed stream can keep advancing. Once it fell outside the
				// measured native Sync pull (up to ~27%, plus one audio block),
				// keep carrying it until it catches up or a normal tick resumes.
				const block = 35 * (1 + s.pitch / 100)
				s.lateGuard = syncMoving && now - p.at > 150 && s.rawPos <
					(p.lateGuard ? expect - block : p.rawPos + run - block - run * 0.3)
				if (now - p.at > 150 && s.ps && p.ps && s.net === p.net && !p.phaseHold &&
					!(syncMoving && freshRaw && !s.lateGuard)) s.posMs = expect
				// Sync phase steps can be smaller than 180 ms. Audio-block jitter is
				// measured in wall time, so allow it to grow with the deck's rate.
				const phaseMs = 40 * Math.max(1, 1 + s.pitch / 100)
				const jumpMs = s.sync && p.playing ? phaseMs : (p.playing ? 180 : 5)
				const delta = s.posMs - expect
				const phaseFresh = s.posMs - p.phasePos > (now - p.at) * (1 + s.pitch / 100) * 0.25
				const movingPhase = syncMoving && freshRaw && !s.lateGuard && isFinite(p.phase) &&
					delta * p.phase > 0 && Math.abs(delta - p.phase) > phaseMs
				// A large constant deficit under consecutive late callbacks can
				// still be an advancing queued stream. A ramp changes that deficit;
				// a standalone step waits for the next normal callback, as before.
				const latePhase = syncMoving && now - p.at > 150 && Math.abs(delta) > phaseMs + block && !movingPhase
				s.phase = undefined
				s.phasePos = s.posMs
				// A held platter is not an indefinitely stale GUI read. After
				// 500 ms use its real position once, then keep the normal jog line
				// until playback advances again; never seek on every held tick.
				s.phaseHold = s.sync && s.ps && p.ps && s.net === p.net && !freshRaw &&
					(p.phaseHold || now - s.rawStillAt >= 500)
				if (s.phaseHold) s.posMs = s.rawPos
				if (s.phaseHold) { if (!p.phaseHold) s.jumps++ }
				else if (s.playing === p.playing && s.net === p.net && Math.abs(delta) > jumpMs) {
					// A short GUI stall can also leave one stale audio block. Carry
					// the old line until a small Sync step persists on the next tick;
					// its queued recovery then never becomes a backward/forward seek.
					if (s.sync && p.playing && (Math.abs(delta) <= 180 || (isFinite(p.phase) && !phaseFresh)) &&
						!movingPhase && (latePhase || !isFinite(p.phase) || Math.abs(delta - p.phase) > phaseMs || !phaseFresh)) {
						s.phase = delta; s.phaseAt = isFinite(p.phase) ? p.phaseAt : now; s.posMs = expect
					} else s.jumps++
				}
				if (s.jumps !== p.jumps || s.playing !== p.playing || s.net !== p.net || Math.abs(s.pitch - p.pitch) > 0.02 || s.master !== p.master || s.sync !== p.sync || s.range !== p.range || s.keylock !== p.keylock) edge = true
				if (!s.ps && s.posMs !== p.posMs) busy = true // scrub, seek or CUE preview while paused
			}
			if (s.cue) busy = true
			if (s.ps) moving = true
			// Engine moves TrackPosition in audio blocks: at 25 Hz a playing deck's raw
			// position is up to ~35 ms of track time behind (high pitch). With the speed
			// known, the upper envelope of position - speed * time is the newest block:
			// post that line (noise of a few ms), decaying 0.5 ms/s to follow pitch rounding.
			const speed = 1 + s.pitch / 100, raw = s.posMs - speed * now
			if (s.phaseHold) s.env = s.avg = raw
			else if (syncMoving) {
				// Native Sync pulls phase by changing the transport's instantaneous
				// speed for up to ~1.6 s. The nominal-pitch envelope would conceal
				// that ramp. Publish fresh progress directly during this transition;
				// stale reads still carry the preceding line and cannot seek backwards.
				const accepted = freshRaw && !s.lateGuard && !isFinite(s.phase)
				const pos = !accepted && p && p.ps && s.net === p.net ? p.posMs + (now - p.at) * (1 + p.pitch / 100) : s.rawPos
				s.avg = pos - speed * now; s.env = s.avg + s.syncBias
			}
			else if (s.ps && p && p.ps && p.net === s.net && p.pitch === s.pitch && p.sync === s.sync && p.jumps === s.jumps && isFinite(p.env)) {
				// A jog brake: the positions average ~17 ms of wall time behind the line
				// (25 at most in steady play). Further behind, the deck is slower than its
				// pitch: the line closes a fifth of the excess per tick. No step, so a late
				// position from a busy GUI thread costs a few ms, not a slip.
				s.avg = p.avg + (raw - p.avg) * 0.2
				const slow = p.env - s.avg - 25 * speed
				s.env = Math.max(raw, p.env - (slow > 0 ? slow * 0.2 : 0.0005 * (now - p.at)))
			} else s.env = s.avg = raw
			s.lineMs = s.ps ? s.env + speed * now : s.posMs
			// A jog nudge moves the line off the one the peer carries on: post it at once,
			// not at the next heartbeat (25 Hz for 2 s, so the peer follows the nudge).
			const sent = scShadowSent[n]
			if (s.ps && sent && sent.playing && sent.deck === d && sent.net === s.net && sent.pitch === s.pitch && sent.jumps === s.jumps &&
				Math.abs(s.lineMs - sent.positionMs - (now - sent.at) * speed) > 15) edge = true
			scShadowPrev[d] = s
			scShadowSampleSpare[d] = p || {}
		}
		let lead = 0
		for (let d = 1; d <= 4 && root.scPlayer && !scShadowDisplayOnly; d++) if (mine.indexOf(d) < 0 && (scShadowAt[d] || scShadowEmptyObject).link && scShadowProperties(d).master.state) lead = d
		if (lead !== scShadowLead) { scShadowLead = lead; edge = true }
		scShadowMasterBack(mine, now)
		// Every edge (play, CUE, jump, pitch, master, sync, track) posts at once. The
		// heartbeat only proves liveness (Go marks a peer stale after 2 s) and corrects
		// the peer's extrapolation, which runs on the exact pitch: 500 ms keeps 1.5 s of
		// margin for a busy GUI thread. Each post and the peer's reply are JS strings
		// that drive V4's GC trigger (250 ms here meant a GC every ~75-150 s of play).
		if (edge || now - scShadowPostAt >= 500) scShadowPost(mine, now, edge)
		if (edge || busy) scShadowBusyAt = now
		const rate = now - scShadowBusyAt < 2000 ? 40 : moving ? 80 : 200
		if (scShadowTickTimer.interval !== rate) scShadowTickTimer.interval = rate
		if (scShadowDebug) scShadowDbgFlush(now)
	}
	function scShadowPost(mine, now, edge) {
		let post = scShadowPostRequest
		if (!post) {
			post = new XMLHttpRequest(); scShadowPostRequest = post
			// 409 on a ping: stream-sync restarted or our decks went stale there.
			post.onreadystatechange = function() { if (post.readyState === 4 && post.status === 409) root.scShadowPostFull = true }
		}
		if (post.readyState > 0 && post.readyState < 4) {
			scShadowPostPending = true
			if (now - scShadowPostAt < 3000) return
			post.abort()
		}
		const pNet = root.scTr("/Engine/Sync/Network/MasterStatus")
		const net = !!(pNet && pNet.state)
		let shadowLead = false, localMaster = false, lead = 0
		for (let d = 1; d <= 4; d++) {
			if (!scShadowProperties(d).master.state) continue
			if (mine.indexOf(d) < 0) shadowLead = true
			else localMaster = true
		}
		if (net && !shadowLead && !scShadowCdjMaster && !localMaster) {
			for (let i = 0; i < mine.length; i++) if (scShadowPrev[mine[i]].playing) { lead = mine[i]; break }
			if (!lead) lead = mine[0]
		}
		if (now - scShadowLinkAt >= 2000) { scShadowLinkId = (root.scTr("/Network/NetworkId") || scShadowEmptyObject).string || ""; scShadowLinkAt = now }
		const link = scShadowLinkId
		const own = scShadowOwn
		for (let n = 0; n < mine.length; n++) {
			const d = mine[n], s = scShadowPrev[d], g = scShadowProperties(d)
			let cc = scShadowCueCache[d]
			if (!cc) cc = scShadowCueCache[d] = {net: "?", at: 0, textAt: 0, rev: 0, cues: [], pool: []}
			if (cc.net !== s.net || now - cc.at >= 500) {
				// Names and colours are strings: re-read them with a new or moved cue, else every 2 s.
				const text = cc.net !== s.net || now - cc.textAt >= 2000
				let count = 0, changed = cc.net !== s.net
				for (let i = 0; s.net && i < 8; i++) {
					const q = g.cues[i]
					if (!q.exists.state) continue
					const c = cc.pool[i] || (cc.pool[i] = {})
					const pos = q.position.value
					if (text || c.i !== i + 1 || c.p !== pos || cc.cues[count] !== c) {
						const name = q.name.string || "", color = String(q.color.color || "#ffffff")
						if (name !== c.n || color !== c.c) changed = true
						c.n = name; c.c = color
					}
					if (c.i !== i + 1 || c.p !== pos || cc.cues[count] !== c) changed = true
					c.i = i + 1; c.p = pos
					cc.cues[count++] = c
				}
				if (count !== cc.cues.length) changed = true
				cc.cues.length = count; cc.net = s.net; cc.at = now
				if (text) cc.textAt = now
				if (changed) cc.rev++
			}
			const o = own[n] || (own[n] = {})
			// A track loaded over another: Engine names the file ~0.2 s before its title,
			// artist and length (o5-R2). Still the last track's within 1 s: not filled yet.
			if (o.net && s.net && o.net !== s.net) { cc.was = o.title; cc.wasBy = o.artist; cc.wasAt = now }
			const shift = g.keyOffset.unnormalized || 0 // a key shift: the key's name with it
			const fresh = o.net !== s.net || !(now - (scShadowTextAt[d] || 0) < 2000) || o.keyOffset !== shift
			o.deck = d; o.net = s.net; o.cues = s.net ? cc.cues : null; o.cuePos = s.net ? g.cuePosition.value : undefined
			o.playing = s.playing; o.cue = s.cue; o.link = link; o.positionMs = s.lineMs; o.at = s.at; o.jumps = s.jumps
			o.pitch = s.pitch; o.master = s.master || d === lead; o.lead = s.master; o.bpm = s.bpm; o.currentBpm = s.currentBpm
			o.sync = s.sync; o.beat = g.beat.unnormalized || 0
			o.range = s.range; o.keylock = s.keylock // the peer's shadow deck shows the same range and key lock
			// What this deck's header and overview show beside the track; the peer's shadow deck shows them, its copy takes none.
			o.loop = !!g.loop.state; o.keyOffset = shift; o.stems = !!g.stemmed.state
			o.slip = g.slip.state ? Math.max(1, g.slipPosition.unnormalized || 0) : 0 // slip active: its timeline, in samples
			o.shadowMaster = scShadowLead
			// Titles arrive after the load: re-read on a new track and every 2 s.
			if (fresh) {
				o.title = s.net ? g.title.string || g.trackName.string || "" : ""
				o.artist = s.net ? g.artist.string || "" : ""
				const last = now - cc.wasAt < 1000 && o.title === cc.was && o.artist === cc.wasBy
				if (last) o.title = o.artist = ""
				o.keyName = o.title ? g.currentKey.string || "-" : "" // with the title, as this deck's header writes it ("-": no key)
				const sr = g.rate.value, len = g.length.unnormalized
				o.durationMs = s.net && !last && sr > 0 && len > 0 ? Math.round(len / sr * 1000) : 0 // the peer tells its shadow deck's track by it
				scShadowTextAt[d] = !s.net || o.title ? now : 0 // Engine fills SongName after the load: retry until it does
			}
			o.dbg = ""
			// What the peer shows from this deck, as last sent in full.
			const sent = scShadowSent[n] || (scShadowSent[n] = {})
			// The peer carries the position on from the last full post at its pitch;
			// o.positionMs is the filtered line (scShadowTick), so a real slip only.
			let onLine = false
			if (sent.deck === d && sent.net === o.net && sent.playing === o.playing && sent.pitch === o.pitch && sent.jumps === o.jumps) {
				const expect = sent.positionMs + (o.playing ? (o.at - sent.at) * (1 + o.pitch / 100) : 0)
				onLine = Math.abs(o.positionMs - expect) <= (o.playing ? 15 : 0.5)
			}
			if (!onLine || sent.cue !== o.cue || sent.master !== o.master || sent.lead !== o.lead || sent.sync !== o.sync || sent.cuePos !== o.cuePos ||
				sent.title !== o.title || sent.artist !== o.artist || sent.keyName !== o.keyName || sent.durationMs !== o.durationMs || sent.link !== o.link || sent.bpm !== o.bpm || sent.currentBpm !== o.currentBpm || sent.range !== o.range || sent.keylock !== o.keylock || sent.cuesRev !== cc.rev ||
				sent.loop !== o.loop || sent.keyOffset !== o.keyOffset || sent.stems !== o.stems || sent.slip !== o.slip) edge = true // slip's timeline runs on: in full at every heartbeat while it is active
		}
		scShadowPostAt = now; scShadowPostPending = false
		// Nothing the peer shows changed: ping instead of serialising the decks again
		// (stream-sync keeps them live and extrapolated). Full post at least every 5 s.
		if (!edge && !scShadowPostFull && now - scShadowFullAt < 5000 && own.length === scShadowSent.length) {
			post.open("POST", "http://127.0.0.1:8080/v1/sc5000/decks/alive")
			post.send()
			return
		}
		for (let n = 0; n < own.length; n++) {
			const o = own[n], sent = scShadowSent[n]
			sent.deck = o.deck; sent.net = o.net; sent.playing = o.playing; sent.cue = o.cue; sent.jumps = o.jumps; sent.pitch = o.pitch
			sent.master = o.master; sent.lead = o.lead; sent.sync = o.sync; sent.cuePos = o.cuePos; sent.title = o.title; sent.artist = o.artist; sent.keyName = o.keyName; sent.durationMs = o.durationMs
			sent.link = o.link; sent.bpm = o.bpm; sent.currentBpm = o.currentBpm; sent.range = o.range; sent.keylock = o.keylock; sent.loop = o.loop; sent.keyOffset = o.keyOffset; sent.stems = o.stems; sent.slip = o.slip; sent.cuesRev = scShadowCueCache[o.deck].rev; sent.positionMs = o.positionMs; sent.at = o.at
		}
		scShadowSent.length = own.length
		scShadowFullAt = now; scShadowPostFull = false
		post.open("POST", "http://127.0.0.1:8080/v1/sc5000/decks")
		post.send(JSON.stringify(own))
	}
	// One request at a time: Go holds it until the other player's state changes
	// (at most a second; 120 ms while CDJ positions refresh), so an edge arrives
	// here within a millisecond of its UDP.
	function scShadowPollIdle() {
		if (!scShadowIdle || scShadowDisplayOnly) return
		// A track can still be unloading locally after the peer is empty.
		const now = Date.now()
		for (let d = 1; d <= 4; d++) if (scFourDeckRole.indexOf(String(d)) < 0) root.scShadowEmpty(d, now)
	}
	function scShadowPoll() {
		let x = scShadowPollRequest
		if (!x) {
			x = new XMLHttpRequest()
			scShadowPollRequest = x
			x.onreadystatechange = function() {
				if (x.readyState !== 4) return
				let wait = 500
				try {
					if (x.status === 200) {
						const version = x.getResponseHeader("X-Shadow-State") || ""
						root.scShadowLive.seen = Date.now()
						if (root.scShadowDisplayOnly && version && version === root.scShadowResponseVersion) {
							scShadowPollTimer.interval = 60; scShadowPollTimer.restart(); return
						}
						const unchanged = x.getResponseHeader("X-Shadow-Unchanged") === "1"
						const r = unchanged ? null : JSON.parse(x.responseText)
						if (unchanged || r.unchanged === true) {
							root.scShadowPollIdle()
							wait = 60
						} else {
							root.scShadowIdle = false
							root.scShadowResponseVersion = version
							root.scShadowDebug = !!r.debug
							root.scShadowApplyAll(r.decks || root.scShadowEmptyArray, r.displayOnly !== false)
							root.scShadowIdle = r.displayOnly === false && x.getResponseHeader("X-Shadow-Idle") === "1"
							wait = !r.role ? 500 : r.ver === undefined ? 120 : 60
							if (r.ver !== undefined) root.scShadowVer = r.ver
						}
					}
				} catch (err) { console.warn("EngineX shadow decks: " + err) }
				scShadowPollTimer.interval = wait
				scShadowPollTimer.restart()
			}
		}
		if (x.readyState > 0 && x.readyState < 4) {
			if (Date.now() - scShadowPollAt < 3000) return
			x.abort()
		}
		++scShadowGen
		scShadowPollAt = Date.now()
		let scLines = null
		if (typeof root !== "undefined" && root && root.scTr) {
			for (let d = 1; d <= 4; d++) {
				try {
					const p = root.scTr(root.scShadowLinePaths[d - 1])
					if (p && p.state) (scLines || (scLines = [])).push(d)
				} catch (e) {}
			}
		}
		const scLineQuery = scLines ? "&line=" + scLines.join(",") : ""
		x.open("GET", "http://127.0.0.1:8080/v1/sc5000/shadow?ver=" + scShadowVer + (scShadowIdle ? "&idle=1" : "") + scLineQuery)
		x.send()
	}
	// The Engine slots whose track is still on its way: stream-sync copies it, or
	// Engine loads it. Until Engine holds it (at.ready) the display renderer covers
	// the slot with the source's title, cover and the progress, as a stock deck
	// shows a track before its waveform; never "No Track Loaded" in between.
	function scShadowCovers(decks) {
		const out = []
		for (const e of decks) {
			const d = Number(e.deck), at = scShadowAt[d]
			if (e.displayOnly === true || e.lineIn || scFourDeckRole.indexOf(String(d)) >= 0 || !(e.path || e.loading)) continue
			if (e.path && at && at.ready && at.path === e.path) continue
			out.push({deck: d, key: e.key || "", title: e.title, artist: e.artist, bpm: e.bpm, pitch: e.pitch, durationMs: e.durationMs,
				loading: true, engine: true, copying: !e.path, progress: e.progress, error: e.error})
		}
		return out
	}
	// What a waveform lane says while it has nothing to draw: 0 nothing, 1 the copy
	// runs, 2 Engine loads the copy, 3 the source still computes the detail waveform,
	// 4-6 the copy failed (stream-sync tries again by itself): no file, no answer in time, other.
	function scShadowBusy(s, meta, detailed) {
		if (!s || s.stale || s.metadataOnly) return 0
		if (s.engine) return !s.copying ? 2 : s.error === "" ? 1 : s.error === "unavailable" ? 4 : s.error === "timeout" ? 5 : 6
		return s.loading || !meta || (meta.detailPending && !detailed) ? 3 : 0
	}
	// Engine mode: what the source deck's header shows and its copy here cannot know. The copy stays
	// out of Sync and the master selection (Engine Link carries both), and it is a bare file: Engine
	// here names it by its file name and analyses a key the source's library may not have. So the
	// stock header items (BpmInfo, JP07SyncIndicator, ArtistSong, KeyInfo) read the source's here. Display only.
	property var scShadowHead: ({})
	function scShadowHeadOf(d) { return scShadowHead[d] || scShadowEmptyObject }
	function scShadowApplyAll(decks, displayOnly) {
		scShadowDisplayOnly = displayOnly !== false
		const head = {}
		if (!scShadowDisplayOnly) for (const d of [1, 2, 3, 4]) {
			if (scFourDeckRole.indexOf(String(d)) >= 0) continue
			const e = decks.find(x => x.deck === d)
			if (e && e.path) head[d] = {master: typeof e.lead === "boolean" ? e.lead : e.master, sync: !!e.sync, title: e.title || "", artist: e.artist || "", keyName: e.keyName || "",
				loop: !!e.loop, keyOffset: e.keyOffset || 0, stems: !!e.stems, slip: e.slip || 0}
			// The last track is still on the deck (the next one's copy on its way, the peer gone): its
			// header stays until it is unloaded. Without it the header read the copy's file name and key.
			else if (scShadowHead[d] && (scShadowAt[d] || {}).path) head[d] = scShadowHead[d]
		}
		if (JSON.stringify(head) !== JSON.stringify(scShadowHead)) scShadowHead = head
		let peerLead = 0, peerPlaying = false
		for (const e of decks) { if (e.shadowMaster && !e.stale) peerLead = e.shadowMaster; if (e.playing && !e.cue && !e.stale) peerPlaying = true }
		scShadowPeerLead = peerLead; scShadowPeerPlaying = peerPlaying
		if (scShadowDisplayOnly) {
			// No Planck reads/writes or legacy helpers here, even when path is empty.
			scShadowCdjMaster = false // display flags must never suppress the real network master
			scShadowLineMask = 0
			scShadowEngineMask = 0
			scShadowApplyDisplay(decks)
			return
		}
		// Per slot: a deck with displayOnly (a CDJ whose file Engine cannot have, or on
		// a Line In channel) goes to the display renderer, the others (EngineX peers,
		// CDJs playing from a stick) through Engine. Both work side by side.
		let mask = 30, lineMask = 0, master = false
		for (const e of decks) {
			if (e.displayOnly === true) mask &= ~(1 << e.deck)
			else {
				if (e.lineIn) lineMask |= 1 << e.deck
				if (e.master) master = true // display flags must never suppress the real network master
			}
		}
		scShadowEngineMask = mask
		scShadowLineMask = lineMask
		root.scShadowCdjMaster = master
		for (const e of decks) if (e.displayOnly !== true) root.scShadowApply(e)
		scShadowApplyDisplay(decks.filter(e => e.displayOnly === true).concat(scShadowCovers(decks))) // the slots Engine holds are cleared there
		const now = Date.now()
		for (const d of [1, 2, 3, 4]) {
			if (scFourDeckRole.indexOf(String(d)) >= 0) continue
			const e = decks.find(x => x.deck === d)
			const at = scShadowAt[d]
			// A slot whose source became a CDJ falls through: scShadowEmpty unloads what Engine still holds.
			// loading: Go is still copying the track, wait for it. Not with the source's
			// previous track on the deck: that one goes now, as when the source ejects.
			if (e && e.displayOnly !== true && (e.path || e.loading) && !(!e.path && e.id && at && at.path && at.id !== e.id)) {
				if (at) at.emptyAt = 0
				continue
			}
			root.scShadowEmpty(d, now)
		}
	}
	// The other player went away (off, rebooting) or ejected its track: stop this
	// shadow deck and, after 350 ms (one dropped update must not eject), unload it
	// so no stale track or waveform stays, handing master back to our own deck.
	function scShadowEmpty(d, now) {
		// A display slot is Engine's business only while our own load is still on it.
		if (scShadowDisplayOnly || (((scShadowEngineMask >> d) & 1) === 0 && !(scShadowAt[d] && scShadowAt[d].path))) return
		const at = scShadowAt[d] || (scShadowAt[d] = {path: ""})
		at.landing = undefined
		// On an all-in-one console (#160), leave Decks 3/4 alone unless a shadow peer loaded them.
		if (!root.scPlayer && !at.path) return
		const pPlay = root.scTr("/Engine/Deck" + d + "/Play")
		if (pPlay && pPlay.state) pPlay.state = false
		if (!at.emptyAt) at.emptyAt = now
		if (now - at.emptyAt < 350 || !(at.path || (root.scTr("/Engine/Deck" + d + "/Track/SongLoaded") || {}).state)) return
		// Standalone players share Sync/master through Engine Link; consoles (#160) need presses.
		if ((!at.link || !root.scPlayer || at.nativeSync) && (root.scTr("/Engine/Deck" + d + "/SyncMode") || {}).string !== "Off") scPress("/Engine/Deck" + d + "/UnSync")
		if ((!at.link || !root.scPlayer) && (root.scTr("/Engine/Deck" + d + "/DeckIsMaster") || {}).state) {
			const m0 = scFourDeckRole.split("")[0]
			if (m0) scPress("/Engine/Deck" + m0 + "/RequestMaster")
		}
		const u1 = root.scTr("/Engine/Deck" + d + "/Unload")
		if (u1) u1.state = true
		const u2 = root.scTr("/Engine/Deck" + d + "/Track/Unload")
		if (u2) u2.state = true
		delete scShadowAt[d]
		if (scShadowHead[d]) { const head = Object.assign({}, scShadowHead); delete head[d]; scShadowHead = head }
	}
	function scShadowApply(e) {
		const d = e.deck
		if (((scShadowEngineMask >> d) & 1) === 0) return
		if (scFourDeckRole.indexOf(String(d)) >= 0 || !e.path) return
		const g = p => root.scTr("/Engine/Deck" + d + p)
		const now = Date.now()
		let at = scShadowAt[d] || {path: ""}
		// The source's key (e.id) says which track this is, not the path alone: the
		// same path can be another file, and a track loaded again is a new load.
		const id = e.id || e.path
		if (at.path !== e.path || at.id !== id) {
			at = scShadowAt[d] = {path: e.path, id: id, tries: 0, seekAt: 0, seeks: 0, loads: at.loads || 0, jumps: e.jumps, nativeSync: !!at.nativeSync}
			root.scShadowLoad(d, at, e, now)
			return
		}
		if (!root.scShadowHolds(d, at, e, now)) return
		at.ready = true
		if (root.scShadowEnd(d, at, e, now)) return
		at.link = e.link || ""
		at.rate = scShadowRate(e)
		// no scShadowLink(): a RemoteState/Register subscription made Engine spam "service has disappeared" and block the Librarian when the other player went off; our UDP push already carries the edges
		scShadowCues(d, at, e, now)
		// Updates sent before an Engine Link edge arrive after it: skip them, or
		// they would undo the edge for a moment.
		if (now - (at.linkAt || 0) < 300 && (e.playing && !e.cue) !== at.linkPS) return
		if (e.cue && at.linkCueOff && now - at.linkCueOff < 300) return
		const local = !!g("/PlayState").state
		// Heartbeats can keep the same sequence. The landing observer still
		// needs their liveness and carried source line on every delivery.
		at.sourceTarget = e.positionMs + (e.playing && e.at ? Math.max(0, now - e.at) * at.rate : 0)
		at.sourceAt = now; at.sourcePlaying = !!e.playing && !e.stale
		if (!at.sourcePlaying || !e.sync || !e.link || !root.scPlayer || at.nativeSync || (e.sync && e.pitch > 100)) at.landing = undefined
		if (e.seq && e.seq === at.lastSeq && e.playing === local && !at.wantMaster && !at.check && !at.nativeSync) return
		at.lastSeq = e.seq
		const wasPreview = at.preview
		at.preview = !!e.cue
		if (!e.playing) at.pausedAt = e.positionMs
		// The CDJ took master: so does its shadow deck, and Engine drops it on ours
		// (one master). Only after the change (for 5 s), so pressing MASTER here
		// afterwards wins it back.
		// Standalone players share master through Engine Link; consoles (#160) need the shadow master.
		if ((!e.link || !root.scPlayer) && e.master && !at.master) at.wantMaster = now + 5000
		if (!e.master || (e.link && root.scPlayer)) at.wantMaster = 0
		at.master = !!e.master
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		if (!(sr > 0 && len > 0)) return
		const syncEdge = !!e.sync && at.sourceSync === false
		const tempoStep = at.sourceRate !== undefined && Math.abs(at.rate - at.sourceRate) > 2e-4
		at.sourceSync = !!e.sync
		at.sourceRate = at.rate
		const seek = ms => root.scShadowSeek(d, at, ms)
		// e.at is when Go extrapolated e.positionMs; this runs later on a busy GUI
		// thread, so carry the target to now before comparing.
		const target = e.positionMs + (e.playing && e.at ? Math.max(0, now - e.at) * scShadowRate(e) : 0)
		const posMs = g("/Track/TrackPosition").unnormalized / sr * 1000
		const drift = target - posMs // > 0: behind
		const meanReady = at.env !== undefined && now - at.envT0 >= 300
		const ownLine = meanReady ? at.env + at.envRate * (now - at.envT0 + 18) : posMs
		const phaseDrift = target - ownLine
		at.drift = drift
		const pSync = g("/SyncMode")
		const synced = !!pSync && pSync.string !== "" && pSync.string !== "Off"
		// CDJs and consoles (#160) need Engine Sync here. A standalone peer's synced deck
		// sends its exact rate (CurrentBPM), which the fader and the loop below follow.
		// The fader's native input is bounded to 0..1 (at most +100%).
		// Native Sync has its own rate modifier: use it for a linked source past
		// that stop. Keep ownership until its Sync is released, including a new
		// track. UnSync retains the native rate until the source moves its fader.
		if (e.link && root.scPlayer && e.sync && e.pitch > 100) at.nativeSync = true
		const wantSync = !!e.sync && (!e.link || !root.scPlayer || at.nativeSync)
		const phaseFollower = !!e.link && root.scPlayer && !at.nativeSync && !!e.sync
		if (at.nativeSync && !e.sync && !synced && e.pitch <= 100) at.nativeSync = false
		const dbg = act => { if (root.scShadowDebug) root.scShadowDbg({deck: d, act: act, posMs: Math.round(posMs), target: Math.round(target), drift: Math.round(drift), sd: Math.round(at.sd || 0), pitch: e.pitch, pll: at.pll || 0, range: g("/SpeedRange").string, sync: !!e.sync, synced: synced, bpm: e.bpm, currentBpm: e.currentBpm, beat: e.beat, jumps: e.jumps, seeks: at.seeks || 0, master: !!g("/DeckIsMaster").state, link: !!e.link}) }
		if (!e.playing) at.pll = 0
		// e.pitch is rounded to 0.01 %: at 138 BPM that alone slipped 4 ms a minute.
		// The fader goes to the source's tempo ratio, so the BPM shown is the source's.
		const exact = e.bpm > 0 && e.currentBpm > 0 ? (e.currentBpm / e.bpm - 1) * 100 : NaN
		// Native Sync can pull the source's phase in after its tempo changed.
		// Watch its posted line through a bounded transition, without confusing our own raw
		// decoder position with another source change. Heartbeats stay on line.
		const sourceDelta = target - at.phaseLine - (now - at.phaseAt) * at.phaseRate
		const phaseStep = now < (at.phaseUntil || 0) && Math.abs(sourceDelta) > 20 * at.rate
		// A separate large cue/jump remains an immediate transport command,
		// including after this transition spent its compensation budget.
		const newJump = e.jumps !== at.jumps && Math.abs(sourceDelta) > 180 * Math.max(1, at.rate)
		if (syncEdge || tempoStep || phaseStep) {
			if (syncEdge || tempoStep) {
				if (phaseFollower && (syncEdge || now >= (at.phaseHardUntil || 0))) {
					at.phaseHardUntil = now + 4000; at.transitionSeeks = 0
				}
				at.phaseUntil = phaseFollower ? Math.min(at.phaseHardUntil, now + 1000) : now + 1000
				if (at.landing && Math.abs(at.landing.rate - at.rate) > 2e-4) at.landing = undefined
			} else if (phaseFollower) at.phaseUntil = Math.min(at.phaseHardUntil, Math.max(at.phaseUntil, now + 500))
			at.phaseStableAt = now
			at.phaseLine = target; at.phaseAt = now; at.phaseRate = at.rate
			if (scShadowBent && scShadowBent.at === at) scShadowBendEnd()
			at.fine = undefined
		}
		if (newJump) {
			// Cues and loop wraps keep their original one-seek transport path.
			// They do not replenish or inherit Sync's compensation budget.
			at.landing = undefined; at.phaseHardUntil = 0; at.phaseUntil = 0; at.transitionSeeks = 0
			at.phaseLine = target; at.phaseAt = now; at.phaseRate = at.rate
		}
		if (!(wantSync && synced) && !(at.nativeSync && e.pitch > 100)) scShadowSpeed(d, Math.abs(exact - e.pitch) < 0.02 ? exact : e.pitch, e.range)
		const pKey = g("/Track/KeyLock")
		if (typeof e.keylock === "boolean" && pKey && !!pKey.state !== e.keylock) pKey.state = e.keylock
		// Play, CUE held, pause, CUE released: on the first update that shows it.
		// at.cmd only stops rewriting the same state while Engine follows (a frame
		// or two); a change of the source's state always goes through.
		if (e.playing !== local) {
			at.landing = undefined
			if (scShadowBent && scShadowBent.at === at) scShadowBendEnd()
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
				at.sd = undefined
				dbg(e.playing ? "play" : "pause")
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
		// Standalone players with an EngineX peer (e.link) share Engine Link sync natively,
		// and pressing Sync/UnSync on a shadow deck fought the stock SYNC and MASTER buttons.
		if ((!e.link || !root.scPlayer || at.nativeSync) && wantSync !== synced && now >= (at.syncAt || 0)) {
			scPress("/Engine/Deck" + d + (wantSync ? "/Sync" : "/UnSync"))
			at.syncAt = now + 800
		}
		if (!e.playing) {
			at.landing = undefined; at.phaseUntil = 0
			// Paused: seek once per source position (the cue point moved, jog
			// search) and check the landing once. It is off by a decoder frame, or
			// a beat with Quantize, and seeking again every tick replayed it.
			at.jumps = e.jumps
			const fresh = at.target === undefined || at.target === null || Math.abs(e.positionMs - at.target) > 5
			const check = at.check && now >= at.check
			at.sd = undefined
			if ((fresh || check) && Math.abs(drift) > 10 && now - at.seekAt >= 40) {
				seek(e.positionMs)
				at.target = e.positionMs
				at.check = fresh ? now + 60 : 0
				dbg("seek")
			} else if (check) at.check = 0
			scShadowBeat(d, at, e, drift, now)
			return
		}
		at.target = null
		// Playing: a seek flushes Engine's decoder and waveform (the visible snap), so
		// it is for source discontinuities, or a persistent gap past lim.
		// at.wide: two such seeks within 10 s did not land (Quantize snaps to a
		// beat). Steady drift below that is closed by the fader instead (at.pll).
		// A linked copy still follows the source's song position: native beat Sync
		// can choose another beat. Seeking drops it to native tempo Sync.
		const lim = wantSync && synced && !at.nativeSync ? 2000 : at.wide ? 800 : 300
		const jumpMs = e.link && root.scPlayer && e.sync ? 20 * Math.max(1, at.rate) : 60
		const jump = e.jumps !== at.jumps && Math.abs(drift) > jumpMs
		// A Sync edge or tempo step is a new phase/rate, not clock drift. Apply
		// it on this update, with the speed already set above. Keep small audio
		// block noise on the existing bend path and never snap on a heartbeat.
		const snap = (!wantSync || at.nativeSync) && (syncEdge || tempoStep || phaseStep) &&
			Math.abs(phaseFollower && meanReady ? phaseDrift : drift) > (phaseFollower && meanReady ? 35 : 20) * at.rate
		if (!jump) at.jumps = e.jumps
		at.over = Math.abs(drift) > lim ? (at.over || 0) + 1 : 0
		if (snap || (jump && now - at.seekAt >= 80) || (at.over >= 3 && now - at.seekAt >= 1000)) {
			// Coalesce new source positions while a native seek is outstanding.
			// A seek freezes the playing transport before it resumes; another
			// request would repeat that loss, not correct its landing.
			const observe = phaseFollower && !!at.line && now < (at.phaseHardUntil || 0)
			if (observe && (at.landing || (at.transitionSeeks || 0) >= 2)) {
				at.jumps = e.jumps; at.over = 0
				return dbg(at.landing ? "landing" : "transition")
			}
			if (!jump && !snap) {
				at.wide = at.wide || now - (at.lastSteady || 0) < 10000
				at.lastSteady = now
			}
			if (scShadowBent && scShadowBent.at === at) scShadowBendEnd()
			if (observe && (snap || jump)) root.scShadowLandingSeek(d, at, target, ownLine, now, 0, meanReady)
			else seek(target)
			at.jumps = e.jumps
			at.over = 0
			at.seeks = (at.seeks || 0) + 1
			at.pll = 0
			at.sd = undefined
			dbg(snap ? "snap" : jump ? "jump" : "seek")
			return
		}
		// Never steer /Engine/DeckN/Speed with a synthetic PLL: changing Speed alters
		// the displayed CurrentBPM on screen ("BPM moving back and forth") and forces
		// Engine's Elastique time-stretcher to re-tune every few seconds (3-5 s stutter).
		// Both SC5000 decks run on 20 ppm crystals at the exact unrounded tempo ratio.
		// What is left (a seek lands tens of ms off, the source was nudged or pulled
		// in by its Sync, the two clocks) was never closed below lim: 105 ms ahead on
		// one player, 38 ms behind on the other, under a green phase meter. It is
		// closed the way a DJ does, with Engine's PITCH BEND (scShadowBend), measured
		// against the deck's own line (scShadowLine). The source posts its newest
		// audio block, 18 ms of wall time ahead of the mean of its raw positions
		// (measured), and that line still moves +-6 ms: the gap is averaged over
		// 8 s (a step past 15 ms at once) and, while that average is under 10 s old,
		// only bent past 12 ms. At 6 ms on a 2 s old line the deck was bent 55 times
		// in six minutes, to and fro.
		if (wantSync && synced && !at.nativeSync) return dbg("sync")
		if (at.landing) return dbg("landing")
		if (now - at.seekAt < 300) return dbg("settle")
		at.sd = at.sd === undefined ? drift : at.sd * 0.85 + drift * 0.15
		at.pll = 0
		if (scShadowBent && scShadowBent.at === at) return dbg("bending")
		const age = at.env !== undefined ? now - at.envT0 : 0
		// A gap past 40 ms (a play start: the source's first line is 80-120 ms off the
		// one it settles on) is no noise: bent after 1 s, not left for 2 s more.
		const gap = age >= 1000 ? target - at.env - at.envRate * (now - at.envT0 + 18) : 0
		if (age >= 2000 || Math.abs(gap) > 40) {
			at.fine = at.fine === undefined || Math.abs(gap - at.fine) > 15 ? gap : at.fine + (gap - at.fine) * Math.min(1, (now - at.fineAt) / 8000)
			at.fineAt = now
			// 12 and 6 ms on screen and in the air: of the deck's own time at its rate
			// (at -50 % the copy stood 13, then 23 ms off the grid for 10 s, rec14-a).
			if (Math.abs(at.fine) > (age < 10000 ? 12 : 6) * at.rate && Math.abs(at.fine) <= lim) {
				scShadowBend(d, at, at.fine)
				return dbg("bend")
			}
		}
		dbg("steady")
	}
	// Seek loss is measured against the issued command, never against a source
	// that may still be pulling phase. Only one compensated correction is allowed.
	function scShadowLandingSeek(d, at, target, oldLine, now, loss, oldWarm) {
		const requested = target + loss
		const issued = root.scShadowSeek(d, at, requested)
		if (!isFinite(issued)) return
		at.transitionSeeks = (at.transitionSeeks || 0) + 1
		at.landing = {at: at.seekAt, target: issued, rate: at.rate, old: oldLine, uncertain: !oldWarm,
			lastAt: now, lastPos: oldLine, first: 0, values: [], learned: false}
	}
	function scShadowLandingTick(now) {
		for (const d in scShadowAt) {
			const at = scShadowAt[d], b = at.landing, l = at.line
			if (!b || !l) continue
			if (scShadowDisplayOnly || !root.scPlayer || !at.sourcePlaying || !at.link || at.nativeSync ||
				!l.play.state || (l.loaded && !l.loaded.state) || !(b.rate > 0) || !(l.sr.value > 0) ||
				((scShadowEngineMask >> d) & 1) === 0 || scFourDeckRole.indexOf(String(d)) >= 0 ||
				now - at.sourceAt > 1500 || (!b.learned && now - b.at > 1500) || now >= at.phaseHardUntil ||
				Math.abs(at.rate - b.rate) > 2e-4 || (scShadowBent && scShadowBent.at === at)) {
				at.landing = undefined; continue
			}
			const pos = l.pos.unnormalized / l.sr.value * 1000, dt = now - b.lastAt
			const progress = pos - b.lastPos
			b.lastAt = now; b.lastPos = pos
			if (now - b.at < 60 || dt <= 0 || progress <= dt * b.rate * 0.25 || Math.abs(pos - b.target) < 3 * b.rate) continue
			const sample = pos - (now - b.at) * b.rate
			// An old advancing line is not proof of landing. Reject its whole
			// audio-block uncertainty band, including the occasional high read.
			if (sample >= b.old - (b.uncertain ? 50 : 40) * b.rate && sample <= b.old + (b.uncertain ? 50 : 20) * b.rate) continue
			if (!b.first) b.first = now
			b.values.push(sample); if (b.values.length > 4) b.values.shift()
			if (b.values.length < 3 || now - b.first < 80) continue
			const sorted = b.values.slice().sort((a, c) => a - c)
			// Several samples must describe one new playback line; a seek's
			// discontinuity or a still-frozen decoder is not a loss estimate.
			if (sorted[sorted.length - 1] - sorted[0] > 40 * b.rate) continue
			// Use the follower's mean-line convention (+18 ms of audio-block
			// lag), rather than learning ordinary raw-read lag as seek loss.
			// The middle pair rejects one stale low or ahead-of-line sample.
			const mid = Math.floor(sorted.length / 2)
			const line = (sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2) + 18 * b.rate
			const loss = b.target - line
			if (!(loss >= 0 && loss <= 500 * b.rate)) { at.landing = undefined; continue }
			if (!b.learned) {
				b.learned = true; at.env = undefined; at.fine = undefined
				at.seekLoss = loss; at.seekLossAt = now
				if (root.scShadowDebug) root.scShadowDbg({deck: Number(d), act: "landed", lossMs: loss / b.rate, samples: b.values.length})
			}
			if (now - at.phaseStableAt < 350) continue
			const source = at.sourceTarget + (now - at.sourceAt) * at.rate
			const observed = line + (now - b.at) * b.rate
			if (at.transitionSeeks >= 2) {
				at.landing = undefined; continue
			}
			// A first landing can temporarily coincide with the moving source.
			// Keep observing through this transition: a later phase update uses
			// the same correction budget and cannot become a second bare seek.
			if (Math.abs(source - observed) <= 20 * at.rate) continue
			root.scShadowLandingSeek(Number(d), at, source, observed, now, loss, true)
			at.seeks = (at.seeks || 0) + 1
			if (root.scShadowDebug) root.scShadowDbg({deck: Number(d), act: "trim", lossMs: loss / at.rate, target: source + loss})
		}
	}
	// The shadow deck's own line: the mean of position - rate * time over 5 s, in
	// at.env at the rate frozen in at.envRate since at.envT0. Not the upper envelope
	// the sender posts: that rides on the one raw position in sixty that reads
	// 13 ms ahead, so for seconds after every new line (a bend, a seek) it read
	// 4-10 ms low and the deck was bent forward, then back when the next one came.
	function scShadowLine(now) {
		if (scShadowDisplayOnly) return
		for (const d in scShadowAt) {
			const at = scShadowAt[d]
			if (!at.ready) continue
			const l = at.line || (at.line = {pos: root.scTr("/Engine/Deck" + d + "/Track/TrackPosition"), play: root.scTr("/Engine/Deck" + d + "/PlayState"), sr: root.scTr("/Engine/Deck" + d + "/Track/SampleRate"), loaded: root.scTr("/Engine/Deck" + d + "/Track/SongLoaded")})
			if (!l.play.state || !(at.rate > 0) || !(l.sr.value > 0)) { at.env = undefined; continue }
			// 60 ms off the line is no block of audio (22 ms at most) but a seek that
			// landed after the line began: averaged in, a start at 41 s read 800 ms.
			const pos = l.pos.unnormalized / l.sr.value * 1000
			const fresh = at.env === undefined || Math.abs(at.rate - at.envRate) > 2e-4 || Math.abs(pos - at.envRate * (now - at.envT0) - at.env) > 60
			if (fresh) { at.envT0 = now; at.envRate = at.rate; at.envN = 1; at.fine = undefined }
			const raw = pos - at.envRate * (now - at.envT0)
			at.env = fresh ? raw : at.env + (raw - at.env) * Math.max(1 / ++at.envN, (now - at.envAt) / 5000)
			at.envAt = now
		}
	}
	// Engine's pitch bend (the PITCH BEND buttons) runs a deck 7 % fast or slow
	// while held (measured on a JP07: 27 ms in 400 ms; 7 % of its own speed: at
	// -50 % a 1.36 s bend moved it 51 ms, not 95). No seek, which flushes the
	// decoder and snaps the waveform, and no Speed change: the BPM shown stays the
	// source's. One deck at a time, 80 % of the gap, 1.5 s at most; the line is
	// measured anew after it.
	property var scShadowBent: null
	Timer { id: scShadowBendTimer; onTriggered: root.scShadowBendEnd() }
	function scShadowBend(d, at, ms) {
		if (scShadowBent) return
		const p = root.scTr("/Engine/Deck" + d + "/Track/Nudge" + (ms > 0 ? "Forward" : "Backward"))
		if (!p) return
		p.state = true
		scShadowBent = {p: p, at: at}
		scShadowBendTimer.interval = Math.min(1500, Math.abs(ms) * 0.8 / (0.07 * (at.rate > 0 ? at.rate : 1)))
		scShadowBendTimer.restart()
	}
	function scShadowBendEnd() {
		const b = scShadowBent
		if (!b) return
		scShadowBent = null
		scShadowBendTimer.stop()
		b.p.state = false
		b.at.env = undefined
	}
	// Asks Engine for the source's track on shadow deck d. Engine answers nothing,
	// so scShadowHolds reads back what the deck holds and asks again.
	function scShadowLoad(d, at, e, now) {
		if (((scShadowEngineMask >> d) & 1) === 0) return
		// Lock Playing Deck refuses a load onto a playing deck, and no other track
		// may play under the source's name: stop first, and ask again soon.
		const ps = root.scTr("/Engine/Deck" + d + "/PlayState"), play = root.scTr("/Engine/Deck" + d + "/Play")
		const playing = !!(ps && ps.state)
		if (playing && play) play.state = false
		root.scTr("/Client/Librarian/AddToDatabaseAndLoadToDeck" + d).url = "file://" + encodeURI(e.path)
		at.loads = (at.loads || 0) + 1
		at.tries = (at.tries || 0) + 1
		at.askedAt = now
		at.loadUntil = now + 1200
		at.retryAt = now + (playing ? 1500 : Math.min(30000, 3000 << Math.min(at.tries - 1, 4)))
		at.held = false
		at.ready = false
		if (root.scShadowDebug) root.scShadowDbg({deck: d, act: "load", path: e.path, id: at.id, loads: at.loads, tries: at.tries, why: at.why || ""})
	}
	// Why Engine's track on shadow deck d is not the source's (e), "" when it is.
	// Engine changes a deck's track without us: Continue loads the next one when a
	// deck plays out, a load can be refused or fail, two quick loads can finish in
	// the other order. The file Engine names decides; when Engine names none or
	// another, the track's length does, then title and BPM together. A title or
	// BPM alone does not: Engine's analysis of a copy, a CDJ's rekordbox title and
	// half or double tempo differ on the same audio.
	function scShadowWrong(d, e) {
		const g = p => root.scTr("/Engine/Deck" + d + p) || scShadowEmptyObject
		if (!g("/Track/SongLoaded").state) return "empty"
		const nfc = v => v.normalize ? v.normalize("NFC") : v
		// Engine names the file by volume ("net://<id>/<volume>/<path>"): compare below the mount point.
		const parts = String(e.path).split("/").filter(v => v !== "")
		const tail = nfc("/" + parts.slice(Math.max(Math.min(2, parts.length - 1), parts.length - 3)).join("/"))
		const name = g("/Track/TrackNetworkPath").string || g("/Track/TrackUri").string || ""
		let plain = name
		try { plain = decodeURIComponent(name) } catch (err) {}
		const file = name === "" ? 0 : nfc(name).endsWith(tail) || nfc(plain).endsWith(tail) ? 1 : -1
		if (file > 0) return ""
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		const length = e.durationMs > 0 && sr > 0 && len > 0 ? (Math.abs(len / sr * 1000 - e.durationMs) <= 2000 ? 1 : -1) : 0
		const heldTitle = g("/Track/SongName").string || g("/Track/TrackName").string || ""
		const title = e.title && heldTitle ? (e.title === heldTitle ? 1 : -1) : 0
		const ob = g("/OriginalBPM").unnormalized
		const ratio = e.bpm > 0 && ob > 0 ? e.bpm / ob : 0
		const bpm = ratio > 0 ? ([1, 2, 0.5].some(k => Math.abs(ratio / k - 1) <= 0.03) ? 1 : -1) : 0
		if (length < 0) return "length"
		// Another name: the same audio only when the length agrees and the title does not differ.
		if (file < 0 && !(length > 0 && title >= 0)) return "file"
		if (title < 0 && bpm < 0) return "title"
		return ""
	}
	// True once Engine holds the source's track on shadow deck d; only then is the
	// deck steered. Checked again when the track's length changes and every second
	// (strings), so a track Engine swapped in later is found and loaded again.
	function scShadowHolds(d, at, e, now) {
		if (((scShadowEngineMask >> d) & 1) === 0 || now < at.loadUntil) return false
		const len = (root.scTr("/Engine/Deck" + d + "/Track/TrackLength") || scShadowEmptyObject).unnormalized
		if (at.held && len === at.len && now - at.heldAt < 1000) return true
		// Engine is still loading (a long file, a slow drive): asking again would start it over.
		if ((root.scTr("/Engine/Deck" + d + "/Track/SongLoading") || scShadowEmptyObject).state && now - at.askedAt < 60000) {
			at.ready = false
			return false
		}
		const why = root.scShadowWrong(d, e)
		if (why === "") {
			at.held = true; at.heldAt = now; at.len = len; at.tries = 0; at.why = ""
			return true
		}
		const ps = root.scTr("/Engine/Deck" + d + "/PlayState"), play = root.scTr("/Engine/Deck" + d + "/Play")
		if (why !== "empty" && ps && ps.state && play) play.state = false
		// It was ours until now (Engine swapped it): load at once. Else Engine may
		// still be loading: ask again after 3, 6, 12, 24, then every 30 s.
		if (at.held || now >= at.retryAt) {
			at.why = why
			root.scShadowLoad(d, at, e, now)
		}
		at.ready = false
		return false
	}
	// Continue (Engine's AutoPlay) loads and plays the next track when a deck plays
	// out, on a shadow deck too; a seek to the source's end position then ended
	// that one as well, and so on through the list. With Continue on, a shadow
	// deck stops 700 ms short of its end. True while it is parked there.
	function scShadowEndGap() {
		const p = root.scTr("/Client/Preferences/Profile/Application/AutoPlay")
		return p && p.state ? 700 : 0
	}
	function scShadowEnd(d, at, e, now) {
		const gap = root.scShadowEndGap()
		if (!gap) return false
		const g = p => root.scTr("/Engine/Deck" + d + p) || scShadowEmptyObject
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		if (!(sr > 0 && len > 0)) return false
		const endMs = len / sr * 1000 - gap
		const target = e.positionMs + (e.playing && e.at ? Math.max(0, now - e.at) * scShadowRate(e) : 0)
		const posMs = g("/Track/TrackPosition").unnormalized / sr * 1000
		const local = !!g("/PlayState").state
		if (target < endMs && !(local && posMs >= endMs)) return false
		if (local) g("/Play").state = false
		if (target < endMs) return false // ran ahead of the source: stopped, and scShadowApply lines it up again
		if (Math.abs(posMs - endMs) > 50 && now - at.seekAt >= 200) root.scShadowSeek(d, at, endMs)
		return true
	}
	// The shadow deck's fader at pitch percent, in the source's SpeedRange (want:
	// one of Engine's own entries) so the header reads the same. No range sent (a
	// CDJ) or a pitch beyond it: the range (8 % by default) is widened first, or
	// +11.1 % (a synced 126 BPM track at 140) stuck at +8 %.
	function scShadowSpeed(d, pitch, want) {
		if (((scShadowEngineMask >> d) & 1) === 0) return
		const pRange = root.scTr("/Engine/Deck" + d + "/SpeedRange"), pSpeed = root.scTr("/Engine/Deck" + d + "/Speed")
		if (!pRange || !pSpeed) return
		let range = parseFloat(pRange.string) || 8
		if (pRange.entries) {
			let i = want > 0 && Math.abs(pitch) <= want ? pRange.entries.findIndex(v => parseFloat(v) === want) : -1
			if (i < 0 && Math.abs(pitch) > range - 0.2) {
				i = pRange.entries.findIndex(v => parseFloat(v) >= Math.abs(pitch) + 0.2)
				if (i < 0) i = pRange.entries.length - 1
			}
			if (i >= 0 && parseFloat(pRange.entries[i]) !== range) {
				pRange.index = i
				range = parseFloat(pRange.string) || range
			}
		}
		const speed = Math.max(0, Math.min(1, 0.5 + pitch / (2 * range)))
		if (Math.abs(pSpeed.value - speed) * 2 * range > 0.002) pSpeed.value = speed
	}
	// Debug telemetry (debug_telemetry=1 or /tmp/sc5000-debug, told by GET /shadow):
	// the receiver's decisions, posted in batches to Go's ring and log.
	property bool scShadowDebug: false
	property var scShadowDbgBuf: []
	property real scShadowDbgAt: 0
	function scShadowDbg(o) {
		o.t = Date.now()
		if (scShadowDbgBuf.length < 400) scShadowDbgBuf.push(o)
	}
	function scShadowDbgFlush(now) {
		if (!scShadowDbgBuf.length || now - scShadowDbgAt < 500) return
		scShadowDbgAt = now
		const x = new XMLHttpRequest()
		x.open("POST", "http://127.0.0.1:8080/v1/sc5000/debug/shadow")
		x.send(JSON.stringify(scShadowDbgBuf))
		scShadowDbgBuf = []
	}
	// The source's hot cues and CUE point (track samples), set through Engine's
	// own deck properties: Engine shows them on the waveform, the overview and
	// the pads, and stores them itself. Nothing else may write its m.db (it
	// holds it with an exclusive lock and its own page cache; a write broke it).
	function scShadowCues(d, at, e, now) {
		if (((scShadowEngineMask >> d) & 1) === 0) return
		if (!Array.isArray(e.cues)) return // a CDJ: no cues sent
		const sig = JSON.stringify(e.cues) + "/" + e.cuePos
		if (sig === at.cueSig && (at.cuePasses || 0) >= 2) return
		if (now < (at.cueAt || 0) + (sig === at.cueSig ? 1200 : 250)) return
		at.cueAt = now
		at.cuePasses = sig === at.cueSig ? (at.cuePasses || 0) + 1 : 1
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
		if (((scShadowEngineMask >> d) & 1) === 0) return
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
		at.landing = undefined
		if (((scShadowEngineMask >> d) & 1) === 0) return
		const g = p => root.scTr("/Engine/Deck" + d + p)
		const sr = g("/Track/SampleRate").value, len = g("/Track/TrackLength").unnormalized
		if (!(sr > 0 && len > 0)) return
		const lenMs = len / sr * 1000
		const target = Math.max(0, Math.min(ms, lenMs - root.scShadowEndGap()))
		g("/Track/SetSongPosition").value = target / lenMs
		at.seekAt = Date.now()
		at.env = undefined
		return target
	}
	// Engine Link already mirrors the other Engine's properties here, within a
	// few ms of that Engine changing them (measured: our own update arrives
	// 60-130 ms later, two GUI-thread XHR hops). So play/stop and the CUE LED
	// come from Engine Link; positions, jumps and pitch still from our updates.
	// Register takes one path per event-loop turn: one path per update.
	property var scShadowLinks: ({})
	function scShadowLink(d, uuid) {
		if (((scShadowEngineMask >> d) & 1) === 0) return
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
		if (((scShadowEngineMask >> d) & 1) === 0) return
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
			message: root.scT("Restart Engine to apply? Playback will stop."),
			options: Prompt.Cancel | Prompt.Ok
		})
		prompt.finished.connect(function(r) { if (r === Prompt.AcceptRole) scCall(query + "&restart=1") })
	}
	property string scUpdateRepo: "Cd1s/EngineX-OS"
	// #79: only GET /v1/sc5000/ui (loopback) carries these; shown in Settings -> About.
	property string scConsoleUrl: ""
	property string scAccessCode: ""
	// #148: home enginex-stemsd / remote Stems status from stems-lan.json.
	property var scLanStemsInfo: null
	property string scLanStems: ""
	function scLanStemsLine(st, lang) {
		void lang
		const host = st && st.online ? String(st.hostname || st.endpoint || "") : ""
		if (host) {
			const backend = String(st.backend || "htdemucs")
			return scT("High-quality stems:") + (scLang === "en" ? " " : "") + host + " (" + backend + ")"
		}
		return scT("On-deck stems (basic quality). For high-quality stems, run the EngineX Stems service on a computer on the same network.")
	}
	property string scUpdateText: ""
	property bool scBroadcastOn: false
	property bool scUpfaderEcho: false
	property string scLastBroadcastTrack: ""
	property bool scDualColumn: false
	property bool scTracklistWaveform: true
	property bool scCueCountdown: true
	property int scSyncGuardPct: 0
	property bool scSwapLoopOrder: false
	property int scLedBrightness: 100
	property int scUIScale: 100
	property bool scStemsEqMode: false
	property bool scStemsEqApplied: false
	onScHasStemsEqChanged: {
		if (!scHasStemsEq && scStemsEqApplied) {
			scStemsEqApplied = false
			for (let ch = 1; ch <= 4; ch++) {
				const m = root.scTr("/Engine/Mixer/Channel" + ch + "/ChannelStrip/Mode")
				if (m) m.index = 0
			}
		}
	}
	onScStemsEqModeChanged: {
		if (!scHasStemsEq || (!scUiLoaded && !scStemsEqMode)) return
		scStemsEqApplied = true
		for (let ch = 1; ch <= 4; ch++) {
			const m = root.scTr("/Engine/Mixer/Channel" + ch + "/ChannelStrip/Mode")
			if (m) m.index = scStemsEqMode ? 1 : 0
		}
	}
	property bool scBeatJumpPads: false
	property bool scMixstreamSweepOpen: false
	property string scPioneerFx: "off"
	property string scFxTimeMode: "beats"
	property int scFxTimeMs: 500
	property bool scFxSelectorAvailable: false
	property bool scFxMasterBypassed: false
	readonly property bool scHasPxDelay: scFxSelectorAvailable || (!scFxMasterBypassed && ["spiral", "pitch_echo", "dub_echo"].indexOf(scPioneerFx) >= 0)
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
	// On a 4-deck unit this deck's own pair (12 / 24) wins; root.decks is undefined in Engine 5.3.0.
	function scDeckList() {
		if (root.scFourDeckRole !== "") return root.scFourDeckRole.split("").map(c => parseInt(c)).filter(d => d >= 1 && d <= 4)
		return (root.decks && root.decks.length ? root.decks : [1, 2]).map(d => parseInt(d))
	}
	// #49: when isPlayer is cleared for the 4-deck layout, Engine binds /Engine/Deck1..4/Color
	// to PlayerColor1..4 (which default to white #ffffff on 1/2 and black #000000 on 3/4 on JP07)
	// instead of PlayerColor{N}{A,B}. Mirror the player's Layer A/B colours into the 4 deck slots.
	function scPlayerColorOf(tag) {
		const p = Planck.getProperty("/Client/Preferences/Profile/Application/PlayerColor" + tag)
		return p && p.translator ? p.translator.color : "transparent"
	}
	readonly property bool scPlayerColorsSynced: {
		if (!Config.device.isPlayer && root.scIsPlayerBoard && root.scFourDeckRole !== "") {
			const pp = Planck.getProperty("/Client/Preferences/Player")
			const pn = (pp && pp.translator && parseInt(pp.translator.string)) || 1
			const role = root.scFourDeckRole || (pn === 2 ? "24" : "13")
			const tags = {1: "1A", 2: "2A", 3: "1B", 4: "2B"}
			const dA = parseInt(role[0])
			const dB = role.length > 1 ? parseInt(role[1]) : 0
			if (dA >= 1 && dA <= 4) tags[dA] = pn + "A"
			if (dB >= 1 && dB <= 4) tags[dB] = pn + "B"
			for (let d = 1; d <= 4; d++) {
				const c = scPlayerColorOf(tags[d])
				const dp = Planck.getProperty("/Client/Preferences/Profile/Application/PlayerColor" + d)
				if (dp && dp.translator && dp.translator.color !== c) dp.translator.color = c
				const dc = Planck.getProperty("/Engine/Deck" + d + "/Color")
				if (dc && dc.translator && dc.translator.color !== c) dc.translator.color = c
			}
		}
		return true
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
	// Keep inactive dictionaries as scalar JSON strings. Only the selected language
	// becomes a JS object/GC root; retain all spellings for live language switching.
	readonly property var scDicts: (@DICTS@)
	readonly property var scDict: JSON.parse(scDicts[scLang] || scDicts.en)
	function scT(s) { return scDict[s] || s }
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
	// Deck art for a streamed track: Engine hands the 7" deck / MicroDeck "" or image://streaming/, so ask the gateway by title+artist.
	// Native song-name bindings invalidate on track changes; retry only while metadata is missing.
	function scDeckArtUrl(deck, path, tick, metadata) {
		const p = String(path || "")
		if (p !== "" && p !== "image://streaming/") return p
		const t = k => (root.scTr("/Engine/Deck" + deck + "/Track/" + k) || {})
		if (!t("SongLoaded").state) return p
		const title = scPlainTitle(t("SongName").string || "")
		if (title === "") return p
		return "http://127.0.0.1:8080/v1/sc5000/art?song=" + encodeURIComponent(title) + "&artist=" + encodeURIComponent(t("ArtistName").string || "")
	}
	function scDeckArtPending(deck, path, url) {
		if (path !== "" && path !== "image://streaming/") return false
		const loaded = root.scTr("/Engine/Deck" + deck + "/Track/SongLoaded")
		return !!loaded && loaded.state && String(url).indexOf("http://127.0.0.1:8080/v1/sc5000/art?") !== 0
	}
	property int scMidiTargetDeck: 3
	property int scMidiLastAckSeq: 0
	property real scMidiLastApplyMs: 0
	property real scMidiSendAt: 0
	property bool scMidiActive: false
	property int scMidiFailCount: 0
	property var scMidiXhr: null
	function scMidiInitBridge() {
		if (!Math.scMidiBridge || !Math.scMidiBridge.installed) {
			Math.scMidiBridge = {
				installed: true,
				enabled: false,
				connected: false,
				helperOnline: false,
				lastError: "",
				wake: function() {
					root.scMidiFailCount = 0
					Math.scMidiBridge.enabled = true
					scMidiPollTimer.interval = 16
					scMidiPollTimer.restart()
				},
				stop: function() {
					root.scMidiActive = false
					Math.scMidiBridge.enabled = false
					scMidiPollTimer.stop()
				}
			}
		}
		const probe = new XMLHttpRequest()
		probe.onreadystatechange = function() {
			if (probe.readyState !== 4) return
			if (probe.status === 200) {
				try {
					const st = JSON.parse(probe.responseText)
					Math.scMidiBridge.helperOnline = true
					Math.scMidiBridge.connected = !!st.connected
					Math.scMidiBridge.enabled = !!st.enabled
					Math.scMidiBridge.lastError = st.error ? String(st.error) : ""
					if (st.targetDeck) root.scMidiTargetDeck = st.targetDeck
					if (st.enabled) Math.scMidiBridge.wake()
					return
				} catch (e) {}
			}
			Math.scMidiBridge.helperOnline = false
		}
		probe.open("GET", "http://127.0.0.1:17838/status")
		probe.send("")
	}
	function scMidiReadDeck(d) {
		const b = "/Engine/Deck" + d + "/"
		const tLoaded = root.scTr(b + "Track/SongLoaded"), tPlay = root.scTr(b + "PlayState")
		const tPos = root.scTr(b + "Track/TrackPosition"), tLen = root.scTr(b + "Track/TrackLength"), tSr = root.scTr(b + "Track/SampleRate")
		const tSpd = root.scTr(b + "Speed"), tRng = root.scTr(b + "SpeedRange"), tKey = root.scTr(b + "Track/KeyLock")
		const tSync = root.scTr(b + "Sync"), tLoop = root.scTr(b + "Track/LoopEnableState"), tBrw = root.scTr("/GUI/ShowBrowseView")
		const lenS = tLen ? (tLen.unnormalized || 0) : 0, posS = tPos ? (tPos.unnormalized || 0) : 0, sr = (tSr && tSr.value > 0) ? tSr.value : 44100
		return {
			songLoaded: !!(tLoaded && tLoaded.state), playing: !!(tPlay && tPlay.state),
			posNorm: lenS > 0 ? (posS / lenS) : 0.0, durationSec: lenS > 0 ? (lenS / sr) : 0.0,
			speed: tSpd ? tSpd.value : 0.5, speedRangeIndex: tRng ? tRng.index : 0,
			speedRangeCount: (tRng && tRng.entries) ? tRng.entries.length : 4,
			keylock: !!(tKey && tKey.state), sync: !!(tSync && tSync.state),
			loopEnabled: !!(tLoop && tLoop.state), browseOpen: !!(tBrw && tBrw.state)
		}
	}
	function scMidiApplyOps(r) {
		if (!r || !r.seq) return
		const d = r.targetDeck || 3, b = "/Engine/Deck" + d + "/", v = r.values || ({})
		if (v.vinylMode !== undefined) { const t = root.scTr(b + "ExternalScratchWheelScratchEnable"); if (t) t.state = !!v.vinylMode }
		if (v.jogTouch !== undefined) { const t = root.scTr(b + "ExternalScratchWheelTouch"); if (t) t.value = v.jogTouch ? 1.0 : 0.0 }
		if (v.jogMoveTicks !== undefined && v.jogMoveTicks !== 0) {
			const t = root.scTr(b + "ExternalScratchWheelMove")
			if (t) {
				if (v.jogMoveTicks > 0) t.incValue(v.jogMoveTicks, 0.0, 0.0, false, true, 0.0)
				else t.decValue(-v.jogMoveTicks, 0.0, 0.0, false, true, 0.0)
			} else if (v.setPosNorm !== undefined) { const tp = root.scTr(b + "Track/SetSongPosition"); if (tp) tp.value = v.setPosNorm }
		} else if (v.setPosNorm !== undefined) { const t = root.scTr(b + "Track/SetSongPosition"); if (t) t.value = v.setPosNorm }
		if (v.nudgeFwd !== undefined) { const t = root.scTr(b + "Track/NudgeForward"); if (t) t.value = v.nudgeFwd }
		if (v.nudgeBwd !== undefined) { const t = root.scTr(b + "Track/NudgeBackward"); if (t) t.value = v.nudgeBwd }
		if (v.speed !== undefined) { const t = root.scTr(b + "Speed"); if (t) t.setValue(v.speed, 1, 0) }
		if (v.speedOffsetUp !== undefined) { const t = root.scTr(b + "SpeedOffsetUp"); if (t) t.state = !!v.speedOffsetUp }
		if (v.speedOffsetDown !== undefined) { const t = root.scTr(b + "SpeedOffsetDown"); if (t) t.state = !!v.speedOffsetDown }
		if (v.speedNeutral !== undefined) { const t = root.scTr(b + "SpeedNeutral"); if (t) t.state = !!v.speedNeutral }
		if (v.play !== undefined) { const t = root.scTr(b + "Play"); if (t) t.state = !!v.play }
		if (v.cueHold !== undefined) { const t = root.scTr(b + "Track/CueAction"); if (t) t.value = v.cueHold ? 1.0 : 0.0 }
		const acts = r.actions || []
		for (let i = 0; i < acts.length; i++) {
			const a = acts[i].action
			if (a === "toggle_play") { const tp = root.scTr(b + "Play"), tps = root.scTr(b + "PlayState"); if (tp) tp.state = !(tps ? tps.state : tp.state) }
			else if (a === "track_next") { const t = root.scTr(b + "Track/NextTrack"); if (t) t.state = true }
			else if (a === "track_prev") { const t = root.scTr(b + "Track/PreviousTrack"); if (t) t.state = true }
			else if (a === "seek_forward") { const t = root.scTr(b + "Track/SeekForward"); if (t) t.state = true }
			else if (a === "seek_backward") { const t = root.scTr(b + "Track/SeekBackward"); if (t) t.state = true }
			else if (a === "loop_in") { const t = root.scTr(b + "Track/LoopIn"); if (t) t.state = true }
			else if (a === "loop_out") { const t = root.scTr(b + "Track/LoopOut"); if (t) t.state = true }
			else if (a === "reloop_exit") { const t = root.scTr(b + "Track/Reloop"); if (t) t.state = true }
			else if (a === "loop_halve") { const t = root.scTr(b + "Track/LoopHalveDouble"); if (t) t.decValue(0.1, 0.0, 1.0, false, false) }
			else if (a === "loop_double") { const t = root.scTr(b + "Track/LoopHalveDouble"); if (t) t.incValue(0.1, 0.0, 1.0, false, false) }
			else if (a === "cycle_tempo_range") { const t = root.scTr(b + "SpeedRange"); if (t) { const c = t.entries ? t.entries.length : 4; t.index = (t.index + 1) % c } }
			else if (a === "toggle_keylock") { const t = root.scTr(b + "Track/KeyLock"); if (t) t.state = !t.state }
			else if (a === "toggle_sync") { const ts = root.scTr(b + "Sync"), tu = root.scTr(b + "UnSync"); if (ts && !ts.state) ts.state = true; else if (tu) tu.state = true }
			else if (a === "load_selected_track") { const t = root.scTr("/Client/Librarian/LoadToDeck" + d); if (t) t.state = true }
			else if (a === "toggle_browse_view") { const t = root.scTr("/GUI/ShowBrowseView"); if (t) t.state = !t.state }
			else if (a === "browse_scroll") {
				const st = acts[i].steps || 0, t = root.scTr(st > 0 ? "/Qml/ControlFocus/Increment" : "/Qml/ControlFocus/Decrement")
				for (let s = 0; s < Math.min(8, Math.abs(st)); s++) if (t) t.state = true
			}
		}
		scMidiLastAckSeq = r.seq
	}
	Timer {
		id: scMidiPollTimer
		interval: 16; running: false; repeat: true
		onTriggered: {
			if (!root.scUiLoaded) return
			let x = root.scMidiXhr
			if (!x) {
				x = new XMLHttpRequest()
				root.scMidiXhr = x
				x.onreadystatechange = function() {
					if (x.readyState !== 4) return
					if (x.status === 200) {
						try {
							const r = JSON.parse(x.responseText)
							root.scMidiFailCount = 0
							root.scMidiActive = !!r.enabled
							if (Math.scMidiBridge) {
								Math.scMidiBridge.helperOnline = true
								Math.scMidiBridge.enabled = !!r.enabled
							}
							if (r.targetDeck) root.scMidiTargetDeck = r.targetDeck
							if (r.seq) {
								root.scMidiApplyOps(r)
								root.scMidiLastApplyMs = Math.max(0.5, (Date.now() - root.scMidiSendAt) * 0.5)
							}
							if (!r.enabled) {
								scMidiPollTimer.stop()
							} else {
								scMidiPollTimer.interval = 16
							}
							return
						} catch (e) {}
					}
					root.scMidiActive = false
					root.scMidiFailCount = (root.scMidiFailCount || 0) + 1
					if (Math.scMidiBridge) {
						Math.scMidiBridge.helperOnline = false
						Math.scMidiBridge.lastError = "MIDI Mapper helper unreachable"
					}
					if (root.scMidiFailCount >= 3) {
						scMidiPollTimer.stop()
					} else {
						scMidiPollTimer.interval = 2000
					}
				}
			}
			if (x.readyState > 0 && x.readyState < 4) return
			const body = { action: "poll", deckState: root.scMidiReadDeck(root.scMidiTargetDeck || 3) }
			if (root.scMidiLastAckSeq > 0) {
				body.ackSeq = root.scMidiLastAckSeq
				body.ackApplyMs = root.scMidiLastApplyMs
				root.scMidiLastAckSeq = 0
			}
			root.scMidiSendAt = Date.now()
			x.open("POST", "http://127.0.0.1:17838/action")
			x.setRequestHeader("Content-Type", "application/json")
			x.send(JSON.stringify(body))
		}
	}
	function scPlainTitle(song) { return (song || "").replace(/\\s\\[\\d+ BPM(?: [0-9]{1,2}[AB])?\\]$/, "") }
	// Engine's messages name its one streaming client; say which service is showing.
	function scBrand(s) {
		const service = scServices.find(v => v.id === scScope)
		return s && service && scScope !== "tidal" ? String(s).replace(/TIDAL/gi, service.name) : s
	}
	// Capture manual and auto-detected IANA time zones via stock Planck.timeDateController
	// signals and persist them through stream-sync (/data/enginex/state/timezone) without
	// interposing setenv/unsetenv in Engine. Treat UTC as an unsolicited fallback only
	// shortly after boot and only while the time-zone picker has not been used; a UTC
	// chosen in the picker (or after the boot window) is saved like any zone.
	property bool scTimeZoneBootWindowOpen: true
	Timer {
		interval: 30000; running: true; repeat: false
		onTriggered: root.scTimeZoneBootWindowOpen = false
	}
	property string scLastGoodTimeZone: ""
	property string scPendingTimeZone: ""
	property bool scRestoringTimeZone: false
	property bool scTimeZonePickerUsed: false
	property bool scTimeZonePickerHooked: false
	function scHookTimeZonePicker() {
		if (scTimeZonePickerHooked || typeof Planck === "undefined" || !Planck) return
		const p = Planck.getProperty("/GUI/ShowTimeZoneView")
		if (!p || !p.translator) return
		scTimeZonePickerHooked = true
		if (p.translator.state) scTimeZonePickerUsed = true
		if (p.translator.stateChanged) {
			p.translator.stateChanged.connect(function() {
				if (p.translator.state) root.scTimeZonePickerUsed = true
			})
		}
	}
	function scCheckTimeZonePicker() {
		scHookTimeZonePicker()
		if (scTimeZonePickerUsed || typeof Planck === "undefined" || !Planck) return scTimeZonePickerUsed
		const p = Planck.getProperty("/GUI/ShowTimeZoneView")
		if (p && p.translator && p.translator.state) scTimeZonePickerUsed = true
		return scTimeZonePickerUsed
	}
	function scIsBadTimeZone(tz) {
		if (!tz || tz.length < 3 || tz.length > 63) return true
		if (tz === "UTC") return false
		if (tz.indexOf("/") <= 0) return true
		const parts = tz.split("/")
		for (let i = 0; i < parts.length; i++) {
			const s = parts[i]
			if (!s || s === "." || s === ".." || s.length > 32) return true
		}
		return false
	}
	function scNormalizeTimeZone(raw, utc) {
		const s = String(raw || "").trim().replace(/ /g, "_")
		if (utc) return s === "Etc/UTC" ? "Etc/UTC" : "UTC"
		return s
	}
	function scIsFallbackUtc(tz) {
		return tz === "UTC" || tz === "Etc/UTC" || tz === "Universal"
	}
	function scPostTimeZone(tz) {
		if (scIsBadTimeZone(tz)) return
		const x = new XMLHttpRequest()
		x.open("POST", "http://127.0.0.1:8080/v1/sc5000/ui?timezone=" + encodeURIComponent(tz))
		x.onreadystatechange = function() {
			if (x.readyState !== 4) return
			if (x.status === 200 && root.scPendingTimeZone === tz) root.scPendingTimeZone = ""
		}
		x.send()
	}
	function scOnTimeZoneChanged() {
		if (scRestoringTimeZone || typeof Planck === "undefined" || !Planck || !Planck.timeDateController) return
		scCheckTimeZonePicker()
		const c = Planck.timeDateController
		const utc = !!c.isSystemTimeZoneUTC()
		const tz = scNormalizeTimeZone(c.getSystemTimeZone(), utc)
		if (scIsBadTimeZone(tz)) return
		if (utc && !scTimeZonePickerUsed && scTimeZoneBootWindowOpen) {
			if (!scRestoringTimeZone && scLastGoodTimeZone !== "" && !scIsFallbackUtc(scLastGoodTimeZone)) {
				scRestoringTimeZone = true
				c.setSystemTimeZone(scLastGoodTimeZone)
				scRestoringTimeZone = false
			}
			return
		}
		scLastGoodTimeZone = tz
		scPendingTimeZone = tz
		scPostTimeZone(tz)
	}
	Connections {
		target: typeof Planck !== "undefined" && Planck ? Planck.timeDateController : null
		ignoreUnknownSignals: true
		function onTimeZoneSelect() { root.scTimeZonePickerUsed = true }
		function onSystemTimeZoneSetSucceeded() { root.scOnTimeZoneChanged() }
	}
	function scCall(query, done) {
		if (!query && !done) {
			if (!scUiPoll) {
				scUiPoll = new XMLHttpRequest()
				scUiPoll.onreadystatechange = scUiPollReply
			}
			if (scUiPoll.readyState !== 0 && scUiPoll.readyState !== 4) {
				if (Date.now() - scUiPollAt < 10000) return
				scUiPoll.abort()
			}
			scUiPollAt = Date.now()
			scUiPoll.open("GET", "http://127.0.0.1:8080/v1/sc5000/ui")
			scUiPoll.send()
			return
		}
		const x = new XMLHttpRequest()
		x.open(query ? "POST" : "GET", "http://127.0.0.1:8080/v1/sc5000/ui" + (query ? "?" + query : ""))
		x.onreadystatechange = function() {
			if (x.readyState !== 4 || x.status !== 200) return
			const reply = JSON.parse(x.responseText)
			if (!reply.fxSelectorReady) scApply(reply)
			if (done) done(reply)
		}
		x.send()
	}
	property var scUiPoll: null
	property real scUiPollAt: 0
	property string scUiDigest: ""
	property real scUiAppliedAt: 0
	function scUiPollReply() {
		if (scUiPoll.readyState !== 4 || scUiPoll.status !== 200) return
		// Unchanged settings (stream-sync's digest): skip the parse and the writes,
		// but re-apply every 30 s in case a local change raced a reply.
		const digest = scUiPoll.getResponseHeader("X-UI-Digest") || "", now = Date.now()
		if (digest && digest === scUiDigest && now - scUiAppliedAt < 30000) { scEnsureTidal(); return }
		scApply(JSON.parse(scUiPoll.responseText))
		scUiDigest = digest; scUiAppliedAt = now
	}
	function scApply(v) {
		if (v.fxSelectorAvailable !== undefined) {
			scFxMasterBypassed = !!v.fxSelectorAvailable
			scFxSelectorAvailable = scFxMasterBypassed && (Config.audio.fx.hasDJFx || Config.audio.fx.hasNHFx)
		}
		scEnsureTidal()
		if (v.version) scVersion = v.version
		if (v.buildTag !== undefined) scBuildTag = String(v.buildTag || "")
		scLang = v.language
		scWave = v.waveform
		scQuality = v.quality || "flac"
		scNeteaseMode = v.neteaseMode || "official"
		scUpdateRepo = v.githubRepo || scUpdateRepo
		if (v.accessCode) { scAccessCode = v.accessCode; scConsoleUrl = v.consoleUrl || "" }
		if (v.accessCode || v.lanStems !== undefined) root.scLanStemsInfo = v.lanStems || null
		scLanStems = root.scLanStemsLine ? root.scLanStemsLine(root.scLanStemsInfo, scLang) : ""
		scMasterLimiter = !!v.masterLimiter
		scAutoGain = !!v.autoGain
		scAutoAnalyze = !!v.autoAnalyze
		scOmniSource = !!v.omniSource
		scProlinkShare = !!v.prolinkShare
		scProlinkExport = !!v.prolinkExport
		scPlayer = !!v.player
		scFourDeckRole = v.fourDeckWaveform ? (v.fourDeckRoleActive || "") : ""
		scFourDeckOn = !!v.fourDeckWaveform
		scFourDeckRoleSetting = v.fourDeckRole || "auto"
		scShadowMode = v.shadowMode || "auto"
		scKeepPlayhead(v.playhead || 0)
		if (!scSameItems(scSources, v.sources)) scSources = v.sources
		if (!scSameItems(scLoggedIn, v.loggedIn)) scLoggedIn = v.loggedIn
		scScope = v.scope
		scBroadcastOn = !!v.broadcastEnabled
		const prevUpfaderEcho = scUpfaderEcho
		scUpfaderEcho = !!v.upfaderEcho
		if (scHasMixer && scTransitionFxView && (!scUiLoaded || scUpfaderEcho !== prevUpfaderEcho)) {
			if (!!scTransitionFxView.enabled !== scUpfaderEcho)
				scTransitionFxView.enabled = scUpfaderEcho
		}
		if (typeof v === "string") scParseSettingsConf(v)
		else if (v) {
			if (v.cueCountdown !== undefined) scCueCountdown = !!v.cueCountdown
			else if (v.cue_countdown !== undefined) scCueCountdown = v.cue_countdown !== "0" && v.cue_countdown !== 0 && v.cue_countdown !== false
			if (v.browserDualColumn !== undefined) scDualColumn = !!v.browserDualColumn
			else if (v.browser_dual_column !== undefined) scDualColumn = v.browser_dual_column === "1" || v.browser_dual_column === 1 || v.browser_dual_column === true
			if (v.tracklistWaveform !== undefined) scTracklistWaveform = !!v.tracklistWaveform
			else if (v.tracklist_waveform !== undefined) scTracklistWaveform = v.tracklist_waveform === "1" || v.tracklist_waveform === 1 || v.tracklist_waveform === true
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
			scFxMsApply(v.fxTimeMode !== undefined ? v.fxTimeMode : v.fx_time_mode,
			            v.fxTimeMs !== undefined ? v.fxTimeMs : v.fx_time_ms)
			if (v.pioneerFx !== undefined) scPioneerFx = v.pioneerFx || "off"
			else if (v.pioneer_fx !== undefined) scPioneerFx = v.pioneer_fx || "off"
		}
		if (scHasStemsEq && scStemsEqMode && !scStemsEqApplied) {
			scStemsEqApplied = true
			for (let ch = 1; ch <= 4; ch++) {
				const m = root.scTr("/Engine/Mixer/Channel" + ch + "/ChannelStrip/Mode")
				if (m) m.index = 1
			}
		}
		const p = Planck.getProperty("/Client/Preferences/Profile/Application/WaveformStyle")
		if (p.translator && (p.translator.index == 1) !== (v.waveform === "rgb"))
			p.translator.index = v.waveform === "rgb" ? 1 : 0
		if (v && v.timezone && !scIsBadTimeZone(String(v.timezone))) {
			if (scLastGoodTimeZone === "") scLastGoodTimeZone = String(v.timezone)
		}
		const wasUiLoaded = scUiLoaded
		scUiLoaded = true
		if (!wasUiLoaded) {
			scMidiInitBridge()
			scHookTimeZonePicker()
			if (typeof Planck !== "undefined" && Planck && Planck.timeDateController) {
				const c = Planck.timeDateController
				const curUtc = !!c.isSystemTimeZoneUTC()
				const curTz = scNormalizeTimeZone(c.getSystemTimeZone(), curUtc)
				if (!curUtc && !scIsBadTimeZone(curTz)) {
					scLastGoodTimeZone = curTz
					if (!v || v.timezone !== curTz) {
						scPendingTimeZone = curTz
						scPostTimeZone(curTz)
					}
				} else if (curUtc && !scTimeZonePickerUsed && scLastGoodTimeZone !== "" && !scIsFallbackUtc(scLastGoodTimeZone)) {
					scRestoringTimeZone = true
					c.setSystemTimeZone(scLastGoodTimeZone)
					scRestoringTimeZone = false
				}
			}
		} else if (scPendingTimeZone !== "") {
			scPostTimeZone(scPendingTimeZone)
		}
		scProbeCueClocks()
	}
	function scSameItems(a, b) {
		if (a === b) return true
		if (!a || !b || a.length !== b.length) return false
		for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
		return true
	}
	function scFxMsApply(mode, value) {
		scFxTimeMode = mode === "ms" ? "ms" : "beats"
		scFxTimeMs = scFxMsInteger(value)
		if (scFxTimeMode !== "ms") scFxMsWanted = -1
	}
	function scFxMsInteger(value) {
		const text = String(value)
		if (!/^[0-9]+$/.test(text) || text.length > 10) return 500
		const n = Number(text)
		return n <= 4294967295 ? Math.max(10, Math.min(4000, n)) : 500
	}
	function scParseSettingsConf(text) {
		let fxMode = "beats", fxMs = 500
		scCueCountdown = true // default on: only an explicit cue_countdown=0 turns it off
		scTracklistWaveform = true
		for (const line of (text || "").split("\\n")) {
			const eq = line.indexOf("=")
			if (eq < 0) continue
			const key = line.slice(0, eq).trim()
			const val = line.slice(eq + 1).trim()
			if (key === "browser_dual_column") root.scDualColumn = val === "1"
			else if (key === "tracklist_waveform") root.scTracklistWaveform = val !== "0" && val !== "false"
			else if (key === "cue_countdown") root.scCueCountdown = val !== "0"
			else if (key === "sync_guard_pct") root.scSyncGuardPct = parseInt(val) || 0
			else if (key === "swap_loop_order") root.scSwapLoopOrder = val === "1"
			else if (key === "led_brightness") root.scLedBrightness = parseInt(val) || 100
			else if (key === "ui_scale") root.scUIScale = parseInt(val) || 100
			else if (key === "stems_eq_mode") root.scStemsEqMode = val === "1"
			else if (key === "beat_jump_pads") root.scBeatJumpPads = val === "1"
			else if (key === "fx_time_mode" && line.trim().indexOf("fx_time_mode=") === 0) fxMode = line.trim().slice(13) === "ms" ? "ms" : "beats"
			else if (key === "fx_time_ms" && line.trim().indexOf("fx_time_ms=") === 0) fxMs = scFxMsInteger(line.trim().slice(11))
			else if (key === "pioneer_fx") root.scPioneerFx = val || "off"
		}
		scFxMsApply(fxMode, fxMs)
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
		scWave = id
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
		property var trackIds: null
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
			if (!trackIds) trackIds = Array.from({ length: 2000 }, (_, i) => i + 1)
			const lines = (Planck.librarianController.collectionController.getTrackUrisFromUniversalIds(trackIds) || "").split("\\n")
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
		model: root.scDeckList()
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
			visible: root.scHasMixer
			values: ["Off", "On"]
			currentValue: values[root.scUpfaderEcho ? 1 : 0]
			onSelected: function(index) { root.scCall("upfaderEcho=" + index) }
		},
		EntryBase.ListEntry {
			text: "Stems EQ Mode"
			visible: root.scHasStemsEq
			values: ["Off", "On"]
			currentValue: values[root.scStemsEqMode ? 1 : 0]
			onSelected: function(index) { root.scCall("stemsEqMode=" + index) }
		},
		EntryBase.ListEntry {
			text: "Color FX Style"
			visible: !root.scFxMasterBypassed
			readonly property var ids: ["off", "spiral", "pitch_echo", "slip_roll", "dub_echo", "space"]
			values: ["Off", "Spiral", "Pitch Echo", "Slip Roll", "Dub Echo", "Space"]
			currentValue: values[Math.max(0, ids.indexOf(root.scPioneerFx))]
			onSelected: function(index) { root.scCall("pioneerFx=" + ids[index]) }
		},
		EntryBase.ListEntry {
			text: "PX Delay Time Mode"
			visible: root.scHasPxDelay
			values: ["Default (Off)", "Milliseconds (ms)"]
			currentValue: values[root.scFxTimeMode === "ms" ? 1 : 0]
			onSelected: function(index) { root.scCall("fxTimeMode=" + (index === 1 ? "ms" : "beats")) }
		},
		Text {
			visible: root.scHasPxDelay
			Layout.fillWidth: true
			leftPadding: 24; rightPadding: 24
			wrapMode: Text.WordWrap
			color: "#9a9a9a"
			font.pixelSize: 16
			text: "Spiral / Pitch Echo / Dub Echo only; stock Beat FX stay in beats"
		},
		EntryBase.ListEntry {
			text: "PX Delay Time (ms)"
			visible: root.scHasPxDelay && root.scFxTimeMode === "ms"
			readonly property var times: [10, 18, 33, 60, 110, 200, 364, 500, 663, 1207, 2197, 4000]
			values: times.map(n => String(n))
			currentValue: String(root.scFxTimeMs)
			onSelected: function(index) { root.scCall("fxTimeMs=" + times[index]) }
		},
"""

BEAT_JUMP_ENTRIES = """
		EntryBase.ListEntry {
			text: "Beat Jump Pad Bank"
			visible: root.scIsMixstream
			values: ["Off", "On"]
			currentValue: values[root.scBeatJumpPads ? 1 : 0]
			onSelected: function(index) { root.scCall("beatJumpPads=" + index) }
		},
		EntryBase.ListEntry {
			text: "Swap Loop / Beat Jump"
			visible: root.scHasAutoLoopKnob
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

TRACKLIST_WAVEFORM_ENTRY = """
		EntryBase.ListEntry {
			text: "Tracklist Waveform"
			values: ["Off", "On"]
			currentValue: values[root.scTracklistWaveform ? 1 : 0]
			onSelected: function(index) { root.scCall("tracklistWaveform=" + index) }
		},
"""

LIBRARY_ENTRIES = """
		EntryBase.ListEntry {
			text: "OmniSource (Drives in Streaming)"
			values: ["Off", "On"]
			currentValue: values[root.scOmniSource ? 1 : 0]
			onSelected: function(index) { root.scCall("omniSource=" + index) }
		},
""" + TRACKLIST_WAVEFORM_ENTRY

CUE_COUNTDOWN_ENTRY = """
		EntryBase.ListEntry {
			text: "Cue Countdown"
			values: ["Off", "On"]
			currentValue: values[root.scCueCountdown ? 1 : 0]
			onSelected: function(index) { root.scCall("cueCountdown=" + index) }
		},
"""

UI_SCALE_ENTRY = """
		EntryBase.ListEntry {
			text: "UI Scale"
			entryWidth: narrow ? 104 : 88
			readonly property var ids: ["110", "105", "100", "95", "90", "85", "80", "75"]
			values: ["110%", "105%", "100%", "95%", "90%", "85%", "80%", "75%"]
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
			visible: root.scCanFourDeck
			values: ["Off", "On"]
			currentValue: root.scFourDeckOn ? "On" : "Off"
			onSelected: function(index) { if ((index === 1) !== root.scFourDeckOn) root.scRestartCall("fourDeckWaveform=" + index) }
		},
		EntryBase.ListEntry {
			text: "4-Deck Role"
			entryWidth: 104
			visible: root.scCanFourDeck && root.scFourDeckOn
			readonly property var ids: ["auto", "13", "24", "12", "34", "1", "2"]
			values: ["Auto", "1+3", "2+4", "1+2", "3+4", "1", "2"]
			currentValue: values[Math.max(0, ids.indexOf(root.scFourDeckRoleSetting))]
			onSelected: function(index) { if (ids[index] !== root.scFourDeckRoleSetting) root.scRestartCall("fourDeckRole=" + ids[index]) }
		},
		EntryBase.ListEntry {
			id: scShadowModeEntry
			readonly property var ids: ["auto", "engine", "cdj"]
			readonly property int at: Math.max(0, ids.indexOf(root.scShadowMode))
			text: "Shadow Deck Mode"
			visible: root.scCanFourDeck && root.scFourDeckOn
			entryWidth: 176 // "All Engine Mode" does not fit the stock 136
			values: ["Auto", "All Engine Mode", "All CDJ Mode"]
			currentValue: values[at]
			onSelected: function(index) { if (index !== at) root.scRestartCall("shadowMode=" + ids[index]) }
		},
		@HEADLINE@ { // one sentence on the selected mode; too long for the row itself
			visible: root.scCanFourDeck && root.scFourDeckOn
			leftPadding: 24; rightPadding: 24
			wrapMode: Text.WordWrap
			Layout.preferredHeight: implicitHeight
			color: "#9a9a9a"
			text: ["Auto: copyable tracks load locally like a local deck; uncopyable CDJ tracks show CDJ waveforms", "All Engine Mode: Engine loads every source and shows it exactly like a local deck", "All CDJ Mode: no track is loaded for any source (CDJ / Pro DJ Link view)"][scShadowModeEntry.at]
		},
"""

# #99 Layout Manager: what a View 1..5 keeps besides the stock layout (scViewSnapshot).
VIEW_ENTRIES = """
		@HEADLINE@ {
			text: "ENGINEX VIEW PRESET"
		},
""" + CUE_COUNTDOWN_ENTRY + UI_SCALE_ENTRY + DUAL_COLUMN_ENTRY + BEAT_JUMP_ENTRIES + FOUR_DECK_ENTRIES

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
							onClicked: if (!seg.selected) { root.scWave = seg.waveId; root.scCall("waveform=" + seg.waveId) }
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
				visible: root.scHasMixer && !root.scHasStockTransitionTile && !(typeof Config !== "undefined" && Config && Config.device && Config.device.hasHardwareFaderFxButtons)
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
									const on = index !== 0
									root.scUpfaderEcho = on
									root.scSetTransitionFx(vfx, on, index === 2)
									root.scCall("upfaderEcho=" + (on ? "1" : "0"))
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
		model: root.scDeckList()
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
		for (const d of root.scDeckList()) {
			const o = Planck.getProperty("/Engine/Deck%1/OriginalBPM".arg(d)).translator
			const s = Planck.getProperty("/Engine/Deck%1/SyncMode".arg(d)).translator
			const l = Planck.getProperty("/Engine/Deck%1/Track/SongLoaded".arg(d)).translator
			if (m && o && s && l && l.state && s.string !== "Off" && o.unnormalized > 0 && m.unnormalized > 0) scWidenRange(d, scHarmonicSyncNeed(m.unnormalized, o.unnormalized))
		}
	}
"""

# Control Center (5.3.0 only): Engine's own Sync Mode preference (Bar / Beat /
# Tempo) as a Control Center tile, next to SMART_SYNC_MAIN.
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


# Read by the display-only shadow lane; the Browse encoder zooms the stock way.
ZOOM_PROP = """
	readonly property QObjProperty scZoomProp: Planck.getProperty("/Engine/Master/WaveformZoom")
"""


GUARD = '(typeof root !== "undefined" && root && root.scDeckArtUrl)'

# Keep the stock stacking context: native modal views must cover every shadow item.
# Full-track textures and marker delegates are static; a single content x offset moves.
SHADOW_LAYER = r"""
    Rectangle {
        id: scDisplay
        objectName: "EngineX_Shadow_@KIND@"
        readonly property Item owner: @OWNER@
        parent: layoutParent()
        function layoutParent() {
            let p = owner.parent
            while (p && (p instanceof RowLayout || p instanceof ColumnLayout || p instanceof GridLayout || p instanceof StackLayout || p instanceof Row || p instanceof Column || p instanceof Grid || p instanceof Flow)) p = p.parent
            return p
        }
        readonly property int deck: typeof root !== "undefined" && root && root.scShadowDeckFor ? root.scShadowDeckFor(owner) : 0
        readonly property bool active: typeof root !== "undefined" && root && root.scShadowVisible && root.scShadowVisible(deck)
        readonly property bool wave: "@KIND@" === "waveform"
        readonly property bool overview: "@KIND@" === "overview"
        readonly property bool performance: "@KIND@" === "performance"
        readonly property bool graphical: wave || overview
        // Only a displayed layer follows the shadow state. Stock views keep hidden
        // instances of every lane (16-18 waveforms per player); each one used to
        // re-run its whole binding tree on every poll reply (16-20 ms GUI time).
        readonly property bool live: active && inheritedVisible
        readonly property var sample: live ? root.scShadowData(deck) : null
        // var properties notify on every write, even with the same object: copy
        // meta/cues/loops/grid/cueInfo only when the object itself changes.
        property var meta: null
        property var cues: root.scShadowEmptyArray
        property var loops: root.scShadowEmptyArray
        property real position: 0
        property bool stale: false
        property var cueInfo: null
        property var grid: root.scShadowEmptyArray
        property int gridSlots: 0 // grows in powers of two while shown; hidden layers keep none
        property int tileLo: 0
        property int tileHi: 31
        onLiveChanged: if (!live) gridSlots = 0
        property real gridAnchor: -Infinity
        function refreshSample() {
            const s = sample, m = s && s.meta || null
            if (m !== meta) meta = m
            const c = graphical && s ? root.scShadowCuesFor(deck, s) : root.scShadowEmptyArray
            if (c !== cues) cues = c
            refreshPosition()
            refreshDetails() // track/performance: cueInfo follows a jump or a cue edit at once
        }
        // The waveform scrolls on the render thread: an XAnimator, no per-frame property
        // writes or bindings on the GUI thread. The overview moves < 4 px/s and follows
        // the 200 ms clock without animation.
        // An Animator's clock counts frames at the display mode's whole refresh rate: 59
        // for the players' 59.34 Hz panel, so it runs 0.57 % fast (5.7 ms a second, 3.4 s
        // in ten minutes) and loses every dropped frame. The 200 ms clock steers it:
        // stop() writes the frame on screen back to x, the next run goes on from there
        // and closes what it is off the line within 0.4 s. Only a jump jumps.
        property real scrollSpeed: 0 // of the running scroll, track ms per animation ms
        property string scrollKey: ""
        property int scrollJumps: 0
        function contentX(ms) { return -(ms - span * playhead) / span * width }
        // Read from meta itself in the refresh functions: inside onMetaChanged the
        // bindings below (duration, totalPixels, ...) still hold the previous meta,
        // so a pending -> detail switch while paused drew the tile window of the
        // overview-only reply (no tile under the playhead: blank waveform).
        function metaDuration(m) { return m && m.durationMs > 0 ? m.durationMs : 1 }
        function metaWaveDuration(m) { return m && m.waveformDurationMs > 0 ? m.waveformDurationMs : metaDuration(m) }
        function metaTotalPixels(m) { return m && m.waveformWidth > 0 && m.waveformWidth <= 65536 ? m.waveformWidth : 0 }
        function metaTilePixels(m) { return m && m.tileWidth > 0 ? Math.min(2048, m.tileWidth) : 2048 }
        function refreshPosition(tick) {
            const now = Date.now(), s = sample
            const pos = s ? root.scShadowPosition(s, now) : 0
            if (pos !== position) position = pos
            const st = !!s && (s.stale || now - Math.max(s.at, root.scShadowLive.seen) >= root.scShadowExpiryMs)
            if (st !== stale) stale = st
            if (!wave) return
            const end = metaDuration(meta)
            const speed = live && visible && s && s.playing && !st && pos < end ? root.scShadowRate(s) : 0
            if (speed > 0) {
                let from = pos, v = speed
                if (scroll.running && scrollSpeed > 0 && s.key === scrollKey && s.jumps === scrollJumps) {
                    if (!tick) return // a new sample on the same line: the clock steers there
                    scroll.stop()
                    const shown = (playhead - scMoving.x / width) * span
                    // Same track, no jump (seek, hot cue, loop) and within 300 ms: on from
                    // what is shown, at the speed that is on the line 0.4 s later.
                    if (Math.abs(pos - shown) <= 300) {
                        from = shown
                        v = Math.max(speed / 4, speed + (pos - shown) / 400)
                    }
                } else scroll.stop()
                from += v * 17 // a run shows its first value one frame after the last one's
                scrollSpeed = v; scrollKey = s.key; scrollJumps = s.jumps
                scroll.from = contentX(from)
                scroll.to = contentX(end)
                scroll.duration = Math.max(1, Math.round((end - from) / v))
                scroll.start()
            } else {
                const x = contentX(pos)
                if (scrollSpeed > 0 || scroll.running) {
                    // A stopped Animator has written its own value back: settle with a
                    // 1 ms run to x, so the scene ends at x too (CUE released on its spot).
                    scroll.stop()
                    scroll.from = x; scroll.to = x; scroll.duration = 1
                    scroll.start()
                } else if (scMoving.x !== x) scMoving.x = x
                scrollSpeed = 0
            }
        }
        // Forget the running line (zoom, size, visibility); refreshPosition restarts or settles.
        function restartScroll() { scrollSpeed = 0; refreshPosition() }
        function refreshDetails() {
            if (!graphical) {
                const ci = live ? root.scNextCueInfo(deck, 0) : null
                if (ci !== cueInfo) cueInfo = ci
            }
            if (!wave || !meta) {
                if (grid.length) grid = root.scShadowEmptyArray
                gridAnchor = -Infinity
                return
            }
            const padding = Math.min(span / 2, 10000), from = position - span * playhead, m = meta
            // Re-anchor at half the padding: the view (scrolled on the render thread)
            // runs up to one 200 ms clock ahead of start.
            if (Math.abs(from - gridAnchor) > padding / 2) {
                gridAnchor = from
                // Only the tiles under this window are drawn: every Image is its own
                // texture batch, and clipped ones still cost the render thread.
                const total = metaTotalPixels(m), tileMs = total > 0 ? metaTilePixels(m) / total * metaWaveDuration(m) : 0
                const lo = tileMs > 0 ? Math.floor((from - padding) / tileMs) : 0, hi = tileMs > 0 ? Math.floor((from + span + padding) / tileMs) : 31
                if (lo !== tileLo) tileLo = lo
                if (hi !== tileHi) tileHi = hi
                const rows = root.scShadowGrid(deck, m, from - padding, from + span + padding)
                if (rows.length > gridSlots) gridSlots = Math.max(32, 1 << Math.ceil(Math.log2(rows.length)))
                if (rows !== grid) grid = rows
            }
        }
        onSampleChanged: refreshSample()
        onVisibleChanged: { restartScroll(); if (visible) refreshDetails() }
        onWidthChanged: restartScroll()
        onMetaChanged: {
            const l = graphical && meta && meta.loops || root.scShadowEmptyArray
            if (l !== loops) loops = l
            gridAnchor = -Infinity; restartScroll(); refreshDetails()
        }
        onSpanChanged: { gridAnchor = -Infinity; restartScroll(); refreshDetails() }
        // The stale cut-off and the grid window still need a clock between replies.
        // A resting or stale sample does not move: its refreshes come with the sample.
        Timer { interval: 200; repeat: true; running: scDisplay.live && scDisplay.visible && scDisplay.sample !== null && scDisplay.sample.playing && !scDisplay.sample.stale; onTriggered: { scDisplay.refreshPosition(true); scDisplay.refreshDetails() } }
        readonly property real duration: metaDuration(meta)
        readonly property real waveformDuration: metaWaveDuration(meta)
        readonly property real pitchRate: wave && sample ? Math.max(0.05, 1 + (Number(sample.pitch) || 0) / 100) : 1
        readonly property real span: overview ? duration : Math.max(1000, (root.scZoomProp && root.scZoomProp.translator ? root.scZoomProp.translator.unnormalized : 8) * 1000) * pitchRate
        // The lane keeps the stock playhead setting (WaveFormItem.zeroPosition, fed
        // from /Client/Preferences/PlayheadPosition): same line as the local lanes.
        readonly property real playhead: "@KIND@" === "overview" ? position / duration : wave && owner.zeroPosition > 0 && owner.zeroPosition < 1 ? owner.zeroPosition : 0.5
        onPlayheadChanged: if (wave) { gridAnchor = -Infinity; restartScroll(); refreshDetails() }
        // Stock overview style: Praline (default) grows from the bottom edge, Butterfly (1) is mirrored.
        readonly property bool half: "@KIND@" === "overview" && (root.scTr("/Client/Preferences/Profile/Application/TrackOverviewStyle") || {}).index !== 1
        readonly property real start: overview ? 0 : position - span * playhead
        readonly property var tiles: meta && meta.detail && Array.isArray(meta.waveformTiles) ? meta.waveformTiles : []
        readonly property int totalPixels: metaTotalPixels(meta)
        readonly property int tilePixels: metaTilePixels(meta)
        readonly property bool tiled: wave && totalPixels > 0 && tiles.length > 0 && tiles.length <= 32
        readonly property bool detailed: !!meta && !!meta.detail && (!!meta.waveformUrl || tiled)
        readonly property int busyState: live && wave ? root.scShadowBusy(sample, meta, detailed) : 0
        // While the detail is still coming, the scrolling lane shows only grid + playhead; a stretched overview there looks like the whole song (user report 11-10).
        readonly property string imageUrl: !meta ? "" : (overview ? meta.overviewUrl || meta.waveformUrl : detailed ? meta.waveformUrl || meta.overviewUrl : meta.detailPending ? "" : meta.overviewUrl || meta.waveformUrl) || ""
        readonly property bool rgb: graphical && (wave && typeof owner.waveformStyleProperty !== "undefined" && owner.waveformStyleProperty.translator ? owner.waveformStyleProperty.translator.index === 1 : root.scWave === "rgb")
        @PALETTE@
        property string paletteQuery: ""
        property string pastPaletteQuery: ""
        function refreshPalette() {
            const suffix = "&saturation=" + saturation + "&brightAmp=" + brightAmp + "&brightGain=" + brightGain + "&brightFloor=" + brightFloor + (half ? "&half=1" : "")
            paletteQuery = "rgb=" + (rgb ? "1" : "0") + "&low=" + String(lowColor).slice(1) + "&mid=" + String(midColor).slice(1) + "&high=" + String(highColor).slice(1) + suffix
            pastPaletteQuery = rgb ? paletteQuery + "&dim=0.6" : "rgb=0&low=" + String(pastLowColor).slice(1) + "&mid=" + String(pastMidColor).slice(1) + "&high=" + String(pastHighColor).slice(1) + suffix
        }
        onRgbChanged: paletteUpdate.restart()
        onHalfChanged: paletteUpdate.restart()
        onLowColorChanged: paletteUpdate.restart()
        onMidColorChanged: paletteUpdate.restart()
        onHighColorChanged: paletteUpdate.restart()
        onPastLowColorChanged: paletteUpdate.restart()
        onPastMidColorChanged: paletteUpdate.restart()
        onPastHighColorChanged: paletteUpdate.restart()
        onSaturationChanged: paletteUpdate.restart()
        onBrightAmpChanged: paletteUpdate.restart()
        onBrightGainChanged: paletteUpdate.restart()
        onBrightFloorChanged: paletteUpdate.restart()
        // Coalesce the stock properties: never request intermediate mixed palettes.
        Timer { id: paletteUpdate; interval: 16; onTriggered: scDisplay.refreshPalette() }
        function waveAsset(url, past) { const query = past ? pastPaletteQuery : paletteQuery; return url && query ? root.scShadowAsset(url) + (url.indexOf("?") < 0 ? "?" : "&") + query : "" }
        function screenX(ms) { return ms / span * width }
        width: owner.width
        height: owner.height
        // Follow geometry signals, not the animation tick. Reparented layers must
        // also respect the visibility of every original ancestor.
        property bool inheritedVisible: false
        property var geometryWatches: []
        property var basis: Qt.matrix4x4(1,0,0,0, 0,1,0,0, 0,0,1,0, 0,0,0,1)
        property real ax: NaN
        property real ay: NaN
        property real bx: NaN
        property real by: NaN
        property real cx: NaN
        property real cy: NaN
        function refreshGeometry() {
            let shown = active
            for (let p = owner; p && p !== parent; p = p.parent) if (!p.visible) { shown = false; break }
            inheritedVisible = shown
            if (!shown || !(owner.width > 0 && owner.height > 0)) return
            const a = owner.mapToItem(parent,0,0), b = owner.mapToItem(parent,1,0), c = owner.mapToItem(parent,0,1)
            if (ax === a.x && ay === a.y && bx === b.x && by === b.y && cx === c.x && cy === c.y) return
            ax=a.x; ay=a.y; bx=b.x; by=b.y; cx=c.x; cy=c.y
            basis = Qt.matrix4x4(b.x-a.x,c.x-a.x,0,a.x, b.y-a.y,c.y-a.y,0,a.y, 0,0,1,0, 0,0,0,1)
        }
        function unwatchGeometry() {
            for (let i=0;i<geometryWatches.length;i++) {
                const w=geometryWatches[i]
                try { w.signal.disconnect(w.callback) } catch(err) {}
            }
            geometryWatches=[]
        }
        function watchGeometry() {
            unwatchGeometry()
            for (let p=owner;p && p!==parent;p=p.parent) {
                for (const name of ["xChanged","yChanged","widthChanged","heightChanged","rotationChanged","scaleChanged","visibleChanged","parentChanged"]) {
                    const signal=p[name], callback=name === "parentChanged" ? watchGeometry : refreshGeometry
                    if (signal && signal.connect) {
                        signal.connect(callback)
                        geometryWatches.push({signal:signal,callback:callback})
                    }
                }
            }
            refreshGeometry()
        }
        onActiveChanged: refreshGeometry()
        onParentChanged: if (geometryWatches) watchGeometry()
        Component.onCompleted: { watchGeometry(); paletteUpdate.restart() }
        Component.onDestruction: unwatchGeometry()
        transform: Matrix4x4 { matrix: scDisplay.basis }
        z: {
            let p = owner
            while (p.parent && p.parent !== parent) p = p.parent
            return p.z + 0.01
        }
        clip: true
        visible: active && inheritedVisible && owner.width > 0 && owner.height > 0
        color: wave ? "black" : "#101114" // the stock lanes are black: on grey the stock fade over the left edge shows as a band
        Binding { target: scDisplay; property: "color"; value: scDisplay.overview ? Style.surface01Color : "transparent"; when: !scDisplay.wave; restoreMode: Binding.RestoreBindingOrValue }
        // Stock roots can hide an empty Engine slot. Restore their original bindings on exit;
        // never a value: read while the view was off screen, visible is false, and writing
        // that back hid the stock lane for good (Engine mode showed no waveform).
        Binding { target: scDisplay.owner; property: "visible"; value: true; when: scDisplay.active; restoreMode: Binding.RestoreBinding }
        Binding { target: scDisplay.owner; property: "enabled"; value: false; when: scDisplay.active; restoreMode: Binding.RestoreBindingOrValue }
        // The stock view under this one is fully covered: opacity 0 keeps its layout
        // but drops it from the scene graph (-2 % render thread at rest).
        Binding { target: scDisplay.owner; property: "opacity"; value: 0; when: scDisplay.visible; restoreMode: Binding.RestoreBindingOrValue }
        // Text lanes (track, performance) and the overview change a few times a
        // second at most: render them once into a texture (-3 % render thread at rest).
        layer.enabled: live && !wave
        Item {
            id: scMoving
            objectName: "EngineX_Shadow_Content"
            width: scDisplay.width; height: scDisplay.height
            // x: written by refreshPosition / XAnimator only (track coordinates inside).
            XAnimator { id: scroll; target: scMoving; easing.type: Easing.Linear }
            // Hidden stock lanes (most instances) build none of this: fewer objects and
            // bindings for every V4 collection to mark.
            Loader {
                active: scDisplay.live && scDisplay.graphical
                width: scDisplay.width; height: scDisplay.height
                sourceComponent: Component { Item {
        Image {
            visible: scDisplay.wave && source !== ""
            source: scDisplay.wave && !scDisplay.tiled && !(scDisplay.sample && scDisplay.sample.engine) ? scDisplay.waveAsset(scDisplay.imageUrl) : "" // a covered Engine slot has the overview only: not stretched over the scrolling lane
            sourceSize.width: scDisplay.overview || !scDisplay.detailed ? 1024 : 2048
            sourceSize.height: scDisplay.overview || !scDisplay.detailed ? 64 : 128
            asynchronous: true
            cache: false
            smooth: true
            fillMode: Image.Stretch
            x: scDisplay.screenX(0)
            y: 0
            width: (scDisplay.overview || !scDisplay.detailed ? scDisplay.duration : scDisplay.waveformDuration) / scDisplay.span * scDisplay.width
            height: scDisplay.height
            opacity: scDisplay.stale ? 0.55 : 1
            // Palette comes from the local stock waveform/theme, never the peer.
        }
        Repeater {
            model: scDisplay.tiled ? Math.min(32, scDisplay.tiles.length) : 0
            delegate: Image {
                required property int index
                readonly property int pixels: Math.max(0, Math.min(scDisplay.tilePixels, scDisplay.totalPixels - index * scDisplay.tilePixels))
                readonly property real startMs: index * scDisplay.tilePixels / scDisplay.totalPixels * scDisplay.waveformDuration
                readonly property real endMs: (index * scDisplay.tilePixels + pixels) / scDisplay.totalPixels * scDisplay.waveformDuration
                source: scDisplay.waveAsset(scDisplay.tiles[index])
                visible: index >= scDisplay.tileLo && index <= scDisplay.tileHi
                sourceSize.width: Math.min(2048, pixels)
                sourceSize.height: 128
                asynchronous: true; cache: false; smooth: true
                fillMode: Image.Stretch
                x: scDisplay.screenX(startMs)
                y: 0
                width: (endMs - startMs) / scDisplay.span * scDisplay.width
                height: scDisplay.height
                opacity: scDisplay.stale ? 0.55 : 1
            }
        }
        // Native overview: elapsed RGB is 60%; band mode uses the stock Past colors.
        // Clipped into non-overlapping left (past) and right (remaining) strips over the
        // 30 px height above timeBar, with the stock 2.2x / 1.3x bottom-anchored scale (6c0e5ac).
        Item {
            id: scOverviewStrip
            visible: scDisplay.overview
            clip: true
            width: scDisplay.width
            height: Math.max(2, scDisplay.height - 2)
            readonly property real splitX: Math.max(0, Math.min(scDisplay.width, scDisplay.playhead * scDisplay.width))
            readonly property real yStretch: scDisplay.half ? (scDisplay.rgb ? @RGB_SCALE@ : @BANDS_SCALE@) : 1
            Item {
                width: scOverviewStrip.splitX
                height: scOverviewStrip.height
                clip: true
                Image {
                    source: scDisplay.overview ? scDisplay.waveAsset(scDisplay.imageUrl, true) : ""
                    sourceSize.width: 1024; sourceSize.height: 64
                    width: scDisplay.width; height: scOverviewStrip.height
                    asynchronous: true; cache: false; smooth: true
                    fillMode: Image.Stretch
                    opacity: scDisplay.stale ? 0.55 : 1
                    transform: Scale { origin.y: scOverviewStrip.height; yScale: scOverviewStrip.yStretch }
                }
            }
            Item {
                anchors.right: parent.right
                width: Math.max(0, scDisplay.width - scOverviewStrip.splitX)
                height: scOverviewStrip.height
                clip: true
                Image {
                    anchors.right: parent.right
                    source: scDisplay.overview ? scDisplay.waveAsset(scDisplay.imageUrl, false) : ""
                    sourceSize.width: 1024; sourceSize.height: 64
                    width: scDisplay.width; height: scOverviewStrip.height
                    asynchronous: true; cache: false; smooth: true
                    fillMode: Image.Stretch
                    opacity: scDisplay.stale ? 0.55 : 1
                    transform: Scale { origin.y: scOverviewStrip.height; yScale: scOverviewStrip.yStretch }
                }
            }
        }
        // A fixed pool of grid lines: a new grid window (every half span of play)
        // re-reads grid[index] instead of destroying and creating every delegate,
        // which was the main JS garbage of a playing shadow deck.
        Repeater {
            model: scDisplay.gridSlots
            // The stock grid: a short grey tick at both edges on every beat (underneath
            // the waveform, z: -1); a bar has red ticks, a line between them and its
            // outlined number at the bottom left (on top of the waveform, z: 1).
            delegate: Item {
                id: scBeat
                required property int index
                readonly property var beat: index < scDisplay.grid.length ? scDisplay.grid[index] : null
                readonly property bool bar: beat !== null && beat.number % 4 === 0
                visible: beat !== null
                z: bar ? 1 : -1
                x: beat ? scDisplay.screenX(beat.ms) - 2 : 0
                width: 4; height: scDisplay.height
                Rectangle { width: 4; height: 8; color: scBeat.bar ? Style.error01Color : Style.gridlinesColor }
                Rectangle { y: scBeat.height - 8; width: 4; height: 8; color: scBeat.bar ? Style.error01Color : Style.gridlinesColor }
                Rectangle { visible: scBeat.bar; x: 1; y: 8; width: 2; height: scBeat.height - 16; color: Style.icon01Color }
                Text { textFormat: Text.PlainText; visible: scBeat.bar; anchors.right: scBeat.left; anchors.rightMargin: 5; anchors.bottom: scBeat.bottom; anchors.bottomMargin: 3; text: scBeat.bar ? scBeat.beat.number / 4 + 1 : ""; color: Style.font02Color; style: Text.Outline; styleColor: Style.surface01Color; font.pixelSize: 15; font.weight: Font.Medium }
            }
        }
        Repeater {
            model: scDisplay.loops
            delegate: Rectangle {
                required property var modelData
                readonly property real clipLeft: scDisplay.screenX(modelData.startMs)
                readonly property real clipRight: scDisplay.screenX(modelData.endMs)
                visible: clipRight > clipLeft
                x: clipLeft; width: Math.max(0, clipRight - clipLeft); height: scDisplay.height
                color: modelData.c || "#26a6ff"
                opacity: 0.18
                Text { textFormat: Text.PlainText; text: modelData.n || ""; color: "white"; font.pixelSize: 10; elide: Text.ElideRight; width: parent.width }
            }
        }
        Repeater {
            model: scDisplay.cues
            delegate: Item {
                id: scCueItem
                required property var modelData
                readonly property bool isMainCue: modelData.i === 0
                readonly property color cueColor: isMainCue ? Style.icon02Color : (modelData.c || "#26a6ff")
                readonly property string cueLabel: isMainCue ? "" : (modelData.n || "")
                x: scDisplay.screenX(modelData.ms)
                width: 0
                height: scDisplay.overview ? Math.max(2, scDisplay.height - 2) : scDisplay.height
                z: 4
                EngineXEmbedded.ColoredImage {
                    id: scCueUpper
                    objectName: "HotCueTriangle"
                    visible: scDisplay.wave
                    anchors.top: parent.top
                    anchors.horizontalCenter: parent.left
                    source: scDisplay.wave ? Qt.resolvedUrl("./img/cue.png") : ""
                    color: scCueItem.cueColor
                }
                Text {
                    objectName: "HotCueLabel"
                    visible: scDisplay.wave && text.length > 0
                    anchors.right: scCueUpper.left
                    anchors.top: parent.top
                    textFormat: Text.PlainText
                    text: scCueItem.cueLabel
                    color: Style.font02Color
                    font.pixelSize: 17
                    Rectangle {
                        z: -1
                        anchors.fill: parent
                        color: Style.surface01Color
                    }
                }
                EngineXEmbedded.ColoredImage {
                    objectName: "HotCueTriangle"
                    visible: scDisplay.wave
                    rotation: 180
                    anchors.bottom: parent.bottom
                    anchors.horizontalCenter: parent.left
                    source: scDisplay.wave ? Qt.resolvedUrl("./img/cue.png") : ""
                    color: scCueItem.cueColor
                }
                Rectangle {
                    visible: scDisplay.overview
                    anchors.horizontalCenter: parent.left
                    anchors.horizontalCenterOffset: 0.5
                    width: 3
                    height: Math.max(0, parent.height - 2)
                    color: scCueItem.cueColor
                    antialiasing: true
                }
                EngineXEmbedded.ColoredImage {
                    visible: scDisplay.overview
                    anchors.bottom: parent.bottom
                    anchors.horizontalCenter: parent.left
                    source: scDisplay.overview ? Qt.resolvedUrl("./img/icon_cue.png") : ""
                    color: scCueItem.cueColor
                    scale: scDisplay.owner && scDisplay.owner.markerScale ? scDisplay.owner.markerScale : 0.75
                    transformOrigin: Item.Bottom
                }
            }
        }
                } }
            }
        } // scMoving: only this item moves each animation frame
        Loader {
            active: scDisplay.live && scDisplay.graphical
            anchors.fill: parent
            sourceComponent: Component { Item {
        // The stock overview's minute marks (every 10 minutes above 50).
        Repeater {
            id: scMinutes
            readonly property int step: scDisplay.duration > 3000000 ? 10 : 1
            readonly property int count: "@KIND@" === "overview" && scDisplay.meta ? Math.ceil(scDisplay.duration / (60000 * step)) : 0
            model: count > scDisplay.width / 4 ? 0 : count
            delegate: Rectangle {
                required property int index
                x: Math.max(0, Math.min(index * 60000 * scMinutes.step / scDisplay.duration * scDisplay.width - width, scDisplay.width - width))
                y: scDisplay.height - height - 3
                width: 2; height: 6
                color: scMinutes.step > 1 ? Style.active01Color : Style.icon02Color
            }
        }
        Rectangle {
            objectName: "Deck_WaveformOverview_TimeBar"
            visible: scDisplay.overview && !!scDisplay.sample && !scDisplay.sample.engine
            readonly property var tv: typeof PlanckGUI !== "undefined" && PlanckGUI && PlanckGUI.views && PlanckGUI.views.timeInfoViewController && scDisplay.deck > 0 ? PlanckGUI.views.timeInfoViewController.timeInfoViews[scDisplay.deck - 1] : null
            readonly property bool showElapsed: !!(tv && tv.flipped)
            readonly property real nowX: Math.max(0, Math.min(scDisplay.playhead * scDisplay.width - 2, scDisplay.width - 4))
            x: showElapsed ? 0 : Math.min(scDisplay.width, nowX + 5)
            y: scDisplay.height - 2
            width: Math.max(0, showElapsed ? nowX - 1 : scDisplay.width - x)
            height: 2
            color: Style.icon02Color
        }
        // Stock TrackEndFlashWarning: red flash on the overview when remaining time <= TrackEndWarningTime.
        Rectangle {
            objectName: "EngineX_TrackEndWarning"
            visible: scDisplay.overview
            anchors.fill: parent
            readonly property var warnTr: scDisplay.overview ? root.scTr("/Client/Preferences/Profile/Application/TrackEndWarningTime") : null
            readonly property real warnSec: warnTr ? (parseInt(warnTr.string) || warnTr.unnormalized || 0) : 0
            readonly property real totalMs: (scDisplay.sample && scDisplay.sample.durationMs) || scDisplay.duration
            readonly property bool endWarning: scDisplay.overview && !!scDisplay.sample && scDisplay.sample.playing && !scDisplay.stale && totalMs > 0 && warnSec > 0 && totalMs - scDisplay.position > 0 && totalMs - scDisplay.position <= warnSec * 1000
            readonly property var flashTr: endWarning ? root.scTr("/Engine/Master/GlobalButtonFlashState") : null
            color: Style.systemRed01Color
            opacity: endWarning && flashTr && flashTr.state ? 0.7 : 0.0
            Behavior on opacity { OpacityAnimator { duration: 280 } }
        }
        Rectangle {
            visible: scDisplay.graphical && !!scDisplay.sample && !scDisplay.sample.engine // a track on its way has no position yet
            x: Math.max(0, Math.min(scDisplay.width - width, Math.round(scDisplay.playhead * scDisplay.width - width / 2)))
            width: scDisplay.wave ? 6 : 4; height: scDisplay.height; color: scDisplay.wave ? Style.surface01Color : Style.icon02Color
            Rectangle {
                visible: scDisplay.wave
                x: 1; width: 4; height: parent.height
                color: Style.icon02Color
            }
        }
        Text { textFormat: Text.PlainText;
            visible: !!scDisplay.sample
            anchors.right: parent.right; anchors.bottom: parent.bottom; anchors.margins: 2
            color: "#dddddd"; font.pixelSize: 10
            text: !scDisplay.sample ? "" : (scDisplay.stale ? "OFFLINE · " : "") + (scDisplay.sample.metadataOnly ? "REMOTE" : scDisplay.sample.engine || scDisplay.busyState > 0 ? "" : !scDisplay.meta ? "Waiting for source analysis" : !scDisplay.detailed && !(scDisplay.meta.detailPending && !scDisplay.overview) ? "OVERVIEW" : "")
        }
            } }
        }
        Loader {
            id: scHeaderLoader
            active: scDisplay.live && !scDisplay.graphical
            anchors.fill: parent
            sourceComponent: Component { Item {
                id: scHeader
                anchors.fill: parent
                readonly property var dv: scDisplay.owner && scDisplay.owner.deckView ? scDisplay.owner.deckView : null
                readonly property string hostState: scDisplay.owner && scDisplay.owner.parent && scDisplay.owner.parent.parent && scDisplay.owner.parent.parent.state || ""
                readonly property bool splitMain: hostState === "Split_Main" || (!!dv && dv.deckGroupMode === 1 && dv.deckSizeMode === 0)
                readonly property bool splitMini: hostState === "Split_Mini" || (!!dv && dv.deckGroupMode === 1 && dv.deckSizeMode === 1)
                readonly property bool groupedMain: !splitMain && !splitMini && (hostState === "Grouped_Main" || (!!dv && dv.deckGroupMode === 0 && dv.deckSizeMode === 0))
                readonly property bool perfSplitMain: scDisplay.performance && ((scDisplay.owner && scDisplay.owner.state === "Split_Main") || splitMain)
                readonly property bool hasBottomRow: !scDisplay.performance && !groupedMain && !splitMain && !splitMini
                readonly property bool loaded: !!scDisplay.sample
                readonly property bool isSync: !!(scDisplay.sample && scDisplay.sample.sync)
                readonly property bool isLead: !!(scDisplay.sample && (scDisplay.sample.lead !== undefined && scDisplay.sample.lead !== null ? scDisplay.sample.lead : scDisplay.sample.master))
                function tryFlipTime(x, y) {
                    if (!loaded || scDisplay.performance || !timeView || !scTimeSlot.visible || !(scTimeSlot.width > 0)) return false
                    const p = scDisplay.mapToItem(scTimeSlot, x, y)
                    if (p.x >= 0 && p.x <= scTimeSlot.width && p.y >= -12 && p.y <= scTimeSlot.height + 12) {
                        timeView.flip()
                        return true
                    }
                    return false
                }
                readonly property bool keyLock: !!(scDisplay.sample && scDisplay.sample.keylock)
                readonly property real pitchVal: scDisplay.sample ? Math.round((Number(scDisplay.sample.pitch) || 0) * 100) / 100 : 0
                readonly property string rangeStr: scDisplay.sample && scDisplay.sample.range > 0 ? String(scDisplay.sample.range) : "8"
                readonly property string deckLabel: "Deck " + scDisplay.deck
                readonly property string rawTitle: scDisplay.sample ? scDisplay.sample.title || (scDisplay.meta && scDisplay.meta.title) || scDisplay.sample.name || (scDisplay.sample.loading ? (Math.scI18n ? Math.scI18n.t("Loading…") : "Loading…") : "") : ""
                readonly property string fileName: scDisplay.sample ? scDisplay.sample.name || rawTitle : ""
                readonly property string artistName: scDisplay.sample ? scDisplay.sample.artist || (scDisplay.meta && scDisplay.meta.artist) || "" : ""
                readonly property string artUrl: (scDisplay.meta && scDisplay.meta.artworkUrl ? root.scShadowAsset(scDisplay.meta.artworkUrl) : "") || (dv && dv.artistSongView ? dv.artistSongView.albumArtPath : "")
                readonly property var timeView: typeof PlanckGUI !== "undefined" && PlanckGUI && PlanckGUI.views && PlanckGUI.views.timeInfoViewController && scDisplay.deck > 0
                    ? PlanckGUI.views.timeInfoViewController.timeInfoViews[scDisplay.deck - 1] : null
                readonly property bool timeElapsed: !!(timeView && timeView.flipped)
                readonly property var timeModeTr: root.scTr("/Client/Preferences/Profile/Application/TimeDisplayMode")
                readonly property bool timeDynamic: !!(timeModeTr && timeModeTr.index === 0)
                readonly property real totalMs: (scDisplay.sample && scDisplay.sample.durationMs) || (scDisplay.meta && scDisplay.meta.durationMs) || 0
                readonly property string timeText: {
                    if (!loaded || !(totalMs > 0)) return ""
                    const pos = Math.max(0, Math.min(totalMs, scDisplay.position))
                    const raw = timeElapsed ? pos : Math.max(0, totalMs - pos)
                    const rate = timeDynamic ? Math.max(0.01, 1 + pitchVal / 100) : 1
                    const sec = Math.floor(raw / (rate * 1000))
                    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60
                    const pfx = timeElapsed ? "" : "-"
                    return pfx + (h > 0 ? h + ":" + (m < 10 ? "0" : "") + m + ":" + (s < 10 ? "0" : "") + s : m + ":" + (s < 10 ? "0" : "") + s)
                }
                readonly property var keyModeTr: root.scTr("/Client/Preferences/Profile/Application/KeyDisplayMode")
                readonly property int keyModeIdx: keyModeTr && keyModeTr.index !== undefined ? keyModeTr.index : 3
                readonly property var colorizeKeyTr: root.scTr("/Client/Preferences/Profile/Application/ColorizeKey")
                readonly property bool colorizeKey: !colorizeKeyTr || colorizeKeyTr.state
                readonly property string rawKey: {
                    const sk = scDisplay.sample && scDisplay.sample.keyName || ""
                    if (sk === "-") return ""
                    return sk || (scDisplay.meta && scDisplay.meta.keyName) || ""
                }
                readonly property string keyText: {
                    if (!rawKey) return "-"
                    const idx = typeof Planck !== "undefined" && Planck && Planck.getKeyIndex ? Planck.getKeyIndex(rawKey) : -1
                    if (idx < 0 || idx >= 24) return rawKey
                    const tables = [
                        ["C","Am","G","Em","D","Bm","A","F#m","E","C#m","B","G#m","F#","D#m","C#","A#m","G#","Fm","D#","Cm","A#","Gm","F","Dm"],
                        ["C","Am","G","Em","D","Bm","A","Gbm","E","Dbm","B","Abm","Gb","Ebm","Db","Bbm","Ab","Fm","Eb","Cm","Bb","Gm","F","Dm"],
                        ["1d","1m","2d","2m","3d","3m","4d","4m","5d","5m","6d","6m","7d","7m","8d","8m","9d","9m","10d","10m","11d","11m","12d","12m"],
                        ["8B","8A","9B","9A","10B","10A","11B","11A","12B","12A","1B","1A","2B","2A","3B","3A","4B","4A","5B","5A","6B","6A","7B","7A"]
                    ]
                    const tbl = tables[keyModeIdx] || tables[3]
                    return tbl[idx] || rawKey
                }
                readonly property color keyColor: keyText !== "-" && colorizeKey && root && root.getColorForKey
                    ? root.getColorForKey(keyText, Style.font01Color)
                    : (keyLock ? Style.active01Color : Style.font01Color)
                RowLayout {
                    visible: !scDisplay.performance
                    anchors.fill: parent
                    spacing: scHeader.splitMain || scHeader.splitMini ? 16 : 8
                    Item {
                        Layout.fillHeight: true
                        Layout.minimumWidth: height
                        Layout.maximumWidth: height
                        Image {
                            id: scCover
                            anchors.fill: parent
                            antialiasing: true
                            fillMode: Image.PreserveAspectCrop
                            asynchronous: true
                            cache: false
                            source: scHeader.artUrl
                        }
                    }
                    ColumnLayout {
                        Layout.fillHeight: true
                        Layout.fillWidth: true
                        spacing: 0
                        RowLayout {
                            Layout.fillHeight: true
                            Layout.fillWidth: true
                            spacing: 8
                            Layout.bottomMargin: scHeader.hasBottomRow ? 4 : 0
                            ColumnLayout {
                                Layout.fillWidth: true
                                Layout.fillHeight: true
                                Layout.rightMargin: 8
                                spacing: 0
                                EngineXEmbedded.TrackDisplayText {
                                    id: scTitleDisplay
                                    respectHideTitleArtist: true
                                    filename: scHeader.fileName
                                    title: scHeader.rawTitle
                                    providerName: ""
                                }
                                Text {
                                    objectName: "Deck_Title"
                                    Layout.fillWidth: true
                                    Layout.alignment: Qt.AlignVCenter
                                    elide: Text.ElideRight
                                    font: scHeader.splitMain ? Style.fontDisplay : scHeader.groupedMain ? Style.fontH1 : Style.fontH0
                                    color: Style.font02Color
                                    text: scHeader.loaded ? (scTitleDisplay.hideTitleArtist ? "" : (scTitleDisplay.display || scHeader.rawTitle)) : scHeader.deckLabel
                                    textFormat: Text.PlainText
                                    verticalAlignment: Text.AlignVCenter
                                }
                                Text {
                                    objectName: "Deck_Artist"
                                    Layout.fillWidth: true
                                    Layout.alignment: Qt.AlignVCenter
                                    visible: !scHeader.loaded || (!scHeader.hasBottomRow && !scTitleDisplay.hideTitleArtist && !scTitleDisplay.shouldShowFilename && scHeader.artistName.length > 0)
                                    elide: Text.ElideRight
                                    font: scHeader.splitMain ? Style.fontH1 : Style.fontH3
                                    color: Style.font01Color
                                    text: scHeader.loaded ? (scTitleDisplay.hideTitleArtist || scTitleDisplay.shouldShowFilename ? "" : scHeader.artistName) : "No Track Loaded"
                                    textFormat: Text.PlainText
                                    verticalAlignment: Text.AlignVCenter
                                }
                            }
                            RowLayout {
                                Layout.preferredHeight: 26
                                spacing: 0
                                visible: scHeader.loaded
                                Layout.bottomMargin: 3
                                Layout.topMargin: 3
                                RowLayout {
                                    visible: scHeader.splitMini
                                    Layout.fillHeight: true
                                    Layout.rightMargin: 16
                                    spacing: 0
                                    AutoLoop {
                                        Layout.fillWidth: false
                                        Layout.preferredWidth: 52
                                        Layout.rightMargin: 4
                                        view: scHeader.dv ? scHeader.dv.autoLoopView : null
                                        spacing: 4
                                        iconWidth: 16
                                        iconHeight: 10
                                        textFont: Style.fontH3
                                    }
                                    BeatJump {
                                        Layout.fillWidth: false
                                        Layout.preferredWidth: 64
                                        view: scHeader.dv ? scHeader.dv.beatJumpView : null
                                        deckIndex: scDisplay.deck
                                        spacing: 4
                                        iconWidth: 16
                                        iconHeight: 10
                                        textFont: Style.fontH3
                                        visible: Config.audio.deck.hasBeatJump
                                    }
                                    EngineXEmbedded.JP07SyncIndicator {
                                        Layout.fillWidth: false
                                        Layout.preferredHeight: 24
                                        Layout.minimumWidth: 160
                                        Layout.preferredWidth: 160
                                        Layout.maximumWidth: 160
                                        Layout.leftMargin: 4
                                        Layout.rightMargin: 4
                                        deckIndex: scDisplay.deck
                                        isMaster: scHeader.isLead
                                    }
                                    Item {
                                        Layout.alignment: Qt.AlignVCenter
                                        Layout.preferredHeight: 22
                                        Layout.minimumWidth: 112
                                        Layout.preferredWidth: 112
                                        Layout.maximumWidth: 112
                                        Layout.leftMargin: 2
                                        Layout.rightMargin: 2
                                        clip: true
                                        Row {
                                            anchors.fill: parent
                                            readonly property var info: scDisplay.cueInfo
                                            visible: info !== null && root.scCueCountdown
                                            spacing: 4
                                            Rectangle { width: 3; height: 14; radius: 1; anchors.verticalCenter: parent.verticalCenter; color: parent.info ? parent.info.color : "transparent" }
                                            Text { width: Math.max(0, Math.min(Math.ceil(implicitWidth) + 2, parent.width - Math.ceil(scCueCountTextSplit.implicitWidth) - 12)); anchors.verticalCenter: parent.verticalCenter; elide: Text.ElideRight; font: Style.fontH3; color: Style.font01Color; visible: text !== "" && parent.width - Math.ceil(scCueCountTextSplit.implicitWidth) >= 28; text: parent.info ? parent.info.label : "" }
                                            Text { id: scCueCountTextSplit; anchors.verticalCenter: parent.verticalCenter; font: Style.fontH3; color: parent.info && parent.info.urgent ? "#ff9f1c" : Style.font02Color; text: parent.info ? parent.info.text : "" }
                                        }
                                    }
                                    SpeedInfo {
                                        Layout.leftMargin: 4
                                        speedState: scHeader.pitchVal
                                        speedRange: scHeader.rangeStr
                                        speedRangeTextWidth: 0
                                        speedRangeTextHorizontalAlignment: Text.AlignRight
                                        speedStateTextWidth: 0
                                        speedStateTextHorizontalAlignment: Text.AlignRight
                                        columnSpacing: 8
                                        flat: true
                                    }
                                }
                                Item {
                                    id: scTimeSlot
                                    objectName: "Deck_TimeInfo"
                                    Layout.fillHeight: true
                                    Layout.rightMargin: scHeader.splitMain ? 40 : 16
                                    implicitWidth: scTimeText.implicitWidth
                                    Text {
                                        id: scTimeText
                                        objectName: "Deck_TimeInfo_TextItem"
                                        anchors.centerIn: parent
                                        horizontalAlignment: Text.AlignHCenter
                                        verticalAlignment: Text.AlignBottom
                                        textFormat: Text.PlainText
                                        font: scHeader.splitMain ? Style.fontDisplay : scHeader.splitMini ? Style.fontTitle : Style.fontH0
                                        color: scHeader.timeText !== "" ? Style.font02Color : Style.font01Color
                                        text: scHeader.timeText
                                    }
                                }
                                Rectangle { width: 1; height: scHeader.splitMain || scHeader.splitMini ? 56 : scHeader.groupedMain ? 32 : 20; Layout.alignment: Qt.AlignVCenter; color: Style.surface03Color }
                                Item {
                                    id: scKeyItem
                                    objectName: "KeyInfo"
                                    Layout.fillHeight: true
                                    readonly property int horizontalSpacing: scHeader.splitMain ? 36 : 18
                                    readonly property int internalSpacing: 5
                                    Layout.preferredWidth: horizontalSpacing * 2 + scKeyText.width + scKeyIcon.width + 4
                                    RowLayout {
                                        anchors.horizontalCenter: parent.horizontalCenter
                                        anchors.verticalCenter: parent.verticalCenter
                                        spacing: 0
                                        Item { Layout.fillWidth: true; Layout.preferredHeight: 1 }
                                        Text {
                                            id: scKeyText
                                            text: scHeader.keyText
                                            font: scHeader.splitMain ? Style.fontDisplay : scHeader.splitMini ? Style.fontTitle : Style.fontH0
                                            Layout.alignment: Qt.AlignBaseline
                                            Layout.topMargin: scKeyMetrics.height % 2 ? 0 : 1
                                            color: scHeader.keyColor
                                            readonly property int pixelHeight: font.pixelSize
                                            TextMetrics {
                                                id: scKeyMetrics
                                                font: scKeyText.font
                                                text: scHeader.keyText
                                            }
                                        }
                                        Item { Layout.preferredWidth: scKeyItem.internalSpacing; Layout.preferredHeight: 1 }
                                        EngineXEmbedded.ColoredImage {
                                            id: scKeyIcon
                                            source: scHeader.splitMain ? "qrc:/@ICONS@/KeyLock.png" : "qrc:/@ICONS@/KeyLock2.png"
                                            color: scHeader.keyLock ? scKeyText.color : Style.disabled01Color
                                            property real scale: (scKeyText.pixelHeight * 0.75) / src.implicitHeight
                                            implicitHeight: src.implicitHeight * scale
                                            implicitWidth: src.implicitWidth * scale
                                            Layout.bottomMargin: scKeyIcon.implicitHeight % 2 ? 2 : 3
                                        }
                                        Item { Layout.fillWidth: true; Layout.preferredHeight: 1 }
                                    }
                                }
                                Rectangle { width: 1; height: scHeader.splitMain || scHeader.splitMini ? 56 : scHeader.groupedMain ? 32 : 20; Layout.alignment: Qt.AlignVCenter; color: Style.surface03Color }
                                Item {
                                    objectName: "PerformanceView_Deck_BpmInfo"
                                    Layout.leftMargin: 16
                                    Layout.preferredWidth: scHeader.splitMain ? 184 : scHeader.splitMini ? 148 : scHeader.groupedMain ? 104 : 88
                                    Layout.minimumWidth: scShadowBpm.displayWidth
                                    Layout.fillHeight: true
                                    Item {
                                        id: scShadowBpmSlot
                                        width: scShadowBpm.displayWidth
                                        height: scShadowBpm.implicitHeight
                                        anchors.centerIn: parent
                                        EngineXEmbedded.Bpm {
                                            id: scShadowBpm
                                            deckIndex: scDisplay.deck
                                            deckLoaded: scHeader.loaded
                                            songAnalyzed: bpmValue > 0
                                            decimalPlaces: 2
                                            bpmValue: {
                                                const s = scDisplay.sample, m = scDisplay.meta
                                                return s ? s.currentBpm || (s.bpm || (m && m.bpm) || 0) * (1 + (s.pitch || 0) / 100) : 0
                                            }
                                            fractionSeparator: "."
                                            emptyIntegral: "-"
                                            animateWhenUnanalysed: false
                                            font: Style.makeFont(Style.fontDisplay.family, scHeader.splitMain ? 72 : scHeader.splitMini ? 48 : 36, Font.Normal)
                                            fractionFont: font
                                            color: scHeader.isSync ? Style.active01Color : Style.font02Color
                                            fractionColor: color
                                            fractionalBottomMargin: 0
                                        }
                                    }
                                }
                            }
                        }
                        RowLayout {
                            Layout.fillWidth: true
                            spacing: 2
                            visible: scHeader.loaded && scHeader.hasBottomRow
                            AutoLoop {
                                Layout.fillWidth: false
                                Layout.alignment: Qt.AlignLeft
                                Layout.preferredWidth: 52
                                view: scHeader.dv ? scHeader.dv.autoLoopView : null
                                spacing: 4
                                iconWidth: 16
                                iconHeight: 10
                                textFont: Style.fontH3
                            }
                            BeatJump {
                                Layout.fillWidth: false
                                view: scHeader.dv ? scHeader.dv.beatJumpView : null
                                deckIndex: scDisplay.deck
                                spacing: 4
                                iconWidth: 16
                                iconHeight: 10
                                textFont: Style.fontH3
                                Layout.preferredWidth: 64
                                Layout.alignment: Qt.AlignLeft
                                visible: Config.audio.deck.hasBeatJump
                            }
                            Rectangle { width: 1; height: 18; Layout.alignment: Qt.AlignVCenter; color: Style.surface03Color }
                            EngineXEmbedded.JP07SyncIndicator {
                                Layout.preferredHeight: 24
                                Layout.fillWidth: false
                                Layout.minimumWidth: 135
                                Layout.preferredWidth: 135
                                Layout.maximumWidth: 135
                                Layout.alignment: Qt.AlignCenter
                                deckIndex: scDisplay.deck
                                isMaster: scHeader.isLead
                            }
                            Rectangle { width: 1; height: 18; Layout.alignment: Qt.AlignVCenter; color: Style.surface03Color }
                            Item {
                                Layout.alignment: Qt.AlignVCenter
                                Layout.preferredHeight: 22
                                Layout.fillWidth: true
                                Layout.minimumWidth: 50
                                Layout.preferredWidth: 100
                                Layout.leftMargin: 3
                                Layout.rightMargin: 2
                                clip: true
                                Row {
                                    objectName: "EngineX_ShadowCueCountdown"
                                    anchors.fill: parent
                                    readonly property var info: scDisplay.cueInfo
                                    visible: info !== null && root.scCueCountdown
                                    spacing: 4
                                    Rectangle { width: 3; height: 14; radius: 1; anchors.verticalCenter: parent.verticalCenter; color: parent.info ? parent.info.color : "transparent" }
                                    Text { width: Math.max(0, Math.min(Math.ceil(implicitWidth) + 1, parent.width - Math.ceil(scCueCountText1.implicitWidth) - 11)); anchors.verticalCenter: parent.verticalCenter; elide: Text.ElideRight; font: Style.fontH3; color: Style.font01Color; visible: text !== "" && parent.width - Math.ceil(scCueCountText1.implicitWidth) >= 24; text: parent.info ? parent.info.label : "" }
                                    Text { id: scCueCountText1; anchors.verticalCenter: parent.verticalCenter; font: Style.fontH3; color: parent.info && parent.info.urgent ? "#ff9f1c" : Style.font02Color; text: parent.info ? parent.info.text : "" }
                                }
                            }
                            SoftTakeoverIndicators {
                                iconWidth: 14
                                iconHeight: 10
                                flow: Flow.LeftToRight
                                spacing: -4
                                speedOffsetIsUp: false
                                speedOffsetIsDown: false
                            }
                            SpeedInfo {
                                Layout.fillHeight: true
                                speedState: scHeader.pitchVal
                                speedRange: scHeader.rangeStr
                                speedRangeTextFont: Style.fontH1
                                speedRangeTextWidth: 0
                                speedRangeTextHorizontalAlignment: Text.AlignRight
                                speedStateTextFont: Style.fontH1
                                speedStateTextWidth: 0
                                speedStateTextHorizontalAlignment: Text.AlignRight
                                textLayoutAlignment: Qt.AlignRight | Qt.AlignVCenter
                                columnSpacing: 8
                                flat: true
                            }
                        }
                    }
                }
                RowLayout {
                    visible: scDisplay.performance && scHeader.loaded
                    anchors.fill: parent
                    spacing: 0
                    AutoLoop {
                        Layout.fillWidth: false
                        Layout.fillHeight: true
                        Layout.preferredWidth: 52
                        Layout.rightMargin: 4
                        view: scHeader.dv ? scHeader.dv.autoLoopView : null
                        spacing: 4
                        iconWidth: 16
                        iconHeight: 10
                        textFont: Style.fontH3
                    }
                    BeatJump {
                        Layout.fillWidth: false
                        Layout.fillHeight: true
                        Layout.preferredWidth: 64
                        view: scHeader.dv ? scHeader.dv.beatJumpView : null
                        deckIndex: scDisplay.deck
                        spacing: 4
                        iconWidth: 16
                        iconHeight: 10
                        textFont: Style.fontH3
                        visible: Config.audio.deck.hasBeatJump
                    }
                    Rectangle { Layout.fillHeight: true; Layout.topMargin: 2; Layout.bottomMargin: 2; width: 1; color: Style.surface03Color; visible: !scHeader.perfSplitMain }
                    Item { Layout.fillWidth: true; visible: scHeader.perfSplitMain }
                    EngineXEmbedded.JP07SyncIndicator {
                        Layout.fillWidth: false
                        Layout.preferredHeight: 24
                        Layout.minimumWidth: 164
                        Layout.preferredWidth: 164
                        Layout.maximumWidth: 164
                        Layout.leftMargin: 6
                        Layout.rightMargin: 6
                        Layout.alignment: Qt.AlignCenter
                        leadTextFont: scHeader.perfSplitMain ? Style.fontTitle : Style.fontH3
                        deckIndex: scDisplay.deck
                        isMaster: scHeader.isLead
                    }
                    Rectangle { Layout.fillHeight: true; Layout.topMargin: 2; Layout.bottomMargin: 2; width: 1; color: Style.surface03Color; visible: !scHeader.perfSplitMain }
                    Item {
                        Layout.alignment: Qt.AlignVCenter
                        Layout.preferredHeight: 22
                        Layout.minimumWidth: 112
                        Layout.preferredWidth: 112
                        Layout.maximumWidth: 112
                        Layout.leftMargin: 2
                        Layout.rightMargin: 2
                        clip: true
                        Row {
                            objectName: "EngineX_ShadowCueCountdown"
                            anchors.fill: parent
                            readonly property var info: scDisplay.cueInfo
                            visible: info !== null && root.scCueCountdown
                            spacing: 4
                            Rectangle { width: 3; height: 14; radius: 1; anchors.verticalCenter: parent.verticalCenter; color: parent.info ? parent.info.color : "transparent" }
                            Text { width: Math.max(0, Math.min(Math.ceil(implicitWidth) + 2, parent.width - Math.ceil(scCueCountText2.implicitWidth) - 12)); anchors.verticalCenter: parent.verticalCenter; elide: Text.ElideRight; font: Style.fontH3; color: Style.font01Color; visible: text !== "" && parent.width - Math.ceil(scCueCountText2.implicitWidth) >= 24; text: parent.info ? parent.info.label : "" }
                            Text { id: scCueCountText2; anchors.verticalCenter: parent.verticalCenter; font: Style.fontH3; color: parent.info && parent.info.urgent ? "#ff9f1c" : Style.font02Color; text: parent.info ? parent.info.text : "" }
                        }
                    }
                    Item { Layout.fillWidth: true }
                    SoftTakeoverIndicators {
                        Layout.alignment: Qt.AlignCenter
                        Layout.leftMargin: 4
                        Layout.rightMargin: scHeader.perfSplitMain ? 6 : 4
                        iconWidth: scHeader.perfSplitMain ? 24 : 16
                        iconHeight: scHeader.perfSplitMain ? 16 : 12
                        flow: Flow.LeftToRight
                        spacing: -4
                        speedOffsetIsUp: false
                        speedOffsetIsDown: false
                    }
                    SpeedInfo {
                        Layout.fillHeight: true
                        speedState: scHeader.pitchVal
                        speedRange: scHeader.rangeStr
                        speedRangeTextFont: scHeader.perfSplitMain ? Style.fontTitle : Style.fontH1
                        speedRangeTextWidth: 0
                        speedRangeTextColor: scHeader.perfSplitMain ? Style.font02Color : Style.font01Color
                        speedRangeTextHorizontalAlignment: Text.AlignRight
                        speedStateTextFont: scHeader.perfSplitMain ? Style.fontTitle : Style.fontH1
                        speedStateTextWidth: 0
                        speedStateTextColor: Style.font02Color
                        speedStateTextHorizontalAlignment: Text.AlignRight
                        textLayoutAlignment: Qt.AlignCenter
                        separatorHeight: scHeader.perfSplitMain ? 32 : 0
                        columnSpacing: scHeader.perfSplitMain ? 12 : 8
                        flat: true
                    }
                }
            } }
        }
@BUSY@        // Covers remote Stems, needle-drop, grid editing and transport touch targets;
        // only presses inside the shadow header's time display flip Elapsed/Remaining.
        MouseArea { objectName: "EngineX_ShadowBlocker"; anchors.fill: parent; acceptedButtons: Qt.AllButtons; preventStealing: true; function handlePress(x, y) { return !scDisplay.graphical && typeof scHeaderLoader !== "undefined" && scHeaderLoader.item ? scHeaderLoader.item.tryFlipTime(x, y) : false } onPressed: function(mouse) { handlePress(mouse.x, mouse.y) }; onWheel: function(wheel) { wheel.accepted = true } }
    }
"""

# Waveform lanes only. The stock download indicator (Deck/DownloadProgress.qml):
# its ring, its colours and font, one line beside the ring instead of below (a
# lane is 60-100 px high). The text is one text: line without comparisons, so the
# language packs translate it.
SHADOW_BUSY = r"""
        Loader {
            id: scBusy
            // Auto mode, a CDJ this renderer draws because Engine cannot have its file
            // (sample.fallback, cdjFallback in stream-sync): the same line says why, for
            // 8 s per track, and for as long as the lane has no waveform to draw.
            readonly property string why: scDisplay.busyState === 0 && scDisplay.live && scDisplay.sample && !scDisplay.sample.stale && scDisplay.sample.fallback || ""
            readonly property string whyKey: why === "" ? "" : why + scDisplay.sample.key
            property string told: ""
            Timer { interval: 8000; running: scDisplay.visible && scBusy.whyKey !== "" && scBusy.whyKey !== scBusy.told; readonly property string key: scBusy.whyKey; onKeyChanged: if (running) restart(); onTriggered: scBusy.told = scBusy.whyKey }
            active: scDisplay.busyState > 0 || why !== "" && (told !== whyKey || scDisplay.sample.metadataOnly)
            // Beside the stock playhead line (drawn over the lane), on its wider side.
            readonly property real line: scDisplay.playhead * scDisplay.width
            readonly property real room: Math.max(line, scDisplay.width - line) - 24
            x: line > scDisplay.width / 2 ? 12 : line + 12
            anchors.verticalCenter: parent.verticalCenter
            sourceComponent: Component { Row {
                objectName: "EngineX_Shadow_Busy"
                spacing: 8
                transformOrigin: Item.Left
                scale: Math.min(1, scDisplay.height / 40, scBusy.room / width)
@RING@                Text { textFormat: Text.PlainText
                    anchors.verticalCenter: parent.verticalCenter
                    color: Style.font02Color; font: Style.fontH1
                    readonly property int retried: scDisplay.busyState > 3 || ["network", "timeout"].indexOf(scBusy.why) >= 0 ? 1 : 0
                    text: (["Downloading", "Loading", "Waiting for source analysis", "File unavailable.", "Failed to open file within appropriate time.", "Could not load via network."][scDisplay.busyState - 1] || {streaming: "Streaming: no file to copy from CDJ.", link: "rekordbox Link: no file to copy from CDJ.", media: "CD or 2nd USB: no file to copy from CDJ.", network: "Could not load via network.", timeout: "Failed to open file within appropriate time."}[scBusy.why] || " ") + [" ", " " + "Retrying…"][retried]
                }
            } }
        }
"""

# A known share fills the ring; without one (no Content-Length, Engine loading) a
# quarter turns. A failed copy shows the text alone.
SHADOW_RING = r"""
                Item {
                    width: 32; height: 32
                    visible: scDisplay.busyState > 0 && scDisplay.busyState < 4
                    readonly property real share: scDisplay.sample ? scDisplay.sample.progress : 0
                    ScStock.ProgressRing {
                        anchors.fill: parent; visible: parent.share > 0
                        progress: parent.share
                        backgroundColor: Style.surface03Color; ringColor: Style.font02Color; strokeWidth: 2
                    }
                    ScStock.ProgressRing {
                        id: scTurning
                        anchors.fill: parent; visible: !(parent.share > 0)
                        progress: 0.25
                        backgroundColor: Style.surface03Color; ringColor: Style.font02Color; strokeWidth: 2
                        RotationAnimator on rotation { running: scTurning.visible; from: 0; to: 360; duration: 1000; loops: Animation.Infinite }
                    }
                }
"""


def shadow_layer(text: str, kind: str, ring: bool = False) -> str:
    """Keep visual items inside the stock stacking context, outside Row/ColumnLayouts.

    ring: this QRC ships the stock ProgressRing (5.3.0+)."""
    # Stock roots request QtQuick 2.0/2.1; Matrix4x4 and Binding.restoreMode
    # need the newer revisions available in both supported Engine Qt builds.
    ring = ring and kind == "waveform"
    text = re.sub(r"(?m)^import QtQuick(?: \d+\.\d+)?\s*$", "import QtQuick 2.15\nimport Embedded as EngineXEmbedded" + ("\nimport EngineOS.UI.Controls 1.0 as ScStock" if ring else ""), text)
    text = re.sub(r"(?m)^import QtQuick.Layouts(?: \d+\.\d+)?[ \t]*$", "import QtQuick.Layouts 1.15", text)
    if not re.search(r"(?m)^import QtQuick.Layouts(?: \d+\.\d+)?[ \t]*$", text):
        text = "import QtQuick.Layouts 1.15\n" + text
    root_object = re.search(r"(?m)^[A-Za-z_]\w*(?:\.\w+)*\s*\{", text)
    if root_object is None:
        raise ValueError("shadow display target has no QML root object")
    owner = re.search(r"(?m)^(?:    |\t)id:\s*([A-Za-z_]\w*)\s*$", text)
    if owner:
        owner_id = owner.group(1)
    else:
        owner_id = "scStockShadowHost"
        pos = root_object.end()
        text = text[:pos] + "\n    id: " + owner_id + "\n" + text[pos:]
    end = text.rfind("}")
    if end < 0:
        raise ValueError("shadow display target has no QML root")
    palette = []
    for band, prop in (("Low", "lowColor"), ("Mid", "midColor"), ("High", "highColor")):
        stock = f"waveform{band}{'Fade' if kind == 'waveform' else ''}Color"
        colors = BAND_FADE if kind == "waveform" else BAND_COLORS
        fallback = f'(rgb ? "{RGB_COLORS[band]}" : {three_band(stock, band, colors)})'
        # 3-band lanes take the stock main colours. height*Color is the Fade set there
        # (45-60 % alpha): drawn alone, the whole lane looked greyed out and translucent.
        value = f'(!rgb && typeof owner.color{band}Color !== "undefined" ? owner.color{band}Color : typeof owner.height{band}Color !== "undefined" ? owner.height{band}Color : {fallback})' if kind == "waveform" else fallback
        if kind not in ("waveform", "overview"):
            value = '"#000000"'
        palette.append(f"readonly property color {prop}: {value}")
        past = three_band(f"waveform{band}PastColor", band, BAND_PAST) if kind in ("waveform", "overview") else '"#000000"'
        palette.append(f"readonly property color past{band}Color: {past}")
    # The stock overview RGB QSB hardcodes these same WaveFormItem defaults.
    for prop, native, default in (("saturation", "saturationRgbWave", 1.612), ("brightAmp", "brightAmpFactorRgbWave", 0.473), ("brightGain", "brightGainRgbWave", 2.405), ("brightFloor", "brightFloorRgbWave", 0.197)):
        value = f'(typeof owner.{native} !== "undefined" ? owner.{native} : {default})' if kind == "waveform" else str(default)
        palette.append(f"readonly property real {prop}: {value}")
    busy = SHADOW_BUSY.lstrip("\n").replace("@RING@", SHADOW_RING.lstrip("\n") if ring else "") if kind == "waveform" else ""
    icons = "Skins/UIKit/Icons" if v53() else "UIKit.1/UIKit/Icons"
    raw_layer = SHADOW_LAYER
    if kind in ("waveform", "overview"):
        h_start = raw_layer.index("        Loader {\n            id: scHeaderLoader\n            active: scDisplay.live && !scDisplay.graphical\n")
        h_end = raw_layer.index("@BUSY@")
        raw_layer = raw_layer[:h_start] + raw_layer[h_end:]
    layer = raw_layer.replace("@BUSY@", busy).replace("@KIND@", kind).replace("@OWNER@", owner_id).replace("@PALETTE@", "\n        ".join(palette)).replace("@ICONS@", icons).replace("@RGB_SCALE@", str(OVERVIEW_Y_SCALE_RGB)).replace("@BANDS_SCALE@", str(OVERVIEW_Y_SCALE_BANDS))
    return text[:end] + layer + text[end:]


def shadow_targets(qrc: Path):
    """Stock visual roots (5.0.4 and 5.3); only patch files actually shipped by this QRC."""
    v = ROOT + "Views/"
    composite = "Skins/EngineOS/UI/CompositeControls/"
    targets = {
        v + "Common/Waveform/Waveform.qml": "waveform",
        v + "Common/WaveformOverview/WaveformOverview.qml": "overview",
        composite + "DeckTrackData/GroupedMiniDeckTrackDataContent.qml": "track",
        composite + "DeckTrackData/GroupedMainDeckTrackDataContent.qml": "track",
        composite + "DeckTrackData/SplitMiniDeckTrackDataContent.qml": "track",
        composite + "DeckTrackData/SplitMainDeckTrackDataContent.qml": "track",
        composite + "DeckTrackData/DeckTrackDataContent.qml": "track",
        composite + "DeckPerformanceData/DeckPerformanceDataContent.qml": "performance",
        composite + "DeckPerformanceData/PortraitDeckPerformanceDataContent.qml": "performance",
    }
    return {rel: kind for rel, kind in targets.items() if (qrc / rel).is_file()}


def cue_capsule(ind: str, deck_expr: str, h: int, px: int, compact: bool = False, lead_sep: bool = True, fill_width: bool = False) -> str:
    """Bound the countdown after LEAD; only its cue name may elide."""
    sizing = ("    Layout.fillWidth: true\n    Layout.minimumWidth: 50\n    Layout.preferredWidth: 100"
              if fill_width else
              "    Layout.minimumWidth: 112\n    Layout.preferredWidth: 112\n    Layout.maximumWidth: 112")
    q = f"""Item {{
    // Reserve the Cue slot even without a cue: LEAD never changes width.
    Layout.alignment: Qt.AlignVCenter
    Layout.preferredHeight: 22
{sizing}
    Layout.leftMargin: 3
    Layout.rightMargin: 2
    clip: true
    Row {{
        id: cueCap
        objectName: "EngineX_CueCountdown"
        anchors.fill: parent
        readonly property var info: typeof root !== "undefined" && root && root.scCueCountdown && root.scNextCueInfo ? root.scNextCueInfo({deck_expr}, root.scCueTickFor({deck_expr})) : null
        visible: info !== null && root.scCueCountdown
        spacing: 4
{'        Rectangle { width: 1; height: 18; anchors.verticalCenter: parent.verticalCenter; color: Style.surface03Color }' if lead_sep else ''}
        Rectangle {{ width: 3; height: 14; radius: 1; anchors.verticalCenter: parent.verticalCenter; color: cueCap.info ? cueCap.info.color : "transparent" }}
        Text {{ width: Math.max(0, Math.min(Math.ceil(implicitWidth) + 1, parent.width - Math.ceil(cueCountText.implicitWidth) - 11)); anchors.verticalCenter: parent.verticalCenter; elide: Text.ElideRight; font: Style.fontH3; color: Style.font01Color; visible: text !== "" && parent.width - Math.ceil(cueCountText.implicitWidth) >= 24; text: cueCap.info ? cueCap.info.label : "" }}
        Text {{ id: cueCountText; anchors.verticalCenter: parent.verticalCenter; font: Style.fontH3; color: cueCap.info && cueCap.info.urgent ? "#ff9f1c" : Style.font02Color; text: cueCap.info ? cueCap.info.text : "" }}
    }}
}}
"""
    return "".join(ind + l + "\n" if l else "\n" for l in q.splitlines())


def art_retry(ind, deck, path, image):
    """Keep retry state and its Timer on the Image, outside PropertyChanges."""
    lines = [
        "property int scArtTick: 0",
        "Timer {",
        "    interval: 500; repeat: true",
        f"    running: {image}.visible && {GUARD} && root.scDeckArtPending({deck}, {path}, {image}.source)",
        f"    onTriggered: {image}.scArtTick = ({image}.scArtTick + 1) & 0xffff",
        "}",
    ]
    return "".join(ind + line + "\n" for line in lines)


def art_source(ind, deck, path, metadata, image, add_id=False):
    """An art lookup follows its deck's metadata, independently of playing/cue ticks."""
    identity = f"{ind}id: {image}\n" if add_id else ""
    return identity + art_retry(ind, deck, path, image) + (
        f"{ind}source: {GUARD} ? root.scDeckArtUrl({deck}, {path}, {image}.scArtTick, {metadata}) : {path}\n")


class PatchDict(dict):
    def __setitem__(self, k, v):
        if k in self:
            old = self[k]
            if not (isinstance(old, list) and isinstance(v, list) and v[:len(old)] == old):
                raise ValueError(f"duplicate patch key: {k}")
        super().__setitem__(k, v)


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
        # A shadow deck draws its source's slip line: the source's slip timeline in samples, 0 while slip is not active.
        ("    readonly property bool isSlipModeActive: !isRemoteDeck && pSlipActive && pSlipActive.translator\n        ? pSlipActive.translator.value\n        : false\n",
         '    readonly property int scSlip: typeof root !== "undefined" && root && root.scShadowHeadOf ? root.scShadowHeadOf(deckIndex).slip || 0 : 0\n'
         "    readonly property bool isSlipModeActive: scSlip > 0 || (!isRemoteDeck && pSlipActive && pSlipActive.translator\n        ? pSlipActive.translator.value\n        : false)\n"),
        ("    readonly property int slipModePosition: isSlipModeActive && pSlipModePosition",
         "    readonly property int slipModePosition: scSlip > 0 ? scSlip : isSlipModeActive && pSlipModePosition"),
        ("				bottomMargin: 7\n", "				bottomMargin: 0\n"),
        ("		objectName: \"Deck_WaveformOverview_ViewContainer\"\n",
         "		objectName: \"Deck_WaveformOverview_ViewContainer\"\n		clip: true\n"),
        ("			visible: waveformOverview.songLoaded && !waveformOverview.isRemoteDeck\n",
         "			transform: Scale { origin.y: overview.height; yScale: root.scOverviewScale(overview.trackData, overview.rgbMode) }\n"
         "			visible: waveformOverview.songLoaded && !waveformOverview.isRemoteDeck\n"),
    ]
    main = DEV_HARNESS if dev else ""
    services = (SERVICES_MAIN + FX_CONTROLLER).replace("@ICONS@", "Skins/UIKit/Icons" if v53() else "UIKit.1/UIKit/Icons").replace("@RGB_SCALE@", str(OVERVIEW_Y_SCALE_RGB)).replace("@BANDS_SCALE@", str(OVERVIEW_Y_SCALE_BANDS)) + (SMART_SYNC_MAIN if v53() else "")
    out = PatchDict({
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
            # The actual main view has finished loading and the stock splash has
            # disappeared. Do not equate resource registration with UI readiness.
            ('objectName: "SplashScreenLoader"', 'objectName: "SplashScreenLoader"' + BOOT_READY),
            (f"	Connections {{\n		target: {window()}\n", services + main + ZOOM_PROP + "\n	Rectangle { anchors.fill: parent; z: -1000; color: \"#000000\" } // UI Scale: no window colour round the edges\n" + f"\n	Connections {{\n		target: {window()}\n"),
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
            # #175: the shutdown overlay must cover the enlarged logical canvas too.
            (f"\t\t\tparent: Overlay.overlay ? Overlay.overlay : uiContainer\n"
             f"\t\t\twidth: {window()}.defaultScreenSize.width\n"
             f"\t\t\theight: {window()}.defaultScreenSize.height\n",
             "\t\t\tparent: Overlay.overlay ? Overlay.overlay : uiContainer\n"
             "\t\t\tanchors.fill: parent\n"),
        ],
        # #175: natural PNG dimensions shrink with UI Scale; fill the logical canvas.
        # Keep the stock geometryTag and multi-screen image selection for every panel.
        ROOT + "Items/SplashScreen.qml": [
            ("\t\tImage {\n\t\t\tsource:",
             "\t\tImage {\n\t\t\tanchors.fill: parent\n\t\t\tsource:"),
            ("\t\t\tid: overlay\n", "\t\t\tid: overlay\n\t\t\tanchors.fill: parent\n"),
        ],
        v + "Common/Waveform/Waveform.qml": wave,
        # EngineX: Denon firmware would replace the whole mod, so its prompt never
        # shows; the Update page checks the GitHub repository instead.
        ROOT + "Items/MessageBoxes/FirmwareUpdateAvailableMessageBox.qml": [
            ("	onUpdateAvailableChanged: {\n", "	onUpdateAvailableChanged: {\n		return\n")],
        prefs + "Entries/Version.qml": [(
            "	text: `Engine OS ${versionString} <font color=\\\"${grey}\\\">(${gitHashString})</font>`",
            "	wrapMode: Text.WordWrap\n"
            "	Layout.preferredHeight: implicitHeight\n"
            "	text: `EngineX OS ${root.scVersion || versionString}${root.scAboutBuildTag() ? ` · ${root.scAboutBuildTag()}` : \"\"} <font color=\\\"${grey}\\\">(github.com/${root.scUpdateRepo})</font>${root.scAccessCode ? `<br><small>${root.scT(\"Web Console\")} ${root.scConsoleUrl}<br>${root.scT(\"Access Code\")} ${root.scAccessCode}${!root.scIsAZ04 && root.scLanStems ? `<br>${String(root.scLanStems).replace(/&/g, \"&amp;\").replace(/</g, \"&lt;\").replace(/>/g, \"&gt;\")}` : \"\"}</small>` : \"\"}`")],
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
            ("Config.device.hasStemsProEmbeddedRendering && !isCurrentDeviceNetworkDrive" if v53() else "Config.device.hasStemsProEmbeddedRendering",
             '(typeof root !== "undefined" && root && root.scIsAZ04 ? (' + ("Config.device.hasStemsProEmbeddedRendering && !isCurrentDeviceNetworkDrive" if v53() else "Config.device.hasStemsProEmbeddedRendering") + ') : true)'),
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
        prefs + "Entries/LinkBeatJumpLoopSize.qml": [("active: Config.audio.deck.beatJump.canLinkToAutoLoop", "active: true")],
        # Same gate as above: show the Stems List switch and the per-track stems icon on armhf; keep AZ04 stock.
        prefs + "Entries/StemsList.qml": [("active: Planck.hasStemsPreRenderedPlayback()", 'active: Planck.hasStemsPreRenderedPlayback() || !(typeof root !== "undefined" && root && root.scIsAZ04)')],
        v + "BrowseView/Components/Components/StemsIcon.qml": [(
            "visible: (Planck.hasStemsPreRenderedPlayback() || (!!row.modelData.isDemoTrack && Config.device.hasDemoContentPreRenderedStems) ) && row.modelData.stems !== 0",
            'visible: (typeof root !== "undefined" && root && root.scIsAZ04 ? (Planck.hasStemsPreRenderedPlayback() || (!!row.modelData.isDemoTrack && Config.device.hasDemoContentPreRenderedStems)) : true) && row.modelData.stems !== 0')],
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
            ("		PlayerNumber { },\n", LANGUAGE_ENTRY + LED_ENTRY + (POWER_ENTRY if v53() else "") + "		PlayerNumber { },\n"),
        ],
        **{prefs + ("" if v53() else "TabContents/") + f"{name}TabContent.qml": [
            (f'import "{"" if v53() else "../"}Entries"\n', f'import QtQuick.Layouts 1.12\nimport "{"" if v53() else "../"}Entries"\nimport "{"" if v53() else "../"}Entries/Base" as EntryBase\nimport Planck 1.0\n'),
            (f"{last} {{}}", f"{last} {{}}," + entries.rstrip().rstrip(",")),
        ] for name, last, entries in [
            ("Services", "SpotifyEnable" if v53() else "CloudServices", SERVICES_ENTRIES),
            ("Playback", "KeySyncMode", PLAYBACK_ENTRIES),
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
\t\t\t\tvisible: root.scHasMixer
\t\t\t\treadonly property var vfx: PlanckGUI.views.transitionFx
\t\t\t\tvalues: ["Off", "All Faders", "Upfader Only"]
\t\t\t\tcurrentValue: vfx ? (!vfx.enabled ? "Off" : (vfx.triggerSource === 1 ? "Upfader Only" : "All Faders")) : (root.scUpfaderEcho ? "Upfader Only" : "Off")
\t\t\t\tonSelected: function(index) {
\t\t\t\t\tconst on = index !== 0
\t\t\t\t\troot.scUpfaderEcho = on
\t\t\t\t\troot.scSetTransitionFx(vfx, on, index === 2)
\t\t\t\t\troot.scCall("upfaderEcho=" + (on ? "1" : "0"))
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
            ("		DayModeEnable { }\n", "		DayModeEnable { }\n" + WAVE_TILE.replace("@LOW@", BAND_COLORS["Low"]).replace("@MID@", BAND_COLORS["Mid"]).replace("@HIGH@", BAND_COLORS["High"]) + UPFADER_ECHO_TILE + (SYNC_TILE if v53() else "")),
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
                (
                    "		visible: Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string === CollectionCategoryPlanck\n"
                    "				|| Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                    "		visible: Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string === CollectionCategoryPlanck\n"
                    "				|| isStreamingProviderWithSort\n"
                    "				|| Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                ) if not v53() else (
                    "    visible: Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string ===\n"
                    "            CollectionCategoryPlanck ||\n"
                    "        Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string ===\n"
                    "            CollectionCategoryOfflineLibrary ||\n"
                    "        Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                    "    visible: Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string ===\n"
                    "            CollectionCategoryPlanck ||\n"
                    "        Planck.getProperty(\"/Client/Librarian/CollectionCategories\").translator.string ===\n"
                    "            CollectionCategoryOfflineLibrary ||\n"
                    "        isStreamingProviderWithSort ||\n"
                    "        Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                )
            ),
            (
                "    readonly property bool isStreamingProviderWithSort: collectionController.isCurrentCollectionStreaming &&\n"
                "        Planck.librarianController.streamingInterfaceController.hasSortMenu\n",
                "    readonly property bool isStreamingProviderWithSort: Boolean(collectionController && collectionController.isCurrentCollectionStreaming)\n",
            ),
            (
                (
                    "		property var menuRoles: {\n"
                    "			if(showButton) {\n"
                    "				var result = [",
                    "		property var menuRoles: {\n"
                    "			if(showButton) {\n"
                    '				if (isStreamingProviderWithSort) return showPlayOrderRole ? ["song", "bpm", "artist", "key", "rating", "length", "playOrder"] : ["song", "bpm", "artist", "key", "rating", "length"]\n'
                    "				var result = [",
                ) if not v53() else (
                    "    property var menuRoles: {\n"
                    "        if (showButton) {\n"
                    "            var result = [\n",
                    "    property var menuRoles: {\n"
                    "        if (showButton) {\n"
                    '            if (isStreamingProviderWithSort) return showPlayOrderRole ? ["song", "bpm", "artist", "key", "rating", "length", "playOrder"] : ["song", "bpm", "artist", "key", "rating", "length"]\n'
                    "            var result = [\n",
                )
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
                "        scPostSortSettings()\n"
                "    }\n"
                "    onSortOrderChanged: {\n"
                "        scPostSortSettings()\n"
                "    }\n"
                "    function scPostSortSettings() {\n"
                "        if (!isStreamingProviderWithSort) return\n"
                "        try {\n"
                "            var scReq = new XMLHttpRequest()\n"
                '            scReq.open("POST", "http://127.0.0.1:8080/v1/sc5000/ui", true)\n'
                '            scReq.setRequestHeader("Content-Type", "application/json")\n'
                "            scReq.send(JSON.stringify({\n"
                '                "streamSortRole": sortRole === "playOrder" ? "default" : sortRole,\n'
                '                "streamSortOrder": sortOrder === Qt.DescendingOrder ? "desc" : "asc"\n'
                "            }))\n"
                "        } catch (e) {}\n"
                "    }\n",
            ),
            (
                "        dialog.collectionController = sortRow.collectionController\n",
                "        dialog.collectionController = sortRow.collectionController\n"
                "        dialog.sortRow = sortRow\n",
            ),
        ],
        v + "BrowseView/Components/SortMenu/SortMenuDialog.qml": [
            (
                "    property AbstractCollectionController collectionController\n"
                "    property SortControllerInterface sortController: PlanckGUI.views.sortController\n",
                "    property AbstractCollectionController collectionController\n"
                "    property var sortRow: null\n"
                "    property SortControllerInterface sortController: PlanckGUI.views.sortController\n",
            ),
            (
                "                        collectionController.sortColumn(cell.role, sortOrder, false)\n"
                "                        onScreen = false\n",
                "                        if (sortRow && sortRow.isStreamingProviderWithSort) {\n"
                "                            sortRow.sortRole = cell.role\n"
                "                        }\n"
                "                        collectionController.sortColumn(cell.role, sortOrder, false)\n"
                "                        onScreen = false\n",
            ),
        ],
        v + "BrowseView/Components/Components/SortRole.qml": [(
            "        text: parent.displayData\n",
            '        text: root.scDualColumn && (row.modelData.bpm || "") !== "" ? ((row.modelData.bpm || "") + (parent.displayData ? " · " + parent.displayData : "")) : parent.displayData\n',
        )],
    })
    if not v53():
        # Engine offers streamed rows only Prepare; add Stems (see scStemStream).
        out[v + "BrowseView/Components/Components/SwipeLeftItem.qml"] = [
            ("	readonly property list<string> actionLabels: leftActions.map( action => action.display.label )\n",
             '	readonly property bool scStem: !(typeof root !== "undefined" && root && root.scIsAZ04) && String(row.modelData && row.modelData.uri || "").startsWith("streaming://TIDAL/Track/") && !leftActions.some(action => /stem/i.test(String(action.display && action.display.label || "")))\n'
             "	readonly property list<string> actionLabels: leftActions.map( action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : action.display.label ).concat(scStem ? [root.scT(\"Stems\")] : [])\n"),
            ("leftActions.map( action => action.display.commitMessage )", "leftActions.map( action => /^stems?$/i.test(String(action.display && action.display.label || \"\")) ? root.scT(\"Stems\") : action.display.commitMessage ).concat(scStem ? [root.scT(\"Stems\")] : [])"),
            ("leftActions.map( action => action.display.color )", "leftActions.map( action => action.display.color ).concat(scStem ? [Style.systemBlue01Color] : [])"),
            ("				  leftActions[actionIndex].trigger(row.index)\n",
             "				  if (actionIndex < leftActions.length) leftActions[actionIndex].trigger(row.index)\n"
             "				  else root.scStemStream(row.modelData.trackId)\n"),
        ]
    out[prefs + ("" if v53() else "TabContents/") + "LibraryTabContent.qml"].append((
        "StemsList {}," if v53() else "StemsList { },",
        ("StemsList {}," if v53() else "StemsList { },") + """
\t\tText {
\t\t\tvisible: !(typeof root !== "undefined" && root && root.scIsAZ04)
\t\t\tLayout.fillWidth: true
\t\t\tleftPadding: 24; rightPadding: 24
\t\t\twrapMode: Text.WordWrap
\t\t\tcolor: "#9a9a9a"
\t\t\tfont.pixelSize: 16
\t\t\ttext: root.scLanStemsLine(root.scLanStemsInfo, root.scLang)
\t\t},""",
    ))
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
        out["Skins/UIKit/Elements/StatusBar/SortMenuDialog.qml"] = [(
            "                    onClicked: {\n"
            "                        if (sortMenuDialog.sortViewModel) {\n"
            "                            sortMenuDialog.sortViewModel.selectRole(modelData.role)\n"
            "                        }\n"
            "                        onScreen = false\n"
            "                    }\n",
            "                    onClicked: {\n"
            "                        const cc = Planck.librarianController.currentCollectionController\n"
            "                        if (cc && cc.isCurrentCollectionStreaming) {\n"
            "                            try {\n"
            "                                var scReq = new XMLHttpRequest()\n"
            '                                scReq.open("POST", "http://127.0.0.1:8080/v1/sc5000/ui", true)\n'
            '                                scReq.setRequestHeader("Content-Type", "application/json")\n'
            "                                scReq.send(JSON.stringify({\n"
            '                                    "streamSortRole": modelData.role === "playOrder" ? "default" : modelData.role\n'
            "                                }))\n"
            "                            } catch (e) {}\n"
            "                        }\n"
            "                        if (sortMenuDialog.sortViewModel) {\n"
            "                            sortMenuDialog.sortViewModel.selectRole(modelData.role)\n"
            "                        }\n"
            "                        onScreen = false\n"
            "                    }\n",
        )]
        out["Skins/UIKit/Elements/StatusBar/SortOrderButton.qml"] = [(
            "    onClicked: {\n"
            "        if (sortViewModel) sortViewModel.toggleOrder()\n"
            "    }\n",
            "    onClicked: {\n"
            "        if (sortViewModel) {\n"
            "            const cc = Planck.librarianController.currentCollectionController\n"
            "            if (cc && cc.isCurrentCollectionStreaming) {\n"
            "                var willBeAsc = !sortViewModel.isAscending\n"
            "                try {\n"
            "                    var scReq = new XMLHttpRequest()\n"
            '                    scReq.open("POST", "http://127.0.0.1:8080/v1/sc5000/ui", true)\n'
            '                    scReq.setRequestHeader("Content-Type", "application/json")\n'
            "                    scReq.send(JSON.stringify({\n"
            '                        "streamSortOrder": willBeAsc ? "asc" : "desc"\n'
            "                    }))\n"
            "                } catch (e) {}\n"
            "            }\n"
            "            sortViewModel.toggleOrder()\n"
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
        )]
        # #49: a swipe loads the role's own decks (1+3: left 1, right 3), not the stock
        # left/right layer deck. Only "Deck N" actions; Prepare List etc. stay stock.
        for side, right in (("left", "false"), ("right", "true")):
            a = side + "Actions"
            if side == "left":
                out[ROOT + f"Views/BrowseView/Components/Components/Swipe{side.title()}Item.qml"] = [
                    (f"    readonly property list<string> actionLabels: {a}.map(action => action.display.label)\n",
                     '    readonly property bool scStem: !(typeof root !== "undefined" && root && root.scIsAZ04) && !!(row.isStreamingTrack || row.streamingProviderName || String(row.modelData && row.modelData.uri || "").startsWith("streaming://TIDAL/Track/") || (row.modelData && row.modelData.trackId > 0)) && !leftActions.some(action => /stem/i.test(String(action.display && action.display.label || "")))\n'
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
        # #171 uses the stock Mixer Menu -> Channels -> Input selectors. Its
        # C++ labels need the same live dictionary as the QML literals.
        out[ROOT + "Views/MixerMenu/Pages/ChannelsPageItems/InputSelect.qml"] = [
            ("property MixerMenuChannelSection controller",
             '''property MixerMenuChannelSection controller
    // SC LIVE 4 Bluetooth/Computer routes are unverified. Keep native indices
    // when filtering the visible options, including the original icon mapping.
    readonly property var scInputIndexes: {
        const result = []
        if (controller) for (let i = 0; i < controller.inputs.length; i++) {
            const name = controller.inputs[i]
            if (Config.device.name !== "JP21" || name === "Engine" || name === "Line In") result.push(i)
        }
        return result
    }
    readonly property string scAuxChannel: Config.device.name === "JP21" && Config.audio.hasSoftwareLineIn && inputSelect.controller
        ? (String(inputSelect.controller.title).match(/[34]/) || [""])[0] : ""
    readonly property var scAuxLine: scAuxChannel ? Planck.getProperty("/Engine/Mixer/Channel" + scAuxChannel + "/Line") : null
    readonly property bool scAuxLineActive: !!scAuxLine && !!scAuxLine.translator && scAuxLine.translator.state'''),
            ('text: "Input"', 'text: Config.device.name === "JP21" ? root.scT("Input") : "Input"'),
            ("inputSelect.controller.selectInput(index)",
             '''const nativeIndex = inputSelect.scInputIndexes[index]
            inputSelect.controller.selectInput(nativeIndex)
            // An empty channel's native currentInput can remain Engine while
            // Line is true. Clear that native switch explicitly on Engine.
            if (scAuxLine && scAuxLine.translator && inputSelect.controller.inputs[nativeIndex] === "Engine")
                scAuxLine.translator.state = false'''),
            ("labelText: inputSelect.controller.currentInput",
             'labelText: Config.device.name === "JP21" ? root.scT(inputSelect.scAuxLineActive ? "Line In" : inputSelect.controller.currentInput) : inputSelect.controller.currentInput'),
            ("i < inputSelect.controller.inputs.length", "i < inputSelect.scInputIndexes.length"),
            ("optionName: inputSelect.controller.inputs[i],",
             'optionName: Config.device.name === "JP21" ? root.scT(inputSelect.controller.inputs[inputSelect.scInputIndexes[i]]) : inputSelect.controller.inputs[inputSelect.scInputIndexes[i]],'),
            ("optionIcon: inputSelect.controller.inputIcons[i],",
             "optionIcon: inputSelect.controller.inputIcons[inputSelect.scInputIndexes[i]],"),
        ]
        out[ROOT + "Views/MixerMenu/MixerMenu.qml"] = [
            ("text: modelData.title", 'text: Config.device.name === "JP21" ? root.scT(modelData.title) : modelData.title'),
        ]
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
            # Stock TransitionFxEnable stays untouched in Control Center (active when
            # hasTransitionFx && !hasHardwareFaderFxButtons); UPFADER_ECHO_TILE hides itself there,
            # while Settings -> Mixer keeps Off / All Faders / Upfader Only synced with it.
            v + "StatusBar/Buttons/TransitionFxButton.qml": [
                ("    visible: Config.audio.fx.hasTransitionFx && transitionFx && !Config.ui.hasTransitionFxSelectionPopup\n",
                 '    visible: Config.audio.fx.hasTransitionFx && transitionFx && !Config.ui.hasTransitionFxSelectionPopup && (typeof root === "undefined" || !root || root.scHasMixer)\n'),
            ],
            prefs + "Entries/TransitionFxSection.qml": [
                ('        TransitionFxSelectEffect {\n            view: transitionFx.view\n        }',
                 '        TransitionFxSelectEffect {\n            view: transitionFx.view\n        }\n        ListEntry {\n            text: "Trigger Source"\n            visible: typeof root !== "undefined" && root && root.scHasMixer\n            readonly property var vfx: transitionFx.view\n            values: ["Off", "All Faders", "Upfader Only"]\n            currentValue: vfx ? (!vfx.enabled ? "Off" : (vfx.triggerSource === 1 ? "Upfader Only" : "All Faders")) : (root.scUpfaderEcho ? "Upfader Only" : "Off")\n            onSelected: function(index) {\n                const on = index !== 0\n                root.scUpfaderEcho = on\n                root.scSetTransitionFx(vfx, on, index === 2)\n                root.scCall("upfaderEcho=" + (on ? "1" : "0"))\n            }\n        }'),
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
    scaled = PatchDict({
        v + "ControlCenter/ControlCenter.qml": size + [(f"height: {H} +", f"height: {UH} +")],
        v + "ControlCenter/ControlCenterTabView.qml": [(f"{H} - height", f"{UH} - height")],
        v + "StatusBar/AbstractStatusBar.qml": [(f"dragPosition * {H}", f"dragPosition * {UH}")],
        v + "EmbeddedBase.qml": [(f"implicitWidth: {W}\n", f"implicitWidth: {UW}\n"), (f"implicitHeight: {H}\n", f"implicitHeight: {UH}\n")],
        v + "StorageMode/StorageModeOverlay.qml": size,
        ROOT + "Items/Pads/PadModeSelectDialogue.qml": size,
        prefs + "Entries/private/ColorWheelDialog.qml": size,
        # Settings -> Device -> Pad LED Brightness also dims the on-screen pads.
        ROOT + "Items/Pads/Pads.qml": [("id: pads\n", "id: pads\n    opacity: typeof root !== \"undefined\" && root && root.scLedBrightness ? Math.max(0.2, Math.min(1, root.scLedBrightness / 100)) : 1\n")],
    })
    if v53():
        scaled[v + "MainView/Fx/DJFxSelectCarousel.qml"] = size
        # Keep the stock carousel, entries, controls and native popup selection.
        # A touch tap latches it open; the hardware Selecting property is a
        # momentary hold on consoles with a physical Sweep FX button.
        scaled[v + "MainView/Fx/SweepFxSelectCarousel.qml"] = [
            ('Loader {\n', '''Loader {
    id: scSweepCarousel
    readonly property bool scTouchOpen: Config.audio.fx.hasNHFx && typeof root !== "undefined" && root.scMixstreamSweepOpen
'''),
            ('visible: view.popup.visible\n', 'visible: view.popup.visible || scSweepCarousel.scTouchOpen\n'),
            ('hasCloseButton: view.popup.hasCloseButton\n', 'hasCloseButton: view.popup.hasCloseButton || scSweepCarousel.scTouchOpen\n'),
            ('onAccepted: result => view.popup.select(result)\n', '''onAccepted: result => {
            if (scSweepCarousel.scTouchOpen) {
                // Native model order differs from SweepEffect's wire values.
                // The hold-button adapter has no active selection on a touch
                // tap; write the same native Type property as SweepFxSelect.
                const types = {"Filter": 1, "Wash": 2, "Dub Echo": 3, "Noise": 5, "Filter Roll": 4}
                const type = types[view.popup.entries[result]]
                if (type !== undefined) for (let ch = 1; ch <= 2; ch++) {
                    const p = Planck.getProperty("/Engine/Mixer/Channel" + ch + "/SweepFx/Type")
                    if (p && p.translator) p.translator.unnormalized = type
                }
                root.scMixstreamSweepOpen = false
            } else view.popup.select(result)
        }
'''),
            ('onRejected: view.popup.abort()\n', '''onRejected: {
            if (scSweepCarousel.scTouchOpen) root.scMixstreamSweepOpen = false
            view.popup.abort()
        }
'''),
        ]
        # #166: the compact Numark decks have four pads and no Stems EQ key.
        # Leave hardware pad modes alone; the preference selects a touch bank.
        scaled[ROOT + "Items/Pads/Pads.qml"].extend([
            ('    required property int deckIndex\n', '''    required property int deckIndex
    readonly property bool scMixstream: Config.audio.fx.hasNHFx
    readonly property bool scBeatJumpBank: scMixstream && Config.audio.deck.hasBeatJump && typeof root !== "undefined" && !!root.scBeatJumpPads
    readonly property var scStripProperty: scMixstream ? Planck.getProperty("/Engine/Mixer/Channel" + deckIndex + "/ChannelStrip/Mode") : null
    readonly property var scStripMode: scStripProperty ? scStripProperty.translator : null
    function scJump(action) {
        const p = Planck.getProperty("/Engine/Deck" + deckIndex + "/Track/BeatJump/" + action)
        if (p && p.translator) p.translator.state = true
    }
'''),
            ('        GridLayout {\n            id: padsLayout\n', '''        ColumnLayout {
            visible: pads.scMixstream
            Layout.preferredWidth: 84
            Layout.fillHeight: true
            Layout.topMargin: 16
            Layout.bottomMargin: 16
            spacing: 4
            Button {
                objectName: "MixstreamSweepFx" + pads.deckIndex
                Layout.fillWidth: true
                Layout.fillHeight: true
                text: "SWEEP FX"
                enabled: Config.ui.hasSweepFxSelectionPopup
                onClicked: root.scMixstreamSweepOpen = true
            }
            Button {
                objectName: "MixstreamStemsEq" + pads.deckIndex
                Layout.fillWidth: true
                Layout.fillHeight: true
                text: "EQ / STEMS"
                highlighted: !!pads.scStripMode && pads.scStripMode.index === 1
                enabled: Config.audio.hasStemsLevel && !!pads.scStripMode
                onClicked: pads.scStripMode.index = pads.scStripMode.index === 1 ? 0 : 1
            }
        }

        RowLayout {
            objectName: "MixstreamBeatJumpBank" + pads.deckIndex
            visible: pads.scBeatJumpBank
            Layout.fillWidth: true
            Layout.fillHeight: true
            Layout.margins: 16
            spacing: 4
            Repeater {
                model: [
                    {label: "Jump −", action: "JumpBack"},
                    {label: "Jump +", action: "JumpForward"},
                    {label: "½", action: "BeatJumpHalve"},
                    {label: "×2", action: "BeatJumpDouble"}
                ]
                delegate: Button {
                    required property var modelData
                    objectName: "Mixstream" + modelData.action + pads.deckIndex
                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    text: typeof root !== "undefined" && root.scT ? root.scT(modelData.label) : modelData.label
                    onClicked: pads.scJump(modelData.action)
                }
            }
        }

        GridLayout {
            id: padsLayout
            visible: !pads.scBeatJumpBank
'''),
        ])
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
        # #152: unlock Shift + Cue (PFL) channel-strip mode toggle (EQ <-> Stems Level) on all
        # mixer boards, and prevent rapid double-tapping Hot Cue from bouncing STEMS back to CUES.
        scaled["ControlSurfaceModules/ControlSurfaceModules/MixerChannelCore.qml"] = [(
            "    readonly property bool canToggleMode: module.pflShiftAction == Action.ToggleChannelStripMode\n",
            "    readonly property bool canToggleMode: Config.audio.hasStemsLevel || module.pflShiftAction == Action.ToggleChannelStripMode\n",
        )]
        scaled["ControlSurfaceModules/ControlSurfaceModules/PadModeSelect.qml"] = [(
            "            ValueNoteAssignment {\n"
            "                id: padModeSelect\n",
            "            property double scStemsLatchedAt: 0\n\n"
            "            ValueNoteAssignment {\n"
            "                id: padModeSelect\n",
        ), (
            "                        } else {\n"
            "                            Planck.getProperty(\n"
            "                                `${deckConfig.activeDeckPath}PadModeButton${model.index}Pressed`\n"
            "                            ).translator.trigger()\n"
            "                        }\n",
            "                        } else {\n"
            "                            const now = Date.now()\n"
            "                            if (model.index === 0 && now - button.scStemsLatchedAt < 350) {\n"
            "                                if (!deckConfig.padsModeIsStems && deckConfig.pPadsView && deckConfig.pPadsView.translator) {\n"
            "                                    deckConfig.pPadsView.translator.string = \"STEMS\"\n"
            "                                }\n"
            "                                return\n"
            "                            }\n"
            "                            const wasStems = deckConfig.padsModeIsStems\n"
            "                            Planck.getProperty(\n"
            "                                `${deckConfig.activeDeckPath}PadModeButton${model.index}Pressed`\n"
            "                            ).translator.trigger()\n"
            "                            if (model.index === 0 && !wasStems) {\n"
            "                                button.scStemsLatchedAt = now\n"
            "                            }\n"
            "                        }\n",
        )]
        # Keep the stock two-item BeatJump layout. The forward target lives
        # inside the size field, so it cannot widen AutoLoop / BeatJump / LEAD.
        bj = "Skins/EngineOS/UI/CompositeControls/"
        def bj_tap(action, call="p.translator.state = true", right_margin=0):
            return ('        MouseArea {\n            anchors.fill: parent\n'
                    f'            anchors.rightMargin: {right_margin}\n            anchors.topMargin: -6\n            anchors.bottomMargin: -6\n'
                    f'            onClicked: {{ const p = Planck.getProperty("/Engine/Deck" + beatJump.deckIndex + "/Track/BeatJump/{action}"); if (p && p.translator) {call} }}\n'
                    '        }\n')
        scaled[bj + "Deck/DeckItem/BeatJump.qml"] = [
            ("    required property BeatJumpViewInterface view\n",
             "    required property BeatJumpViewInterface view\n    property int deckIndex: 1\n"),
            ("        SequentialAnimation {\n            id: beatJumpImageAnimation\n",
             bj_tap("JumpBack") + "\n        SequentialAnimation {\n            id: beatJumpImageAnimation\n"),
            ("        SequentialAnimation {\n            id: beatJumpTextAnimation\n",
             bj_tap("BeatJumpIndex", "p.translator.incValue(0, 1, 0, true)", 14) +
             '        Text {\n            objectName: "Deck_BeatJump_Forward"\n'
             '            anchors.right: parent.right\n            height: parent.height\n            width: 14\n'
             '            verticalAlignment: Text.AlignVCenter\n            horizontalAlignment: Text.AlignRight\n'
             '            font: beatJumpText.font\n            color: Style.font01Color\n            text: "\\u00bb"\n'
             + bj_tap("JumpForward") + "        }\n\n        SequentialAnimation {\n            id: beatJumpTextAnimation\n"),
        ]
        for rel, owner, indent in (
            ("Deck/DeckSharedItems/DeckSharedItems.qml", "deckSharedItems", "            "),
            ("DeckPerformanceData/DeckPerformanceDataContent.qml", "deckPerformanceDataContent", "        "),
            ("DeckPerformanceData/PortraitDeckPerformanceDataContent.qml", "deckPerformanceDataContent", "                "),
            ("DeckOverview/DeckOverview.qml", "control", "                    "),
            ("DeckTrackData/GroupedMiniDeckTrackDataContent.qml", "deckTrackDataContent", "                    "),
        ):
            line = indent + "view: " + owner + ".deckView.beatJumpView\n"
            scaled[bj + rel] = [(line, line + indent + "deckIndex: " + owner + ".deckView.deckData.deckIndex\n")]
            line = indent + "view: " + owner + ".deckView.autoLoopView\n"  # AutoLoop: the deck whose source's loop it shows
            scaled[bj + rel].append((line, indent + "deckIndex: " + owner + ".deckView.deckData.deckIndex\n" + line))
        # Same compact order in both deck counts: Loop / BeatJump / LEAD /
        # Cue / soft takeover / speed. Never spend the time display's width.
        scaled[bj + "DeckPerformanceData/DeckPerformanceData.qml"] = [
            ("        visible: control.deckView.deckData.songLoaded\n",
             '        visible: control.deckView.deckData.songLoaded || (typeof root !== "undefined" && root && root.scShadowVisible && root.scShadowVisible(control.deckView.deckData.deckIndex))\n'),
        ]
        performance = bj + "DeckPerformanceData/DeckPerformanceDataContent.qml"
        scaled[performance] += [
            ("    Separator {\n        visible: !spacer2.visible && softTakeoverIndicators.visible\n    }\n    Item {\n        id: spacer2\n        Layout.fillWidth: true\n        visible: false\n",
             "    Separator {\n        visible: !spacer1.visible && softTakeoverIndicators.visible\n    }\n" +
             cue_capsule("    ", "deckPerformanceDataContent.deckData.deckIndex", 22, 16, compact=True, lead_sep=False, fill_width=False) +
             "\n    Item {\n        id: spacer2\n        Layout.fillWidth: true\n        visible: true\n"),
            ("        id: autoLoop\n", "        id: autoLoop\n        Layout.fillWidth: false\n        Layout.rightMargin: 4\n"),
            ("        id: beatJump\n", "        id: beatJump\n        Layout.fillWidth: false\n"),
        ]
        # Both controls have spacing:6; patch() deliberately requires a unique
        # anchor, so apply it together with the control-specific view line.
        for control, width in (("autoLoop", 52), ("beatJump", 64)):
            view = "autoLoopView" if control == "autoLoop" else "beatJumpView"
            anchor = ("        view: deckPerformanceDataContent.deckView." + view + "\n" +
                      ("        deckIndex: deckPerformanceDataContent.deckView.deckData.deckIndex\n" if control == "beatJump" else ""))
            scaled[performance].append((anchor + "        spacing: 6\n", anchor + "        spacing: 4\n"))
        # Size the right speed group (▲▼, range, pitch %) from the worst-case strings
        # ("±100" and "-100.00%") measured with TextMetrics in the active font when
        # speed*TextWidth is 0, while keeping stock >0 widths 100% unchanged.
        scaled[bj + "Deck/DeckItem/SpeedInfo.qml"] = [
            ("    Text {\n        id: speedRangeText\n",
             '    TextMetrics { id: scRangeWorstMetrics; font: speedRangeText.font; text: "\\u00b1100" }\n'
             '    TextMetrics { id: scStateWorstMetrics; font: speedStateText.font; text: "-100.00%" }\n\n'
             "    Text {\n        id: speedRangeText\n"),
            ("        Layout.minimumWidth: speedRangeTextWidth\n",
             "        Layout.minimumWidth: speedRangeTextWidth > 0 ? speedRangeTextWidth : Math.max(speedRangeTextWidth, Math.ceil(scRangeWorstMetrics.width))\n"
             "        Layout.maximumWidth: speedRangeTextWidth > 0 ? Number.POSITIVE_INFINITY : Math.max(speedRangeTextWidth, Math.ceil(scRangeWorstMetrics.width))\n"),
            ("        Layout.minimumWidth: speedStateTextWidth\n        Layout.maximumWidth: speedStateTextWidth\n",
             "        Layout.minimumWidth: speedStateTextWidth > 0 ? speedStateTextWidth : Math.max(speedStateTextWidth, Math.ceil(scStateWorstMetrics.width))\n"
             "        Layout.maximumWidth: speedStateTextWidth > 0 ? speedStateTextWidth : Math.max(speedStateTextWidth, Math.ceil(scStateWorstMetrics.width))\n"),
        ]
        scaled[performance] += [
            ("        Layout.fillWidth: true\n        Layout.preferredHeight: 24\n        Layout.maximumWidth: 200\n        Layout.leftMargin: 24\n        Layout.rightMargin: 24\n",
             "        Layout.fillWidth: false\n        Layout.preferredHeight: 24\n        Layout.minimumWidth: 164\n        Layout.preferredWidth: 164\n        Layout.maximumWidth: 164\n        Layout.leftMargin: 6\n        Layout.rightMargin: 6\n"),
            ("                Layout.leftMargin: 24\n                Layout.rightMargin: 12\n",
             "                Layout.leftMargin: 4\n                Layout.rightMargin: 4\n"),
            ("                target: softTakeoverIndicators\n                Layout.rightMargin: 32\n",
             "                target: softTakeoverIndicators\n                Layout.rightMargin: 6\n"),
            ("        speedRangeTextFont: Style.fontH1\n        speedRangeTextWidth: 80\n        speedRangeTextHorizontalAlignment: Text.AlignHCenter\n\n        speedStateTextFont: Style.fontH1\n        speedStateTextWidth: 80\n        speedStateTextHorizontalAlignment: Text.AlignRight\n\n        textLayoutAlignment: Qt.AlignCenter\n        columnSpacing: 0\n",
             "        speedRangeTextFont: Style.fontH1\n        speedRangeTextWidth: 0\n        speedRangeTextHorizontalAlignment: Text.AlignRight\n\n        speedStateTextFont: Style.fontH1\n        speedStateTextWidth: 0\n        speedStateTextHorizontalAlignment: Text.AlignRight\n\n        textLayoutAlignment: Qt.AlignCenter\n        columnSpacing: 8\n"),
            ("                speedRangeTextFont: Style.fontTitle\n                speedRangeTextWidth: 144\n                speedRangeTextColor: Style.font02Color\n\n                speedStateTextFont: Style.fontTitle\n                speedStateTextWidth: 144\n",
             "                speedRangeTextFont: Style.fontTitle\n                speedRangeTextWidth: 0\n                speedRangeTextHorizontalAlignment: Text.AlignRight\n                speedRangeTextColor: Style.font02Color\n\n                speedStateTextFont: Style.fontTitle\n                speedStateTextWidth: 0\n                columnSpacing: 12\n"),
        ]
        for control, width in (("autoLoop", 52), ("beatJump", 64)):
            scaled[performance] += [
                (f"                target: {control}\n                Layout.preferredWidth: 88\n                iconWidth: 20\n                iconHeight: 12\n                textFont: Style.fontH1\n",
                 f"                target: {control}\n                Layout.preferredWidth: {width}\n                iconWidth: 16\n                iconHeight: 10\n                textFont: Style.fontH3\n"),
                (f"                target: {control}\n                Layout.preferredWidth: 116\n                textFont: Style.fontTitle\n                iconWidth: 26\n                iconHeight: 16\n",
                 f"                target: {control}\n                Layout.preferredWidth: {width}\n                textFont: Style.fontH3\n                iconWidth: 16\n                iconHeight: 10\n"),
            ]
        mini = bj + "DeckTrackData/GroupedMiniDeckTrackDataContent.qml"
        scaled[mini] += [
            ("                SoftTakeoverIndicators {\n", cue_capsule("                ", "deckTrackDataContent.deckIndex", 22, 16, compact=True, lead_sep=False, fill_width=True) + "\n                SoftTakeoverIndicators {\n                    iconWidth: 14\n                    iconHeight: 10\n                    flow: Flow.LeftToRight\n                    spacing: -4\n"),
            ("                spacing: 16\n                visible: deckTrackDataContent.songLoaded\n", "                spacing: 2\n                visible: deckTrackDataContent.songLoaded\n"),
            ("                    Layout.alignment: Qt.AlignLeft\n                    Layout.preferredWidth: 72\n", "                    Layout.fillWidth: false\n                    Layout.alignment: Qt.AlignLeft\n                    Layout.preferredWidth: 52\n"),
            ("                    textFont: Style.fontH1\n                    Layout.preferredWidth: 72\n", "                    Layout.fillWidth: false\n                    textFont: Style.fontH3\n                    Layout.preferredWidth: 64\n"),
            ("                    Layout.preferredHeight: 24\n                    Layout.fillWidth: true\n                    Layout.maximumWidth: 200\n",
             "                    Layout.preferredHeight: 24\n                    Layout.fillWidth: false\n                    Layout.minimumWidth: 135\n                    Layout.preferredWidth: 135\n                    Layout.maximumWidth: 135\n"),
            ("                    speedRangeTextFont: Style.fontH1\n                    speedStateTextFont: Style.fontH1\n                    speedRangeTextHorizontalAlignment: Text.AlignRight\n                    speedStateTextHorizontalAlignment: Text.AlignRight\n                    textLayoutAlignment: Qt.AlignRight | Qt.AlignVCenter\n                    flat: true\n",
             "                    speedRangeTextFont: Style.fontH1\n                    speedRangeTextWidth: 0\n                    speedRangeTextHorizontalAlignment: Text.AlignRight\n                    speedStateTextFont: Style.fontH1\n                    speedStateTextWidth: 0\n                    speedStateTextHorizontalAlignment: Text.AlignRight\n                    textLayoutAlignment: Qt.AlignRight | Qt.AlignVCenter\n                    columnSpacing: 8\n                    flat: true\n"),
        ]
        # Use control-specific beginnings to keep all anchors unique.
        scaled[mini] += [
            ("                    view: deckTrackDataContent.deckView.autoLoopView\n                    spacing: 6\n                    iconWidth: 20\n                    iconHeight: 12\n                    textFont: Style.fontH1\n",
             "                    view: deckTrackDataContent.deckView.autoLoopView\n                    spacing: 4\n                    iconWidth: 16\n                    iconHeight: 10\n                    textFont: Style.fontH3\n"),
            ("                    deckIndex: deckTrackDataContent.deckView.deckData.deckIndex\n                    spacing: 6\n                    iconWidth: 20\n                    iconHeight: 12\n",
             "                    deckIndex: deckTrackDataContent.deckView.deckData.deckIndex\n                    spacing: 4\n                    iconWidth: 16\n                    iconHeight: 10\n"),
        ]
        split_controls = """        AutoLoop {
            Layout.fillWidth: false
            Layout.preferredWidth: 52
            Layout.rightMargin: 4
            deckIndex: deckTrackDataContent.deckIndex
            view: deckTrackDataContent.deckView.autoLoopView
            spacing: 4
            iconWidth: 16
            iconHeight: 10
            textFont: Style.fontH3
        }
        BeatJump {
            Layout.fillWidth: false
            Layout.preferredWidth: 64
            view: deckTrackDataContent.deckView.beatJumpView
            deckIndex: deckTrackDataContent.deckIndex
            spacing: 4
            iconWidth: 16
            iconHeight: 10
            textFont: Style.fontH3
            visible: Config.audio.deck.hasBeatJump
        }
"""
        split_speed = """        SpeedInfo {
            Layout.leftMargin: 4
            speedState: deckTrackDataContent.deckData.speedState
            speedRange: deckTrackDataContent.deckData.speedRange
            speedRangeTextWidth: 0
            speedRangeTextHorizontalAlignment: Text.AlignRight
            speedStateTextWidth: 0
            speedStateTextHorizontalAlignment: Text.AlignRight
            columnSpacing: 8
            flat: true
        }
"""
        scaled[bj + "DeckTrackData/SplitMiniDeckTrackDataContent.qml"] = [
            ("        JP07SyncIndicator {\n", split_controls + "\n        JP07SyncIndicator {\n"),
            ("            Layout.fillWidth: true\n            Layout.preferredHeight: 24\n            Layout.maximumWidth: 160\n            Layout.leftMargin: 24\n            Layout.rightMargin: 40\n",
             "            Layout.fillWidth: false\n            Layout.preferredHeight: 24\n            Layout.minimumWidth: 160\n            Layout.preferredWidth: 160\n            Layout.maximumWidth: 160\n            Layout.leftMargin: 4\n            Layout.rightMargin: 4\n"),
            ("        DeckInfoTimeInfo {\n", cue_capsule("        ", "deckTrackDataContent.deckIndex", 22, 16, compact=True, lead_sep=False, fill_width=False) + "\n" + split_speed + "\n        DeckInfoTimeInfo {\n"),
            ("            horizontalSpacing: 36\n", "            horizontalSpacing: 18\n"),
        ]
        for item, text_id in (("AutoLoop", "autoLoopText"), ("BeatJump", "beatJumpText")):
            scaled.setdefault(bj + "Deck/DeckItem/" + item + ".qml", []).append((
                f"        id: {text_id}\n", f"        id: {text_id}\n        Layout.fillWidth: parent.Layout.preferredWidth > 0 && parent.Layout.preferredWidth <= 64\n        Layout.minimumWidth: parent.Layout.preferredWidth > 0 && parent.Layout.preferredWidth <= 64 ? 0 : -1\n        fontSizeMode: Text.HorizontalFit\n        minimumPixelSize: 14\n" +
                ("        rightPadding: 14\n" if item == "BeatJump" else "")))
        # The shadow header feeds the stock formatter its own tempo; stock users keep stock defaults.
        scaled[ROOT + "Items/Bpm.qml"] = [
            ("    property alias fractionColor: fractionText.color\n",
             "    property alias fractionColor: fractionText.color\n    readonly property real displayWidth: implicitWidth + fractionText.implicitWidth\n"),
            ("    readonly property bool currentBPMSplitValid:",
             "    property real bpmValue: pCurrentBPM && pCurrentBPM.translator ? pCurrentBPM.translator.unnormalized : 0\n\n    readonly property bool currentBPMSplitValid:"),
            ("if (songAnalyzed && pCurrentBPM && pCurrentBPM.translator)", "if (songAnalyzed)"),
            ("pCurrentBPM.translator.unnormalized.toFixed(bpm.decimalPlaces)", "bpmValue.toFixed(bpm.decimalPlaces)"),
        ]
        # Two decimals, and nothing else: the stock Row, colours and tap stay, and whatever stock
        # shows without an analysed tempo ("-", an unanalysed track) is stock's own text.
        # The shadow header mirrors the source deck's Sync (green BPM) and LEAD; the copy itself is in neither.
        def head(deck, field, fallback="false"):
            return f'(typeof root !== "undefined" && root && root.scShadowHeadOf && typeof root.scShadowHeadOf({deck}).{field} === "boolean" ? root.scShadowHeadOf({deck}).{field} : ({fallback}))'
        # Title, artist and key are the source's (its library's), not what Engine here reads from the copy.
        heads = 'readonly property var scHead: typeof root !== "undefined" && root && root.scShadowHeadOf ? root.scShadowHeadOf(deckIndex) : null\n'
        scaled[bj + "Deck/DeckItem/ArtistSong.qml"] = [
            ("    readonly property string songTextString: deckLoaded ? displayText.display : deckLabel\n"
             '    readonly property string artistTextString: deckLoaded ? artistName : "No Track Loaded"\n',
             "    " + heads +
             '    readonly property string songTextString: deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title) || displayText.display) : deckLabel\n'
             '    readonly property string artistTextString: deckLoaded ? (displayText.hideTitleArtist ? "" : (scHead && scHead.title ? scHead.artist : artistName)) : "No Track Loaded"\n'),
            ("    readonly property bool onlyFileName: view.onlyFileName\n",
             "    readonly property bool onlyFileName: view.onlyFileName && !(scHead && scHead.title)\n"),
            ("    readonly property bool isStemmed: view.stemmed\n",
             "    readonly property bool isStemmed: scHead && scHead.title ? scHead.stems : view.stemmed\n"),
            ("        providerName: artistSong.trackSource\n",
             "        providerName: artistSong.trackSource\n        respectHideTitleArtist: true\n"),
            ("            visible: artistSong.artistVisible && !artistSong.onlyFileName && artistSong.artistTextString.length > 0\n",
             "            visible: artistSong.artistVisible && !artistSong.onlyFileName && !displayText.hideTitleArtist && artistSong.artistTextString.length > 0\n"),
        ]
        scaled[bj + "KeyEditor/KeyInfo.qml"] = [
            ('    readonly property string currentKey: keyEditorController ? keyEditorController.getViewModel().currentKey : ""\n',
             "    " + heads +
             '    readonly property string currentKey: scHead && scHead.keyName ? (scHead.keyName === "-" ? "" : scHead.keyName)\n'
             '        : keyEditorController ? keyEditorController.getViewModel().currentKey : ""\n'),
            ("    readonly property int offset: keyEditorController ? keyEditorController.getViewModel().keyOffset : 0\n",
             "    readonly property int offset: scHead && scHead.title ? scHead.keyOffset : keyEditorController ? keyEditorController.getViewModel().keyOffset : 0\n")]
        # The source's loop lights the shadow header's loop icon and size; the copy itself does not loop.
        scaled.setdefault(bj + "Deck/DeckItem/AutoLoop.qml", []).extend([
            ("    required property AutoLoopViewInterface view\n",
             "    required property AutoLoopViewInterface view\n    property int deckIndex: 0\n"
             "    readonly property bool scLoop: view.loopEnableState || " + head("deckIndex", "loop") + "\n"),
            ("        color: view.loopEnableState ? Style.active01Color : Style.icon01Color\n", "        color: scLoop ? Style.active01Color : Style.icon01Color\n"),
            ("        color: view.loopEnableState ? Style.active01Color : Style.font01Color\n", "        color: scLoop ? Style.active01Color : Style.font01Color\n"),
        ])
        scaled[ROOT + "Views/MainView/Components/JP07SyncIndicator.qml"] = [
            ("    property bool isMaster: pDeckIsMaster.translator && pDeckIsMaster.translator.state\n",
             "    property bool isMaster: " + head("deckIndex", "master", "pDeckIsMaster.translator && pDeckIsMaster.translator.state") + "\n")]
        scaled[bj + "Deck/DeckItem/BpmInfo.qml"] = [
            ("    readonly property bool syncEnabled: deckController && deckController.syncInfoViewController.syncEnabled\n",
             "    readonly property bool syncEnabled: " + head("deckIndex", "sync", "deckController && deckController.syncInfoViewController.syncEnabled") + "\n"),
            ("    property bool tappable: true\n",
             "    Layout.minimumWidth: implicitWidth\n    property bool tappable: true\n"
             '    readonly property var scBpm: deckIndex > 0 ? Planck.getProperty("/Engine/Deck%1/CurrentBPM".arg(deckIndex)) : null\n'
             '    readonly property var scAnalyzed: deckIndex > 0 ? Planck.getProperty("/Engine/Deck%1/Track/SongAnalyzed".arg(deckIndex)) : null\n'
             "    readonly property var scBpm2: bpmInfoView && bpmInfoView.bpmFractionalText && scAnalyzed && scAnalyzed.translator && scAnalyzed.translator.state\n"
             "        && scBpm && scBpm.translator && scBpm.translator.unnormalized > 0 ? scBpm.translator.unnormalized.toFixed(2).split(\".\") : null\n"),
            ("                text: bpmInfoView && bpmInfoView.bpmText\n",
             "                text: bpmInfo.scBpm2 ? bpmInfo.scBpm2[0] : bpmInfoView && bpmInfoView.bpmText\n"),
            ("                text: bpmInfoView && bpmInfoView.bpmFractionalText\n",
             '                text: bpmInfo.scBpm2 ? "." + bpmInfo.scBpm2[1] : bpmInfoView && bpmInfoView.bpmFractionalText\n'),
        ]
        scaled["ControlSurfaceModules/ControlSurfaceModules/private/ActionPadRGBLed.qml"] =[("    enabled: deckConfig.inDJMode\n", "    enabled: deckConfig.inDJMode\n" + pad_brt)]
        scaled["ControlSurfaceModules/ControlSurfaceModules/private/QuadPadRGBLed.qml"] = [("    idx: note\n", "    idx: note\n" + pad_brt)]
        scaled["airAssignments/OutputAssignment/ColorOutputAssignment.qml"] = [
            ("	property bool debug: false;\n",
             "	property bool debug: false;\n"
             "	// EngineX: dimming now happens after gamma in the C hook (snd_seq_event_output_direct); QML sends the stock colour.\n"
             "	property real scBrightness: typeof globalConfig !== \"undefined\" && globalConfig && globalConfig.scPadBrightness ? globalConfig.scPadBrightness : 1\n"
             "	onScBrightnessChanged: if (enabled) device.sendColor(channel, idx, privatePart.scScaled())\n"),
            ("				device.sendColor(channel, idx, outputColor);\n", "				device.sendColor(channel, idx, scScaled());\n"),
            ("		id: privatePart\n",
             "		id: privatePart\n"
             "		function scScaled() { return outputColor }\n"),
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
            ("                    greyOut: !microDeckLoader.viewModel.activeDeck && !deckTabs.isTouchable\n",
             f"                    greyOut: {role} ? root.scDeckLocked(microDeckLoader.viewModel.deckNumber) : !microDeckLoader.viewModel.activeDeck && !deckTabs.isTouchable\n"),
        ]
        # 4-deck: the Load Track overlay shows only this player's two decks (the other
        # two refuse loads anyway, LoadToDeckFilter), centred by the stock spacers.
        scaled["Skins/EngineOS/UI/CompositeControls/LoadOverlay/LoadOverlayDecks.qml"] = [
            ("        visible: repeater.count === 2\n    }\n\n    Repeater {\n",
             f"        visible: repeater.count === 2 || !!({role})\n    }}\n\n    Repeater {{\n"),
            ("        visible: repeater.count === 2\n", f"        visible: repeater.count === 2 || !!({role})\n"),
            ("            visible: modelData.visible\n",
             f"            visible: modelData.visible && !({role} && root.scDeckLocked(modelData.deckIndex))\n"),
        ]
        scaled["Skins/EngineOS/UI/CompositeControls/Deck/MicroDeck.qml"] = [(
            "                text: microDeck.loadedDeckSongName\n",
            f"                text: ({role} ? microDeck.deckIndex + \" · \" : \"\") + ({role} && root.scShadowHeadOf(microDeck.deckIndex).title || microDeck.loadedDeckSongName)\n"),
            ("                id: albumArt\n",
             "                id: albumArt\n" + art_retry("                ", "microDeck.deckIndex", "microDeck.loadedDeckAlbumArtPath", "albumArt")),
            ("                source: microDeck.loadedDeckAlbumArtPath\n",
             f"                source: {GUARD} ? root.scDeckArtUrl(microDeck.deckIndex, microDeck.loadedDeckAlbumArtPath, albumArt.scArtTick, microDeck.loadedDeckSongName) : microDeck.loadedDeckAlbumArtPath\n")]
        scaled["Skins/EngineOS/UI/CompositeControls/Waveform/Waveform.qml"] = [(
            "                visible: lockedDeckHint.active\n",
            '                visible: lockedDeckHint.active && !(typeof root !== "undefined" && root && root.scShadowVisible && root.scShadowVisible(waveform.deckIndex))\n')]
        scaled[ROOT + "Views/Common/Waveform/WaveformLineInHint.qml"] = [(
            "    visible: deckIndex >= 1 && pLine.translator && pLine.translator.state\n",
            '    visible: deckIndex >= 1 && pLine.translator && pLine.translator.state && !(typeof root !== "undefined" && root && root.scShadowVisible && root.scShadowVisible(deckIndex) && root.scShadowData(deckIndex).lineIn)\n'),
            # Forced Engine mode on a console: say why the CDJ on this Line In has no waveform.
            ("    AIRMouseEventBlocker {}\n",
             "    AIRMouseEventBlocker {}\n"
             "    Text {\n"
             "        anchors.centerIn: parent\n"
             "        width: parent.width - 24\n"
             "        horizontalAlignment: Text.AlignHCenter\n"
             "        wrapMode: Text.WordWrap\n"
             '        visible: typeof root !== "undefined" && !!root && ((root.scShadowLineMask >> deckIndex) & 1) === 1\n'
             '        color: "#9a9a9a"\n'
             "        font.pixelSize: 16\n"
             '        text: "All Engine Mode: no CDJ waveform on Line In"\n'
             "    }\n")]
        scaled["Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/AlbumArt.qml"] = [(
            "        source: albumArt.albumArtPath\n",
            art_source("        ", "albumArt.deckIndex", "albumArt.albumArtPath", "albumArt.deckView && albumArt.deckView.artistSongView ? albumArt.deckView.artistSongView.songName : \"\"", "scArtImage", add_id=True))]
        # In the 4-deck layout the deck colour came out white (measured on a 1+3 deck with
        # blue PlayerColor1/2), which has no gradient and draws a black strip, and an unset
        # PlayerColor3/4 is black: use the deck's PlayerColor preference, then the player's
        # Layer A/B preference (PlayerColor1A/2A/1B/2B), then that of the deck on the same
        # side (1 or 2). White and black count as unset.
        scaled[v + "MainView/DeckInfo/DeckColor/DeckColor.qml"] = [(
            '    readonly property color playerColor: deckView ? deckView.deckData.deckColor : "transparent"\n',
            '    readonly property color scOwnColor: deckView ? deckView.deckData.deckColor : "transparent"\n'
            '    readonly property bool scPlayerFourDeck: typeof root !== "undefined" && !!root && !!root.scFourDeckRole && (!!root.scIsPlayerBoard || ["JP07", "JP08", "JP13", "JP14"].indexOf(Config.device.name) >= 0)\n'
            '    function scPref(n) { const p = Planck.getProperty("/Client/Preferences/Profile/Application/PlayerColor" + n); return p && p.translator ? p.translator.color : "transparent" }\n'
            '    function scLayerPref(n) {\n'
            '        const pp = Planck.getProperty("/Client/Preferences/Player")\n'
            '        const pn = (pp && pp.translator && parseInt(pp.translator.string)) || 1\n'
            '        const role = typeof root !== "undefined" && root && root.scFourDeckRole ? root.scFourDeckRole : (pn === 2 ? "24" : "13")\n'
            '        const tags = {1: "1A", 2: "2A", 3: "1B", 4: "2B"}\n'
            '        const dA = parseInt(role[0]), dB = role.length > 1 ? parseInt(role[1]) : 0\n'
            '        if (dA >= 1 && dA <= 4) tags[dA] = pn + "A"\n'
            '        if (dB >= 1 && dB <= 4) tags[dB] = pn + "B"\n'
            '        return scPref(tags[n] || n)\n'
            '    }\n'
            '    function scBlank(c) { return c.a === 0 || (c.r === c.g && c.g === c.b && (c.r === 0 || c.r === 1)) }\n'
            '    readonly property color playerColor: !deckView || !scBlank(scOwnColor) ? scOwnColor\n'
            '        : scPlayerFourDeck && !scBlank(scLayerPref(deckIndex)) ? scLayerPref(deckIndex)\n'
            '        : !scBlank(scPref(deckIndex)) ? scPref(deckIndex)\n'
            '        : deckIndex > 2 && !scBlank(scPref(deckIndex - 2)) ? scPref(deckIndex - 2) : scOwnColor\n')]
        # #49: 4-deck mode clears Config.device.isPlayer in C++ so Engine builds the 4-deck
        # layout, but player boards (JP07/JP08/JP13/JP14) must keep stock player UI behaviour:
        # direct browse-encoder load to this unit's active layer deck, PlayerColorSelect,
        # Player #, Layer B switch, RemoteDeckPicker, EngineConnect, and TouchFX Mute.
        is_player = '(Config.device.isPlayer || (typeof root !== "undefined" && root && root.scIsPlayerBoard) || ["JP07", "JP08", "JP13", "JP14"].indexOf(Config.device.name) >= 0)'
        scaled[ROOT + "main.qml"] = [
            (
                "    readonly property int activeDeck: parseInt(root.activeDecks[0])\n",
                '    readonly property int activeDeck: root.scFourDeckRole !== ""\n'
                '        ? parseInt(root.scFourDeckRole[activeDeckLayer === "B" ? root.scFourDeckRole.length - 1 : 0])\n'
                "        : parseInt(root.activeDecks[0])\n",
            ),
            (
                "    readonly property int passiveDeck: root.passiveDecks.length > 0 ? parseInt(root.passiveDecks[0]) : -1\n",
                '    readonly property int passiveDeck: root.scFourDeckRole !== ""\n'
                '        ? (root.scFourDeckRole.length > 1 ? parseInt(root.scFourDeckRole[activeDeckLayer === "B" ? 0 : root.scFourDeckRole.length - 1]) : -1)\n'
                "        : (root.passiveDecks.length > 0 ? parseInt(root.passiveDecks[0]) : -1)\n",
            ),
            (
                '    readonly property QObjProperty pActiveDeckLayer: Planck.getProperty("/GUI/Decks/Deck/Layer")\n'
                "    readonly property string activeDeckLayer: pActiveDeckLayer && pActiveDeckLayer.translator\n"
                "        ? pActiveDeckLayer.translator.string\n"
                '        : "A"\n'
                '    readonly property int activeLayerIndex: activeDeckLayer === "A" ? 1 : 2\n'
                '    readonly property QObjProperty pDeckColor: Planck.getProperty("/Engine/Deck%1/Color".arg(activeLayerIndex))\n',
                '    readonly property QObjProperty pActiveDeckLayer: Planck.getProperty(\n'
                '        root.scFourDeckRole === "24" || root.scFourDeckRole === "2"\n'
                '            ? "/GUI/Decks/DeckRight/Layer"\n'
                '            : root.scFourDeckRole !== ""\n'
                '              ? "/GUI/Decks/DeckLeft/Layer"\n'
                '              : "/GUI/Decks/Deck/Layer"\n'
                '    )\n'
                "    readonly property string activeDeckLayer: pActiveDeckLayer && pActiveDeckLayer.translator\n"
                '        ? (pActiveDeckLayer.translator.index === 1 ? "B" : "A")\n'
                '        : "A"\n'
                '    readonly property int activeLayerIndex: activeDeckLayer === "A" ? 1 : 2\n'
                '    readonly property QObjProperty pDeckColor: Planck.getProperty("/Engine/Deck%1/Color".arg(root.scFourDeckRole !== "" ? activeDeck : activeLayerIndex))\n',
            ),
        ]
        scaled[prefs + "Entries/DeckColorSelect.qml"] = [(
            '    readonly property string colorSelectEntry: Config.device.isPlayer ? "PlayerColorSelect" : "ConsoleColorSelect"\n',
            f'    readonly property string colorSelectEntry: {is_player} ? "PlayerColorSelect" : "ConsoleColorSelect"\n',
        )]
        scaled[prefs + "Entries/private/DualLayerColorSelect.qml"] = [(
            '        visible: Planck.getProperty("/GUI/Decks/Deck/Layer").translator.numEntries === 2\n',
            '        readonly property var scLayerTr: {\n'
            '            const p = Planck.getProperty("/GUI/Decks/Deck/Layer")\n'
            '            if (p && p.translator) return p.translator\n'
            '            const role = typeof root !== "undefined" && root && root.scFourDeckRole ? root.scFourDeckRole : ""\n'
            '            if (!role) return null\n'
            '            const fb = Planck.getProperty(role === "24" || role === "2" ? "/GUI/Decks/DeckRight/Layer" : "/GUI/Decks/DeckLeft/Layer")\n'
            '            return fb && fb.translator ? fb.translator : null\n'
            '        }\n'
            '        visible: !!scLayerTr && scLayerTr.numEntries === 2\n',
        )]
        scaled[ROOT + "StateNodes/States/PerformanceViewState.qml"] = [
            (
                "    readonly property bool isPlayer: Config.device.isPlayer\n",
                f"    readonly property bool isPlayer: {is_player}\n",
            ),
            (
                '                ? "/Client/Librarian/LoadToDeck%1".arg(root.activeDecks[0])\n',
                '                ? "/Client/Librarian/LoadToDeck%1".arg(root.activeDeck)\n',
            ),
        ]
        scaled[ROOT + "StateNodes/States/PerformanceViewStreamingState.qml"] = [
            (
                "    readonly property bool isPlayer: Config.device.isPlayer\n",
                f"    readonly property bool isPlayer: {is_player}\n",
            ),
            (
                '              ? "/Client/Librarian/LoadToDeck%1".arg(root.activeDecks[0])\n',
                '              ? "/Client/Librarian/LoadToDeck%1".arg(root.activeDeck)\n',
            ),
        ]
        scaled[ROOT + "StateNodes/ControlFocusNode.qml"] = [(
            "    property bool loadToDeckDirectly: Config.device.isPlayer\n",
            f"    property bool loadToDeckDirectly: {is_player}\n",
        )]
        scaled[prefs + "Entries/PlayerNumber.qml"] = [(
            "    active: Config.device.isPlayer\n",
            f"    active: {is_player}\n",
        )]
        scaled[prefs + "Entries/ShowLayerB.qml"] = [
            (
                "    active: Config.device.isPlayer && Config.audio.deck.hasLayers\n",
                f"    readonly property bool scIsPlayer: {is_player}\n"
                "    active: scIsPlayer && Config.audio.deck.hasLayers\n",
            ),
            (
                '        text: Config.device.isPlayer ? "Layer B" : "Deck 3/4"\n',
                '        text: scIsPlayer ? "Layer B" : "Deck 3/4"\n',
            ),
        ]
        scaled[v + "EmbeddedView.qml"].extend([
            (
                "        active: Config.device.isPlayer\n        sourceComponent: RemoteDeckPicker {}\n",
                f"        active: {is_player}\n        sourceComponent: RemoteDeckPicker {{}}\n",
            ),
            (
                "        active: Config.device.isPlayer\n        sourceComponent: EngineConnect {}\n",
                f"        active: {is_player}\n        sourceComponent: EngineConnect {{}}\n",
            ),
            (
                "            warningMessage: Config.device.isPlayer\n",
                f"            warningMessage: {is_player}\n",
            ),
        ])
        scaled[v + "EngineConnect/EngineConnect.qml"] = [
            (
                "                readonly property int numberOfAvailableDecks: Config.audio.numberOfDecks\n",
                f"                readonly property int numberOfAvailableDecks: {is_player} ? 2 : Config.audio.numberOfDecks\n",
            ),
            (
                '                        deckLayer: enabled &&\n'
                '                        Planck.getProperty("/GUI/Decks/Deck/Layer").translator.numEntries - 1 >= index\n'
                '                            ? Planck.getProperty("/GUI/Decks/Deck/Layer").translator.entries[deckLayerIndex]\n'
                '                            : "-"\n',
                '                        readonly property var scLayerTr: {\n'
                '                            const p = Planck.getProperty("/GUI/Decks/Deck/Layer")\n'
                '                            if (p && p.translator) return p.translator\n'
                '                            const role = typeof root !== "undefined" && root && root.scFourDeckRole ? root.scFourDeckRole : ""\n'
                '                            if (!role) return null\n'
                '                            const fb = Planck.getProperty(role === "24" || role === "2" ? "/GUI/Decks/DeckRight/Layer" : "/GUI/Decks/DeckLeft/Layer")\n'
                '                            return fb && fb.translator ? fb.translator : null\n'
                '                        }\n'
                "                        deckLayer: enabled && scLayerTr && scLayerTr.numEntries - 1 >= index\n"
                '                            ? (typeof root !== "undefined" && root && root.scFourDeckRole ? (deckLayerIndex === 0 ? "A" : "B") : scLayerTr.entries[deckLayerIndex])\n'
                '                            : "-"\n',
            ),
            (
                '                        readonly property QObjProperty pDeckColor: Planck.getProperty(\n'
                '                            "/Engine/Deck%1/Color".arg(deckLayer === "A" ? 1 : 2)\n'
                '                        )\n',
                '                        readonly property int scColorDeck: typeof root !== "undefined" && root && root.scFourDeckRole\n'
                '                            ? parseInt(root.scFourDeckRole[deckLayer === "A" ? 0 : root.scFourDeckRole.length - 1])\n'
                '                            : (deckLayer === "A" ? 1 : 2)\n'
                '                        readonly property QObjProperty pDeckColor: Planck.getProperty(\n'
                '                            "/Engine/Deck%1/Color".arg(scColorDeck)\n'
                '                        )\n',
            ),
        ]
        scaled[v + "TouchFx/TouchFxViewLowerBar.qml"] = [(
            "            visible: Config.device.isPlayer && PlanckGUI.views.touchFxView.hasMute\n",
            f"            visible: {is_player} && PlanckGUI.views.touchFxView.hasMute\n",
        )]
        scaled[ROOT + "Items/Base/DialogHeader.qml"] = [(
            "        if (Config.device.isPlayer) {\n",
            f"        if ({is_player}) {{\n",
        )]
        scaled[v + "DriveSelect/CloudStorageSelect.qml"] = [(
            "                showPlayerNumber: Config.device.isPlayer\n",
            f"                showPlayerNumber: {is_player}\n",
        )]
    for rel, fixes in scaled.items():
        assert not isinstance(out.get(rel), str), rel
        out.setdefault(rel, []).extend(fixes)
    for rel, fixes in legacy_qml_fixes().items():
        assert not isinstance(out.get(rel), str), rel  # a whole-file replacement already owns it
        out.setdefault(rel, []).extend(fixes)
    if v53():
        out.setdefault(ROOT + "Views/MainView/Fx/DJFxSpinner.qml", []).extend([
            ('                objectName: "DJFx_Beats_Decrease_Button"', '                objectName: "DJFx_Beats_Decrease_Button"\n                text: spinner.scMsActive ? "−" : ""'),
            ('                objectName: "DJFx_Beats_Increase_Button"', '                objectName: "DJFx_Beats_Increase_Button"\n                text: spinner.scMsActive ? "+" : ""'),
            ('                    source: Qt.resolvedUrl("img/minus.png")', '                    source: spinner.scMsActive ? "" : Qt.resolvedUrl("img/minus.png")'),
            ('                    source: Qt.resolvedUrl("img/plus.png")', '                    source: spinner.scMsActive ? "" : Qt.resolvedUrl("img/plus.png")'),
            ('    property bool active: false', '    readonly property bool scMsActive: typeof root !== "undefined" && root.scFxMsController && !!root.scFxMsController(parameter)\n    Binding { target: spinner; property: "isTouchable"; when: spinner.scMsActive; value: true; restoreMode: Binding.RestoreBindingOrValue }\n    Binding { target: header; property: "text"; when: spinner.scMsActive; value: spinner.scMsActive && root.scT ? root.scT("Time (ms)") : "Time (ms)"; restoreMode: Binding.RestoreBindingOrValue }\n    property bool active: false'),
            ('                text: parameter ? parameter.valueText : ""', '                text: spinner.scMsActive ? (root.scFxTimeMs + " ms") : (parameter ? parameter.valueText : "")'),
            ('                onClicked: parameter.decValue()', '                onClicked: { if (!spinner.scMsActive || !root.scFxMsStep(parameter, -1)) parameter.decValue() }'),
            ('                onClicked: parameter.incValue()', '                onClicked: { if (!spinner.scMsActive || !root.scFxMsStep(parameter, 1)) parameter.incValue() }'),
        ])
        out[ROOT + "Views/MainView/Fx/DJFx.qml"] = [
            ("import Planck 1.0", "import Planck 1.0\nimport Common"),
            ("    property string indicator\n", "    property string indicator\n" + FX_SELECTOR),
            ("model: djFxView ? djFxView.effectsNames : {}", "model: djFx.scFxNames"),
            ("index: djFxView ? djFxView.selectionIndex : 0", "index: djFx.scFxIndex >= 0 ? djFx.scFxIndex : (djFxView ? djFxView.selectionIndex : 0)"),
            ("onAccepted: result => djFxView.selectEffect(result)", "onAccepted: result => djFx.scFxAccept(result)"),
            ("onRejected: djFxView.abortSelecting()", "onRejected: djFx.scFxAbort()"),
            ("parameter: djFxView ? djFxView.selectParameter : null", "parameter: djFxView ? djFxView.selectParameter : null\n            text: djFx.scFxSelected >= 0 ? djFx.scFxNames[djFx.scFxSelected] : (parameter ? parameter.valueText : \"\")"),
        ]
        out["ControlSurfaceModules/ControlSurfaceModules/private/DJFxSelectKnob.qml"] = [
            ("        Item {\n", "        Item {\n" + FX_KNOB),
            ("                            fxConfig.channel[modelData].pSelectPressed.translator.state = value > 0", """                            if (value > 0) scFxPushOwned = !!scFxCanUse()
                            if (scFxPushOwned) {
                                if (value > 0) scFxAction("Accept")
                                else scFxPushOwned = false
                            } else fxConfig.channel[modelData].pSelectPressed.translator.state = value > 0"""),
            ("                    if (fxConfig.channel[modelData].selecting) {", """                    if (scFxCanUse()) { scFxAction(value > 0.5 ? "Minus" : "Plus"); return }
                    if (fxConfig.channel[modelData].selecting) {"""),
            ("""                target: PropertyTarget {
                    prop: fxConfig.channel[modelData].pEffectIndex
                }""", FX_JOG_TARGET.strip("\n")),
        ]
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
    return PatchDict({
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
                "    readonly property bool showOverView: showBrowseView && PlanckGUI.views.browseViewController.showTrackOverview\n" if v53() else
                "\treadonly property bool showOverView: showBrowseView && PlanckGUI.views.browseViewController.showTrackOverview\n",
                "    readonly property bool showOverView: showBrowseView && PlanckGUI.views.browseViewController.showTrackOverview && (typeof root === \"undefined\" || root.scTracklistWaveform === undefined || root.scTracklistWaveform)\n" if v53() else
                "\treadonly property bool showOverView: showBrowseView && PlanckGUI.views.browseViewController.showTrackOverview && (typeof root === \"undefined\" || root.scTracklistWaveform === undefined || root.scTracklistWaveform)\n",
            ),
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
    })


# Only these properties hold display text. Ids such as StateMap names never match.
TEXT_LINE = re.compile(r'^(\s*(?:[\w.]*\.)?(?:text|label|labelText|title|message|headline|info|display|description|missingContentHint|buttonText|value)\s*:)(.*)$')
BARE_LITERAL = re.compile(r'^(\s*)"([^"\\]+)"(\s*)$')
# Template literals match whole, ${...} included (`Version: ${version}`).
LITERAL = re.compile(r'"([^"\\\n]*)"|`([^`\\\n]*)`')


def load_dict():
    rows = {}
    for line in (HERE / "i18n.tsv").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            en, hans, hant = line.split("\t")
            rows[en] = {"zh-Hans": hans, "zh-Hant": hant}
    return rows


def encoded_dicts(rows):
    """Inactive languages stay scalar strings, not thousands of traced JS properties."""
    result = {}
    for lang in ("en", "zh-Hans", "zh-Hant"):
        d = {}
        for en, t in rows.items():
            to = en if lang == "en" else t[lang]
            for spelling in (en, t["zh-Hans"], t["zh-Hant"]):
                d[spelling] = to
        result[lang] = json.dumps(d, ensure_ascii=False, separators=(",", ":"))
    return result


def detect_build_tag(explicit=None, release=False):
    if release or os.environ.get("ENGINEX_RELEASE") == "1":
        return ""
    if explicit is not None:
        return explicit.strip()
    if os.environ.get("ENGINEX_BUILD_TAG"):
        return os.environ["ENGINEX_BUILD_TAG"].strip()
    try:
        sha = subprocess.check_output(
            ["git", "-C", str(HERE), "rev-parse", "--short=7", "HEAD"],
            stderr=subprocess.DEVNULL, text=True,
        ).strip()
    except Exception:
        return ""
    if not sha:
        return ""
    track = os.environ.get("ENGINEX_BUILD_TRACK", "").strip()
    if not track:
        try:
            branch = subprocess.check_output(
                ["git", "-C", str(HERE), "rev-parse", "--abbrev-ref", "HEAD"],
                stderr=subprocess.DEVNULL, text=True,
            ).strip()
        except Exception:
            branch = ""
        if "sandbox" in branch:
            track = "sbx"
        elif "shadow" in branch:
            track = "dev"
        else:
            try:
                log = subprocess.check_output(
                    ["git", "-C", str(HERE), "log", "-n", "30", "--format=%s%n%D", "HEAD"],
                    stderr=subprocess.DEVNULL, text=True,
                )
            except Exception:
                log = ""
            sbx_at = log.find("sandbox-main")
            dev_at = log.find("shadow-grid")
            if sbx_at >= 0 and (dev_at < 0 or sbx_at < dev_at):
                track = "sbx"
            else:
                track = "dev"
    return f"{track} {sha}"


def render_language(text, lang, dicts, build_tag=""):
    return (text.replace("@DICTS@", json.dumps(dicts, ensure_ascii=False))
            .replace("@LANG@", lang)
            .replace("@BUILD_TAG@", json.dumps(build_tag)))


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
    ap.add_argument("--qrc", type=Path)
    ap.add_argument("--out", type=Path)
    ap.add_argument("--dev", action="store_true", help="add the eval harness (never ship this)")
    ap.add_argument("--build-tag", default=None, help="override build tag (e.g. 'sbx 085a437')")
    ap.add_argument("--release", action="store_true", help="omit dev/sandbox commit tag from UI")
    ap.add_argument("--print-build-tag", action="store_true", help="print computed build tag and exit")
    a = ap.parse_args()
    build_tag = detect_build_tag(a.build_tag, a.release)
    if a.print_build_tag:
        print(build_tag)
        return
    if not a.qrc or not a.out:
        ap.error("--qrc and --out are required")
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
        if rel == ROOT + "EmbeddedMain.qml" and v53():
            verify_boot_ready_loader(text)
        for old, new in change:
            text = patch(text, old, new, rel)
        base[rel] = text
    for rel, kind in shadow_targets(a.qrc).items():
        text = base.get(rel)
        if text is None:
            text = (a.qrc / rel).read_text(encoding="utf-8")
        base[rel] = shadow_layer(text, kind, (a.qrc / "Skins/EngineOS/UI/Controls/ProgressRing.qml").is_file())
    # A qrc path this base does not ship renders as a blank icon (5.3.0 has no UIKit.1/).
    for rel, text in base.items():
        for ref in re.findall(r'"qrc:/([^"$%{}+]+\.(?:png|svg|jpg|qml))"', text.replace("%20", " ")):
            if not (a.qrc / ref).is_file():
                raise SystemExit(f"{rel}: qrc:/{ref} is not in {a.qrc}")
    rows = load_dict()
    skip = compared_strings(a.qrc)
    dicts = encoded_dicts(rows)
    for lang in ("en", "zh-Hans", "zh-Hant"):
        table = {en: (en if lang == "en" else t[lang]) for en, t in rows.items()}
        files = dict(base)
        for f, rel in translated_qml(a.qrc):
            source = files.get(rel) or f.read_text(encoding="utf-8", errors="strict")
            changed = translate(source, table, skip, live=rel.startswith(("Skins/EmbeddedCommon/", "Skins/Embedded/")))
            if changed != source:
                files[rel] = changed
        for rel, text in files.items():
            text = render_language(text, lang, dicts, build_tag)
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
