// Throwaway: EMain CPU while one track plays with the cue countdown visible; Engine beat property dump.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property bool off: @OFF@
    property int step: 0
    property real readyAt: 0
    property real stepStarted: 0
    property int polls: 0
    property var ctx: ({})
    function say(text) { console.warn("X166 CD|" + text + "|") }
    function opt(path) { const p = Planck.getProperty(path); return p && p.translator ? p.translator : null }
    function tr(path) { const t = opt(path); if (!t) throw new Error("missing " + path); return t }
    function g(sub) { return tr("/Engine/Deck1" + sub) }
    function num(x) { return typeof x === "number" ? Math.round(x * 1000) / 1000 : x }
    function sr() { return g("/Track/SampleRate").value }
    function len() { return g("/Track/TrackLength").unnormalized }
    function pos() { return g("/Track/TrackPosition").unnormalized }
    function seekSeconds(s) { g("/Track/SetSongPosition").value = s * sr() / len() }
    function cpu() {
        const x = new XMLHttpRequest()
        x.open("GET", "file:///proc/thread-self/schedstat", false); x.send()
        return Number(x.responseText.split(" ")[0]) / 1e6
    }
    function caps() {
        const found = [], seen = new Set()
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.objectName === "EngineX_CueCountdown") found.push(o)
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
            if (o.item) walk(o.item)
        }
        walk(probe.Window.window.contentItem)
        return found
    }
    function shown() { return caps().map(c => c.visible ? (c.info ? c.info.text : "none") : "hidden").join(",") }
    function dismiss() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        PlanckGUI.views.prompts.drop("StemsDownload")
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
    }
    function dismissOverlays() {
        const win = probe.Window.window, roots = [win.contentItem]
        if (win.overlay) roots.push(win.overlay)
        const seen = new Set()
        function press(label) {
            for (let a = label, n = 0; a && n < 5; a = a.parent, n++) {
                if (typeof a.clicked === "function") { try { a.clicked(); return true } catch (e) {} }
                for (let i = 0; a.children && i < a.children.length; i++) {
                    const c = a.children[i]
                    if (c !== label && typeof c.clicked === "function" && c.width > 0) { try { c.clicked(null); return true } catch (e) {} }
                }
            }
            return false
        }
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.text === "Dismiss" && o.visible) press(o)
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
            if (o.item) walk(o.item)
        }
        for (const r of roots) walk(r)
    }
    // One short line per property (console lines are cut at about 100 characters).
    function dump(tag) {
        say(tag + " pos=" + num(pos()) + " smp " + num(pos() / sr()) + " s cd=" + shown())
        for (const n of ["/BeatNumber", "/Track/BarsElapsed", "/Track/BarsRemaining", "/Track/FirstDownbeatSamplePosition", "/Track/FirstAnchorTempo",
            "/Track/TrackHasAnchors", "/Track/HasAnchoredBeatGrid", "/OriginalBPM", "/CurrentBPM", "/Track/ElapsedTime", "/Track/CuePosition", "/PlayState", "/Track/CueLEDState"]) {
            const t = opt("/Engine/Deck1" + n)
            say(tag + " " + n + " " + (t ? "u=" + num(t.unnormalized) + " v=" + num(t.value) + " s=" + String(t.string).slice(0, 12) + " st=" + t.state : "absent"))
        }
    }
    property int nBeat: 0
    property int nPos: 0
    property int nBar: 0
    property int nInfo: 0
    property int nBeatInt: 0
    property int lastBeat: -9
    property bool counting: false
    property var tBeat: null
    property var tPos: null
    property var tBar: null
    property var cap: null
    Connections { target: probe.tBeat; enabled: probe.counting; function onUnnormalizedChanged() { probe.nBeat++; const n = Math.floor(probe.tBeat.unnormalized); if (n !== probe.lastBeat) { probe.lastBeat = n; probe.nBeatInt++ } } }
    Connections { target: probe.tPos; enabled: probe.counting; function onUnnormalizedChanged() { probe.nPos++ } }
    Connections { target: probe.tBar; enabled: probe.counting; function onStringChanged() { probe.nBar++ } }
    Connections { target: probe.cap; enabled: probe.counting; function onInfoChanged() { probe.nInfo++ } }
    readonly property var steps: [
        [500, function() {
            dismiss()
            for (let n = 1; n <= 4; n++) { const p = opt("/Engine/Deck" + n + "/Play"); if (p) p.state = false }
            tr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url = "file:///data/music/cd-test.wav"
        }],
        [2000, function() {
            const loaded = opt("/Engine/Deck1/Track/SongLoaded"), o = opt("/Engine/Deck1/OriginalBPM")
            ++probe.polls
            return !!loaded && loaded.state && !!o && o.unnormalized > 60 && Math.abs(o.unnormalized - 120) > 0.1 && len() > 0
        }],
        [800, function() {
            say("track " + num(len()) + " smp rate " + sr() + " polls " + probe.polls)
            g("/Track/QuickCue1SetPosition").value = 80.0 * sr()
            dismissOverlays()
        }],
        [900, function() { dismissOverlays(); seekSeconds(5.0) }],
        [900, function() { dump("p5"); seekSeconds(20.5) }],
        [900, function() { dump("p20.5"); seekSeconds(60.0) }],
        [900, function() { dump("p60"); seekSeconds(0.1) }],
        [900, function() { dump("p0.1"); seekSeconds(5.0) }],
        // CUE hold (preview): which property moves the playhead with PlayState off?
        [600, function() { const c = opt("/Engine/Deck1/Cue"); say("cue prop " + (c ? "present st=" + c.state : "absent")); ctx.p0 = pos(); ctx.s0 = shown(); if (c) c.state = true }],
        [2500, function() { dump("cuehold"); say("cuehold moved " + num((pos() - ctx.p0) / sr()) + " s cd " + ctx.s0 + " -> " + shown()); const c = opt("/Engine/Deck1/Cue"); if (c) c.state = false }],
        [900, function() { seekSeconds(5.0) }],
        [900, function() {
            if (probe.off) root.scCueCountdown = false
            probe.tBeat = opt("/Engine/Deck1/BeatNumber"); probe.tPos = opt("/Engine/Deck1/Track/TrackPosition"); probe.tBar = opt("/Engine/Deck1/Track/BarsElapsed")
            probe.cap = caps().find(c => c.visible) || null
            tr("/Engine/Deck1/Play").state = true
        }],
        [2000, function() { probe.counting = true }],
        [4000, function() {
            probe.counting = false
            say("notify 4s: beat=" + probe.nBeat + " beatInt=" + probe.nBeatInt + " pos=" + probe.nPos + " bar=" + probe.nBar + " info=" + probe.nInfo)
            ctx.c0 = cpu(); ctx.t0 = Date.now(); ctx.x0 = pos()
        }],
        [30000, function() {}],
        [300, function() {
            const c1 = cpu(), t1 = Date.now()
            say("CPU emain " + num((c1 - ctx.c0) / (t1 - ctx.t0) * 1000) + " ms/s over " + (t1 - ctx.t0) + " ms; played " + num((pos() - ctx.x0) / sr()) + " s")
            say("end countdown=" + root.scCueCountdown + " cap=" + shown() + " PlayState=" + tr("/Engine/Deck1/PlayState").state)
            console.warn("X166 DONE|PASS|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }]
    ]
    Timer {
        interval: 250
        running: true
        repeat: true
        onTriggered: {
            const now = Date.now()
            if (now < probe.readyAt || probe.step >= probe.steps.length) return
            if (probe.stepStarted === 0) probe.stepStarted = now
            try {
                const done = probe.steps[probe.step][1]()
                if (done === false && now - probe.stepStarted < 120000) return
                if (done === false) throw new Error("step " + probe.step + " did not finish")
                probe.readyAt = now + probe.steps[probe.step][0]
                probe.stepStarted = 0
                probe.step++
            } catch (e) {
                console.warn("X166 ERROR|step " + probe.step + ": " + e + "|")
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
