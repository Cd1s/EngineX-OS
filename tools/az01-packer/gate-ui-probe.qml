// release-gate.sh only, never shipped. The gate copies this over the stock
// qrc:/Skins/Common/Items/QmlWarnings.qml (EmbeddedMain always creates it) in its
// sandbox /data, so Engine's own item tree reports what the shipped pack rendered:
// boot splash unlock/dismissal (#140), Settings -> Services -> Streaming rows,
// the About version line, the Upfader Echo tile, and Control Center's size at UI Scale 75 %.
// Lines are short "SCPROBE kind|field|...|" records: Engine splits long log lines.
import QtQuick 2.12
import QtQuick.Window 2.2
import QtQuick.Controls 2.12
import Planck 1.0

Item {
    id: probe
    visible: false

    function kids(o) {
        const k = []
        if (o.children) for (let i = 0; i < o.children.length; i++) k.push(o.children[i])
        if (o.contentItem && k.indexOf(o.contentItem) < 0) k.push(o.contentItem)
        if (o.item && k.indexOf(o.item) < 0) k.push(o.item)
        return k
    }
    function walk(o, seen, f) {
        if (!o || seen.has(o)) return
        seen.add(o)
        f(o)
        for (const c of kids(o)) walk(c, seen, f)
    }
    function isSection(o) { return typeof o.label === "string" && "labelVisible" in o }
    function entries(section, f) { // the switch rows of a preferences Section
        walk(section, new Set(), function (c) {
            if (c !== section && "accessory" in c && "checked" in c && typeof c.text === "string" && c.text) f(c)
        })
    }
    function streamingSection() { // stock or pack: both list SoundCloud
        let found = null
        walk(probe.Window.window.contentItem, new Set(), function (o) {
            if (!found && isSection(o)) entries(o, function (c) { if (c.text === "SoundCloud") found = o })
        })
        return found
    }
    function splashUnlocked() {
        const sc = PlanckGUI.views ? PlanckGUI.views.splashScreenController : null
        return !!(sc && sc.viewModel && sc.viewModel.isUnlocked)
    }
    function report() {
        let main = null // EmbeddedMain's root, which carries the pack's settings
        let tabView = null
        let splashLoader = null
        walk(probe.Window.window.contentItem, new Set(), function (o) {
            if (!main && "scAutoAnalyze" in o) main = o
            if (!tabView && o.objectName === "ControlCenterTabView") tabView = o
            if (!splashLoader && o.objectName === "SplashScreenLoader") splashLoader = o
            if (o.objectName === "Version")
                console.warn("SCPROBE version|" + String(o.text).replace(/<[^>]*>/g, "") + "|")
            if (o.objectName === "ControlCenter_LiveStream")
                console.warn("SCPROBE tile|" + o.objectName + "|" + (o.visible ? 1 : 0) + "|")
            if (isSection(o))
                entries(o, function (c) { console.warn("SCPROBE entry|" + o.label + "|" + c.text + "|" + (c.visible ? 1 : 0) + "|") })
        })
        const spActive = splashLoader ? (splashLoader.active ? 1 : 0) : 1
        console.warn("SCPROBE splash|" + spActive + "|" + (splashUnlocked() ? 1 : 0) + "|")
        // UI Scale < 100 %: Control Center must follow uiContainer (root.transformed), not
        // defaultScreenSize, or a bare band shows right and below. The Drawer is not in the
        // item tree; its popup item (the ancestor directly under the Overlay) has its size.
        let drawer = tabView
        while (drawer && drawer.parent && drawer.parent !== probe.Overlay.overlay) drawer = drawer.parent
        if (main && drawer && main.transformed && "scUIScale" in main) {
            const was = main.scUIScale
            main.scUIScale = 75 // bindings re-evaluate synchronously; restored right after
            const uw = Math.round(main.transformed.width), uh = Math.round(main.transformed.height)
            const w = Math.round(drawer.width), h = Math.round(drawer.height)
            main.scUIScale = was
            // stock adds 1 px to the height when the screen is rotated 90 degrees
            const ok = uw > 0 && w === uw && (h === uh || h === uh + 1)
            console.warn("SCPROBE ccsize|75|" + w + "x" + h + "|" + uw + "x" + uh + "|" + (ok ? 1 : 0) + "|")
        } else {
            console.warn("SCPROBE ccsize|none|" + !!main + "|" + !!drawer + "|")
        }
        // #55: the preference survives, but automatic submission is unavailable.
        // A native property merely existing does not prove a safe queue adapter.
        console.warn("SCPROBE analyze|" + (main ? typeof main.scAutoAnalyze : "none") + "|" + (main ? main.scAutoAnalyzeAvailable : "none") + "|")
        // #206: with the recorder active, a streaming selection still loads (no record
        // message) and the recorder's streaming block/abort stay off. Restored afterwards.
        const hasRec = !!(Config && Config.audio && Config.audio.hasRecorder)
        const recActive = Planck.getProperty("/Engine/Mixer/Recorder/Active")
        const recBlocked = Planck.getProperty("/Engine/Mixer/Recorder/BlockedByStreaming")
        const recAbort = Planck.getProperty("/Engine/Mixer/Recorder/Abort")
        const arb = Planck.librarianController ? Planck.librarianController.trackLoadArbitrator : null
        if (hasRec && arb && recActive && recActive.translator && recBlocked && recBlocked.translator && recAbort && recAbort.translator) {
            const was = recActive.translator.state
            recActive.translator.state = true
            arb.selectedTrackChanged(true, "Tidal")
            const canLoad = arb.canLoad(3)
            const msg = String(arb.canLoadMessage(1, canLoad)) + String(arb.swipeLeftMessage) + String(arb.swipeRightMessage)
            const res = "SCPROBE recstream|" + (recActive.translator.state ? 1 : 0) + "|" + (canLoad ? 1 : 0) + "|" +
                (/record/i.test(msg) ? 0 : 1) + "|" + (recBlocked.translator.state ? 1 : 0) + "|" + (recAbort.translator.state ? 1 : 0) + "|"
            arb.selectedTrackChanged(false, "")
            recActive.translator.state = was
            console.warn(res)
        } else {
            console.warn("SCPROBE recstream|none|" + (hasRec ? 1 : 0) + "|" + (arb ? 1 : 0) + "|")
        }
    }

    // A whole-tree walk takes 20-35 s under qemu: find the section once, then only poll it.
    property var section: null
    property var tabNames: []
    property int tabTry: 0
    property int phase: 0
    property bool demoConnected: false
    property bool samplerConnected: false
    property int demoWait: 0

    Repeater {
        id: scAvailSources
        model: Planck.librarianController ? Planck.librarianController.browserSourceControllerPtr().availableSourceModel() : null
        delegate: Item { property string rowName: String(model.name || "") }
    }
    Repeater {
        id: scConnSources
        model: Planck.librarianController ? Planck.librarianController.browserSourceControllerPtr().connectedSourceModel() : null
        delegate: Item {
            property string rowName: String(model.name || "")
            property bool isDemo: !!model.isDemoDevice
            property bool isSampler: !!model.isSamplerDemoDevice
        }
    }
    function selectAvailableMatch(re) {
        const bsc = Planck.librarianController.browserSourceControllerPtr()
        const am = bsc.availableSourceModel()
        for (let i = 0; i < scAvailSources.count; i++) {
            const it = scAvailSources.itemAt(i)
            if (it && re.test(it.rowName)) {
                bsc.select(am.index(i, 0))
                return true
            }
        }
        return false
    }

    Timer {
        property int step: 0
        interval: 3000
        repeat: true
        running: true
        onTriggered: {
            try {
                if (probe.phase === 1) {
                    let stockFaderEcho = 0
                    walk(probe.Window.window.contentItem, new Set(), function (o) {
                        if (o.objectName === "ControlCenter_WaveformStyle" || o.objectName === "ControlCenter_UpfaderEcho")
                            console.warn("SCPROBE tile|" + o.objectName + "|" + (o.visible ? 1 : 0) + "|")
                        if (o.objectName === "Preferences_Entry" && o.icon && String(o.icon.source || "").indexOf("Faderecho.png") >= 0 && o.visible && (!o.parent || o.parent.visible))
                            stockFaderEcho = 1
                    })
                    console.warn("SCPROBE tile|ControlCenter_StockFaderEcho|" + stockFaderEcho + "|")
                    Planck.getProperty("/GUI/CloseDialogs").translator.state = true
                    const ssv = PlanckGUI.views.sourceScreenView
                    if (!ssv.visible) ssv.onQuickSourceButtonClicked()
                    if (!ssv.expanded) ssv.onExpandButtonClicked()
                    selectAvailableMatch(/Demo/i)
                    probe.demoWait = 0
                    probe.phase = 2
                    return
                }
                if (probe.phase === 2) {
                    for (let i = 0; i < scConnSources.count; i++) {
                        const it = scConnSources.itemAt(i)
                        if (it && (it.isDemo || /Demo/i.test(it.rowName))) probe.demoConnected = true
                    }
                    if (!probe.demoConnected && ++probe.demoWait <= 5) {
                        selectAvailableMatch(/Demo/i)
                        return
                    }
                    const ssv = PlanckGUI.views.sourceScreenView
                    if (!ssv.visible) ssv.onQuickSourceButtonClicked()
                    if (!ssv.expanded) ssv.onExpandButtonClicked()
                    selectAvailableMatch(/Sample/i)
                    probe.demoWait = 0
                    probe.phase = 3
                    return
                }
                if (probe.phase === 3) {
                    const bsc = Planck.librarianController.browserSourceControllerPtr()
                    const cm = bsc.connectedSourceModel()
                    for (let i = 0; i < scConnSources.count; i++) {
                        const it = scConnSources.itemAt(i)
                        if (it && (it.isSampler || /Sample/i.test(it.rowName))) probe.samplerConnected = true
                    }
                    if (!probe.samplerConnected && ++probe.demoWait <= 5) {
                        selectAvailableMatch(/Sample/i)
                        return
                    }
                    for (let i = scConnSources.count - 1; i >= 0; i--) {
                        const it = scConnSources.itemAt(i)
                        if (it && (it.isDemo || it.isSampler || /Demo|Sample/i.test(it.rowName)))
                            bsc.detach(cm.index(i, 0))
                    }
                    if (PlanckGUI.views.sourceScreenView.visible)
                        PlanckGUI.views.sourceScreenView.close()
                    console.warn("SCPROBE demosource|" + (probe.demoConnected ? 1 : 0) + "|" + (probe.samplerConnected ? 1 : 0) + "|")
                    console.warn("SCPROBE done|")
                    running = false
                    return
                }
                step++
                // #140: wait for the boot splash controller to unlock before opening
                // Control Center, without walking the item tree on every early tick.
                if (!splashUnlocked() && step <= 40) return
                if (!probe.section) {
                    PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                    probe.section = streamingSection()
                    if (!probe.section && step <= 60) return
                    let view = probe.section
                    while (view && typeof view.selectTab !== "function") view = view.parent
                    const names = []
                    for (let i = 0; view && i < view.tabs.count; i++) names.push(view.tabs.get(i).display)
                    probe.tabNames = names
                }
                // The packs translate the sub-tab names selectSubTab() matches on: try each.
                if (probe.section && !probe.section.visible && probe.tabTry < probe.tabNames.length * 2) {
                    const name = probe.tabNames[probe.tabTry++ % probe.tabNames.length]
                    console.warn("SCPROBE nav|" + name + "|")
                    PlanckGUI.views.controlCenter.selectSubTab(name)
                    return
                }
                if (!(probe.section && probe.section.visible)) console.warn("SCPROBE error|Settings -> Services never showed|")
                report()
                probe.phase = 1
                PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameControlCenter)
                return
            } catch (e) {
                console.warn("SCPROBE error|" + e + "|")
                console.warn("SCPROBE done|")
                running = false
            }
        }
    }
}
