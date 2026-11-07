import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 880; height: 600; color: "#111923"
    property var state: ({})
    property string message: "Start the Jog Logo helper to connect."
    function request(body) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17835/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { root.message = xhr.responseText || "Helper unavailable"; return }
            root.state = JSON.parse(xhr.responseText)
            root.message = root.state.note
            if (body && body.action === "crop") { preview.source = ""; preview.source = root.state.preview + "?t=" + Date.now() }
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }
    Component.onCompleted: request(null)
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 20; spacing: 12
        Label { text: "JOG LOGO STUDIO · 600 × 600"; font.pixelSize: 24; color: "white" }
        Label { text: root.message; color: "#75e7bc"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        RowLayout {
            TextField { id: source; Layout.fillWidth: true; placeholderText: "Source image path under /media or /mnt"; selectByMouse: true }
            Button { text: "Center crop"; onClicked: root.request({action:"crop", source:source.text}) }
        }
        RowLayout {
            Rectangle {
                Layout.preferredWidth: 260; Layout.preferredHeight: 260; color: "#080d15"; radius: 130
                Image { id: preview; anchors.fill: parent; source: root.state.ready ? root.state.preview : ""; fillMode: Image.PreserveAspectFit; cache: false }
            }
            ColumnLayout {
                Layout.fillWidth: true
                Label { text: "Media volume paths · one per line"; color: "white" }
                TextArea { id: volumes; Layout.fillWidth: true; Layout.preferredHeight: 120; placeholderText: "/media/USB1\n/media/USB2"; selectByMouse: true }
                CheckBox { id: sync; text: "Sync to all listed drives (otherwise first only)" }
                CheckBox { id: enabled; text: "Custom logo enabled"; checked: true }
                Button {
                    text: "Apply to media / restore original"
                    onClicked: {
                        var paths = volumes.text.split("\n").map(function(p) { return p.trim() }).filter(function(p) { return p.length > 0 })
                        root.request({action:"apply", volumes:paths, sync:sync.checked, enabled:enabled.checked})
                    }
                }
            }
        }
        Label { text: "Writes Engine Library/logo.png, preserving any original. Disable restores the original. Reload the media profile on every player. Models without a jog display cannot show a jog logo."; color: "#a3b3ca"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Item { Layout.fillHeight: true }
    }
}
