import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 1024; height: 640; color: "#0f1722"
    property var state: ({})
    property string message: "Connect a USB MIDI controller (e.g. Pioneer CDJ-400) and enable the mapper."

    function syncBridgeState(val) {
        if (typeof Math.scMidiBridge === "undefined" || !Math.scMidiBridge) {
            Math.scMidiBridge = {
                installed: false,
                enabled: !!(val && val.enabled),
                connected: !!(val && val.connected),
                helperOnline: !!val,
                lastError: (val && val.error) ? String(val.error) : ""
            }
        } else {
            Math.scMidiBridge.helperOnline = !!val
            Math.scMidiBridge.connected = !!(val && val.connected)
            Math.scMidiBridge.lastError = (val && val.error) ? String(val.error) : ""
            if (val && val.enabled && Math.scMidiBridge.wake) {
                Math.scMidiBridge.wake()
            } else if (val && !val.enabled && Math.scMidiBridge.stop) {
                Math.scMidiBridge.stop()
            }
        }
    }

    function request(body, cb) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17838/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) {
                root.syncBridgeState(null)
                root.message = xhr.responseText || "MIDI Mapper helper unavailable. Run start.sh on the player."
                return
            }
            var val = JSON.parse(xhr.responseText)
            root.syncBridgeState(val)
            if (cb) {
                cb(val)
                return
            }
            root.state = val
            if (val.error) {
                root.message = "SAFETY AUTO-DISABLED: " + val.error
            } else if (val.presetErrors && val.presetErrors.length > 0) {
                root.message = "PRESET WARNING: " + val.presetErrors[0]
            } else if (val.running) {
                root.message = "LIVE · " + (val.connectedDevice || "MIDI Controller") + " → Deck " + val.targetDeck + " (" + (val.vinylMode ? "VINYL SCRATCH" : "CDJ PITCH BEND") + ")"
            } else if (val.enabled) {
                root.message = val.matchHint || ("Waiting for USB MIDI controller (" + (val.presetName || "CDJ-400") + ")...")
            } else {
                root.message = "Mapper disabled · Zero background polling · Hardware layers untouched."
            }
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }

    Component.onCompleted: request(null)
    // Only poll status while the panel is visible AND the Mod is enabled
    Timer {
        interval: 2000
        running: !!(root.visible && root.state && root.state.enabled)
        repeat: true
        onTriggered: root.request(null)
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
                visible: !!(root.state && (root.state.error || (root.state.alsaAvailable && !root.state.threadAlive)))
                text: "Restart Worker"
                onClicked: root.request({ action: "restart_worker" })
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
                              "  |  MIDI Port: " + ((root.state && root.state.connectedDevice) ? root.state.connectedDevice : ((root.state && root.state.enabled && root.state.matchHint) ? root.state.matchHint : "Auto-detect (preset match)"))
                        color: (root.state && root.state.enabled && !root.state.connectedDevice) ? "#f5c56b" : "#d4e2f4"; font.pixelSize: 13
                        elide: Text.ElideRight; Layout.fillWidth: true
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
                    text: (root.state && root.state.presetErrors && root.state.presetErrors.length > 0)
                        ? ("Ignored invalid preset files: " + root.state.presetErrors.join("; "))
                        : "Custom controller JSON presets can be placed in /data/enginex/mods-state/midi-controller-mapper/presets/*.json."
                    color: (root.state && root.state.presetErrors && root.state.presetErrors.length > 0) ? "#ff9da5" : "#7f96b0"
                    font.pixelSize: 12; wrapMode: Text.Wrap; Layout.fillWidth: true
                }
            }
        }
    }
}
