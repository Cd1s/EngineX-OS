// Sandbox only: drives Channel 1 and Channel 2 SweepFx/Value through stock
// InputAssignment PropertyTarget + NormalizedFloatTranslator::setValue(..., 1, 0),
// matching ControlSurfaceModules/SweepFxKnob.qml -> ValueOutput.qml (useSoftTakeover: false).
import QtQuick 2.12
import Planck 1.0
import InputAssignment 0.1

Item {
    id: probe
    property int phase: 0
    property int waitTicks: 0

    PropertyTarget {
        id: target1
        path: "/Engine/Mixer/Channel1/SweepFx/Value"
    }
    PropertyTarget {
        id: target2
        path: "/Engine/Mixer/Channel2/SweepFx/Value"
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
                var t1 = target1.get(0, 0.5)
                var t2 = target2.get(1, 0.5)
                if ((!root.scColorFxAvailable || !t1 || !t2) && probe.waitTicks < 24)
                    return
                console.warn("CFXPROP_QML ready ticks=" + probe.waitTicks
                    + " colorFxAvailable=" + root.scColorFxAvailable
                    + " pioneerFx=" + root.scPioneerFx
                    + " dspGuardsOff=" + root.scOff("dsp-guards")
                    + " t1=" + (t1 ? t1.value : "null")
                    + " t2=" + (t2 ? t2.value : "null"))
                // Hold 2 s after ready so the native driver finds controls and records centred state.
                probe.phase = 1
                probe.waitTicks = 0
                return
            }
            probe.waitTicks++
            if (probe.phase === 1 && probe.waitTicks >= 4) {
                var a1 = target1.get(0, 0.05)
                if (a1) a1.setValue(0.05, 1, 0)
                console.warn("CFXPROP_QML step=1 ch1=" + (a1 ? a1.value : "null")
                    + " ch2=" + (target2.get(1, 0.5) ? target2.get(1, 0.5).value : "null"))
                probe.phase = 2
                probe.waitTicks = 0
            } else if (probe.phase === 2 && probe.waitTicks >= 5) {
                var a2 = target2.get(1, 0.05)
                if (a2) a2.setValue(0.05, 1, 0)
                console.warn("CFXPROP_QML step=2 ch1=" + (target1.get(0, 0.05) ? target1.get(0, 0.05).value : "null")
                    + " ch2=" + (a2 ? a2.value : "null"))
                probe.phase = 3
                probe.waitTicks = 0
            } else if (probe.phase === 3 && probe.waitTicks >= 5) {
                var b1 = target1.get(0, 0.95)
                var b2 = target2.get(1, 0.95)
                if (b1) b1.setValue(0.95, 1, 0)
                if (b2) b2.setValue(0.95, 1, 0)
                console.warn("CFXPROP_QML step=3 ch1=" + (b1 ? b1.value : "null")
                    + " ch2=" + (b2 ? b2.value : "null"))
                probe.phase = 4
                probe.waitTicks = 0
            } else if (probe.phase === 4 && probe.waitTicks >= 5) {
                var c1 = target1.get(0, 0.5)
                var c2 = target2.get(1, 0.5)
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
