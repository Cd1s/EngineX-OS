#!/usr/bin/env python3
"""Check cause-1 shadow grid offset while both units play synced at the same BPM.

Fails on d34e420d (copy follow error beyond +/-3 ms p99) and passes with the
sender waveformPositionItem + receiver half-RTT + paused-follow deadband fix.

Measures on both screens (Unit A: Deck 2 copy vs Deck 1 local; Unit B: Deck 1
copy vs Deck 2 local) at 60 FPS (SHADOW_TWIN_HEADLESS=320x200):
  - Copy display follow error (copy wfPos - source wfPos): mean, median, p99(|err|), max
  - Per-screen lane separation and which deck appears ahead on each screen
  - Paused follow invariant: PlayState == false on both copies
  - Paused alignment: 0.000 ms (<= 0.05 ms) when both decks are paused
  - Steady-play cost: corrections/min (X-Shadow-State ver bumps & feed updates)
    and each copy unit's Engine GUI-thread CPU % (/proc/<pid>/task/<pid>/stat)
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
PAUSED_LIMIT_MS = 0.05
COST_WINDOW_S = float(os.environ.get("SGO_COST_WINDOW_S", "20.0"))
N_SAMPLES = int(os.environ.get("SGO_SAMPLES", "120"))


def wait_for(fn, timeout=60, label="condition"):
    dl = time.monotonic() + timeout
    last = None
    while time.monotonic() < dl:
        v = fn()
        if v:
            return v
        last = v
        time.sleep(0.25)
    raise AssertionError(f"timed out waiting for {label} (last={last})")


def ns_curl_headers_and_json(side, path):
    raw = subprocess.check_output(
        ["ip", "netns", "exec", twin.SIDE_CFG[side]["netns"], "curl", "-sSi", "http://127.0.0.1:8080" + path],
        text=True,
    )
    head, _, body = raw.partition("\r\n\r\n")
    if not body:
        head, _, body = raw.partition("\n\n")
    headers = {}
    for ln in head.splitlines()[1:]:
        if ":" in ln:
            k, v = ln.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    return headers, json.loads(body) if body.strip() else {}


def shadow_ver(side):
    headers, _ = ns_curl_headers_and_json(side, "/v1/enginex/shadow/decks")
    st = headers.get("x-shadow-state", "")
    try:
        return int(st.split(":", 1)[0])
    except (ValueError, IndexError):
        return 0


def find_engine_pid(side):
    """Find the host-visible PID of Engine running inside this twin sandbox side."""
    work_str = str(twin.WORK.resolve())
    marker = f"{work_str}/{side}/"
    for p in Path("/proc").iterdir():
        if not p.name.isdigit():
            continue
        try:
            cmd = (p / "cmdline").read_bytes().decode("utf-8", "ignore")
            if "/usr/Engine/Engine" not in cmd:
                continue
            mi = (p / "mountinfo").read_text(errors="ignore")
            if marker in mi:
                return int(p.name)
        except OSError:
            continue
    return None


def read_gui_thread_cpu_ticks(pid):
    """Return (utime + stime) ticks of Engine's main (Qt GUI) thread (tid == pid)."""
    st = Path(f"/proc/{pid}/task/{pid}/stat").read_text()
    rparen = st.rfind(")")
    fields = st[rparen + 2 :].split()
    # fields[0]=state (field 3); utime=field 14 -> index 11; stime=field 15 -> index 12
    return int(fields[11]) + int(fields[12])


def parse_grid_b0(side, filename):
    db = twin.WORK / side / "data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db"
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as con:
        row = con.execute(
            "SELECT p.beatData FROM Track t JOIN PerformanceData p ON t.id=p.trackId "
            "WHERE t.filename=? ORDER BY t.id DESC LIMIT 1",
            (filename,),
        ).fetchone()
    assert row and row[0], f"missing beatData for {side}/{filename}"
    raw = zlib.decompress(row[0][4:])
    sr, _tot = struct.unpack_from(">dd", raw, 0)
    off = 17
    grids = []
    for _ in range(2):
        n = struct.unpack_from(">Q", raw, off)[0]
        off += 8
        ms = [struct.unpack_from("<dqII", raw, off + i * 24) for i in range(n)]
        off += n * 24
        grids.append(ms)
    use = grids[1] if len(grids[1]) >= 2 else grids[0]
    m0, m1 = use[0], use[1]
    spb = (m1[0] - m0[0]) / (m1[1] - m0[1])
    b0 = m0[0] - m0[1] * spb
    return sr, spb, b0


def mod_beat_ms(b_diff, cur_bpm):
    d = ((b_diff + 0.5) % 1.0) - 0.5
    return d * (60000.0 / max(1.0, cur_bpm))


def pct(vals, p):
    if not vals:
        return float("nan")
    s = sorted(vals)
    idx = min(len(s) - 1, max(0, math.ceil(len(s) * p / 100.0) - 1))
    return s[idx]


def fmt_stats(vals):
    if not vals:
        return "n=0"
    absv = [abs(v) for v in vals]
    return (
        f"n={len(vals):3d} mean={statistics.mean(vals):+6.2f}ms med={statistics.median(vals):+6.2f}ms "
        f"p99(|e|)={pct(absv, 99):5.2f}ms max(|e|)={max(absv):5.2f}ms "
        f"[min={min(vals):+6.2f}, max={max(vals):+6.2f}]"
    )


def deck_sync_info(side, deck):
    js = f"""(() => {{
        const g = p => root.rtTr("/Engine/Deck{deck}" + p) || {{}};
        const pm = PlanckGUI && PlanckGUI.views && PlanckGUI.views.getPhaseMeter
            ? PlanckGUI.views.getPhaseMeter({deck - 1}) : null;
        return JSON.stringify({{
            play: !!g("/PlayState").state,
            master: !!g("/DeckIsMaster").state,
            sync: g("/SyncMode").string || "Off",
            bpm: g("/OriginalBPM").unnormalized || 0,
            curBpm: g("/CurrentBPM").unnormalized || 0,
            aligned: pm ? !!pm.isAligned : false,
            pmOff: pm ? Number(pm.phaseOffset) : 0,
        }});
    }})()"""
    return twin.evj(side, js)


def ensure_sync_a_lead():
    """Lock A Deck 1 as MASTER/LEAD and B Deck 2 as Sync follower without toggling Sync off."""
    a1 = deck_sync_info("A", 1)
    if not a1["master"]:
        twin.ev("A", 'root.rtPress("/Engine/Deck1/RequestMaster"); "reqMasterA"')
        time.sleep(0.6)
    b2 = deck_sync_info("B", 2)
    if b2["sync"] == "Off":
        twin.ev("B", 'root.rtPress("/Engine/Deck2/Sync"); "syncB"')
        time.sleep(0.6)
    for _ in range(32):
        time.sleep(0.25)
        a1 = deck_sync_info("A", 1)
        b2 = deck_sync_info("B", 2)
        if a1["master"] and not b2["master"] and b2["sync"] != "Off" and b2["aligned"]:
            return a1, b2
    return a1, b2


def arm_burst(side, n_samples=120, interval_ms=40):
    js = f"""(() => {{
        const bag = root.scShadowTextAt;
        bag._sgoChk = [];
        if (bag._sgoChkT) {{ bag._sgoChkT.stop(); bag._sgoChkT.destroy(); bag._sgoChkT = null; }}
        bag._sgoChkT = Qt.createQmlObject('import QtQml; Timer {{ interval: {interval_ms}; repeat: true; running: true }}', root);
        bag._sgoChkT.triggered.connect(function() {{
            if (bag._sgoChk.length >= {n_samples}) {{ bag._sgoChkT.stop(); return; }}
            const now = Date.now();
            const readD = d => {{
                const g = p => root.rtTr("/Engine/Deck" + d + p) || {{}};
                const wf = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem(d) : null;
                const pr = root.scShadowPrev && root.scShadowPrev[d];
                const at = root.scShadowAt && root.scShadowAt[d];
                return {{
                    play: !!g("/PlayState").state,
                    sr: g("/Track/SampleRate").value || 44100,
                    bpm: g("/OriginalBPM").unnormalized || 0,
                    curBpm: g("/CurrentBPM").unnormalized || 0,
                    tp: g("/Track/TrackPosition").unnormalized || 0,
                    wf: wf ? wf.currentPosition : null,
                    pr: pr ? {{ at: pr.at, lineMs: pr.lineMs }} : null,
                    feedAt: at ? (at.feedAt || 0) : 0,
                }};
            }};
            bag._sgoChk.push({{ now, d1: readD(1), d2: readD(2) }});
        }});
        return "armed";
    }})()"""
    twin.ev(side, js)


def read_burst(side, n_samples=120):
    wait_for(
        lambda: twin.evj(side, "JSON.stringify(root.scShadowTextAt._sgoChk ? root.scShadowTextAt._sgoChk.length : 0)")
        >= n_samples,
        30,
        f"{side} burst",
    )
    return twin.evj(side, "JSON.stringify(root.scShadowTextAt._sgoChk)")


def measure_playing_pair(label, fn_a, fn_b, n_samples=120):
    sr_a, spb_a, b0_a = parse_grid_b0("A", fn_a)
    sr_b, spb_b, b0_b = parse_grid_b0("B", fn_b)
    sr_b1, spb_b1, b0_b1 = parse_grid_b0("B", fn_a)
    sr_a2, spb_a2, b0_a2 = parse_grid_b0("A", fn_b)

    arm_burst("A", n_samples, 40)
    arm_burst("B", n_samples, 40)
    la = read_burst("A", n_samples)
    lb = read_burst("B", n_samples)

    dt_a = [la[i]["now"] - la[i - 1]["now"] for i in range(1, len(la))]
    dt_b = [lb[i]["now"] - lb[i - 1]["now"] for i in range(1, len(lb))]

    # Assert copies never play (PlayState == false) on every sample
    a2_played = sum(1 for r in la if r["d2"]["play"])
    b1_played = sum(1 for r in lb if r["d1"]["play"])
    assert a2_played == 0 and b1_played == 0, f"shadow copy played! A2={a2_played} B1={b1_played}"

    a_scr, b_scr = [], []
    tp_wf_a, line_wf_a, tp_wf_b, line_wf_b = [], [], [], []
    for r in la:
        d1, d2 = r["d1"], r["d2"]
        if d1["wf"] is None or d2["wf"] is None or not d1["pr"]:
            continue
        cur = d1["curBpm"]
        rate = d1["curBpm"] / max(1.0, d1["bpm"])
        a_scr.append(mod_beat_ms((d2["wf"] - b0_a2) / spb_a2 - (d1["wf"] - b0_a) / spb_a, cur))
        tp_ms = d1["tp"] * 1000.0 / sr_a
        wf_ms = d1["wf"] * 1000.0 / sr_a
        line_ms = d1["pr"]["lineMs"] + (r["now"] - d1["pr"]["at"]) * rate
        tp_wf_a.append(tp_ms - wf_ms)
        line_wf_a.append(line_ms - wf_ms)

    for r in lb:
        d1, d2 = r["d1"], r["d2"]
        if d1["wf"] is None or d2["wf"] is None or not d2["pr"]:
            continue
        cur = d2["curBpm"]
        rate = d2["curBpm"] / max(1.0, d2["bpm"])
        b_scr.append(mod_beat_ms((d1["wf"] - b0_b1) / spb_b1 - (d2["wf"] - b0_b) / spb_b, cur))
        tp_ms = d2["tp"] * 1000.0 / sr_b
        wf_ms = d2["wf"] * 1000.0 / sr_b
        line_ms = d2["pr"]["lineMs"] + (r["now"] - d2["pr"]["at"]) * rate
        tp_wf_b.append(tp_ms - wf_ms)
        line_wf_b.append(line_ms - wf_ms)

    b1_err, a2_err, b1_net, a2_net, sync_wf = [], [], [], [], []
    for ra in la:
        rb = min(lb, key=lambda x: abs(x["now"] - ra["now"]))
        dt = rb["now"] - ra["now"]
        if abs(dt) > 25:
            continue
        a1, a2, b1, b2 = ra["d1"], ra["d2"], rb["d1"], rb["d2"]
        if None in (a1["wf"], a2["wf"], b1["wf"], b2["wf"]) or not a1["pr"] or not b2["pr"]:
            continue
        cur = a1["curBpm"]
        rate_a = a1["curBpm"] / max(1.0, a1["bpm"])
        rate_b = b2["curBpm"] / max(1.0, b2["bpm"])

        a1_wf_at_b = a1["wf"] * 1000.0 / sr_a + dt * rate_a
        b1_wf = b1["wf"] * 1000.0 / sr_b1
        b1_err.append(b1_wf - a1_wf_at_b)

        b2_wf_at_a = b2["wf"] * 1000.0 / sr_b - dt * rate_b
        a2_wf = a2["wf"] * 1000.0 / sr_a2
        a2_err.append(a2_wf - b2_wf_at_a)

        a1_line_at_b = a1["pr"]["lineMs"] + (rb["now"] - a1["pr"]["at"]) * rate_a
        b2_line_at_a = b2["pr"]["lineMs"] + (ra["now"] - b2["pr"]["at"]) * rate_b
        b1_net.append(b1_wf - a1_line_at_b)
        a2_net.append(a2_wf - b2_line_at_a)

        b2_wf_at_a_smp = b2["wf"] - (dt / 1000.0) * rate_b * sr_b
        sync_wf.append(mod_beat_ms((a1["wf"] - b0_a) / spb_a - (b2_wf_at_a_smp - b0_b) / spb_b, cur))

    a_scr_mean = statistics.mean(a_scr)
    b_scr_mean = statistics.mean(b_scr)
    a_lead = "Deck 1 (local)" if a_scr_mean < 0 else "Deck 2 (copy)"
    b_lead = "Deck 2 (local)" if b_scr_mean < 0 else "Deck 1 (copy)"

    b1_p99 = pct([abs(v) for v in b1_err], 99)
    a2_p99 = pct([abs(v) for v in a2_err], 99)

    print(f"\n==================== {label} ====================", flush=True)
    print(f"  40ms cadence: A={statistics.mean(dt_a):.1f}ms (max={max(dt_a):.0f}ms) | B={statistics.mean(dt_b):.1f}ms (max={max(dt_b):.0f}ms)")
    print(f"  Copy PlayState=false: A2={a2_played}/{len(la)} B1={b1_played}/{len(lb)} (OK)")
    print(f"  UNIT A screen (Deck2 copy - Deck1 local): {fmt_stats(a_scr)} -> ahead: {a_lead}")
    print(f"  UNIT B screen (Deck1 copy - Deck2 local): {fmt_stats(b_scr)} -> ahead: {b_lead}")
    print(f"  Systematic copy offset (A_scr+B_scr)/2:   {(a_scr_mean + b_scr_mean) / 2:+6.2f}ms")
    print(f"  B1 copy wf - A1 src wf:                   {fmt_stats(b1_err)}")
    print(f"  A2 copy wf - B2 src wf:                   {fmt_stats(a2_err)}")
    print(f"  Sender lineMs - src wf (A1 / B2):         {statistics.mean(line_wf_a):+6.2f}ms / {statistics.mean(line_wf_b):+6.2f}ms")
    print(f"  Receiver Copy wf - lineMs (B1 / A2):      {statistics.mean(b1_net):+6.2f}ms / {statistics.mean(a2_net):+6.2f}ms")
    print(f"  True Engine Sync phase (A1 wf - B2 wf):   {fmt_stats(sync_wf)}")
    print(f"  Sync status: A1={deck_sync_info('A', 1)} | B2={deck_sync_info('B', 2)}", flush=True)

    return {
        "label": label,
        "b1_mean": statistics.mean(b1_err),
        "a2_mean": statistics.mean(a2_err),
        "b1_p99": b1_p99,
        "a2_p99": a2_p99,
        "a_scr_mean": a_scr_mean,
        "b_scr_mean": b_scr_mean,
        "a_lead": a_lead,
        "b_lead": b_lead,
    }


def measure_steady_cost(window_s=20.0):
    """Measure corrections/min and copy unit Engine GUI-thread CPU % during steady play."""
    pid_a = find_engine_pid("A")
    pid_b = find_engine_pid("B")
    assert pid_a and pid_b, f"could not locate Engine PIDs: A={pid_a} B={pid_b}"
    clk_tck = os.sysconf(os.sysconf_names["SC_CLK_TCK"])

    # Install lightweight counter on scShadowFeedSent updates? Simpler: count X-Shadow-State ver bumps
    # directly via stream-sync (no polling load on Engine GUI thread during the window!).
    ver_a0, ver_b0 = shadow_ver("A"), shadow_ver("B")
    cpu_a0, cpu_b0 = read_gui_thread_cpu_ticks(pid_a), read_gui_thread_cpu_ticks(pid_b)
    t0 = time.monotonic()
    time.sleep(window_s)
    dt = time.monotonic() - t0
    cpu_a1, cpu_b1 = read_gui_thread_cpu_ticks(pid_a), read_gui_thread_cpu_ticks(pid_b)
    ver_a1, ver_b1 = shadow_ver("A"), shadow_ver("B")

    gui_cpu_a = (cpu_a1 - cpu_a0) / clk_tck / dt * 100.0
    gui_cpu_b = (cpu_b1 - cpu_b0) / clk_tck / dt * 100.0
    corr_a_min = (ver_a1 - ver_a0) / dt * 60.0
    corr_b_min = (ver_b1 - ver_b0) / dt * 60.0

    print(f"\n==================== STEADY-PLAY COST ({dt:.1f}s window) ====================", flush=True)
    print(f"  Unit A (holds Deck 2 copy): corrections={ver_a1 - ver_a0} ({corr_a_min:.1f}/min) | Engine GUI-thread CPU={gui_cpu_a:.2f}%")
    print(f"  Unit B (holds Deck 1 copy): corrections={ver_b1 - ver_b0} ({corr_b_min:.1f}/min) | Engine GUI-thread CPU={gui_cpu_b:.2f}%", flush=True)
    return {
        "window_s": dt,
        "corr_a": ver_a1 - ver_a0,
        "corr_b": ver_b1 - ver_b0,
        "corr_a_min": corr_a_min,
        "corr_b_min": corr_b_min,
        "gui_cpu_a": gui_cpu_a,
        "gui_cpu_b": gui_cpu_b,
    }


def check_paused_alignment(fn_a, fn_b):
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state = false; "pauseA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Play").state = false; "pauseB"')
    time.sleep(1.2)
    sr_a, _, _ = parse_grid_b0("A", fn_a)
    sr_b, _, _ = parse_grid_b0("B", fn_b)
    sr_b1, _, _ = parse_grid_b0("B", fn_a)
    sr_a2, _, _ = parse_grid_b0("A", fn_b)

    def snap(side):
        return twin.evj(
            side,
            """(() => {
                const readD = d => {
                    const g = p => root.rtTr("/Engine/Deck" + d + p) || {};
                    const wf = PlanckGUI && PlanckGUI.waveformPositionItem ? PlanckGUI.waveformPositionItem(d) : null;
                    return { play: !!g("/PlayState").state, tp: g("/Track/TrackPosition").unnormalized || 0, wf: wf ? wf.currentPosition : null };
                };
                return JSON.stringify({ d1: readD(1), d2: readD(2) });
            })()""",
        )

    sa, sb = snap("A"), snap("B")
    assert not sa["d2"]["play"] and not sb["d1"]["play"], "copy played while paused"
    a1_tp_wf = (sa["d1"]["tp"] - sa["d1"]["wf"]) * 1000.0 / sr_a
    b2_tp_wf = (sb["d2"]["tp"] - sb["d2"]["wf"]) * 1000.0 / sr_b
    b1_err = sb["d1"]["wf"] * 1000.0 / sr_b1 - sa["d1"]["wf"] * 1000.0 / sr_a
    a2_err = sa["d2"]["wf"] * 1000.0 / sr_a2 - sb["d2"]["wf"] * 1000.0 / sr_b
    print(f"\n==================== PAUSED ALIGNMENT ====================", flush=True)
    print(f"  Local tp-wf: A1={a1_tp_wf:+.3f}ms B2={b2_tp_wf:+.3f}ms")
    print(f"  Paused copy wf - source wf: B1={b1_err:+.3f}ms A2={a2_err:+.3f}ms", flush=True)
    assert abs(a1_tp_wf) <= PAUSED_LIMIT_MS and abs(b2_tp_wf) <= PAUSED_LIMIT_MS, f"paused tp-wf: {a1_tp_wf}, {b2_tp_wf}"
    assert abs(b1_err) <= PAUSED_LIMIT_MS and abs(a2_err) <= PAUSED_LIMIT_MS, f"paused copy err: B1={b1_err}, A2={a2_err}"


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"

    # Give B a 128 BPM track of its own (track-b128.wav) and remove opposite local files so shadow copies are forced.
    shutil.copy2(twin.WORK / "A/data/music/track-a.wav", twin.WORK / "B/data/music/track-b128.wav")
    for side, fn in (("A", "track-b.wav"), ("B", "track-a.wav")):
        p = twin.WORK / side / "data/music" / fn
        if p.exists():
            p.unlink()

    # Load and analyze own tracks first with link down, then bring link up for shadow copies.
    link_cmd = ["ip", "-n", twin.SIDE_CFG["B"]["netns"], "link", "set", "eth0"]
    subprocess.run([*link_cmd, "down"], check=True)
    try:
        twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "loadA"')
        twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b.wav"; "loadB132"')
        wait_for(
            lambda: twin.evj("A", 'JSON.stringify(!!root.rtTr("/Engine/Deck1/Track/SongAnalyzed").state)')
            and twin.evj("B", 'JSON.stringify(!!root.rtTr("/Engine/Deck2/Track/SongAnalyzed").state)'),
            60,
            "own 132 analyzed",
        )
        twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b128.wav"; "loadB128"')
        wait_for(
            lambda: twin.evj("B", 'JSON.stringify(!!root.rtTr("/Engine/Deck2/Track/SongAnalyzed").state && Math.abs((root.rtTr("/Engine/Deck2/OriginalBPM").unnormalized||0)-128)<0.2)'),
            60,
            "B2 128 analyzed",
        )
    finally:
        subprocess.run([*link_cmd, "up"], check=True)

    wait_for(
        lambda: twin.evj("A", 'JSON.stringify(!!root.rtTr("/Engine/Deck2/Track/SongAnalyzed").state)')
        and twin.evj("B", 'JSON.stringify(!!root.rtTr("/Engine/Deck1/Track/SongAnalyzed").state)'),
        60,
        "shadow copies analyzed",
    )

    # Confirm paused alignment BEFORE play (and after play below).
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Track/SetSongPosition").value = 0.05; "seekA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Track/SetSongPosition").value = 0.05; "seekB"')
    time.sleep(0.8)

    # 1. Native same BPM (128.0 vs 128.0), Sync ON, A1=LEAD
    subprocess.run(["python3", str(Path(__file__).resolve().parent / "twin.py"), "play", "start"], check=True)
    a1, b2 = ensure_sync_a_lead()
    print(f"Synced 128v128: A1={a1} | B2={b2}", flush=True)
    time.sleep(2.5)
    r_native = measure_playing_pair("NATIVE SAME BPM (128 vs 128), SYNC ON, LEAD=A1", "track-a.wav", "track-b128.wav", N_SAMPLES)
    cost = measure_steady_cost(COST_WINDOW_S)

    # 2. Pitched same BPM (A1 128.0 LEAD vs B2 132.0 @ -3.03% = 128.0 SYNC)
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Play").state = false; "stopB"')
    time.sleep(0.3)
    twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b.wav"; "loadB132"')
    wait_for(
        lambda: twin.evj("B", 'JSON.stringify(!!root.rtTr("/Engine/Deck2/Track/SongAnalyzed").state && Math.abs((root.rtTr("/Engine/Deck2/OriginalBPM").unnormalized||0)-132)<0.2)'),
        60,
        "B2 132 analyzed",
    )
    wait_for(
        lambda: twin.evj("A", 'JSON.stringify(!!root.rtTr("/Engine/Deck2/Track/SongAnalyzed").state && Math.abs((root.rtTr("/Engine/Deck2/OriginalBPM").unnormalized||0)-132)<0.2)'),
        60,
        "A2 132 shadow analyzed",
    )
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Track/SetSongPosition").value = 0.05; "seekA"')
    twin.ev("B", 'root.rtTr("/Engine/Deck2/Track/SetSongPosition").value = 0.05; "seekB"')
    time.sleep(0.4)
    subprocess.run(["python3", str(Path(__file__).resolve().parent / "twin.py"), "play", "B"], check=True)
    a1, b2 = ensure_sync_a_lead()
    print(f"Synced 128v132: A1={a1} | B2={b2}", flush=True)
    time.sleep(2.5)
    r_pitched = measure_playing_pair("PITCHED SAME BPM (128 vs 132@-3.03%=128), SYNC ON, LEAD=A1", "track-a.wav", "track-b.wav", N_SAMPLES)

    # 3. Paused alignment (0.000 ms)
    check_paused_alignment("track-a.wav", "track-b.wav")

    # Summary & hard assertion (p99 <= P99_LIMIT_MS on both copies for both pairs)
    worst_p99 = max(r_native["b1_p99"], r_native["a2_p99"], r_pitched["b1_p99"], r_pitched["a2_p99"])
    print("\n==================== SUMMARY ====================", flush=True)
    print(
        f"  Native  128v128: B1 p99={r_native['b1_p99']:.2f}ms (mean={r_native['b1_mean']:+.2f}ms) | "
        f"A2 p99={r_native['a2_p99']:.2f}ms (mean={r_native['a2_mean']:+.2f}ms)"
    )
    print(
        f"  Pitched 128v132: B1 p99={r_pitched['b1_p99']:.2f}ms (mean={r_pitched['b1_mean']:+.2f}ms) | "
        f"A2 p99={r_pitched['a2_p99']:.2f}ms (mean={r_pitched['a2_mean']:+.2f}ms)"
    )
    print(
        f"  Cost: A={cost['corr_a_min']:.1f} corr/min, GUI CPU={cost['gui_cpu_a']:.2f}% | "
        f"B={cost['corr_b_min']:.1f} corr/min, GUI CPU={cost['gui_cpu_b']:.2f}%"
    )
    print(f"  Worst copy p99(|err|): {worst_p99:.2f}ms (limit <= {P99_LIMIT_MS:.2f}ms)", flush=True)
    assert worst_p99 <= P99_LIMIT_MS, f"FAIL: copy p99(|err|) {worst_p99:.2f}ms exceeds +/-{P99_LIMIT_MS:.2f}ms"
    print("PASS: shadow grid offset within +/-3.00ms p99 on both screens", flush=True)


if __name__ == "__main__":
    main()
