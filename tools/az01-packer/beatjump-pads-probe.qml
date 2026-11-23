// Sandbox only: loaded over Skins/Embedded/main.qml by test_beatjump_pads_sandbox.sh (through
// test_controller_qml_compile.sh with CQC_PROBE), which fills in @BOARD@. It runs the pack's real controller-side
// modules (GlobalAssignmentConfig, PadModeSelect, ActionPads and its RGB pad LEDs) in the board's own stock Engine with
// that board's Config, the way the board's controller assignment file places them, and drives them through their
// ValueNoteAssignment.onNote, as a MIDI note does. Engine's GUI engine cannot run the stock ActionOutput (its
// QObjProperty type belongs to the controller engine), so every pad's stock output object is replaced by a recorder:
// what reaches it is what the stock ActionOutput would get. Beat Jump writes go to the real
// /Engine/Deck<N>/Track/BeatJump/JumpBack and JumpForward and are counted on their translators.
import QtQuick
import Planck
import ControlSurfaceModules 0.1

Rectangle {
    id: probe
    width: 1280
    height: 800
    color: "#282828"
    readonly property var planckQuickView: null
    readonly property string board: "@BOARD@"
    readonly property bool player: ["JP07", "JP08", "JP13", "JP14"].indexOf(Config.device.name) >= 0
    readonly property bool fourDecks: board.endsWith("-fourdeck")
    property var sent: ({})
    property var calls: []
    property var counts: ({})
    property int step: 0
    property bool failed: false
    property bool done: false
    property var steps: []

    function say(line) { console.warn((line.length > 110 ? line.slice(0, 110) + "..." : line)) }
    function check(ok, what) {
        say("BJT " + (ok ? "PASS" : "FAIL") + "|" + board + "|" + what + "|")
        if (!ok) failed = true
    }
    function count(path) { return counts[path] || 0 }
    function watch(path) {
        const p = Planck.getProperty(path)
        const t = p && p.translator
        if (!t) { check(false, "no translator for " + path); return }
        counts[path] = 0
        t.stateChanged.connect(function() { if (t.state) probe.counts[path] = (probe.counts[path] || 0) + 1 })
    }
    function find(name, from) {
        const seen = new Set()
        function walk(o) {
            if (!o || seen.has(o)) return null
            seen.add(o)
            if (o.objectName === name) return o
            const kids = []
            if (o.children) for (let i = 0; i < o.children.length; i++) kids.push(o.children[i])
            if (o.data) for (let i = 0; i < o.data.length; i++) kids.push(o.data[i])
            for (const c of kids) { const f = walk(c); if (f) return f }
            return null
        }
        return walk(from)
    }
    function pad(n) { return find("Action Pad " + n + " Deck", pads) }
    function modeButton(i) { return find("Pad Mode Select " + i, modeSelect) }
    function press(n, v) { pad(n).onNote(0, v === undefined ? 1.0 : v, 0) }
    function release(n) { pad(n).onNote(0, 0.0, 0) }
    function mode(i, shift) { device.shift = !!shift; modeButton(i).onNote(0, 1.0, 0); modeButton(i).onNote(0, 0.0, 0); device.shift = false }
    function deckBit(n) { return (globalConfig.scBeatJumpDecks & (1 << n)) !== 0 }
    function recorder(n) {
        return Qt.createQmlObject('import QtQuick; QtObject { function setValue(channel, value, enabled, timestamp) { probe.calls.push({pad: ' + n + ', value: value, enabled: enabled, shift: device.shift}) } }', probe)
    }
    function lastCalls(k) { return calls.slice(calls.length - k) }
    function colour(note) { return String(sent[note] || "") }

    QtObject {
        id: device
        property bool shift: false
        property bool leftParameter: false
        property var oleds: []
        function sendColor(channel, idx, c) { probe.sent[idx] = String(c).toLowerCase() }
        function sendSimpleColor(channel, idx, v) { probe.sent[idx] = "s" + v }
    }
    Utility { id: util }
    GlobalAssignmentConfig { id: globalConfig; midiChannel: 0 }
    // The controller engine's DeckAssignmentConfig needs that engine's property type; this stands in for the values the
    // modules read. activeDeckPath is what a Layer or 4-deck switch changes.
    QtObject {
        id: deckConfig
        property string name: ""
        property int midiChannel: 0
        property string activeDeckPath: "/Engine/Deck1/"
        readonly property string activeDeckTrackPath: activeDeckPath + "Track/"
        property bool padsEnabled: true
        readonly property bool performanceModesEnabled: true
        readonly property bool inDJMode: true
        readonly property bool inComputerMode: false
        readonly property bool playerGridEditActive: false
        readonly property bool tempoSyncedFlashState: false
        readonly property var pTempoSyncedFlashState: null
        property string padsMode: "CUES"
        readonly property bool padsModeIsStems: false
        readonly property bool padsModeIsSlicer: false
        readonly property bool padsModeIsSlicerContinuous: false
        readonly property bool padsModeIsAutoLoop: false
        readonly property bool padsModeIsLoop: false
        readonly property bool padsModeIsSampler: false
        readonly property var pPadIndexOffset: ({ translator: { unnormalized: 0, maximum: 0 } })
        readonly property int padIndexOffset: 0
        readonly property var pPadsView: null
    }
    // As the four players' assignment files place them (HOT CUE 27, LOOP 30, ROLL 28, SLICER 29; pads from note 32).
    PadModeSelect {
        id: modeSelect
        ledType: LedType.RGB
        buttonsModel: ListModel { ListElement { note: 27 } ListElement { note: 30 } ListElement { note: 28 } ListElement { note: 29 } }
    }
    ActionPads { id: pads; firstPadNote: 32; ledType: LedType.RGB }

    readonly property string jumpBack1: "/Engine/Deck1/Track/BeatJump/JumpBack"
    readonly property string jumpForward1: "/Engine/Deck1/Track/BeatJump/JumpForward"
    readonly property string jumpBack2: "/Engine/Deck2/Track/BeatJump/JumpBack"
    readonly property string jumpBack3: "/Engine/Deck3/Track/BeatJump/JumpBack"
    readonly property string jumpForward3: "/Engine/Deck3/Track/BeatJump/JumpForward"
    readonly property string shiftLoop1: "/Engine/Deck1/PadModeButton1ShiftPressed"
    readonly property string loop1: "/Engine/Deck1/PadModeButton1Pressed"
    readonly property string hotCue1: "/Engine/Deck1/PadModeButton0Pressed"
    function jumps() { return count(jumpBack1) + count(jumpForward1) + count(jumpBack2) + count(jumpBack3) + count(jumpForward3) }
    property int jumpsBefore: 0
    property int callsBefore: 0

    Component.onCompleted: {
        steps = [
            function() {
                for (const p of [jumpBack1, jumpForward1, jumpBack2, shiftLoop1, loop1, hotCue1].concat(fourDecks ? [jumpBack3, jumpForward3] : [])) watch(p)
                if (fourDecks)
                    check(Config.audio.numberOfDecks === 4 && !Config.device.isPlayer, "4-deck layout: " + Config.audio.numberOfDecks + " decks, Config.device.isPlayer " + Config.device.isPlayer)
                check(Config.device.hardwarePadsCount >= 1 && !!pad(1) && !!modeButton(1), "real modules instantiated: " + Config.device.hardwarePadsCount + " pads")
                for (let n = 1; n <= Config.device.hardwarePadsCount; n++) {
                    const o = pad(n).output
                    check(!!o && !!o.scStock && typeof o.scStock.setValue === "function", "pad " + n + " keeps the stock ActionOutput")
                    o.scStock = recorder(n)
                }
                check(globalConfig.scBeatJumpPads === player, "setting " + globalConfig.scBeatJumpPads + " by default (player " + player + ")")
            },
            function() {
                // Stock pads: press and release reach the stock output, nothing jumps.
                jumpsBefore = jumps(); callsBefore = calls.length
                press(1); release(1); device.shift = true; press(1); release(1); device.shift = false
                const c = lastCalls(4)
                check(calls.length - callsBefore === 4 && c[0].value > 0 && c[1].value === 0 && c[2].shift && c[3].value === 0 && jumps() === jumpsBefore,
                      "stock pads: press, release, Shift + pad (delete) reach the stock output")
                mode(1, true) // Shift + LOOP
            },
            function() {
                if (!player) {
                    check(count(shiftLoop1) === 1 && globalConfig.scBeatJumpDecks === 0, "Shift + LOOP stays stock (PadModeButton1ShiftPressed " + count(shiftLoop1) + ")")
                    callsBefore = calls.length; jumpsBefore = jumps()
                    for (let n = 1; n <= Config.device.hardwarePadsCount; n++) { press(n); release(n) }
                    check(calls.length - callsBefore === 2 * Config.device.hardwarePadsCount && jumps() === jumpsBefore, "pads stay stock after Shift + LOOP")
                    done = true
                    return
                }
                check(count(shiftLoop1) === 0 && deckBit(1) && modeSelect.scBeatJumpOn && pads.scBeatJumpOn, "Shift + LOOP enters the Beat Jump pad mode on Deck1, stock Shift + LOOP not triggered")
                jumpsBefore = jumps(); callsBefore = calls.length
                press(1)
            },
            function() {
                check(count(jumpBack1) === 1 && count(jumpForward1) === 0 && calls.length === callsBefore, "pad 1 writes JumpBack on Deck1")
                release(1); press(2)
            },
            function() {
                check(count(jumpForward1) === 1 && count(jumpBack1) === 1 && calls.length === callsBefore, "pad 2 writes JumpForward, its release nothing")
                release(2)
                jumpsBefore = jumps()
                for (let n = 3; n <= 8; n++) { press(n); release(n) }
                device.shift = true; press(1); release(1); press(2); release(2); device.shift = false
            },
            function() {
                check(jumps() === jumpsBefore && calls.length === callsBefore, "pads 3-8 and Shift + pad write nothing")
                // LEDs: pad 1 back, pad 2 forward, pads 3-8 unlit; LOOP lit.
                check(colour(32) === String(globalConfig.systemBlueLedColor).toLowerCase(), "pad 1 LED " + colour(32))
                check(colour(33) === String(globalConfig.padsModeSelectedAltColor).toLowerCase(), "pad 2 LED " + colour(33))
                let dark = true
                for (let n = 34; n <= 39; n++) dark = dark && colour(n) === "#000000"
                check(dark, "pads 3-8 unlit")
                check(colour(30) === String(globalConfig.padsModeSelectedAltColor).toLowerCase() && modeSelect.scBeatJumpOn, "LOOP button lit " + colour(30))
                deckConfig.padsEnabled = false
            },
            function() {
                let dark = true
                for (let n = 32; n <= 39; n++) dark = dark && colour(n) === "#000000"
                check(dark, "padsEnabled false: every pad unlit")
                jumpsBefore = jumps(); press(1); release(1)
                check(jumps() === jumpsBefore, "padsEnabled false: pad 1 does not jump")
                deckConfig.padsEnabled = true
                // Layer B (Deck2): its own stock pads; Deck1 keeps the mode.
                deckConfig.activeDeckPath = "/Engine/Deck2/"
            },
            function() {
                callsBefore = calls.length; jumpsBefore = jumps()
                press(1); release(1)
                check(!pads.scBeatJumpOn && calls.length - callsBefore === 2 && jumps() === jumpsBefore && deckBit(1), "Layer B: stock pads, Deck1 keeps the mode")
                deckConfig.activeDeckPath = "/Engine/Deck1/"
            },
            function() {
                callsBefore = calls.length
                press(1); release(1)
                check(pads.scBeatJumpOn && count(jumpBack1) === 2 && calls.length === callsBefore, "back on Layer A: still Beat Jump (JumpBack " + count(jumpBack1) + ")")
                // 4 Decks / Paired Player: this deck side's other deck is Deck3.
                if (fourDecks) deckConfig.activeDeckPath = "/Engine/Deck3/"
            },
            function() {
                if (!fourDecks) return
                check(!pads.scBeatJumpOn, "Deck3 starts stock")
                mode(1, true)
            },
            function() {
                if (!fourDecks) return
                press(2); release(2)
                check(deckBit(1) && deckBit(3) && count(jumpForward3) === 1 && count(jumpForward1) === 1, "Deck3 in the mode too, pad 2 jumps Deck3 forward")
                deckConfig.activeDeckPath = "/Engine/Deck1/"
            },
            function() {
                if (!fourDecks) return
                press(1); release(1)
                check(pads.scBeatJumpOn && count(jumpBack1) === 3 && count(jumpBack3) === 0, "back on Deck1: still its own mode, jumps Deck1")
            },
            function() {
                // A pad held when the mode starts is released through the stock output.
                mode(0, false) // HOT CUE: leaves the mode on Deck1, then the stock HOT CUE action
            },
            function() {
                check(!deckBit(1) && deckBit(3) === fourDecks && count(hotCue1) === 1, "unshifted HOT CUE leaves the mode on Deck1 only and runs its stock action")
                callsBefore = calls.length
                press(4)
                mode(1, true)
            },
            function() {
                release(4)
                const c = lastCalls(2)
                check(deckBit(1) && calls.length - callsBefore === 2 && c[0].pad === 4 && c[0].value > 0 && c[1].pad === 4 && c[1].value === 0, "a pad held into the mode is released through the stock output")
                mode(1, false) // unshifted LOOP: leaves, then the stock LOOP action
            },
            function() {
                check(!deckBit(1) && count(loop1) === 1 && !modeSelect.scBeatJumpOn, "unshifted LOOP leaves the mode and runs its stock action")
                callsBefore = calls.length; jumpsBefore = jumps()
                for (let n = 1; n <= 8; n++) { press(n); release(n) }
                check(calls.length - callsBefore === 16 && jumps() === jumpsBefore, "leaving the mode gives every pad its stock output back")
                mode(1, true)
                check(deckBit(1), "Shift + LOOP enters again")
                // The setting from the main UI: Off.
                const r = Planck.createProperty("EngineXBeatJumpPads", 0.0, 0)
                Planck.getProperty("/GUI/Scripted/EngineXBeatJumpPads").translator.value = 0.5
            },
            function() {},
            function() {
                callsBefore = calls.length; jumpsBefore = jumps()
                press(1); release(1)
                check(!globalConfig.scBeatJumpPads && globalConfig.scBeatJumpDecks === 0 && !pads.scBeatJumpOn && calls.length - callsBefore === 2 && jumps() === jumpsBefore,
                      "setting Off (0.5) reaches the controller, clears the mode, pads stock")
                const before = count(shiftLoop1)
                mode(1, true)
                check(count(shiftLoop1) === before + 1 && !deckBit(1), "setting Off: Shift + LOOP is stock")
                Planck.getProperty("/GUI/Scripted/EngineXBeatJumpPads").translator.value = 1.0
            },
            function() {},
            function() {
                mode(1, true)
                check(globalConfig.scBeatJumpPads && deckBit(1), "setting On (1.0): Shift + LOOP enters again")
            },
        ]
    }
    Timer {
        interval: 400; running: true; repeat: true
        onTriggered: {
            if (!probe.done && probe.step < probe.steps.length) {
                try { probe.steps[probe.step]() } catch (e) { probe.check(false, "step " + probe.step + " threw " + e) }
                probe.step++
                return
            }
            probe.say("BJT DONE|" + (probe.failed ? "FAIL" : "PASS") + "|" + probe.board + "|")
            running = false
            Planck.getProperty("/Client/Quit").translator.state = true
        }
    }
}
