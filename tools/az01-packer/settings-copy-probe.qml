// Test only: native profile save, filtered load and byte-identical round trip.
import QtQuick 2.12
import Planck 1.0
Item {
    id: probe
    visible: false
    property int step: 0
    property string original: ""
    readonly property string profile: "/data/t7-drive/Engine Library/user.profile"
    function tr(key) { return Planck.getProperty("/Client/Preferences/Profile/Application/" + key).translator }
    function check(ok, label) { console.warn("T7 " + (ok ? "PASS|" : "FAIL|") + label + "|") }
    Repeater {
        id: sources
        model: Planck.librarianController.browserSourceControllerPtr().connectedSourceModel()
        delegate: Item {
            property string rowName: String(model.name || "")
            Component.onCompleted: {
                for (const k of Object.keys(model)) if (/path|type/i.test(k)) console.warn("T7 ROLE|" + k + "|")
            }
        }
    }
    function selectTestDrive() {
        const bsc = Planck.librarianController.browserSourceControllerPtr()
        for (let i = 0; i < sources.count; i++) if (/t7/i.test(sources.itemAt(i).rowName)) {
            bsc.select(bsc.connectedSourceModel().index(i, 0)); return
        }
    }
    Timer {
        interval: 1500; repeat: true; running: probe.step < 9
        onTriggered: {
            try {
                const dc = Planck.librarianController.devicesController
                const save = Planck.getProperty("/Client/Preferences/Profile/SaveApplicationProfile").translator
                const load = Planck.getProperty("/Client/Preferences/Profile/LoadApplicationProfile").translator
                if (probe.step === 0) {
                    probe.selectTestDrive()
                } else if (probe.step === 1) {
                    if (dc.getCurrentDevicePath() !== "/data/t7-drive") { probe.selectTestDrive(); return }
                    probe.tr("PlayerColor4A").color = "#654321"
                    probe.tr("DefaultSpeedRange").string = "±8"
                    const warning = probe.tr("TrackEndWarningTime")
                    const warningValue = warning.value
                    warning.value = 1
                    console.warn("T7 WARNING_MAX|" + warning.unnormalized + "|")
                    warning.value = 0
                    console.warn("T7 WARNING_MIN|" + warning.unnormalized + "|")
                    warning.value = warningValue
                    console.warn("T7 ENUM|" + JSON.stringify(probe.tr("DefaultSpeedRange").entries) + "|")
                    save.state = true
                } else if (probe.step === 2) {
                    if (save.state) return
                    probe.original = Planck.getTextFromFile(probe.profile)
                    probe.check(!!probe.original, "native profile exported")
                    Planck.writeToFile("/data/t7-native-before.profile", probe.original)
                    const filtered = JSON.parse(probe.original)
                    filtered.data.User.children = {DefaultSpeedRange: {string: "±20", type: 4}}
                    filtered.data.User.childorder = ["DefaultSpeedRange"]
                    Planck.writeToFile("/data/enginex/settings-copy/usb.profile", JSON.stringify(filtered))
                    Planck.writeToFile("/data/enginex/settings-copy/usb-read", (Math.floor(Date.now() / 1000) + 30) + "\n" + probe.profile + "\n")
                    load.state = true
                } else if (probe.step === 3) {
                    if (load.state) return
                    probe.check(probe.tr("DefaultSpeedRange").string === "±20", "native filtered profile applied")
                    probe.check(String(probe.tr("PlayerColor4A").color) === "#654321", "filtered profile retains player color")
                    Planck.writeToFile("/data/enginex/settings-copy/usb-read", "")
                    probe.check(Planck.getTextFromFile(probe.profile) === probe.original, "USB profile file unchanged during filtered import")
                    load.state = true
                } else if (probe.step === 4) {
                    if (load.state) return
                    probe.check(probe.tr("DefaultSpeedRange").string === "±8", "native profile undo restored value")
                    probe.check(String(probe.tr("PlayerColor4A").color) === "#654321", "undo retains player color")
                    save.state = true
                } else if (probe.step === 5) {
                    if (save.state) return
                    const after = Planck.getTextFromFile(probe.profile)
                    probe.check(after === probe.original, "native export bytes match original after undo")
                    Planck.writeToFile("/data/t7-native-after.profile", after)
                    console.warn("T7 DONE|")
                    probe.step = 9; return
                }
                probe.step++
            } catch (e) { console.warn("T7 FAIL|exception: " + e + "|"); probe.step = 9 }
        }
    }
}
