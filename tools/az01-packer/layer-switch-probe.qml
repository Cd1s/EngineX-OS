// Sandbox only, replaces QmlWarnings.qml (layer-switch-sandbox.sh). Loads and plays every deck,
// then toggles the deck layers and logs when each toggle starts; the fixture logs every frame.
import QtQuick
import QtQuick.Window
import Planck
Item {
    id: probe
    visible: false
    readonly property int runs: @RUNS@
    readonly property bool load: @LOAD@ // false: empty decks (a model whose audio the sandbox cannot fake)
    readonly property bool census: @CENSUS@ // list the objects each toggle creates (not for timing)
    property var seen: null
    readonly property bool player: Config.device.isPlayer
    readonly property int decks: player ? 2 : 4
    property int stage: 0
    property int deck: 1
    property int run: 0
    property real waitUntil: Date.now() + 15000
    property real deadline: 0
    property real t0: 0
    property real tPre: 0
    property real cpuPre: 0
    property real cpu0: 0
    function tr(path) { const p = Planck.getProperty(path); return p ? p.translator : null }
    function log(s) { console.warn("LSW " + s + "|") }
    // CPU time of this (GUI) thread in ms: the switch's QML work, apart from the llvmpipe frames.
    function cpu() {
        const x = new XMLHttpRequest()
        x.open("GET", "file:///proc/thread-self/schedstat", false); x.send()
        return Number(x.responseText.split(" ")[0]) / 1e6
    }
    function side() { return player ? "" : run % 2 ? "Right" : "Left" }
    function layerTr() { return tr("/GUI/Decks/Deck" + side() + "/Layer") }
    function snapshot() {
        const all = new Map(), stack = [probe.Window.window.contentItem]
        while (stack.length) {
            const o = stack.pop(), key = String(o)
            if (!o || all.has(key)) continue
            all.set(key, key.replace(/_QML(TYPE)?_\d+/, "").replace(/\(0x[0-9a-f]+.*$/, ""))
            const kids = o.data || o.children
            for (let i = 0; kids && i < kids.length; i++) stack.push(kids[i])
        }
        return all
    }
    function created(before, after) {
        const n = {}
        for (const [k, t] of after) if (!before.has(k)) n[t] = (n[t] || 0) + 1
        return Object.entries(n).sort((a, b) => b[1] - a[1]).map(e => e[0] + "=" + e[1]).join(",")
    }
    function dismiss(o) {
        if (!o) return
        if ((o.objectName === "DismissButton" || o.objectName === "NextButton") && typeof o.clicked === "function") o.clicked()
        for (let i = 0; o.children && i < o.children.length; i++) dismiss(o.children[i])
    }
    function closeFirstRun() {
        const v = PlanckGUI.views
        if (v.onBoardingController) v.onBoardingController.close()
        if (v.prompts && typeof v.prompts.drop === "function") v.prompts.drop("StemsDownload")
        if (v.sourceScreenView && typeof v.sourceScreenView.close === "function") v.sourceScreenView.close()
        for (const path of ["/GUI/ShowWifiView", "/GUI/ShowBrowseView"]) { const t = tr(path); if (t) t.state = false }
        const close = tr("/GUI/CloseDialogs"); if (close) close.state = true
        dismiss(probe.Window.window && probe.Window.window.contentItem)
    }
    Timer {
        interval: 100; running: true; repeat: true
        onTriggered: {
            if (Date.now() < probe.waitUntil) return
            try { probe.step(Date.now()) } catch (e) { probe.log("ERROR|" + e); running = false }
        }
    }
    function step(now) {
        if (stage === 0) {
            closeFirstRun()
            log("CONFIG|" + Config.device.name + "|player=" + player + "|fourDeckMode=" +
                (PlanckGUI.views.layoutObserver && PlanckGUI.views.layoutObserver.fourDeckMode))
            stage = load ? 1 : 3; deadline = now + 120000
            if (load) tr("/Client/Librarian/AddToDatabaseAndLoadToDeck" + deck).url = "file:///data/music/t" + deck + ".wav"
        } else if (stage === 1) {
            if (!tr("/Engine/Deck" + deck + "/Track/SongLoaded").state) {
                if (now > deadline) throw new Error("deck " + deck + " did not load")
                return
            }
            log("LOADED|" + deck)
            if (deck < decks) {
                deck++; deadline = now + 120000
                tr("/Client/Librarian/AddToDatabaseAndLoadToDeck" + deck).url = "file:///data/music/t" + deck + ".wav"
                return
            }
            for (let d = 1; d <= decks; d++) tr("/Engine/Deck" + d + "/Play").state = true
            closeFirstRun()
            stage = 2; waitUntil = now + 20000 // analysis and waveforms settle while playing
        } else if (stage === 2) {
            for (let d = 1; d <= decks; d++) {
                const playing = tr("/Engine/Deck" + d + "/PlayState").state
                log("PLAY|" + d + "|" + playing + "|" + tr("/Engine/Deck" + d + "/Track/TrackPosition").unnormalized)
                if (!playing) throw new Error("deck " + d + " is not playing")
            }
            stage = 3
        } else if (stage === 3) { // a quiet second before each toggle is the reference
            if (run === runs) { log("DONE|PASS"); stage = 9; return }
            tPre = Date.now(); cpuPre = cpu(); stage = 4; waitUntil = now + 1000
        } else if (stage === 4) {
            const t = layerTr(), before = t.index
            if (census) seen = snapshot()
            cpu0 = cpu(); cpuPre = cpu0 - cpuPre
            t0 = Date.now(); tPre = t0 - tPre
            if (player) t.incValue(0, 1, 0, true); else t.state = !t.state
            const set = Date.now() - t0, cpuSet = cpu() - cpu0
            log("T0|" + run + "|" + (side() || "Deck") + "|" + t0 + "|set=" + set + "|from=" + before + "|to=" + t.index + "|cpuset=" + Math.round(cpuSet))
            stage = 5; waitUntil = t0 + 1000
        } else if (stage === 5) {
            // GUI CPU in the second after the toggle, less the reference second's (scaled to length).
            const post = cpu() - cpu0, span = Date.now() - t0
            log("GUI|" + run + "|extra=" + Math.round(post - cpuPre * span / tPre) + "|post=" + Math.round(post) + "|pre=" + Math.round(cpuPre * span / tPre))
            if (census) { const after = snapshot(); log("NEW|" + run + "|" + created(seen, after)); log("GONE|" + run + "|" + created(after, seen)) }
            run++; stage = 3; waitUntil = now + 1000
        }
    }
}
