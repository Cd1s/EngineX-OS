// Sandbox only: loaded over QmlWarnings.qml by stop-time-sandbox.sh (it fills in @BOARD@, @ARM@, @DECKCONFIG@, @MOTOR@
// and @BRAKE@).
// #329: a player's Stop Time knob (stock ControlSurfaceModules StopTime, CC 7 on SC5000 / SC5000M / SC6000 / SC6000M) sets
// the ramp-down time: Engine applies it when PLAY stops a deck, and on the motorized players (SC5000M, SC6000M) the
// Motorized module also hands it to the platter motor (device.setStopTime). The probe builds the knob the way the board's
// assignment file does: the module Engine serves, with deckConfig as the stock file has it (arm 2deck) or as
// engine-ui-guard rewrites it for the 4-deck layout (arm 4deck; the sandbox script cuts those lines out of the real guard).
//   - Motor (motorized boards): the Motorized module's own ramp-down lines (cut from the module Engine serves) with a
//     stand-in motor device: does setStopTime follow the knob?
//   - Brake (the boards where a track loads in this sandbox): Deck 1 plays and, after each PLAY release, the probe measures how far
//     and how long it runs on, with every RampDownTime property Engine has written at the top and at the bottom (does
//     Engine's brake follow them on this layout?) and with the knob at the top and at the bottom (does the knob reach it?).
// It passes when every part that runs on the board follows the knob; the other lines are the evidence.
import QtQuick
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    readonly property string arm: "@ARM@"
    readonly property bool brakeRuns: "@BRAKE@" === "1"
    readonly property bool expectPlayer: "@EXPECT_PLAYER@" === "true"
    readonly property string expectName: "@EXPECT_NAME@"
    readonly property string expectPath: "@EXPECT_PATH@"
    readonly property bool motorRuns: "@MOTOR@" !== ""
    property var motor: null
    property int step: 0
    property real readyAt: 0
    property real stepStarted: 0
    property var ctx: ({})
    property int polls: 0
    property var knob: null
    function say(text) { console.warn("X329 ST|" + board + "-" + arm + "|" + text + "|") }
    function opt(path) { const p = Planck.getProperty(path); return p && p.translator ? p.translator : null }
    function tr(path) { const t = opt(path); if (!t) throw new Error("missing " + path); return t }
    function num(x) { return typeof x === "number" ? Math.round(x * 1000) / 1000 : x }
    function flag(o, k) { return o && typeof o[k] !== "undefined" ? String(o[k]) : "absent" }
    function rampPaths() {
        const out = ["/Client/Preferences/RampDownTime"]
        for (const s of ["1", "2", "3", "4", "Left", "Right"]) out.push("/Client/Preferences/RampDownTime/Deck" + s)
        return out
    }
    function rampState() {
        const have = rampPaths().filter(p => opt(p)).map(p => p.replace("/Client/Preferences/RampDownTime", "RDT") + "=" + num(opt(p).value))
        return have.length ? have.join(" ") : "no RampDownTime property"
    }
    function setAll(v) {
        for (const p of rampPaths()) { const t = opt(p); if (t) t.value = v }
        say("every RampDownTime property to " + v + "; " + rampState())
    }
    function dismiss() {
        const close = Planck.getProperty("/GUI/CloseDialogs")
        if (close && close.translator) close.translator.state = true
    }
    function evidence() {
        const device = typeof Config !== "undefined" && Config ? Config.device : null
        const audio = typeof Config !== "undefined" && Config ? Config.audio : null
        const ramp = audio ? audio.rampDownTime : null
        say("config isPlayer=" + flag(device, "isPlayer") + " hasMotorizedPlatters=" + flag(device, "hasMotorizedPlatters")
            + " rampDownTime.isPerDeck=" + flag(ramp, "isPerDeck") + " rampDownTime.isPerLayer=" + flag(ramp, "isPerLayer")
            + " rampDownTime.canStore=" + flag(ramp, "canStore"))
        say("properties at start " + rampState())
        // The arm's layout, or nothing below means anything: a failed 4-deck switch leaves isPlayer true.
        if (!device || device.isPlayer !== expectPlayer)
            throw new Error("layout: Config.device.isPlayer is " + flag(device, "isPlayer") + ", this arm needs " + expectPlayer)
        if (!brakeRuns && !motorRuns) throw new Error("neither the brake nor the motor check runs on this board")
    }
    function makeKnob() {
        const text = "import QtQuick 2.12\nimport Planck 1.0\nimport ControlSurfaceModules 0.1\nItem {\n"
            + "    QtObject {\n        id: deckConfig\n        property int midiChannel: 0\n@DECKCONFIG@\n    }\n"
            + "    readonly property string deckName: deckConfig.name\n"
            + "    StopTime { id: stopTime; cc: 7 }\n"
            + "    function path() { return stopTime.output && stopTime.output.target ? String(stopTime.output.target.path) : \"\" }\n"
            + "}\n"
        knob = Qt.createQmlObject(text, probe, "stop-time-knob.qml")
        const path = knob.path()
        ctx.knobPath = path
        say("knob deckConfig.name=\"" + knob.deckName + "\" target=" + path + " exists=" + (opt(path) ? "yes" : "no"))
        if (!path) throw new Error("the stock StopTime module gave no target path")
        if (knob.deckName !== expectName || path !== expectPath)
            throw new Error("knob: deckConfig.name \"" + knob.deckName + "\" and target " + path + ", this arm needs \""
                + expectName + "\" and " + expectPath)
        // The target must be a property Engine has and keeps what is written to it.
        const target = opt(path)
        if (!target) throw new Error("knob: Engine has no " + path)
        const before = target.value
        target.value = 0.25
        const kept = Math.abs(target.value - 0.25) < 1e-6
        target.value = before
        if (!kept) throw new Error("knob: " + path + " did not keep a written value")
        say("knob target " + path + " exists and keeps a written value")
        if (!motorRuns) { say("motor: this board's assignment has no Motorized module"); return }
        const motorText = "import QtQuick 2.12\nimport Planck 1.0\nimport airAssignments 1.0\nItem {\n"
            + "    QtObject {\n        id: deckConfig\n        property int midiChannel: 0\n@DECKCONFIG@\n"
            + "        readonly property string activeDeckPath: \"/Engine/Deck1/\"\n    }\n"
            + "    QtObject {\n        id: device\n        property var stopTimes: []\n        function setStopTime(v) { stopTimes.push(v) }\n    }\n"
            + "@MOTOR@\n"
            + "    function stopTimes() { return device.stopTimes }\n"
            + "}\n"
        motor = Qt.createQmlObject(motorText, probe, "stop-time-motor.qml")
        say("motor: the Motorized module's ramp-down binding built; setStopTime calls so far " + JSON.stringify(motor.stopTimes()))
    }
    function motorCalls() { return motor ? motor.stopTimes().map(num) : [] }
    // What the knob's ValueOutput does on a CC: the value lands on its target property, when Engine has one.
    function turnKnob(v) {
        const t = opt(ctx.knobPath)
        if (t) t.value = v
        say("knob to " + v + (t ? "" : " (its target does not exist: the turn goes nowhere)") + "; " + rampState()
            + (motor ? "; motor setStopTime calls " + JSON.stringify(motorCalls()) : ""))
    }
    function loaded() {
        const l = opt("/Engine/Deck1/Track/SongLoaded"), o = opt("/Engine/Deck1/OriginalBPM")
        if (++probe.polls % 20 === 1) say("load poll loaded=" + (l ? l.state : "absent") + " orig=" + (o ? num(o.unnormalized) : "absent"))
        return !!l && l.state && !!o && o.unnormalized > 60
    }
    // One brake: play Deck 1 for two seconds, release PLAY, sample the position every 20 ms for 2.5 s.
    property var samples: []
    property real releasedAt: 0
    Timer {
        id: sampler
        interval: 20
        repeat: true
        onTriggered: probe.samples.push([Date.now() - probe.releasedAt, tr("/Engine/Deck1/Track/TrackPosition").unnormalized])
    }
    function release() {
        probe.samples = []
        probe.releasedAt = Date.now()
        probe.samples.push([0, tr("/Engine/Deck1/Track/TrackPosition").unnormalized])
        tr("/Engine/Deck1/Play").state = false
        sampler.start()
    }
    function measure(label) {
        sampler.stop()
        const s = probe.samples, rate = tr("/Engine/Deck1/Track/SampleRate").value || 44100
        let lastMove = 0
        for (let i = 1; i < s.length; i++) if (Math.abs(s[i][1] - s[i - 1][1]) > 1) lastMove = s[i][0]
        const travel = s.length ? (s[s.length - 1][1] - s[0][1]) / rate : 0
        ctx[label] = {runOn: lastMove / 1000, travel: travel}
        say("brake " + label + ": ran on " + num(lastMove / 1000) + " s, " + num(travel) + " s of track after PLAY was released"
            + " (" + s.length + " samples over " + num(s.length ? s[s.length - 1][0] / 1000 : 0) + " s, "
            + (stopped() ? "stopped" : "still moving at the 12 s limit") + ", play state " + tr("/Engine/Deck1/PlayState").state + ")")
    }
    // The deck has stopped when its position has not moved over the last 300 ms of samples.
    function stopped() {
        const s = probe.samples
        if (s.length < 2) return false
        const last = s[s.length - 1]
        for (let i = s.length - 2; i >= 0 && last[0] - s[i][0] <= 300; i--) if (Math.abs(last[1] - s[i][1]) > 1) return false
        return last[0] >= 300
    }
    function brake(label, before) {
        return [
            [300, function() { if (before) before() }],
            [2000, function() { tr("/Engine/Deck1/Play").state = true }],
            [0, function() { release() }],
            // Sample until the deck has stopped (the whole run-on), at most 12 s.
            [300, function() { if (!stopped() && Date.now() - probe.releasedAt < 12000) return false; measure(label) }]
        ]
    }
    function moves(hi, lo) { return ctx[hi].runOn - ctx[lo].runOn >= 0.2 && ctx[hi].travel - ctx[lo].travel >= 0.05 }
    readonly property var start: [
        [6000, function() { dismiss() }],
        [500, function() { evidence(); makeKnob() }]
    ]
    // The motor: the knob at the top, then at the bottom; setStopTime must follow (127 * t * t).
    readonly property var motorPart: [
        [300, function() { turnKnob(1.0) }],
        [300, function() { ctx.motorTop = motorCalls().slice(); turnKnob(0.0) }],
        [300, function() {
            const top = ctx.motorTop, all = motorCalls()
            ctx.motorFollows = top.length > 0 && Math.abs(top[top.length - 1] - 127) < 0.5 && all.length > top.length
                && Math.abs(all[all.length - 1]) < 0.5
            say("motor: the knob reaches the platter motor's stop time: " + (ctx.motorFollows ? "yes" : "no")
                + " (setStopTime " + JSON.stringify(all) + ")")
        }]
    ]
    readonly property var loadPart: [
        [500, function() { for (let n = 1; n <= 4; n++) { const p = opt("/Engine/Deck" + n + "/Play"); if (p) p.state = false }
                            tr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url = "file:///data/music/stop-test.wav" }],
        [2000, function() { return loaded() }]
    ]
    readonly property var trials: [].concat(
        brake("as-booted", null),
        brake("all-top", function() { setAll(1.0) }),
        brake("all-bottom", function() { setAll(0.0) }),
        brake("knob-top", function() { setAll(0.5); turnKnob(1.0) }),
        brake("knob-bottom", function() { turnKnob(0.0) }))
    readonly property var finish: [
        [500, function() {
            let ok = true
            if (brakeRuns) {
                const engine = moves("all-top", "all-bottom"), reach = moves("knob-top", "knob-bottom")
                say("brake: Engine's brake follows its RampDownTime properties: " + (engine ? "yes" : "no"))
                say("brake: the knob reaches Engine's brake: " + (reach ? "yes" : "no"))
                ok = ok && reach
            }
            if (motorRuns) ok = ok && !!ctx.motorFollows
            console.warn("X329 DONE|" + (ok ? "PASS" : "FAIL") + "|" + board + "-" + arm + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }]
    ]
    readonly property var steps: start.concat(motorRuns ? motorPart : []).concat(brakeRuns ? loadPart.concat(trials) : []).concat(finish)
    Timer {
        interval: 100
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
                console.warn("X329 ERROR|" + board + "-" + arm + "|step " + probe.step + ": " + e + "|")
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
