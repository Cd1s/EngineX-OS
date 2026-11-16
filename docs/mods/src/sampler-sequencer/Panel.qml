import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12
import Planck 1.0

Rectangle {
    id: root
    width: 940
    height: 620
    color: "#111923"

    property double bpm: 120.0
    property double masterBpm: 0.0
    property string clockSource: "manual"
    property int clockDeck: 0
    property string division: "1/16"
    property int swing: 0
    property bool followDeck: true
    property bool playing: false
    property int currentStep: 0
    property double nextStepTimeMs: 0
    property var steps: [
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false],
        [false, false, false, false, false, false, false, false]
    ]
    property string message: "Tap cells to program steps, then press Play."

    readonly property double effectiveBpm: (followDeck && masterBpm > 0) ? masterBpm : bpm

    function request(body, clockOnly) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17839/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) {
                if (!clockOnly) root.message = xhr.responseText || "Helper unavailable"
                return
            }
            var v = JSON.parse(xhr.responseText)
            if (!clockOnly) {
                root.bpm = v.bpm
                root.division = v.division
                root.swing = v.swing
                root.followDeck = v.followDeck
                root.steps = v.steps
                if (v.note) root.message = v.note
            }
            if (v.clock && v.clock.bpm > 0) {
                root.masterBpm = v.clock.bpm
                root.clockSource = v.clock.source
                root.clockDeck = v.clock.deck
            } else {
                root.masterBpm = 0
                root.clockSource = "manual"
                root.clockDeck = 0
            }
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }

    function stepDurationMs(stepIdx) {
        var beatsPerStep = root.division === "1/4" ? 1.0 : (root.division === "1/8" ? 0.5 : 0.25)
        var baseMs = (60000.0 / Math.max(40.0, root.effectiveBpm)) * beatsPerStep
        var factor = (stepIdx % 2 === 0) ? (1.0 + root.swing / 100.0) : (1.0 - root.swing / 100.0)
        return Math.max(10.0, baseMs * factor)
    }

    function triggerStep(stepIdx) {
        for (var slot = 1; slot <= 8; slot++) {
            if (root.steps[slot - 1] && root.steps[slot - 1][stepIdx]) {
                var pLoaded = Planck.getProperty("/Engine/Sampler" + slot + "/Loaded")
                var loaded = pLoaded && pLoaded.translator ? pLoaded.translator.state : false
                if (loaded) {
                    var pTrig = Planck.getProperty("/Engine/Sampler" + slot + "/Triggered")
                    if (pTrig && pTrig.translator) {
                        pTrig.translator.state = true
                    }
                }
            }
        }
    }

    function togglePlay() {
        if (root.playing) {
            root.playing = false
            stepTimer.stop()
            return
        }
        root.currentStep = 0
        root.nextStepTimeMs = Date.now()
        root.playing = true
        stepTimer.interval = 1
        stepTimer.start()
    }

    Timer {
        id: stepTimer
        repeat: false
        onTriggered: {
            if (!root.playing) return
            root.triggerStep(root.currentStep)
            root.nextStepTimeMs += root.stepDurationMs(root.currentStep)
            root.currentStep = (root.currentStep + 1) % 8
            var now = Date.now()
            if (root.nextStepTimeMs < now - 200) {
                root.nextStepTimeMs = now
            }
            stepTimer.interval = Math.max(1, Math.round(root.nextStepTimeMs - now))
            stepTimer.start()
        }
    }

    Timer {
        id: clockTimer
        interval: 250
        repeat: true
        running: root.playing && root.followDeck
        onTriggered: root.request(null, true)
    }

    Component.onCompleted: root.request(null, false)
    Component.onDestruction: stepTimer.stop()

    ColumnLayout {
        anchors.fill: parent
        anchors.margins: 16
        spacing: 10

        RowLayout {
            Layout.fillWidth: true
            spacing: 10
            Label {
                text: "SAMPLER SEQUENCER · 8×8"
                font.pixelSize: 22
                font.bold: true
                color: "white"
                Layout.fillWidth: true
            }
            Button {
                text: root.playing ? "Stop" : "Play"
                highlighted: root.playing
                onClicked: root.togglePlay()
            }
            Button {
                text: "Clear"
                onClicked: root.request({action: "clear"}, false)
            }
        }

        RowLayout {
            Layout.fillWidth: true
            spacing: 8

            Button {
                text: root.followDeck ? "Clock: MASTER" : "Clock: Manual"
                highlighted: root.followDeck
                onClicked: root.request({action: "set", followDeck: !root.followDeck}, false)
            }
            Button {
                text: "-"
                enabled: !root.followDeck || root.masterBpm <= 0
                onClicked: root.request({action: "set", bpm: Math.max(40, root.bpm - 1)}, false)
            }
            Label {
                text: root.effectiveBpm.toFixed(1) + " BPM" + (root.clockDeck > 0 && root.followDeck ? " (Deck " + root.clockDeck + ")" : "")
                color: "#77e6bd"
                font.pixelSize: 15
                Layout.preferredWidth: 160
            }
            Button {
                text: "+"
                enabled: !root.followDeck || root.masterBpm <= 0
                onClicked: root.request({action: "set", bpm: Math.min(240, root.bpm + 1)}, false)
            }

            Repeater {
                model: ["1/4", "1/8", "1/16"]
                Button {
                    text: modelData
                    highlighted: root.division === modelData
                    onClicked: root.request({action: "set", division: modelData}, false)
                }
            }

            Repeater {
                model: [0, 25, 50]
                Button {
                    text: "Swing " + modelData + "%"
                    highlighted: root.swing === modelData
                    onClicked: root.request({action: "set", swing: modelData}, false)
                }
            }
        }

        ColumnLayout {
            Layout.fillWidth: true
            Layout.fillHeight: true
            spacing: 4

            Repeater {
                model: 8
                RowLayout {
                    id: slotRow
                    required property int index
                    readonly property int slotNum: index + 1
                    readonly property var pLoaded: Planck.getProperty("/Engine/Sampler" + slotNum + "/Loaded")
                    readonly property var pPlaying: Planck.getProperty("/Engine/Sampler" + slotNum + "/Playing")
                    readonly property var pLabel: Planck.getProperty("/Engine/Sampler" + slotNum + "/Label")
                    readonly property bool isLoaded: pLoaded && pLoaded.translator ? pLoaded.translator.state : false
                    readonly property bool isPlaying: pPlaying && pPlaying.translator ? pPlaying.translator.state : false
                    readonly property string slotLabel: pLabel && pLabel.translator && pLabel.translator.string ? pLabel.translator.string : ("Slot " + slotNum)

                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    spacing: 4

                    Rectangle {
                        Layout.preferredWidth: 150
                        Layout.fillHeight: true
                        radius: 4
                        color: slotRow.isPlaying ? "#245443" : "#1a2533"
                        border.color: slotRow.isLoaded ? "#4cc99b" : "#2d3b4d"
                        border.width: 1
                        Label {
                            anchors.fill: parent
                            anchors.margins: 6
                            verticalAlignment: Text.AlignVCenter
                            elide: Text.ElideRight
                            text: slotRow.slotNum + " · " + slotRow.slotLabel
                            color: slotRow.isLoaded ? "white" : "#7b8da3"
                            font.pixelSize: 13
                        }
                    }

                    Repeater {
                        model: 8
                        Button {
                            required property int index
                            readonly property bool activeCell: !!(root.steps[slotRow.index] && root.steps[slotRow.index][index])
                            readonly property bool isCurrentStep: root.playing && root.currentStep === index

                            Layout.fillWidth: true
                            Layout.fillHeight: true
                            text: (index + 1)
                            background: Rectangle {
                                radius: 4
                                color: activeCell ? (isCurrentStep ? "#77e6bd" : "#2d9d78") : (isCurrentStep ? "#28394d" : "#18222e")
                                border.color: isCurrentStep ? "#ffffff" : "#2b394a"
                                border.width: isCurrentStep ? 2 : 1
                            }
                            onClicked: root.request({action: "toggle", slot: slotRow.slotNum, step: index + 1}, false)
                        }
                    }
                }
            }
        }

        Label {
            text: root.message
            color: "#9db0c7"
            font.pixelSize: 12
            Layout.fillWidth: true
        }
    }
}
