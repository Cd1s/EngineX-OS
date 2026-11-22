"""Unit tests for tools/mapping/inventory.py.

Run: python3 -m unittest discover -s tools/mapping

The synthetic assignment file below uses the same module instances (and the same
spelling) as the stock JP07 file, so the tests need no firmware. The committed
inventory of the four player boards (pkg/mapping/data) is checked too, because
the console and the generator read it.
"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.normpath(os.path.join(HERE, "..", ".."))
DATA = os.path.join(ROOT, "pkg", "mapping", "data")
PLAYERS = ["JP07", "JP08", "JP13", "JP14"]

spec = importlib.util.spec_from_file_location("inventory", os.path.join(HERE, "inventory.py"))
inventory = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inventory)

SAMPLE = """import airAssignments 1.0
import ControlSurfaceModules 0.1
import QtQuick 2.12

MidiAssignment {
    objectName: "SC5000 Controller Assignment"

    GlobalAssignmentConfig {
        id: globalConfig
        midiChannel: 0
    }

    Shift {
        note: 26
        ledType: LedType.Simple
    }

    DeckAssignmentConfig {
        id: deckConfig
        midiChannel: globalConfig.midiChannel
    }

    Censor {
        note: 7
    }

    Sync {
        syncNote: 20
        syncHoldAction: Action.InstantDouble
        masterNote: 21
    }

    PadModeSelect {
        ledType: LedType.RGB
        buttonsModel: ListModel {
            ListElement {
                note: 27
            }
            ListElement {
                note: 30
            }
        }
    }

    Component.onCompleted: {
        Midi.sendNoteOn(globalConfig.midiChannel, 48, 1)
    }
}
"""


def run_inventory(assignments, qrc, out, *boards):
    return subprocess.run(
        [sys.executable, "-I", os.path.join(HERE, "inventory.py"), "--assignments", assignments,
         "--qrc", qrc, "--out", out, "--boards", *boards],
        check=True, capture_output=True, text=True).stdout


class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.assign = os.path.join(self.tmp.name, "assign")
        self.qrc = os.path.join(self.tmp.name, "qrc")
        self.out = os.path.join(self.tmp.name, "out")
        os.makedirs(os.path.join(self.assign, "JPXX"))
        os.makedirs(self.qrc)
        with open(os.path.join(self.assign, "JPXX", "JPXX_Controller_Assignments.qml"), "w") as f:
            f.write(SAMPLE)

    def controls(self):
        run_inventory(self.assign, self.qrc, self.out, "JPXX")
        with open(os.path.join(self.out, "JPXX.json")) as f:
            return json.load(f)

    def test_note_controls_get_module_and_note_ids(self):
        d = self.controls()
        ids = {c["id"]: c for c in d["controls"]}
        for want in ("Shift:26", "Censor:7", "Sync:20", "Sync:21", "PadModeSelect:27", "PadModeSelect:30"):
            self.assertIn(want, ids)
        self.assertEqual(ids["PadModeSelect:30"]["number"], 30)
        self.assertEqual(ids["PadModeSelect:30"]["type"], "Note")
        self.assertEqual(ids["Sync:20"]["module"], "Sync")
        self.assertEqual(d["controls_count"], len(d["controls"]))

    def test_ids_are_unique_and_a_repeat_is_numbered(self):
        rows = inventory.add_ids([
            {"module": "Media", "type": "Note", "number": 13},
            {"module": "Media", "type": "Note", "number": 13},
            {"module": "AutoLoop", "type": "CC", "number": 3},
            {"module": "JogWheel", "type": "14-bit CC", "number": "Upper: 17, Lower: 49"},
        ])
        self.assertEqual([r["id"] for r in rows], ["Media:13", "Media:13#2", "AutoLoop:cc3", "JogWheel:cc17+49"])

    def test_output_is_deterministic(self):
        first = self.controls()
        with open(os.path.join(self.out, "JPXX.json"), "rb") as f:
            a = f.read()
        run_inventory(self.assign, self.qrc, self.out, "JPXX")
        with open(os.path.join(self.out, "JPXX.json"), "rb") as f:
            self.assertEqual(a, f.read())
        self.assertEqual(first["board"], "JPXX")

    def test_markdown_only_on_request(self):
        run_inventory(self.assign, self.qrc, self.out, "JPXX")
        self.assertFalse(os.path.exists(os.path.join(self.out, "JPXX.md")))


class CommittedDataTests(unittest.TestCase):
    """The data the console and the generator read."""

    def load(self, board):
        with open(os.path.join(DATA, board + ".json")) as f:
            return json.load(f)

    def test_player_boards_are_committed_with_unique_ids(self):
        for board in PLAYERS:
            d = self.load(board)
            ids = [c["id"] for c in d["controls"]]
            self.assertEqual(len(ids), len(set(ids)), board)
            self.assertEqual(d["board"], board)
            self.assertEqual(d["controls_count"], len(ids))
            for c in d["controls"]:
                self.assertTrue(c["type"])
                if c["type"] in ("Note", "CC"):
                    self.assertIsInstance(c["number"], int)

    def test_the_players_share_their_button_notes(self):
        notes = {b: {c["id"] for c in self.load(b)["controls"] if c["type"] == "Note"} for b in PLAYERS}
        self.assertEqual(notes["JP07"], notes["JP13"])
        self.assertEqual(notes["JP08"], notes["JP14"])

    def test_the_buttons_the_mapping_needs_are_there(self):
        for board in PLAYERS:
            ids = {c["id"] for c in self.load(board)["controls"]}
            for want in ("Shift:26", "PadModeSelect:27", "PadModeSelect:28", "PadModeSelect:29",
                         "PadModeSelect:30", "Sync:20", "Shortcuts:12", "Back:16", "BrowseEncoder:18"):
                self.assertIn(want, ids, board)

    def test_no_build_host_paths_in_the_data(self):
        for board in PLAYERS:
            with open(os.path.join(DATA, board + ".json")) as f:
                text = f.read()
            self.assertNotIn("/root/", text)


if __name__ == "__main__":
    unittest.main()
