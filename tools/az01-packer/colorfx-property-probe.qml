// Sandbox only: drives Channel 1 and Channel 2 SweepFx/Value through stock
// Planck.getProperty("<sweepFxPath>Value").translator.setValue(v, 1, 0),
// matching ControlSurfaceModules/SweepFxKnob.qml -> ValueOutput.qml (HARDEXTERNAL = 1)
// -> PropertyTarget.qml (which wraps Planck.getProperty(path).translator).
import QtQuick 2.12
import Planck 1.0

Item {
    id: probe
    property int phase: 0
    property int waitTicks: 0
    property var prop1: null
    property var prop2: null

    function getTranslator(idx) {
        if (idx === 1) {
            if (!prop1) prop1 = Planck.getProperty("/Engine/Mixer/Channel1/SweepFx/Value")
            return prop1 ? prop1.translator : null
        }
        if (!prop2) prop2 = Planck.getProperty("/Engine/Mixer/Channel2/SweepFx/Value")
        return prop2 ? prop2.translator : null
    }

    Component.onCompleted: {
        console.warn("CFXPROP_QML init product=" + Config.device.name
            + " hasNHFx=" + Config.audio.fx.hasNHFx
            + " hasDJFx=" + Config.audio.fx.hasDJFx
            + " colorFxAvailable=" + root.scColorFxAvailable
            + " pioneerFx=" + root.scPioneerFx
            + " dspGuardsOff=" + root.scOff("dsp-guards"))
    }

    Timer {
        interval: 500
        repeat: true
        running: true
        onTriggered: {
            if (probe.phase === 0) {
                probe.waitTicks++
                var t1 = probe.getTranslator(1)
                var t2 = probe.getTranslator(2)
                if ((!root.scColorFxAvailable || !t1 || !t2) && probe.waitTicks < 24)
                    return
                var ty1 = Planck.getProperty("/Engine/Mixer/Channel1/SweepFx/Type")
                var ty2 = Planck.getProperty("/Engine/Mixer/Channel2/SweepFx/Type")
                console.warn("CFXPROP_QML ready ticks=" + probe.waitTicks
                    + " colorFxAvailable=" + root.scColorFxAvailable
                    + " pioneerFx=" + root.scPioneerFx
                    + " dspGuardsOff=" + root.scOff("dsp-guards")
                    + " t1=" + (t1 ? t1.value : "null")
                    + " t2=" + (t2 ? t2.value : "null")
                    + " type1=" + (ty1 && ty1.translator ? ty1.translator.string : "null")
                    + " type2=" + (ty2 && ty2.translator ? ty2.translator.string : "null"))
                // Hold 2 s after ready so the native driver finds controls and records centred state.
                probe.phase = 1
                probe.waitTicks = 0
                return
            }
            probe.waitTicks++
            if (probe.phase === 1 && probe.waitTicks >= 4) {
                var a1 = probe.getTranslator(1)
                if (a1) a1.setValue(0.05, 1, 0)
                var a2 = probe.getTranslator(2)
                console.warn("CFXPROP_QML step=1 ch1=" + (a1 ? a1.value : "null")
                    + " ch2=" + (a2 ? a2.value : "null"))
                probe.phase = 2
                probe.waitTicks = 0
            } else if (probe.phase === 2 && probe.waitTicks >= 5) {
                var a2b = probe.getTranslator(2)
                if (a2b) a2b.setValue(0.05, 1, 0)
                var a1b = probe.getTranslator(1)
                console.warn("CFXPROP_QML step=2 ch1=" + (a1b ? a1b.value : "null")
                    + " ch2=" + (a2b ? a2b.value : "null"))
                probe.phase = 3
                probe.waitTicks = 0
            } else if (probe.phase === 3 && probe.waitTicks >= 5) {
                var b1 = probe.getTranslator(1)
                var b2 = probe.getTranslator(2)
                if (b1) b1.setValue(0.95, 1, 0)
                if (b2) b2.setValue(0.95, 1, 0)
                console.warn("CFXPROP_QML step=3 ch1=" + (b1 ? b1.value : "null")
                    + " ch2=" + (b2 ? b2.value : "null"))
                probe.phase = 4
                probe.waitTicks = 0
            } else if (probe.phase === 4 && probe.waitTicks >= 5) {
                var c1 = probe.getTranslator(1)
                var c2 = probe.getTranslator(2)
                if (c1) c1.setValue(0.5, 1, 0)
                if (c2) c2.setValue(0.5, 1, 0)
                console.warn("CFXPROP_QML step=4 ch1=" + (c1 ? c1.value : "null")
                    + " ch2=" + (c2 ? c2.value : "null"))
                probe.phase = 5
                probe.waitTicks = 0
            }
        }
    }
}
