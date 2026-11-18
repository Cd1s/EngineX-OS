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
        sites = re.findall(r'\{"([^"]+)", ' + ", ".join(["(0x[0-9a-f]+)"] * 8) + r'\}', table[:2500])
        apis = {m[0]: int(m[1], 16) for m in re.findall(r'\{"(5\.3\.0(?:-sclive4)?)", "[^"]*",\n(?:.*\n){5}\s+(0x[0-9a-f]+), 0x', source)}
        self.assertEqual({s[0] for s in sites}, set(self.BINARIES))
        checked = 0
        for name, lock, prepare_v2, close_v2, filename, bind_int64, bind_int, db_handle, sql in sites:
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
            lock, prepare_v2, close_v2, filename, bind_int64, bind_int, db_handle, sql = (int(x, 16) for x in (lock, prepare_v2, close_v2, filename, bind_int64, bind_int, db_handle, sql))
            self.assertEqual((word(lock), word(lock + 4)), (0xe16d42f4, 0xe1cd60f8), name)
            self.assertEqual((word(api + 4 * 116), word(api + 4 * 179), word(api + 4 * 180)), (prepare_v2, close_v2, filename), name)
            bl = word(prepare_v2 + 0x1c)
            self.assertEqual(bl >> 24, 0xEB, name)
            self.assertEqual((prepare_v2 + 0x1C + 8 + (((bl & 0xFFFFFF) ^ 0x800000) - 0x800000) * 4) & 0xFFFFFFFF, lock, name)
            self.assertEqual((word(close_v2), word(word(api + 4 * 16))), (0xE3A01001, 0xE3A01000), name)
            # Engine binds the analysis read's id: bind_int64, and bind_int that branches to it.
            self.assertEqual((word(api + 4 * 5), word(api + 4 * 4), word(api + 4 * 49), word(api + 4 * 153)), (bind_int64, bind_int, db_handle, sql), name)
            self.assertEqual((word(bind_int64), word(bind_int64 + 4)), (0xE16D41F8, 0xE1A04001), name)
            b = word(bind_int + 4)
            self.assertEqual((word(bind_int), b >> 24), (0xE1A03FC2, 0xEA), name)
            self.assertEqual((bind_int + 4 + 8 + (((b & 0xFFFFFF) ^ 0x800000) - 0x800000) * 4) & 0xFFFFFFFF, bind_int64, name)
            self.assertEqual([word(db_handle + 4 * k) for k in range(3)], [0xE3500000, 0x15900000, 0xE12FFF1E], name)
            self.assertEqual([word(sql + 4 * k) for k in range(3)], [0xE3500000, 0x159000D0, 0xE12FFF1E], name)
            checked += 1
        if not checked:
            self.skipTest("no lab Engine binaries on this host")


class GatewayTakesOnlyTidalTest(unittest.TestCase):
    """Engine reaches stream-sync's gateway only through its TIDAL client's two URLs: Engine Link
    (another deck's USB) and the Remote Library (Engine DJ desktop) keep Engine's own transport."""

    def test_only_tidal_urls_point_at_stream_sync(self):
        import importlib.util
        path = Path(__file__).resolve().parent / "unlock_jp07_features.py"
        spec = importlib.util.spec_from_file_location("unlock_jp07_features", path)
        unlock = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(unlock)
        redirected = []
        for patch in unlock.PATCHES:
            _, old, new, _ = patch
            for shift in range(-8, 9):
                text = bytes((b - shift) & 0xFF for b in new)
                if unlock.STREAM_SYNC_URL in text:
                    redirected.append(bytes((b - shift) & 0xFF for b in old))
        self.assertEqual(redirected, [b"https://api.tidal.com/v1", b"https://auth.tidal.com/v"])
