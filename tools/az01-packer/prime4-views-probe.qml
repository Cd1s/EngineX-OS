import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0

Item {
    id: p
    property int phase: 0
    property var eMain: null
    property var gcfg: null
    property var tabObj: null
    property int waitTicks: 0

    function log(msg) { console.warn("VIEWSPROBE " + msg) }
    function check(name, ok, detail) {
        log(name + "|" + (ok ? "PASS" : "FAIL") + (detail ? "|" + detail : ""))
    }
    function findMain(o) {
        if (!o) return null
        if ("scViewActive" in o && "scFourDeckRole" in o) return o
        if (o.children) {
            for (let i = 0; i < o.children.length; i++) {
                const r = findMain(o.children[i])
                if (r) return r
            }
        }
        return null
    }
    function findLinkedEntry(o) {
        if (!o) return null
        if (o.text !== undefined && String(o.text) === "Linked View") return o
        if (o.children) {
            for (let i = 0; i < o.children.length; i++) {
                const r = findLinkedEntry(o.children[i])
                if (r) return r
            }
        }
        return null
    }

    Timer {
        interval: 250; running: true; repeat: true
        onTriggered: {
            const c = PlanckGUI.views && PlanckGUI.views.performanceViewController
            if (!c || !c.viewStateController) return
            if (!p.eMain) p.eMain = p.findMain(p.Window.window ? p.Window.window.contentItem : null)
            if (!p.eMain) return

            if (p.phase === 0) {
                if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
                if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
                if (PlanckGUI.views.sourceScreenView) PlanckGUI.views.sourceScreenView.close()
                const pClose = Planck.getProperty("/GUI/CloseDialogs")
                if (pClose && pClose.translator) pClose.translator.state = true
                const pShowBrowse = Planck.getProperty("/GUI/ShowBrowseView")
                if (pShowBrowse && pShowBrowse.translator) pShowBrowse.translator.state = false
                // Wait for real /v1/enginex/ui + /v1/enginex/views polling to finish scUiLoaded & scViewReady
                if (!p.eMain.scUiLoaded || !p.eMain.scViewReady || p.eMain.scVersion !== "5.3.0.5") {
                    p.waitTicks++
                    if (p.waitTicks < 40) return
                    p.check("startup-ready", false, "scUiLoaded=" + p.eMain.scUiLoaded + " scViewReady=" + p.eMain.scViewReady + " ver=" + p.eMain.scVersion)
                    p.phase = 99
                    return
                }
                // 1. Linked View ignored even with fourDeckWaveform=true from mock server
                p.check("linked-view-ignored",
                    p.eMain.scFourDeckRole === "" && p.eMain.scFourDeckOn === false &&
                    !p.eMain.scDeckLocked(1) && !p.eMain.scDeckLocked(2) && !p.eMain.scDeckLocked(3) && !p.eMain.scDeckLocked(4),
                    "role=" + p.eMain.scFourDeckRole + " on=" + p.eMain.scFourDeckOn + " l3=" + p.eMain.scDeckLocked(3))
                // Instantiate LayoutManagerTab to check Linked View entry visibility
                const tc = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/LayoutManagerTab.qml")
                if (tc.status === Component.Ready) {
                    p.tabObj = tc.createObject(p)
                    const le = p.findLinkedEntry(p.tabObj)
                    p.check("linked-view-absent", !!le && le.visible === false, "found=" + !!le + " vis=" + (le ? le.visible : "null"))
                } else {
                    p.check("linked-view-absent", false, tc.errorString())
                }
                // Instantiate GlobalAssignmentConfig in controller module scope
                const gc = Qt.createComponent("qrc:/ControlSurfaceModules/ControlSurfaceModules/GlobalAssignmentConfig.qml")
                if (gc.status !== Component.Ready) {
                    p.check("gcfg-ready", false, gc.errorString())
                    p.phase = 99
                    return
                }
                p.gcfg = gc.createObject(p, {midiChannel: 0})
                // Ensure we start from View 1
                const vsc = c.viewStateController
                p.check("view1-stock",
                    vsc.currentView === 0 && p.eMain.scViewActive === 1 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Horizontal" &&
                    !c.libraryView.visibleSettingValue &&
                    c.deckController.deckCountSetting.currentName === "2 Decks" &&
                    Planck.getProperty("/GUI/PadsViewEnabled").translator.state === true,
                    "cur=" + vsc.currentView + " active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue + " pads=" + Planck.getProperty("/GUI/PadsViewEnabled").translator.state)
                // Trigger SHIFT + VIEW (/GUI/CyclePerformanceView) -> View 2
                Planck.getProperty("/GUI/CyclePerformanceView").translator.trigger()
                p.phase = 1
                return
            }
            const vsc = c.viewStateController
            if (p.phase === 1) {
                p.check("shift-view-2",
                    vsc.currentView === 1 && p.eMain.scViewActive === 2 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Vertical" &&
                    c.libraryView.visibleSettingValue === true,
                    "cur=" + vsc.currentView + " active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue)
                Planck.getProperty("/GUI/CyclePerformanceView").translator.trigger()
                p.phase = 2
                return
            }
            if (p.phase === 2) {
                p.check("shift-view-3",
                    vsc.currentView === 2 && p.eMain.scViewActive === 3 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Horizontal" &&
                    c.libraryView.visibleSettingValue === false &&
                    c.deckController.deckCountSetting.currentName === "4 Decks",
                    "cur=" + vsc.currentView + " active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue + " decks=" + c.deckController.deckCountSetting.currentName)
                Planck.getProperty("/GUI/CyclePerformanceView").translator.trigger()
                p.phase = 3
                return
            }
            if (p.phase === 3) {
                p.check("shift-view-back-1",
                    vsc.currentView === 0 && p.eMain.scViewActive === 1 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Horizontal" &&
                    c.libraryView.visibleSettingValue === false,
                    "cur=" + vsc.currentView + " active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue)
                // Switch to Horizontal + Library visible and check GlobalAssignmentConfig
                c.waveformController.waveformOrientationSetting.setCurrentIndex(0)
                if (!c.libraryView.visibleSettingValue) c.libraryView.performanceLibraryEnableClicked()
                p.phase = 4
                return
            }
            if (p.phase === 4) {
                p.check("load-horiz-lib",
                    p.gcfg.waveformIsForeground === true &&
                    p.gcfg.waveformsAreVertical === false &&
                    p.gcfg.compactBrowserVisible === true &&
                    p.gcfg.canLoad === true,
                    "fg=" + p.gcfg.waveformIsForeground + " vert=" + p.gcfg.waveformsAreVertical + " compact=" + p.gcfg.compactBrowserVisible + " canLoad=" + p.gcfg.canLoad)
                // Hide Library and confirm compactBrowserVisible returns false
                if (c.libraryView.visibleSettingValue) c.libraryView.performanceLibraryEnableClicked()
                p.phase = 5
                return
            }
            if (p.phase === 5) {
                p.check("load-horiz-nolib",
                    p.gcfg.waveformIsForeground === true &&
                    p.gcfg.compactBrowserVisible === false &&
                    p.gcfg.canLoad === false,
                    "fg=" + p.gcfg.waveformIsForeground + " compact=" + p.gcfg.compactBrowserVisible + " canLoad=" + p.gcfg.canLoad)
                // User-saved View 2 priority over stock preset
                p.eMain.scViews = {"2": {waveformOrientation: "Horizontal", library: false, pads: false}}
                Planck.getProperty("/GUI/CyclePerformanceView").translator.trigger()
                p.phase = 6
                return
            }
            if (p.phase === 6) {
                p.check("saved-view-priority",
                    p.eMain.scViewActive === 2 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Horizontal" &&
                    c.libraryView.visibleSettingValue === false &&
                    Planck.getProperty("/GUI/PadsViewEnabled").translator.state === false,
                    "active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue + " pads=" + Planck.getProperty("/GUI/PadsViewEnabled").translator.state)
                // Clear saved View 2, go back to View 1, and verify Layout Manager scViewSelect(2) restores stock View 2
                p.eMain.scViews = {}
                p.eMain.scViewSelect(1)
                p.phase = 7
                return
            }
            if (p.phase === 7) {
                p.eMain.scViewSelect(2)
                p.phase = 8
                return
            }
            if (p.phase === 8) {
                p.check("select-view-2",
                    vsc.currentView === 1 && p.eMain.scViewActive === 2 &&
                    c.waveformController.waveformOrientationSetting.currentName === "Vertical" &&
                    c.libraryView.visibleSettingValue === true,
                    "cur=" + vsc.currentView + " active=" + p.eMain.scViewActive + " orient=" + c.waveformController.waveformOrientationSetting.currentName + " lib=" + c.libraryView.visibleSettingValue)
                p.check("done", true, "")
                p.phase = 99
            }
        }
    }
}
