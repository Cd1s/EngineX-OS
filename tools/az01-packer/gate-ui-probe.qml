// release-gate.sh only, never shipped. The gate copies this over the stock
// qrc:/Skins/Common/Items/QmlWarnings.qml (EmbeddedMain always creates it) in its
// sandbox /data, so Engine's own item tree reports what the shipped pack rendered:
// Settings -> Services -> Streaming rows, the About version line, the waveform tile.
// Lines are short "SCPROBE kind|field|...|" records: Engine splits long log lines.
import QtQuick 2.12
import QtQuick.Window 2.2

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
    function report() {
        walk(probe.Window.window.contentItem, new Set(), function (o) {
            if (o.objectName === "Version")
                console.warn("SCPROBE version|" + String(o.text).replace(/<[^>]*>/g, "") + "|")
            if (o.objectName === "ControlCenter_WaveformStyle")
                console.warn("SCPROBE tile|" + o.objectName + "|")
            if (typeof o.label === "string" && "labelVisible" in o) // a preferences Section
                walk(o, new Set(), function (c) {
                    if (c !== o && "accessory" in c && "checked" in c && typeof c.text === "string" && c.text)
                        console.warn("SCPROBE entry|" + o.label + "|" + c.text + "|" + (c.visible ? 1 : 0) + "|")
                })
        })
    }

    Timer {
        property int step: 0
        interval: 3000
        repeat: true
        running: true
        onTriggered: {
            try {
                step++
                if (step === 1) PlanckGUI.views.controlCenter.openTab(ControlCenterTabNameSettings)
                if (step === 2) PlanckGUI.views.controlCenter.selectSubTab("Services")
                if (step < 4) return
                report()
            } catch (e) {
                console.warn("SCPROBE error|" + e + "|")
            }
            console.warn("SCPROBE done|")
            running = false
        }
    }
}
