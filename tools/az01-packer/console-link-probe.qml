import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0

// Drives test_console_link_sandbox.sh from inside the console's Engine: for each phase it sets the
// Line In view switch (a POST to the settings endpoint, as the menu does), the channels' Line state and, in stub mode, how many linked units are present,
// waits, and logs what the QML holds. Lines: CLPHASE|BEGIN|name|epoch-ms, CLPHASE|STATE|name|..., CLPHASE|SHOT|name.
// Stub mode and pair mode run the same plan; only what answers on port 8080 differs.
Item {
    id: p
    property var eMain: null
    property int idx: -1
    property real phaseAt: 0
    property bool logged: false
    property bool shot: false
    property real layoutAt: 0
    property bool writeFailed: false
    property int wantMask: 0
    readonly property int settleMs: 8000
    readonly property int lenMs: 18000
    readonly property string planName: "@PLAN@" // stage1, send (console, stage 2) or player (a player's lanes, stage 2): set when the script copies this file
    readonly property var plan: planName === "send" ? planSend : planName === "player" ? planPlayer : planStage1
    readonly property var planStage1: [
        {n: "off", on: 0, line: [], units: 2, shot: true},
        {n: "on-noline", on: 1, line: [], units: 2, shot: false},
        {n: "on-line3", on: 1, line: [3], units: 1, shot: true},
        {n: "on-line34", on: 1, line: [3, 4], units: 2, shot: true},
        {n: "release4", on: 1, line: [3], units: 2, shot: false},
        {n: "on-line234", on: 1, line: [2, 3, 4], units: 3, shot: false},
        {n: "cdj-prolink-off", on: 1, line: [3], units: 1, cdjoff: 1, shot: true},
        {n: "release-all", on: 1, line: [], units: 3, shot: false},
        {n: "off-again", on: 0, line: [], units: 3, shot: false}
    ]
    // Stage 2, the console (a stub answers the held request: it says whether a player asks): Stock, Line In with nobody asking, a
    // player asks, nobody asks again, Stock again (the answer says "asks" but the QML must not even look).
    readonly property var planSend: [
        {n: "x-off", on: 0, line: [], units: 0, asked: 0, shot: false},
        {n: "x-idle", on: 1, line: [], units: 0, asked: 0, shot: false},
        {n: "x-asked", on: 1, line: [], units: 0, asked: 1, shot: false},
        {n: "x-quiet", on: 1, line: [], units: 0, asked: 0, shot: false},
        {n: "x-off-again", on: 0, line: [], units: 0, asked: 1, shot: false}
    ]
    // Stage 2, a player's lanes: this Engine is told it is a player in the 4-deck layout (role 1+2, lanes 3 and 4 free); the stub, as
    // the player's stream-sync, shows two console decks there with the row on.
    readonly property var planPlayer: [
        {n: "p-row-off", on: 0, line: [], units: 0, row: 0, shot: true},
        {n: "p-row-on", on: 0, line: [], units: 0, row: 1, shot: true},
        {n: "p-row-off-again", on: 0, line: [], units: 0, row: 0, shot: false}
    ]
    function log(msg) { console.warn("CLPHASE|" + String(msg).replace(/ /g, "_")) } // the engine log keeps a message only up to its first space
    function findMain(o) {
        if (!o) return null
        if ("scUiLoaded" in o && "scFourDeckRole" in o) return o
        if (o.children) for (let i = 0; i < o.children.length; i++) { const r = findMain(o.children[i]); if (r) return r }
        return null
    }
    function collect(o, name, out) {
        if (!o) return
        if (o.objectName === name) out.push(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) collect(o.children[i], name, out)
    }
    // The stock first-start screens and the Stem Separation announcement sit over the performance view; the other probes
    // (layer-switch-probe.qml) close them the same way.
    function tr(path) { const q = Planck.getProperty(path); return q ? q.translator : null }
    function pressDismiss(o) {
        if (!o) return
        if ((o.objectName === "DismissButton" || o.objectName === "NextButton") && typeof o.clicked === "function") o.clicked()
        for (let i = 0; o.children && i < o.children.length; i++) pressDismiss(o.children[i])
    }
    function dismiss() {
        if (typeof PlanckGUI === "undefined" || !PlanckGUI.views) return
        const v = PlanckGUI.views
        if (v.outOfBoxUpdateScreenShowing) v.outOfBoxUpdateScreenShowing = false
        if (v.onBoardingController) v.onBoardingController.close()
        if (v.prompts && typeof v.prompts.drop === "function") v.prompts.drop("StemsDownload")
        if (v.sourceScreenView && typeof v.sourceScreenView.close === "function") v.sourceScreenView.close()
        for (const path of ["/GUI/ShowWifiView", "/GUI/ShowBrowseView"]) { const t = tr(path); if (t) t.state = false }
        const close = tr("/GUI/CloseDialogs"); if (close) close.state = true
        pressDismiss(Window.window ? Window.window.contentItem : null)
    }
    // A lane for channel 3 or 4 needs the performance view to show four waveforms: stock's own Layout choice, which the Line In
    // view never changes. The capture run picks it the way the Layout menu does (as scPairedApplyLayout does).
    function fourWaveforms() {
        const m = eMain && eMain.scViewControls ? eMain.scViewControls() : null
        if (!m || !m.deckCount || !m.waveformCount) return false
        const pick = (set, word) => { const i = set.settingNames.indexOf(word); return i >= 0 ? i : set.settingNames.length - 1 }
        if (m.deckCount.currentName !== "4 Decks") m.deckCount.setCurrentIndex(pick(m.deckCount, "4 Decks"))
        if (m.waveformCount.currentName !== "4 Waveforms") m.waveformCount.setCurrentIndex(pick(m.waveformCount, "4 Waveforms"))
        return true
    }
    function layoutNames() {
        const m = eMain && eMain.scViewControls ? eMain.scViewControls() : null
        return m && m.deckCount && m.waveformCount ? m.deckCount.currentName + "/" + m.waveformCount.currentName : "none"
    }
    function post(url, done) {
        const x = new XMLHttpRequest()
        x.open("POST", url)
        if (done) x.onreadystatechange = function() { if (x.readyState === 4) done() }
        x.send()
    }
    function setLines(list) {
        let mask = 0
        for (let d = 1; d <= 4; d++) {
            const want = list.indexOf(d) >= 0
            if (want) mask |= 1 << d
            const pr = Planck.getProperty("/Engine/Mixer/Channel" + d + "/Line")
            const t = pr ? pr.translator : null
            if (t) { try { t.state = want } catch (e) {} }
            if (!t || !!t.state !== want) writeFailed = true
        }
        wantMask = mask
        log("LINEWRITE|" + (writeFailed ? "the mixer property would not take the write: the probe sets scLineMask" : "mixer property written, the QML follows its signal"))
    }
    // The busy line of every visible lane (the reason a lane gives while it has no waveform to draw).
    function busyTexts() {
        const rows = [], out = []
        collect(eMain.Window.window ? eMain.Window.window.contentItem : null, "EngineX_Shadow_Busy", rows)
        for (const r of rows) {
            if (!r.visible) continue
            for (let i = 0; i < r.children.length; i++) {
                const c = r.children[i]
                if (typeof c.text === "string" && c.text.trim() !== "") out.push(c.text.trim())
            }
        }
        return out
    }
    function timers() {
        const out = []
        const m = eMain
        for (let i = 0; i < m.data.length; i++) {
            const o = m.data[i]
            if (o && typeof o.running === "boolean" && typeof o.interval === "number" && typeof o.repeat === "boolean" && o.running) out.push(o.interval + (o.repeat ? "r" : "o"))
        }
        out.sort()
        return out.join(",")
    }
    function state(name) {
        const m = eMain
        const each = f => [1, 2, 3, 4].map(d => f(d) ? 1 : 0).join("")
        const labels = [], lanes = []
        collect(m.Window.window ? m.Window.window.contentItem : null, "EngineX_Shadow_Unit", labels)
        const texts = []
        for (const l of labels) if (l.visible) texts.push(l.text)
        // The engine log cuts a message at about 118 characters: one short line per fact.
        log("STATE|" + name + "|on=" + (m.scConsoleLine ? 1 : 0) + "|mask=" + m.scLineMask + "|live=" + (m.scLineLive ? 1 : 0) + "|role=" + (m.scFourDeckRole || "-") +
            "|lk=" + each(d => m.scDeckLocked(d)) + "|vis=" + each(d => m.scShadowVisible(d)) +
            "|ld=" + each(d => typeof m.scRemoteLocal === "function" ? m.scRemoteLocal(d) : !m.scDeckLocked(d)))
        log("TM|" + name + "|[" + timers() + "]")
        log("LABELS|" + name + "|" + JSON.stringify(texts))
        log("HYBRID|" + name + "|" + (Config.device.hasHybridMode ? 1 : 0))
        log("LAYOUTNOW|" + name + "|" + layoutNames())
        log("BUSY|" + name + "|" + JSON.stringify(busyTexts()))
        log("S2|" + name + "|asked=" + (m.scConsoleAsked ? 1 : 0) + "|line=" + (m.scConsoleLine ? 1 : 0) + "|row=" + (m.scShowConsole ? 1 : 0) + "|role=" + (m.scFourDeckRole || "-"))
        const cl = []
        collect(m.Window.window ? m.Window.window.contentItem : null, "EngineX_Shadow_Console", cl)
        const consoleTexts = []
        for (const l of cl) if (l.visible) consoleTexts.push(l.text)
        log("CONSOLELABELS|" + name + "|" + JSON.stringify(consoleTexts))
    }
    property int pendingPhase: -1
    // The stub's answer is set before the channels change: a poll caused by the channel change must meet the units of
    // this phase, or it shows (and fetches the picture of) a unit that is only there for the phase before. A real
    // stream-sync answers /ctl with 404, which is as good as an answer; the timer covers a server that does not.
    function applyPhase(i) {
        if (pendingPhase !== i || idx !== i) return
        pendingPhase = -1
        const ph = plan[i]
        setLines(ph.line)
        if (writeFailed && eMain.scConsoleLine) eMain.scLineMask = wantMask
        // A channel that went back to its Engine input has let go of its lane in this very turn, before any poll.
        if (ph.n === "release4") state("release4-now")
        // Stock's own Layout choice, set again at every phase in case the view system put its saved layout back.
        if (ph.line.length > 0 || i === 0) { const ok = fourWaveforms(); log("LAYOUT|" + ph.n + "|" + (ok ? "set_" + layoutNames() : "not_available")) }
    }
    function begin(i) {
        idx = i; phaseAt = Date.now(); logged = false; shot = false; layoutAt = 3000
        const ph = plan[i]
        log("BEGIN|" + ph.n + "|" + phaseAt)
        post("http://127.0.0.1:8080/v1/enginex/ui?consoleLineView=" + ph.on)
        pendingPhase = i
        applyTimer.restart()
        post("http://127.0.0.1:8080/ctl?units=" + ph.units + "&cdjoff=" + (ph.cdjoff || 0) + "&asked=" + (ph.asked || 0) + "&row=" + (ph.row || 0), function() { p.applyPhase(i) }) // stub only: the linked units on the network in this phase
    }
    Timer { id: applyTimer; interval: 1500; repeat: false; onTriggered: p.applyPhase(p.pendingPhase) }
    Timer {
        interval: 250; running: true; repeat: true
        onTriggered: {
            if (!p.eMain) p.eMain = p.findMain(p.Window.window ? p.Window.window.contentItem : null)
            const m = p.eMain
            if (!m || !m.scUiLoaded) return
            if (p.idx < 0 || Date.now() - p.phaseAt < 4000) p.dismiss()
            if (p.idx < 0) { p.begin(0); return }
            const el = Date.now() - p.phaseAt
            // When the mixer property does not take a write, the probe plays the mixer's part and keeps the mask.
            if (p.writeFailed && m.scConsoleLine && m.scLineMask !== p.wantMask) m.scLineMask = p.wantMask
            const ph = p.plan[p.idx]
            // The first phase sets the layout while the view system is still starting up, and something there puts the two-deck view
            // back within the first seconds: it is set again every 3 s in every phase (a no-op once it holds), so that every capture
            // is the same 4 Decks / 4 Waveforms view and only the Line In view differs.
            if (el > p.layoutAt) { p.layoutAt += 3000; p.fourWaveforms() }
            if (!p.logged && el > p.lenMs - 700) {
                p.logged = true
                p.state(ph.n)
                if (ph.shot) {
                    const win = m.Window.window ? m.Window.window.contentItem : null
                    if (win) win.grabToImage(function(r) { p.log("SHOT|" + ph.n + "|" + r.saveToFile("/tmp/cl-shot-" + ph.n + ".png")) })
                }
            }
            // The release phase also reports how fast a lane goes: the state is read in the tick that turned the channel off.
            if (el > p.lenMs) {
                if (p.idx + 1 < p.plan.length) { p.writeFailed = false; p.begin(p.idx + 1) }
                else { p.log("DONE"); p.idx = 99; Qt.quit() }
            }
        }
    }
}
