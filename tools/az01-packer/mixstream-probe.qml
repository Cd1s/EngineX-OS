// Sandbox only: loaded over QmlWarnings.qml by mixstream-sandbox.sh.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    property int stage: 0
    property int effect: 0
    property real readyAt: 0
    property var testPads: []
    // Verified against the native popup's current index, rather than the
    // analysis report's unverified enum/name table.
    readonly property var types: [1, 2, 3, 4, 5]
    readonly property var names: ["Filter", "Wash", "Dub Echo", "Filter Roll", "Noise"]
    function check(ok, message) {
        if (!ok) throw new Error(message)
        console.warn("X166 PASS|" + message + "|")
    }
    function tr(path) {
        const p = Planck.getProperty(path)
        check(!!p && !!p.translator, path)
        return p.translator
    }
    function find(name, from) {
        const seen = new Set()
        function walk(o) {
            if (!o || seen.has(o)) return null
            seen.add(o)
            if (o.objectName === name) return o
            const children = []
            if (o.children) for (let i = 0; i < o.children.length; i++) children.push(o.children[i])
            if (o.contentItem) children.push(o.contentItem)
            if (o.item) children.push(o.item)
            for (const c of children) { const found = walk(c); if (found) return found }
            return null
        }
        if (from) return walk(from)
        for (const pads of probe.testPads) { const found = walk(pads); if (found) return found }
        return walk(probe.Window.window.contentItem)
    }
    // Pad bar at its natural height, half the 1024 px screen wide: measured and pictured as shipped.
    // The bars in testPads are stretched to 150 px to check touch targets.
    function shoot(tag) {
        const component = Qt.createComponent("qrc:/Skins/Embedded/Items/Pads/Pads.qml")
        for (let deck = 1; deck <= 2; deck++) {
            const pads = component.createObject(probe.Window.window.contentItem, {deckIndex: deck, width: 508, x: (deck - 1) * 512, y: 100, z: 1000})
            const grid = probe.find("Deck_Pads_Layout", pads)
            console.warn("X166 PADS|" + tag + "|deck=" + deck + "|height=" + pads.implicitHeight + "|padGridWidth=" + Math.round(grid.width) + "|")
            pads.grabToImage(function(r) {
                probe.check(r.saveToFile("/tmp/mx-pads-" + tag + "-deck" + deck + ".png"), "picture of the " + tag + " Pad bar deck " + deck)
                pads.destroy()
            }, Qt.size(pads.width * 2, pads.implicitHeight * 2))
        }
    }
    function all(name, from) {
        const found = [], seen = new Set()
        function walk(o) {
            if (!o || seen.has(o)) return
            seen.add(o)
            if (o.objectName === name) found.push(o)
            if (o.children) for (let i = 0; i < o.children.length; i++) walk(o.children[i])
            if (o.contentItem) walk(o.contentItem)
        }
        walk(from)
        return found
    }
    // Settings -> Playback: the channel strip's EQ / Stems switch, one DECK segment per channel, lit while that channel is in Stems.
    function stemsEqRow() {
        const component = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/PlaybackTabContent.qml")
        check(component.status === Component.Ready, "Playback settings load: " + component.errorString())
        const page = component.createObject(probe.Window.window.contentItem, {width: 1280, height: 640, z: 1000})
        check(!!page, "create the Playback settings")
        const row = find("EngineX_StemsEqDecks", page)
        check(!!row && row.visible, "Stems EQ row is in Settings -> Playback")
        const segments = all("ListEntryButton", row)
        check(segments.length === 2, "one DECK segment per channel")
        // Without a Stems track the native access filter rejects mode 1. Test the
        // shipped row's bindings and click handler with signal-emitting translators.
        for (let ch = 0; ch < 2; ch++)
            row.modes[ch] = Qt.createQmlObject('import QtQml; QtObject { property int index: 0 }', page)
        row.values = row.values.slice() // refresh the binding's dependencies on the test translators
        // The same kind of row as stock builds it, for the picture and the geometry to be compared with.
        const stockButton = all("ListEntryButton", page).filter(b => segments.indexOf(b) < 0)[0]
        let stockRow = stockButton
        while (stockRow && stockRow.values === undefined) stockRow = stockRow.parent
        if (stockRow) stockRow.grabToImage(function(r) { check(r.saveToFile("/tmp/mx-stock-playback-row.png"), "picture of a stock Playback row") }, Qt.size(stockRow.width * 2, stockRow.height * 2))
        for (let ch = 1; ch <= 2; ch++) {
            const seg = segments[ch - 1]
            // Stock's own row buttons: same width as the entry says, same height as the buttons of the other rows.
            console.warn("X166 STEMSEQ_GEOM|channel=" + ch + "|" + seg.width + "x" + seg.height + "|stock=" + (stockButton ? stockButton.width + "x" + stockButton.height : "none") + "|")
            check(!!stockButton && seg.width === row.entryWidth && seg.height === stockButton.height, "Stems EQ channel " + ch + " is a stock-sized row button")
            const own = row.modes[ch - 1]
            const other = row.modes[2 - ch]
            const before = other.index
            check(own.index === 0 && !seg.down, "Stems EQ starts off " + ch)
            seg.clicked()
            check(own.index === 1 && seg.down, "Stems EQ click turns on and highlights " + ch)
            check(other.index === before, "Stems EQ row keeps the other channel " + ch)
            seg.clicked()
            check(own.index === 0 && !seg.down, "Stems EQ click turns off and clears highlight " + ch)
            check(other.index === before, "Stems EQ row keeps the other channel " + ch)
            console.warn("X166 STEMSEQ|channel=" + ch + "|mock-mode-sequence=0,1,0|PASS|")
        }
        row.grabToImage(function(r) { check(r.saveToFile("/tmp/mx-stems-eq-row.png"), "picture of the Stems EQ row") }, Qt.size(row.width * 2, row.height * 2))
    }
    Timer {
        interval: 1500
        running: true
        repeat: true
        onTriggered: {
            if (Date.now() < probe.readyAt) return
            probe.readyAt = Date.now() + 500
            try {
                if (probe.stage === 0) {
                    console.warn("X166 CONFIG|" + Config.device.name + "|beatJump=" + Config.audio.deck.hasBeatJump
                                 + "|NH=" + Config.audio.fx.hasNHFx + "|noise=" + Config.audio.fx.sweepFx.hasNoiseSweep
                                 + "|roll=" + Config.audio.fx.sweepFx.hasFilterRoll + "|popup=" + Config.ui.hasSweepFxSelectionPopup + "|")
                    probe.check(Config.audio.fx.hasNHFx, "Main FX retains hasNHFx")
                    probe.check(Config.audio.deck.hasBeatJump, "Config.audio.deck.hasBeatJump")
                    probe.check(Config.audio.fx.sweepFx.hasNoiseSweep, "Config.audio.fx.sweepFx.hasNoiseSweep")
                    probe.check(Config.audio.fx.sweepFx.hasFilterRoll, "Config.audio.fx.sweepFx.hasFilterRoll")
                    probe.check(Config.ui.hasSweepFxSelectionPopup, "Config.ui.hasSweepFxSelectionPopup")
                    for (let deck = 1; deck <= 2; deck++) {
                        for (const a of ["JumpBack", "JumpForward", "BeatJumpHalve", "BeatJumpDouble", "BeatJumpIndex"])
                            probe.tr("/Engine/Deck" + deck + "/Track/BeatJump/" + a)
                        probe.tr("/Engine/Mixer/Channel" + deck + "/ChannelStrip/Mode")
                    }
                    root.scBeatJumpPads = true
                    probe.tr("/GUI/PadsViewEnabled").state = true
                    // Fresh /data may show onboarding. Exercise the actual
                    // shipped Pads at half-screen width without saved UI state.
                    const component = Qt.createComponent("qrc:/Skins/Embedded/Items/Pads/Pads.qml")
                    probe.check(component.status === Component.Ready, "Pads component loads: " + component.errorString())
                    for (let deck = 1; deck <= 2; deck++) {
                        const pads = component.createObject(probe.Window.window.contentItem,
                            {deckIndex: deck, width: 508, height: 150, x: (deck - 1) * 512, y: 300, z: 1000})
                        probe.check(!!pads, "create shipped Pads deck " + deck)
                        probe.testPads.push(pads)
                    }
                    probe.stage++
                } else if (probe.stage === 1) {
                    for (let deck = 1; deck <= 2; deck++) {
                        const bank = probe.find("MixstreamBeatJumpBank" + deck)
                        probe.check(!!bank && bank.visible, "visible touch Beat Jump bank " + deck)
                        const padsLayout = probe.find("Deck_Pads_Layout")
                        probe.check(!!padsLayout && padsLayout.visible, "visible native pads alongside Beat Jump " + deck)
                        for (const a of ["JumpBack", "JumpForward", "BeatJumpHalve", "BeatJumpDouble"]) {
                            const button = probe.find("Mixstream" + a + deck)
                            probe.check(!!button && button.enabled && button.width >= 44 && button.height >= 44,
                                        "touch action and geometry " + a + " deck " + deck)
                            button.clicked()
                        }
                        const sweep = probe.find("MixstreamSweepFx" + deck)
                        probe.check(!!sweep && sweep.enabled && sweep.width >= 44 && sweep.height >= 44, "touch Sweep FX triangle deck " + deck)
                        probe.check(!probe.find("MixstreamStemsEq" + deck, probe.testPads[deck - 1]), "no EQ / STEMS button on the Pad bar deck " + deck)
                    }
                    probe.shoot("beat-jump-bank")
                    probe.stemsEqRow()
                    probe.find("MixstreamSweepFx1").clicked()
                    probe.stage++
                } else if (probe.stage === 2) {
                    const wheel = probe.find("SweepFx_Select_Carousel")
                    probe.check(!!wheel && wheel.visible && wheel.hasCloseButton, "native Sweep FX carousel opens from deck 1")
                    console.warn("X166 CAROUSEL|" + JSON.stringify(PlanckGUI.views.sweepFxSettings.popup.entries) + "|")
                    for (const name of probe.names)
                        probe.check(PlanckGUI.views.sweepFxSettings.popup.entries.indexOf(name) >= 0, "carousel entry " + name)
                    wheel.rejected()
                    probe.check(!root.scMixstreamSweepOpen, "carousel dismiss resets touch request")
                    probe.find("MixstreamSweepFx2").clicked()
                    probe.stage++
                } else if (probe.stage === 3) {
                    const wheel = probe.find("SweepFx_Select_Carousel")
                    probe.check(!!wheel && wheel.visible, "native carousel reopens from deck 2")
                    const index = PlanckGUI.views.sweepFxSettings.popup.entries.indexOf("Dub Echo")
                    wheel.accepted(index)
                    probe.check(!root.scMixstreamSweepOpen, "native carousel accepts touch selection")
                    probe.stage++
                } else if (probe.stage === 4) {
                    for (let deck = 1; deck <= 2; deck++) {
                        const raw = probe.tr("/Engine/Mixer/Channel" + deck + "/SweepFx/Type").unnormalized
                        console.warn("X166 SELECTED|channel=" + deck + "|type=" + raw + "|")
                        probe.check(raw === 3, "native selection applies Dub Echo to channel " + deck)
                    }
                    probe.tr("/Engine/Mixer/Channel1/SweepFx/Type").unnormalized = probe.types[0]
                    probe.stage++
                } else if (probe.stage === 5) {
                    const type = probe.types[probe.effect]
                    const raw = probe.tr("/Engine/Mixer/Channel1/SweepFx/Type").unnormalized
                    const popup = PlanckGUI.views.sweepFxSettings.popup
                    const name = String(popup.entries[popup.current]).trim()
                    console.warn("X166 SWEEP|" + type + "|" + name + "|")
                    probe.check(raw === type && name === probe.names[probe.effect], "Sweep Type " + type + " = " + name)
                    probe.tr("/Engine/Mixer/Channel1/SweepFx/Value").value = 0.8
                    probe.effect++
                    if (probe.effect < probe.types.length) {
                        probe.tr("/Engine/Mixer/Channel1/SweepFx/Type").unnormalized = probe.types[probe.effect]
                    } else {
                        root.scBeatJumpPads = false
                        probe.stage++
                    }
                } else {
                    for (let deck = 1; deck <= 2; deck++) {
                        const bank = probe.find("MixstreamBeatJumpBank" + deck)
                        probe.check(!!bank && !bank.visible, "Beat Jump preference off hides Beat Jump bank " + deck)
                        const padsLayout = probe.find("Deck_Pads_Layout")
                        probe.check(!!padsLayout && padsLayout.visible, "native pads remain visible deck " + deck)
                        probe.check(!!probe.find("MixstreamSweepFx" + deck) && !probe.find("MixstreamStemsEq" + deck, probe.testPads[deck - 1]), "only the Sweep FX triangle beside the pads deck " + deck)
                    }
                    probe.shoot("default")
                    console.warn("X166 DONE|PASS|")
                    running = false
                }
            } catch (e) {
                console.warn("X166 ERROR|" + e + "|")
                running = false
            }
            probe.readyAt = Date.now() + 500
        }
    }
}
