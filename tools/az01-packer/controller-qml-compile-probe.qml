// Sandbox only: loaded over Skins/Embedded/main.qml by test_controller_qml_compile.sh, which fills in @BOARD@ and
// @URLS@. As the root it runs even when the main UI cannot load. It compiles, with the Engine's own QML compiler and its
// own type registrations, the main UI root (EmbeddedMain.qml), every controller-side QML file the UI pack replaces and
// every stock controller assignment and device file in this rootfs (those pull the replaced modules in, as the
// controller engine does at start-up). A file that does not compile here is dead on a deck: no main screen, or that
// board's pads, buttons or LEDs without their QML.
import QtQuick
import Planck

Rectangle {
    id: probe
    width: 1280
    height: 800
    color: "#282828"
    // Receives the C++ pointer of PlanckQuickView (as stock QmlError.qml)
    readonly property var planckQuickView: null
    readonly property string board: "@BOARD@"
    readonly property var urls: @URLS@
    function say(line) {
        console.warn(line.length > 100 ? line.slice(0, 100) + "..." : line)
    }
    Timer {
        interval: 1000; running: true
        onTriggered: {
            let ok = 0, bad = 0
            // Engine wraps long log lines: report each file by its line number in the list, errors by file name.
            for (let i = 0; i < probe.urls.length; i++) {
                const c = Qt.createComponent(probe.urls[i], Component.PreferSynchronous)
                if (c.status === Component.Ready) {
                    ok++
                    probe.say("CQC OK|" + probe.board + "|" + (i + 1) + "|")
                } else {
                    bad++
                    probe.say("CQC FAIL|" + probe.board + "|" + (i + 1) + "|")
                    for (const l of c.errorString().split("\n"))
                        if (l.trim()) probe.say("CQC ERR|" + probe.board + "|" + (i + 1) + "|" + l.trim().replace(/^.*\/([^\/]+\.qml)/, "$1"))
                }
            }
            probe.say("CQC DONE|" + probe.board + "|ok " + ok + "|failed " + bad + "|")
            Planck.getProperty("/Client/Quit").translator.state = true
        }
    }
}
