import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 720; height: 440; color: "#111923"
    property var state: ({})
    property string message: "Checking optional background services…"
    function request(body) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17837/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { root.message = xhr.responseText || "Helper unavailable"; return }
            root.state = JSON.parse(xhr.responseText)
            root.message = root.state.active ? "CLUB MODE · " + root.state.phase : "STANDARD MODE"
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }
    Component.onCompleted: request(null)
    Timer { interval: 5000; running: root.visible; repeat: true; onTriggered: root.request(null) }
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 24; spacing: 14
        Label { text: "MINIMAL CLUB MODE"; color: "white"; font.pixelSize: 26 }
        Label { text: root.message; color: root.state.active ? "#5ce7b0" : "#b4c6de"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Label { text: "Local USB / SD · Rekordbox bridge · Pro DJ Link · Audio engine preserved"; color: "white"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        RowLayout {
            Button { text: "Pause optional services"; enabled: !root.state.active; onClicked: root.request({action:"enable"}) }
            Button { text: "Restore previous services"; enabled: !!root.state.active; onClicked: root.request({action:"disable"}) }
        }
        Label { text: "Paused: " + (root.state.services ? root.state.services.join(", ") : "none"); color: "#b9c9df"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Label { text: "A fixed allowlist pauses separate download/deemix/stems/broadcast units. The combined stream-sync host remains active to preserve controls and bridge functions. Only previously running services resume; there is no permanent masking or 100% CPU guarantee."; color: "#a2b2c8"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Item { Layout.fillHeight: true }
    }
}
