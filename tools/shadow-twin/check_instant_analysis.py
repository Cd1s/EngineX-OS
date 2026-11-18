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
            "SELECT t.id,t.path,p.trackData,p.overviewWaveFormData,p.beatData,p.quickCues,p.loops "
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


def main():
    assert os.environ.get("SHADOW_TWIN_WORK"), "use a fresh SHADOW_TWIN_WORK"
    assert row("A", "track-a.wav") is None and row("B", "track-a.wav") is None
    (twin.WORK / "B/data/music/track-a.wav").unlink()
    cmd = ["ip", "-n", twin.SIDE_CFG["B"]["netns"], "link", "set", "eth0"]
    subprocess.run([*cmd, "down"], check=True)
    try:
        twin.ev("A", 'root.rtTr("/Client/Librarian/AddToDatabaseAndLoadToDeck1").url="file:///data/music/track-a.wav"; "requested"')
        source = wait_for(lambda: (r := row("A", "track-a.wav")) and all(r[2:5]) and r)

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
        print("Source analysis fields:", [len(b or b"") for b in source[2:]], flush=True)
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
    assert target[2:] == source[2:], "receiving Engine did not store the source blobs"
    def loaded():
        state = twin.evj(
            "B", 'JSON.stringify({loaded: root.rtTr("/Engine/Deck1/Track/SongLoaded").state,'
            'bpm: root.rtTr("/Engine/Deck1/OriginalBPM").unnormalized})'
        )
        if state["loaded"]:
            assert abs(state["bpm"] - 128) < 0.1, state
            return state
        return None

    wait_for(loaded)
    assert received() == target
    analyzer_log = twin.WORK / "B/upper/var/log/enginex-uimod-oa.log"
    assert "track-a.wav" not in analyzer_log.read_text(), "the shadow entered the EXPF replay"
    print("PASS: peer shadow received source analysis on first load; no OfflineAnalyzer for that track")


if __name__ == "__main__":
    main()
