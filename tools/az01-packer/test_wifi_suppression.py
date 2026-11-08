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


if __name__ == "__main__":
    unittest.main()
