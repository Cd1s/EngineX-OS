import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 620; height: 340; color: "#111923"
    property string apiBase: "http://127.0.0.1:8080"
    property string message: "Checking Tailscale status…"
    property var status: ({})
    function refresh() {
        var xhr = new XMLHttpRequest()
        xhr.open("GET", apiBase + "/v1/sc5000/tailscale")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { message = "System API unavailable or authentication required. Open Remote access in the Web Console."; return }
            status = JSON.parse(xhr.responseText)
            message = status.running ? "Tailscale daemon running" : "Tailscale daemon stopped"
        }
        xhr.send()
    }
    Component.onCompleted: refresh()
    Timer { interval: 5000; running: root.visible; repeat: true; onTriggered: root.refresh() }
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 22
        Label { text: "TAILSCALE REMOTE · v1.104.5"; color: "white"; font.pixelSize: 22 }
        Label { text: root.message; color: "#62e8bc"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Label { text: JSON.stringify(root.status, null, 2); color: "#c6d2e4"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        RowLayout {
            Button { text: "Refresh"; onClicked: root.refresh() }
            Button { text: "Remote access settings"; onClicked: Qt.openUrlExternally(root.apiBase) }
        }
        Label { text: "Uses the existing verified system installer. This status Mod does not install a binary, sign in, or expose ports automatically."; color: "#9baac0"; wrapMode: Text.Wrap; Layout.fillWidth: true }
    }
}
