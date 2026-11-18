"""Check the first peer load in a fresh engine-mode shadow-twin sandbox."""

import json
import os
import re
import sqlite3
import subprocess
import time
from contextlib import closing
from urllib.parse import urlencode

import twin


def row(side, filename):
    db = twin.WORK / side / "data/AppDataUser/AIR Music Technology/Engine/Engine Library/Database2/m.db"
    if not db.is_file():
        return None
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro", uri=True)) as conn:
        return conn.execute(
            "SELECT t.id,t.path,p.trackData,p.overviewWaveFormData,p.beatData,p.quickCues,p.loops, "
            "t.isAnalyzed,t.isBeatGridLocked,t.bpm,t.bpmAnalyzed "
            "FROM Track t JOIN PerformanceData p ON t.id=p.trackId WHERE t.filename=?"
            + (" AND t.isAnalyzed=1" if side == "A" else ""),
            (filename,),
        ).fetchone()


def wait_for(fn):
    deadline = time.monotonic() + 85
    while time.monotonic() < deadline:
        if result := fn():
            return result
        time.sleep(0.5)
    raise AssertionError("sandbox state did not arrive")


def analyzer_log(side):
    return subprocess.check_output(twin.ns_chroot_cmd(side, "cat /var/log/enginex-uimod-oa.log"), text=True)


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"
    assert row("A", "track-a.wav") is None and row("B", "track-a.wav") is None
    (twin.WORK / "B/data/music/track-a.wav").unlink()
    cmd = ["ip", "-n", twin.SIDE_CFG["B"]["netns"], "link", "set", "eth0"]
    subprocess.run([*cmd, "down"], check=True)
    try:
        twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "requested"')
        source = wait_for(lambda: (r := row("A", "track-a.wav")) and all(r[2:5]) and r)
        twin.ev("A", 'root.rtTr("/Engine/Deck1/Track/QuickCue1SetPosition").value=441000;'
                'root.rtTr("/Engine/Deck1/Track/QuickCue1Name").string="Shadow test cue"; "set cue"')
        source = wait_for(lambda: (r := row("A", "track-a.wav")) and r[5] != source[5] and r)

        def get(path):
            return subprocess.check_output([
                "ip", "netns", "exec", twin.SIDE_CFG["A"]["netns"], "curl", "-sS",
                "http://127.0.0.1:8080" + path,
            ])
        decks = wait_for(lambda: [d for d in json.loads(get("/peer/decks"))["decks"] or [] if d.get("key")])
        assert len(decks) == 1, decks
        query = urlencode({"key": decks[0]["key"], "format": "native"})
        response = get("/peer/decks/1/perf?" + query)
        assert response.startswith(b"RBXA1"), (decks[0], source[:2], [len(b or b"") for b in source[2:]], response[:60])
        print("Source analysis fields:", [len(b or b"") for b in source[2:7]], "flags:", source[7:], flush=True)
    finally:
        subprocess.run([*cmd, "up"], check=True)

    def received():
        logs = "\n".join(p.read_text(errors="replace") for p in (twin.WORK / "B/logs").glob("engine*") if p.is_file())
        if re.search(r'OfflineAnalyzer .*Analyzed "track-a\.wav"', logs):
            candidates = list((twin.WORK / "B/data").rglob("*.analysis")) + list((twin.WORK / "B/data").rglob("*.perf"))
            raise AssertionError(f"the shadow ran OfflineAnalyzer; sidecars: {candidates}")
        r = row("B", "track-a.wav")
        if r and f"shadow analysis filled track {r[0]}" in logs:
            return r
        return None

    try:
        target = wait_for(received)
    except AssertionError:
        r = row("B", "track-a.wav")
        print("Receiver DB row:", r[:2] if r else None, flush=True)
        print("Receiver sidecars:", [p.name for p in (twin.WORK / "B/data/enginex/shadow-v3").rglob("*")], flush=True)
        raise
    assert "/shadow-v3/peer-" in target[1], target[:2]
    assert target[2:7] == source[2:7], "receiving Engine did not store the source blobs"
    def deck_state():
        return twin.evj(
            "B", 'JSON.stringify({loaded: root.rtTr("/Engine/Deck1/Track/SongLoaded").state,'
            'analysed: root.rtTr("/Engine/Deck1/Track/SongAnalyzed").state,'
            'cue: root.rtTr("/Engine/Deck1/Track/QuickCue1Name").string,'
            'bpm: root.rtTr("/Engine/Deck1/OriginalBPM").unnormalized})'
        )

    def ready(state):
        return state["loaded"] and state["analysed"] and state["cue"] == "Shadow test cue" and abs(state["bpm"] - 128) < 0.1

    wait_for(lambda: (state := deck_state()) and ready(state))
    assert received()[:9] == target[:9], "analysis changed during the first load"
    print("Receiver analysis flags:", target[7:], flush=True)
    assert target[7:9] == source[7:9], (source[7:], target[7:])
    sidecars = twin.WORK / "B/data/enginex/shadow-v3"
    assert any(p.read_bytes().startswith(b"EXPF") for p in sidecars.rglob("track-a.wav.perf")), "missing EXPF fallback"
    # The retained EXPF path delays its replay until after the loader settles.
    # Keep reading the deck during that window to catch a reset after load.
    deadline = time.monotonic() + 6
    while time.monotonic() < deadline:
        state = deck_state()
        assert ready(state), state
        time.sleep(0.35)
    # Read /var in the chroot: it is a separate tmpfs mount. Finish a stock
    # analysis on B before checking that A's track never took the replay path.
    twin.ev("B", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck2").url="file:///data/music/track-b.wav"; "requested"')
    wait_for(lambda: (r := row("B", "track-b.wav")) and r[7] == 1)
    log = analyzer_log("B")
    assert "installed" in log, log
    assert "track-a.wav" not in log, "the shadow entered the OfflineAnalyzer:\n" + log
    final = received()
    assert final[:9] == target[:9], [i for i, (a, b) in enumerate(zip(final, target)) if a != b]
    print("PASS: peer shadow received source analysis on first load; no OfflineAnalyzer for that track")


if __name__ == "__main__":
    main()
