#!/usr/bin/env python3
"""Helpers of test_console_link_sandbox.sh for console linking stage 2 (a player shows the console's decks), run with real
stream-syncs and no Engine: the QML of the console and of the player are played by these drivers, over loopback only.

  console <t0>        the console's QML: Line In set per phase; a held request to the console's stream-sync (what the real QML
                      does), and while it is told that a player asks, its decks posted twice a second; nothing otherwise.
  player <t0> <row>   the player's QML: the row Show Console set per phase (row=0: an older player, which has no such row),
                      its own decks 3 and 4 posted, /v1/enginex/shadow polled every second, the console lanes it answers logged.
  sample <t0>         the console's stream-sync as seen from outside: its state at the end of each phase (SAMPLE).
  beacon <ip> <secs>  a console whose deck loop runs: a plain beacon to a player every 2 s (a console of 5.3.0.5-r1 never
                      beacons by itself, and the new one only after an ask; this makes the player find it either way).
  asker <secs>        a second, new player as far as the console can tell: a pull with console=1 every 250 ms.
t0 is an epoch in seconds all of them share. Output is one line per fact."""
import json
import socket
import sys
import threading
import time
import urllib.request

BASE = "http://127.0.0.1:8080"
# name, seconds, row Show Console on the player, Line In on the console, decks the console would send, whether the player polls
PHASES = [
    ("row-off", 14, 0, 1, [1, 2], 1),
    ("row-on", 18, 1, 1, [1, 2], 1),
    ("line-1", 14, 1, 1, [2], 1),  # channel 1 switched to Line: deck 1 leaves the post
    ("row-off-again", 16, 0, 1, [1, 2], 1),
    ("player-gone", 26, 1, 1, [1, 2], 0),  # the player stops asking: nobody polls, its loop ends
    ("console-stock", 16, 1, 0, [1, 2], 1),  # Line In off on the console: it listens to nobody
]
END = sum(p[1] for p in PHASES)


def phase_at(t0, now=None):
    t = (time.time() if now is None else now) - t0
    acc = 0
    for i, p in enumerate(PHASES):
        if t < acc + p[1]:
            return i, t - acc
        acc += p[1]
    return len(PHASES), 0


def req(method, path, obj=None, timeout=4):
    r = urllib.request.Request(BASE + path, data=json.dumps(obj).encode() if obj is not None else (b"" if method == "POST" else None), method=method)
    with urllib.request.urlopen(r, timeout=timeout) as resp:
        return resp.read()


def get(path, timeout=4):
    return json.loads(req("GET", path, timeout=timeout) or b"{}")


def udp_bound(port):
    want = ":%04X" % port
    for f in ("/proc/net/udp", "/proc/net/udp6"):
        try:
            for line in open(f).read().splitlines()[1:]:
                if line.split()[1].upper().endswith(want):
                    return True
        except OSError:
            pass
    return False


def console(t0):
    state = {"asked": False, "on": None, "posts": {}}
    lock = threading.Lock()

    def hold():  # the held request, as the QML sends it: carries what it believes now
        while time.time() - t0 < END + 2:
            i, _ = phase_at(t0)
            if i >= len(PHASES) or not PHASES[i][3]:
                time.sleep(0.3)
                continue
            try:
                a = bool(get("/v1/enginex/consolesend?asked=%d" % (1 if state["asked"] else 0), timeout=35).get("asked"))
            except Exception as e:
                print("ASKERR|%s" % e, flush=True)
                time.sleep(1)
                continue
            if a != state["asked"]:
                state["asked"] = a
                i, off = phase_at(t0)
                print("ASKED|%d|%s|%.1f|%s" % (int(time.time() * 1000), PHASES[min(i, len(PHASES) - 1)][0], off, a), flush=True)

    threading.Thread(target=hold, daemon=True).start()
    while time.time() - t0 < END + 2:
        i, off = phase_at(t0)
        if i >= len(PHASES):
            break
        name, _, _, line_in, decks, _ = PHASES[i]
        if state["on"] != line_in:  # Line In is the console's own setting: the QML's switch
            try:
                req("POST", "/v1/enginex/ui?consoleLineView=%d" % line_in)
                state["on"] = line_in
                if not line_in:
                    state["asked"] = False  # the QML aborts its request and forgets
            except Exception as e:
                print("UIERR|%s" % e, flush=True)
        if state["asked"] and line_in:
            now = int(time.time() * 1000)
            body = [{"deck": d, "title": "C%d" % d, "net": "net://u/(Unknown)/enginex/Stream/a.mp3", "link": "{c0ns01e}", "playing": True, "positionMs": 1000, "at": now} for d in decks]
            try:
                req("POST", "/v1/enginex/decks", body)
                state["posts"].setdefault(name, []).append(round(off, 1))
            except Exception as e:
                print("POSTERR|%s" % e, flush=True)
        time.sleep(0.5)
    for name, _, *_ in PHASES:
        offs = state["posts"].get(name, [])
        print("POSTS|%s|%d|last=%s" % (name, len(offs), offs[-1] if offs else "-"), flush=True)


def player(t0, row):
    state = {"row": None}
    last = None
    while time.time() - t0 < END + 2:
        i, off = phase_at(t0)
        if i >= len(PHASES):
            break
        name, _, want, _, _, polls = PHASES[i]
        if row and state["row"] != want:
            try:
                req("POST", "/v1/enginex/ui?consoleDecks=%d" % want)
                state["row"] = want
                print("ROW|%s|%d" % (name, want), flush=True)
            except Exception as e:
                print("ROWERR|%s" % e, flush=True)
        if not polls:
            time.sleep(0.5)
            continue
        now = int(time.time() * 1000)
        own = [{"deck": d, "title": "P%d" % d, "net": "net://u/(Unknown)/enginex/Stream/a.mp3", "playing": True, "positionMs": 2000, "at": now, "player": 1, "active": d == 3} for d in (3, 4)]
        try:
            req("POST", "/v1/enginex/decks", own)
            r = get("/v1/enginex/shadow")
            lanes = sorted((d["deck"], d.get("console", 0), d.get("model", ""), bool(d.get("displayOnly")), d.get("title", "")) for d in (r.get("decks") or []))
            print("LANES|%d|%s|%.1f|%s" % (int(time.time() * 1000), name, off, json.dumps(lanes)), flush=True)
            last = lanes
        except Exception as e:
            print("LANEERR|%s" % e, flush=True)
        # the end of every phase: what the player's stream-sync holds
        if off > PHASES[i][1] - 1.6:
            try:
                d = get("/v1/enginex/debug/shadow")
                print("PLAYERDEBUG|%s|%s" % (name, json.dumps(d.get("console") or {})), flush=True)
            except Exception as e:
                print("PLAYERDEBUGERR|%s" % e, flush=True)
        time.sleep(1.0)
    print("PLAYERDONE|%s" % json.dumps(last), flush=True)


def sample(t0):
    seen = set()
    while time.time() - t0 < END + 2:
        i, off = phase_at(t0)
        if i < len(PHASES) and off > PHASES[i][1] - 1.2 and i not in seen:
            seen.add(i)
            row = {"phase": PHASES[i][0]}
            try:
                d = get("/v1/enginex/debug/shadow")
                row.update(running=d.get("running"), console=d.get("console") or {}, peers=len(d.get("peers") or {}), ownAgeMs=d.get("ownAgeMs"), own=len(d.get("own") or []))
            except Exception as e:
                row["error"] = str(e)
            row["udp50198"], row["udp50199"] = udp_bound(50198), udp_bound(50199)
            print("SAMPLE|" + json.dumps(row), flush=True)
        time.sleep(0.4)


def asker(secs):
    """Another new player, as far as the console can tell: a pull with console=1 every 250 ms, and nothing else."""
    end = time.time() + secs
    while time.time() < end:
        try:
            req("GET", "/peer/decks?console=1", timeout=2)
        except Exception:
            pass
        time.sleep(0.25)


def beacon(ip, secs):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    end = time.time() + secs
    while time.time() < end:
        s.sendto(b"EngineX-shadow 1", (ip, 50199))
        time.sleep(2)


if __name__ == "__main__":
    role = sys.argv[1]
    if role == "console":
        console(float(sys.argv[2]))
    elif role == "player":
        player(float(sys.argv[2]), int(sys.argv[3]))
    elif role == "sample":
        sample(float(sys.argv[2]))
    elif role == "beacon":
        beacon(sys.argv[2], float(sys.argv[3]))
    elif role == "asker":
        asker(float(sys.argv[2]))
