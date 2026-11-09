import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0

Item {
    id: probe
    visible: false

    function log(msg) {
        console.warn("PRIME4_PROBE " + msg)
    }

    Timer {
        id: runTimer
        interval: 1000
        repeat: true
        running: true
        property int tick: 0

        onTriggered: {
            tick++
            try {
                // Dismiss OutOfBox / OnBoarding wizard if showing
                if (PlanckGUI.views.outOfBoxUpdateScreenShowing) {
                    PlanckGUI.views.outOfBoxUpdateScreenShowing = false
                }
                if (PlanckGUI.views.onBoardingController) {
                    PlanckGUI.views.onBoardingController.close()
                }
                if (PlanckGUI.views.prompts) {
                    PlanckGUI.views.prompts.drop("StemsDownload")
                }

                // Close Source Screen if visible
                if (PlanckGUI.views.sourceScreenView) {
                    PlanckGUI.views.sourceScreenView.close()
                }
                let pClose = Planck.getProperty("/GUI/CloseDialogs")
                if (pClose && pClose.translator) {
                    pClose.translator.state = true
                }

                // Dismiss any NewFeaturePopup (like Stem Separation promo)
                let win = probe.Window.window ? probe.Window.window.contentItem : null
                function dismissAll(o) {
                    if (!o) return
                    if (o.objectName === "DismissButton") {
                        if (o.clicked) o.clicked()
                    }
                    if (o.view && typeof o.view.onDismissed === "function") {
                        if (o.notifier && o.notifier.close) o.notifier.close()
                        o.view.onDismissed()
                    }
                    if (o.children) {
                        for (let i = 0; i < o.children.length; i++) dismissAll(o.children[i])
                    }
                }
                dismissAll(win)

                // Switch from Browse/Library view to Waveform Performance view
                let pShowBrowse = Planck.getProperty("/GUI/ShowBrowseView")
                if (pShowBrowse && pShowBrowse.translator) {
                    pShowBrowse.translator.state = false
                }

                // Switch to 4-Deck / 4-Waveform layout
                let ctrl = PlanckGUI.views.performanceViewController
                if (ctrl) {
                    if (ctrl.deckController && ctrl.deckController.deckCountSetting) {
                        let dc = ctrl.deckController.deckCountSetting
                        log("deckCountSetting names=" + JSON.stringify(dc.settingNames) + " current=" + dc.currentName)
                        let idx = dc.settingNames.indexOf("4 Decks")
                        if (idx >= 0) dc.setCurrentIndex(idx)
                        else if (dc.settingNames.length > 1) dc.setCurrentIndex(1)
                    }
                    if (ctrl.waveformController && ctrl.waveformController.waveformCountSetting) {
                        let wc = ctrl.waveformController.waveformCountSetting
                        log("waveformCountSetting names=" + JSON.stringify(wc.settingNames) + " current=" + wc.currentName)
                        let idx = wc.settingNames.indexOf("4 Waveforms")
                        if (idx >= 0) wc.setCurrentIndex(idx)
                        else if (wc.settingNames.length > 1) wc.setCurrentIndex(1)
                    }
                }

                // Step 1: Ensure Channel 3 is in Line In mode on PRIME 4
                let pLine3 = Planck.getProperty("/Engine/Mixer/Channel3/Line")
                if (pLine3 && pLine3.translator) {
                    pLine3.translator.state = true
                }

                // Check main root
                let main = probe.Window.window ? probe.Window.window.contentItem : null
                if (!main) return

                let eMain = null
                function findMain(o) {
                    if (!o || eMain) return
                    if ("scFourDeckRole" in o && "scShadowPoll" in o) {
                        eMain = o
                        return
                    }
                    if (o.children) {
                        for (let i = 0; i < o.children.length; i++) findMain(o.children[i])
                    }
                }
                findMain(main)

                if (eMain) {
                    if (tick === 1) {
                        eMain.scFourDeckRole = "12"
                        eMain.scFourDeckOn = true
                        eMain.scFourDeckRoleSetting = "12"
                    }
                    log("tick=" + tick + " role=" + eMain.scFourDeckRole + " on=" + eMain.scFourDeckOn + " mine=" + JSON.stringify(eMain.scShadowMine))
                    // Trigger shadow poll
                    eMain.scShadowPoll()

                    let s3 = eMain.scShadowData(3)
                    let v3 = eMain.scShadowVisible(3)
                    let l3 = eMain.scDeckLocked(3)
                    log("tick=" + tick + " s3=" + (s3 ? s3.key : "null") + " v3=" + v3 + " l3=" + l3)

                    // Check visual items in scene graph
                    let wave3Found = false
                    let deckCover3Visible = true
                    function inspect(o) {
                        if (!o) return
                        if (o.objectName === "EngineX_Shadow_waveform" && o.deck === 3) {
                            wave3Found = true
                        }
                        if (o.id === "deckCover" || (o.objectName && o.objectName.indexOf("deckCover") >= 0)) {
                            // Check parent deck
                            if (o.parent && o.parent.deckIndex === 3) {
                                deckCover3Visible = o.visible
                            }
                        }
                        if (o.children) {
                            for (let i = 0; i < o.children.length; i++) inspect(o.children[i])
                        }
                    }
                    inspect(main)

                    log("wave3Found=" + wave3Found + " deckCover3=" + deckCover3Visible)

                    // At tick 5, capture the screenshot
                    if (tick >= 5) {
                        runTimer.running = false
                        main.grabToImage(function(result) {
                            let ok = result.saveToFile("/tmp/prime4-cdj-4deck.png")
                            log("SCREENSHOT_SAVED|" + ok)
                            Qt.quit()
                        })
                    }
                }
            } catch (err) {
                log("ERROR: " + err)
            }
        }
    }
}
