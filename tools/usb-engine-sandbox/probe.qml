// tools/usb-engine-sandbox only, never shipped. run.sh serves it in place of the stock
// qrc:/Skins/Common/Items/QmlWarnings.qml (EmbeddedMain always creates it). It waits for the
// sticks run.sh attached (@LABELS@), selects each one, opens the browse view and logs what
// Engine shows: visible texts, the connected-source rows and the librarian state.
// Short lines "FSP <tick>|<what>|...": Engine splits long log lines; read-probe.py joins them.
import QtQuick 2.12
import QtQuick.Window 2.2
import Planck 1.0

Item {
    id: probe
    visible: false
    property int tick: 0
    property int phase: 0
    property int cur: 0
    property var names: []
    property var seenTexts: ({})

    function log(m) { m = String(m).replace(/\s+/g, " "); for (let i = 0; i < m.length; i += 110) console.warn("FSP " + m.substr(i, 110)) }
    function kids(o) {
        const k = []
        if (o.children) for (let i = 0; i < o.children.length; i++) k.push(o.children[i])
        if (o.contentItem && k.indexOf(o.contentItem) < 0) k.push(o.contentItem)
        return k
    }
    function walk(o, seen, f) {
        if (!o || seen.has(o) || o.visible === false) return
        seen.add(o); f(o)
        for (const c of kids(o)) walk(c, seen, f)
    }
    function texts(tag) { // visible texts not logged before (keyboard keys are one character)
        const out = []
        walk(probe.Window.window.contentItem, new Set(), function (o) {
            if (typeof o.text !== "string") return
            const t = o.text.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim()
            if (t.length > 1 && out.indexOf(t) < 0) out.push(t)
        })
        const fresh = out.filter(function (t) { return !probe.seenTexts[tag + t] })
        for (const t of out) probe.seenTexts[tag + t] = true
        log(tag + "|texts(" + out.length + ")|" + fresh.join(" ¦ "))
    }
    function prim(obj, re) {
        const o = []
        if (!obj) return "null"
        for (const k in obj) {
            if (!re.test(k)) continue
            let v; try { v = obj[k] } catch (e) { continue }
            if (typeof v === "function" || (typeof v === "object" && v !== null)) continue
            o.push(k + "=" + v)
        }
        return o.join(",")
    }
    function bsc() { return Planck.librarianController.browserSourceControllerPtr() }
    function prop(p) { const x = Planck.getProperty(p); return x ? x.translator : null }
    function state(tag) {
        const lc = Planck.librarianController
        log(tag + "|lc|" + prim(lc, /rekord|one|collection|craylist|device|library|import/i))
        log(tag + "|dc|" + prim(lc ? lc.devicesController : null, /./))
        try { log(tag + "|rbc|" + prim(lc.rekordBoxImportJobController, /./)) } catch (e) {}
        const c = prop("/Client/Librarian/Collections"); log(tag + "|collections|" + (c ? c.string : "?"))
    }

    Repeater {
        id: scConn
        model: Planck.librarianController ? Planck.librarianController.browserSourceControllerPtr().connectedSourceModel() : null
        delegate: Item { property string rowName: String(model.name || ""); property string type: String(model.providerType || ""); property string info: prim(model, /name|type|identifier|isEnabled|iconText|isOffline|isMassStorage|hasSpinner|subtitle|status/i) }
    }

    Timer {
        interval: 4000
        repeat: true
        running: true
        onTriggered: {
            probe.tick++
            const T = "t" + probe.tick
            try {
                if (probe.phase === 0) {
                    const n = []
                    for (let i = 0; i < scConn.count; i++) { const it = scConn.itemAt(i); if (it && /^(@LABELS@)$/.test(it.rowName)) n.push(it.rowName) }
                    if (probe.tick % 3 === 0) texts(T)
                    if ((n.length >= (+"@NSTICKS@") && probe.tick > 6) || probe.tick > 25) {
                        for (let i = 0; i < scConn.count; i++) { const it = scConn.itemAt(i); if (it) log(T + "|conn" + i + "|" + it.info) }
                        probe.names = n.sort(); probe.phase = 1; log(T + "|sticks|" + probe.names.join(",")); state(T)
                    }
                    return
                }
                if (probe.phase === 1) {
                    if (probe.cur >= probe.names.length) { probe.phase = 9; log(T + "|done|"); return }
                    const name = probe.names[probe.cur]
                    for (let i = 0; i < scConn.count; i++) { const it = scConn.itemAt(i); if (it && it.rowName === name) bsc().select(bsc().connectedSourceModel().index(i, 0)) }
                    probe.phase = 2; probe.sub = 0; return
                }
                if (probe.phase === 2) { // ~40 s per stick: the source view, then the browse view
                    const name = probe.names[probe.cur]
                    probe.sub++
                    if (probe.sub === 3) { if (PlanckGUI.views.sourceScreenView.visible) PlanckGUI.views.sourceScreenView.close(); prop("/GUI/ShowBrowseView").state = true }
                    texts(name + "." + probe.sub)
                    if (probe.sub === 2 || probe.sub === 6) state(name + "." + probe.sub)
                    if (probe.sub >= 10) { prop("/GUI/ShowBrowseView").state = false; probe.cur++; probe.phase = 1 }
                    return
                }
            } catch (e) {
                log(T + "|error|" + e)
            }
        }
    }
    property int sub: 0
}
