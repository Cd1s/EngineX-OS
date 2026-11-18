// Sandbox only: loaded over QmlWarnings.qml by slot-icon-sandbox.sh (it fills in @BOARD@).
// Shows the shipped deck header (MainView/ArtistSong.qml, from the pack) for a track whose source is Engine's TIDAL slot, once per active
// service (root.scScope), and checks which mark the header picks and at what size. This Engine has no streaming track, so the track's source,
// song and artist are modelled on a copy of the pack's own file; the icon code is the shipped code.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    readonly property string board: "@BOARD@"
    property int step: 0
    property real readyAt: 0
    property var host: null
    property var strip: null
    property int failures: 0
    readonly property var scopes: [
        {id: "tidal", mark: "qrc:/Skins/UIKit/Icons/SourceTIDAL.png", original: false},
        {id: "deezer", mark: "file:///data/enginex/ui/icons/deezer.png", original: true},
        {id: "spotify", mark: "file:///data/enginex/ui/icons/spotify.png", original: true},
        {id: "applemusic", mark: "file:///data/enginex/ui/icons/applemusic.png", original: true},
        {id: "netease", mark: "file:///data/enginex/ui/icons/netease.png", original: true},
        {id: "soundcloud", mark: "qrc:/Skins/UIKit/Icons/SourceSoundCloudColored.png", original: true},
        {id: "beatport", mark: "qrc:/Skins/UIKit/Icons/SourceBeatport LINKColored.png", original: true},
        {id: "rekordbox", mark: "qrc:/Skins/UIKit/Icons/SourceEngineRemoteLibrary.png", original: true}
    ]
    function say(text) { console.warn("X166 SLOT|" + text + "|") }
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
    function fileText(url) {
        const x = new XMLHttpRequest()
        x.open("GET", url, false)
        x.send()
        return x.responseText
    }
    // The pack's own header with the track's three Engine strings modelled.
    function build(source) {
        let text = fileText("file:///data/enginex/ui/packs/en/Skins/Embedded/Views/MainView/ArtistSong.qml")
        expect(text.length > 1000, "read the pack's ArtistSong.qml (" + text.length + " bytes)")
        for (const [name, value] of [["songName", "A track from the streaming slot"], ["artistName", "An Artist"], ["trackSource", source]]) {
            const re = new RegExp("readonly property string " + name + ": [^\n]*\n")
            expect(re.test(text), "the file reads " + name + " from Engine")
            text = text.replace(re, "property string " + name + ": " + JSON.stringify(value) + "\n")
        }
        const holder = Qt.createQmlObject('import QtQuick 2.15; Rectangle { color: "#1c1c1c"; width: 760; height: 80 }', host, "holder")
        const item = Qt.createQmlObject(text, holder, "qrc:/Skins/Embedded/Views/MainView/ArtistSong.qml")
        item.width = 760; item.height = 80
        return {holder: holder, item: item}
    }
    function iconOf(item) {
        for (let i = 0; i < item.children.length; i++) if (item.children[i].source !== undefined) return item.children[i]
        return null
    }
    function shot(tag, target) {
        target.grabToImage(function(r) { say("picture " + tag + " " + r.saveToFile("/tmp/slot-" + board + "-" + tag + ".png")) })
    }
    function check(s) {
        const icon = iconOf(strip.item)
        expect(!!icon && icon.visible, s.id + ": the header shows a mark")
        if (!icon) return
        const source = decodeURIComponent(String(icon.source))
        expect(source === s.mark, s.id + ": mark " + source)
        expect(!!icon.useOriginal === s.original, s.id + ": " + (s.original ? "the service's own colours" : "tinted as stock"))
        expect(Math.round(icon.width) === 48 && Math.round(icon.height) === 48, s.id + ": 48 x 48 like stock's mark (" + Math.round(icon.width) + " x " + Math.round(icon.height) + ")")
        shot(s.id, strip.holder)
    }
    function stepsFor() {
        const list = [[500, function() {
            dismiss()
            probe.host = Qt.createQmlObject('import QtQuick 2.15; Item { z: 1000 }', probe.Window.window.contentItem, "host")
            probe.host.width = 800; probe.host.height = 900
        }]]
        scopes.forEach(s => {
            list.push([900, function() {
                if (strip) strip.holder.destroy()
                root.scScope = s.id
                probe.strip = build("TIDAL")
            }])
            list.push([700, function() { check(s) }])
        })
        // A track that is not from the slot keeps stock's own mark whatever the service, and a local track has none.
        list.push([900, function() { strip.holder.destroy(); root.scScope = "deezer"; probe.strip = build("SoundCloud") }])
        list.push([700, function() {
            const icon = iconOf(strip.item)
            expect(!!icon && decodeURIComponent(String(icon.source)).indexOf("SourceSoundCloud.png") >= 0 && !icon.useOriginal, "SoundCloud source: stock's own mark stays")
        }])
        list.push([900, function() { strip.holder.destroy(); probe.strip = build("") }])
        list.push([700, function() {
            const icon = iconOf(strip.item)
            expect(!!icon && !icon.visible, "local track: no mark")
        }])
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
