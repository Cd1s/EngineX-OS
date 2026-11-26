#!/usr/bin/env python3
"""Checks of share-decks-sandbox.sh (#329 Share Decks). usage: share_decks_sandbox_check.py <sandbox dir>
Times are seconds after the probe's READY (the console's driver got it). Exits 1 on any failed expectation."""
import json
import re
import sys
from pathlib import Path

run = Path(sys.argv[1])
failures, warnings = [], []


def check(ok, what):
    print(("  ok   " if ok else "  FAIL ") + what)
    if not ok:
        failures.append(what)


def warn(what):
    print("  WARN " + what)
    warnings.append(what)


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
check(any(p.startswith("DONE") for p in probe), "the probe reached its end")

console = lines("console.log")
ready = next((int(l.split("|")[2]) for l in console if l.startswith("MARK|ready|")), None)
check(ready is not None, "the console's driver got READY")
if ready is None:
    sys.exit(1)


def rel(ms):
    return (ms - ready) / 1000.0


lanes = []
for l in console:
    if l.startswith("LANES|"):
        _, ms, phase, _, js = l.split("|", 4)
        lanes.append((rel(int(ms)), phase, json.loads(js)))
debug = {l.split("|")[1]: json.loads(l.split("|", 2)[2]) for l in console if l.startswith("CONSOLEDEBUG|")}
samples = []
for l in lines("player.log"):
    if l.startswith("SAMPLE|"):
        s = json.loads(l[len("SAMPLE|"):])
        s["rel"] = rel(s["t"])
        samples.append(s)
check(len(samples) > 60, "the player's sampler ran (%d samples)" % len(samples))


def window(a, b):
    return [s for s in samples if a <= s["rel"] <= b and "error" not in s]


def lane_of_player(ls):
    return [x for x in ls if x.get("lineIn") and x.get("model") == "SC5000"]


# Without an ask: the row is on, the listener is up, and the QML has posted nothing (no sampler, no request but the held one).
w = window(2, 14)
check(w and all((s.get("console") or {}).get("shares") and (s.get("console") or {}).get("listening") and s.get("udp50198") for s in w),
      "no ask: the row is on and the player listens on UDP 50198")
check(w and all(not (s.get("console") or {}).get("asked") and not s.get("own") for s in w),
      "no ask: nobody asked and the player's QML posted nothing")
check(all(not s.get("running") for s in samples if "error" not in s), "the player's deck loop never ran (no beacon of its own, no pulls)")

# A channel on Line: the console asks, finds the player, and its Line In lane shows the player's deck on the active layer.
first = next((t for t, phase, ls in lanes if phase == "ask" and lane_of_player(ls) and lane_of_player(ls)[0].get("title")), None)
check(first is not None and first - 15 <= 5, "a channel on Line: the player's deck is on the console's lane %s s after the first ask"
      % (round(first - 15, 1) if first is not None else "never"))
w = window(18, 24)
own = w[-1].get("own") if w else None
title = next((d.get("title") for d in (own or []) if d.get("deck") == 1), "")
check(w and all((s.get("console") or {}).get("asked") for s in w), "asked: the player knows a console asks")
check(bool(own) and [d.get("deck") for d in own] == [1, 2] and bool(title), "asked: the QML posts Layer A and Layer B, Deck 1 with its track (%r)" % title)
check(bool(own) and [bool(d.get("active")) for d in own] == [True, False], "asked: Deck 1 is the active layer")
number = re.search(r"playerNumber=(\d+)", evidence)
if number and 1 <= int(number.group(1)) <= 8:
    check(bool(own) and all(d.get("player") == int(number.group(1)) for d in own), "asked: the post carries the player number (%s)" % number.group(1))
else:
    warn("Engine has no player number here (%s): the post leaves it out, as on any unit that does not know it" % evidence)
check(bool(own) and own[0].get("playing") is True, "asked: Deck 1 plays")
steady = [ls for t, phase, ls in lanes if 18 <= t <= 24]
check(steady and all(any(x.get("title") == title and x.get("playing") and x.get("displayOnly") for x in lane_of_player(ls))
                     for ls in steady), "the console's Line In lane: the player's deck, playing, display only, with its model")
d = debug.get("ask") or {}
check("10.79.0.2" in (d.get("peers") or []) and d.get("running"), "the console found the player by its answer: %s" % d)

# The stock Layer: B moves the lane to Deck 2, A back to Deck 1.
layer = [p for p in probe if p.startswith("LAYER")]
moved = layer and all(re.search(r"want=(\d) now=(\d)", p) and re.search(r"want=(\d) now=(\d)", p).group(1) == re.search(r"want=(\d) now=(\d)", p).group(2) for p in layer)
if not moved:
    warn("the probe could not move the stock Layer here (%s): the active flag is covered by the unit test only" % layer)
else:
    w = window(27, 32)
    check(w and all([bool(d.get("active")) for d in (s.get("own") or [])] == [False, True] for s in w[1:]), "Layer B: Deck 2 is the active one")
    ls = [ls for t, phase, ls in lanes if 27 <= t <= 32]
    check(ls and all(not any(x.get("title") == title for x in lane_of_player(l)) for l in ls[1:]), "Layer B: the lane leaves Deck 1's track")
    w = window(36, 44)
    check(w and all([bool(d.get("active")) for d in (s.get("own") or [])] == [True, False] for s in w[1:]), "Layer A again: Deck 1 is the active one")
    ls = [ls for t, phase, ls in lanes if 36 <= t <= 44]
    check(ls and all(any(x.get("title") == title for x in lane_of_player(l)) for l in ls[1:]), "Layer A again: the lane shows Deck 1's track")

# Silence: the console's loop asks on for 10 s after its QML's last poll, then nobody asks for 6 s: the QML is told and stops.
w = window(64, 71)
check(w and all(not (s.get("console") or {}).get("asked") for s in w), "silence: the asks ran out")
check(w and all((s.get("ownAgeMs") or 0) > 2000 for s in w), "silence: the QML stopped posting (newest post %s ms old)" % ([s.get("ownAgeMs") for s in w][-1:] or "-"))

# The row off on the player: no listener, nothing kept, and the console that asks again finds nothing.
w = window(74, 87)
check(w and all(not (s.get("console") or {}).get("shares") and not (s.get("console") or {}).get("listening") and not s.get("udp50198") for s in w),
      "row off: no listener on UDP 50198")
check(w and all(not s.get("own") for s in w), "row off: the shared decks are forgotten")
ls = [ls for t, phase, ls in lanes if 74 <= t <= 87]
check(ls and all(not lane_of_player(l) for l in ls), "row off: the console's Line In lane does not show the player")
d = debug.get("row-off") or {}
check(d.get("running") is True and "10.79.0.2" not in (d.get("peers") or []), "row off: the asking console does not find the player: %s" % d)


# Cost: the Engine's CPU time per phase (all threads, and the GUI thread that runs the QML sampler), as a share of one core.
def rate(a, b, key):
    w = [s for s in samples if a <= s["rel"] <= b and key in s]
    if len(w) < 2:
        return None
    return round(100.0 * (w[-1][key] - w[0][key]) / ((w[-1]["t"] - w[0]["t"]) / 1000.0), 1)


print("cost (Engine CPU, % of one core: all threads / GUI thread):")
for name, a, b in (("no ask, playing", 3, 14), ("asked, playing", 17, 24), ("asked again later", 38, 44),
                   ("silence end", 63, 71), ("row off", 76, 87)):
    print("  %-20s %s / %s" % (name, rate(a, b, "cpu"), rate(a, b, "gui")))
print("RESULT|%s|%d failures, %d warnings" % ("PASS" if not failures else "FAIL", len(failures), len(warnings)))
sys.exit(1 if failures else 0)
