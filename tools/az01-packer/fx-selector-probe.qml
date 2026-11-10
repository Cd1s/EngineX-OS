// Sandbox only: real selector views + real private knob component with MIDI
// config fixtures. The separate C driver replays DSP events offline.
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0
Item {
    id: probe
    Component.onCompleted: console.warn("FXCONFIG product=" + Config.device.name + " hasDJFx=" + Config.audio.fx.hasDJFx + " hasNHFx=" + Config.audio.fx.hasNHFx)
    property int phase: 0
    property int attempts: 0
    property bool failed: false
    property bool checkedOne: false
    property bool checkedTwo: false
    property var knob: null
    property var capturedOne: []
    property var capturedTwo: []
    property int rateStep: -1
    property bool rateDone: false
    readonly property var nativeRates: [1 / 16, 1 / 8, 1 / 4, 1 / 2, 3 / 4, 1, 2, 4, 8, 16, 32]
    Timer {
        interval: 100; repeat: true; running: probe.rateStep >= 0 && !probe.rateDone
        onTriggered: {
            const step = probe.rateStep++
            const path = "/Engine/Mixer/DJFxUnit1/"
            // These are the targets of JC11's original BEAT -/+ assignments.
            if (step < 10) Planck.getProperty(path + "DecrementBeats").translator.state = true
            else {
                const index = step - 10
                const parameter = root.scFxControllers[0].djFxView.variantParameters[0]
                probe.check(parameter.value === probe.nativeRates[index], "native-Rate-index-" + index)
                if (index < 10) Planck.getProperty(path + "IncrementBeats").translator.state = true
                else probe.rateDone = true
            }
        }
    }
    function capture(unit, value) {
        if (!(value > 0 && value < 0.00001)) return
        const f = new Float32Array([value])
        const bits = new Uint32Array(f.buffer)[0]
        if (unit === 1) capturedOne = capturedOne.concat([bits])
        else capturedTwo = capturedTwo.concat([bits])
    }
    Connections {
        target: Planck.getProperty("/Engine/Mixer/DJFxUnit1/Mix").translator
        function onValueChanged() { probe.capture(1, target.value) }
    }
    Connections {
        target: Planck.getProperty("/Engine/Mixer/DJFxUnit2/Mix").translator
        function onValueChanged() { probe.capture(2, target.value) }
    }
    property QtObject fxConfig: QtObject {
        property int midiChannel: 0
        property bool hasSendStyleEffects: true
        property bool hasContinuousFxKnobs: false
        property var channelNames: ["1", "2"]
        property var channel: ({})
    }
    function check(value, name) {
        console.warn("FXPROBE " + name + "|" + (value ? "PASS" : "FAIL"))
        if (!value) failed = true
    }
    function walk(o, f, seen) {
        if (!seen) seen = new Set()
        if (!o || seen.has(o)) return
        seen.add(o); f(o)
        if (o.children) for (let i = 0; i < o.children.length; ++i) walk(o.children[i], f, seen)
        if (o.item) walk(o.item, f, seen)
        if (o.contentItem) walk(o.contentItem, f, seen)
    }
    function exists(path) {
        const x = new XMLHttpRequest()
        x.open("GET", "file://" + path, false)
        x.send()
        return x.responseText.indexOf("ok") === 0
    }
    function checkWire(unit) {
        const x = new XMLHttpRequest()
        x.open("GET", "file:///data/sc5000-custom/ui/fx-selector.request", false); x.send()
        const fields = x.responseText.trim().split(/\s+/)
        const values = unit === 1 ? capturedOne : capturedTwo
        check(fields.length === 6 && values.indexOf(Number(fields[3])) >= 0 && values.indexOf(Number(fields[4])) >= 0, "unit" + unit + "-Mix-wire-bits")
    }
    function channel(u) {
        const path = "/Engine/Mixer/DJFxUnit" + u + "/"
        return {nameSuffix: String(u), djfxPath: path, selecting: false,
            pSelectPressed: Planck.getProperty(path + "SelectPressed"),
            pSelecting: Planck.getProperty(path + "Selecting"),
            pSelectionIndex: Planck.getProperty(path + "PreselectIndex"),
            pEffectIndex: Planck.getProperty(path + "EffectIndex")}
    }
    Timer {
        interval: 250; running: true; repeat: true
        onTriggered: {
            if (++probe.attempts > 160) { probe.check(false, "timeout"); stop(); return }
            const a = root.scFxControllers[0], b = root.scFxControllers[1]
            if (probe.phase === 0) {
                if (!a || !b || !a.scFxEnabled || !b.scFxEnabled) return
                probe.check(root.scFxMasterBypassed, "legacy-master-menu-hidden")
                probe.check(a.scFxNames.length === a.scFxStock.length + 5 && b.scFxNames.length === b.scFxStock.length + 5, "appended-list")
                for (let i = a.scFxStock.length; i < a.scFxNames.length; ++i) console.warn("FXPROBE name|" + a.scFxNames[i])
                probe.fxConfig.channel = {"1": probe.channel(1), "2": probe.channel(2)}
                const c = Qt.createComponent("qrc:/ControlSurfaceModules/ControlSurfaceModules/private/DJFxSelectKnob.qml")
                probe.check(c.status === Component.Ready, "native-knob-component")
                if (c.status !== Component.Ready) { console.warn("FXPROBE component-error|" + c.errorString()); stop(); return }
                probe.knob = c.createObject(probe, {pushNote: -1, touchNote: -1, turnCC: 0})
                a.scFxIndex = a.scFxStock.length - 1
                probe.phase = 1; return
            }
            if (probe.phase >= 1 && probe.phase <= 4) {
                let one = null
                probe.walk(probe.knob, function(o) { if (o.scFxUnit === 1 && typeof o.scFxAction === "function") one = o })
                probe.check(!!one && !!one.scFxCanUse(), "knob-context-ready")
                if (!one) { stop(); return }
                one.scFxAction(probe.phase === 4 ? "Minus" : "Plus")
                ++probe.phase; return
            }
            if (probe.phase === 5) {
                probe.check(a.scFxIndex === a.scFxStock.length + 1, "three-plus-one-minus")
                probe.check(b.scFxIndex < b.scFxStock.length, "unit-bus-isolation")
                let one = null
                probe.walk(probe.knob, function(o) { if (o.scFxUnit === 1 && typeof o.scFxMove === "function") one = o })
                one.scFxMove(1, 0)
                probe.phase = 50; return
            }
            if (probe.phase === 50) {
                if (a.scFxBusy) return
                probe.check(a.scFxIndex === a.scFxStock.length + 1, "continuous-zero-no-movement")
                let one = null
                probe.walk(probe.knob, function(o) { if (o.scFxUnit === 1 && typeof o.scFxMove === "function") one = o })
                one.scFxMove(1, 5 / 63)
                probe.phase = 51; return
            }
            if (probe.phase === 51) {
                if (a.scFxBusy) return
                probe.check(a.scFxIndex === a.scFxNames.length - 1, "continuous-five-step-burst-clamps")
                a.scFxAccept(a.scFxStock.length)
                probe.phase = 6; return
            }
            if (probe.phase === 6) {
                if (a.scFxBusy) return
                if (!probe.checkedOne) {
                    probe.checkWire(1)
                    probe.check(Planck.getProperty("/Engine/Mixer/DJFxUnit1/Type").translator.string === "Flanger", "unit1-carrier")
                    probe.check(a.scFxSelected === a.scFxStock.length, "unit1-px-label")
                    probe.checkedOne = true
                    probe.rateStep = 0
                }
                if (!probe.exists("/tmp/fx-native-stage1")) return
                b.scFxAccept(b.scFxStock.length + 1)
                probe.phase = 7; return
            }
            if (probe.phase === 7) {
                if (b.scFxBusy) return
                if (!probe.checkedTwo) {
                    probe.checkWire(2)
                    probe.check(Planck.getProperty("/Engine/Mixer/DJFxUnit2/Type").translator.string === "Flanger", "unit2-carrier")
                    probe.checkedTwo = true
                }
                if (!probe.exists("/tmp/fx-native-stage2")) return
                a.scFxAccept(a.scFxStock.indexOf("Flanger"))
                probe.phase = 8; return
            }
            if (probe.phase === 8) {
                if (a.scFxBusy || !probe.exists("/tmp/fx-native-done") || !probe.rateDone) return
                probe.check(a.scFxSelected === -1 && Planck.getProperty("/Engine/Mixer/DJFxUnit1/Type").translator.string === "Flanger", "stock-flanger-selectable")
                console.warn("FXPROBE done|" + (probe.failed ? "FAIL" : "PASS"))
                if (probe.knob) probe.knob.destroy()
                stop()
                Planck.getProperty("/Client/Quit").translator.state = true
            }
        }
    }
}
