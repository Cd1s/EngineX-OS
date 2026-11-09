// Sandbox only, replaces QmlWarnings.qml. Uses the shipped native input selectors.
import QtQuick
import QtQuick.Window
import Planck
Item {
    id: probe
    visible: false
    property int stage: 0
    property var sections: []
    property var line: []
    property var selectors: []
    function check(ok, message) {
        if (!ok) throw new Error(message)
        console.warn("X171 PASS|" + message + "|")
    }
    function tr(path) {
        const p = Planck.getProperty(path)
        probe.check(!!p && !!p.translator, path)
        return p.translator
    }
    function button(channel) {
        for (const child of probe.selectors[channel - 1].children)
            if (child.objectName === "InputSelect_DropDownButton") return child
        throw new Error("native InputSelect button missing")
    }
    function select(channel, label) {
        const section = probe.sections[channel - 1]
        const nativeIndex = Array.from(section.inputs).indexOf(label)
        const index = Array.from(probe.selectors[channel - 1].scInputIndexes).indexOf(nativeIndex)
        probe.check(index >= 0, "visible native input choice " + label + " channel " + channel)
        probe.button(channel).optionSelected(index)
    }

    Timer {
        interval: 1200; running: true; repeat: true
        onTriggered: {
            try {
                if (probe.stage === 0) {
                    console.warn("X171 CONFIG|" + Config.device.name + "|software=" + Config.audio.hasSoftwareLineIn + "|menu=" + Config.ui.hasMixerMenu + "|")
                    probe.check(Config.device.name === "JP21", "JP21 stock Engine")
                    probe.check(Config.audio.hasSoftwareLineIn, "Config.audio.hasSoftwareLineIn")
                    probe.check(Config.ui.hasMixerMenu, "Config.ui.hasMixerMenu")
                    for (const channel of [3, 4]) {
                        const base = "/Engine/Mixer/Channel" + channel + "/Line"
                        const a = Planck.getProperty(base)
                        const b = Planck.getProperty(base + "/Active")
                        console.warn("X171 PROPERTY|" + base + "|base=" + (!!a && !!a.translator) + "|active=" + (!!b && !!b.translator) + "|")
                        probe.line.push(probe.tr(base))
                    }
                    const menu = PlanckGUI.views.mixerMenu
                    probe.check(!!menu, "native Mixer Menu controller")
                    for (let i = 0; i < menu.pages.length; i++) {
                        const page = menu.pages[i]
                        console.warn("X171 PAGE|" + page.type + "|" + page.title + "|")
                        if (page.type === "Channels") {
                            menu.tabIndex = i
                            for (const section of page.sections) {
                                const number = Number(String(section.title).match(/[1-4]/)[0])
                                probe.sections[number - 1] = section
                            }
                        }
                    }
                    probe.check(probe.sections.length === 4, "native four channel input sections")
                    menu.show()
                    const component = Qt.createComponent("qrc:/Skins/Embedded/Views/MixerMenu/Pages/ChannelsPageItems/InputSelect.qml")
                    probe.check(component.status === Component.Ready, "shipped InputSelect QML loads: " + component.errorString())
                    for (let channel = 1; channel <= 4; channel++) {
                        const control = component.createObject(probe.Window.window.contentItem, {controller: probe.sections[channel - 1], visible: false})
                        probe.check(!!control, "create shipped input selector " + channel)
                        probe.selectors.push(control)
                        const names = Array.from(control.scInputIndexes).map(i => probe.sections[channel - 1].inputs[i])
                        const expectedInputs = channel < 3 ? ["Engine"] : ["Engine", "Line In"]
                        probe.check(JSON.stringify(names) === JSON.stringify(expectedInputs), "JP21 only verified inputs channel " + channel)
                        for (let i = 0; i < probe.button(channel).options.count; i++)
                            probe.check(probe.button(channel).options.get(i).optionName === root.scT(names[i]), "visible input label/index " + channel + ":" + i)
                    }
                    for (let i = 0; i < 4; i++) console.warn("X171 INPUTS|" + (i + 1) + "|" + JSON.stringify(Array.from(probe.sections[i].inputs)) + "|")
                    probe.stage++
                } else if (probe.stage === 1) {
                    probe.check(!probe.line[0].state && !probe.line[1].state, "fresh LineIn starts off")
                    const input = probe.tr("/Engine/Mixer/AuxMic2/InputSelect")
                    console.warn("X171 SELECTOR|index=" + input.index + "|" + input.string + "|")
                    input.index = 1
                    probe.select(3, "Line In")
                    probe.stage++
                } else if (probe.stage === 2) {
                    probe.check(probe.line[0].state && !probe.line[1].state, "native selector enables Channel3 LineIn")
                    console.warn("X171 LABEL|" + root.scLang + "|actual=" + probe.button(3).labelText + "|native=" + probe.sections[2].currentInput + "|expected=" + root.scT("Line In") + "|")
                    probe.check(probe.button(3).labelText === root.scT("Line In"), "translated native Line In label")
                    probe.tr("/Engine/Mixer/AuxMic2/Active").state = true
                    probe.stage++
                } else if (probe.stage === 3) {
                    probe.check(probe.tr("/Engine/Mixer/AuxMic2/Active").state && probe.line[0].state, "ON does not deselect Line In")
                    probe.select(4, "Line In")
                    probe.stage++
                } else if (probe.stage === 4) {
                    probe.check(probe.line[0].state && probe.line[1].state, "both native Line switches can be active")
                    probe.select(3, "Engine")
                    probe.stage++
                } else if (probe.stage === 5) {
                    probe.check(!probe.line[0].state && probe.line[1].state, "Engine closes empty Channel3; Channel4 remains Line In")
                    probe.tr("/Engine/Mixer/AuxMic2/InputSelect").index = 0
                    probe.stage++
                } else if (probe.stage === 6) {
                    probe.check(probe.tr("/Engine/Mixer/AuxMic2/InputSelect").index === 0, "native selector changes to Mic2")
                    probe.select(4, "Engine")
                    probe.stage++
                } else if (probe.stage === 7) {
                    probe.check(!probe.line[0].state && !probe.line[1].state, "both Line switches return to Engine")
                    probe.check(probe.tr("/Engine/Mixer/AuxMic2/Active").state, "original ON state retained for stock routing")
                    probe.tr("/Engine/Mixer/AuxMic2/Active").state = false
                    const expected = root.scLang === "zh-Hans" ? "输入" : root.scLang === "zh-Hant" ? "輸入" : "Input"
                    probe.check(probe.selectors[2].children[0].text === expected, "translated native Input heading")
                    console.warn("X171 LANGUAGE|" + root.scLang + "|" + expected + "|")
                    console.warn("X171 DONE|PASS|")
                    stop()
                }
            } catch(e) { console.warn("X171 ERROR|" + e + "|"); stop() }
        }
    }
}
