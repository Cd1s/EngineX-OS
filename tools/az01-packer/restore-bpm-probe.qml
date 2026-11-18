// Sandbox only: loaded over QmlWarnings.qml by restore-bpm-sandbox.sh (it fills in @BOARD@ and @E2E@).
// #289: Engine's own Sync on two real tracks (128 and 124 BPM): UnSync with the setting Off keeps the synced rate (stock),
// with it On and the tempo control at 0 % the deck returns to its own BPM, with the control moved it stays synced.
// Also renders the shipped Profile -> Playback page with the new switch row.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    readonly property bool e2e: "@E2E@" === "1"
    property int step: 0
    property real readyAt: 0
    property real stepStarted: 0
    property var ctx: ({})
    property int polls: 0
    property int failures: 0
    function say(text) { console.warn("X166 RB|" + text + "|") }
    function expect(ok, message) {
        if (ok) { say("PASS " + message); return }
        failures++
        console.warn("X166 ERROR|" + message + "|")
    }
    function check(ok, message) {
        if (!ok) throw new Error(message)
        say("PASS " + message)
    }
    function opt(path) { const p = Planck.getProperty(path); return p && p.translator ? p.translator : null }
    function tr(path) { const t = opt(path); if (!t) throw new Error("missing " + path); return t }
    function num(x) { return typeof x === "number" ? Math.round(x * 1000) / 1000 : x }
    function bpm(d, sub) { return tr("/Engine/Deck" + d + sub).unnormalized }
    function sync(d) { return tr("/Engine/Deck" + d + "/SyncMode").string }
    function snap(tag) {
        say(tag + " | deck1 cur=" + num(bpm(1, "/CurrentBPM")) + " | deck2 sync=" + sync(2) + " orig=" + num(bpm(2, "/OriginalBPM"))
            + " cur=" + num(bpm(2, "/CurrentBPM")) + " speed=" + tr("/Engine/Deck2/Speed").value + " range=" + tr("/Engine/Deck2/SpeedRange").string
            + " master2=" + tr("/Engine/Deck2/DeckIsMaster").state)
    }
    function dismiss() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        PlanckGUI.views.prompts.drop("StemsDownload")
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
    }
    function dismissOverlays() {
        const win = probe.Window.window, roots = [win.contentItem]
        if (win.overlay) roots.push(win.overlay)
        let pressed = 0
        const seen = new Set()
        function press(label) {
            for (let a = label, n = 0; a && n < 5; a = a.parent, n++) {
                if (typeof a.clicked === "function") { try { a.clicked(); return true } catch (e) { say("dismiss: " + e) } }
                for (let i = 0; a.children && i < a.children.length; i++) {
                    const c = a.children[i]
                    if (c !== label && typeof c.clicked === "function" && c.width > 0) { try { c.clicked(null); return true } catch (e) { say("dismiss: " + e) } }
                }
            }
            return false
        }
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.text === "Dismiss" && o.visible && press(o)) pressed++
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
            if (o.item) walk(o.item)
        }
        for (const r of roots) walk(r)
        return pressed
    }
    function shot(tag) {
        probe.Window.window.contentItem.grabToImage(function(r) { say("picture " + tag + " " + r.saveToFile("/tmp/rb-" + board + "-" + tag + ".png")) })
    }
    // Settings -> Playback as shipped: the new switch row, bound to the pack's flag.
    function playbackPage() {
        const component = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/PlaybackTabContent.qml")
        check(component.status === Component.Ready, "Playback settings load: " + component.errorString())
        const page = component.createObject(probe.Window.window.contentItem, {width: 1280, height: 1800, z: 1000})
        check(!!page, "create the Playback settings")
        const seen = new Set()
        let row = null
        const labels = []
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.checkedExternally !== undefined && typeof o.text === "string") {
                labels.push(o.text)
                if (o.text === "Restore BPM on Sync Off") row = o
            }
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
            if (o.item) walk(o.item)
        }
        walk(page)
        say("switch rows on the page: " + labels.join(" | "))
        check(!!row && row.visible, "Restore BPM on Sync Off is a switch row in Profile -> Playback")
        root.scRestoreBpmOnUnsync = false
        check(row.checkedExternally === false, "the row follows the flag: Off")
        root.scRestoreBpmOnUnsync = true
        check(row.checkedExternally === true, "the row follows the flag: On")
        page.grabToImage(function(r) { say("picture playback-page " + r.saveToFile("/tmp/rb-" + board + "-playback-page.png")) })
        row.grabToImage(function(r) { say("picture playback-row " + r.saveToFile("/tmp/rb-" + board + "-playback-row.png")) }, Qt.size(row.width * 2, row.height * 2))
        ctx.page = page
    }
    function load(deck, file) { tr("/Client/Librarian/AddToDatabaseAndLoadToDeck" + deck).url = "file:///data/music/" + file }
    function loaded(deck) {
        const l = opt("/Engine/Deck" + deck + "/Track/SongLoaded"), o = opt("/Engine/Deck" + deck + "/OriginalBPM")
        if (++probe.polls % 20 === 1) say("load poll deck " + deck + " loaded=" + (l ? l.state : "absent") + " orig=" + (o ? num(o.unnormalized) : "absent"))
        return !!l && l.state && !!o && o.unnormalized > 60 && Math.abs(o.unnormalized - 120) > 0.1
    }
    readonly property var pagePart: [
        [500, function() { dismiss() }],
        [600, function() { dismissOverlays(); playbackPage() }],
        [1500, function() { shot("playback-window"); if (ctx.page) ctx.page.destroy() }]
    ]
    readonly property var load1: [
        [500, function() { for (let n = 1; n <= 4; n++) { const p = opt("/Engine/Deck" + n + "/Play"); if (p) p.state = false }; load(1, "cd-test.wav") }],
        [2000, function() { return loaded(1) }],
        [500, function() { load(2, "cd-test2.wav") }],
        [2000, function() { return loaded(2) }],
        [800, function() {
            dismissOverlays()
            snap("both loaded")
            expect(Math.abs(bpm(1, "/OriginalBPM") - 128) < 1 && Math.abs(bpm(2, "/OriginalBPM") - 124) < 1, "tracks analysed as 128 and 124 BPM")
            tr("/Engine/Deck1/Play").state = true
            tr("/Engine/Deck2/Play").state = true
        }],
        [1500, function() { tr("/Engine/Deck1/RequestMaster").state = true }],
        [1500, function() { snap("playing, deck 1 master"); expect(sync(2) === "Off", "deck 2 starts unsynced"); root.scRestoreBpmOnUnsync = false }]
    ]
    function syncOn() { root.scPress("/Engine/Deck2/Sync", true) }
    function syncOff() { root.scPress("/Engine/Deck2/UnSync", true) }
    readonly property var stock: [
        [300, function() { syncOn() }],
        [2500, function() { snap("OFF: synced"); expect(sync(2) !== "Off", "Sync engaged"); expect(Math.abs(bpm(2, "/CurrentBPM") - bpm(1, "/CurrentBPM")) < 0.3, "deck 2 runs at deck 1's tempo") }],
        [300, function() { syncOff() }],
        [1500, function() {
            snap("OFF: after UnSync")
            expect(sync(2) === "Off", "Sync is off")
            expect(Math.abs(bpm(2, "/CurrentBPM") - bpm(1, "/CurrentBPM")) < 0.3, "setting Off: the deck keeps the synced rate (stock)")
        }]
    ]
    readonly property var restoring: [
        [300, function() { root.scRestoreBpmOnUnsync = true; syncOn() }],
        [2500, function() { snap("ON: synced"); expect(sync(2) !== "Off", "Sync engaged again"); ctx.speed = tr("/Engine/Deck2/Speed").value; shot("synced") }],
        [300, function() { syncOff() }],
        [1500, function() {
            snap("ON: after UnSync, control at 0 %")
            expect(sync(2) === "Off", "Sync is off")
            expect(Math.abs(bpm(2, "/CurrentBPM") - bpm(2, "/OriginalBPM")) < 0.05, "setting On, control at 0 %: the deck is back at its own BPM")
            expect(Math.abs(tr("/Engine/Deck2/Speed").value - 0.5) < 1e-6, "the control reads 0 % again")
            shot("restored")
        }]
    ]
    readonly property var moved: [
        [300, function() { tr("/Engine/Deck2/Speed").value = 0.53 }],
        [1200, function() { snap("moved control, unsynced"); syncOn() }],
        [2500, function() { snap("ON: synced with the control moved"); expect(sync(2) !== "Off", "Sync engaged with a moved control") }],
        [300, function() { syncOff() }],
        [1500, function() {
            snap("ON: after UnSync, control moved")
            expect(Math.abs(bpm(2, "/CurrentBPM") - bpm(1, "/CurrentBPM")) < 0.3, "control moved: nothing changes, the deck keeps the synced rate (today's behaviour)")
            expect(Math.abs(tr("/Engine/Deck2/Speed").value - 0.53) < 1e-6, "the moved control was not touched")
            tr("/Engine/Deck2/Speed").value = 0.5
        }]
    ]
    readonly property var finish: [
        [500, function() {
            say("failures " + failures)
            console.warn("X166 DONE|" + (failures === 0 ? "PASS" : "FAIL") + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }]
    ]
    readonly property var steps: e2e ? load1.concat(stock).concat(restoring).concat(moved).concat(pagePart).concat(finish) : pagePart.concat(finish)
    Timer {
        interval: 250
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
                console.warn("X166 ERROR|step " + probe.step + ": " + e + "|")
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
