"""The UI hook's stage fill (tools/ui-mod/rekordbox_stage.h) and the bridge's stage (pkg/rbstate).

The C test runs the fill against real SQLite and the bridge's own library schema. The contract
test reads the Go source: every name, magic, rule number and JSON field the hook reads must be
what pkg/rbstate writes, so a change on the Go side fails here instead of silently stopping the
fill on the deck.
"""
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
UI = ROOT / "tools/ui-mod"


class RekordboxStageFillTest(unittest.TestCase):
    def test_fill_against_real_sqlite_and_the_bridge_schema(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "test_rekordbox_stage"
            subprocess.run(["cc", "-O2", "-Wall", "-Wextra", "-Werror", "-Wno-unused-function", "-fsanitize=address,undefined",
                            "-pthread", str(UI / "test_rekordbox_stage.c"), "-o", str(binary), "-lsqlite3"], check=True)
            subprocess.run([str(binary), str(ROOT / "pkg/enginedb/schema.sql")], check=True, env={**os.environ, "TMPDIR": directory})


def struct_tags(source, name):
    body = re.search(r"^type %s struct \{\n(.*?)^\}" % name, source, re.S | re.M)[1]
    return dict(re.findall(r'^\s+(\w+)\s+[^`\n]*`json:"([^",]+)', body, re.M))


class RekordboxStageContractTest(unittest.TestCase):
    """What the hook reads is what pkg/rbstate writes."""

    def setUp(self):
        self.hook = (UI / "rekordbox_stage.h").read_text()
        self.stage = (ROOT / "pkg/rbstate/stage.go").read_text()
        self.rbstate = (ROOT / "pkg/rbstate/rbstate.go").read_text()
        self.snapshot = (ROOT / "pkg/rbstate/snapshot.go").read_text()

    def define(self, name):
        return re.search(r'^#define %s ("(?:[^"\\]|\\.)*"|\S+)' % name, self.hook, re.M)[1]

    def go_string(self, source, name):
        return '"%s"' % re.search(r'\b%s\s*=\s*"([^"]+)"' % name, source)[1]

    def test_names_magics_and_rules(self):
        self.assertEqual(self.define("SC_RBS_RULES"), re.search(r"\bRules\s*=\s*(\d+)", self.rbstate)[1])
        self.assertEqual(self.define("SC_RBS_INDEX"), self.go_string(self.stage, "stageIndex"))
        self.assertEqual(self.define("SC_RBS_SNAPSHOT"), self.go_string(self.rbstate, "SnapshotName"))
        self.assertEqual(self.define("SC_RBS_SNAPSHOT_MAGIC"), self.go_string(self.snapshot, "snapshotMagic"))
        self.assertEqual(self.define("SC_RBS_ROOT"), self.go_string(self.stage, "defaultStageRoot"))
        self.assertEqual(self.go_string(self.stage, "perfMagic"), '"RBXA1"')
        self.assertIn('memcmp(b, "RBXA1", 5)', self.hook)

    def test_file_names_and_the_perf_layout(self):
        # <hex SHA-1 of the key>.bin, written whole before its index line.
        self.assertRegex(self.stage, r"func stageName\(key string\) string \{\s*h := sha1\.Sum\(\[\]byte\(key\)\)\s*return hex\.EncodeToString\(h\[:\]\)")
        self.assertIn('{".bin", EncodePerf(t.Perf), line.HasPerf}', self.stage)
        # The magic, five little-endian uint32 lengths each with its bytes in this order, one Complete byte.
        self.assertIn("for _, blob := range [][]byte{p.TrackData, p.Overview, p.Beats, p.QuickCues, p.Loops}", self.stage)
        self.assertIn("binary.Write(&b, binary.LittleEndian, uint32(len(blob)))", self.stage)
        self.assertIn('{"trackData", "overviewWaveFormData", "beatData", "quickCues", "loops"}', self.hook)

    def test_json_fields_the_hook_reads(self):
        staged = struct_tags(self.stage, "Staged")
        line = struct_tags(self.stage, "indexLine")
        sig = struct_tags(self.rbstate, "TrackSig")
        header = struct_tags(self.rbstate, "Header")
        wanted = {
            staged["Key"]: "k", staged["Rel"]: "rel", staged["Missing"]: "missing", staged["Digest"]: "wr",
            staged["Degraded"]: "deg", staged["HasPerf"]: "perf", staged["ActiveLoops"]: "active",
            line["Drop"]: "drop", sig["Rules"]: "v", sig["Row"]: "w", staged["Sig"]: "sig", header["LibraryUUID"]: "uuid",
        }
        for go, hook in wanted.items():
            self.assertEqual(go, hook)
            self.assertIn('"%s"' % hook, self.hook)
        # The index embeds the staged track, so its fields are the line's own.
        self.assertRegex(self.stage, r"type indexLine struct \{\s*\*Staged")

    def test_member_order_the_index_table_reads(self):
        # The live-key table reads only a line's first members: a staged line is {"k":…,"sig":…,"rel":…
        # (Staged's first fields, and indexLine embeds Staged first), a drop is {"drop":…}.
        staged = re.search(r"^type Staged struct \{\n(.*?)^\}", self.stage, re.S | re.M)[1]
        first = re.findall(r"^\t(\w+)\s", staged, re.M)[:3]
        self.assertEqual(first, ["Key", "Sig", "Rel"])
        line = re.search(r"^type indexLine struct \{\n(.*?)^\}", self.stage, re.S | re.M)[1]
        self.assertEqual(re.findall(r"^\t(\S+)", line, re.M)[:2], ["*Staged", "Drop"])
        self.assertIn("appendLine(indexLine{Drop: k})", self.stage)
        # The snapshot header's members up to the uuid, in Go's order.
        header = re.search(r"^type Header struct \{\n(.*?)^\}", self.rbstate, re.S | re.M)[1]
        self.assertEqual(re.findall(r"^\t(\w+)\s", header, re.M)[:3], ["Format", "Rules", "LibraryUUID"])

    def test_the_hook_folds_paths_as_fold_path_does(self):
        fold = re.search(r"^func FoldPath\(rel string\) string \{\n(.*?)^\}", self.rbstate, re.S | re.M)[1]
        steps = ['strings.ReplaceAll(rel, "\\\\", "/")', 'strings.TrimPrefix(rel, "../")', 'strings.TrimLeft(rel, "/")', "strings.ToLower(rel)"]
        at = [fold.index(step) for step in steps]
        self.assertEqual(at, sorted(at))
        # Non-ASCII paths are Go's alone (NFC and Unicode case folding): the hook goes by "rel".
        self.assertIn("norm.NFC.String(cases.Fold().String(norm.NFC.String(rel)))", fold)


if __name__ == "__main__":
    unittest.main()
