// Sandbox only: loaded over QmlWarnings.qml by slot-icon-sandbox.sh (it fills in @BOARD@).
// Shows the shipped deck header (MainView/ArtistSong.qml, from the pack) for a track whose source is Engine's TIDAL slot, and checks which mark
// the header picks and at what size: once per selected service (root.scScope) for a track with no service on record, then for two decks whose
// tracks each name their own service (a fake gateway answers the pack's lookup with the X-Enginex-Provider header). This Engine has no
// streaming track, so the track's source, song and artist are modelled on a copy of the pack's own file; the icon code is the shipped code.
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
    property var pair: null
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
    function build(source, deck, parent, y) {
        let text = fileText("file:///data/enginex/ui/packs/en/Skins/Embedded/Views/MainView/ArtistSong.qml")
        expect(text.length > 1000, "read the pack's ArtistSong.qml (" + text.length + " bytes)")
        for (const [name, value] of [["songName", "A track from the streaming slot"], ["artistName", "An Artist"], ["trackSource", source]]) {
            const re = new RegExp("readonly property string " + name + ": [^\n]*\n")
            expect(re.test(text), "the file reads " + name + " from Engine")
            text = text.replace(re, "property string " + name + ": " + JSON.stringify(value) + "\n")
        }
        const holder = Qt.createQmlObject('import QtQuick 2.15; Rectangle { color: "#1c1c1c"; width: 760; height: 80 }', parent || host, "holder")
        holder.y = y || 0
        const item = Qt.createQmlObject(text, holder, "qrc:/Skins/Embedded/Views/MainView/ArtistSong.qml")
        item.deckIndex = deck || 1
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
    function checkStrip(item, tag, mark, original) {
        const icon = iconOf(item)
        expect(!!icon && icon.visible, tag + ": the header shows a mark")
        if (!icon) return
        const source = decodeURIComponent(String(icon.source))
        expect(source === mark, tag + ": mark " + source)
        expect(!!icon.useOriginal === original, tag + ": " + (original ? "the service's own colours" : "tinted as stock"))
        expect(Math.round(icon.width) === 48 && Math.round(icon.height) === 48, tag + ": 48 x 48 like stock's mark (" + Math.round(icon.width) + " x " + Math.round(icon.height) + ")")
    }
    function check(s) {
        checkStrip(strip.item, s.id, s.mark, s.original)
        shot(s.id, strip.holder)
    }
    // A track loads on a deck: the pack asks the gateway which service it is from (scSwapStemmed, on Engine's TrackUri change) and notes the header
    // of the answer. This Engine has no streaming track to load, so the probe makes the same request and notes the answer the same way;
    // scSwapStemmed itself runs in the Node test with a fake XHR.
    function load(deck, id) {
        const x = new XMLHttpRequest()
        x.open("GET", "http://127.0.0.1:8080/v1/enginex/stems/local?id=" + id)
        x.onreadystatechange = function() {
            if (x.readyState !== 4) return
            const service = root.scServiceOf(x)
            say("deck " + deck + " track " + id + ": HTTP " + x.status + ", service \"" + service + "\"; one header \"" + x.getResponseHeader("X-Enginex-Provider") + "\", lower \"" + x.getResponseHeader("x-enginex-provider") + "\", all " + JSON.stringify(x.getAllResponseHeaders()).slice(0, 90) + ", body " + x.responseText.length + " " + x.responseText.slice(0, 40))
            root.scNoteService(deck, service)
        }
        x.send()
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
        // Two decks, each track from its own service: each header shows its own mark, whatever service is selected.
        const marks = {
            deezer: "file:///data/enginex/ui/icons/deezer.png", beatport: "qrc:/Skins/UIKit/Icons/SourceBeatport LINKColored.png",
            rekordbox: "qrc:/Skins/UIKit/Icons/SourceEngineRemoteLibrary.png", tidal: "qrc:/Skins/UIKit/Icons/SourceTIDAL.png"
        }
        const two = function(tag, mark1, own1, mark2, own2) {
            checkStrip(pair.a.item, tag + " deck 1", mark1, own1)
            checkStrip(pair.b.item, tag + " deck 2", mark2, own2)
            expect(mark1 !== mark2, tag + ": the two decks show two different marks")
            shot(tag, pair.box)
        }
        list.push([900, function() {
            strip.holder.destroy()
            root.scScope = "spotify"
            const box = Qt.createQmlObject('import QtQuick 2.15; Rectangle { color: "#1c1c1c"; width: 760; height: 170 }', host, "box")
            probe.pair = {box: box, a: build("TIDAL", 1, box, 0), b: build("TIDAL", 2, box, 90)}
            load(1, 1); load(2, 3)
        }])
        list.push([1200, function() { two("own-A-spotify-selected", marks.deezer, true, marks.beatport, true) }])
        list.push([900, function() { root.scScope = "tidal" }])
        list.push([900, function() { two("own-B-tidal-selected", marks.deezer, true, marks.beatport, true) }])
        // A rekordbox Link row (computer or CDJ media) is not TIDAL's mark even while TIDAL is selected.
        list.push([900, function() { load(2, 4) }])
        list.push([1200, function() { two("own-C-rekordbox-link", marks.deezer, true, marks.rekordbox, true) }])
        // A track the gateway does not know (real TIDAL's) keeps TIDAL's mark under the TIDAL scope; under another scope it gets the fallback.
        list.push([900, function() { load(1, 99) }])
        list.push([1200, function() {
            checkStrip(pair.a.item, "unknown track under TIDAL", marks.tidal, false)
            checkStrip(pair.b.item, "rekordbox deck unchanged", marks.rekordbox, true)
            shot("own-D-unknown", pair.box)
            root.scScope = "deezer"
        }])
        list.push([900, function() { checkStrip(pair.a.item, "unknown track, Deezer selected: the fallback", marks.deezer, true) }])
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
