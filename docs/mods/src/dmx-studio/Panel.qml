import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12
import Planck 1.0

Rectangle {
    id: root
    width: 1024; height: 640; color: "#111923"
    property var state: ({})
    property string lastText: ""
    property var config: ({})
    property string message: "Start this Mod's start.sh helper to connect."
    property int universe: 1
    property bool initialized: false
    // #209 beat clock: follow the MASTER deck's beat. Panel-only switch: toggling it
    // never sends the edit form, and the clock action never touches show.json.
    property bool followDeck: true
    property bool clockBusy: false
    property string clockText: "Beat: manual BPM"
    property var deckProps: ({})
    function deckProp(d, name) {
        var key = d + name, t = deckProps[key]
        if (t === undefined) {
            var p = Planck.getProperty("/Engine/Deck" + d + name)
            t = p ? p.translator : null
            if (t) deckProps[key] = t
        }
        return t
    }
    // The playing MASTER deck, else the first playing deck, else a paused MASTER
    // (the look holds), else 0: no deck plays and the manual BPM runs.
    function clockDeck() {
        var master = 0, playing = 0
        for (var d = 1; d <= 4; d++) {
            var m = deckProp(d, "/DeckIsMaster"), p = deckProp(d, "/PlayState")
            var on = !!(p && p.state)
            if (m && m.state) {
                if (on) return {deck: d, playing: true}
                master = d
            } else if (on && !playing) playing = d
        }
        if (playing) return {deck: playing, playing: true}
        return {deck: master, playing: false}
    }
    function postClock(body) {
        if (clockBusy) return
        clockBusy = true
        var xhr = new XMLHttpRequest()
        xhr.open("POST", "http://127.0.0.1:17834/action")
        xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            root.clockBusy = false
            if (xhr.status !== 200) { root.clockText = "Beat clock: " + (xhr.responseText || "helper unavailable"); return }
            var c = JSON.parse(xhr.responseText).clock
            root.clockText = c.source === "deck" ? "Beat: Deck " + c.deck + (c.playing ? "" : " (paused, holding)") + " · " + c.bpm.toFixed(1) + " BPM · beat " + (Math.floor(c.beat) + 1)
                                                 : "Beat: manual " + c.bpm + " BPM"
        }
        xhr.send(JSON.stringify(body))
    }
    function feedClock() {
        var pick = clockDeck(), d = pick.deck
        var beat = d ? deckProp(d, "/BeatNumber") : null
        if (!d || !beat || beat.unnormalized === undefined) { postClock({action:"clock", deck:0}); return }
        var bpm = deckProp(d, "/CurrentBPM"), pos = deckProp(d, "/Track/TrackPosition")
        postClock({action:"clock", deck:d, playing:pick.playing, beat:beat.unnormalized,
                   bpm:bpm && bpm.unnormalized > 0 ? bpm.unnormalized : 0,
                   position:pos ? pos.unnormalized : null, at:Date.now()})
    }
    onFollowDeckChanged: if (!followDeck) postClock({action:"clock", deck:0})
    function request(body) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17834/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { root.message = xhr.responseText || "Helper unavailable. Run start.sh on the player."; return }
            var value = JSON.parse(xhr.responseText)
            var changed = xhr.responseText !== root.lastText
            root.lastText = xhr.responseText
            root.state = value
            root.message = value.error || (value.running ? "OUTPUT LIVE — 30 Hz" : "Output stopped / preview only")
            if (!root.initialized) {
                root.config = value.config
                outputs.text = JSON.stringify(value.config.outputs, null, 2)
                patch.text = JSON.stringify(value.config.patch, null, 2)
                root.initialized = true
            }
            if (changed) stage.requestPaint()
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }
    function apply() {
        try {
            config.outputs = JSON.parse(outputs.text); config.patch = JSON.parse(patch.text)
            config.bpm = bpm.value; config.effect = effect.currentText
            config.intensity = intensity.value; config.red = red.value; config.green = green.value; config.blue = blue.value
            config.smokeLevel = smoke.value; config.smokeInterval = interval.value; config.smokeDuration = duration.value
            config.smokeArmed = armed.checked
            request({action:"configure", config:config})
        } catch (error) { message = String(error) }
    }
    Component.onCompleted: request(null)
    Timer { interval: 500; running: root.visible; repeat: true; onTriggered: root.request(null) }
    Timer { interval: 150; running: flash.down; repeat: true; onTriggered: root.request({action:"flash", held:true}) }
    // 10 Hz with the read time; the helper extrapolates between updates.
    Timer { interval: 100; running: root.followDeck && (root.visible || !!root.state.running); repeat: true; onTriggered: root.feedClock() }
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 14; spacing: 8
        Label { text: "DMX STUDIO · Independent lighting console"; color: "white"; font.pixelSize: 24 }
        Label { text: root.message; color: root.state.running ? "#54f0b1" : "#f8ca76"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        RowLayout {
            Button { text: "Apply controls / show"; enabled: root.initialized; onClicked: root.apply() }
            Button { text: "START OUTPUT"; onClicked: root.request({action:"start"}) }
            Button { text: "BLACKOUT / STOP"; onClicked: { armed.checked = false; root.request({action:"stop"}) } }
            Label { text: "Monitor universe"; color: "white" }
            SpinBox { from: 1; to: 32768; value: 1; onValueModified: root.universe = value }
        }
        TabBar {
            id: tabs; Layout.fillWidth: true
            TabButton { text: "Fixtures & output" }
            TabButton { text: "BPM / EFX / smoke" }
            TabButton { text: "2D stage" }
            TabButton { text: "512-channel monitor" }
        }
        StackLayout {
            currentIndex: tabs.currentIndex; Layout.fillWidth: true; Layout.fillHeight: true
            ScrollView {
                clip: true
                ColumnLayout {
                    width: root.width - 60
                    Label { text: "Outputs JSON · select host/device explicitly; internal universe 1 → Art-Net 0"; color: "white" }
                    TextArea {
                        id: outputs; Layout.fillWidth: true; Layout.preferredHeight: 125; selectByMouse: true; color: "white"
                        placeholderText: '[{"protocol":"artnet","universe":1,"host":"192.168.1.50"}]'
                        wrapMode: TextEdit.Wrap
                    }
                    Label { text: "USB route: ftdi + device:/dev/ttyUSB0, or udmx + bus:1,address:2. sACN uses host/universe."; color: "#abb9cc"; wrapMode: Text.Wrap; Layout.fillWidth: true }
                    RowLayout {
                        ComboBox { id: profile; model: ["LED Par", "LED Bar", "Moving Head Beam", "Moving Head Wash", "Moving Head Spot", "Moving Head 3-in-1", "Smoke"] }
                        Label { text: "Address"; color: "white" }
                        SpinBox { id: address; from: 1; to: 512 }
                        Button {
                            text: "Add fixture to patch"
                            onClicked: {
                                try {
                                    var fixtures = JSON.parse(patch.text || "[]")
                                    fixtures.push({profile:profile.currentText, address:address.value, universe:root.universe, x:0.5, y:0.5, gobo:0, beam:0, mode:0})
                                    patch.text = JSON.stringify(fixtures, null, 2)
                                } catch (error) { root.message = String(error) }
                            }
                        }
                    }
                    TextArea { id: patch; Layout.fillWidth: true; Layout.preferredHeight: 160; selectByMouse: true; color: "white"; wrapMode: TextEdit.Wrap; placeholderText: "[]" }
                    Label { text: "Generic profiles: verify channel order with your fixture manual. x/y: 0..1 stage position; gobo/beam/mode/strobe: 0..255. Stop before editing routes/patch."; color: "#abb9cc"; wrapMode: Text.Wrap; Layout.fillWidth: true }
                }
            }
            ScrollView {
                clip: true
                ColumnLayout {
                    width: root.width - 60
                    RowLayout {
                        Label { text: "BPM"; color: "white" }
                        SpinBox { id: bpm; from: 20; to: 300; value: root.config.bpm || 120 }
                        ComboBox { id: effect; model: ["Circle", "8-Figure", "Pan-Tilt Sweep", "Static"] }
                        CheckBox { text: "Follow MASTER deck beat"; checked: root.followDeck; onToggled: root.followDeck = checked }
                        Label { text: root.followDeck ? root.clockText : "BPM is manually set."; color: "#abb9cc" }
                    }
                    Label { text: "Master intensity"; color: "white" }
                    Slider { id: intensity; from: 0; to: 255; stepSize: 1; value: root.config.intensity || 0; Layout.fillWidth: true }
                    RowLayout {
                        Label { text: "RGB"; color: "white" }
                        SpinBox { id: red; from: 0; to: 255; value: root.config.red === undefined ? 255 : root.config.red }
                        SpinBox { id: green; from: 0; to: 255; value: root.config.green === undefined ? 255 : root.config.green }
                        SpinBox { id: blue; from: 0; to: 255; value: root.config.blue === undefined ? 255 : root.config.blue }
                    }
                    Label { text: "Smoke level · mapped only to fixtures with the Smoke profile"; color: "white" }
                    Slider { id: smoke; from: 0; to: 255; stepSize: 1; value: root.config.smokeLevel || 0; Layout.fillWidth: true }
                    RowLayout {
                        Label { text: "Interval seconds"; color: "white" }
                        SpinBox { id: interval; from: 1; to: 3600; value: root.config.smokeInterval || 30 }
                        Label { text: "Duration seconds"; color: "white" }
                        SpinBox { id: duration; from: 0; to: 60; value: root.config.smokeDuration || 0 }
                        CheckBox { id: armed; text: "Arm smoke"; checked: false }
                        Button {
                            id: flash; text: "SMOKE FLASH · HOLD"; enabled: armed.checked && !!root.state.running
                            onPressed: root.request({action:"flash", held:true})
                            onReleased: root.request({action:"flash", held:false})
                            onCanceled: root.request({action:"flash", held:false})
                        }
                    }
                    Label { text: "Apply after changing controls. Set duration to 0 for flash-only. Flash expires within 0.5s without a held heartbeat. Smoke disarms on restart/blackout."; color: "#abb9cc"; wrapMode: Text.Wrap; Layout.fillWidth: true }
                }
            }
            Canvas {
                id: stage
                onPaint: {
                    var ctx = getContext("2d")
                    ctx.fillStyle = "#080e17"; ctx.fillRect(0, 0, width, height)
                    var fixtures = root.state.config ? root.state.config.patch : []
                    for (var i = 0; i < fixtures.length; ++i) {
                        var f = fixtures[i], x = (f.x === undefined ? 0.5 : f.x) * width, y = (f.y === undefined ? 0.5 : f.y) * height
                        var frame = root.state.frames[String(f.universe)] || []
                        ctx.fillStyle = root.state.running ? "#52e8b0" : "#63738a"
                        ctx.beginPath(); ctx.arc(x, y, 12, 0, Math.PI * 2); ctx.fill()
                        ctx.fillStyle = "white"; ctx.fillText(f.profile + " U" + f.universe + ":" + f.address, x + 16, y)
                        if (f.profile.indexOf("Moving Head") === 0) {
                            var pan = ((frame[f.address - 1] || 0) * 256 + (frame[f.address] || 0)) / 65535
                            var tilt = ((frame[f.address + 1] || 0) * 256 + (frame[f.address + 2] || 0)) / 65535
                            ctx.strokeStyle = "#52e8b0"; ctx.beginPath(); ctx.moveTo(x, y); ctx.lineTo(x + (pan - 0.5) * 160, y + (tilt - 0.5) * 160); ctx.stroke()
                        }
                    }
                }
            }
            ScrollView {
                clip: true
                Grid {
                    columns: 16; spacing: 3
                    Repeater {
                        model: 512
                        Rectangle {
                            width: 55; height: 34
                            property int level: root.state.frames && root.state.frames[String(root.universe)] ? root.state.frames[String(root.universe)][index] : 0
                            color: Qt.rgba(0.1, 0.25 + level / 510, 0.28, 1)
                            Label { anchors.centerIn: parent; text: (index + 1) + ": " + parent.level; font.pixelSize: 10; color: "white" }
                        }
                    }
                }
            }
        }
    }
}
