import subprocess
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
BUILD_PACKS = REPO / "tools" / "ui-mod" / "build_packs.py"
LAB = Path("/root/build/sc5000-lab")


class WifiSuppressionTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="wifi-test-"))

    def tearDown(self):
        subprocess.run(["rm", "-rf", str(self.tmp)], check=True)

    def test_wifi_and_streaming_suppression_530(self):
        qrc = LAB / "5.3.0" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found")

        out = self.tmp / "packs_530"
        res = subprocess.run(
            ["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
            capture_output=True,
            text=True,
        )
        self.assertEqual(res.returncode, 0, f"build_packs failed: {res.stderr}")

        # Check WifiLoginFailed.qml
        wifi_qml = out / "en" / "Skins" / "Embedded" / "Views" / "WifiSelect" / "WifiLoginFailed.qml"
        self.assertTrue(wifi_qml.is_file(), f"{wifi_qml} missing")
        content = wifi_qml.read_text(encoding="utf-8")
        self.assertIn("wifiVisible", content)
        self.assertIn("isPlaying", content)
        self.assertIn("if (!wifiVisible || isPlaying || reason === \"lost-connection\") return", content)

        # Check StreamingDialog.qml
        streaming_qml = out / "en" / "Skins" / "Embedded" / "Views" / "Streaming" / "StreamingDialog.qml"
        self.assertTrue(streaming_qml.is_file(), f"{streaming_qml} missing")
        s_content = streaming_qml.read_text(encoding="utf-8")
        self.assertIn("function onShowNoNetwork() {\n\t\t\treturn", s_content)
        self.assertIn("function onShowLostConnection() {\n\t\t\treturn", s_content)
        self.assertIn("isLoginVisible", s_content)
        self.assertIn("cancelLogoutSpinner", s_content)

        # Check SortMenu.qml & SortMenuDialog.qml in 5.3.0
        sort_qml = out / "en" / "Skins" / "Embedded" / "Views" / "BrowseView" / "Components" / "SortMenu" / "SortMenu.qml"
        self.assertTrue(sort_qml.is_file(), f"{sort_qml} missing")
        sort_content = sort_qml.read_text(encoding="utf-8")
        self.assertIn("isStreamingProviderWithSort ||", sort_content)
        self.assertIn("readonly property bool isStreamingProviderWithSort: Boolean(collectionController && collectionController.isCurrentCollectionStreaming)", sort_content)
        self.assertIn("if (isStreamingProviderWithSort) return showPlayOrderRole ? [\"song\", \"bpm\", \"artist\", \"key\", \"rating\", \"length\", \"playOrder\"] : [\"song\", \"bpm\", \"artist\", \"key\", \"rating\", \"length\"]", sort_content)
        self.assertIn("scPostSortSettings()", sort_content)
        self.assertIn("onSortOrderChanged:", sort_content)
        self.assertIn("dialog.sortRow = sortRow", sort_content)

        sort_dialog_qml = out / "en" / "Skins" / "Embedded" / "Views" / "BrowseView" / "Components" / "SortMenu" / "SortMenuDialog.qml"
        self.assertTrue(sort_dialog_qml.is_file(), f"{sort_dialog_qml} missing")
        dialog_content = sort_dialog_qml.read_text(encoding="utf-8")
        self.assertIn("property var sortRow: null", dialog_content)
        self.assertIn("if (sortRow && sortRow.isStreamingProviderWithSort) {\n                            sortRow.sortRole = cell.role\n                        }", dialog_content)

        # Check UIKit SortMenuDialog.qml & SortOrderButton.qml in 5.3.0
        uikit_dialog = out / "en" / "Skins" / "UIKit" / "Elements" / "StatusBar" / "SortMenuDialog.qml"
        self.assertTrue(uikit_dialog.is_file(), f"{uikit_dialog} missing")
        uikit_dialog_content = uikit_dialog.read_text(encoding="utf-8")
        self.assertIn("cc.isCurrentCollectionStreaming", uikit_dialog_content)
        self.assertIn('scReq.open("POST", "http://127.0.0.1:8080/v1/enginex/ui", true)', uikit_dialog_content)
        self.assertNotIn(', false)', uikit_dialog_content.split('scReq.open(', 1)[1].split('\n', 1)[0])
        self.assertTrue(uikit_dialog_content.index("http://127.0.0.1:8080/v1/enginex/ui") < uikit_dialog_content.index("selectRole"))

        uikit_order = out / "en" / "Skins" / "UIKit" / "Elements" / "StatusBar" / "SortOrderButton.qml"
        self.assertTrue(uikit_order.is_file(), f"{uikit_order} missing")
        uikit_order_content = uikit_order.read_text(encoding="utf-8")
        self.assertIn("cc.isCurrentCollectionStreaming", uikit_order_content)
        self.assertIn('scReq.open("POST", "http://127.0.0.1:8080/v1/enginex/ui", true)', uikit_order_content)
        self.assertNotIn(', false)', uikit_order_content.split('scReq.open(', 1)[1].split('\n', 1)[0])
        self.assertTrue(uikit_order_content.index("http://127.0.0.1:8080/v1/enginex/ui") < uikit_order_content.index("toggleOrder"))

    def test_wifi_and_streaming_suppression_504(self):
        qrc = LAB / "5.0.4" / "qrc"
        if not qrc.is_dir():
            self.skipTest(f"{qrc} not found")

        out = self.tmp / "packs_504"
        res = subprocess.run(
            ["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
            capture_output=True,
            text=True,
        )
        self.assertEqual(res.returncode, 0, f"build_packs failed: {res.stderr}")

        # Check WifiLoginFailed.qml in 5.0.4
        wifi_qml = out / "en" / "Skins" / "EmbeddedCommon" / "Views" / "WifiSelect" / "WifiLoginFailed.qml"
        self.assertTrue(wifi_qml.is_file(), f"{wifi_qml} missing")
        content = wifi_qml.read_text(encoding="utf-8")
        self.assertIn("wifiVisible", content)
        self.assertIn("isPlaying", content)
        self.assertIn("if (!wifiVisible || isPlaying || reason === \"lost-connection\") return", content)

        # Check StreamingDialog.qml in 5.0.4
        streaming_qml = out / "en" / "Skins" / "EmbeddedCommon" / "Views" / "Streaming" / "StreamingDialog.qml"
        self.assertTrue(streaming_qml.is_file(), f"{streaming_qml} missing")
        s_content = streaming_qml.read_text(encoding="utf-8")
        self.assertIn("function onShowNoNetwork() {\n\t\t\treturn", s_content)
        self.assertIn("function onShowLostConnection() {\n\t\t\treturn", s_content)
        self.assertIn("isLoginVisible", s_content)
        self.assertIn("cancelLogoutSpinner", s_content)

        # Check SortMenu.qml & SortMenuDialog.qml in 5.0.4
        sort_qml_504 = out / "en" / "Skins" / "EmbeddedCommon" / "Views" / "BrowseView" / "Components" / "SortMenu" / "SortMenu.qml"
        self.assertTrue(sort_qml_504.is_file(), f"{sort_qml_504} missing")
        sort_content_504 = sort_qml_504.read_text(encoding="utf-8")
        self.assertIn("|| isStreamingProviderWithSort", sort_content_504)
        self.assertIn("if (isStreamingProviderWithSort) return showPlayOrderRole ? [\"song\", \"bpm\", \"artist\", \"key\", \"rating\", \"length\", \"playOrder\"] : [\"song\", \"bpm\", \"artist\", \"key\", \"rating\", \"length\"]", sort_content_504)
        self.assertIn('scReq.open("POST", "http://127.0.0.1:8080/v1/enginex/ui", true)', sort_content_504)
        self.assertNotIn(', false)', sort_content_504.split('scReq.open(', 1)[1].split('\n', 1)[0])
        self.assertIn("dialog.sortRow = sortRow", sort_content_504)

        sort_dialog_504 = out / "en" / "Skins" / "EmbeddedCommon" / "Views" / "BrowseView" / "Components" / "SortMenu" / "SortMenuDialog.qml"
        self.assertTrue(sort_dialog_504.is_file(), f"{sort_dialog_504} missing")
        dialog_content_504 = sort_dialog_504.read_text(encoding="utf-8")
        self.assertIn("property var sortRow: null", dialog_content_504)
        self.assertIn("if (sortRow && sortRow.isStreamingProviderWithSort) {\n                            sortRow.sortRole = cell.role\n                        }", dialog_content_504)
        self.assertTrue(dialog_content_504.index("sortRow.sortRole = cell.role") < dialog_content_504.index("collectionController.sortColumn"))


if __name__ == "__main__":
    unittest.main()
