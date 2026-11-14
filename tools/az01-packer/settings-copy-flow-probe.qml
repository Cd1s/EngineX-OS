// Native test-only receiver: stock USB import, then the real clean-restart Undo.
import QtQuick 2.12
import Planck 1.0
Item {
    id: probe
    visible: false
    property int step: 0
    property bool requestBusy: false
    property string hash: ""
    property string sourceProfile: ""
    readonly property string drive: "/media/T7-USB"
    readonly property string profile: drive + "/Engine Library/user.profile"
    function tr(key) { return Planck.getProperty("/Client/Preferences/Profile/Application/" + key).translator }
    function check(ok, label) { console.warn("T7 " + (ok ? "PASS|" : "FAIL|") + label + "|") }
    function post(action, body, done) {
        requestBusy = true
        root.scCopyRequest(action, body, function(value, error) {
            requestBusy = false
            if (!value) { console.warn("T7 FAIL|" + action + ": " + error + "|"); return }
            if (done) done(value)
        })
    }
    Component.onCompleted: {
        if (Planck.getTextFromFile("/data/t7-flow-stage") === "undo") step = 20
    }
    Timer {
        interval: 750; repeat: true; running: true
        onTriggered: {
            if (probe.requestBusy || !root.scCopyState.nativeTypes || !root.scCopyState.nativeTypes.DisplayFileName) return
            try {
                const save = Planck.getProperty("/Client/Preferences/Profile/SaveApplicationProfile").translator
                if (probe.step === 0) {
                    root.scCopySelect("T7-USB")
                    probe.step = 1
                } else if (probe.step === 1) {
                    if (root.scCopyCurrentDrive() !== probe.drive) { root.scCopySelect("T7-USB"); return }
                    Planck.getProperty("/Client/Preferences/Player").translator.index = 1
                    probe.tr("DefaultSpeedRange").string = "±8"
                    probe.tr("PlayerColor4A").color = "#654321"
                    save.state = true
                    probe.step = 2
                } else if (probe.step === 2) {
                    if (save.state) return
                    Planck.writeToFile("/data/t7-flow-settings.before", Planck.getTextFromFile("/data/enginex/ui/settings.conf"))
                    const source = JSON.parse(Planck.getTextFromFile(probe.profile))
                    source.data.User.children.DefaultSpeedRange.string = "±20"
                    source.data.User.children.PlayerColor4A.color = "#123456"
                    source.data.User.children.Name.string = "Foreign profile"
                    source.data.User.children.Product.string = "JP13"
                    probe.sourceProfile = JSON.stringify(source)
                    Planck.writeToFile(probe.profile, probe.sourceProfile)
                    const body = root.scCopyCapture(); body.drive = probe.drive
                    probe.step = 3
                    probe.post("usb-preview", body, function(value) {
                        probe.hash = value.profileHash
                        Planck.writeToFile(probe.drive + "/Engine Library/enginex.profile", JSON.stringify({version:1, profileSha256:probe.hash, ui:{waveform:"rgb"}}))
                        probe.step = 4
                    })
                } else if (probe.step === 4) {
                    const body = root.scCopyCapture()
                    body.drive = probe.drive; body.includeEngineX = true; body.profileHash = probe.hash; body.confirmed = true
                    probe.step = 5
                    probe.post("usb-prepare", body, function() {
                        root.scCopyUSBLoading = true
                        Planck.getProperty("/Client/Preferences/Profile/LoadApplicationProfile").translator.state = true
                        probe.step = 6
                    })
                } else if (probe.step === 6) {
                    if (root.scCopyState.operation.phase !== "done") return
                    probe.check(probe.tr("DefaultSpeedRange").string === "±20", "USB native preference applied through backend")
                    probe.check(root.scWave === "rgb", "USB EngineX companion applied without restart")
                    probe.check(Planck.getProperty("/Client/Preferences/Player").translator.index === 1, "USB retains Player number")
                    probe.check(String(probe.tr("PlayerColor4A").color) === "#654321", "USB retains local color")
                    probe.check(Planck.getTextFromFile(probe.profile) === probe.sourceProfile, "USB source bytes untouched")
                    Planck.writeToFile("/data/t7-flow-stage", "undo")
                    probe.step = 7
                    const undo = root.scCopyCapture(); undo.confirmed = true; undo.playing = false; undo.previousDrive = probe.drive
                    probe.post("undo", undo, function() { Planck.quit(QuitReasons.AnalyticsChange, true) })
                } else if (probe.step === 20) {
                    const op = root.scCopyState.operation || {}
                    if (op.phase !== "done" || op.operation !== "undo") return
                    probe.check(probe.tr("DefaultSpeedRange").string === "±8", "Undo native value restored after restart")
                    probe.check(Planck.getProperty("/Client/Preferences/Player").translator.index === 1, "Undo retains Player number")
                    probe.check(String(probe.tr("PlayerColor4A").color) === "#654321", "Undo retains local color")
                    probe.check(Planck.getTextFromFile("/data/enginex/ui/settings.conf") === Planck.getTextFromFile("/data/t7-flow-settings.before"), "Undo restores actual settings.conf bytes")
                    Planck.writeToFile("/data/t7-flow-done", "pass\n")
                    console.warn("T7 DONE|full USB import and restart Undo|")
                    probe.step = 99
                }
            } catch (e) { console.warn("T7 FAIL|flow exception: " + e + "|"); probe.step = 99 }
        }
    }
}
