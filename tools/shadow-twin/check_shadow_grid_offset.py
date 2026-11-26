#!/usr/bin/env python3
"""Check cause-1 shadow grid offset while both units play synced at the same BPM.

Fails on d34e420d (copy follow error beyond +/-3 ms p99) and passes with the
sender waveformPositionItem + receiver UDP half-RTT + paused-follow deadband fix.
"""

from __future__ import annotations

import json
import math
import os
import shutil
import sqlite3
import statistics
import struct
import subprocess
import sys
import time
import zlib
from contextlib import closing
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import twin

P99_LIMIT_MS = float(os.environ.get("SGO_P99_LIMIT_MS", "3.0"))
SCREEN_P99_LIMIT_MS = float(os.environ.get("SGO_SCREEN_P99_LIMIT_MS", "3.0"))
STOCK_PHASE_LIMIT_MS = float(os.environ.get("SGO_STOCK_PHASE_LIMIT_MS", "3.0"))
BPM_TOL = float(os.environ.get("SGO_BPM_TOL", "0.05"))
PAUSED_LIMIT_MS = 0.05
COST_WINDOW_S = float(os.environ.get("SGO_COST_WINDOW_S", "20.0"))
N_SAMPLES = int(os.environ.get("SGO_SAMPLES", "150"))
MIN_PAIRED_SAMPLES = int(os.environ.get("SGO_MIN_PAIRED", str(max(20, int(N_SAMPLES * 0.7)))))


def wait_for(fn, timeout=60, label="condition"):
    dl, last = time.monotonic() + timeout, None
    while time.monotonic() < dl:
        last = fn()
        if last:
            return last
        time.sleep(0.25)
    raise AssertionError(f"timed out waiting for {label} (last={last})")


def wait_analyzed(side, deck, bpm=None, timeout=60):
    bpm_chk = f" && Math.abs((root.rtTr('/Engine/Deck{deck}/OriginalBPM').unnormalized||0)-{bpm})<0.2" if bpm else ""
    js = f'JSON.stringify(!!root.rtTr("/Engine/Deck{deck}/Track/SongAnalyzed").state{bpm_chk})'
    wait_for(lambda: twin.evj(side, js), timeout, f"{side}{deck} analyzed" + (f" ({bpm})" if bpm else ""))


def shadow_ver(side):
    out = subprocess.check_output(
        ["ip", "netns", "exec", twin.SIDE_CFG[side]["netns"], "curl", "-sS", "http://127.0.0.1:8080/v1/enginex/shadow"],
        text=True,
    )
    return int(json.loads(out).get("ver", 0))


def find_engine_pid(side):
    marker = f"{twin.WORK.resolve()}/{side}/"
    for p in Path("/proc").iterdir():
        if not p.name.isdigit():
            continue
        try:
            argv0 = (p / "cmdline").read_bytes().split(b"\x00", 1)[0].decode("utf-8", "ignore")
            if (argv0 == "/usr/Engine/Engine" or argv0.endswith("/Engine")) and marker in (p / "mountinfo").read_text(errors="ignore"):
                return int(p.name)
        except OSError:
            continue
    return None


def read_gui_thread_cpu_ticks(pid):
    st = Path(f"/proc/{pid}/task/{pid}/stat").read_text()
    f = st[st.rfind(")") + 2 :].split()
    return int(f[11]) + int(f[12])


def parse_grid_b0(side, filename):
    db = twin.WORK / side / "data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db"
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as con:
        row = con.execute(
            "SELECT p.beatData FROM Track t JOIN PerformanceData p ON t.id=p.trackId WHERE t.filename=? ORDER BY t.id DESC LIMIT 1",
            (filename,),
        ).fetchone()
    assert row and row[0], f"missing beatData for {side}/{filename}"
    raw = zlib.decompress(row[0][4:])
    sr = struct.unpack_from(">d", raw, 0)[0]
    off, grids = 17, []
    for _ in range(2):
        n = struct.unpack_from(">Q", raw, off)[0]
        off += 8
        grids.append([struct.unpack_from("<dqII", raw, off + i * 24) for i in range(n)])
        off += n * 24
    m0, m1 = (grids[1] if len(grids[1]) >= 2 else grids[0])[:2]
    spb = (m1[0] - m0[0]) / (m1[1] - m0[1])
    return sr, spb, m0[0] - m0[1] * spb


def mod_beat_ms(b_diff, cur_bpm):
    return (((b_diff + 0.5) % 1.0) - 0.5) * (60000.0 / max(1.0, cur_bpm))


def pct(vals, p):
    if not vals:
        return float("nan")
    s = sorted(vals)
    return s[min(len(s) - 1, max(0, math.ceil(len(s) * p / 100.0) - 1))]


def fmt_stats(vals):
    if not vals:
        return "n=0"
    absv = [abs(v) for v in vals]
    return (
        f"n={len(vals):3d} mean={statistics.mean(vals):+6.2f}ms med={statistics.median(vals):+6.2f}ms "
        f"p99(|e|)={pct(absv, 99):5.2f}ms max(|e|)={max(absv):5.2f}ms [{min(vals):+6.2f}, {max(vals):+6.2f}]"
    )


def deck_sync_info(side, deck):
    return twin.evj(
        side,
        f"""(() => {{
            const g = p => root.rtTr("/Engine/Deck{deck}" + p) || {{}};
            const wf = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem({deck}) : null;
            const pm = PlanckGUI && PlanckGUI.views && PlanckGUI.views.getPhaseMeter ? PlanckGUI.views.getPhaseMeter({deck - 1}) : null;
            return JSON.stringify({{
                now: Date.now(), play: !!g("/PlayState").state, master: !!g("/DeckIsMaster").state,
                sync: g("/SyncMode").string || "Off", bpm: g("/OriginalBPM").unnormalized || 0,
                curBpm: g("/CurrentBPM").unnormalized || 0, aligned: pm ? !!pm.isAligned : false,
                wf: wf ? wf.currentPosition : null,
            }});
        }})()""",
    )


def sync_state_problems(a1, b2, bpm_tol=BPM_TOL):
    checks = [
        (a1.get("play"), "A1 not playing"),
        (b2.get("play"), "B2 not playing"),
        (a1.get("master"), "A1 not LEAD/master"),
        (not b2.get("master"), "B2 unexpectedly LEAD/master"),
        (a1.get("sync", "Off") != "Off", "A1 Sync Off"),
        (b2.get("sync", "Off") != "Off", "B2 Sync Off"),
        (b2.get("aligned"), "B2 not aligned"),
    ]
    errs = [msg for ok, msg in checks if not ok]
    ba, bb = float(a1.get("curBpm") or 0), float(b2.get("curBpm") or 0)
    if ba <= 0 or bb <= 0 or abs(ba - bb) > bpm_tol:
        errs.append(f"effective BPM mismatch ({ba:.2f} vs {bb:.2f}, tol={bpm_tol})")
    return errs


def snapshot_stock_phase_ms(a1, b2, grid_a, grid_b):
    if a1.get("wf") is None or b2.get("wf") is None or float(a1.get("curBpm") or 0) <= 0:
        return None
    (_, spb_a, b0_a), (sr_b, spb_b, b0_b) = grid_a, grid_b
    cur = float(a1["curBpm"])
    rate_b = float(b2.get("curBpm") or cur) / max(1.0, float(b2.get("bpm") or cur))
    b2_wf_at_a = b2["wf"] - ((float(b2.get("now", 0)) - float(a1.get("now", 0))) / 1000.0) * rate_b * sr_b
    return mod_beat_ms((a1["wf"] - b0_a) / spb_a - (b2_wf_at_a - b0_b) / spb_b, cur)


def ensure_sync_a_lead(grid_a, grid_b, bpm_tol=BPM_TOL, stock_phase_limit_ms=STOCK_PHASE_LIMIT_MS):
    """Enable Sync on both decks, lock A1=LEAD & B2=aligned follower within stock phase, or raise."""
    last_a1, last_b2, last_phase = {}, {}, None
    for attempt in range(2):
        for side, deck in (("A", 1), ("B", 2)):
            if deck_sync_info(side, deck)["sync"] == "Off":
                twin.ev(side, f'root.rtPress("/Engine/Deck{deck}/Sync"); "sync{side}"')
                time.sleep(0.4)
        if not deck_sync_info("A", 1)["master"]:
            twin.ev("A", 'root.rtPress("/Engine/Deck1/RequestMaster"); "reqMasterA"')
            time.sleep(0.5)
        for _ in range(48):
            time.sleep(0.25)
            last_a1, last_b2 = deck_sync_info("A", 1), deck_sync_info("B", 2)
            last_phase = snapshot_stock_phase_ms(last_a1, last_b2, grid_a, grid_b)
            if not sync_state_problems(last_a1, last_b2, bpm_tol) and last_phase is not None and abs(last_phase) <= stock_phase_limit_ms:
                return last_a1, last_b2, last_phase
        if attempt == 0:
            twin.ev("B", 'root.rtTr("/Engine/Deck2/Play").state = false; "stopB"')
            time.sleep(0.3)
            twin.ev("B", 'root.rtTr("/Engine/Deck2/Track/SetSongPosition").value = 0.05; "seekB"')
            time.sleep(0.4)
            subprocess.run(["python3", str(Path(__file__).resolve().parent / "twin.py"), "play", "B"], check=True)
    raise AssertionError(
        f"FIXTURE FAIL: Sync/LEAD did not lock within stock phase <= {stock_phase_limit_ms:.2f}ms "
        f"(problems={sync_state_problems(last_a1, last_b2, bpm_tol)}, phase={last_phase}, A1={last_a1}, B2={last_b2})"
    )


def arm_burst(side, n_samples=150):
    """Sample local & copy WaveFormPositionItem on beforeSynchronizing (~30 Hz)."""
    return twin.ev(
        side,
        f"""(() => {{
            const bag = root.scShadowTextAt;
            bag._sgoChk = [];
            if (bag._sgoChkT) {{ bag._sgoChkT.stop(); bag._sgoChkT.destroy(); bag._sgoChkT = null; }}
            if (bag._sgoWin && bag._sgoCb) {{ try {{ bag._sgoWin.beforeSynchronizing.disconnect(bag._sgoCb); }} catch(e) {{}} }}
            const w1 = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem(1) : null;
            const w2 = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem(2) : null;
            const pm1 = PlanckGUI && PlanckGUI.views && PlanckGUI.views.getPhaseMeter ? PlanckGUI.views.getPhaseMeter(0) : null;
            const pm2 = PlanckGUI && PlanckGUI.views && PlanckGUI.views.getPhaseMeter ? PlanckGUI.views.getPhaseMeter(1) : null;
            const take = () => {{
                const wf1 = w1 ? w1.currentPosition : null, wf2 = w2 ? w2.currentPosition : null, now = Date.now();
                const rd = (d, wfVal, pm) => {{
                    const g = p => root.rtTr("/Engine/Deck" + d + p) || {{}};
                    const pr = root.scShadowPrev && root.scShadowPrev[d];
                    return {{
                        play: !!g("/PlayState").state, master: !!g("/DeckIsMaster").state,
                        sync: g("/SyncMode").string || "Off", aligned: pm ? !!pm.isAligned : false,
                        bpm: g("/OriginalBPM").unnormalized || 0, curBpm: g("/CurrentBPM").unnormalized || 0,
                        wf: wfVal, pr: pr ? {{ at: pr.at, lineMs: pr.lineMs }} : null,
                    }};
                }};
                bag._sgoChk.push({{ now, d1: rd(1, wf1, pm1), d2: rd(2, wf2, pm2) }});
            }};
            const win = (typeof planckQuickView !== "undefined" && planckQuickView && planckQuickView.beforeSynchronizing) ? planckQuickView : null;
            if (win) {{
                let k = 0; bag._sgoWin = win;
                bag._sgoCb = () => {{ if (bag._sgoChk.length >= {n_samples}) {{ try {{ win.beforeSynchronizing.disconnect(bag._sgoCb); }} catch(e) {{}} return; }} if ((++k) % 2 === 0) take(); }};
                win.beforeSynchronizing.connect(bag._sgoCb);
                return "armed:beforeSynchronizing";
            }}
            bag._sgoChkT = Qt.createQmlObject('import QtQml; Timer {{ interval: 40; repeat: true; running: true }}', root);
            bag._sgoChkT.triggered.connect(() => {{ if (bag._sgoChk.length >= {n_samples}) {{ bag._sgoChkT.stop(); return; }} take(); }});
            return "armed:timer";
        }})()""",
    )


def read_burst(side, n_samples=150):
    wait_for(
        lambda: twin.evj(side, "JSON.stringify(root.scShadowTextAt._sgoChk ? root.scShadowTextAt._sgoChk.length : 0)") >= n_samples,
        30,
        f"{side} burst",
    )
    return twin.evj(side, "JSON.stringify(root.scShadowTextAt._sgoChk)")


def _screen_series(rows, loc_k, cpy_k, g_loc, g_cpy):
    (sr_l, spb_l, b0_l), (_, spb_c, b0_c) = g_loc, g_cpy
    scr, line_wf = [], []
    for r in rows:
        dl, dc = r[loc_k], r[cpy_k]
        if dl["wf"] is None or dc["wf"] is None or not dl.get("pr"):
            continue
        cur, rate = dl["curBpm"], dl["curBpm"] / max(1.0, dl["bpm"])
        scr.append(mod_beat_ms((dc["wf"] - b0_c) / spb_c - (dl["wf"] - b0_l) / spb_l, cur))
        line_wf.append(dl["pr"]["lineMs"] + (r["now"] - dl["pr"]["at"]) * rate - dl["wf"] * 1000.0 / sr_l)
    return scr, line_wf


def evaluate_burst(
    label, la, lb, grids, *,
    p99_limit_ms=P99_LIMIT_MS, screen_limit_ms=SCREEN_P99_LIMIT_MS,
    stock_phase_limit_ms=STOCK_PHASE_LIMIT_MS, bpm_tol=BPM_TOL,
    min_paired=MIN_PAIRED_SAMPLES, enforce=True,
):
    """Pure validator/evaluator for one dual-screen burst."""
    g_a, g_b, g_b1, g_a2 = grids
    (sr_a, spb_a, b0_a), (sr_b, spb_b, b0_b) = g_a, g_b
    sr_b1, sr_a2 = g_b1[0], g_a2[0]
    if len(la) < min_paired or len(lb) < min_paired:
        raise AssertionError(f"{label}: too few raw frames (A={len(la)}, B={len(lb)}, min={min_paired})")

    a2_play = sum(1 for r in la if r["d2"].get("play"))
    b1_play = sum(1 for r in lb if r["d1"].get("play"))
    if a2_play or b1_play:
        raise AssertionError(f"{label}: shadow copy played! A2={a2_play}/{len(la)} B1={b1_play}/{len(lb)}")

    bad_a = [i for i, r in enumerate(la) if not (r["d1"].get("play") and r["d1"].get("master") and r["d1"].get("sync", "Off") != "Off" and (r["d1"].get("curBpm") or 0) > 0)]
    bad_b = [i for i, r in enumerate(lb) if not (r["d2"].get("play") and not r["d2"].get("master") and r["d2"].get("sync", "Off") != "Off" and r["d2"].get("aligned") and (r["d2"].get("curBpm") or 0) > 0)]
    if bad_a or bad_b:
        raise AssertionError(
            f"{label}: Sync/LEAD/play dropped during burst (bad A1={len(bad_a)}/{len(la)} ex={la[bad_a[0]]['d1'] if bad_a else None}; "
            f"bad B2={len(bad_b)}/{len(lb)} ex={lb[bad_b[0]]['d2'] if bad_b else None})"
        )

    a_scr, line_wf_a = _screen_series(la, "d1", "d2", g_a, g_a2)
    b_scr, line_wf_b = _screen_series(lb, "d2", "d1", g_b, g_b1)
    b1_err, a2_err, b1_net, a2_net, sync_wf, bpm_diffs = [], [], [], [], [], []
    for ra in la:
        rb = min(lb, key=lambda x: abs(x["now"] - ra["now"]))
        dt = rb["now"] - ra["now"]
        a1, a2, b1, b2 = ra["d1"], ra["d2"], rb["d1"], rb["d2"]
        if abs(dt) > 25 or None in (a1["wf"], a2["wf"], b1["wf"], b2["wf"]) or not a1.get("pr") or not b2.get("pr"):
            continue
        bpm_diff = abs(a1["curBpm"] - b2["curBpm"])
        if bpm_diff > bpm_tol:
            raise AssertionError(f"{label}: effective BPMs diverged ({a1['curBpm']:.2f} vs {b2['curBpm']:.2f}, tol={bpm_tol})")
        bpm_diffs.append(bpm_diff)
        rate_a, rate_b = a1["curBpm"] / max(1.0, a1["bpm"]), b2["curBpm"] / max(1.0, b2["bpm"])
        b1_wf, a2_wf = b1["wf"] * 1000.0 / sr_b1, a2["wf"] * 1000.0 / sr_a2
        b1_err.append(b1_wf - (a1["wf"] * 1000.0 / sr_a + dt * rate_a))
        a2_err.append(a2_wf - (b2["wf"] * 1000.0 / sr_b - dt * rate_b))
        b1_net.append(b1_wf - (a1["pr"]["lineMs"] + (rb["now"] - a1["pr"]["at"]) * rate_a))
        a2_net.append(a2_wf - (b2["pr"]["lineMs"] + (ra["now"] - b2["pr"]["at"]) * rate_b))
        sync_wf.append(mod_beat_ms((a1["wf"] - b0_a) / spb_a - (b2["wf"] - (dt / 1000.0) * rate_b * sr_b - b0_b) / spb_b, a1["curBpm"]))

    if min(len(a_scr), len(b_scr), len(b1_err), len(a2_err), len(sync_wf)) < min_paired:
        raise AssertionError(f"{label}: too few paired samples (a_scr={len(a_scr)}, b_scr={len(b_scr)}, paired={len(b1_err)}, min={min_paired})")

    a_scr_mean, b_scr_mean = statistics.mean(a_scr), statistics.mean(b_scr)
    res = {
        "label": label, "paired": len(b1_err), "max_bpm_diff": max(bpm_diffs),
        "a_scr": a_scr, "b_scr": b_scr, "b1_err": b1_err, "a2_err": a2_err, "sync_wf": sync_wf,
        "line_wf_a": line_wf_a, "line_wf_b": line_wf_b, "b1_net": b1_net, "a2_net": a2_net,
        "b1_p99": pct([abs(v) for v in b1_err], 99), "a2_p99": pct([abs(v) for v in a2_err], 99),
        "a_scr_p99": pct([abs(v) for v in a_scr], 99), "b_scr_p99": pct([abs(v) for v in b_scr], 99),
        "stock_mean": statistics.mean(sync_wf), "stock_p99": pct([abs(v) for v in sync_wf], 99),
        "a_lead": "Deck 1 (local)" if a_scr_mean < 0 else "Deck 2 (copy)",
        "b_lead": "Deck 2 (local)" if b_scr_mean < 0 else "Deck 1 (copy)",
    }
    if enforce:
        if res["stock_p99"] > stock_phase_limit_ms:
            raise AssertionError(
                f"FIXTURE FAIL ({label}): stock Engine Sync source-vs-source phase p99={res['stock_p99']:.2f}ms "
                f"(mean={res['stock_mean']:+.2f}ms) exceeds {stock_phase_limit_ms:.2f}ms "
                f"(copy follow p99 B1={res['b1_p99']:.2f}ms A2={res['a2_p99']:.2f}ms)"
            )
        if max(res["b1_p99"], res["a2_p99"]) > p99_limit_ms:
            raise AssertionError(
                f"SHADOW FAIL ({label}): copy/source follow p99 B1={res['b1_p99']:.2f}ms A2={res['a2_p99']:.2f}ms "
                f"exceeds +/-{p99_limit_ms:.2f}ms (stock phase p99={res['stock_p99']:.2f}ms)"
            )
        if max(res["a_scr_p99"], res["b_scr_p99"]) > screen_limit_ms:
            raise AssertionError(
                f"SCREEN FAIL ({label}): per-screen grid spacing p99 A={res['a_scr_p99']:.2f}ms B={res['b_scr_p99']:.2f}ms "
                f"exceeds +/-{screen_limit_ms:.2f}ms (copy p99 B1={res['b1_p99']:.2f}/A2={res['a2_p99']:.2f}ms, stock phase p99={res['stock_p99']:.2f}ms)"
            )
    return res


def run_synced_case(label, fn_a, fn_b, *, load_b_bpm=None, measure_cost=False):
    """Shared pause -> optional reload B2 -> seek -> Sync ON -> play -> lock -> burst flow."""
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state = false; "stopA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Play").state = false; "stopB"')
    time.sleep(0.4)
    if load_b_bpm is not None:
        twin.ev("B", f'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/{fn_b}"; "loadB"')
        wait_analyzed("B", 2, load_b_bpm)
        wait_analyzed("A", 2, load_b_bpm)
    grids = (parse_grid_b0("A", fn_a), parse_grid_b0("B", fn_b), parse_grid_b0("B", fn_a), parse_grid_b0("A", fn_b))
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Track/SetSongPosition").value = 0.05; "seekA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Track/SetSongPosition").value = 0.05; "seekB"')
    time.sleep(0.6)
    for side, deck in (("A", 1), ("B", 2)):
        if deck_sync_info(side, deck)["sync"] == "Off":
            twin.ev(side, f'root.rtPress("/Engine/Deck{deck}/Sync"); "sync{side}"')
            time.sleep(0.3)
    subprocess.run(["python3", str(Path(__file__).resolve().parent / "twin.py"), "play", "A"], check=True)
    wait_for(lambda: deck_sync_info("A", 1)["master"] and deck_sync_info("A", 1)["play"], 10, f"A1 LEAD ({label})")
    subprocess.run(["python3", str(Path(__file__).resolve().parent / "twin.py"), "play", "B"], check=True)
    a1, b2, phase = ensure_sync_a_lead(grids[0], grids[1])
    print(f"Synced ({label}): phase={phase:+.2f}ms | A1={a1} | B2={b2}", flush=True)
    time.sleep(2.5)

    mode_a, mode_b = arm_burst("A", N_SAMPLES), arm_burst("B", N_SAMPLES)
    la, lb = read_burst("A", N_SAMPLES), read_burst("B", N_SAMPLES)
    dt_a = [la[i]["now"] - la[i - 1]["now"] for i in range(1, len(la))]
    dt_b = [lb[i]["now"] - lb[i - 1]["now"] for i in range(1, len(lb))]
    r = evaluate_burst(label, la, lb, grids, enforce=False)
    print(f"\n==================== {label} ({mode_a}/{mode_b}) ====================", flush=True)
    print(f"  Cadence: A={statistics.mean(dt_a):.1f}ms (max={max(dt_a):.0f}ms) | B={statistics.mean(dt_b):.1f}ms (max={max(dt_b):.0f}ms) | paired={r['paired']}/{N_SAMPLES} | max|dBPM|={r['max_bpm_diff']:.3f}")
    print(f"  Copy PlayState=false & Sync/LEAD engaged on all {len(la)}/{len(lb)} frames (OK)")
    print(f"  Stock source-vs-source phase (A1 - B2):   {fmt_stats(r['sync_wf'])}")
    print(f"  B1 copy wf - A1 src wf:                   {fmt_stats(r['b1_err'])}")
    print(f"  A2 copy wf - B2 src wf:                   {fmt_stats(r['a2_err'])}")
    print(f"  UNIT A screen (Deck2 copy - Deck1 local): {fmt_stats(r['a_scr'])} -> ahead: {r['a_lead']}")
    print(f"  UNIT B screen (Deck1 copy - Deck2 local): {fmt_stats(r['b_scr'])} -> ahead: {r['b_lead']}")
    print(f"  Sender lineMs - src wf (A1 / B2):         {statistics.mean(r['line_wf_a']):+6.2f}ms / {statistics.mean(r['line_wf_b']):+6.2f}ms")
    print(f"  Receiver Copy wf - lineMs (B1 / A2):      {statistics.mean(r['b1_net']):+6.2f}ms / {statistics.mean(r['a2_net']):+6.2f}ms", flush=True)
    r = evaluate_burst(label, la, lb, grids, enforce=True)
    cost = measure_steady_cost(COST_WINDOW_S) if measure_cost else None
    return r, cost


def measure_steady_cost(window_s=20.0):
    pid_a, pid_b = find_engine_pid("A"), find_engine_pid("B")
    assert pid_a and pid_b, f"could not locate Engine PIDs: A={pid_a} B={pid_b}"
    clk = os.sysconf(os.sysconf_names["SC_CLK_TCK"])
    va0, vb0 = shadow_ver("A"), shadow_ver("B")
    ca0, cb0, t0 = read_gui_thread_cpu_ticks(pid_a), read_gui_thread_cpu_ticks(pid_b), time.monotonic()
    time.sleep(window_s)
    dt = time.monotonic() - t0
    ca1, cb1 = read_gui_thread_cpu_ticks(pid_a), read_gui_thread_cpu_ticks(pid_b)
    va1, vb1 = shadow_ver("A"), shadow_ver("B")
    out = {
        "corr_a_min": (va1 - va0) / dt * 60.0, "corr_b_min": (vb1 - vb0) / dt * 60.0,
        "gui_cpu_a": (ca1 - ca0) / clk / dt * 100.0, "gui_cpu_b": (cb1 - cb0) / clk / dt * 100.0,
    }
    print(f"\n==================== STEADY-PLAY COST ({dt:.1f}s window, PIDs A={pid_a} B={pid_b}) ====================", flush=True)
    print(f"  Unit A (Deck 2 copy): ver {va0}->{va1} ({out['corr_a_min']:.1f}/min) | Engine GUI CPU={out['gui_cpu_a']:.2f}%")
    print(f"  Unit B (Deck 1 copy): ver {vb0}->{vb1} ({out['corr_b_min']:.1f}/min) | Engine GUI CPU={out['gui_cpu_b']:.2f}%", flush=True)
    return out


def check_paused_alignment(fn_a, fn_b):
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state = false; "pauseA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Play").state = false; "pauseB"')
    time.sleep(1.2)
    sr_a, sr_b, sr_b1, sr_a2 = (parse_grid_b0(s, f)[0] for s, f in (("A", fn_a), ("B", fn_b), ("B", fn_a), ("A", fn_b)))
    snap_js = """(() => {
        const rd = d => { const g = p => root.rtTr("/Engine/Deck"+d+p)||{}; const wf = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem(d) : null;
            return { play: !!g("/PlayState").state, tp: g("/Track/TrackPosition").unnormalized||0, wf: wf ? wf.currentPosition : null }; };
        return JSON.stringify({ d1: rd(1), d2: rd(2) });
    })()"""
    sa, sb = twin.evj("A", snap_js), twin.evj("B", snap_js)
    assert not sa["d2"]["play"] and not sb["d1"]["play"], "copy played while paused"
    a1_tp_wf = (sa["d1"]["tp"] - sa["d1"]["wf"]) * 1000.0 / sr_a
    b2_tp_wf = (sb["d2"]["tp"] - sb["d2"]["wf"]) * 1000.0 / sr_b
    b1_err = sb["d1"]["wf"] * 1000.0 / sr_b1 - sa["d1"]["wf"] * 1000.0 / sr_a
    a2_err = sa["d2"]["wf"] * 1000.0 / sr_a2 - sb["d2"]["wf"] * 1000.0 / sr_b
    print(f"\n==================== PAUSED ALIGNMENT ====================", flush=True)
    print(f"  Local tp-wf: A1={a1_tp_wf:+.3f}ms B2={b2_tp_wf:+.3f}ms | Copy err: B1={b1_err:+.3f}ms A2={a2_err:+.3f}ms", flush=True)
    assert abs(a1_tp_wf) <= PAUSED_LIMIT_MS and abs(b2_tp_wf) <= PAUSED_LIMIT_MS, f"paused tp-wf: {a1_tp_wf}, {b2_tp_wf}"
    assert abs(b1_err) <= PAUSED_LIMIT_MS and abs(a2_err) <= PAUSED_LIMIT_MS, f"paused copy err: B1={b1_err}, A2={a2_err}"


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"
    # Keep eth0 continuously UP so Engine Link / Ableton Link clock sync stays
    # converged from boot; removing the opposite local music files forces shadow copies.
    shutil.copy2(twin.WORK / "A/data/music/track-a.wav", twin.WORK / "B/data/music/track-b128.wav")
    for side, fn in (("A", "track-b.wav"), ("B", "track-a.wav")):
        (twin.WORK / side / "data/music" / fn).unlink(missing_ok=True)

    twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "loadA"')
    twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b128.wav"; "loadB128"')
    wait_analyzed("A", 1, 128)
    wait_analyzed("B", 2, 128)
    wait_analyzed("A", 2, 128)
    wait_analyzed("B", 1, 128)

    r_native, cost = run_synced_case("NATIVE SAME BPM (128 vs 128), SYNC ON, LEAD=A1", "track-a.wav", "track-b128.wav", measure_cost=True)
    r_pitched, _ = run_synced_case("PITCHED SAME BPM (128 vs 132@-3.03%=128), SYNC ON, LEAD=A1", "track-a.wav", "track-b.wav", load_b_bpm=132)
    check_paused_alignment("track-a.wav", "track-b.wav")

    worst_copy = max(r_native["b1_p99"], r_native["a2_p99"], r_pitched["b1_p99"], r_pitched["a2_p99"])
    worst_screen = max(r_native["a_scr_p99"], r_native["b_scr_p99"], r_pitched["a_scr_p99"], r_pitched["b_scr_p99"])
    print("\n==================== SUMMARY ====================", flush=True)
    for tag, r in (("Native  128v128", r_native), ("Pitched 128v132", r_pitched)):
        print(
            f"  {tag}: copy p99 B1={r['b1_p99']:.2f}ms/A2={r['a2_p99']:.2f}ms | "
            f"screen p99 A={r['a_scr_p99']:.2f}ms/B={r['b_scr_p99']:.2f}ms | "
            f"stock phase mean={r['stock_mean']:+.2f}ms p99={r['stock_p99']:.2f}ms | paired={r['paired']}"
        )
    print(f"  Cost: A={cost['corr_a_min']:.1f} corr/min, GUI CPU={cost['gui_cpu_a']:.2f}% | B={cost['corr_b_min']:.1f} corr/min, GUI CPU={cost['gui_cpu_b']:.2f}%")
    print(f"  PASS: copy p99={worst_copy:.2f}ms (<= {P99_LIMIT_MS:.2f}ms) | screen p99={worst_screen:.2f}ms (<= {SCREEN_P99_LIMIT_MS:.2f}ms) on both screens", flush=True)


if __name__ == "__main__":
    main()
