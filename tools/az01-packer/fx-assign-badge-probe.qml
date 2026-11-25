// Sandbox only: loaded over QmlWarnings.qml by fx-assign-badge-sandbox.sh (it fills in @BOARD@). Never shipped.
// #276: the pack's FX bar (MainView/Fx/DJFx.qml) with the FX assign badge, on a real Engine and UI hook.
// 1. The shipped file from the pack (qrc, compiled by Engine) on this board: no badge unless the board is an SC LIVE.
// 2. A copy of the pack's file whose board table maps this board to the SC LIVE 2 targets (the only change, plus the
//    two `required` properties given defaults because Qt.createQmlObject cannot pass them): the badge follows the
//    same /Engine/Mixer/<target>/DJFx/Assigned properties the stock JP20/JP21 selector writes (one true at a time).
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    readonly property string djfxUrl: "qrc:/Skins/Embedded/Views/MainView/Fx/DJFx.qml"
    readonly property var targets: ["Channel1", "Channel2", "Main"]
    property int step: 0
    property real readyAt: 0
    property int failures: 0
    property var shipped: null
    property var mapped: null
    property var saved: ({})
    // eglfs allows one window: the items go on a host inside Engine's own window, like the slot-icon probe.
    property var host: null

    function say(text) { console.warn("X276 BADGE|" + text + "|") }
    function expect(ok, message) {
        if (ok) { say("PASS " + message); return }
        failures++
        console.warn("X276 ERROR|" + message + "|")
    }
    function dismiss() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
    }
    function fileText(url) {
        const x = new XMLHttpRequest()
        x.open("GET", url, false)
        x.send()
        return x.responseText
    }
    function walk(o, f) {
        if (!o) return
        f(o)
        if (o.children) for (let i = 0; i < o.children.length; ++i) walk(o.children[i], f)
        if (o.contentItem && o.contentItem !== o) walk(o.contentItem, f)
    }
    function find(root, name) {
        let hit = null
        walk(root, function(o) { if (!hit && o.objectName === name) hit = o })
        return hit
    }
    function prop(target) { return Planck.getProperty("/Engine/Mixer/" + target + "/DJFx/Assigned") }
    // What the stock selector does for one position: exactly one target true (DJFxAssignSwitch.onNote).
    function select(on) {
        for (const t of targets) {
            const p = prop(t)
            if (p && p.translator) p.translator.state = on.indexOf(t) >= 0
        }
    }
    function shot(tag) {
        host.grabToImage(function(r) { say("picture " + tag + " " + r.saveToFile("/tmp/fx-assign-" + board + "-" + tag + ".png")) })
    }
    function label() {
        const t = find(mapped, "DJFx_Assign_Target")
        return t ? t.text : "(no label)"
    }
    function stepsFor() {
        const list = []
        list.push([3000, function() {
            dismiss()
            say("board " + Config.device.name + " hasDJFx " + Config.audio.fx.hasDJFx + " dualFx " + Config.audio.fx.hasDualFx)
            expect(Config.device.name === board, "Engine runs as " + board)
            for (const t of targets) {
                const p = prop(t)
                expect(!!(p && p.translator), "Engine has /Engine/Mixer/" + t + "/DJFx/Assigned")
                saved[t] = p && p.translator ? p.translator.state : false
            }
        }])
        list.push([1500, function() {
            host = Qt.createQmlObject('import QtQuick 2.15; Rectangle { z: 1000; color: "#141414"; width: 1280; height: 200 }', probe.Window.window.contentItem, "host")
            const c = Qt.createComponent(djfxUrl)
            expect(c.status === Component.Ready, "the pack's DJFx.qml compiles in Engine " + (c.status === Component.Error ? c.errorString() : ""))
            shipped = c.createObject(host, {djFxView: PlanckGUI.views.embeddedDJFx.fx1, suppressPopups: true, x: 20, y: 10, width: 1240, height: 72})
            expect(!!shipped, "the shipped FX bar was created")
            const badge = find(shipped, "DJFx_Assign")
            const isScLive = ["JP20", "JP21"].indexOf(board) >= 0
            expect(!!badge && badge.visible === isScLive, "shipped file on " + board + ": badge " + (isScLive ? "shown" : "hidden"))
            expect(!!find(shipped, "DJFx_Name_Parameter"), "the stock effect name is still there")
        }])
        list.push([1500, function() {
            let text = fileText("file:///data/enginex/ui/packs/en/Skins/Embedded/Views/MainView/Fx/DJFx.qml")
            expect(text.split('"JP20": [').length === 2, "the pack's board table has one JP20 entry")
            text = text.replace('"JP20": [', '"' + board + '": [')
            for (const [from, to] of [
                ["required property EmbeddedDJFxViewInterface djFxView", "property var djFxView: PlanckGUI.views.embeddedDJFx.fx1"],
                ["required property bool suppressPopups", "property bool suppressPopups: true"]]) {
                expect(text.split(from).length === 2, "modelled: " + from)
                text = text.replace(from, to)
            }
            mapped = Qt.createQmlObject(text, host, djfxUrl)
            mapped.x = 20; mapped.y = 110; mapped.width = 1240; mapped.height = 72
            const badge = find(mapped, "DJFx_Assign")
            expect(!!badge && badge.visible, "board mapped to the SC LIVE 2 table: badge shown")
            expect(!!badge && Math.round(badge.width) === 72, "badge is 72 px wide (" + (badge ? badge.width : -1) + ")")
            const name = find(mapped, "DJFx_Name_Parameter")
            expect(!!name && !!badge && badge.mapToItem(mapped, 0, 0).x < name.mapToItem(mapped, 0, 0).x, "badge sits left of the effect name")
        }])
        const cases = [[["Channel1"], "1", "ch1"], [["Channel2"], "2", "ch2"], [["Main"], "Main", "main"], [[], "–", "none"],
                       [["Channel1", "Main"], "1 Main", "two"]]
        for (const [on, want, tag] of cases) {
            list.push([600, function() { select(on) }])
            list.push([400, function() {
                expect(label() === want, "assigned " + JSON.stringify(on) + " shows \"" + want + "\" (got \"" + label() + "\")")
                shot(tag)
            }])
        }
        list.push([800, function() {
            for (const t of targets) { const p = prop(t); if (p && p.translator) p.translator.state = saved[t] }
            say("failures " + failures)
            console.warn("X276 DONE|" + (failures === 0 ? "PASS" : "FAIL") + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }])
        return list
    }
    readonly property var steps: stepsFor()
    Timer {
        interval: 250
        running: true
        repeat: true
        onTriggered: {
            const now = Date.now()
            if (now < probe.readyAt || probe.step >= probe.steps.length) return
            try {
                const entry = probe.steps[probe.step]
                entry[1]()
                probe.readyAt = now + entry[0]
                probe.step++
            } catch (e) {
                console.warn("X276 ERROR|step " + probe.step + ": " + e + "|")
                probe.failures++
                probe.step = probe.steps.length
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
