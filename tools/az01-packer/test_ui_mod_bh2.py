import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parent.parent.parent
BUILD_PACKS = REPO / "tools" / "ui-mod" / "build_packs.py"
LAB = Path("/root/build/sc5000-lab")


def load_build_packs():
    spec = importlib.util.spec_from_file_location("build_packs", BUILD_PACKS)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


class UiModBH2Test(unittest.TestCase):
    def test_connected_accounts_dialog_suppressed_for_custom_services_bh2_a6_2(self):
        bp = load_build_packs()
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            key = root + "Views/Streaming/StreamingConnectedAccountsDialog.qml"
            self.assertTrue(key in edits, key)
            joined = "\n".join(new for _old, new in edits[key])
            self.assertIn("root.scQuiet() || root.scUnasked(provider)", joined)

    def test_composite_artist_song_has_skip_keys_for_primego_mixstream_bh2_a6_3(self):
        bp = load_build_packs()
        bp.ROOT = "Skins/Embedded/"
        edits = bp.edits(False)
        key = "Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/ArtistSong.qml"
        joined = "\n".join(new for _old, new in edits[key])
        self.assertIn("id: scSkipKeys", joined)
        self.assertIn("scNoSkipKeys", joined)
        self.assertIn("PreviousTrack", joined)
        self.assertIn("NextTrack", joined)
        self.assertIn("eventPoint.position.x >= scSkipKeys.x", joined)

    def test_update_model_clears_when_up_to_date_bh2_a6_4(self):
        bp = load_build_packs()
        block = bp.SERVICES_MAIN.split("if (v.updateAvailable && v.downloadUrl)", 1)[1].split("x.send()", 1)[0]
        self.assertIn("scUpdateModel.clear()", block)

    def test_connected_source_encoder_navigation_skips_hidden_and_reaches_footer_bh2_a6_5(self):
        bp = load_build_packs()
        for root, focus_node in (
            ("Skins/Embedded/", "Skins/Embedded/StateNodes/EmbeddedControlFocusNode.qml"),
            ("Skins/EmbeddedCommon/", "Skins/EmbeddedCommon/StateNodes/EmbeddedCommonControlFocusNode.qml"),
        ):
            bp.ROOT = root
            edits = bp.edits(False)
            self.assertIn(focus_node, edits)
            focus_joined = "\n".join(new for _old, new in edits[focus_node])
            self.assertIn('name: "ScSourceIndex"', focus_joined)
            self.assertIn('name: "ScSourceSelect"', focus_joined)

            state_key = root + "StateNodes/States/SourceScreenState.qml"
            state_joined = "\n".join(new for _old, new in edits[state_key])
            self.assertIn('index: "/Qml/ControlFocus/ScSourceIndex"', state_joined)
            self.assertIn('action: "/Qml/ControlFocus/ScSourceSelect"', state_joined)

            conn_key = root + "Views/BrowserSource/ConnectedSource.qml"
            conn_joined = "\n".join(new for _old, new in edits[conn_key])
            self.assertIn("function scEntries()", conn_joined)
            self.assertIn("function scStep(", conn_joined)
            self.assertIn("function scActivate()", conn_joined)
            self.assertIn('connectedSource.scFocusKind === "footer"', conn_joined)

        js_body = bp.SOURCE_FOOTER.split("function scHiddenSourceName(name) {", 1)[1].split("\tConnections {\n\t\ttarget: PlanckGUI", 1)[0]
        script = """
const assert = require("assert");
let scFocusKind = "", scFocusRow = -1, scFocusService = "";
let opened = [], selectedConn = [];
const connectedSource = { expanded: false, parent: null, itemAtIndex: () => null };
const scFooterRepeater = { itemAt: () => null };
const root = {
  scSourceNameRole: 257,
  scServices: [{ id: "tidal", name: "TIDAL" }, { id: "spotify", name: "Spotify" }, { id: "deezer", name: "Deezer" }],
  scSources: ["tidal", "spotify", "deezer"],
  scOpen: id => opened.push(id),
};
const connRows = ["TIDAL", "SoundCloud", "USB 1"];
const cm = { rowCount: () => connRows.length, index: r => r, data: r => connRows[r] };
const Planck = { librarianController: { browserSourceControllerPtr: () => ({ connectedSourceModel: () => cm, availableSourceModel: () => null, select: idx => selectedConn.push(idx) }) } };
function scHiddenSourceName(name) {
""" + js_body + """
scStep(1);
assert.strictEqual(scFocusKind, "conn");
assert.strictEqual(scFocusRow, 2); // skipped hidden TIDAL(0) and SoundCloud(1), landed on USB 1(2)
scStep(-1);
assert.strictEqual(scFocusKind, "conn");
assert.strictEqual(scFocusRow, 2); // turning up clamps at first visible row, never enters 0-height rows
scStep(1);
assert.strictEqual(scFocusKind, "footer");
assert.strictEqual(scFocusService, "tidal");
scStep(1);
assert.strictEqual(scFocusService, "spotify");
scActivate();
assert.deepStrictEqual(opened, ["spotify"]);
"""
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_breadcrumb_stack_view_bounds_encoder_above_hidden_rows_bh2_a6_6(self):
        bp = load_build_packs()
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            stack_key = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml"
            stack_joined = "\n".join(new for _old, new in edits[stack_key])
            self.assertIn("readonly property int scVisibleRows:", stack_joined)
            self.assertIn("if (currentRowIndex + 1 < scVisibleRows) currentRowIndex++", stack_joined)
            self.assertIn("currentRowIndex = Math.max(0, scVisibleRows - 1)", stack_joined)

            view_key = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbView.qml"
            self.assertIn(view_key, edits)
            view_joined = "\n".join(new for _old, new in edits[view_key])
            self.assertIn("contentContainer.stackView.currentItem.scVisibleRows", view_joined)


if __name__ == "__main__":
    unittest.main()
