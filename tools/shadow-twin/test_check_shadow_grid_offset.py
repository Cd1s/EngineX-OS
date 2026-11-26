#!/usr/bin/env python3
"""Unit tests for check_shadow_grid_offset.py validation gates."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_shadow_grid_offset as sgo


SR = 44100.0
SPB = SR * 60.0 / 128.0
GRIDS = ((SR, SPB, 0.0), (SR, SPB, 0.0), (SR, SPB, 0.0), (SR, SPB, 0.0))


def make_bursts(
    n=40,
    *,
    copy_err_ms=1.0,
    stock_phase_ms=0.0,
    a1_play=True,
    b2_play=True,
    a1_master=True,
    b2_master=False,
    a1_sync="Beat",
    b2_sync="Beat",
    b2_aligned=True,
    a1_cur_bpm=128.0,
    b2_cur_bpm=128.0,
    b2_orig_bpm=128.0,
    a2_play=False,
    b1_play=False,
    b_time_skew_ms=0,
):
    la, lb = [], []
    base_ms = 1_700_000_000_000
    for i in range(n):
        t = base_ms + i * 33
        a1_ms = 5000.0 + i * 33.0
        # stock phase = A1 - B2 => B2 = A1 - stock_phase_ms
        b2_ms = a1_ms - stock_phase_ms
        a2_ms = b2_ms + copy_err_ms
        b1_ms = a1_ms + copy_err_ms
        la.append(
            {
                "now": t,
                "d1": {
                    "play": a1_play,
                    "master": a1_master,
                    "sync": a1_sync,
                    "aligned": True,
                    "sr": SR,
                    "bpm": 128.0,
                    "curBpm": a1_cur_bpm,
                    "tp": a1_ms * SR / 1000.0,
                    "wf": a1_ms * SR / 1000.0,
                    "pr": {"at": t, "lineMs": a1_ms},
                },
                "d2": {
                    "play": a2_play,
                    "master": False,
                    "sync": "Off",
                    "aligned": False,
                    "sr": SR,
                    "bpm": b2_orig_bpm,
                    "curBpm": b2_cur_bpm,
                    "tp": a2_ms * SR / 1000.0,
                    "wf": a2_ms * SR / 1000.0,
                    "pr": None,
                },
            }
        )
        tb = t + b_time_skew_ms
        rate_a = a1_cur_bpm / 128.0
        rate_b = b2_cur_bpm / max(1.0, b2_orig_bpm)
        lb.append(
            {
                "now": tb,
                "d1": {
                    "play": b1_play,
                    "master": False,
                    "sync": "Off",
                    "aligned": False,
                    "sr": SR,
                    "bpm": 128.0,
                    "curBpm": a1_cur_bpm,
                    "tp": (b1_ms + b_time_skew_ms * rate_a) * SR / 1000.0,
                    "wf": (b1_ms + b_time_skew_ms * rate_a) * SR / 1000.0,
                    "pr": None,
                },
                "d2": {
                    "play": b2_play,
                    "master": b2_master,
                    "sync": b2_sync,
                    "aligned": b2_aligned,
                    "sr": SR,
                    "bpm": b2_orig_bpm,
                    "curBpm": b2_cur_bpm,
                    "tp": (b2_ms + b_time_skew_ms * rate_b) * SR / 1000.0,
                    "wf": (b2_ms + b_time_skew_ms * rate_b) * SR / 1000.0,
                    "pr": {"at": tb, "lineMs": b2_ms + b_time_skew_ms * rate_b},
                },
            }
        )
    return la, lb


class CheckShadowGridOffsetGatesTest(unittest.TestCase):
    def test_clean_synced_burst_passes_and_reports_stock_phase_separately(self):
        la, lb = make_bursts(n=40, copy_err_ms=1.2, stock_phase_ms=0.4)
        res = sgo.evaluate_burst("clean", la, lb, GRIDS, min_paired=30)
        self.assertAlmostEqual(res["b1_p99"], 1.2, delta=0.05)
        self.assertAlmostEqual(res["a2_p99"], 1.2, delta=0.05)
        self.assertAlmostEqual(res["stock_mean"], 0.4, delta=0.05)
        self.assertAlmostEqual(res["stock_p99"], 0.4, delta=0.05)
        self.assertLessEqual(res["a_scr_p99"], 3.0)
        self.assertLessEqual(res["b_scr_p99"], 3.0)

    def test_fails_when_sync_or_lead_not_engaged_in_window(self):
        for kw in (
            {"a1_sync": "Off"},
            {"b2_sync": "Off"},
            {"a1_master": False},
            {"b2_master": True},
            {"b2_aligned": False},
            {"a1_play": False},
            {"b2_play": False},
        ):
            la, lb = make_bursts(n=40, **kw)
            with self.subTest(kw=kw), self.assertRaises(AssertionError):
                sgo.evaluate_burst("bad-sync", la, lb, GRIDS, min_paired=30)

    def test_fails_when_effective_bpms_differ(self):
        la, lb = make_bursts(n=40, a1_cur_bpm=128.0, b2_cur_bpm=132.0, b2_orig_bpm=132.0)
        with self.assertRaisesRegex(AssertionError, "effective BPMs diverged"):
            sgo.evaluate_burst("bad-bpm", la, lb, GRIDS, min_paired=30)

    def test_fails_with_too_few_paired_frames(self):
        la, lb = make_bursts(n=40, b_time_skew_ms=4000)
        with self.assertRaisesRegex(AssertionError, "too few paired"):
            sgo.evaluate_burst("few-paired", la, lb, GRIDS, min_paired=20)

    def test_out_of_phase_fixture_fails_with_distinct_stock_phase_error(self):
        # Reproduces the Batam native case: copy follow p99 is 1.5 ms, but stock
        # source-vs-source phase is +18.6 ms (screens ~20 ms apart despite aligned=True).
        la, lb = make_bursts(n=40, copy_err_ms=1.5, stock_phase_ms=18.6)
        with self.assertRaisesRegex(AssertionError, r"FIXTURE FAIL.*stock Engine Sync"):
            sgo.evaluate_burst("out-of-phase-native", la, lb, GRIDS, min_paired=30)

    def test_screen_spacing_gate_fails_when_screen_p99_exceeds_limit(self):
        # Copy p99 (2.2 ms) and stock phase (1.5 ms) each pass their own <=3.0 ms
        # checks, but combine on Unit B's screen to 3.7 ms > 3.0 ms screen gate.
        la, lb = make_bursts(n=40, copy_err_ms=2.2, stock_phase_ms=1.5)
        with self.assertRaisesRegex(AssertionError, "SCREEN FAIL"):
            sgo.evaluate_burst("screen-spacing", la, lb, GRIDS, min_paired=30, screen_limit_ms=3.0)

    def test_ensure_sync_a_lead_raises_on_timeout(self):
        unsynced = {"now": 1000, "play": True, "master": False, "sync": "Off", "bpm": 128.0, "curBpm": 128.0, "aligned": False, "wf": 0.0}
        with (
            mock.patch.object(sgo, "deck_sync_info", return_value=unsynced),
            mock.patch.object(sgo.twin, "ev", return_value="ok"),
            mock.patch.object(sgo.time, "sleep", return_value=None),
            mock.patch.object(sgo.subprocess, "run", return_value=None),
            self.assertRaisesRegex(AssertionError, "FIXTURE FAIL"),
        ):
            sgo.ensure_sync_a_lead(GRIDS[0], GRIDS[1])


if __name__ == "__main__":
    unittest.main()
