// Sandbox regression for #303. Exercise the generated Mixer page and its stock translator.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    property var page
    property var row
    property int step: 0
    property real position
    property real curve
    property bool saved: false
    function levels() {
        const result = []
        for (let i = 1; i <= 2; i++) {
            const p = tr("Channel" + i + "/Crossfader/PostCrossfadeLevel")
            result.push(p ? [p.value, p.unnormalized] : null)
        }
        return JSON.stringify(result)
    }
    function tr(name) { return Planck.getProperty("/Engine/Mixer/" + name).translator }
    function check(ok, reason) { if (!ok) throw new Error(reason) }
    Timer {
        interval: 1000
        running: true
        repeat: true
        onTriggered: {
            try {
                if (step === 0) {
                    const c = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/MixerTabContent.qml")
                    check(c.status === Component.Ready, c.errorString())
                    page = c.createObject(probe.Window.window.contentItem, {width: 1280, height: 720, z: 1000})
                    check(!!page, "Mixer page missing")
                } else if (step === 1) {
                    for (let i = 0; i < page.content.length; i++)
                        if (page.content[i].reverse !== undefined) row = page.content[i]
                    const rev = Planck.getProperty("/Engine/Mixer/CrossfaderReverse")
                    console.warn("X303 ENDPOINT|" + Config.device.name + "|" + (rev && rev.translator ? typeof rev.translator.state : "missing"))
                    check(row && row.visible, "Crossfader Reverse row missing")
                    check(tr("CrossfaderReverse").state === false && !row.checkedExternally, "default not off")
                    tr("CrossfaderPosition").value = 0
                    position = tr("CrossfaderPosition").value
                    curve = tr("CrossfaderCurve").value
                    page.contentY = Math.min(page.contentHeight - page.height, Math.max(0, row.y - page.height / 2))
                } else if (step === 2) {
                    console.warn("X303 NORMAL|" + levels())
                    row.clicked()
                } else if (step === 3) {
                    console.warn("X303 REVERSE|" + levels())
                    check(tr("CrossfaderReverse").state === true && row.checkedExternally, "switch did not enable reverse")
                    check(position === tr("CrossfaderPosition").value && curve === tr("CrossfaderCurve").value, "position or curve changed")
                    page.grabToImage(function(r) { saved = r.saveToFile("/tmp/crossfader-reverse.png") })
                } else if (step === 4) {
                    row.clicked()
                } else if (step === 5) {
                    console.warn("X303 RESTORE|" + levels())
                    check(saved && !row.checkedExternally && !tr("CrossfaderReverse").state, "screenshot or return to normal failed")
                    console.warn("X303 PASS|" + Config.device.name)
                    running = false
                    Planck.getProperty("/Client/Quit").translator.state = true
                }
                step++
            } catch (e) {
                console.warn("X303 FAIL|" + e)
                running = false
                Planck.getProperty("/Client/Quit").translator.state = true
            }
        }
    }
}
