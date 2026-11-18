import QtQuick 2.12
import EngineOS.UI.Controls 1.0
import EngineOS.UI.Style 1.0

Button {
    id: entryButton
    objectName: "EntryButton"
    // settings-polish: rounded like the option pills, lighter while pressed.
    background: Rectangle {
        radius: 6
        color: entryButton.pressed ? Qt.lighter(Style.surface03Color, 1.4) : Style.surface03Color
    }
}
