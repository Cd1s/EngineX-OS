import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 720; height: 440; color: "#111923"
    property var state: ({})
    property string message: "Reading CPU and RAM load…"
    function refresh() {
        var xhr = new XMLHttpRequest()
        xhr.open("GET", "http://127.0.0.1:17840/status")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { root.message = xhr.responseText || "Helper unavailable"; return }
            root.state = JSON.parse(xhr.responseText)
            root.message = "CPU " + Math.round(root.state.cpuPercent) + "%  ·  RAM " +
                Math.round(root.state.ramPercent) + "% (" + root.state.ramUsedMB + " / " + root.state.ramTotalMB + " MB)"
        }
        xhr.send("")
    }
    Component.onCompleted: refresh()
    Timer { interval: 2000; running: root.visible; repeat: true; onTriggered: root.refresh() }
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 24; spacing: 14
        Label { text: "CPU & RAM LOAD"; color: "white"; font.pixelSize: 26 }
        Label { text: root.message; color: "#5ce7b0"; font.pixelSize: 20; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Label { text: "Displays live CPU and RAM load in the top status bar next to the clock via TimeIndicator.qml overlay."; color: "#b9c9df"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Item { Layout.fillHeight: true }
    }
}
