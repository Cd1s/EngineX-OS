// Sandbox only: loaded over QmlWarnings.qml by cc-fit-sandbox.sh (it fills in the two @...@ words).
// Lays the shipped Control Center tab out at the board's real panel size, with that board's own Engine Config, and checks
// that every tile is on screen and none reaches the Source / Wi-Fi / Bluetooth row anchored at the bottom of the tab.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    property int stage: 0
    property bool failed: false
    readonly property string board: "@BOARD@"
    readonly property int panelW: @WIDTH@
    readonly property int panelH: @HEIGHT@
    property var screen: null
    property var tab: null
    function check(ok, message) {
        if (!ok) throw new Error(message)
        console.warn("X166 PASS|" + message + "|")
    }
    function find(name, from) {
        const seen = new Set()
        function walk(o) {
            if (!o || seen.has(o)) return null
            seen.add(o)
            if (o.objectName === name) return o
            const children = []
            if (o.children) for (let i = 0; i < o.children.length; i++) children.push(o.children[i])
            for (const c of children) { const found = walk(c); if (found) return found }
            return null
        }
        return walk(from)
    }
    function rect(item) {
        const a = item.mapToItem(tab, 0, 0), b = item.mapToItem(tab, item.width, item.height)
        return {x: a.x, y: a.y, w: b.x - a.x, h: b.y - a.y}
    }
    function meets(a, b) { return a.x < b.x + b.w - 0.01 && b.x < a.x + a.w - 0.01 && a.y < b.y + b.h - 0.01 && b.y < a.y + a.h - 0.01 }
    function build() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        PlanckGUI.views.prompts.drop("StemsDownload")
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
        const component = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/ControlCenterTab.qml")
        check(component.status === Component.Ready, "Control Center tab loads: " + component.errorString())
        // The panel, as the open Control Center lays it out: an 80 px tab bar above the tab and an 80 px status bar below it.
        screen = Qt.createQmlObject('import QtQuick 2.12; Rectangle { color: "#1c1c1c"
            Rectangle { width: parent.width; height: 80; color: "#2e2e2e" }
            Rectangle { y: parent.height - 80; width: parent.width; height: 80; color: "#2e2e2e" } }', probe.Window.window.contentItem)
        screen.width = panelW; screen.height = panelH; screen.z = 1000
        tab = component.createObject(screen, {y: 80, width: panelW, height: panelH - 160})
        check(!!tab, "create the Control Center tab")
    }
    function measure() {
        let flow = null
        for (let i = 0; i < tab.children.length; i++) if (tab.children[i].numColumns !== undefined) flow = tab.children[i]
        const bottom = find("ControlCenter_AdditionalControls", tab)
        check(!!flow && !!bottom, "found the tile Flow and the bottom row")
        const bottomRect = rect(bottom), tiles = [], xs = new Set(), ys = new Set()
        let covered = 0, outside = 0, odd = 0
        for (let i = 0; i < flow.children.length; i++) {
            const t = flow.children[i]
            if (!t.visible) continue
            const r = rect(t)
            tiles.push(r)
            xs.add(Math.round(r.x)); ys.add(Math.round(r.y))
            console.warn("X166 CCRECT|" + board + "|" + i + "|" + r.x.toFixed(2) + "|" + r.y.toFixed(2) + "|" + r.w.toFixed(2) + "|" + r.h.toFixed(2) + "|")
            if (meets(r, bottomRect)) covered++
            if (r.x < -0.01 || r.x + r.w > panelW + 0.01 || r.y < 0 || r.y + r.h > tab.height + 0.01) outside++
            if (t.width !== 256 || t.height !== 104) odd++
        }
        // The Engine cuts a log line at about 100 characters.
        console.warn("X166 CCFIT|" + board + "|" + Config.device.name + "|" + panelW + "x" + panelH + "|tiles=" + tiles.length
                     + "|cols=" + xs.size + "|rows=" + ys.size + "|")
        console.warn("X166 CCGRID|" + board + "|scale=" + flow.scale.toFixed(3)
                     + "|lowest=" + Math.max.apply(null, tiles.map(r => r.y + r.h)).toFixed(1) + "|bottomRowTop=" + bottomRect.y.toFixed(1) + "|")
        console.warn("X166 CCOVER|" + board + "|covered=" + covered + "|outside=" + outside + "|")
        check(tiles.length > 0, "the Control Center shows tiles")
        check(odd === 0, "every tile is 256 x 104 (the size the grid fit counts with)")
        check(covered === 0, "no tile reaches the bottom row (" + covered + " of " + tiles.length + " are under it)")
        check(outside === 0, "every tile is inside the tab (" + outside + " are not)")
    }
    Timer {
        interval: 1500
        running: true
        repeat: true
        onTriggered: {
            try {
                if (probe.stage === 0) probe.build()
                else if (probe.stage === 3) {
                    probe.screen.grabToImage(function(r) { console.warn("X166 SHOT|" + probe.board + "|" + r.saveToFile("/tmp/mx-cc-fit.png") + "|") })
                    probe.measure()
                } else if (probe.stage === 5) {
                    console.warn("X166 DONE|" + (probe.failed ? "FAIL" : "PASS") + "|")
                    Planck.getProperty("/Client/Quit").translator.state = true
                    running = false
                }
            } catch (e) {
                console.warn("X166 ERROR|" + e + "|")
                probe.failed = true
                if (probe.stage === 3) probe.stage = 4 // the measurements were printed; let the picture finish
                else { Planck.getProperty("/Client/Quit").translator.state = true; running = false }
            }
            probe.stage++
        }
    }
}
