// Sandbox only: loaded over QmlWarnings.qml by share-decks-sandbox.sh in a player's own Engine 5.3.0 (Stock layout).
// #329 Share Decks: this player's decks are shown by a linked console's Line In view. The probe is the DJ: it checks that
// the player really runs the stock layout (Config.device.isPlayer, the stock Layer and Stop Time properties), loads the
// test track on Deck 1 and plays it, tells the console's driver when it is ready (GET @MARK@/ready), switches the stock
// Layer to B and back at fixed times, and switches the row off near the end, as a tap on the row does
// (POST /v1/enginex/ui?shareDecks=0). Everything else is observed from outside: the player's stream-sync (what the QML
// posted, when) and the console's stream-sync (what its Line In lane shows).
import QtQuick
import Planck

Item {
    id: probe
    visible: false
    readonly property string mark: "@MARK@"
    property int step: 0
    property real readyAt: 0
    property real stepStarted: 0
    property real t0: 0
    property int polls: 0
    function say(text) { console.warn("X329S|" + text + "|") }
    function opt(path) { const p = Planck.getProperty(path); return p && p.translator ? p.translator : null }
    function tr(path) { const t = opt(path); if (!t) throw new Error("missing " + path); return t }
    function flag(o, k) { return o && typeof o[k] !== "undefined" ? String(o[k]) : "absent" }
    function get(url) { const x = new XMLHttpRequest(); x.open("GET", url); x.send() }
    function dismiss() {
        const close = Planck.getProperty("/GUI/CloseDialogs")
        if (close && close.translator) close.translator.state = true
    }
    // The layout and the stock properties this feature leaves alone, or nothing below means anything.
    function evidence() {
        const device = typeof Config !== "undefined" && Config ? Config.device : null
        const layer = opt("/GUI/Decks/Deck/Layer"), player = opt("/Client/Preferences/Player"), ramp = opt("/Client/Preferences/RampDownTime")
        say("EVIDENCE isPlayer=" + flag(device, "isPlayer") + " layer=" + (layer ? layer.string + "/" + layer.index : "absent")
            + " playerNumber=" + (player ? player.string : "absent") + " stopTime=" + (ramp ? "present" : "absent")
            + " leftLayer=" + (opt("/GUI/Decks/DeckLeft/Layer") ? "present" : "absent"))
        if (!device || device.isPlayer !== true) throw new Error("not the Stock layout: Config.device.isPlayer is " + flag(device, "isPlayer"))
        if (!layer) throw new Error("the stock /GUI/Decks/Deck/Layer is missing")
    }
    function loaded() {
        const l = opt("/Engine/Deck1/Track/SongLoaded"), o = opt("/Engine/Deck1/OriginalBPM")
        if (++probe.polls % 20 === 1) say("load poll loaded=" + (l ? l.state : "absent") + " orig=" + (o ? o.unnormalized : "absent"))
        return !!l && l.state && !!o && o.unnormalized > 60
    }
    // The stock Layer as a Layer press leaves it: index 0 is A (Deck 1), 1 is B (Deck 2).
    function setLayer(index) {
        const t = tr("/GUI/Decks/Deck/Layer")
        t.index = index
        if (t.index !== index) t.string = index === 1 ? "B" : "A"
        say("LAYER want=" + index + " now=" + t.index + "/" + t.string + " at=" + Date.now())
    }
    function at(seconds) { return Date.now() - probe.t0 >= seconds * 1000 }
    readonly property var steps: [
        [6000, function() { dismiss() }],
        [500, function() { evidence() }],
        [500, function() { for (let n = 1; n <= 2; n++) { const p = opt("/Engine/Deck" + n + "/Play"); if (p) p.state = false }
                            tr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url = "file:///data/music/share-test.wav" }],
        [500, function() { return loaded() }],
        [3000, function() { tr("/Engine/Deck1/Play").state = true; say("PLAY at=" + Date.now()) }],
        [0, function() { probe.t0 = Date.now(); get(probe.mark + "/ready"); say("READY at=" + probe.t0) }],
        // The console asks from R+15 to R+45 (the driver's phases); the Layer moves inside that window.
        [0, function() { if (!at(25)) return false; setLayer(1) }],
        [0, function() { if (!at(33)) return false; setLayer(0) }],
        // The row off, as a tap on it: the console asks again from R+72.
        [0, function() { if (!at(70)) return false
                         const x = new XMLHttpRequest(); x.open("POST", "http://127.0.0.1:8080/v1/enginex/ui?shareDecks=0"); x.send()
                         say("ROW off at=" + Date.now()) }],
        [0, function() { if (!at(90)) return false; get(probe.mark + "/done"); say("DONE|play=" + tr("/Engine/Deck1/PlayState").state) }],
        [1000, function() { Planck.getProperty("/Client/Quit").translator.state = true }]
    ]
    Timer {
        interval: 100
        running: true
        repeat: true
        onTriggered: {
            const now = Date.now()
            if (now < probe.readyAt || probe.step >= probe.steps.length) return
            if (probe.stepStarted === 0) probe.stepStarted = now
            try {
                const done = probe.steps[probe.step][1]()
                if (done === false && now - probe.stepStarted < 240000) return
                if (done === false) throw new Error("step " + probe.step + " did not finish in 240 s")
                probe.readyAt = now + probe.steps[probe.step][0]
                probe.stepStarted = 0
                probe.step++
            } catch (e) {
                console.warn("X329S|ERROR|step " + probe.step + ": " + e + "|")
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
