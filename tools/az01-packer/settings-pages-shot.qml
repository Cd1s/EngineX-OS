// Documentation only: open a Settings sub-tab and bring one row into view.
// doc-shots.sh fills @TABS@ (display names, any language) and @ROWS@ (row texts to show).
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0
Item {
    id: probe
    visible: false
    property int tick: 0
    property int switched: 0
    property int scrolled: 0
    function walk(o, f) {
        if (!o) return
        f(o)
        if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i], f)
    }
    Timer {
        interval: 1000; repeat: true; running: true
        onTriggered: {
            probe.tick++
            const win = probe.Window.window ? probe.Window.window.contentItem : null
            if (!win) return
            if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
            if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
            PlanckGUI.views.prompts.drop("StemsDownload")
            Planck.getProperty("/GUI/CloseDialogs").translator.state = true
            probe.walk(win, function(o) {
                if (o.objectName === "DismissButton" && typeof o.clicked === "function") o.clicked()
                if (o.objectName === "NextButton" && typeof o.clicked === "function") o.clicked()
            })
            try { PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings) } catch (e) {}
            if (!probe.switched) {
                probe.walk(win, function(o) {
                    if (probe.switched || o.objectName !== "ControlCenter_SubTabBar" || !o.visible) return
                    const tv = o.parent
                    if (!tv || !tv.tabs) return
                    const names = @TABS@
                    for (let i = 0; i < tv.tabs.count; i++) {
                        const d = tv.tabs.get(i) ? tv.tabs.get(i).display : ""
                        if (names.indexOf(d) >= 0) { o.currentIndex = i; probe.switched = probe.tick; return }
                    }
                })
            }
            if (probe.switched && probe.tick === probe.switched + 3) {
                let flick = null
                probe.walk(win, function(o) {
                    if (typeof o.contentY === "number" && typeof o.contentHeight === "number" && o.visible
                            && o.height > 300 && o.contentHeight > o.height + 10 && (!flick || o.height > flick.height)) flick = o
                })
                const rows = @ROWS@
                let row = null
                probe.walk(win, function(o) { if (o.visible && rows.indexOf(o.text) >= 0) row = o })
                if (flick && row) {
                    const y = row.mapToItem(flick.contentItem, 0, 0).y
                    if (y + row.height > flick.contentY + flick.height - 10 || y < flick.contentY)
                        flick.contentY = Math.max(flick.originY, Math.min(y - flick.height / 2, flick.originY + flick.contentHeight - flick.height))
                }
                probe.scrolled = probe.tick
            }
            if (probe.scrolled && probe.tick === probe.scrolled + 2)
                win.grabToImage(function(result) { result.saveToFile("/tmp/shot.png"); Qt.quit() })
        }
    }
}
