// Sandbox only: loaded over QmlWarnings.qml by cue-countdown-sandbox.sh (it fills in @BOARD@).
// Loads a real 128 BPM track with hot cues on Deck 1, then reads the #151 countdown (a count made afresh, and the capsules) while the
// playhead, the pitch fader and the beat grid are moved through Engine's own properties and its Beat Grid Edit controller.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    property int step: 0
    property real readyAt: 0
    property real stepStarted: 0
    property var ed: null
    property var ctx: ({})
    property int polls: 0
    property int failures: 0
    property var rows: ({})
    function say(text) { console.warn("X166 CD|" + text + "|") }
    // A failed expectation is reported and counted; the run goes on so the data and pictures are complete.
    function expect(ok, message) {
        if (ok) { say("PASS " + message); return }
        failures++
        console.warn("X166 ERROR|" + message + "|")
    }
    function check(ok, message) {
        if (!ok) throw new Error(message)
        say("PASS " + message)
    }
    function opt(path) { const p = Planck.getProperty(path); return p && p.translator ? p.translator : null }
    function tr(path) { const t = opt(path); if (!t) throw new Error("missing " + path); return t }
    function g(sub) { return tr("/Engine/Deck1" + sub) }
    function num(x) { return typeof x === "number" ? Math.round(x * 1000) / 1000 : x }
    function peek(sub) {
        const t = opt("/Engine/Deck1" + sub)
        return t ? num(t.unnormalized) + "/" + num(t.value) : "absent"
    }
    function cd() { const i = root.scCueCount(1, root.scCueTableOf(1)); return i ? i.text + " " + i.label : "null" }
    function sr() { return g("/Track/SampleRate").value }
    function len() { return g("/Track/TrackLength").unnormalized }
    function pos() { return g("/Track/TrackPosition").unnormalized }
    function seekSeconds(s) { g("/Track/SetSongPosition").value = s * sr() / len() }
    function snapshot(tag) {
        say(tag + " pos=" + num(pos() / sr()) + "s orig=" + peek("/OriginalBPM") + " cur=" + peek("/CurrentBPM")
            + " beat=" + peek("/BeatNumber") + " cd=" + cd())
    }
    // The shipped header capsules (every layout variant of Deck 1's header), found once.
    property var capList: null
    function capsules() {
        if (capList) return capList
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
        capList = found
        return found
    }
    function shown() {
        return capsules().map(c => c.visible ? (c.info ? c.info.text : "none") : "hidden").join(",")
    }
    function report(tag) {
        const fresh = root.scCueCount(1, root.scCueTableOf(1))
        say(tag + " pos=" + num(pos() / sr()) + "s orig=" + peek("/OriginalBPM") + " fresh=" + (fresh ? fresh.text : "null") + " capsules(" + capsules().length + ")=" + shown())
    }
    function dismiss() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        PlanckGUI.views.prompts.drop("StemsDownload")
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
    }
    // First-run announcements (Stem Separation, ...) cover the deck: press their Dismiss: the label's nearest clickable ancestor.
    function dismissOverlays() {
        const win = probe.Window.window, roots = [win.contentItem]
        if (win.overlay) roots.push(win.overlay)
        let pressed = 0
        const seen = new Set()
        function press(label) {
            for (let a = label, n = 0; a && n < 5; a = a.parent, n++) {
                if (typeof a.clicked === "function") { try { a.clicked(); return true } catch (e) { say("dismiss: " + e) } }
                for (let i = 0; a.children && i < a.children.length; i++) {
                    const c = a.children[i]
                    if (c !== label && typeof c.clicked === "function" && c.width > 0) { try { c.clicked(null); return true } catch (e) { say("dismiss: " + e) } }
                }
            }
            return false
        }
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.text === "Dismiss" && o.visible && press(o)) pressed++
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
            if (o.item) walk(o.item)
        }
        for (const r of roots) walk(r)
        return pressed
    }
    // The window, and the header around the first visible Deck 1 capsule at a few levels up (an overlay does not hide those).
    function shot(tag) {
        probe.Window.window.contentItem.grabToImage(function(r) { say("picture " + tag + " " + r.saveToFile("/tmp/cd-" + board + "-" + tag + ".png")) })
        const cap = capsules().find(c => c.visible)
        for (let a = cap && cap.parent, n = 1; a && n <= 6; a = a.parent, n++) {
            const level = n, item = a
            say("level " + level + " " + (item.objectName || "-") + " " + Math.round(item.width) + "x" + Math.round(item.height) + " visible=" + item.visible)
            if (item.width > 0 && item.height > 0 && item.width <= 1280)
                item.grabToImage(function(r) { say("picture " + tag + "-level" + level + " " + r.saveToFile("/tmp/cd-" + board + "-" + tag + "-level" + level + ".png")) })
        }
    }
    // The countdown steps where the playhead crosses a grid line (Engine's BeatNumber: a position in the bar, an integer on each
    // line), whatever the cue's own place between lines is. Rows of one scan are compared with the one before.
    function row(tag, wanted) {
        const fresh = root.scCueCount(1, root.scCueTableOf(1)), text = fresh ? fresh.text : "null", beat = g("/BeatNumber").unnormalized
        const list = rows[tag] || (rows[tag] = [])
        const prev = list.length ? list[list.length - 1] : null
        const here = {pos: pos() / sr(), beat: beat, text: text}
        list.push(here)
        say("scan|" + tag + "|want=" + num(wanted) + "|pos=" + num(here.pos) + "|beat=" + num(beat) + "|fresh=" + text + "|cap=" + shown())
        if (!prev || prev.pos === here.pos) return
        expect(text === (shown().split(",")[0]), "scan " + tag + " " + num(here.pos) + " s: the header capsule shows " + text)
        expect((text !== prev.text) === (Math.floor(beat) !== Math.floor(prev.beat)),
               "scan " + tag + " " + num(prev.pos) + " -> " + num(here.pos) + " s: " + prev.text + " -> " + text + " with beat " + num(prev.beat) + " -> " + num(beat))
    }
    // Positions every 50 ms across 0.75 s (a little more than one beat at 128 BPM), a settle after each seek.
    function scan(tag) {
        const list = [], n = 16
        for (let k = 0; k <= n; k++) list.push([650, function() {
            if (k > 0) row(tag, 5.0 + (k - 1) * 0.05)
            if (k < n) seekSeconds(5.0 + k * 0.05)
        }])
        return list
    }
    // One entry per step: [settle ms after it, function; returns false to be called again (polling, up to 240 s)].
    readonly property var setup: [
        [500, function() {
            dismiss()
            for (let n = 1; n <= 4; n++) { const p = opt("/Engine/Deck" + n + "/Play"); if (p) p.state = false }
            tr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url = "file:///data/music/cd-test.wav"
        }],
        [2000, function() {
            const loaded = opt("/Engine/Deck1/Track/SongLoaded"), o = opt("/Engine/Deck1/OriginalBPM")
            if (++probe.polls % 20 === 1) say("load poll " + probe.polls + " loaded=" + (loaded ? loaded.state : "absent") + " orig=" + (o ? num(o.unnormalized) : "absent"))
            return !!loaded && loaded.state && !!o && o.unnormalized > 60 && Math.abs(o.unnormalized - 120) > 0.1 && len() > 0
        }],
        [500, function() {
            // The grid is 128 BPM with its first beat at 0.30 s: a beat line every 0.46875 s. Cue 1 is 0.45 beat off a line.
            say("track " + num(len() / sr()) + " s, rate " + sr() + ", orig " + peek("/OriginalBPM"))
            g("/Track/QuickCue1SetPosition").value = 20.2 * sr()
            g("/Track/QuickCue2SetPosition").value = 40.0 * sr()
            g("/Track/QuickCue3SetPosition").value = 61.0 * sr()
        }],
        [800, function() {
            say("dismissed " + dismissOverlays() + " announcement(s)")
            expect(capsules().some(c => c.visible), "a header capsule is shown for Deck 1 (" + capsules().length + " found)")
            seekSeconds(5.0)
        }]
    ]
    readonly property var openEditor: [
        [900, function() { tr("/GUI/ShowBeatGridEditor").state = true }],
        [900, function() {
            for (let loc = 0; loc < 4 && !ed; loc++) { const e = PlanckGUI.views.beatGridEditor(loc); if (e) ed = e }
            if (!ed) return false
        }]
    ]
    // Beat Grid Edit: Shift Right (the lines move under the cue), then Halve Tempo (the lines get twice as far apart).
    readonly property var shiftGrid: [
        [300, function() { ed.setShiftingGridRight(true) }],
        [1300, function() { ed.setShiftingGridRight(false) }],
        [900, function() { dump("grid shifted"); seekSeconds(5.0) }]
    ]
    readonly property var raiseBpm: [
        [300, function() { ed.halveTempo() }],
        [1500, function() { say("tempo edited: " + peek("/OriginalBPM")); expect(g("/OriginalBPM").unnormalized < 70, "Halve Tempo changed Engine's tempo"); seekSeconds(5.0) }]
    ]
    // Pitch moves the playhead faster, not the number of beats between it and the cue.
    readonly property var pitch: [
        [900, function() { ctx.before = shown(); tr("/Engine/Deck1/Speed").value = 1.0 }],
        [1200, function() {
            expect(opt("/Engine/Deck1/CurrentBPM").unnormalized > opt("/Engine/Deck1/OriginalBPM").unnormalized * 1.05, "the pitch fader raised the current tempo")
            expect(shown() === ctx.before, "pitch +8 % leaves the countdown (" + ctx.before + " -> " + shown() + ")")
            tr("/Engine/Deck1/Speed").value = 0.5
        }]
    ]
    // Engine's beat data, as the sandbox's Engine publishes it (read-only dump), and how often it notifies while the deck plays.
    property int bnCount: 0
    property int baCount: 0
    property var bnT: null
    property var baT: null
    Connections { target: probe.bnT; function onUnnormalizedChanged() { probe.bnCount++ } }
    Connections { target: probe.baT; function onStringChanged() { probe.baCount++ } }
    function dump(tag) {
        const names = ["/BeatNumber", "/Track/BarsElapsed", "/Track/BarsRemaining", "/Track/ElapsedTime", "/Track/FirstDownbeatSamplePosition",
            "/Track/FirstAnchorTempo", "/Track/TrackHasAnchors", "/Track/HasAnchoredBeatGrid", "/OriginalBPM", "/CurrentBPM", "/Track/MetaDataBPM"]
        let out = tag + " pos=" + num(pos() / sr()) + "s"
        for (const n of names) {
            const t = opt("/Engine/Deck1" + n)
            out += " " + n.replace("/Track/", "T/") + "=" + (t ? (num(t.unnormalized) + "/" + num(t.value) + "/" + String(t.string).slice(0, 10) + "/" + t.state) : "absent")
        }
        say(out)
    }
    property var beatSeen: ({t: 0, n: -1})
    property var textSeen: ""
    property var lags: []
    property var sampled: ({beats: 0, texts: 0})
    Timer {
        id: sampler
        interval: 15; repeat: true; running: false
        onTriggered: {
            const now = Date.now(), n = Math.floor(g("/BeatNumber").unnormalized), text = shown().split(",")[0]
            if (n !== probe.beatSeen.n) { probe.beatSeen = {t: now, n: n, bar: 0}; probe.sampled.beats++ }
            if (text !== probe.textSeen) {
                if (probe.textSeen !== "") { probe.lags.push(now - probe.beatSeen.t); probe.sampled.texts++ }
                probe.textSeen = text
            }
        }
    }
    readonly property var play: [
        [500, function() { dump("dump p5"); probe.bnT = opt("/Engine/Deck1/BeatNumber"); probe.baT = opt("/Engine/Deck1/Track/BarsElapsed"); seekSeconds(20.5) }],
        [900, function() { dump("dump p20.5"); seekSeconds(60.0) }],
        [900, function() { dump("dump p60"); seekSeconds(5.0) }],
        [900, function() { probe.bnCount = 0; probe.baCount = 0; probe.lags = []; probe.textSeen = ""; probe.sampled = {beats: 0, texts: 0}; sampler.running = true; tr("/Engine/Deck1/Play").state = true }],
        [4500, function() {
            sampler.running = false
            const ls = probe.lags.slice().sort((a, b) => a - b), med = ls.length ? ls[Math.floor(ls.length / 2)] : -1
            say("play 4.5s: PlayState=" + tr("/Engine/Deck1/PlayState").state + " beatNotifies=" + probe.bnCount + " barNotifies=" + probe.baCount
                + " beatSteps=" + probe.sampled.beats + " textSteps=" + probe.sampled.texts + " lag(ms) median=" + med + " max=" + (ls.length ? ls[ls.length - 1] : -1) + " all=" + ls.join(","))
            expect(probe.sampled.texts >= 5, "the capsule stepped " + probe.sampled.texts + " times in 4.5 s of playback (about 9 beats at 128 BPM)")
            expect(med >= 0 && med < 120, "the capsule steps on Engine's beat: median lag " + med + " ms")
            tr("/Engine/Deck1/Play").state = false
        }],
        [900, function() { say("paused: PlayState=" + tr("/Engine/Deck1/PlayState").state + " beat=" + peek("/BeatNumber")); seekSeconds(5.0) }]
    ]
    // A second tempo from 20.0 s on (an anchor, then Double Tempo): what Engine publishes on both sides of it.
    readonly property var anchors: [
        [500, function() { seekSeconds(20.0) }],
        [900, function() { dump("before-anchor"); ed.insertAnchorAtPlayPosition() }],
        [900, function() { dump("anchored"); ed.doubleTempo() }],
        [1500, function() { dump("doubled@20.0"); seekSeconds(10.0) }],
        [900, function() { dump("seg1@10"); seekSeconds(30.0) }],
        [900, function() { dump("seg2@30"); seekSeconds(5.0) }],
        [900, function() { dump("seg1@5") }]
    ]
    readonly property var finish: [
        [500, function() {
            say("failures " + failures)
            console.warn("X166 DONE|" + (failures === 0 ? "PASS" : "FAIL") + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }]
    ]
    readonly property var steps: setup.concat([[600, function() { dismissOverlays() }], [600, function() { shot("before-edit") }]]).concat(scan("A")).concat(play).concat(openEditor).concat(shiftGrid)
        .concat(scan("B")).concat([[200, function() { shot("after-shift") }]]).concat(raiseBpm).concat(scan("C")).concat(pitch).concat(anchors).concat(finish)
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
                if (done === false && now - probe.stepStarted < 240000) return
                if (done === false) throw new Error("step " + probe.step + " did not finish in 240 s")
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
