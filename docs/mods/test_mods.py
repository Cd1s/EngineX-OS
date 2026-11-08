#!/usr/bin/env python3
"""Lightweight behavior checks; no compiler, dependencies or hardware required.

Run with python3 -B docs/mods/test_mods.py. Temporary files stay under docs/mods.
"""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import struct
import sys
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch
import wave

BASE = Path(__file__).resolve().parent
# Use the real shared helpers while avoiding server startup or device access.
exec((BASE / "src/runtime.py").read_text().split("\nMOD =", 1)[0])


def feature(mid):
    spec = importlib.util.spec_from_file_location(mid, BASE / "src" / mid / "feature.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ModTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=BASE, prefix=".test-")
        self.state = Path(self.temp.name).resolve()
        global STATE
        STATE = self.state

    def tearDown(self):
        self.temp.cleanup()

    def test_registry_and_archive_members(self):
        registry = json.loads((BASE / "registry.json").read_text())
        self.assertEqual(registry["updatedAt"], "2026-11-08")
        self.assertEqual(len(registry["mods"]), 5)
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

    def test_packet_wire_layout(self):
        dmx = feature("dmx-studio")
        frame = bytes(range(256)) * 2
        art = dmx.artnet(2, 7, frame)
        self.assertEqual(len(art), 530)
        self.assertEqual(art[:8], b"Art-Net\0")
        self.assertEqual(struct.unpack_from("<H", art, 14)[0], 1)
        self.assertEqual(struct.unpack_from(">H", art, 16)[0], 512)
        self.assertEqual(art[18:], frame)
        acn = dmx.sacn(513, 255, frame, bytes(range(16)))
        self.assertEqual(len(acn), 638)
        self.assertEqual(acn[22:38], bytes(range(16)))
        for offset, length in ((16, 622), (38, 600), (115, 523)):
            self.assertEqual(struct.unpack_from(">H", acn, offset)[0] & 4095, length)
        self.assertEqual(acn[111], 255)
        self.assertEqual(struct.unpack_from(">H", acn, 113)[0], 513)
        self.assertEqual(struct.unpack_from(">H", acn, 123)[0], 513)
        self.assertEqual(acn[126:], frame)
        self.assertEqual(dmx.sacn(1, 1, frame, bytes(16), True)[112], 0x40)

    def test_dmx_patch_effects_smoke_and_blackout(self):
        dmx = feature("dmx-studio")
        app = dmx.Feature(self.state)
        try:
            app.stop.set(); app.thread.join()
            patch_data = [{"profile":"Moving Head 3-in-1", "universe":1, "address":1}, {"profile":"Smoke", "universe":1, "address":20}]
            app.action({"action":"configure", "config":{"patch":patch_data, "intensity":200, "smokeLevel":180, "smokeArmed":True, "smokeDuration":0}})
            app.running = True
            app.flash_until = 100
            self.assertEqual(app.render(99)[1][19], 180)
            self.assertEqual(app.render(101)[1][19], 0)
            self.assertEqual(app.render(101)[1][4], 200)
            for effect in ("Circle", "8-Figure", "Pan-Tilt Sweep"):
                app.config["effect"] = effect
                self.assertNotEqual(app.render(0)[1][:4], app.render(0.5)[1][:4])
            with self.assertRaises(ValueError):
                app.validated({"patch":patch_data + [{"profile":"Smoke", "universe":1, "address":1}]})
            calls = []
            class Adapter:
                def __init__(self, broken=False): self.broken = broken
                def send(self, frame):
                    calls.append(bytes(frame))
                    if self.broken: raise OSError("disconnected")
                def close(self): pass
            app.adapters = [({"protocol":"ftdi", "universe":1}, Adapter(True)), ({"protocol":"ftdi", "universe":2}, Adapter())]
            app.blackout()
            self.assertEqual(len(calls), 6)
            self.assertTrue(all(frame == bytes(512) for frame in calls))
            self.assertFalse(app.config["smokeArmed"])
            self.assertFalse(app.running)
        finally:
            app.close()

    def test_sampler_exact_slice_and_reference_only_clear(self):
        sampler = feature("sampler-pad-studio").Feature(self.state)
        source = self.state / "source.wav"
        audio = b"".join(struct.pack("<h", i) for i in range(100))
        with wave.open(str(source), "wb") as out:
            out.setparams((1, 2, 44100, 0, "NONE", "not compressed"))
            out.writeframes(audio)
        result = sampler.action({"action":"slice", "pad":8, "source":str(source), "inSample":10, "outSample":30, "sampleRate":44100})
        exported = Path(result["slots"][7]["path"])
        with wave.open(str(exported), "rb") as out:
            self.assertEqual(out.getnframes(), 20)
            self.assertEqual(out.readframes(20), audio[20:60])
        sampler.action({"action":"rename", "pad":8, "name":"Snare"})
        self.assertEqual(sampler.slots[7]["name"], "Snare")
        sampler.action({"action":"clear", "pad":8})
        self.assertEqual(sampler.slots[7]["path"], "")
        self.assertTrue(exported.exists())
        self.assertTrue(source.exists())
        with self.assertRaises(ValueError):
            sampler.action({"action":"slice", "pad":1, "source":str(source), "inSample":10, "outSample":300, "sampleRate":44100})

    def test_logo_original_restore(self):
        module = feature("jog-logo-studio")
        app = module.Feature(self.state / "logo-state")
        app.state.mkdir()
        app.path.write_bytes(b"cropped test PNG")
        volume = self.state / "volume"
        volume.mkdir()
        target = volume / "logo.png"
        target.write_bytes(b"original logo")
        with patch.object(app, "library", return_value=volume):
            app.action({"action":"apply", "volumes":["test"], "sync":False, "enabled":True})
            self.assertEqual(target.read_bytes(), b"cropped test PNG")
            app.action({"action":"apply", "volumes":["test"], "sync":False, "enabled":False})
            self.assertEqual(target.read_bytes(), b"original logo")

    def test_club_pause_restore_and_partial_restore(self):
        club = feature("minimal-club-mode")
        live = {club.SERVICES[0], club.SERVICES[1]}
        calls = []
        fail_start = set()
        def command(action, unit):
            calls.append((action, unit))
            if action == "is-active":
                return types.SimpleNamespace(returncode=0 if unit in live else 3, stdout="active" if unit in live else "inactive", stderr="")
            if action == "stop": live.discard(unit)
            if action == "start":
                if unit in fail_start: return types.SimpleNamespace(returncode=1)
                live.add(unit)
            return types.SimpleNamespace(returncode=0)
        with patch.object(club, "systemctl", side_effect=command):
            app = club.Feature(self.state)
            app.action({"action":"enable"})
            self.assertEqual(live, set())
            self.assertEqual(len(app.status()["services"]), 2)
            fail_start.add(club.SERVICES[1])
            with self.assertRaises(RuntimeError): app.action({"action":"disable"})
            self.assertEqual(app.status()["services"], [club.SERVICES[1]])
            fail_start.clear()
            app.action({"action":"disable"})
            self.assertEqual(live, {club.SERVICES[0], club.SERVICES[1]})
            self.assertFalse(app.path.exists())
            self.assertTrue(all(unit in club.SERVICES for _, unit in calls))


if __name__ == "__main__":
    unittest.main()
