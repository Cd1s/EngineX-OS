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
    // The Control Center's channel-strip EQ / Stems switch: one segment per channel, lit while that channel is in Stems.
    function stemsEqTile() {
        const component = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/ControlCenterTab.qml")
        check(component.status === Component.Ready, "Control Center tab loads: " + component.errorString())
        // Laid out as on the Pro+ / Pro Go panel: its device tree names the panel urt,umo-p076md-t (kernel mode 800 x 1280)
        // with rotation 90, so the UI is 1280 x 800 (this sandbox's window is 800 x 800 for every board). The tab is 640 px high
        // (800 less the 80 px tab bar and 80 px status bar) and does not scroll.
        const backdrop = Qt.createQmlObject('import QtQuick 2.12; Rectangle { color: "#1c1c1c" }', probe.Window.window.contentItem)
        backdrop.width = 1280; backdrop.height = 720; backdrop.z = 1000
        const tab = component.createObject(backdrop, {width: 1280, height: 640})
        check(!!tab, "create the Control Center tab")
        const tile = find("ControlCenter_StemsEq", tab)
        check(!!tile && tile.visible, "Stems EQ strip is in the Control Center")
        // Worst case: every stock tile Mixstream can show is visible. The strip must sit in the room above them, cover none of
        // them and not touch the Source / Wi-Fi / Bluetooth row at the bottom.
        let flow = null
        for (let i = 0; i < tab.children.length; i++) if (tab.children[i].numColumns !== undefined) flow = tab.children[i]
        check(!!flow, "found the tile Flow")
        const box = tile.mapToItem(tab, 0, 0)
        function overlaps(item) {
            const r = item.mapToItem(tab, 0, 0)
            return r.x < box.x + tile.width && box.x < r.x + item.width && r.y < box.y + tile.height && box.y < r.y + item.height
        }
        let shown = 0
        for (let i = 0; i < flow.children.length; i++) {
            if (!flow.children[i].visible) continue
            shown++
            check(!overlaps(flow.children[i]), "Stems EQ strip covers no stock tile (child " + i + ")")
        }
        check(shown >= 17, "worst case: 15 stock tiles + Waveform Style + Smart Sync visible, got " + shown)
        const bottomRow = find("ControlCenter_AdditionalControls", tab)
        check(!!bottomRow && !overlaps(bottomRow), "Stems EQ strip leaves the bottom row alone")
        check(box.y >= 0 && box.y + tile.height <= flow.y, "Stems EQ strip lies in the room above the tiles")
        for (let ch = 1; ch <= 2; ch++) {
            const seg = find("ControlCenter_StemsEq_" + ch, tile)
            check(!!seg && seg.width >= 44 && seg.height >= 44, "touch geometry of Stems EQ channel " + ch)
            const own = tr("/Engine/Mixer/Channel" + ch + "/ChannelStrip/Mode")
            const other = tr("/Engine/Mixer/Channel" + (3 - ch) + "/ChannelStrip/Mode")
            const before = other.index
            for (let i = 0; i < seg.children.length; i++) if (typeof seg.children[i].clicked === "function") seg.children[i].clicked(null)
            // Native access filter can reject Stems without a stems track.
            console.warn("X166 STEMSEQ|channel=" + ch + "|mode=" + own.index + "|selected=" + seg.selected + "|")
            check(other.index === before, "Stems EQ tile keeps the other channel " + ch)
            check(seg.selected === (own.index === 1), "Stems EQ highlight follows the actual mode " + ch)
        }
        backdrop.grabToImage(function(r) { check(r.saveToFile("/tmp/mx-control-center.png"), "picture of the Control Center") })
        tile.grabToImage(function(r) { check(r.saveToFile("/tmp/mx-stems-eq-tile.png"), "picture of the Stems EQ tile") }, Qt.size(tile.width * 2, tile.height * 2))
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
                    probe.stemsEqTile()
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
