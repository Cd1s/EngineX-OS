import subprocess
import tempfile
import unittest
from pathlib import Path


class RekordboxStreamedAnalysisTest(unittest.TestCase):
    def test_statement_uri_and_update(self):
        source = Path(__file__).resolve().parents[1] / "ui-mod/test_rekordbox_analysis.c"
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "test_rekordbox_analysis"
            subprocess.run(["cc", "-O2", "-Wall", "-Wextra", "-Werror", "-fsanitize=address,undefined", str(source), "-o", str(binary), "-lsqlite3"], check=True)
            subprocess.run([str(binary)], check=True)


class RekordboxAnalysisSitesTest(unittest.TestCase):
    """The hook's sqlite sites against the Engine binaries they are for (lab copies, when present)."""

    BINARIES = {
        "5.3.0": "/root/build/sc5000-lab/5.3.0/tree/usr/Engine/Engine",
        "5.3.0-sclive4": "/root/build/sc5000-lab/sclive4-rea61-followup/rootfs/usr/Engine/Engine",
    }

    def test_sites_match_engine(self):
        import re
        import struct
        source = (Path(__file__).resolve().parents[1] / "ui-mod/engine_ui_hook.c").read_text()
        table = source[source.index("install_rekordbox_analysis(uintptr_t base, size_t page) {"):]
        sites = re.findall(r'\{"([^"]+)", (0x[0-9a-f]+), (0x[0-9a-f]+), (0x[0-9a-f]+), (0x[0-9a-f]+)\}', table[:2000])
        apis = {m[0]: int(m[1], 16) for m in re.findall(r'\{"(5\.3\.0(?:-sclive4)?)", "[^"]*",\n(?:.*\n){5}\s+(0x[0-9a-f]+), 0x', source)}
        self.assertEqual({s[0] for s in sites}, set(self.BINARIES))
        checked = 0
        for name, lock, prepare_v2, close_v2, filename in sites:
            path = Path(self.BINARIES[name])
            if not path.exists():
                continue
            elf = path.read_bytes()
            loads = [(int(l[2], 16), int(l[1], 16), int(l[4], 16)) for l in
                     (line.split() for line in subprocess.run(["readelf", "-lW", str(path)], capture_output=True, text=True, check=True).stdout.splitlines())
                     if l and l[0] == "LOAD"]

            def word(va):
                for vaddr, offset, size in loads:
                    if vaddr <= va < vaddr + size:
                        return struct.unpack_from("<I", elf, va - vaddr + offset)[0]
                raise AssertionError(hex(va))

            api = apis[name]
            lock, prepare_v2, close_v2, filename = (int(x, 16) for x in (lock, prepare_v2, close_v2, filename))
            self.assertEqual((word(lock), word(lock + 4)), (0xe16d42f4, 0xe1cd60f8), name)
            self.assertEqual((word(api + 4 * 116), word(api + 4 * 179), word(api + 4 * 180)), (prepare_v2, close_v2, filename), name)
            bl = word(prepare_v2 + 0x1c)
            self.assertEqual(bl >> 24, 0xEB, name)
            self.assertEqual((prepare_v2 + 0x1C + 8 + (((bl & 0xFFFFFF) ^ 0x800000) - 0x800000) * 4) & 0xFFFFFFFF, lock, name)
            self.assertEqual((word(close_v2), word(word(api + 4 * 16))), (0xE3A01001, 0xE3A01000), name)
            checked += 1
        if not checked:
            self.skipTest("no lab Engine binaries on this host")
