import QtQuick 2.5

// Loaded over loopback, not through a newly invented qrc node. The host only
// supplies its existing cached Engine translator and language functions.
Item {
    id: panel
    property var read: null
    property var translateText: null
    property bool opened: false
    property var config: null
    property bool soundSwitch: true
    property string statusText: ""
    property bool clockBusy: false
    property double clockAt: 0
    function t(s) { return translateText ? translateText(s) : s }
    function call(action, value) {
        const x = new XMLHttpRequest()
        x.open(action ? "POST" : "GET", "http://127.0.0.1:8080/v1/sc5000/dmx")
        x.onreadystatechange = function() {
            if (x.readyState !== 4) return
            if (x.status !== 200) { statusText = x.responseText; return }
            try {
                const v = JSON.parse(x.responseText)
                config = v.config
                soundSwitch = !!v.soundSwitch
                statusText = v.error || (v.synced ? v.clock.bpm.toFixed(2) + " BPM" : t("Waiting for master"))
            } catch (e) { statusText = String(e) }
        }
        x.setRequestHeader("Content-Type", "application/json")
        x.send(action ? JSON.stringify({action: action, config: value}) : "")
    }
    function setValue(key, value) {
        if (!config) return
        const c = JSON.parse(JSON.stringify(config))
        c[key] = value
        call("configure", c)
    }
    function sample() {
        if (!read || !config || !config.enabled || soundSwitch) return
        const now = Date.now()
        if (clockBusy && now - clockAt < 1000) return
        let master = 0
        for (let d = 1; d <= 4; d++) {
            if ((read("/Engine/Deck" + d + "/DeckIsMaster") || {}).state) { master = d; break }
        }
        // Same network-master fallback as stream-sync's existing deck posts.
        if (!master && (read("/Engine/Sync/Network/MasterStatus") || {}).state) {
            for (let d = 1; d <= 4; d++) {
                if ((read("/Engine/Deck" + d + "/PlayState") || {}).state) { master = d; break }
            }
        }
        const decks = []
        if (master) {
            const g = p => read("/Engine/Deck" + master + p) || {}
            decks.push({deck: master, master: true, at: now,
                playing: !!g("/PlayState").state,
                bpm: g("/OriginalBPM").unnormalized || 0,
                pitch: parseFloat(g("/SpeedState").string) || 0,
                beat: g("/BeatNumber").unnormalized || 0})
        }
        const x = new XMLHttpRequest()
        clockBusy = true; clockAt = now
        x.open("POST", "http://127.0.0.1:8080/v1/sc5000/dmx/clock")
        x.onreadystatechange = function() { if (x.readyState === 4 && clockAt === now) clockBusy = false }
        x.send(JSON.stringify(decks))
    }
    Component.onCompleted: call("", null)
    Timer { interval: 1000; repeat: true; running: true; onTriggered: panel.call("", null) }
    Timer { interval: 100; repeat: true; running: panel.config && panel.config.enabled && !panel.soundSwitch; onTriggered: panel.sample() }
    Rectangle {
        anchors.right: parent.right; anchors.bottom: parent.bottom
        anchors.margins: 12; width: 156; height: 44; radius: 8
        color: "#135c50"; visible: !panel.opened && !panel.soundSwitch
        Text { anchors.centerIn: parent; text: panel.t("DMX Lighting"); color: "white"; font.pixelSize: 20 }
        MouseArea { anchors.fill: parent; onClicked: { panel.opened = true; panel.call("", null) } }
    }
    Rectangle {
        anchors.fill: parent; color: "#101720"; visible: panel.opened && !panel.soundSwitch
        MouseArea { anchors.fill: parent } // contain touches within this overlay
        Column {
            anchors.centerIn: parent; width: Math.min(parent.width - 40, 720); spacing: 18
            Text { text: panel.t("DMX Lighting") + " · " + panel.statusText; color: "white"; font.pixelSize: 24; width: parent.width; wrapMode: Text.Wrap }
            Row {
                spacing: 12
                Repeater {
                    model: ["Blackout", "Resume", "Close"]
                    Rectangle {
                        width: 190; height: 56; radius: 8; color: index === 0 ? "#982d42" : "#24534d"
                        Text { anchors.centerIn: parent; text: panel.t(modelData); color: "white"; font.pixelSize: 22 }
                        MouseArea { anchors.fill: parent; onClicked: { if (index === 0) panel.call("blackout", null); else if (index === 1) panel.setValue("blackout", false); else panel.opened = false } }
                    }
                }
            }
            Text { text: panel.t("Master Dimmer") + ": " + (panel.config ? Math.round(panel.config.master / 255 * 100) : 0) + "%"; color: "white"; font.pixelSize: 22 }
            Rectangle {
                width: parent.width; height: 56; color: "#303b4c"; radius: 8
                Rectangle { width: parent.width * (panel.config ? panel.config.master / 255 : 0); height: parent.height; color: "#159c85"; radius: 8 }
                MouseArea { anchors.fill: parent; onReleased: panel.setValue("master", Math.max(0, Math.min(255, Math.round(mouse.x / width * 255)))) }
            }
            Row {
                spacing: 8
                Repeater {
                    model: ["Circle", "8-Figure", "Pan-Tilt Sweep", "Static"]
                    Rectangle {
                        width: 168; height: 56; radius: 8; color: panel.config && panel.config.effect === modelData ? "#159c85" : "#303b4c"
                        Text { anchors.centerIn: parent; text: panel.t(modelData); color: "white"; font.pixelSize: 18 }
                        MouseArea { anchors.fill: parent; onClicked: panel.setValue("effect", modelData) }
                    }
                }
            }
            Row {
                spacing: 12
                Repeater {
                    model: ["#ffffff", "#ff0000", "#00ff00", "#0000ff", "#ff00ff", "#00ffff", "#ff9900"]
                    Rectangle {
                        width: 80; height: 56; radius: 8; color: modelData
                        MouseArea {
                            anchors.fill: parent
                            onClicked: {
                                if (!panel.config) return
                                const c = JSON.parse(JSON.stringify(panel.config))
                                c.red = parseInt(modelData.slice(1,3),16); c.green = parseInt(modelData.slice(3,5),16); c.blue = parseInt(modelData.slice(5,7),16)
                                panel.call("configure", c)
                            }
                        }
                    }
                }
            }
            Text { text: panel.t("Patch fixtures in the Web Console"); color: "#aebaca"; font.pixelSize: 20 }
        }
    }
}
