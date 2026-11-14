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
// The footer repeater is reachable only through the footer item (QML component scope).
let footerRows = [];
const connectedSource = { expanded: false, parent: null, itemAtIndex: () => null, footerItem: { scRepeater: { itemAt: r => { footerRows.push(r); return null } } } };
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
assert.deepStrictEqual(footerRows, [0]); // scrolled into view through the footer item
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

    def test_breadcrumb_stack_view_hides_tidal_rising_both_casings_issue_279(self):
        bp = load_build_packs()
        # Verify scEnterTimer uses push()
        self.assertIn('if (typeof row.push === "function") row.push()', bp.SERVICES_MAIN)
        self.assertIn("else row.clicked({})", bp.SERVICES_MAIN)

        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            stack_key = root + "Views/BrowseView/Components/BreadcrumbView/BreadcrumbStackView.qml"
            stack_joined = "\n".join(new for _old, new in edits[stack_key])
            self.assertIn('"TIDAL Rising", "Tidal Rising"', stack_joined)

        # Behavioral test in Node for both 5.0.4 ("TIDAL Rising") and 5.3.0 ("Tidal Rising")
        script = """
const assert = require("assert");

function evalRows(scope, categories) {
    const hidden = scope === "netease"
        ? ["Featured", "Genres", "Moods", "TIDAL Rising", "Tidal Rising"]
        : ["My Collection", "Featured", "Genres", "Moods", "TIDAL Rising", "Tidal Rising"];
    let n = categories.length;
    while (n > 0) {
        const entryLabel = categories[n - 1];
        if (hidden.indexOf(entryLabel) < 0) break;
        n--;
    }
    const hiddenMap = {};
    for (let i = 0; i < categories.length; i++) {
        const cat = categories[i];
        hiddenMap[cat] = scope !== "tidal" && hidden.indexOf(cat) >= 0;
    }
    return { visibleRows: scope === "tidal" ? categories.length : n, hiddenMap };
}

// 5.3.0 casing: "Tidal Rising"
const stock530 = ["My Playlists", "My Collection", "Featured", "Genres", "Moods", "Tidal Rising"];

// Spotify (Issue #279): should collapse to 1 visible row ("My Playlists"), no blank gaps
const resSpotify530 = evalRows("spotify", stock530);
assert.strictEqual(resSpotify530.visibleRows, 1);
assert.strictEqual(resSpotify530.hiddenMap["My Playlists"], false);
assert.strictEqual(resSpotify530.hiddenMap["My Collection"], true);
assert.strictEqual(resSpotify530.hiddenMap["Featured"], true);
assert.strictEqual(resSpotify530.hiddenMap["Genres"], true);
assert.strictEqual(resSpotify530.hiddenMap["Moods"], true);
assert.strictEqual(resSpotify530.hiddenMap["Tidal Rising"], true);

// NetEase: keeps "My Playlists" and "My Collection", hides 4 categories
const resNetease530 = evalRows("netease", stock530);
assert.strictEqual(resNetease530.visibleRows, 2);
assert.strictEqual(resNetease530.hiddenMap["My Playlists"], false);
assert.strictEqual(resNetease530.hiddenMap["My Collection"], false);
assert.strictEqual(resNetease530.hiddenMap["Tidal Rising"], true);

// Tidal: keeps all 6 categories
const resTidal530 = evalRows("tidal", stock530);
assert.strictEqual(resTidal530.visibleRows, 6);
for (const cat of stock530) assert.strictEqual(resTidal530.hiddenMap[cat], false);

// 5.0.4 casing: "TIDAL Rising"
const stock504 = ["My Playlists", "My Collection", "Featured", "Genres", "Moods", "TIDAL Rising"];
const resSpotify504 = evalRows("spotify", stock504);
assert.strictEqual(resSpotify504.visibleRows, 1);
assert.strictEqual(resSpotify504.hiddenMap["TIDAL Rising"], true);

const resNetease504 = evalRows("netease", stock504);
assert.strictEqual(resNetease504.visibleRows, 2);
assert.strictEqual(resNetease504.hiddenMap["TIDAL Rising"], true);

const resTidal504 = evalRows("tidal", stock504);
assert.strictEqual(resTidal504.visibleRows, 6);
"""
        res = subprocess.run(["node", "-e", script], capture_output=True, text=True)
        self.assertEqual(res.returncode, 0, res.stderr)

    def test_connected_source_clears_sc_tidal_row_on_destruction_bh2_a6_7(self):
        bp = load_build_packs()
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            key = root + "Views/BrowserSource/ConnectedSource.qml"
            joined = "\n".join(new for _old, new in edits[key])
            self.assertIn("Component.onDestruction: if (connectedSource.scTidalRow === this) connectedSource.scTidalRow = null", joined)

    def test_autoloop_show_text_uses_sc_loop_bh2_a6_10(self):
        bp = load_build_packs()
        bp.ROOT = "Skins/Embedded/"
        edits = bp.edits(False)
        key = "Skins/EngineOS/UI/CompositeControls/Deck/DeckItem/AutoLoop.qml"
        joined = "\n".join(new for _old, new in edits[key])
        self.assertIn("readonly property bool showText: Config.device.hasAutoLoop || scLoop", joined)

    def test_jp07_sync_indicator_guards_null_controller_bh2_a6_11(self):
        bp = load_build_packs()
        bp.ROOT = "Skins/Embedded/"
        edits = bp.edits(False)
        key = "Skins/Embedded/Views/MainView/Components/JP07SyncIndicator.qml"
        joined = "\n".join(new for _old, new in edits[key])
        self.assertIn("readonly property bool displayPhaseMeter: !!controller && controller.enabled", joined)

    def test_other_updates_hidden_to_prevent_stock_reflash_bh2_a6_12(self):
        bp = load_build_packs()
        for root in ("Skins/Embedded/", "Skins/EmbeddedCommon/"):
            bp.ROOT = root
            edits = bp.edits(False)
            key = root + "Views/ControlCenter/Tabs/Preferences/Entries/OtherUpdates.qml"
            self.assertTrue(key in edits, key)
            joined = "\n".join(new for _old, new in edits[key])
            self.assertIn("visible: false", joined)
            self.assertIn("model: []", joined)

        for ver in ("5.0.4", "5.3.0"):
            qrc = LAB / ver / "qrc"
            if not qrc.is_dir():
                continue
            with tempfile.TemporaryDirectory(prefix=f"ui-bh2-{ver}-") as tmp:
                out = Path(tmp) / "packs"
                res = subprocess.run(
                    ["python3", str(BUILD_PACKS), "--qrc", str(qrc), "--out", str(out)],
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(res.returncode, 0, f"{ver}: {res.stderr}")


if __name__ == "__main__":
    unittest.main()
