import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 1024; height: 640; color: "#0f1722"
    property var state: ({})
    property string message: "Connect a USB MIDI controller (e.g. Pioneer CDJ-400) and enable the mapper."
    property int lastAckSeq: 0
    property var propCache: ({})

    function tr(path) {
        if (typeof Planck === "undefined" || !Planck) return null
        if (!root.propCache) root.propCache = ({})
        var p = root.propCache[path]
        if (!p) {
            p = Planck.getProperty(path)
            if (p) root.propCache[path] = p
        }
        return p ? p.translator : null
    }

    function request(body, cb) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17838/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) {
                root.message = xhr.responseText || "MIDI Mapper helper unavailable. Run start.sh on the player."
                return
            }
            var val = JSON.parse(xhr.responseText)
            if (cb) {
                cb(val)
                return
            }
            root.state = val
            if (val.error) {
                root.message = "SAFETY AUTO-DISABLED: " + val.error
            } else if (val.running) {
                root.message = "LIVE · " + (val.connectedDevice || "MIDI Controller") + " → Deck " + val.targetDeck + " (" + (val.vinylMode ? "VINYL SCRATCH" : "CDJ PITCH BEND") + ")"
            } else if (val.enabled) {
                root.message = "Waiting for USB MIDI controller (" + (val.presetName || "CDJ-400") + ")..."
            } else {
                root.message = "Mapper disabled · Hardware layers and active deck untouched."
            }
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }

    function readDeckState(d) {
        var base = "/Engine/Deck" + d + "/"
        var tLoaded = root.tr(base + "Track/SongLoaded")
        var tPlay = root.tr(base + "PlayState")
        var tPos = root.tr(base + "Track/TrackPosition")
        var tLen = root.tr(base + "Track/TrackLength")
        var tSr = root.tr(base + "Track/SampleRate")
        var tSpd = root.tr(base + "Speed")
        var tRng = root.tr(base + "SpeedRange")
        var tKey = root.tr(base + "Track/KeyLock")
        var tSync = root.tr(base + "Sync")
        var tLoop = root.tr(base + "Track/LoopEnableState")
        var tBrw = root.tr("/GUI/ShowBrowseView")
        var lenSamples = tLen ? (tLen.unnormalized || 0) : 0
        var posSamples = tPos ? (tPos.unnormalized || 0) : 0
        var sr = tSr && tSr.value > 0 ? tSr.value : 44100
        return {
            songLoaded: !!(tLoaded && tLoaded.state),
            playing: !!(tPlay && tPlay.state),
            posNorm: lenSamples > 0 ? (posSamples / lenSamples) : 0.0,
            durationSec: lenSamples > 0 ? (lenSamples / sr) : 0.0,
            speed: tSpd ? tSpd.value : 0.5,
            speedRangeIndex: tRng ? tRng.index : 0,
            speedRangeCount: (tRng && tRng.entries) ? tRng.entries.length : 4,
            keylock: !!(tKey && tKey.state),
            sync: !!(tSync && tSync.state),
            loopEnabled: !!(tLoop && tLoop.state),
            browseOpen: !!(tBrw && tBrw.state)
        }
    }

    function applyBridgeOps(reply) {
        if (!reply || !reply.seq) return
        var d = reply.targetDeck || 3
        var base = "/Engine/Deck" + d + "/"
        var vals = reply.values || ({})
        if (vals.vinylMode !== undefined) {
            var tVin = root.tr(base + "ExternalScratchWheelScratchEnable")
            if (tVin) tVin.state = !!vals.vinylMode
        }
        if (vals.jogTouch !== undefined) {
            var tTch = root.tr(base + "ExternalScratchWheelTouch")
            if (tTch) tTch.value = vals.jogTouch ? 1.0 : 0.0
        }
        if (vals.jogMoveTicks !== undefined && vals.jogMoveTicks !== 0) {
            var tMv = root.tr(base + "ExternalScratchWheelMove")
            if (tMv) {
                if (vals.jogMoveTicks > 0) tMv.incValue(vals.jogMoveTicks, 0.0, 0.0, false, true, 0.0)
                else tMv.decValue(-vals.jogMoveTicks, 0.0, 0.0, false, true, 0.0)
            }
        } else if (vals.setPosNorm !== undefined) {
            var tSetPos = root.tr(base + "Track/SetSongPosition")
            if (tSetPos) tSetPos.value = vals.setPosNorm
        }
        if (vals.nudgeFwd !== undefined) {
            var tNF = root.tr(base + "Track/NudgeForward")
            if (tNF) tNF.value = vals.nudgeFwd
        }
        if (vals.nudgeBwd !== undefined) {
            var tNB = root.tr(base + "Track/NudgeBackward")
            if (tNB) tNB.value = vals.nudgeBwd
        }
        if (vals.speed !== undefined) {
            var tSpd = root.tr(base + "Speed")
            if (tSpd) tSpd.setValue(vals.speed, 1, 0)
        }
        if (vals.speedOffsetUp !== undefined) {
            var tUp = root.tr(base + "SpeedOffsetUp")
            if (tUp) tUp.state = !!vals.speedOffsetUp
        }
        if (vals.speedOffsetDown !== undefined) {
            var tDn = root.tr(base + "SpeedOffsetDown")
            if (tDn) tDn.state = !!vals.speedOffsetDown
        }
        if (vals.speedNeutral !== undefined) {
            var tNe = root.tr(base + "SpeedNeutral")
            if (tNe) tNe.state = !!vals.speedNeutral
        }
        if (vals.play !== undefined) {
            var tPl = root.tr(base + "Play")
            if (tPl) tPl.state = !!vals.play
        }
        if (vals.cueHold !== undefined) {
            var tCue = root.tr(base + "Track/CueAction")
            if (tCue) tCue.value = vals.cueHold ? 1.0 : 0.0
        }
        if (vals.vinylMode !== undefined) {
            var tVin = root.tr(base + "ExternalScratchWheelScratchEnable")
            if (tVin) tVin.state = !!vals.vinylMode
        }
        var acts = reply.actions || []
        for (var i = 0; i < acts.length; i++) {
            var a = acts[i].action
            if (a === "toggle_play") {
                var tp = root.tr(base + "Play")
                var tps = root.tr(base + "PlayState")
                if (tp) tp.state = !(tps ? tps.state : tp.state)
            } else if (a === "track_next") {
                var tn = root.tr(base + "Track/NextTrack")
                if (tn) tn.state = true
            } else if (a === "track_prev") {
                var tpr = root.tr(base + "Track/PreviousTrack")
                if (tpr) tpr.state = true
            } else if (a === "seek_forward") {
                var tsf = root.tr(base + "Track/SeekForward")
                if (tsf) tsf.state = true
            } else if (a === "seek_backward") {
                var tsb = root.tr(base + "Track/SeekBackward")
                if (tsb) tsb.state = true
            } else if (a === "loop_in") {
                var tli = root.tr(base + "Track/LoopIn")
                if (tli) tli.state = true
            } else if (a === "loop_out") {
                var tlo = root.tr(base + "Track/LoopOut")
                if (tlo) tlo.state = true
            } else if (a === "reloop_exit") {
                var trl = root.tr(base + "Track/Reloop")
                if (trl) trl.state = true
            } else if (a === "loop_halve") {
                var tlh = root.tr(base + "Track/LoopHalveDouble")
                if (tlh) tlh.decValue(0.1, 0.0, 1.0, false, false)
            } else if (a === "loop_double") {
                var tld = root.tr(base + "Track/LoopHalveDouble")
                if (tld) tld.incValue(0.1, 0.0, 1.0, false, false)
            } else if (a === "cycle_tempo_range") {
                var trg = root.tr(base + "SpeedRange")
                if (trg) {
                    var cnt = trg.entries ? trg.entries.length : 4
                    trg.index = (trg.index + 1) % cnt
                }
            } else if (a === "toggle_keylock") {
                var tkl = root.tr(base + "Track/KeyLock")
                if (tkl) tkl.state = !tkl.state
            } else if (a === "toggle_sync") {
                var tsy = root.tr(base + "Sync")
                var tus = root.tr(base + "UnSync")
                if (tsy && !tsy.state) tsy.state = true
                else if (tus) tus.state = true
            } else if (a === "load_selected_track") {
                var tldk = root.tr("/Client/Librarian/LoadToDeck" + d)
                if (tldk) tldk.state = true
            } else if (a === "toggle_browse_view") {
                var tbv = root.tr("/GUI/ShowBrowseView")
                if (tbv) tbv.state = !tbv.state
            } else if (a === "browse_scroll") {
                var steps = acts[i].steps || 0
                var dirPath = steps > 0 ? "/Qml/ControlFocus/Increment" : "/Qml/ControlFocus/Decrement"
                var tbr = root.tr(dirPath)
                for (var s = 0; s < Math.min(8, Math.abs(steps)); s++) {
                    if (tbr) tbr.state = true
                }
            }
        }
        root.lastAckSeq = reply.seq
    }

    Component.onCompleted: request(null)
    Timer { interval: 400; running: root.visible; repeat: true; onTriggered: root.request(null) }
    Timer {
        interval: 16
        running: !!(root.state && root.state.enabled && typeof Planck !== "undefined" && !Math.scMidiBridgeActive)
        repeat: true
        onTriggered: {
            var d = (root.state && root.state.targetDeck) ? root.state.targetDeck : 3
            var payload = { action: "poll", deckState: root.readDeckState(d) }
            if (root.lastAckSeq > 0) {
                payload.ackSeq = root.lastAckSeq
                root.lastAckSeq = 0
            }
            root.request(payload, function(reply) { root.applyBridgeOps(reply) })
        }
    }

    ColumnLayout {
        anchors.fill: parent; anchors.margins: 16; spacing: 10

        RowLayout {
            Layout.fillWidth: true; spacing: 12
            Label { text: "GENERIC MIDI CONTROLLER MAPPER"; color: "#ffffff"; font.pixelSize: 22; font.bold: true }
            Rectangle {
                radius: 4; color: (root.state && root.state.running) ? "#114d36" : (root.state && root.state.error ? "#5e1922" : "#223042")
                Layout.preferredWidth: 160; Layout.preferredHeight: 28
                Label {
                    anchors.centerIn: parent
                    text: (root.state && root.state.running) ? "CONNECTED · 60 Hz" : (root.state && root.state.error ? "ERROR / SAFE-STOP" : "STANDBY")
                    color: (root.state && root.state.running) ? "#54f0b1" : (root.state && root.state.error ? "#ff8b94" : "#b8cae0")
                    font.pixelSize: 12; font.bold: true
                }
            }
            Item { Layout.fillWidth: true }
            Button {
                text: (root.state && root.state.enabled) ? "DISABLE MAPPER" : "ENABLE MAPPER"
                highlighted: !!(root.state && root.state.enabled)
                onClicked: root.request({ action: (root.state && root.state.enabled) ? "disable" : "enable" })
            }
            Button {
                text: "Refresh USB MIDI"
                onClicked: root.request({ action: "refresh_devices" })
            }
        }

        Rectangle {
            Layout.fillWidth: true; Layout.preferredHeight: 38; radius: 5
            color: (root.state && root.state.error) ? "#4a151d" : "#172434"
            Label {
                anchors.fill: parent; anchors.margins: 8
                verticalAlignment: Text.AlignVCenter
                text: root.message
                color: (root.state && root.state.error) ? "#ff9da5" : ((root.state && root.state.running) ? "#64f2b8" : "#dce7f5")
                font.pixelSize: 14; elide: Text.ElideRight
            }
        }

        RowLayout {
            Layout.fillWidth: true; spacing: 10

            Rectangle {
                Layout.fillWidth: true; Layout.preferredHeight: 150; radius: 6; color: "#15202e"
                ColumnLayout {
                    anchors.fill: parent; anchors.margins: 12; spacing: 8
                    Label { text: "1. TARGET DECK (INTERNAL 4-DECK ROUTING)"; color: "#93abc6"; font.pixelSize: 12; font.bold: true }
                    RowLayout {
                        spacing: 8
                        Repeater {
                            model: [3, 4, 1, 2]
                            Button {
                                text: "Deck " + modelData + (modelData >= 3 ? " (Sub)" : " (Main)")
                                highlighted: ((root.state && root.state.targetDeck) ? root.state.targetDeck : 3) === modelData
                                onClicked: root.request({ action: "configure", targetDeck: modelData })
                            }
                        }
                    }
                    Label {
                        text: "Preset: " + ((root.state && root.state.presetName) ? root.state.presetName : "Pioneer CDJ-400") +
                              "  |  MIDI Port: " + ((root.state && root.state.connectedDevice) ? root.state.connectedDevice : "Auto-detect (ALSA Seq)")
                        color: "#d4e2f4"; font.pixelSize: 13
                    }
                    Label {
                        text: "Drives internal Deck 3/4 directly via StateMap without switching the SC5000 hardware layer or stealing the active deck."
                        color: "#7f96b0"; font.pixelSize: 12; wrapMode: Text.Wrap; Layout.fillWidth: true
                    }
                }
            }

            Rectangle {
                Layout.preferredWidth: 380; Layout.preferredHeight: 150; radius: 6; color: "#15202e"
                ColumnLayout {
                    anchors.fill: parent; anchors.margins: 12; spacing: 6
                    Label { text: "2. TEMPO SOFT TAKEOVER & ▲▼ INDICATORS"; color: "#93abc6"; font.pixelSize: 12; font.bold: true }
                    RowLayout {
                        spacing: 12
                        Rectangle {
                            width: 72; height: 42; radius: 5
                            color: (root.state && root.state.softTakeover && root.state.softTakeover.speedOffsetUp) ? "#1f5c3e" : "#1c2a3b"
                            Label {
                                anchors.centerIn: parent; text: "▲ UP"
                                color: (root.state && root.state.softTakeover && root.state.softTakeover.speedOffsetUp) ? "#58f5b6" : "#5e738b"
                                font.pixelSize: 16; font.bold: true
                            }
                        }
                        Rectangle {
                            width: 110; height: 42; radius: 5
                            color: (root.state && root.state.softTakeover && root.state.softTakeover.locked) ? "#164859" : "#1c2a3b"
                            Label {
                                anchors.centerIn: parent
                                text: (root.state && root.state.softTakeover && root.state.softTakeover.speedNeutral) ? "● NEUTRAL" : ((root.state && root.state.softTakeover && root.state.softTakeover.locked) ? "● LOCKED" : "PICKUP WAIT")
                                color: (root.state && root.state.softTakeover && root.state.softTakeover.locked) ? "#6be3ff" : "#f5c56b"
                                font.pixelSize: 13; font.bold: true
                            }
                        }
                        Rectangle {
                            width: 72; height: 42; radius: 5
                            color: (root.state && root.state.softTakeover && root.state.softTakeover.speedOffsetDown) ? "#5c3b1a" : "#1c2a3b"
                            Label {
                                anchors.centerIn: parent; text: "▼ DN"
                                color: (root.state && root.state.softTakeover && root.state.softTakeover.speedOffsetDown) ? "#ffc164" : "#5e738b"
                                font.pixelSize: 16; font.bold: true
                            }
                        }
                    }
                    Label {
                        text: "Controller Fader: " + (root.state && root.state.softTakeover ? ((root.state.softTakeover.faderNorm - 0.5) * 200).toFixed(1) + "%" : "0.0%") +
                              "   |   Deck Speed: " + (root.state && root.state.softTakeover ? ((root.state.softTakeover.deckSpeedNorm - 0.5) * 200).toFixed(1) + "%" : "0.0%")
                        color: "#dce7f5"; font.pixelSize: 13
                    }
                    Label {
                        text: "Mirrors to /Engine/DeckN/SpeedOffsetUp, SpeedOffsetDown, SpeedNeutral."
                        color: "#7f96b0"; font.pixelSize: 11
                    }
                }
            }
        }

        Rectangle {
            Layout.fillWidth: true; Layout.preferredHeight: 155; radius: 6; color: "#15202e"
            ColumnLayout {
                anchors.fill: parent; anchors.margins: 12; spacing: 6
                Label { text: "3. REAL-TIME JOG INTEGRATOR & TELEMETRY (nice 19 · 60 Hz)"; color: "#93abc6"; font.pixelSize: 12; font.bold: true }
                GridLayout {
                    columns: 4; Layout.fillWidth: true; rowSpacing: 6; columnSpacing: 16
                    Label { text: "Last MIDI Bytes: " + ((root.state && root.state.telemetry && root.state.telemetry.lastMidiHex) ? root.state.telemetry.lastMidiHex : "—"); color: "#ffffff"; font.pixelSize: 13 }
                    Label { text: "Control: " + ((root.state && root.state.telemetry && root.state.telemetry.lastControl) ? root.state.telemetry.lastControl : "—"); color: "#64f2b8"; font.pixelSize: 13 }
                    Label { text: "Jog Velocity (Bn 10): " + ((root.state && root.state.telemetry) ? root.state.telemetry.jogVelocity : 0); color: "#ffffff"; font.pixelSize: 13 }
                    Label { text: "Integrated Δpos: " + ((root.state && root.state.telemetry) ? (root.state.telemetry.lastDeltaPosSec * 1000).toFixed(2) + " ms" : "0.00 ms"); color: "#ffffff"; font.pixelSize: 13 }
                    Label { text: "Latency p50: " + ((root.state && root.state.telemetry && root.state.telemetry.latency) ? root.state.telemetry.latency.p50Ms.toFixed(2) + " ms" : "0.00 ms"); color: "#75d9ff"; font.pixelSize: 13 }
                    Label { text: "Latency p95: " + ((root.state && root.state.telemetry && root.state.telemetry.latency) ? root.state.telemetry.latency.p95Ms.toFixed(2) + " ms" : "0.00 ms"); color: "#75d9ff"; font.pixelSize: 13 }
                    Label { text: "Latency max: " + ((root.state && root.state.telemetry && root.state.telemetry.latency) ? root.state.telemetry.latency.maxMs.toFixed(2) + " ms" : "0.00 ms"); color: "#75d9ff"; font.pixelSize: 13 }
                    Label { text: "Helper CPU (nice " + ((root.state && root.state.telemetry) ? root.state.telemetry.nice : 19) + "): " + ((root.state && root.state.telemetry) ? root.state.telemetry.cpuPercent.toFixed(2) + "%" : "0.00%"); color: "#b8cae0"; font.pixelSize: 13 }
                }
                Label {
                    text: "CDJ-400 continuous velocity integrator: Δpos = (velocity / 63) × dt × sensitivity (64 = stop). Touch (9n 20) engages vinyl scratch; outer ring (Bn 30) nudges pitch bend."
                    color: "#7f96b0"; font.pixelSize: 12; wrapMode: Text.Wrap; Layout.fillWidth: true
                }
            }
        }

        Rectangle {
            Layout.fillWidth: true; Layout.fillHeight: true; radius: 6; color: "#131c28"
            ColumnLayout {
                anchors.fill: parent; anchors.margins: 12; spacing: 4
                Label { text: "HARDWARE LED FEEDBACK & PRESET COMPATIBILITY NOTE"; color: "#f5c56b"; font.pixelSize: 12; font.bold: true }
                Label {
                    text: (root.state && root.state.ledFeedbackNote)
                        ? root.state.ledFeedbackNote
                        : "Pioneer CDJ-400 in standard USB MIDI mode (PC MODE) is transmit-only for button/LED states and does not illuminate hardware LEDs in response to incoming external MIDI Note/CC messages. Deck playback status, vinyl mode, and soft-takeover ▲▼ indicators are rendered directly on the SC5000 display."
                    color: "#b8cae0"; font.pixelSize: 12; wrapMode: Text.Wrap; Layout.fillWidth: true
                }
                Label {
                    text: "Custom controller JSON presets can be placed in /data/enginex/mods-state/midi-controller-mapper/presets/*.json."
                    color: "#7f96b0"; font.pixelSize: 12; wrapMode: Text.Wrap; Layout.fillWidth: true
                }
            }
        }
    }
}
