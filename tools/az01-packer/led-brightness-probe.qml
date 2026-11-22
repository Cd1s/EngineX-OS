// Sandbox only (#325): the real settings picker action and the real controller-side QML, in stock Engine.
// Engine's controller QML engine needs a MIDI device, which the sandbox lacks, so the patched
// GlobalAssignmentConfig and ColorOutputAssignment are instantiated here with a stub `device`: its
// sendColor() asks led_brightness_driver.c (preloaded behind the real UI hook) to play Engine's
// sequencer write. The picker call is root.scCall("ledBrightness=<n>"), exactly what
// Settings -> Device -> Pad LED Brightness does. The driver prints the verdict lines.
import QtQuick 2.12
import Planck 1.0

Item {
    id: probe
    // Replaced by test_led_brightness_sandbox.sh
    property var picks: LEDB_PICKS
    property int maxIdleMs: LEDB_MAX_IDLE_MS
    property int maxPlayMs: LEDB_MAX_PLAY_MS
    property var globalConfig: null
    property var coa: null
    property int stage: 0          // 0 wait UI, 1 settle, 2 pick, 3 wait hit, 4 blink pick, 5 wait blink hit, 6 done
    property int pickIndex: 0
    property real stageAt: 0
    property string label: ""
    property int sends: 0
    property string firstColor: ""
    property string lastCall: ""

    function get(q, cb) {
        const x = new XMLHttpRequest()
        x.open("GET", "http://127.0.0.1:8099/" + q)
        x.onreadystatechange = function() { if (x.readyState === 4 && cb) cb(x.responseText) }
        x.send()
    }
    QtObject {
        id: device
        property bool shift: false
        property bool leftParameter: false
        function sendColor(ch, idx, c) {
            probe.sends++
            if (!probe.firstColor) probe.firstColor = "" + c
            probe.get("led?ch=" + ch + "&idx=" + idx + "&r=" + Math.round(c.r * 127) + "&g=" + Math.round(c.g * 127) + "&b=" + Math.round(c.b * 127))
        }
    }
    function make(url, props) {
        const c = Qt.createComponent(url)
        if (c.status !== Component.Ready) { console.warn("LEDB_QML component " + url + ": " + c.errorString()); return null }
        return c.createObject(probe, props)
    }
    function pick(n, tag) {
        label = tag + n
        firstColor = ""
        get("mark?l=" + label + "&pct=" + n)
        // the real picker action; its reply updates root.scLedBrightness (the on-screen pads) at once
        root.scCall("ledBrightness=" + n)
        stageAt = Date.now()
        console.warn("LEDB_QML pick " + label + " controller scPadBrightness=" + (globalConfig ? globalConfig.scPadBrightness : "none"))
    }
    Timer {
        interval: 100; repeat: true; running: true
        onTriggered: {
            const now = Date.now()
            if (probe.stage === 0) {
                if (root.scUiLoaded) { probe.stage = 1; probe.stageAt = now }
            } else if (probe.stage === 1 && now - probe.stageAt > 3000) {
                probe.globalConfig = probe.make("qrc:/ControlSurfaceModules/ControlSurfaceModules/GlobalAssignmentConfig.qml", {midiChannel: 0})
                probe.coa = probe.make("qrc:/airAssignments/OutputAssignment/ColorOutputAssignment.qml", {channel: 0, idx: 7, enabled: true})
                console.warn("LEDB_QML controller side created: globalConfig=" + !!probe.globalConfig + " pad=" + !!probe.coa + " root.scLedBrightness=" + root.scLedBrightness)
                probe.stage = 2
                probe.stageAt = now - 500
            } else if (probe.stage === 2 && now - probe.stageAt > 2000 + (probe.pickIndex * 137) % 500) { // a different phase against the controller's 500 ms timer each time
                probe.pick(probe.picks[probe.pickIndex], "idle")
                probe.stage = 3
            } else if (probe.stage === 3) {
                const lab = probe.label
                probe.get("hit?l=" + lab, function(t) {
                    if (probe.stage !== 3 || probe.label !== lab) return
                    if (t === "1" || Date.now() - probe.stageAt > 13000) {
                        probe.stage = 4
                        probe.get("result?l=" + lab + "&max=" + probe.maxIdleMs)
                        console.warn("LEDB_QML " + lab + ": colour handed to device.sendColor " + probe.firstColor + " (QML sends the stock colour), sends=" + probe.sends)
                        probe.stageAt = Date.now()
                    }
                })
            } else if (probe.stage === 4 && now - probe.stageAt > 500) {
                probe.pickIndex++
                if (probe.pickIndex < probe.picks.length) { probe.stage = 2; probe.stageAt = now - 1500 }
                else {
                    probe.get("blink?on=1&idx=3&ms=100")
                    probe.stage = 5; probe.stageAt = now
                }
            } else if (probe.stage === 5 && now - probe.stageAt > 1500) {
                // a deck with LED activity: the driver blinks one LED every 100 ms, as playing does
                probe.pick(probe.picks[probe.picks.length - 1] === 60 ? 40 : 60, "play")
                probe.stage = 6
            } else if (probe.stage === 6) {
                const lab = probe.label
                probe.get("hit?l=" + lab, function(t) {
                    if (probe.stage !== 6) return
                    if (t === "1" || Date.now() - probe.stageAt > 8000) {
                        probe.stage = 7
                        probe.get("result?l=" + lab + "&max=" + probe.maxPlayMs)
                        probe.get("blink?on=0")
                        probe.get("done")
                    }
                })
            }
        }
    }
}
