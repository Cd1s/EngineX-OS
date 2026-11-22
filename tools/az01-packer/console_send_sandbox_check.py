#!/usr/bin/env python3
"""Checks of the console linking stage 2 modes of test_console_link_sandbox.sh. usage: console_send_sandbox_check.py <mode> <run dir>
  send              the console's real Engine and pack against a stub of its stream-sync: with Line In on and nobody asking it
                    posts nothing and runs no sampler; asked, it samples and posts; nobody asks again: it stops
  playershots       the same Engine standing in for a player (4-deck layout, role 1+2): with the row off no lane is the
                    console's, with the row on the free lanes 3 and 4 show two console decks, labelled
  sendpair          real stream-syncs (new console, new player) and drivers in place of the two QMLs: the whole life of the row
  sendoldplayer     the same with the player of 5.3.0.5-r1 (the binary of 147e37fb): it must get what it always got from a console
  sendoldconsole    the console of 5.3.0.5-r1 beside the new player with the row on: nothing may show, nothing may break
Exits 1 on any failed expectation."""
import json
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import console_send_pair_driver as drv  # the phase table the drivers ran by

mode, run = sys.argv[1], Path(sys.argv[2])
failures = []


def check(ok, what):
    print(("  ok   " if ok else "  FAIL ") + what)
    if not ok:
        failures.append(what)


def read(name):
    p = run / name
    return p.read_text(encoding="utf-8", errors="replace") if p.exists() else ""


def rep(tm):
    return sorted(t for t in (tm or "").strip("[]").split(",") if t.endswith("r"))


if mode in ("send", "playershots"):
    SETTLE_MS, LEN_MS = 8000, 18000
    log = read("engine.log")
    begin, state, s2, labels, shots = {}, {}, {}, {}, {}
    for m in re.finditer(r"CLPHASE\|([^ ]*)", log):
        parts = m.group(1).split("|")
        if parts[0] == "BEGIN":
            begin[parts[1]] = int(parts[2])
        elif parts[0] == "TM":
            state.setdefault(parts[1], {})["tm"] = parts[2]
        elif parts[0] == "S2":
            s2[parts[1]] = dict(kv.split("=", 1) for kv in parts[2:] if "=" in kv)
        elif parts[0] == "CONSOLELABELS":
            labels[parts[1]] = parts[2]
        elif parts[0] == "SHOT":
            shots[parts[1]] = parts[2] == "true"
    names = ["x-off", "x-idle", "x-asked", "x-quiet", "x-off-again"] if mode == "send" else ["p-row-off", "p-row-on", "p-row-off-again"]
    check(all(n in begin and n in s2 for n in names), "every phase began and reported (%s)" % ",".join(n for n in names if n in s2))
    if mode == "send":
        want = {"x-off": ("0", "0"), "x-idle": ("1", "0"), "x-asked": ("1", "1"), "x-quiet": ("1", "0"), "x-off-again": ("0", "0")}
        for n, (line, asked) in want.items():
            check(s2.get(n, {}).get("line") == line and s2.get(n, {}).get("asked") == asked and s2.get(n, {}).get("role") == "-",
                  "%s: Line In %s, the QML believes asked=%s, no role (%s)" % (n, line, asked, s2.get(n)))
        check(rep(state["x-idle"]["tm"]) == rep(state["x-quiet"]["tm"]) == rep(state["x-off"]["tm"].replace("30000o,", "")) and rep(state["x-asked"]["tm"]) != rep(state["x-idle"]["tm"]),
              "a timer more runs while a player asks, none else, before and after (%s | %s | %s)" % (state["x-idle"]["tm"], state["x-asked"]["tm"], state["x-quiet"]["tm"]))
        rows = [json.loads(l) for l in read("stats.jsonl").splitlines() if l.strip()]

        def at(t):
            best = {}
            for r in rows:
                if r["t"] <= t:
                    best = r["counts"]
            return best

        KINDS = ["GET /v1/enginex/consolesend", "POST /v1/enginex/decks", "POST /v1/enginex/decks/alive", "GET /v1/enginex/shadow", "GET /tile.png"]
        print("\nrequests per minute, by kind (window: the last 10 s of each phase, x6)")
        print("%-14s" % "phase" + "".join("%-14s" % k.split("/v1/enginex/")[-1].replace("GET ", "g ").replace("POST ", "p ")[:12] for k in KINDS))
        rate = {}
        for n in names:
            if n in begin:
                a, b = at((begin[n] + SETTLE_MS) / 1000), at((begin[n] + LEN_MS) / 1000)
                rate[n] = {k: (b.get(k, 0) - a.get(k, 0)) * 6 for k in KINDS}
                print("%-14s" % n + "".join("%-14d" % rate[n][k] for k in KINDS))
        posts = lambda n: rate.get(n, {}).get("POST /v1/enginex/decks", 0) + rate.get(n, {}).get("POST /v1/enginex/decks/alive", 0)
        for n in ("x-off", "x-idle", "x-quiet", "x-off-again"):
            check(posts(n) == 0, "%s: nothing posted (%d a minute)" % (n, posts(n)))
        check(posts("x-asked") >= 60, "x-asked: the sampler runs and posts (%d a minute)" % posts("x-asked"))
        check(rate["x-off"]["GET /v1/enginex/consolesend"] == 0 and rate["x-off-again"]["GET /v1/enginex/consolesend"] == 0,
              "Line In off: not even the held request")
        check(rate["x-idle"]["GET /v1/enginex/consolesend"] <= 6, "Line In on, nobody asks: at most one held request in the window (%d a minute)" % rate["x-idle"]["GET /v1/enginex/consolesend"])
        check(all(rate[n]["GET /v1/enginex/shadow"] == 0 for n in names), "no channel on Line: the Line In poll never runs")
    else:
        check(s2.get("p-row-off", {}).get("row") == "0" and s2.get("p-row-on", {}).get("row") == "1" and s2.get("p-row-off-again", {}).get("row") == "0",
              "the row follows the setting (%s)" % {n: s2.get(n, {}).get("row") for n in names})
        check(all(s2.get(n, {}).get("role") == "12" for n in names), "the Engine stands in for a player on 1+2")
        on = labels.get("p-row-on", "")
        check(on.count("Console_deck_") == 2 and "Console_deck_1_·_PRIME_4" in on and "Console_deck_2_·_PRIME_4" in on, "row on: the free lanes 3 and 4 are drawn and labelled (%s)" % on)
        check(labels.get("p-row-off", "") == "[]" and labels.get("p-row-off-again", "") == "[]", "row off: no lane is the console's (%s | %s)" % (labels.get("p-row-off"), labels.get("p-row-off-again")))
        check(all(shots.get(n) for n in ("p-row-off", "p-row-on")), "two captures saved (%s)" % shots)

else:
    console_log, player_log, sample_log = read("console.log"), read("player.log"), read("sample.log")
    names = [p[0] for p in drv.PHASES]
    old_player, old_console = mode == "sendoldplayer", mode == "sendoldconsole"
    lanes = {}  # phase -> list of (offset, lanes)
    for line in player_log.splitlines():
        if line.startswith("LANES|"):
            _, t, name, off, js = line.split("|", 4)
            lanes.setdefault(name, []).append((float(off), json.loads(js)))
    debug = {}
    for line in player_log.splitlines():
        if line.startswith("PLAYERDEBUG|"):
            _, name, js = line.split("|", 2)
            debug[name] = json.loads(js)
    samples = {}
    for line in sample_log.splitlines():
        if line.startswith("SAMPLE|"):
            row = json.loads(line[7:])
            samples[row["phase"]] = row
    posts = {}
    for line in console_log.splitlines():
        if line.startswith("POSTS|"):
            _, name, n, last = line.split("|")
            posts[name] = (int(n), last[5:])
    asked = [l.split("|") for l in console_log.splitlines() if l.startswith("ASKED|")]
    consoles = lambda name: [(off, [x for x in ls if x[1] > 0]) for off, ls in lanes.get(name, [])]
    print("player lanes that are a console's, per phase (offset: lane/console deck/model)")
    for n in names:
        shown = [(off, ls) for off, ls in consoles(n) if ls]
        print("  %-14s %d of %d answers carried console lanes; last: %s" % (n, len(shown), len(lanes.get(n, [])), json.dumps(shown[-1][1]) if shown else "-"))
    check(all(n in lanes and lanes[n] for n in names if drv.PHASES[names.index(n)][5]), "the player was polled in every phase it polls")

    if old_console:
        check(all(not ls for n in names for _, ls in consoles(n)), "a console of 5.3.0.5-r1 beside the new player: no console lane in any answer")
        check(all(d.get("decks", 0) == 0 for d in debug.values()), "the player holds no console deck at any phase end (%s)" % {n: d.get("decks") for n, d in debug.items()})
        check(any(d.get("row") for d in debug.values()), "the row was on at some phase end, so the pulls with console=1 really went to the old console")
        check("ERR" not in player_log, "no error in the player's driver")
    else:
        check(not old_player or all(not ls for n in names for _, ls in consoles(n)), "the player of 5.3.0.5-r1 never gets a console lane")
        if old_player:
            # A second, new player asks (the driver "asker"): the console sends, and the old player, which pulls without console=1,
            # is never given a deck. With Line In off the console listens to nobody.
            cs = {n: samples.get(n, {}) for n in names}
            check(any(a[-1] == "True" for a in asked), "the new console was asked by another player (%s)" % asked)
            check(posts["row-off"][0] > 10 and posts["row-on"][0] > 10, "and sent: the old player's pulls found a console that sends (%s)" % {n: posts[n][0] for n in names})
            check(all((cs[n].get("console") or {}).get("asked") is True for n in ("row-off", "row-on", "line-1")), "the console counted its asker at the end of the phases with Line In on")
            check(posts["console-stock"][0] == 0 and (cs["console-stock"].get("console") or {}).get("asked") is False, "Line In off: nothing posted, nobody counted")
        else:
            # phase by phase, with the new player
            cs = {n: samples.get(n, {}) for n in names}
            ends = lambda n: (consoles(n)[-1][1] if consoles(n) else None)
            check(all(not ls for _, ls in consoles("row-off")), "row off: no console lane, ever")
            check(posts["row-off"][0] == 0, "row off, Line In on, nobody asks: the console posted nothing (%s)" % (posts["row-off"],))
            c = cs["row-off"].get("console", {})
            check(c.get("hears") is True and c.get("listening") is True and c.get("asked") is False and cs["row-off"].get("running") is False and cs["row-off"].get("udp50198") is True and cs["row-off"].get("udp50199") is False,
                  "row off: the console listens (UDP 50198 bound) and nothing else: no deck loop, UDP 50199 not bound (%s)" % cs["row-off"])
            e = ends("row-on")
            check(e and [x[0] for x in e] == [1, 2] and [x[1] for x in e] == [1, 2] and all(x[2] == "PRIME4" and x[3] for x in e),
                  "row on: the console's decks 1 and 2 are in the player's free lanes 1 and 2, display only, stamped with the console's model (%s)" % e)
            check(posts["row-on"][0] > 10, "row on: the console, asked, posts (%d posts)" % posts["row-on"][0])
            check(cs["row-on"].get("console", {}).get("asked") is True and cs["row-on"].get("running") is False,
                  "row on: the console is asked and still runs no deck loop (%s)" % cs["row-on"])
            check(debug["row-on"].get("decks") == 2 and debug["row-on"].get("row") is True, "row on: the player holds two console decks, kept apart from its peers' (%s)" % debug["row-on"])
            e = ends("line-1")
            check(e and [x[1] for x in e] == [2], "channel 1 on Line: its deck is gone from the player's lanes, deck 2 stays (%s)" % e)
            e = ends("row-off-again")
            check(not e, "row switched off: the console lanes are gone by the end of the phase (%s)" % e)
            check(cs["row-off-again"].get("console", {}).get("asked") is False, "row switched off: the console is no longer asked (%s)" % cs["row-off-again"].get("console"))
            check(posts["row-off-again"][1] != "-" and float(posts["row-off-again"][1]) < 10, "row switched off: the console stopped posting within 6 s of the last ask (last post at +%s s)" % posts["row-off-again"][1])
            check(cs["player-gone"].get("console", {}).get("asked") is False, "the player stopped asking: the console stopped being asked (%s)" % cs["player-gone"].get("console"))
            check(float(posts["player-gone"][1]) < 20, "the player stopped asking: the console stopped posting (last post at +%s s of 26)" % posts["player-gone"][1])
            check(all(not ls for _, ls in consoles("console-stock")), "Line In off on the console: the player, row on, shows nothing of it")
            c = cs["console-stock"].get("console", {})
            check(c.get("hears") is False and c.get("listening") is False and c.get("askers") == 0 and cs["console-stock"].get("udp50198") is False,
                  "Line In off: the console listens to nobody, notes no ask, UDP 50198 closed (%s)" % cs["console-stock"])
            check(posts["console-stock"][0] == 0, "Line In off: nothing posted")

print("\nCONSOLE_SEND_%s_%s" % (mode.upper(), "FAIL" if failures else "PASS"))
sys.exit(1 if failures else 0)
