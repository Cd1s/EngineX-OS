import QtQuick 2.12
import QtQuick.Controls 2.5
import QtQuick.Layouts 1.12

Rectangle {
    id: root
    width: 940; height: 620; color: "#111923"
    property var slots: []
    property int pad: 1
    property string message: "Select a slot, then import or slice a source file."
    function request(body) {
        var xhr = new XMLHttpRequest()
        xhr.open(body ? "POST" : "GET", "http://127.0.0.1:17836/" + (body ? "action" : "status"))
        if (body) xhr.setRequestHeader("Content-Type", "application/json")
        xhr.onreadystatechange = function() {
            if (xhr.readyState !== XMLHttpRequest.DONE) return
            if (xhr.status !== 200) { root.message = xhr.responseText || "Helper unavailable"; return }
            var value = JSON.parse(xhr.responseText)
            root.slots = value.slots; root.message = value.note
        }
        xhr.send(body ? JSON.stringify(body) : "")
    }
    Component.onCompleted: request(null)
    ColumnLayout {
        anchors.fill: parent; anchors.margins: 18; spacing: 10
        Label { text: "SAMPLER PAD STUDIO · 8 SLOT BANK"; font.pixelSize: 24; color: "white" }
        Label { text: root.message; color: "#77e6bd"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        GridLayout {
            columns: 4; Layout.fillWidth: true
            Repeater {
                model: 8
                Button {
                    Layout.fillWidth: true; Layout.preferredHeight: 82
                    highlighted: root.pad === index + 1
                    text: (index + 1) + " · " + (root.slots[index] ? root.slots[index].name : "Empty") + "\n" + (root.slots[index] && root.slots[index].path ? root.slots[index].duration.toFixed(2) + " seconds" : "No sample")
                    onClicked: { root.pad = index + 1; rename.text = root.slots[index] ? root.slots[index].name : "" }
                }
            }
        }
        RowLayout {
            TextField { id: source; Layout.fillWidth: true; placeholderText: "Source audio path under /media or /mnt"; selectByMouse: true }
            Button { text: "Import to slot " + root.pad; onClicked: root.request({action:"import", pad:root.pad, source:source.text}) }
        }
        RowLayout {
            TextField { id: rename; Layout.fillWidth: true; placeholderText: "Sample name"; selectByMouse: true }
            Button { text: "Rename"; onClicked: root.request({action:"rename", pad:root.pad, name:rename.text}) }
            Button { text: "Clear slot"; onClicked: root.request({action:"clear", pad:root.pad}) }
        }
        RowLayout {
            TextField { id: loopIn; placeholderText: "Loop IN sample"; text: "0"; validator: DoubleValidator { bottom: 0; decimals: 0 } }
            TextField { id: loopOut; placeholderText: "Loop OUT sample"; text: "44100"; validator: DoubleValidator { bottom: 1; decimals: 0 } }
            TextField { id: rate; placeholderText: "Sample rate"; text: "44100"; validator: IntValidator { bottom: 8000; top: 384000 } }
            Button { text: "Slice Loop → slot"; onClicked: root.request({action:"slice", pad:root.pad, source:source.text, inSample:Number(loopIn.text), outSample:Number(loopOut.text), sampleRate:Number(rate.text)}) }
        }
        Label { text: root.slots[root.pad - 1] ? root.slots[root.pad - 1].path : ""; color: "#bbcde3"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Label { text: "This is an independent file bank. Load the exported WAV into a native pad through the player browser. No native automatic pad assignment or playback is claimed. Clearing retains exported WAV history."; color: "#a1afc5"; wrapMode: Text.Wrap; Layout.fillWidth: true }
        Item { Layout.fillHeight: true }
    }
}
