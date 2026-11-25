"""#276: the SC LIVE 2 / SC LIVE 4 FX bar shows where the hardware FX selector sends the effect.

The badge only reads the properties the stock selector writes. Its target lists must stay the ones in the stock
JP20 / JP21 assignment files, and it must not show on any other board.
"""
import importlib.util
from pathlib import Path
import re
import unittest

import legacy_names as legacy

SOURCE = Path(__file__).parents[1] / "ui-mod" / "build_packs.py"
spec = importlib.util.spec_from_file_location("build_packs", SOURCE)
bp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bp)

DJFX = "Skins/Embedded/Views/MainView/Fx/DJFx.qml"
LAB = Path(legacy.LAB)
# The SC LIVE 2 / 4 run the SC LIVE 4 Engine: its unpacked official rootfs first, then the other lab copies.
ASSIGNMENTS = ("sclive4-rea61-followup/rootfs/usr/Engine/AssignmentFiles/PresetAssignmentFiles",
               "5.3.0/sandbox/root/usr/Engine/AssignmentFiles/PresetAssignmentFiles",
               "5.3.0/tree/usr/Engine/AssignmentFiles/PresetAssignmentFiles")


def badge_targets(text):
    """The board -> target list table inside the badge, parsed from the QML text."""
    m = re.search(r"readonly property var scTargets: \(\{(.*?)\}\)\[Config\.device\.name\] \|\| \[\]", text, re.S)
    if not m:
        return None
    return {board: re.findall(r'"([A-Za-z0-9]+)"', names)
            for board, names in re.findall(r'"(JP\d+)": \[([^\]]*)\]', m.group(1))}


def stock_assignment(board):
    for rel in ASSIGNMENTS:
        f = LAB / rel / board / f"{board}_Controller_Assignments.qml"
        if f.is_file():
            return f.read_text(encoding="utf-8")
    return None


class FxAssignBadgeTest(unittest.TestCase):
    def setUp(self):
        self.root = bp.ROOT
        bp.ROOT = "Skins/Embedded/"

    def tearDown(self):
        bp.ROOT = self.root

    def edits(self):
        return bp.edits(dev=False).get(DJFX, [])

    def test_badge_is_in_the_djfx_edits_once(self):
        badges = [new for old, new in self.edits() if "scFxAssign" in new]
        self.assertEqual(len(badges), 1, "the DJFx.qml edits must add the FX assign badge exactly once")
        new = badges[0]
        self.assertTrue(new.endswith("    Button {\n        Layout.minimumWidth: 160\n"),
                        "the badge goes in front of the stock effect-name button and keeps it")
        self.assertIn('objectName: `DJFx${djFx.indicator}_Assign`', new)
        self.assertIn('objectName: `DJFx${djFx.indicator}_Assign_Target`', new)

    def test_badge_only_reads_the_assigned_properties(self):
        new = next(new for old, new in self.edits() if "scFxAssign" in new)
        paths = re.findall(r"Planck\.getProperty\(`([^`]*)`\)", new)
        self.assertEqual(paths, ["/Engine/Mixer/${t}/DJFx/Assigned"])
        self.assertNotRegex(new, r"translator\.(state|value|string)\s*=[^=]",
                            "the badge must never write a property: the hardware selector owns them")
        self.assertNotIn("XMLHttpRequest", new)
        self.assertNotIn("Timer", new)

    def test_badge_shows_only_on_sc_live_2_and_4(self):
        new = next(new for old, new in self.edits() if "scFxAssign" in new)
        self.assertEqual(badge_targets(new), {
            "JP20": ["Channel1", "Channel2", "Main"],
            "JP21": ["Channel1", "Channel2", "Channel3", "Channel4", "Main"],
        })
        self.assertIn("visible: scTargets.length > 0", new)
        self.assertIn("Layout.minimumWidth: visible ? 72 : 0", new)

    def test_targets_are_the_stock_selector_targets(self):
        for board in ("JP20", "JP21"):
            text = stock_assignment(board)
            if text is None:
                self.skipTest(f"stock {board} assignment file not in the firmware lab")
            names = re.search(r"FxAssignmentConfig \{.*?channelNames: \[([^\]]*)\]", text, re.S)
            switch = re.search(r"DJFxAssign \{\s*turnCC: (\d+)\s*velocities: \[([^\]]*)\]", text, re.S)
            self.assertTrue(names and switch, f"{board}: stock FX assign block not found")
            stock = re.findall(r'"([A-Za-z0-9]+)"', names.group(1))
            self.assertEqual(len(stock), len(switch.group(2).split(",")),
                             f"{board}: one selector position per target")
            new = next(new for old, new in self.edits() if "scFxAssign" in new)
            self.assertEqual(sorted(badge_targets(new)[board]), sorted(stock), board)

    def test_patch_applies_to_the_stock_djfx_qml(self):
        stock = LAB / "5.3.0" / "qrc" / DJFX
        if not stock.is_file():
            self.skipTest("firmware lab 5.3.0/qrc not available")
        text = stock.read_text(encoding="utf-8")
        for old, new in self.edits():
            text = bp.patch(text, old, new, DJFX)
        self.assertEqual(text.count("id: scFxAssign"), 1)
        badge = text.index("id: scFxAssign")
        button = text.index("    Button {\n        Layout.minimumWidth: 160")
        self.assertLess(badge, button, "badge sits left of the effect name")
        self.assertEqual(text.count("{"), text.count("}"))


if __name__ == "__main__":
    unittest.main()
