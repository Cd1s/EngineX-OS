#!/usr/bin/env python3
"""Unit tests for check_link_ondemand.wait_ui_pack_ready (fails on fixed-55 s 91c54855)."""

from pathlib import Path
import sys
import tempfile
import types
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import twin  # noqa: F401
except ModuleNotFoundError:
    stub = types.ModuleType("twin")
    stub.WORK = Path("/tmp")
    stub.BOOTSTRAP_JS = "bootstrap"
    stub.evj = lambda *a, **kw: {}
    sys.modules["twin"] = stub
import check_link_ondemand as mod


class FakeClock:
    def __init__(self):
        self.t = 0.0

    def now(self) -> float:
        return self.t

    def sleep(self, dt: float) -> None:
        self.t += dt


class WaitUIPackReadyTest(unittest.TestCase):
    def test_waits_past_55s_while_engine_progresses_then_bootstraps(self):
        with tempfile.TemporaryDirectory() as td:
            sdir = Path(td)
            log = sdir / "logs/engine.log"
            log.parent.mkdir(parents=True)
            log.write_text("booting\n")
            ui = sdir / "data/enginex/ui"
            ui.mkdir(parents=True)
            (ui / "boot-token").write_text("tok-1\n")

            clk = FakeClock()
            boot_calls = []

            def fake_sleep(dt: float):
                clk.sleep(dt)
                # Engine keeps making progress across 55 s and finishes loading UI packs at 68 s.
                if clk.t < 68.0:
                    with open(log, "a") as f:
                        f.write(f"tick {clk.t:.1f}\n")
                elif not (ui / "boot-ready").is_file():
                    (ui / "boot-ready").write_text("tok-1\n")

            def fake_evj(side, js, name="boot", timeout=6.0):
                boot_calls.append((side, name, clk.t))
                return {"role": "left", "side": side}

            res = mod.wait_ui_pack_ready(
                "A",
                sdir=sdir,
                stall_timeout=30.0,
                max_wait=120.0,
                poll_interval=2.0,
                evj_fn=fake_evj,
                alive_fn=lambda: True,
                now_fn=clk.now,
                sleep_fn=fake_sleep,
            )
            self.assertEqual(res["role"], "left")
            self.assertGreater(clk.t, 68.0)
            self.assertEqual([c[1] for c in boot_calls], ["boot", "boot2"])

    def test_boot_ready_marker_triggers_without_scheduler_manager_log(self):
        with tempfile.TemporaryDirectory() as td:
            sdir = Path(td)
            (sdir / "logs").mkdir(parents=True)
            (sdir / "logs/engine.log").write_text("qml ui loaded\n")
            ui = sdir / "data/enginex/ui"
            ui.mkdir(parents=True)
            (ui / "boot-token").write_text("tok-42\n")
            (ui / "boot-ready").write_text("tok-42\n")

            clk = FakeClock()
            res = mod.wait_ui_pack_ready(
                "B",
                sdir=sdir,
                stall_timeout=10.0,
                max_wait=30.0,
                poll_interval=0.5,
                evj_fn=lambda side, js, name="boot", timeout=6.0: {"role": "right"},
                alive_fn=lambda: True,
                now_fn=clk.now,
                sleep_fn=clk.sleep,
            )
            self.assertEqual(res["role"], "right")
            self.assertLess(clk.t, 5.0)

    def test_fails_fast_when_engine_exits(self):
        with tempfile.TemporaryDirectory() as td:
            sdir = Path(td)
            (sdir / "logs").mkdir(parents=True)
            (sdir / "logs/engine.log").write_text("fatal crash\n")
            clk = FakeClock()
            with self.assertRaises(RuntimeError) as ctx:
                mod.wait_ui_pack_ready(
                    "A",
                    sdir=sdir,
                    stall_timeout=60.0,
                    max_wait=180.0,
                    poll_interval=1.0,
                    evj_fn=lambda *a, **kw: {},
                    alive_fn=lambda: False,
                    now_fn=clk.now,
                    sleep_fn=clk.sleep,
                )
            self.assertEqual(clk.t, 0.0)
            self.assertIn("exited", str(ctx.exception))

    def test_fails_on_stall_without_waiting_max_wait(self):
        with tempfile.TemporaryDirectory() as td:
            sdir = Path(td)
            (sdir / "logs").mkdir(parents=True)
            (sdir / "logs/engine.log").write_text("stuck here\n")
            clk = FakeClock()
            with self.assertRaises(TimeoutError) as ctx:
                mod.wait_ui_pack_ready(
                    "A",
                    sdir=sdir,
                    stall_timeout=12.0,
                    max_wait=180.0,
                    poll_interval=2.0,
                    evj_fn=lambda *a, **kw: {},
                    alive_fn=lambda: True,
                    now_fn=clk.now,
                    sleep_fn=clk.sleep,
                )
            self.assertGreaterEqual(clk.t, 12.0)
            self.assertLess(clk.t, 20.0)
            self.assertIn("stalled", str(ctx.exception))


    def test_proc_stat_rejects_zombie_and_dead_states(self):
        self.assertTrue(mod._proc_stat_is_alive("1234 (Engine) S 1 1 1 0"))
        self.assertTrue(mod._proc_stat_is_alive("1234 (nsenter (child)) R 1 1 1 0"))
        self.assertFalse(mod._proc_stat_is_alive("1234 (nsenter) Z 1 1 1 0"))
        self.assertFalse(mod._proc_stat_is_alive("1234 (Engine) X 1 1 1 0"))


if __name__ == "__main__":
    unittest.main()
