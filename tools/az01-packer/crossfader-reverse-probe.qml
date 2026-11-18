// #303: exercise the real settings row and read Engine's gains, not just its switch state.
import QtQuick
import QtQuick.Window
import Planck

Item {
    id: probe
    visible: false
    property var page
    property var row
    property real curve
    property int step: 0
    property bool saved: false
    function tr(name) { return Planck.getProperty("/Engine/Mixer/" + name).translator }
    function check(ok, why) { if (!ok) throw new Error(why) }
    function gains(position, reversed) {
        const a = tr("Channel1/Crossfader/PostCrossfadeLevel").value
        const b = tr("Channel2/Crossfader/PostCrossfadeLevel").value
        console.warn("X303 LEVEL|" + step + "|" + position + "|" + reversed + "|" + a + "|" + b)
        check(tr("CrossfaderPosition").value === position && tr("CrossfaderCurve").value === curve, "position or curve changed")
        check(row.checkedExternally === reversed && tr("CrossfaderReverse").state === reversed, "wrong switch state")
        check((position === 0) !== reversed ? a > 0 && b === 0 : b > 0 && a === 0, "wrong side audible")
    }
    Timer {
        interval: 350; repeat: true; running: true
        onTriggered: {
            try {
                switch (step) {
                case 0:
                    const c = Qt.createComponent("qrc:/Skins/Embedded/Views/ControlCenter/Tabs/Preferences/MixerTabContent.qml")
                    check(c.status === Component.Ready, c.errorString())
                    page = c.createObject(probe.Window.window.contentItem, {width: 1280, height: 720, z: 1000})
                    break
                case 1:
                    for (const entry of page.content)
                        if (entry.text === "Crossfader Reverse") row = entry
                    check(row && row.visible, "missing settings row")
                    check(!tr("CrossfaderReverse").state && !row.checkedExternally, "default not off")
                    curve = tr("CrossfaderCurve").value
                    tr("CrossfaderPosition").value = 0
                    page.contentY = Math.max(0, Math.min(page.contentHeight - page.height, row.y - page.height / 2))
                    break
                case 2: gains(0, false); row.clicked(); break
                case 3:
                    gains(0, true)
                    page.grabToImage(function(img) { saved = img.saveToFile("/tmp/crossfader-reverse.png") })
                    tr("CrossfaderPosition").value = 1
                    break
                case 4: gains(1, true); row.clicked(); break
                case 5: gains(1, false); row.clicked(); break
                case 6: gains(1, true); tr("CrossfaderPosition").value = 0; break
                case 7: gains(0, true); row.clicked(); break
                case 8:
                    gains(0, false)
                    check(saved, "missing screenshot")
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
