"""Check #346: live beat-grid edits on source A update loaded shadow copy on B without reload."""

import hashlib
import json
import os
import sqlite3
import subprocess
import sys
import time
from contextlib import closing
from pathlib import Path
from urllib.parse import urlencode

sys.path.insert(0, str(Path(__file__).resolve().parent))
import twin


def db_row(side, filename):
    db = twin.WORK / side / "data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db"
    if not db.is_file():
        return None
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as conn:
        r = conn.execute(
            "SELECT t.id, t.path, t.isAnalyzed, t.bpmAnalyzed, p.beatData "
            "FROM Track t JOIN PerformanceData p ON t.id = p.trackId "
            "WHERE t.filename = ? ORDER BY t.id DESC LIMIT 1",
            (filename,),
        ).fetchone()
    if not r:
        return None
    beat = r[4] or b""
    return {
        "id": r[0],
        "path": r[1],
        "analyzed": r[2],
        "bpmA": r[3],
        "beatLen": len(beat),
        "beatSha": hashlib.sha256(beat).hexdigest()[:12] if beat else "",
    }


def deck_state(side, d=1):
    expr = (
        f'(() => {{ const g = p => root.rtTr("/Engine/Deck{d}" + p) || {{}};'
        f' const at = (root.scShadowAt && root.scShadowAt[{d}]) || {{}};'
        f' return JSON.stringify({{'
        f' loaded: !!g("/Track/SongLoaded").state,'
        f' analyzed: !!g("/Track/SongAnalyzed").state,'
        f' play: !!g("/PlayState").state,'
        f' master: !!g("/DeckIsMaster").state,'
        f' bpm: g("/OriginalBPM").unnormalized || 0,'
        f' firstAnchor: g("/Track/FirstAnchorTempo").value || 0,'
        f' beat: g("/BeatNumber").unnormalized,'
        f' pos: g("/Track/TrackPosition").unnormalized || 0,'
        f' sr: g("/Track/SampleRate").value || 44100,'
        f' cue1: g("/Track/QuickCue1Position").value || 0,'
        f' loads: at.loads || 0,'
        f' gridGen: at.gridGen || "",'
        f' engMask: root.scShadowEngineMask || 0,'
        f' dispOnly: !!root.scShadowDisplayOnly'
        f' }}); }})()'
    )
    return twin.evj(side, expr)


def wait_for(fn, timeout=30, label="condition"):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        res = fn()
        if res:
            return res
        time.sleep(0.35)
    raise AssertionError(f"timed out waiting for {label} (last={last})")


def http_get_json(side, path):
    raw = subprocess.check_output(
        ["ip", "netns", "exec", twin.SIDE_CFG[side]["netns"], "curl", "-sS", "http://127.0.0.1:8080" + path],
        text=True,
    )
    return json.loads(raw)


def oa_log(side):
    return subprocess.check_output(twin.ns_chroot_cmd(side, "cat /var/log/enginex-uimod-oa.log"), text=True)


def edit_grid_a(js_body, hold_sec=0.0):
    twin.ev(
        "A",
        '(() => { const ed = PlanckGUI.views.beatGridEditViewController;'
        ' ed.open(); ed.decksTabBarViewController.selectDeck(1); ' + js_body + ' return "started"; })()',
    )
    if hold_sec > 0:
        time.sleep(hold_sec)
    twin.ev(
        "A",
        '(() => { const ed = PlanckGUI.views.beatGridEditViewController;'
        ' ed.setShiftingGridLeft(false); ed.setShiftingGridRight(false); ed.close(); return "closed"; })()',
    )


def beat_diff(a, b):
    """Compare BeatNumber on A and B adjusted for small TrackPosition delta."""
    dt_beats = (a["pos"] - b["pos"]) / max(1.0, a["sr"]) * (a["bpm"] / 60.0)
    d = ((a["beat"] - b["beat"] - dt_beats) + 2.0) % 4.0 - 2.0
    return abs(d)


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"
    # Remove B's local copy so B must load A's track as a peer shadow copy.
    b_local = twin.WORK / "B/data/music/track-a.wav"
    if b_local.exists():
        b_local.unlink()

    # Load track-a.wav on A Deck 1 and set Hot Cue 1.
    twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "load"')
    wait_for(lambda: (s := deck_state("A"))["loaded"] and s["analyzed"] and abs(s["bpm"] - 128) < 0.2, 60, "A initial load")
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Track/QuickCue1SetPosition").value = 220500; "cue1"')
    wait_for(lambda: (s := deck_state("B"))["loaded"] and s["analyzed"] and abs(s["bpm"] - 128) < 0.2 and abs(s["cue1"] - 220500) < 2, 60, "B initial shadow load")

    a0, b0 = deck_state("A"), deck_state("B")
    ra0, rb0 = db_row("A", "track-a.wav"), db_row("B", "track-a.wav")
    print(f"INITIAL live path: B engMask={b0['engMask']} dispOnly={b0['dispOnly']} play={b0['play']} loads={b0['loads']} bpm={b0['bpm']:.2f} cue1={b0['cue1']:.0f}", flush=True)
    print(f"INITIAL db: A={ra0} B={rb0}", flush=True)
    assert b0["loads"] == 1, b0
    assert "track-a.wav" not in oa_log("B"), "first load must skip OfflineAnalyzer:\n" + oa_log("B")

    # 1) Paused grid edit on A: downbeat shift + halve tempo (128 -> 64) + fractional right shift.
    edit_grid_a("ed.shiftDownbeatForwards(); ed.halveTempo(); ed.setShiftingGridRight(true);", hold_sec=0.5)
    wait_for(lambda: (r := db_row("A", "track-a.wav")) and r["beatSha"] != ra0["beatSha"] and abs(r["bpmA"] - 64) < 0.1, 15, "A edit 1 m.db")
    ra1 = db_row("A", "track-a.wav")
    wait_for(
        lambda: (s := deck_state("B")) and abs(s["bpm"] - 64) < 0.1 and (rb := db_row("B", "track-a.wav")) and rb["beatSha"] == ra1["beatSha"],
        20,
        "B in-place update after edit 1",
    )
    a1, b1, rb1 = deck_state("A"), deck_state("B"), db_row("B", "track-a.wav")
    bd1 = beat_diff(a1, b1)
    print(f"EDIT1 (paused 128->64+shift): A bpm={a1['bpm']:.2f} beat={a1['beat']:.3f} | B bpm={b1['bpm']:.2f} beat={b1['beat']:.3f} diff={bd1:.3f} loads={b1['loads']} cue1={b1['cue1']:.0f} sha={rb1['beatSha']}", flush=True)
    assert b1["loads"] == 1, f"deck reloaded on edit 1: {b1}"
    assert abs(b1["firstAnchor"] - 64) < 0.1, b1
    assert bd1 < 0.08, f"beat phase mismatch after edit 1: A={a1} B={b1} diff={bd1}"
    assert abs(b1["cue1"] - 220500) < 2, f"hot cue lost on edit 1: {b1}"
    assert rb1["analyzed"] == 1 and rb1["beatSha"] == ra1["beatSha"], (ra1, rb1)

    # 2) Second consecutive paused edit: phase-only left shift + anchor insert (BPM stays 64).
    edit_grid_a("ed.setShiftingGridLeft(true);", hold_sec=0.35)
    edit_grid_a("ed.insertAnchorAtPlayPosition();")
    wait_for(lambda: (r := db_row("A", "track-a.wav")) and r["beatSha"] != ra1["beatSha"], 15, "A edit 2 m.db")
    ra2 = db_row("A", "track-a.wav")
    wait_for(
        lambda: (rb := db_row("B", "track-a.wav")) and rb["beatSha"] == ra2["beatSha"] and rb["analyzed"] == 1,
        20,
        "B in-place update after edit 2 (phase/anchor only)",
    )
    a2, b2, rb2 = deck_state("A"), deck_state("B"), db_row("B", "track-a.wav")
    bd2 = beat_diff(a2, b2)
    print(f"EDIT2 (phase+anchor): A beat={a2['beat']:.3f} sha={ra2['beatSha']} | B beat={b2['beat']:.3f} diff={bd2:.3f} loads={b2['loads']} sha={rb2['beatSha']}", flush=True)
    assert b2["loads"] == 1, f"deck reloaded on edit 2: {b2}"
    assert bd2 < 0.08, f"beat phase mismatch after edit 2: A={a2} B={b2} diff={bd2}"
    assert abs(b2["cue1"] - 220500) < 2, f"hot cue lost on edit 2: {b2}"

    # 3) Live edit while A Deck 1 is PLAYING: double tempo (64 -> 128).
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state = true; "play"')
    wait_for(lambda: deck_state("A")["play"], 10, "A playing")
    time.sleep(0.8)
    edit_grid_a("ed.doubleTempo();")
    wait_for(lambda: (r := db_row("A", "track-a.wav")) and abs(r["bpmA"] - 128) < 0.1 and r["beatSha"] != ra2["beatSha"], 15, "A edit 3 playing m.db")
    ra3 = db_row("A", "track-a.wav")
    wait_for(
        lambda: (s := deck_state("B")) and abs(s["bpm"] - 128) < 0.1 and (rb := db_row("B", "track-a.wav")) and rb["beatSha"] == ra3["beatSha"],
        20,
        "B in-place update while A playing",
    )
    a3, b3, rb3 = deck_state("A"), deck_state("B"), db_row("B", "track-a.wav")
    print(f"EDIT3 (playing 64->128): A play={a3['play']} bpm={a3['bpm']:.2f} | B play={b3['play']} master={b3['master']} bpm={b3['bpm']:.2f} loads={b3['loads']} cue1={b3['cue1']:.0f} sha={rb3['beatSha']}", flush=True)
    assert a3["play"], "A stopped playing during grid edit"
    assert b3["loads"] == 1, f"deck reloaded during playing edit: {b3}"
    assert not b3["master"], f"silent shadow copy took MASTER: {b3}"
    assert abs(b3["cue1"] - 220500) < 2, f"hot cue lost on playing edit: {b3}"
    twin.ev("A", 'root.rtTr("/Engine/Deck1/Play").state = false; "pause"')

    # Verify oa_replay skipped oa_settle() (waited < 50 ms) and completed fast.
    log_b = oa_log("B")
    waited_lines = [ln for ln in log_b.splitlines() if "waited " in ln and "track-a.wav" in ln]
    replayed_lines = [ln for ln in log_b.splitlines() if "replayed " in ln and "track-a.wav" in ln]
    print("B oa_replay log:\n" + "\n".join(waited_lines + replayed_lines), flush=True)
    assert len(replayed_lines) >= 3, f"expected >=3 replays, got:\n{log_b}"
    for ln in waited_lines:
        us = int(ln.split()[2].removesuffix("us"))
        assert us < 50000, f"oa_replay waited {us}us (did not skip oa_settle!): {ln}"

    # 4) Display-only mode check: switch B to shadow_mode=cdj, edit grid on A, verify /display updates.
    subprocess.run(
        ["ip", "netns", "exec", twin.SIDE_CFG["B"]["netns"], "curl", "-sS", "-X", "POST",
         "-d", "shadowMode=cdj", "http://127.0.0.1:8080/v1/enginex/ui"],
        check=True,
        stdout=subprocess.DEVNULL,
    )
    def disp_state():
        sh = http_get_json("B", "/v1/enginex/shadow")
        if not sh.get("displayOnly"):
            return None
        d1 = next((d for d in (sh.get("decks") or []) if d.get("deck") == 1 and d.get("key") and not d.get("loading")), None)
        if not d1:
            return None
        meta = http_get_json("B", "/v1/enginex/shadow/display?" + urlencode({"deck": 1, "key": d1["key"]}))
        if not meta.get("beats"):
            return None
        return {"key": d1["key"], "bpm": meta["bpm"], "beat0": meta["beats"][0]["ms"]}

    d_before = wait_for(lambda: (d := disp_state()) and abs(d["bpm"] - 128) < 0.2 and d, 25, "display-only initial 128 BPM")
    edit_grid_a("ed.halveTempo();")
    d_after = wait_for(lambda: (d := disp_state()) and d["key"] != d_before["key"] and abs(d["bpm"] - 64) < 0.2 and d, 25, "display-only updated 64 BPM")
    print(f"DISPLAY-ONLY edit: before={d_before} -> after={d_after}", flush=True)
    print("PASS: #346 live beat-grid edits updated shadow copy in place (0 reloads, fast replay, display-only ok)")


if __name__ == "__main__":
    main()
