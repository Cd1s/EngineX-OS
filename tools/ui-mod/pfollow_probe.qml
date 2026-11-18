import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    property int stage: 0
    property real waitUntil: Date.now() + 8000
    property real deadline: 0
    property real probeEnd: 0

    function tr(path) { const p = Planck.getProperty(path); return p ? p.translator : null }
    function log(s) { console.warn("PFOLOW_QML|" + s) }
    function dismiss(o) {
        if (!o) return
        if ((o.objectName === "DismissButton" || o.objectName === "NextButton") && typeof o.clicked === "function") o.clicked()
        for (let i = 0; o.children && i < o.children.length; i++) dismiss(o.children[i])
    }
    function closeFirstRun() {
        const v = PlanckGUI.views
        if (v.onBoardingController) v.onBoardingController.close()
        if (v.prompts && typeof v.prompts.drop === "function") v.prompts.drop("StemsDownload")
        if (v.sourceScreenView && typeof v.sourceScreenView.close === "function") v.sourceScreenView.close()
        for (const path of ["/GUI/ShowWifiView", "/GUI/ShowBrowseView"]) { const t = tr(path); if (t) t.state = false }
        const close = tr("/GUI/CloseDialogs"); if (close) close.state = true
        dismiss(probe.Window.window && probe.Window.window.contentItem)
    }
    function findWaveforms(o, out) {
        if (!o) return
        if (o.nowLinePositionInSamples !== undefined) {
            const d = o.deckIndex !== undefined ? o.deckIndex : (o.parent && o.parent.deckIndex !== undefined ? o.parent.deckIndex : "?")
            out.push(d + "=" + o.nowLinePositionInSamples + "@" + (o.positionItem ? o.positionItem.currentPosition : "null"))
        }
        const kids = o.data || o.children
        for (let i = 0; kids && i < kids.length; i++) findWaveforms(kids[i], out)
    }
    Timer {
        interval: 250; running: true; repeat: true
        onTriggered: {
            if (Date.now() < probe.waitUntil) return
            try { probe.step(Date.now()) } catch (e) { probe.log("ERROR|" + e); running = false }
        }
    }
    function step(now) {
        if (stage === 0) {
            closeFirstRun()
            log("LOADING_PAUSED_SHADOW_DECK2_WITH_LOCAL_DECK1_UNTOUCHED")
            tr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url = "file:///data/music/t2.wav"
            stage = 1; deadline = now + 60000
        } else if (stage === 1) {
            if (!tr("/Engine/Deck2/Track/SongLoaded").state) {
                if (now > deadline) throw new Error("deck 2 did not load")
                return
            }
            closeFirstRun()
            tr("/Engine/Deck2/Play").state = false
            probeEnd = now + 12000
            stage = 2
            log("LOADED|deck1=local-unloaded|deck2=shadow|play2=" + tr("/Engine/Deck2/PlayState").state)
        } else if (stage === 2) {
            const out = []
            findWaveforms(probe.Window.window && probe.Window.window.contentItem, out)
            log("TICK|play2=" + tr("/Engine/Deck2/PlayState").state + "|tpos2=" + tr("/Engine/Deck2/Track/TrackPosition").unnormalized + "|wf=" + out.join(","))
            if (now >= probeEnd) { log("DONE|PASS"); stage = 9 }
        }
    }
}
