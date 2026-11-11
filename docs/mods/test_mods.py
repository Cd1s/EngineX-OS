#!/usr/bin/env python3
"""Package checks for the official Mods; no compiler, dependencies or hardware required.

Run with python3 -B docs/mods/test_mods.py. The Mods' behaviour tests are Go tests
next to their sources (go test ./docs/mods/src/...): the decks have no Python (#208).
"""
import hashlib
import io
import json
from pathlib import Path
import struct
import tarfile
import unittest

BASE = Path(__file__).resolve().parent
ELF_MACHINE = {"enginex-mod-helper-armv7": (1, 40), "enginex-mod-helper-arm64": (2, 183)}  # (EI_CLASS, e_machine)


class ModTests(unittest.TestCase):
    def test_registry_and_archive_members(self):
        registry = json.loads((BASE / "registry.json").read_text())
        self.assertEqual(registry["updatedAt"], "2026-11-12")
        self.assertEqual(len(registry["mods"]), 6)
        for mod in registry["mods"]:
            blob = (BASE / "packages" / Path(mod["downloadUrl"]).name).read_bytes()
            self.assertEqual(len(blob), mod["size"])
            self.assertEqual(hashlib.sha256(blob).hexdigest(), mod["sha256"])
            with tarfile.open(fileobj=io.BytesIO(blob), mode="r:gz") as tar:
                members = tar.getmembers()
                self.assertTrue(all(m.isfile() and not Path(m.name).is_absolute() and ".." not in Path(m.name).parts for m in members))
                manifest = json.load(tar.extractfile("mod.json"))
                self.assertEqual(manifest["id"], mod["id"])
                self.assertEqual(manifest["version"], mod["version"])
                self.assertIn(manifest["script"], tar.getnames())
                for source in manifest["qmlFiles"]:
                    self.assertGreater(len(tar.extractfile(source).read()), 0)
            for key in ("name", "summary", "description"):
                self.assertEqual(set(mod[key]), {"s", "t", "e"})

    def test_no_package_ships_or_calls_python(self):
        # #208: Engine OS (gen-1, gen-2 and AZ04) has no Python; 1.0.0 helpers could never start.
        registry = json.loads((BASE / "registry.json").read_text())
        for mod in registry["mods"]:
            with tarfile.open(BASE / "packages" / Path(mod["downloadUrl"]).name) as tar:
                manifest = json.load(tar.extractfile("mod.json"))
                for member in tar.getmembers():
                    self.assertFalse(member.name.endswith((".py", ".pyc")), member.name)
                    data = tar.extractfile(member).read()
                    if member.name.endswith((".sh", ".qml")):
                        self.assertNotIn(b"python", data.lower(), member.name)
                if mod["id"] == "tailscale-remote":
                    continue
                self.assertEqual(manifest["script"], "start.sh")
                self.assertFalse(mod["requiresPython"])
                for name, (ei_class, machine) in ELF_MACHINE.items():
                    member = tar.getmember("bin/" + name)
                    self.assertEqual(member.mode & 0o111, 0o111, name)
                    elf = tar.extractfile(member).read()
                    self.assertEqual(elf[:4], b"\x7fELF", name)
                    self.assertEqual((elf[4], struct.unpack_from("<H", elf, 18)[0]), (ei_class, machine), name)
                    # Static: no PT_INTERP program header, so no libc or loader dependency.
                    if ei_class == 1:
                        phoff, phentsize, phnum = struct.unpack_from("<I", elf, 28)[0], *struct.unpack_from("<HH", elf, 42)
                    else:
                        phoff, phentsize, phnum = struct.unpack_from("<Q", elf, 32)[0], *struct.unpack_from("<HH", elf, 54)
                    types = [struct.unpack_from("<I", elf, phoff + i * phentsize)[0] for i in range(phnum)]
                    self.assertNotIn(3, types, f"{name} is dynamically linked")
                start = tar.extractfile("start.sh").read().decode()
                self.assertIn("uname -m", start)
                self.assertIn("enginex-mod-helper-armv7", start)
                self.assertIn("enginex-mod-helper-arm64", start)


if __name__ == "__main__":
    unittest.main()
