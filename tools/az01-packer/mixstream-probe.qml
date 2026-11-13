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
    function find(name) {
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
        for (const pads of probe.testPads) { const found = walk(pads); if (found) return found }
        return walk(probe.Window.window.contentItem)
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
                        const eq = probe.find("MixstreamStemsEq" + deck)
                        probe.check(!!eq && eq.enabled && eq.height >= 44, "touch EQ / STEMS deck " + deck)
                        const sweep = probe.find("MixstreamSweepFx" + deck)
                        probe.check(!!sweep && sweep.enabled && sweep.height >= 44, "touch SWEEP FX deck " + deck)
                        probe.check(sweep.y + sweep.height <= eq.y, "no overlap 2-row Sweep FX above Stems EQ deck " + deck)
                        const own = probe.tr("/Engine/Mixer/Channel" + deck + "/ChannelStrip/Mode")
                        const other = probe.tr("/Engine/Mixer/Channel" + (3 - deck) + "/ChannelStrip/Mode")
                        const before = other.index
                        eq.clicked()
                        // Native access filter can reject Stems without a stems track.
                        console.warn("X166 EQ|deck=" + deck + "|mode=" + own.index + "|highlight=" + eq.highlighted + "|")
                        probe.check(other.index === before, "EQ switch keeps other channel " + deck)
                        probe.check(eq.highlighted === (own.index === 1), "EQ highlight follows actual mode " + deck)
                    }
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
                        const sweep = probe.find("MixstreamSweepFx" + deck)
                        const eq = probe.find("MixstreamStemsEq" + deck)
                        probe.check(!!sweep && !!eq, "controls found deck " + deck)
                        probe.check(sweep.y === eq.y, "single-row Sweep FX and Stems EQ share same row deck " + deck)
                        probe.check(sweep.x + sweep.width <= eq.x || eq.x + eq.width <= sweep.x,
                                    "no horizontal overlap Sweep FX and Stems EQ deck " + deck)
                    }
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
