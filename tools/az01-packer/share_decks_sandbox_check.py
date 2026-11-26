#!/usr/bin/env python3
"""Checks of share-decks-sandbox.sh (#329 Share Decks). usage: share_decks_sandbox_check.py <sandbox dir>
Times are seconds after the probe's READY (the console's driver got it). Exits 1 on any failed expectation: every
phase must have been seen with enough samples, the Engine must have quit by the probe's own quit (exit 0), and the
console's lane must have drawn the shared track (display data and PNG pictures from the real console service)."""
import json
import re
import sys
from pathlib import Path

run = Path(sys.argv[1])
failures = []
MIN = 5  # samples a window needs before anything about it counts


def check(ok, what):
    print(("  ok   " if ok else "  FAIL ") + what)
    if not ok:
        failures.append(what)


def lines(name):
    p = run / name
    return p.read_text(errors="replace").splitlines() if p.exists() else []


probe = [m.group(1) for l in lines("engine.log") for m in [re.search(r"X329S\|(.*)\|", l)] if m]
print("probe:")
for p in probe:
    print("  | " + p[:220])
evidence = next((p for p in probe if p.startswith("EVIDENCE")), "")
check("isPlayer=true" in evidence, "the player runs the Stock layout (Config.device.isPlayer)")
check(" layer=" in evidence and " layer=absent" not in evidence, "the stock /GUI/Decks/Deck/Layer exists")
check("stopTime=present" in evidence, "the stock Stop Time property /Client/Preferences/RampDownTime exists")
check(not any(p.startswith("ERROR") for p in probe), "the probe ran without error")
check(any(p.startswith("DONE") for p in probe) and any(p.startswith("QUIT at=") for p in probe), "the probe reached its end and quit")
check(not any(p.startswith("QUIT-STUCK") for p in probe), "Engine quit on the probe's quit (no QUIT-STUCK)")
rc = (run / "engine.rc").read_text().strip() if (run / "engine.rc").exists() else "missing"
check(rc == "0", "Engine exited 0 (exit %s; 124 is the sandbox's timeout)" % rc)

console = lines("console.log")
ready = next((int(l.split("|")[2]) for l in console if l.startswith("MARK|ready|")), None)
check(ready is not None, "the console's driver got READY")
if ready is None:
    print("RESULT|FAIL|%d failures" % len(failures))
    sys.exit(1)


def rel(ms):
    return (ms - ready) / 1000.0


lanes = []
for l in console:
    if l.startswith("LANES|"):
        _, ms, phase, _, js = l.split("|", 4)
        lanes.append((rel(int(ms)), phase, json.loads(js)))
pngs = {(l.split("|")[1], l.split("|")[2]): int(l.split("|")[3]) for l in console if l.startswith("PNG|")}
debug = {l.split("|")[1]: json.loads(l.split("|", 2)[2]) for l in console if l.startswith("CONSOLEDEBUG|")}
samples = []
for l in lines("player.log"):
    if l.startswith("SAMPLE|"):
        s = json.loads(l[len("SAMPLE|"):])
        s["rel"] = rel(s["t"])
        samples.append(s)
check(len(samples) > 60, "the player's sampler ran (%d samples)" % len(samples))


def window(a, b):
    w = [s for s in samples if a <= s["rel"] <= b and "error" not in s]
    return w if len(w) >= MIN else []  # too few samples: nothing about the window counts


def lanes_in(a, b):
    w = [ls for t, phase, ls in lanes if a <= t <= b]
    return w if len(w) >= MIN else []


def mine(ls):
    return [x for x in ls if x.get("lineIn") and x.get("model") == "SC5000"]


def con(s):
    return s.get("console") or {}


def active(s):
    return [bool(d.get("active")) for d in (s.get("own") or [])]


# Without an ask: the row is on, the listener is up, and the QML has posted nothing (no sampler, no request but the held one).
w = window(2, 14)
check(bool(w) and all(con(s).get("shares") and con(s).get("listening") and s.get("udp50198") for s in w),
      "no ask: the row is on and the player listens on UDP 50198")
check(bool(w) and all(not con(s).get("asked") and not s.get("own") for s in w), "no ask: nobody asked and the player's QML posted nothing")
check(all(not s.get("running") for s in samples if "error" not in s), "the player's deck loop never ran (no beacon of its own, no pulls)")

# A channel on Line: the console asks, finds the player, and its Line In lane shows the player's deck on the active layer.
first = next((t for t, phase, ls in lanes if phase == "ask" and mine(ls) and mine(ls)[0].get("title")), None)
check(first is not None and first - 15 <= 5, "a channel on Line: the player's deck is on the console's lane %s s after the first ask"
      % (round(first - 15, 1) if first is not None else "never"))
w = window(18, 24)
own = w[-1].get("own") if w else None
title = next((d.get("title") for d in (own or []) if d.get("deck") == 1), "")
check(bool(w) and all(con(s).get("asked") for s in w), "asked: the player knows a console asks")
check(bool(own) and [d.get("deck") for d in own] == [1, 2] and bool(title), "asked: the QML posts Layer A and Layer B, Deck 1 with its track (%r)" % title)
check(bool(w) and all(active(s) == [True, False] for s in w), "asked: Deck 1 is the active layer")
number = re.search(r"playerNumber=(\d+)", evidence)
check(bool(number) and bool(own) and all(d.get("player") == int(number.group(1)) for d in own),
      "asked: the post carries the player number (%s)" % (number.group(1) if number else "none in Engine"))
check(bool(own) and own[0].get("playing") is True, "asked: Deck 1 plays")
steady = lanes_in(18, 24)
check(bool(steady) and all(any(x.get("title") == title and x.get("playing") and x.get("displayOnly") for x in mine(ls)) for ls in steady),
      "the console's Line In lane: the player's deck, playing, display only, with its model")
d = debug.get("ask") or {}
check("10.79.0.2" in (d.get("peers") or []) and d.get("running"), "the console found the player by its answer: %s" % d)

# The picture: the real console service drew the shared track (duration and beat grid of the 120 s, 128 BPM test track,
# and PNG pictures: the overview and a detail tile).
shown = [x.get("display") for t, phase, ls in lanes if 15 <= t <= 45 for x in mine(ls) if x.get("title") == title and x.get("display")]
good = [d for d in shown if 119000 <= (d.get("durationMs") or 0) <= 121000 and (d.get("beats") or 0) >= 200]
check(len(good) >= 3, "the console's display data for the shared track: duration and beat grid (%d of %d fetches; last %s)"
      % (len(good), len(shown), shown[-1] if shown else "-"))
check(any(n == "overview" for (n, k) in pngs) and any(n == "tile" for (n, k) in pngs),
      "the console drew the shared track: overview and detail tile PNGs (%s)" % sorted(pngs.items()))

# The stock Layer: B moves the lane to Deck 2 (no track: the lane leaves Deck 1's), A back to Deck 1. Both required.
layer = [p for p in probe if p.startswith("LAYER")]
moved = len(layer) == 2 and all(m and m.group(1) == m.group(2) for m in (re.search(r"want=(\d) now=(\d)", p) for p in layer))
check(moved, "the probe moved the stock Layer to B and back to A (%s)" % layer)
w = window(27, 32)
check(bool(w) and all(active(s) == [False, True] for s in w[1:]), "Layer B: Deck 2 is the active one (%d samples)" % len(w))
ls = lanes_in(27, 32)
check(bool(ls) and all(not any(x.get("title") == title for x in mine(l)) for l in ls[1:]), "Layer B: the lane leaves Deck 1's track (%d polls)" % len(ls))
w = window(36, 44)
check(bool(w) and all(active(s) == [True, False] for s in w[1:]), "Layer A again: Deck 1 is the active one (%d samples)" % len(w))
ls = lanes_in(36, 44)
check(bool(ls) and all(any(x.get("title") == title for x in mine(l)) for l in ls[1:]), "Layer A again: the lane shows Deck 1's track (%d polls)" % len(ls))

# The last channel leaves Line, the row stays on: the console stops asking within its grace (3 s, plus a beacon period
# and a pull), the player's last ask runs out 6 s later and its QML stops; the shared decks go with the session.
last_poll = max((t for t, phase, ls in lanes if phase == "ask"), default=None)
released = next((s["rel"] for s in samples if last_poll is not None and s["rel"] > last_poll and "error" not in s and not con(s).get("asked")), None)
latency = None if released is None or last_poll is None else round(released - last_poll, 1)
check(latency is not None and latency <= 12.0, "the last channel left Line: the player was released %s s after the console's last Line poll" % latency)
w = window(57, 65.5)
check(bool(w) and all(not con(s).get("asked") and not s.get("own") for s in w), "silence: no ask, the QML stopped, the shared decks are gone")
check(bool(w) and all(con(s).get("shares") and con(s).get("listening") for s in w), "silence: the row stays on and the player still listens")

# A channel on Line again: a new ask shows the player again; then the row goes off while the console asks.
ls = lanes_in(68, 75.5)
check(bool(ls) and any(any(x.get("title") == title for x in mine(l)) for l in ls), "asked again: the lane shows the player's deck again")
row_off = next((p for p in probe if p.startswith("ROW off")), "")
check(bool(row_off), "the probe switched the row off during the ask")
w = window(79, 89)
check(bool(w) and all(not con(s).get("shares") and not con(s).get("listening") and not s.get("udp50198") for s in w),
      "row off during an ask: no listener on UDP 50198")
check(bool(w) and all(not s.get("own") and not con(s).get("asked") for s in w), "row off during an ask: the shared decks and the asks are forgotten")
ls = lanes_in(79, 89)
check(bool(ls) and all(not mine(l) for l in ls), "row off during an ask: the console's Line In lane drops the player")
# The console may still know the player's address (its pulls are answered, with nothing): it holds none of its decks.
d = debug.get("ask-row-off") or {}
check(d.get("running") is True and d.get("peerDecks") == 0, "row off: the asking console holds none of the player's decks: %s" % d)


# Cost: CPU time per phase as a share of one core: the Engine (all threads, and the GUI thread that runs the QML sampler),
# the player's stream-sync and the console's stream-sync.
def rate(a, b, key):
    w = [s for s in samples if a <= s["rel"] <= b and key in s]
    if len(w) < MIN:
        return None
    return round(100.0 * (w[-1][key] - w[0][key]) / ((w[-1]["t"] - w[0]["t"]) / 1000.0), 1)


phases = (("no ask, playing", 3, 14), ("asked, playing", 17, 24), ("asked, later", 36, 44), ("after the asks", 57, 65.5),
          ("asked again", 68, 75.5), ("row off", 79, 89))
print("cost (% of one core: Engine all threads / Engine GUI thread / player stream-sync / console stream-sync):")
complete = True
for name, a, b in phases:
    vals = [rate(a, b, k) for k in ("cpu", "gui", "sync", "csync")]
    complete = complete and all(v is not None for v in vals)
    print("  COST|%s|%s" % (name, "|".join(str(v) for v in vals)))
check(complete, "CPU time was sampled for every phase and process")
print("RESULT|%s|%d failures" % ("PASS" if not failures else "FAIL", len(failures)))
sys.exit(1 if failures else 0)
