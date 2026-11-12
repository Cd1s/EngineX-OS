import pathlib
import re
import unittest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]


def section_between(text: str, start_heading: str, next_heading_prefix: str) -> str:
    start = text.index(start_heading)
    rest = text[start + len(start_heading) :]
    m = re.search(rf"(?m)^{re.escape(next_heading_prefix)}\s", rest)
    if not m:
        return text[start:]
    return text[start : start + len(start_heading) + m.start()]


class DocsAuditBH2A10Test(unittest.TestCase):
    def test_feature_inventory_section2_matches_5304_qml_menus_bh2_a10_1(self):
        manual = (REPO_ROOT / "docs/manual/01-feature-inventory-and-ui-spec.md").read_text(encoding="utf-8")
        sec2 = section_between(manual, "## 2. 碟机屏幕上的开关入口", "## 3.")

        # Obsolete 5.3.0.2 claims that must not appear as active UI architecture in Section 2:
        self.assertNotIn("16 项 EngineX 条目，平铺在原厂条目里", sec2)
        self.assertNotIn("仓库没有往这两处加任何 EngineX 条目", sec2)
        self.assertNotIn("设置 → 播放", sec2)
        self.assertNotIn("Settings → Playback", sec2)
        self.assertNotIn("Waveform Style、Live Stream", sec2)
        self.assertNotIn("| Live Stream（OFF / ON） |", sec2)
        self.assertNotRegex(sec2, r"\|\s*6\s*\|\s*Jog Light Mode")
        self.assertNotRegex(sec2, r"\|\s*7\s*\|\s*Auto-Analyze Drives")

        # True 5.3.0.4 QML menu tabs and entries verified against tools/ui-mod/build_packs.py:
        required_fragments = [
            "Pad LED Brightness",
            "Settings → Services",
            "Streaming Quality",
            "NetEase Download",
            "Live Stream",
            "Pro DJ Link Library",
            "Pro DJ Link Export to CDJ (Beta)",
            "Profile → Display",
            "Waveform Style",
            "Profile → Playback",
            "Master Soft Clipper",
            "Auto-Gain (0 dB Normalize)",
            "Stems EQ Mode",
            "Color FX Style",
            "PX Delay Time Mode",
            "Profile → Safety",
            "Sync Guard Tolerance",
            "Profile → Library",
            "OmniSource (Drives in Streaming)",
            "Tracklist Waveform",
            "Layout → ENGINEX VIEW PRESET",
            "Linked View",
            "4-Deck Role",
            "Shadow Deck Mode",
            "UI Scale",
            "Dual-Column Browser",
            "Cue Countdown",
            "Beat Jump Pad Bank",
            "Swap Loop / Beat Jump",
        ]
        for frag in required_fragments:
            self.assertIn(frag, sec2, f"Section 2 missing 5.3.0.4 QML entry/menu {frag!r}")

    def test_feature_inventory_echo_out_console_only_bh2_a10_4(self):
        manual = (REPO_ROOT / "docs/manual/01-feature-inventory-and-ui-spec.md").read_text(encoding="utf-8")
        active = section_between(manual, "## 1. 支持的 16 款机型", "## 5.")
        self.assertNotIn("暂停切歌时释放 2.2 秒回声尾音", active)
        self.assertNotIn("没有 `visible: !root.scPlayer`", active)
        self.assertIn("visible: root.scHasMixer", active)
        self.assertIn("Fader Echo", active)

    def test_content_js_smart_loops_is_stock_bh2_a10_8(self):
        cjs = (REPO_ROOT / "docs/site/content.js").read_text(encoding="utf-8")
        packs = (REPO_ROOT / "tools/ui-mod/build_packs.py").read_text(encoding="utf-8")
        self.assertNotIn("SmartLoops.qml", packs)
        self.assertIn("LinkBeatJumpLoopSize.qml", packs)
        self.assertNotIn("解锁原厂「联动节拍跳跃与 Loop 长度」与智能 Loop", cjs)
        self.assertNotIn("解鎖原廠「連動節拍跳躍與 Loop 長度」與智慧 Loop", cjs)
        self.assertNotIn("unlocked stock Link Beat Jump / Loop Size Controls and Smart Loops", cjs)
        self.assertIn("24-profile-cues-loops.png", cjs)
        self.assertIn("Link Beat Jump / Loop Size Controls", cjs)
        self.assertIn("stock Smart Loops", cjs)

    def test_installation_manual_linux_fastboot_and_firewall_bh2_a10_12(self):
        manual = (REPO_ROOT / "docs/manual/02-installation-and-ota.md").read_text(encoding="utf-8")
        self.assertIn("android-tools", manual)
        self.assertIn("pacman -S android-tools", manual)
        self.assertIn("11224", manual)
        self.assertIn("ufw", manual)
        self.assertIn("fastboot is not installed", manual)
        self.assertNotIn("处于原厂未解锁状态，需要 fastboot 但没找到", manual)

    def test_release_notes_draft_uses_top_level_layout_tab_bh2_a10_2(self):
        notes = (REPO_ROOT / "docs/release-notes-5.3.0.4-draft.md").read_text(encoding="utf-8")
        self.assertNotIn("Control Center → Layout", notes)
        self.assertIn("`Layout → ENGINEX VIEW PRESET → Linked View`", notes)






if __name__ == "__main__":
    unittest.main()
