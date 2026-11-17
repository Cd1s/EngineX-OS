import QtQuick 2.12
import EngineOS.UI.Style 1.0
import UIKit.Elements 1.0
import Planck 1.0
import "../Buttons"
import "base"

Row {
    id: root
    objectName: "TimeIndicator"
    height: statusBar.height
    spacing: 10

    property alias dividerVisible: divider.visible
    property string loadText: ""

    DividerBar {
        id: divider
    }

    TextIndicator {
        id: loadIndicator
        objectName: "CpuRamLoad"
        anchors.verticalCenter: parent.verticalCenter
        active: true
        visible: root.loadText !== ""
        color: Style.font02Color
        font: Style.fontH4
        text: root.loadText
    }

    TextIndicator {
        id: clock
        objectName: "Clock"

        anchors.verticalCenter: parent.verticalCenter

        TextMetrics {
            id: metrics
            font: clock.font
            text: clock.timeFormat
        }

        active: true
        color: Style.font02Color
        font: Style.fontH3
        horizontalAlignment: Text.AlignRight
        width: metrics.boundingRect.width + 10
        property string timeFormat: Planck.timeDateController.timeFormatString

        Timer {
            id: timer
            interval: 1000
            running: clock.visible
            repeat: true
            triggeredOnStart: true
            onTriggered: clock.text = new Date().toLocaleTimeString(Qt.locale(), clock.timeFormat)
        }
    }

    Timer {
        id: loadTimer
        interval: 2000
        running: root.visible
        repeat: true
        triggeredOnStart: true
        onTriggered: {
            var xhr = new XMLHttpRequest()
            xhr.open("GET", "http://127.0.0.1:17840/status")
            xhr.onreadystatechange = function() {
                if (xhr.readyState !== XMLHttpRequest.DONE) return
                if (xhr.status !== 200) { root.loadText = ""; return }
                var d = JSON.parse(xhr.responseText)
                root.loadText = "CPU " + Math.round(d.cpuPercent) + "%  RAM " + Math.round(d.ramPercent) + "%"
            }
            xhr.send("")
        }
    }
}
