#!/usr/bin/env python3
"""Summarise layer-switch-sandbox.sh runs: per toggle, the GUI thread's CPU time inside the switch
(cpuset) and after it (gui_cpu), and the frames around it. Usage: layer_switch_summary.py WORKDIR"""
import re
import statistics
import sys
from pathlib import Path


def toggles(work):
    for log in sorted(work.glob("engine-*.log")):
        run = log.stem[len("engine-"):]
        text = log.read_text(errors="replace")
        swaps = [int(x) / 1000 for x in (work / f"swaps-{run}.log").read_text().split()]
        gui = {int(m[1]): int(m[2]) for m in re.finditer(r"LSW GUI\|(\d+)\|extra=(-?\d+)", text)}
        for m in re.finditer(r"LSW T0\|(\d+)\|(\w+)\|(\d+)\|set=(\d+)\|from=(\d+)\|to=(\d+)\|cpuset=(\d+)", text):
            i, t0 = int(m[1]), int(m[3])
            before = [s for s in swaps if t0 - 900 <= s < t0]
            base = statistics.median(b - a for a, b in zip(before, before[1:]))
            limit = max(2 * base, base + 10)
            last = max(s for s in swaps if s < t0)
            window = [last] + [s for s in swaps if t0 <= s <= t0 + 1900]
            settle = window[1] - t0
            excess = 0.0
            for a, b in zip(window, window[1:]):
                excess += max(0.0, b - a - base)
                if b - a > limit:
                    settle = b - t0
            yield {"run": run, "arm": run.split("-")[0], "i": i, "side": m[2], "moved": m[5] != m[6], "to": int(m[6]), "set": int(m[4]), "cpuset": int(m[7]),
                   "gui": gui.get(i, 0), "first": window[1] - t0, "settle": settle, "excess": excess, "base": base}


def main():
    rows = list(toggles(Path(sys.argv[1])))
    for r in rows:
        print("{run:10} {i:3} {side:5} moved={moved!s:5} set={set:4} cpuset={cpuset:4} gui_cpu={gui:5} first={first:6.1f} "
              "settle={settle:7.1f} excess={excess:7.1f} frame={base:5.1f}".format(**r))
    # Directions differ by tens of ms and a V4 GC lands on some toggles: compare medians per direction.
    for arm in sorted({r["arm"] for r in rows}):
        sel = [r for r in rows if r["arm"] == arm]
        if not all(r["moved"] for r in sel):
            print(f"{arm}: a toggle did not change the layer")
        ways = sorted({(r["side"], r["to"]) for r in sel})
        print(f"{arm:6} cpuset median " + " | ".join(
            f"{side}->{'AB'[to]} {statistics.median(r['cpuset'] for r in sel if (r['side'], r['to']) == (side, to)):.0f}"
            for side, to in ways) + f" | first frame {statistics.median(r['first'] for r in sel):.0f} ms, frame {statistics.median(r['base'] for r in sel):.0f} ms")


if __name__ == "__main__":
    main()
