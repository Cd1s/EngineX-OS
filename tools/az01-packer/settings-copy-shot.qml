// Documentation only: render the actual receiving-deck confirmation with sample preferences.
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0
Item {
    id: probe
    visible: false
    property int tick: 0
    property bool posted: false
    function walk(o, f) {
        if (!o) return
        f(o)
        if (o.children) for (let i=0; i<o.children.length; i++) walk(o.children[i], f)
    }
    Timer {
        interval: 1000; repeat: true; running: true
        onTriggered: {
            probe.tick++
            const win = probe.Window.window ? probe.Window.window.contentItem : null
            if (!win) return
            if (!probe.posted) {
                if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
                if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
                PlanckGUI.views.prompts.drop("StemsDownload")
                Planck.getProperty("/GUI/CloseDialogs").translator.state = true
                probe.walk(win, function(o) {
                    if (o.objectName === "DismissButton" && typeof o.clicked === "function") o.clicked()
                    if (o.objectName === "NextButton" && typeof o.clicked === "function") o.clicked()
                })
            }
            if (probe.tick >= 6 && !probe.posted) {
                let screen = null
                probe.walk(win, function(o) { if (typeof o.scCopyConfirm === "function") screen = o })
                if (!screen) return
                screen.scCopyConfirm({native:["DefaultSpeedRange", "SyncMode"], ui:["waveform", "uiScale"], skippedNative:[], skippedUI:[]}, false, false)
                probe.posted = true
                probe.tick = 6
            }
            if (probe.posted && probe.tick === 9) {
                win.grabToImage(function(result) { result.saveToFile("/tmp/shot.png"); Qt.quit() })
                stop()
            }
        }
    }
}
