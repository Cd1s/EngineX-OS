import QtQuick 2.12
import QtQuick.Layouts 1.12
import EngineOS.UI.Style 1.0
import "Base"

Text {
    Layout.fillWidth: true
    Layout.preferredWidth: PrefsComponentSizes.Widths.Default
    Layout.preferredHeight: PrefsComponentSizes.Heights.Headline
    font: Style.fontH3
    verticalAlignment: Text.AlignBottom
    // settings-polish: in line with the rows, a little air above them, quieter grey.
    leftPadding: 24
    bottomPadding: 6
    color: Qt.rgba(1, 1, 1, 0.55)
}
