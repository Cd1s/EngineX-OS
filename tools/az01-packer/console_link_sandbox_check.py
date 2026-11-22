#!/usr/bin/env python3
"""Checks of test_console_link_sandbox.sh. usage: console_link_sandbox_check.py stub|pair|shots <run dir>
Reads what console-link-probe.qml logged in the console's Engine log (CLPHASE lines) and, per mode, the stub's
request counters (stub) or the samples and the player's views (pair). Prints the zero-cost table and exits 1 on
any failed expectation. The expectations are the stage-1 decisions: display only, nothing runs while no channel is
on Line / AUX, one display poll loop with one, no deck of the console ever held back."""
import json
import re
import sys
from pathlib import Path

mode, run = sys.argv[1], Path(sys.argv[2])
SETTLE_MS, LEN_MS = 8000, 18000
IDLE = ("off", "on-noline", "release-all", "off-again")
failures = []


def rep(tm):  # the repeating timers of a "[a,b,c]" list: a one-shot is a transient (a settle or retry timer) that may or may not be caught
    return sorted(t for t in (tm or "").strip("[]").split(",") if t.endswith("r"))


def check(ok, what):
    print(("  ok   " if ok else "  FAIL ") + what)
    if not ok:
        failures.append(what)


log = (run / "engine.log").read_text(encoding="utf-8", errors="replace")
begin, state, shots = {}, {}, {}
for m in re.finditer(r"CLPHASE\|([^ ]*)", log):
    parts = m.group(1).split("|")
    if parts[0] == "BEGIN":
        begin[parts[1]] = int(parts[2])
    elif parts[0] == "STATE":
        state.setdefault(parts[1], {}).update(dict(kv.split("=", 1) for kv in parts[2:] if "=" in kv))
    elif parts[0] == "TM":
        state.setdefault(parts[1], {})["tm"] = parts[2]
    elif parts[0] == "LABELS":
        state.setdefault(parts[1], {})["labels"] = parts[2]
    elif parts[0] == "LAYOUTNOW":
        state.setdefault(parts[1], {})["layout"] = parts[2]
    elif parts[0] == "HYBRID":
        state.setdefault(parts[1], {})["hybrid"] = parts[2]
    elif parts[0] == "SHOT":
        shots[parts[1]] = parts[2] == "true"
    elif parts[0] == "LINEWRITE":
        print("probe: " + parts[1].replace("_", " "))
names = ["off", "on-noline", "on-line3", "on-line34", "release4", "on-line234", "release-all", "off-again"]
check(all(n in begin for n in names), "every phase began (%s)" % ",".join(n for n in names if n in begin))
check(all(n in state for n in names), "every phase reported its state")
print("stock hasHybridMode on this board: %s (stock's LoadToDeckFilter refuses a load onto a Line channel only without it)" % state.get("off", {}).get("hybrid"))

# What the QML holds, in every phase (both modes).
for n in names:
    s = state.get(n, {})
    check(s.get("role") == "-", "%s: scFourDeckRole stays empty on the console (role=%s)" % (n, s.get("role")))
    check(s.get("lk") == "0000", "%s: no deck is locked (lk=%s)" % (n, s.get("lk")))
    check(s.get("ld") == "1111", "%s: every deck stays loadable (ld=%s)" % (n, s.get("ld")))
for n in IDLE:
    s = state.get(n, {})
    check(s.get("live") == "0" and s.get("vis") == "0000" and s.get("labels") == "[]", "%s: no lane, no label (live=%s vis=%s)" % (n, s.get("live"), s.get("vis")))
check(state.get("off", {}).get("on") == "0" and state.get("on-noline", {}).get("on") == "1" and state.get("off-again", {}).get("on") == "0", "the switch is followed live (off, on, off)")
check(state.get("on-noline", {}).get("mask") == "0", "on, no channel on Line: mask 0")
# The capture run draws with software GL: the GUI is far too slow to read timings from it, so it checks lanes and labels only.
if mode != "shots":
    # The very first phase still has Engine's start-up one-shot (30 s); it is not ours and is gone by the second.
    base = state.get("on-noline", {}).get("tm")
    check(base is not None and all(state.get(n, {}).get("tm") == base for n in ("release-all", "off-again")) and state.get("off", {}).get("tm", "").replace("30000o,", "") == base,
          "timers running on the console are the same in every idle phase, switch on or off: %s" % base)
    s3, s34, s4, s234 = state.get("on-line3", {}), state.get("on-line34", {}), state.get("release4", {}), state.get("on-line234", {})
    check(s3.get("mask") == "8" and s3.get("live") == "1" and s3.get("vis") == "0010", "on-line3: mask 8, live, only channel 3 shows a lane (vis=%s)" % s3.get("vis"))
    units = 1 if mode == "pair" else 2  # the pair run has one linked player, the stub two units: a channel on Line with no unit shows no lane
    want34 = "0011" if units == 2 else "0010"
    check(s34.get("mask") == "24" and s34.get("vis") == want34, "on-line34: mask 24, a lane for each of the %d unit(s) (vis=%s)" % (units, s34.get("vis")))
    want234 = "0111" if units == 2 else "0100"  # three units on the stub's network: channels 2, 3 and 4 all show one; the pair run has one
    check(s234.get("mask") == "28" and s234.get("vis") == want234, "on-line234: mask 28, a lane for each of the %d unit(s) present, none locked, all loadable (vis=%s lk=%s ld=%s)" % (3 if units == 2 else 1, s234.get("vis"), s234.get("lk"), s234.get("ld")))
    check(state.get("release4-now", {}).get("vis") == "0010", "release4: channel 4's lane is gone in the same turn as the channel change (vis=%s)" % state.get("release4-now", {}).get("vis"))
    check(s4.get("vis") == "0010", "release4: channel 3 keeps its lane (vis=%s)" % s4.get("vis"))
    check(s3.get("tm") != base and rep(s234.get("tm")) == rep(s3.get("tm")), "on-line3 and on-line234: the same poll and display timers run, one or three lanes (%s and %s against %s when idle)" % (s3.get("tm"), s234.get("tm"), base))
check(all(shots.get(n) for n in ("off", "on-line3", "on-line34")), "one capture per state saved (%s)" % shots)
check(all(state.get(n, {}).get("layout") == "4_Decks/4_Waveforms" for n in names),
      "the performance view shows four decks and four waveforms in every phase, so the captures differ only by the Line In view (%s)" % {n: state.get(n, {}).get("layout") for n in names})

def totals(rows):
    """Over the whole run, pictures are fetched once per lane key that is shown, never again while it stays: the plan shows five
    keys (player-deck on a channel): 1-3, 2-4 (channels 3 and 4), then 1-2, 2-3, 3-4 (channels 2, 3 and 4 with three units)."""
    c = rows[-1]["counts"]
    keys = 5
    d, t = c.get("GET /v1/enginex/shadow/display", 0), c.get("GET /tile.png", 0)
    check(d == keys, "the whole run asked for the display of %d lane keys, once each (%d requests)" % (keys, d))
    check(0 < t <= 4 * keys, "the whole run fetched at most four pictures per lane key (%d tile requests for %d keys)" % (t, keys))
    check(c.get("POST /v1/enginex/decks", 0) == 0 and c.get("POST /v1/enginex/decks/alive", 0) == 0, "the whole run: the console posted no deck")


if mode == "shots":
    lab = {n: state.get(n, {}).get("labels", "") for n in ("on-line3", "on-line34", "on-line234")}
    check("Linked_player_1_" in lab["on-line3"], "on-line3: the lane on channel 3 is drawn and labelled (%s)" % lab["on-line3"])
    check("Linked_player_1_" in lab["on-line34"] and "Linked_player_2_" in lab["on-line34"], "on-line34: both lanes are drawn and labelled (%s)" % lab["on-line34"])
    check(all(("Linked_player_%d_" % n) in lab["on-line234"] for n in (1, 2, 3)), "on-line234: three lanes are drawn, each says its unit (%s)" % lab["on-line234"])
    totals([json.loads(l) for l in (run / "stats.jsonl").read_text().splitlines() if l.strip()])
elif mode == "stub":
    rows = [json.loads(l) for l in (run / "stats.jsonl").read_text().splitlines() if l.strip()]
    totals(rows)
    KINDS = ["GET /v1/enginex/shadow", "GET /v1/enginex/shadow/display", "GET /tile.png", "POST /v1/enginex/decks", "POST /v1/enginex/decks/alive", "GET /v1/enginex/ui", "GET /v1/enginex/views"]

    def at(t):
        best = {}
        for r in rows:
            if r["t"] <= t:
                best = r["counts"]
        return best

    print("\nrequests per minute, by kind (window: the last 10 s of each phase, x6)")
    print("%-14s" % "phase" + "".join("%-9s" % k.split("/v1/enginex/")[-1].replace("GET ", "g ").replace("POST ", "p ")[:8] for k in KINDS))
    rate = {}
    for n in names:
        if n not in begin:
            continue
        a, b = at((begin[n] + SETTLE_MS) / 1000), at((begin[n] + LEN_MS) / 1000)
        rate[n] = {k: (b.get(k, 0) - a.get(k, 0)) * 6 for k in KINDS + [x for x in b if x.startswith("lines ")]}
        print("%-14s" % n + "".join("%-9d" % rate[n][k] for k in KINDS))
    zero = KINDS[:5]
    for n in IDLE:
        check(all(rate.get(n, {}).get(k, 1) == 0 for k in zero), "%s: no shadow, display, tile or deck request at all" % n)
    for n, want in (("on-line3", "lines 3"), ("on-line34", "lines 3,4"), ("release4", "lines 3"), ("on-line234", "lines 2,3,4")):
        r = rate.get(n, {})
        check(30 <= r.get("GET /v1/enginex/shadow", 0) <= 150, "%s: one display poll loop, about one request a second (%d a minute)" % (n, r.get("GET /v1/enginex/shadow", 0)))
        check(r.get("POST /v1/enginex/decks", 1) == 0 and r.get("POST /v1/enginex/decks/alive", 1) == 0, "%s: the console sends no deck" % n)
        check(r.get(want, 0) > 0 and all(v == 0 for k, v in r.items() if k.startswith("lines ") and k != want), "%s: it asks for exactly the channels on Line (%s)" % (n, want))
        check(r.get("GET /tile.png", 99) <= 6, "%s: pictures are fetched only when they change (%d tile fetches a minute)" % (n, r.get("GET /tile.png", 99)))
else:
    samples, views = {}, []
    for line in (run / "sample.log").read_text(errors="replace").splitlines():
        if line.startswith("SAMPLE|"):
            row = json.loads(line[7:])
            samples[row["phase"]] = row
    for line in (run / "player.log").read_text(errors="replace").splitlines():
        if line.startswith("PLAYERVIEW|") and not line.startswith("PLAYERVIEW|error"):
            views.append(line.split("|", 2)[2])
    print("\nconsole stream-sync, sampled at the end of each phase")
    for n in names:
        print("  %-12s %s" % (n, {k: v for k, v in samples.get(n, {}).items() if k != "phase"}))
    for n in IDLE:
        r = samples.get(n, {})
        never = n in ("off", "on-noline")  # nothing has polled yet: not even a stale peer is listed
        # release-all is sampled 15 s after the last Line channel went; the loop ends 10 s after its last poll, so a peer heard
        # just before that is still inside the 6 s freshness window: only the loop and the port are asserted there.
        quiet = r.get("fresh") == 0 or n == "release-all"
        check(r.get("running") is False and r.get("udp50199") is False and quiet and (r.get("peers") == 0 or not never) and "lanes" not in r,
              "%s: the console's deck loop is not running, UDP 50199 is not bound%s" % (n, "" if n == "release-all" else ", no fresh peer"))
    for n, first in (("on-line3", 3), ("on-line34", 3), ("release4", 3), ("on-line234", 2)):
        units = 1  # the pair run has one linked player
        r = samples.get(n, {})
        check(r.get("running") is True and r.get("udp50199") is True and r.get("own") == 0, "%s: polled with a Line channel: the loop runs, UDP 50199 is bound, the console holds no deck of its own" % n)
        lanes = r.get("lanes") or []
        check(len(lanes) == units and lanes[0][:6] == [first, "Player one deck 3", 1, "SC6000", True, True] and lanes[0][6:] == ["", ""], "%s: the player's active-layer deck is on the first Line channel (%d), display only, no file (%s)" % (n, first, lanes))
    check(len(views) > 20 and all(v == "[]" for v in views), "the player sees no console deck in any phase (%d answers, all empty)" % len(views))
    # What the lane is labelled with is drawn and asserted in the shots run (1280x800); this run's offscreen window is 800x800, a
    # layout without a waveform area, so no label is in the scene here. The answer above is what the label is built from.

print("\nCONSOLE_LINE_%s_%s" % (mode.upper(), "FAIL" if failures else "PASS"))
sys.exit(1 if failures else 0)
