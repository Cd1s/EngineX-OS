import QtQuick 2.12
import QtQuick.Controls.Basic 2.12 as QuickControls
import EngineOS.UI.Controls 1.0
import EngineOS.UI.Style 1.0

PrefsEntry {
    id: entry

    property real spacing: 6
    property int entryWidth: narrow
        ? PrefsComponentSizes.ListEntryButton.Small
        : PrefsComponentSizes.ListEntryButton.Default
    property int rightMargin: 24

    property var values
    property var currentValue
    signal selected(int index)

    property int style: narrow && values.length > 4 ? ListEntryStyles.Style.SpinBox : ListEntryStyles.Style.Row

    property int defaultIndex: -1

    Loader {
        anchors {
            verticalCenter: parent.verticalCenter
            right: parent.right
            rightMargin: entry.rightMargin
        }
        height: parent.height
        sourceComponent: {
            switch (entry.style) {
                case ListEntryStyles.Style.Row:
                    return entry.row
                case ListEntryStyles.Style.SpinBox:
                    return entry.spinBox
            }
        }
    }

    property Component row: Component {
        Row {
            id: control
            height: parent.height
            spacing: entry.spacing

            QuickControls.ButtonGroup {
                buttons: control.children
                exclusive: defaultIndex === -1
            }

            Repeater {
                id: repeater
                model: entry.values

                Button {
                    objectName: "ListEntryButton"
                    function isSelected(value) {
                        if (typeof entry.currentValue === "string") {
                            return entry.currentValue === value
                        }
                        return entry.currentValue && entry.currentValue.includes(value)
                    }
					text: root.scT(modelData)
                    anchors.verticalCenter: parent.verticalCenter
                    width: entry.entryWidth
                    down: pressed || isSelected(modelData) // using checked instead does not update the binding correctly
                    onClicked: {
                        // Required to compare entry.currentValue == text rather than checked
                        // as checked is false in this event handler.
                        if(isSelected(modelData) && entry.defaultIndex >= 0) {
                            entry.selected(entry.defaultIndex)
                        } else {
                            entry.selected(index)
                        }
                    }
                    visible: text.length > 0
                    opacity: enabled ? 1.0 : 0.3
                    background: Rectangle {
                        readonly property bool chosen: parent.isSelected(modelData)
                        radius: 6
                        color: chosen ? Qt.rgba(Style.active02Color.r, Style.active02Color.g, Style.active02Color.b, 0.16)
                                      : parent.pressed ? Qt.rgba(1, 1, 1, 0.12) : Qt.rgba(1, 1, 1, 0.05)
                        border.width: chosen ? 1 : 0
                        border.color: Style.active02Color
                    }
                }
            }
        }
    }

    property Component spinBox: Component {
        QuickControls.SpinBox {
            id: control
            width: 286
            height: parent.height
            font: Style.fontH2
            value: valueFromText(entry.currentValue)
            onValueModified: entry.selected(value)
            from: 0
            to: entry.values.length - 1
            wrap: false
            textFromValue: function (value) {
                if (entry.values[value] === "") {
                    return "None"
                }
                return entry.values[value]
            }
            valueFromText: function (text) {
                for (let i = 0; i < entry.values.length; ++i) {
                    if (entry.values[i].toLowerCase() === text.toLowerCase()) {
                        return i
                    }
                }
                return value
            }
            background: null
            contentItem: Item {
                Text {
                    objectName: "valueItem"
                    width: parent.height
                    height: parent.height
                    anchors.right: parent.right
					text: root.scT(control.textFromValue(control.value, control.locale))
                    font: control.font
                    color: Style.font01Color
                    horizontalAlignment: Qt.AlignHCenter
                    verticalAlignment: Qt.AlignVCenter
                }
            }
            down.indicator: Rectangle {
                objectName: "decButton"
                x: 0
                width: parent.height
                anchors {
                    top: parent.top
                    bottom: parent.bottom
                    margins: 8
                }
                color: Style.surface03Color

                Text {
                    anchors.fill: parent
                    text: "-"
                    font: Style.fontH0
                    color: {
                        if (!parent.enabled) {
                            Style.disabled01Color
                        } else if (control.down.pressed) {
                            Style.active02Color
                        } else {
                            Style.font01Color
                        }
                    }
                    horizontalAlignment: Text.AlignHCenter
                    verticalAlignment: Text.AlignVCenter
                }
            }
            up.indicator: Rectangle {
                objectName: "incButton"
                x: width + 8
                width: parent.height
                anchors {
                    top: parent.top
                    bottom: parent.bottom
                    margins: 8
                }
                color: Style.surface03Color

                Text {
                    anchors.fill: parent
                    text: "+"
                    font: Style.fontH0
                    color: {
                        if (!parent.enabled) {
                            Style.disabled01Color
                        } else if (control.up.pressed) {
                            Style.active02Color
                        } else {
                            Style.font01Color
                        }
                    }
                    horizontalAlignment: Text.AlignHCenter
                    verticalAlignment: Text.AlignVCenter
                }
            }
        }
    }
}
