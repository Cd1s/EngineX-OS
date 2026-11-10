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
        self.assertEqual(registry["updatedAt"], "2026-11-10")
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

    def test_midi_mapper_preset_validation_and_custom_presets(self):
        midi = feature("midi-controller-mapper")
        state_dir = self.state / "midi-state"
        state_dir.mkdir(parents=True, exist_ok=True)
        # Corrupted config.json types must not crash Feature() initialization or /status
        (state_dir / "config.json").write_text(json.dumps({"targetDeck": "four", "enabled": "yes", "presetId": 123}))
        app = midi.Feature(state_dir)
        try:
            st = app.status()
            self.assertEqual(st["presetId"], "pioneer-cdj-400")
            self.assertEqual(st["targetDeck"], 3)
            self.assertFalse(st["enabled"])
            self.assertFalse(st["ledFeedbackSupported"])
            self.assertIn("transmit-only", st["ledFeedbackNote"])
            # Preset IDs with single dots (e.g. cdj.400) are valid
            custom = json.loads((BASE / "src/midi-controller-mapper/presets/pioneer-cdj-400.json").read_text())
            custom["id"] = "custom.cdj.400"
            custom["name"] = "Custom CDJ 400"
            app.action({"action": "save_preset", "preset": custom})
            st2 = app.action({"action": "configure", "presetId": "custom.cdj.400", "targetDeck": 4})
            self.assertEqual(st2["presetId"], "custom.cdj.400")
            self.assertEqual(st2["targetDeck"], 4)
            # Invalid user preset file is surfaced in presetErrors rather than silently ignored
            (state_dir / "presets" / "broken.json").write_text("{not valid json")
            st3 = app.action({"action": "refresh_devices"})
            self.assertTrue(any("broken.json" in err for err in st3["presetErrors"]))
            # Production action() must reject inject_midi test backdoor
            with self.assertRaises(ValueError):
                app.action({"action": "inject_midi", "messages": [[0x90, 0x00, 0x7F]]})
            for bad in (
                {"id": "bad"},
                {**custom, "id": "../escape"},
                {**custom, "id": "bad..dots"},
                {**custom, "id": {"nested": 1}},
                {**custom, "jog": {"encoding": "unknown_mode"}},
                {**custom, "jog": {"touchNote": True}},
                {**custom, "tempoSlider": {"cc": False}},
                {**custom, "malformedLimit": 0},
                {**custom, "malformedLimit": True},
                {**custom, "stormLimitPerSec": 10**12},
                {**custom, "match": {"alsaClientPatterns": "CDJ"}},
                {**custom, "match": {"alsaClientPatterns": [123]}},
                {**custom, "controls": {"x": {"type": "note", "number": 200, "action": "toggle_play"}}},
                {**custom, "controls": {"x": {"type": "note", "number": True, "action": "toggle_play"}}},
                {**custom, "controls": {"x": {"type": "note", "number": 1, "action": "launch_missiles"}}},
            ):
                with self.assertRaises(ValueError):
                    midi.validate_preset(bad)
        finally:
            app.close()

    def test_midi_mapper_cdj400_controls_and_velocity_integration(self):
        midi = feature("midi-controller-mapper")
        app = midi.Feature(self.state / "midi-controls")
        try:
            app.action({"action": "enable"})
            app.update_deck_state({"songLoaded": True, "playing": False, "posNorm": 0.25, "durationSec": 180.0, "speed": 0.5})
            # Transport & buttons verified against Pioneer CDJ-400 manual DRB1452 p.20:
            # Play(90 00), Cue(90 01), Search(90 02/03), TrackSearch(90 04/05), Loop(90 06/07/08),
            # TempoRange(90 10), MT(90 11), JogMode(90 12), LoopDivide(90 16/17), Hold/Sync(90 22),
            # Back(90 32 -> toggle_browse_view), EncoderPush(90 33 -> load_selected_track), Browse(B0 4F)
            app.process_midi_message([0x90, 0x01, 0x7F], now=10.0)
            b1 = app.poll_bridge(now=10.01)
            self.assertTrue(b1["values"]["cueHold"])
            app.process_midi_message([0x90, 0x01, 0x00], now=10.02)
            b2 = app.poll_bridge(now=10.03)
            self.assertFalse(b2["values"]["cueHold"])

            for note, expected in (
                (0x00, "toggle_play"),
                (0x02, "seek_forward"),
                (0x03, "seek_backward"),
                (0x04, "track_next"),
                (0x05, "track_prev"),
                (0x06, "loop_in"),
                (0x07, "loop_out"),
                (0x08, "reloop_exit"),
                (0x10, "cycle_tempo_range"),
                (0x11, "toggle_keylock"),
                (0x16, "loop_halve"),
                (0x17, "loop_double"),
                (0x22, "toggle_sync"),
                (0x32, "toggle_browse_view"),
                (0x33, "load_selected_track"),
            ):
                app.process_midi_message([0x90, note, 0x7F], now=11.0)
                act_batch = app.poll_bridge(now=11.005)
                self.assertEqual([a["action"] for a in act_batch["actions"]], [expected])

            app.process_midi_message([0xB0, 0x4F, 67], now=11.01)  # +3 browse steps
            batch = app.poll_bridge(now=11.02)
            self.assertEqual(batch["actions"], [{"action": "browse_scroll", "steps": 3}])

            # Continuous velocity jog integration across 250ms/350ms intervals (no 0.1s dt truncation or 4ms drift):
            # 90 20 7F touch + B0 10 127 (+63 full fwd) from t=20.0 to t=20.35 (dt=0.35s) -> 64=stop
            app.process_midi_message([0x90, 0x20, 0x7F], now=20.0)
            app.process_midi_message([0xB0, 0x10, 127], now=20.0)
            delta_sec = app.integrate_jog_step(now=20.35)
            self.assertAlmostEqual(delta_sec, 0.63, places=4)  # (63/63) * 0.35s * 1.8 = 0.63s
            app.process_midi_message([0xB0, 0x10, 64], now=20.35)  # 64 = stop
            self.assertEqual(app.jog_velocity, 0)
            self.assertEqual(app.integrate_jog_step(now=20.60), 0.0)
            jog_batch = app.poll_bridge(ack_seq=batch["seq"], ack_apply_ms=1.5, now=20.61)
            self.assertAlmostEqual(jog_batch["values"]["jogMoveTicks"], round(0.63 * 26325.0, 2), places=2)
            self.assertGreater(jog_batch["values"]["setPosNorm"], 0.25)

            # Outer-ring pitch bend (B0 30): stays active past 120ms until B0 30 40 (64 = stop) arrives
            app.process_midi_message([0xB0, 0x30, 96], now=22.0)
            pb_batch = app.poll_bridge(now=22.01)
            self.assertGreater(pb_batch["values"]["nudgeFwd"], 0.3)
            self.assertEqual(pb_batch["values"]["nudgeBwd"], 0.0)
            # 300ms later with no new MIDI message: must NOT auto-zero nudgeFwd
            pb_hold = app.poll_bridge(now=22.31)
            self.assertNotIn("nudgeFwd", pb_hold["values"])
            self.assertEqual(app.pitch_bend_dir, 1)
            # Only zeroes when 64 (stop) arrives
            app.process_midi_message([0xB0, 0x30, 64], now=22.40)
            pb_stop = app.poll_bridge(now=22.41)
            self.assertEqual(pb_stop["values"]["nudgeFwd"], 0.0)
        finally:
            app.close()

    def test_midi_mapper_soft_takeover_and_safety_autodisable(self):
        midi = feature("midi-controller-mapper")
        app = midi.Feature(self.state / "midi-safety")
        try:
            app.action({"action": "enable"})
            app.update_deck_state({"speed": 0.60})
            # First fader message at 102/127 (~0.803) is above 0.60, even though default fader_norm was 0.50:
            # must NOT falsely trigger 'crossed' on initial unseen position!
            app.process_midi_message([0xB0, 0x1D, 102], now=29.0)
            b_init = app.poll_bridge(now=29.01)
            self.assertFalse(app.soft_locked)
            self.assertNotIn("speed", b_init["values"])
            self.assertFalse(b_init["values"]["speedOffsetUp"])
            self.assertTrue(b_init["values"]["speedOffsetDown"])

            app.update_deck_state({"speed": 0.70})
            # Fader at 32/127 (~0.252) swept across 0.70 from 0.803 -> picks up!
            # Let's test unlocking first: set deck speed to 0.70 while fader is at 0.252
            app.soft_locked = False
            app.fader_norm = 32 / 127.0
            app.process_midi_message([0xB0, 0x1D, 33], now=30.0)
            b_wait = app.poll_bridge(now=30.01)
            self.assertFalse(app.soft_locked)
            self.assertNotIn("speed", b_wait["values"])
            self.assertTrue(b_wait["values"]["speedOffsetUp"])
            self.assertFalse(b_wait["values"]["speedOffsetDown"])

            # Sweep fader across 0.70 (90/127 = 0.7087) -> picks up and locks!
            app.process_midi_message([0xB0, 0x1D, 90], now=30.05)
            b_lock = app.poll_bridge(now=30.06)
            self.assertTrue(app.soft_locked)
            self.assertAlmostEqual(b_lock["values"]["speed"], 90 / 127.0, places=4)
            self.assertFalse(b_lock["values"]["speedOffsetUp"])
            self.assertFalse(b_lock["values"]["speedOffsetDown"])

            # External deck speed change immediately unlocks soft takeover (no 350ms blind window)
            app.update_deck_state({"speed": 0.40})
            self.assertFalse(app.soft_locked)
            self.assertTrue(app.speed_offset_down)
            # Moving fader slightly (89/127 = 0.7008, not crossing 0.40) must remain unlocked
            app.process_midi_message([0xB0, 0x1D, 89], now=30.07)
            b_still_locked_out = app.poll_bridge(now=30.08)
            self.assertFalse(app.soft_locked)
            self.assertNotIn("speed", b_still_locked_out["values"])
            self.assertTrue(b_still_locked_out["values"]["speedOffsetDown"])

            # Unrelated ALSA client exit must NOT disable the Mod
            app.connected = True
            app.connected_client = 20
            app.connected_port = 0
            app.connected_name = "Pioneer CDJ-400 [20:0]"
            self.assertFalse(app.simulate_disconnect(client_id=99))
            self.assertTrue(app.status()["enabled"])

            # Connected USB MIDI client hot-unplug auto-disables and flushes zeroed nudge/cue values
            app.process_midi_message([0xB0, 0x30, 96], now=31.0)
            self.assertTrue(app.simulate_disconnect(client_id=20))
            self.assertFalse(app.status()["enabled"])
            self.assertIn("disconnected", app.status()["error"].lower())
            b_flush = app.poll_bridge(now=31.01)
            self.assertFalse(b_flush["enabled"])
            self.assertEqual(b_flush["values"]["nudgeFwd"], 0.0)
            self.assertEqual(b_flush["values"]["cueHold"], False)

            # Malformed MIDI auto-disables after threshold; restart_worker clears error
            app.action({"action": "enable"})
            for _ in range(16):
                app.process_midi_message([0x10, 0x00], now=40.0)
            self.assertFalse(app.status()["enabled"])
            self.assertIn("malformed", app.status()["error"].lower())
            st_restarted = app.action({"action": "restart_worker"})
            self.assertEqual(st_restarted["error"], "")
        finally:
            app.close()


if __name__ == "__main__":
    unittest.main()
