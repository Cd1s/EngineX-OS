// Sandbox only: loaded over QmlWarnings.qml by browse-icon-sandbox.sh (it fills in @BOARD@, @TAG@ and @EXPECT@).
// Shows the browse view's streaming mark (BrowseView/Components/Components/ProviderIcon.qml, the file the shipped pack serves, or stock's
// when the pack has none) for rows of the TIDAL slot, and checks which mark each row draws and at what size: rows of every service under
// several selected services, rows of real TIDAL (the tidal scope with an account paired), and rows whose mark is not the slot's.
// This Engine has no streaming row, so a row is modelled the way Engine's list model gives it (modelData.providerIconUri and .providerName,
// the gateway's provider label); the icon file, its context and its code are the shipped ones.
// @EXPECT@ is "after" (each row shows its own service) or "before" (every row shows the slot's TIDAL mark, as the base pack does).
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    readonly property string tag: "@TAG@"
    readonly property bool after: "@EXPECT@" === "after"
    readonly property string stock: "qrc:/Skins/UIKit/Icons/SourceTIDAL.png"
    readonly property string iconFile: "qrc:/Skins/Embedded/Views/BrowseView/Components/Components/ProviderIcon.qml"
    property int step: 0
    property real readyAt: 0
    property var host: null
    property var panel: null
    property var rows: []
    property int failures: 0
    readonly property var marks: ({
        deezer: "file:///data/enginex/ui/icons/deezer.png", spotify: "file:///data/enginex/ui/icons/spotify.png",
        applemusic: "file:///data/enginex/ui/icons/applemusic.png", netease: "file:///data/enginex/ui/icons/netease.png",
        soundcloud: "qrc:/Skins/UIKit/Icons/SourceSoundCloudColored.png", beatport: "qrc:/Skins/UIKit/Icons/SourceBeatport LINKColored.png",
        rekordbox: "qrc:/Skins/UIKit/Icons/SourceEngineRemoteLibrary.png"
    })
    // label: what the gateway puts on the row; own: the service whose mark the row must show ("" = the slot's TIDAL mark stays).
    readonly property var scenarios: [
        {id: "A-tidal-scope", scope: "tidal", paired: false, rows: [
            {label: "Deezer [128 BPM 8A]", own: "deezer"}, {label: "Spotify [124 BPM 5B]", own: "spotify"}, {label: "Apple Music", own: "applemusic"},
            {label: "网易云音乐 [140 BPM]", own: "netease"}, {label: "SoundCloud [122 BPM 9A]", own: "soundcloud"}, {label: "Beatport [126 BPM 4A]", own: "beatport"},
            {label: "rekordbox [128.5 BPM 8A]", own: "rekordbox"}, {label: "", own: ""}]},
        {id: "B-spotify-scope", scope: "spotify", paired: false, rows: [
            {label: "Deezer [120 BPM]", own: "deezer"}, {label: "Beatport [120 BPM]", own: "beatport"}, {label: "", own: "spotify"},
            {label: "Dropbox", icon: "qrc:/streaming/Dropbox/Icon.png", own: ""}]},
        {id: "C-real-tidal", scope: "tidal", paired: true, rows: [
            {label: "(P) 2024 Some Label", own: ""}, {label: "Deezer [128 BPM 8A]", own: ""}, {label: "", own: ""}]}
    ]
    function say(text) { console.warn("X166 BROWSE|" + text + "|") }
    function expect(ok, message) {
        if (ok) { say("PASS " + message); return }
        failures++
        console.warn("X166 ERROR|" + message + "|")
    }
    function dismiss() {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false
        if (PlanckGUI.views.onBoardingController) PlanckGUI.views.onBoardingController.close()
        PlanckGUI.views.prompts.drop("StemsDownload")
        Planck.getProperty("/GUI/CloseDialogs").translator.state = true
    }
    // A row the way the track list builds one: `row` is the delegate, modelData its model item, and the mark is loaded by URL, so the
    // pack's file (or stock's) is what the Engine serves.
    function build(parent, y, data) {
        const text = 'import QtQuick 2.15\nItem {\n id: row\n width: 900; height: 56\n readonly property var modelData: ' + JSON.stringify(data) + '\n'
            + ' Rectangle { anchors.fill: parent; color: "#1c1c1c"; Rectangle { width: parent.width; height: 1; anchors.bottom: parent.bottom; color: "#333333" } }\n'
            + ' Text { x: 64; y: 6; text: modelData.song; color: "#e8e8e8"; font.pixelSize: 24 }\n'
            + ' Text { x: 64; y: 32; text: modelData.artist; color: "#9a9a9a"; font.pixelSize: 17 }\n'
            + ' Text { anchors.right: parent.right; anchors.rightMargin: 16; anchors.verticalCenter: parent.verticalCenter; text: modelData.providerName === "" ? "(no label)" : modelData.providerName; color: "#6e8cff"; font.pixelSize: 20 }\n'
            + ' Loader { id: mark; objectName: "mark"; x: 20; anchors.verticalCenter: parent.verticalCenter; source: "' + probe.iconFile + '" }\n}\n'
        const item = Qt.createQmlObject(text, parent, "browse-row")
        item.y = y
        return item
    }
    function markOf(row) {
        let loader = null
        for (let i = 0; i < row.children.length; i++) if (row.children[i].objectName === "mark") loader = row.children[i]
        if (!loader || !loader.item) return null
        const root = loader.item
        for (let i = 0; i < root.children.length; i++) if (root.children[i].source !== undefined) return {holder: root, image: root.children[i]}
        return null
    }
    function show(s) {
        if (panel) panel.destroy()
        root.scScope = s.scope
        root.scTidalLoggedIn = s.paired
        const p = Qt.createQmlObject('import QtQuick 2.15\nRectangle { color: "#101010"; width: 900 }', host, "browse-panel")
        p.height = 56 * s.rows.length
        probe.rows = s.rows.map((r, i) => build(p, 56 * i, {
            song: "Streamed track " + (i + 1), artist: "An Artist", providerName: r.label, providerIconUri: r.icon || probe.stock}))
        probe.panel = p
    }
    function check(s) {
        s.rows.forEach((r, i) => {
            const m = markOf(rows[i])
            const name = s.id + " row " + (i + 1) + " (" + (r.label || "no label") + ")"
            expect(!!m, name + ": the row draws a mark")
            if (!m) return
            const source = decodeURIComponent(String(m.image.source))
            const stockMark = r.icon || probe.stock
            const want = after && r.own ? marks[r.own] : stockMark
            const original = after && r.own !== ""
            say(name + ": source " + source + ", original " + !!m.image.useOriginal + ", " + Math.round(m.image.width) + " x " + Math.round(m.image.height) + ", holder visible " + m.holder.visible)
            expect(source === want, name + ": mark " + source + (after ? "" : " (the base pack: the slot's)"))
            expect(!!m.image.useOriginal === original, name + ": " + (original ? "the service's own colours" : "tinted as stock"))
            expect(m.holder.visible && Math.round(m.image.width) === 24, name + ": visible, 24 px wide like stock's")
        })
        panel.grabToImage(function(r) { say("picture " + s.id + " " + r.saveToFile("/tmp/browse-" + board + "-" + tag + "-" + s.id + ".png")) })
    }
    // The rekordbox import banner's counts (scFormatCount), evaluated by this Engine's own QML engine (the same Qt as on a deck).
    // "after" must give each size once; "before" only records what the base pack gives.
    function counts() {
        const want = [[0, "0"], [5, "5"], [999, "999"], [1000, "1,000"], [6031, "6,031"], [19985, "19,985"], [1234567, "1,234,567"]]
        want.forEach(w => {
            const got = root.scFormatCount(w[0])
            say("count " + w[0] + " -> " + got + (got === w[1] ? "" : " (expected " + w[1] + ")"))
            if (after) expect(got === w[1], "count " + w[0] + " reads " + w[1])
        })
        say("Qt " + Qt.version)
        say("banner " + root.scFormatCount(0) + " / " + root.scFormatCount(6031))
    }
    function stepsFor() {
        const list = [[500, function() {
            dismiss()
            probe.host = Qt.createQmlObject('import QtQuick 2.15; Item { z: 1000 }', probe.Window.window.contentItem, "host")
            probe.host.width = 900; probe.host.height = 700
        }]]
        scenarios.forEach(s => {
            list.push([1100, function() { show(s) }])
            list.push([800, function() { check(s) }])
        })
        list.push([300, function() { counts() }])
        list.push([300, function() {
            say("failures " + failures)
            console.warn("X166 DONE|" + (failures === 0 ? "PASS" : "FAIL") + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }])
        return list
    }
    readonly property var steps: stepsFor()
    Timer {
        interval: 250
        running: true
        repeat: true
        onTriggered: {
            const now = Date.now()
            if (now < probe.readyAt || probe.step >= probe.steps.length) return
            try {
                const entry = probe.steps[probe.step]
                entry[1]()
                probe.readyAt = now + entry[0]
                probe.step++
            } catch (e) {
                console.warn("X166 ERROR|step " + probe.step + ": " + e + "|")
                probe.failures++
                probe.step = probe.steps.length
                Planck.getProperty("/Client/Quit").translator.state = true
                running = false
            }
        }
    }
}
