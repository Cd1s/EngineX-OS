import QtQuick 2.12
import QtQuick.Window 2.2
import QtQuick.Controls 2.12
import Planck 1.0

Item {
    id: probe
    visible: false

    property string outDir: "/var/tmp/hunt-L2"
    property string modelName: "@MODEL@"
    property string langName: "@LANG@"
    property int state: 0
    property int subtabIdx: 0
    property int waitTicks: 0
    property int ticks: 0
    property var createdMods: []
    property bool busy: false
    property bool actionDone: false
    property var currentSeqObj: null

    function walk(o, seen, f) {
        if (!o || seen.has(o)) return
        seen.add(o)
        f(o)
        if (o.children) {
            for (let i = 0; i < o.children.length; i++) walk(o.children[i], seen, f)
        }
        if (o.contentItem && !seen.has(o.contentItem)) walk(o.contentItem, seen, f)
        if (o.item && !seen.has(o.item)) walk(o.item, seen, f)
    }

    function walkAll(seen, f) {
        let win = probe.Window.window ? probe.Window.window.contentItem : null
        if (win) walk(win, seen, f)
        let ov = probe.Overlay ? probe.Overlay.overlay : null
        if (ov) walk(ov, seen, f)
    }

    function checkClippedText(win) {
        let clipped = []
        walkAll(new Set(), function(o) {
            if (o.visible && typeof o.text === "string" && o.text.length > 0 && typeof o.paintedWidth === "number") {
                if (o.width > 0 && o.paintedWidth > o.width + 3) {
                    if (o.elide === Text.ElideNone && o.wrapMode === Text.NoWrap) {
                        clipped.push((o.objectName || "unnamed") + " text='" + o.text + "' (painted " + Math.round(o.paintedWidth) + " > width " + Math.round(o.width) + ")")
                    }
                }
            }
        })
        if (clipped.length > 0) {
            console.warn("SWEEP_CLIPPED|" + modelName + "|" + langName + "|state=" + state + "|" + clipped.join(" ;; "))
        }
    }

    function capture(win, filename, nextCallback) {
        if (busy) return
        busy = true
        console.warn("SWEEP_START|" + modelName + "/" + langName + "/" + filename)
        checkClippedText(win)
        win.grabToImage(function(result) {
            let path = outDir + "/" + modelName + "/" + langName + "/" + filename
            result.saveToFile(path)
            console.warn("SWEEP_SAVED|" + modelName + "/" + langName + "/" + filename)
            busy = false
            if (nextCallback) nextCallback()
        })
    }

    function splashUnlocked() {
        let sc = PlanckGUI.views ? PlanckGUI.views.splashScreenController : null
        return !!(sc && sc.viewModel && sc.viewModel.isUnlocked)
    }

    Timer {
        id: sweepTimer
        interval: 500
        repeat: true
        running: true

        onTriggered: {
            try {
                if (busy) return
                ticks++
                let win = probe.Window.window ? probe.Window.window.contentItem : null
                if (!win) return

                // Initial dismissals
                if (state < 2) {
                    if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
                    if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
                    if (PlanckGUI.views.prompts && typeof PlanckGUI.views.prompts.drop === "function") {
                        PlanckGUI.views.prompts.drop("StemsDownload")
                    }
                    let pClose = Planck.getProperty("/GUI/CloseDialogs")
                    if (pClose && pClose.translator) pClose.translator.state = true
                    let pWifi = Planck.getProperty("/GUI/ShowWifiView")
                    if (pWifi && pWifi.translator) pWifi.translator.state = false

                    function dismissPopups(o) {
                        if (!o) return
                        if (o.objectName === "DismissButton" && typeof o.clicked === "function") o.clicked()
                        if (o.objectName === "NextButton" && typeof o.clicked === "function") o.clicked()
                        if (o.view && typeof o.view.onDismissed === "function") {
                            if (o.notifier && o.notifier.close) o.notifier.close()
                            o.view.onDismissed()
                        }
                        if (o.children) for (let i = 0; i < o.children.length; i++) dismissPopups(o.children[i])
                    }
                    dismissPopups(win)
                }

                if (waitTicks > 0) {
                    waitTicks--
                    return
                }

                // State 0: Wait for splash unlock and boot settling
                if (state === 0) {
                    if (!splashUnlocked() && ticks < 40) return
                    state = 1
                    actionDone = false
                    waitTicks = 2
                    return
                }

                // State 1: Capture main Deck / Waveform view
                if (state === 1) {
                    capture(win, "01-deck-view.png", function() {
                        state = 2
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 2: Open Control Center tiles
                if (state === 2) {
                    if (!actionDone) {
                        try {
                            PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameControlCenter)
                        } catch (e) {}
                        actionDone = true
                        waitTicks = 2
                        return
                    }
                    capture(win, "02-control-center.png", function() {
                        state = 3
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 3: Open Layout Manager
                if (state === 3) {
                    if (!actionDone) {
                        try {
                            PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameLayout)
                        } catch (e) {}
                        actionDone = true
                        waitTicks = 2
                        return
                    }
                    capture(win, "03-layout-manager.png", function() {
                        state = 4
                        subtabIdx = 0
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 4: Open Settings and iterate sub-tabs
                if (state === 4) {
                    let subtabBar = null
                    walkAll(new Set(), function(o) {
                        if (!subtabBar && o.objectName === "ControlCenter_SubTabBar") subtabBar = o
                    })
                    if (!actionDone) {
                        try {
                            PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                        } catch (e) {}
                        if (subtabBar && subtabBar.count > 0) {
                            if (subtabIdx < subtabBar.count) {
                                subtabBar.currentIndex = subtabIdx
                                actionDone = true
                                waitTicks = 2
                                return
                            } else {
                                // Finished all subtabs!
                                state = 5
                                actionDone = false
                                waitTicks = 1
                                return
                            }
                        }
                        waitTicks = 1
                        return
                    }
                    // Capture current subtab
                    let tabItem = (subtabBar && subtabIdx < subtabBar.count) ? subtabBar.itemAt(subtabIdx) : null
                    let tabName = (tabItem && tabItem.text) ? tabItem.text : ("tab" + subtabIdx)
                    let fname = "04-settings-" + subtabIdx + "-" + tabName.replace(/[^a-zA-Z0-9_-]/g, "_") + ".png"
                    capture(win, fname, function() {
                        subtabIdx++
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 5: In About tab, trigger OTA check
                if (state === 5) {
                    let subtabBar = null
                    walkAll(new Set(), function(o) {
                        if (!subtabBar && o.objectName === "ControlCenter_SubTabBar") subtabBar = o
                    })
                    let aboutObj = null
                    walkAll(new Set(), function(o) {
                        if (!aboutObj && typeof o.scCheckUpdate === "function") aboutObj = o
                    })
                    if (!actionDone) {
                        try {
                            PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                        } catch (e) {}
                        if (!aboutObj && subtabBar && subtabBar.count > 0) {
                            for (let i = 0; i < subtabBar.count; i++) {
                                let item = subtabBar.itemAt(i)
                                let d = (item && item.text) ? item.text : ""
                                if (d.indexOf("About") >= 0 || d.indexOf("关于") >= 0 || d.indexOf("關於") >= 0 || d.indexOf("Acerca") >= 0) {
                                    subtabBar.currentIndex = i
                                    break
                                }
                            }
                            waitTicks = 2
                            return
                        }
                        if (aboutObj) {
                            aboutObj.scCheckUpdate()
                            actionDone = true
                            waitTicks = 3
                            return
                        }
                        waitTicks = 1
                        return
                    }
                    capture(win, "12-dialog-ota.png", function() {
                        state = 6
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 6: Trigger no-source dialog
                if (state === 6) {
                    if (!actionDone) {
                        let messageObject = {
                            id: "StreamingNoSourceAvailableDialog",
                            title: "No source drive available",
                            message: "Beatgrid, cue, loop and waveform data will not be saved. Played state will not be shown. To enable these features, connect a compatible drive.",
                            options: Prompt.Ok
                        }
                        if (PlanckGUI.views.prompts && PlanckGUI.views.prompts.post) {
                            PlanckGUI.views.prompts.post(messageObject)
                        }
                        actionDone = true
                        waitTicks = 2
                        return
                    }
                    capture(win, "13-dialog-no-source.png", function() {
                        if (PlanckGUI.views.prompts && PlanckGUI.views.prompts.drop) {
                            PlanckGUI.views.prompts.drop("StreamingNoSourceAvailableDialog")
                        }
                        state = 7
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 7: Trigger USB preparing banner
                if (state === 7) {
                    let main = null
                    walkAll(new Set(), function(o) {
                        if (!main && "scUsbPreparing" in o) main = o
                    })
                    if (!actionDone) {
                        if (main) {
                            main.scUsbPreparing = true
                            main.scUsbPrepareKind = "usb"
                            main.scUsbPrepareDone = 250
                            main.scUsbPrepareTotal = 1500
                        }
                        actionDone = true
                        waitTicks = 2
                        return
                    }
                    capture(win, "14-dialog-usb-preparing.png", function() {
                        if (main) main.scUsbPreparing = false
                        state = 8
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 8: Trigger Download failed prompt
                if (state === 8) {
                    if (!actionDone) {
                        let messageObject = {
                            id: "DownloadFailed",
                            title: "Download failed",
                            message: "Download failed",
                            options: Prompt.Ok
                        }
                        if (PlanckGUI.views.prompts && PlanckGUI.views.prompts.post) {
                            PlanckGUI.views.prompts.post(messageObject)
                        }
                        actionDone = true
                        waitTicks = 2
                        return
                    }
                    capture(win, "15-dialog-download-failed.png", function() {
                        if (PlanckGUI.views.prompts && PlanckGUI.views.prompts.drop) {
                            PlanckGUI.views.prompts.drop("DownloadFailed")
                        }
                        state = 9
                        actionDone = false
                        waitTicks = 1
                    })
                    return
                }

                // State 9: Mod Sequencer panel
                if (state === 9) {
                    if (!actionDone) {
                        try {
                            PlanckGUI.views.controlCenter.close()
                        } catch (e) {}
                        let seqComp = Qt.createComponent("file:///tmp/Panel.qml")
                        if (seqComp.status === Component.Ready) {
                            currentSeqObj = seqComp.createObject(win, {
                                x: Math.max(0, (win.width - 940) / 2),
                                y: Math.max(0, (win.height - 620) / 2),
                                z: 999999
                            })
                            actionDone = true
                            waitTicks = 2
                            return
                        } else {
                            console.warn("SWEEP_SEQ_STATUS|" + seqComp.status + "|" + seqComp.errorString())
                            state = 10
                            return
                        }
                    }
                    capture(win, "16-mods-sequencer.png", function() {
                        if (currentSeqObj) currentSeqObj.destroy()
                        state = 10
                    })
                    return
                }

                // State 10: Finished!
                if (state === 10) {
                    console.warn("SWEEP_COMPLETE|" + modelName + "|" + langName)
                    sweepTimer.running = false
                    Qt.quit()
                    return
                }
            } catch (err) {
                console.warn("SWEEP_EXCEPTION|" + err)
                sweepTimer.running = false
                Qt.quit()
            }
        }
    }
}
