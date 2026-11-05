// release-gate.sh only, never shipped. The gate copies this over the stock
// qrc:/Skins/Common/Items/QmlWarnings.qml (EmbeddedMain always creates it) in its
// sandbox /data, so Engine's own item tree reports what the shipped pack rendered:
// Settings -> Services -> Streaming rows, the About version line, the waveform tile.
// Lines are short "SCPROBE kind|field|...|" records: Engine splits long log lines.
import QtQuick 2.12
import QtQuick.Window 2.2
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
    function report() {
        let main = null // EmbeddedMain's root, which carries the pack's settings
        walk(probe.Window.window.contentItem, new Set(), function (o) {
            if (!main && "scAutoAnalyze" in o) main = o
            if (o.objectName === "Version")
                console.warn("SCPROBE version|" + String(o.text).replace(/<[^>]*>/g, "") + "|")
            if (o.objectName === "ControlCenter_WaveformStyle" || o.objectName === "ControlCenter_LiveStream" || o.objectName === "ControlCenter_UpfaderEcho")
                console.warn("SCPROBE tile|" + o.objectName + "|")
            if (isSection(o))
                entries(o, function (c) { console.warn("SCPROBE entry|" + o.label + "|" + c.text + "|" + (c.visible ? 1 : 0) + "|") })
        })
        // #55: the pack's toggle, and the Engine preference it drives.
        const aa = Planck.getProperty("/Client/Preferences/AutoAnalysis")
        console.warn("SCPROBE analyze|" + (main ? typeof main.scAutoAnalyze : "none") + "|" + (aa && aa.translator ? 1 : 0) + "|")
    }

    // A whole-tree walk takes 20-35 s under qemu: find the section once, then only poll it.
    property var section: null
    property var tabNames: []
    property int tabTry: 0

    Timer {
        property int step: 0
        interval: 3000
        repeat: true
        running: true
        onTriggered: {
            try {
                step++
                if (!probe.section) {
                    PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                    probe.section = streamingSection()
                    if (!probe.section && step <= 20) return
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
            } catch (e) {
                console.warn("SCPROBE error|" + e + "|")
            }
            console.warn("SCPROBE done|")
            running = false
        }
    }
}
