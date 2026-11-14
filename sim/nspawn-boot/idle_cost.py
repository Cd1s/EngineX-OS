#!/usr/bin/env python3
"""What a booted nspawn guest spends while idle, per systemd unit and per thread.

  idle_cost.py snap <init_pid>          one JSON snapshot of the guest on stdout
  idle_cost.py diff <before> <after>    what was spent between two snapshots

Unit CPU comes from cgroup cpu.stat, so children that already exited (a shell
loop's cat and sleep) are counted. A voluntary context switch is one sleep and
wake-up of a thread. The build host's cores are faster than a deck's: compare
runs with each other, and read wake-ups, not CPU time, as deck figures.
"""
import json
import os
import sys
import time

CG = "/sys/fs/cgroup"


def read(path):
    try:
        with open(path, "rb") as f:
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""


def fields(text):
    """'key value' or 'key: value' lines as a dict of ints."""
    out = {}
    for line in text.splitlines():
        key, _, value = line.partition(" " if ":" not in line else ":")
        value = value.split()
        if value and value[0].isdigit():
            out[key.strip()] = int(value[0])
    return out


def snap(init_pid):
    ns = os.readlink(f"/proc/{init_pid}/ns/pid")
    root = os.path.dirname(read(f"/proc/{init_pid}/cgroup").strip().split("::", 1)[1])
    tasks, cgroups = {}, {}
    for pid in filter(str.isdigit, os.listdir("/proc")):
        try:
            if os.readlink(f"/proc/{pid}/ns/pid") != ns:
                continue
            tids = os.listdir(f"/proc/{pid}/task")
        except OSError:
            continue
        unit = read(f"/proc/{pid}/cgroup").strip().split("::", 1)[-1]
        unit = unit[len(root):].strip("/") or "."
        cmd = " ".join(read(f"/proc/{pid}/cmdline").split("\0")[:3]).strip()[:70]
        for tid in tids:
            base = f"/proc/{pid}/task/{tid}"
            stat = read(f"{base}/stat")
            if not stat:
                continue
            comm = stat[stat.index("(") + 1:stat.rindex(")")]
            rest = stat[stat.rindex(")") + 2:].split()  # rest[0] is field 3 (state)
            status, io = fields(read(f"{base}/status")), fields(read(f"{base}/io"))
            tasks[f"{pid}/{tid}"] = {
                "unit": unit, "cmd": cmd, "comm": comm,
                "cpu": int(rest[11]) + int(rest[12]),
                "policy": int(rest[38]),
                "vol": status.get("voluntary_ctxt_switches", 0),
                "invol": status.get("nonvoluntary_ctxt_switches", 0),
                "syscr": io.get("syscr", 0), "syscw": io.get("syscw", 0),
                "write_bytes": io.get("write_bytes", 0),
            }
    for path, _, files in os.walk(CG + root):
        if "cpu.stat" in files:
            usage = fields(read(f"{path}/cpu.stat")).get("usage_usec", 0)
            cgroups[path[len(CG + root):].strip("/") or "."] = usage
    return {"time": time.monotonic(), "hz": os.sysconf("SC_CLK_TCK"), "tasks": tasks, "cgroups": cgroups}


def diff(before, after):
    seconds = after["time"] - before["time"]
    print(f"window {seconds:.1f} s; rates are per second")
    print("\n== CPU per unit (cgroup, exited children included), busiest first")
    print(f"{'cpu ms':>9} {'% of a core':>11}  unit")
    units = []
    for name, usage in after["cgroups"].items():
        # A cgroup with child cgroups repeats their total: keep the leaves.
        if any(other.startswith(name + "/") for other in after["cgroups"]) or name == ".":
            continue
        units.append(((usage - before["cgroups"].get(name, 0)) / 1000, name))
    for ms, name in sorted(units, reverse=True):
        if ms >= 1:
            print(f"{ms:9.0f} {ms / seconds / 10:11.3f}  {name}")
    print(f"{sum(ms for ms, _ in units):9.0f} {sum(ms for ms, _ in units) / seconds / 10:11.3f}  TOTAL")
    print("\n== per thread, most wake-ups first (threads that started in the window count from 0)")
    print(f"{'cpu ms':>8} {'wake/s':>8} {'preempt/s':>9} {'read/s':>7} {'write/s':>7} {'wr kB':>7}  unit | process | thread")
    rows, total = [], 0.0
    tick = 1000 / after["hz"]
    for key, now in after["tasks"].items():
        old = before["tasks"].get(key, {})
        d = {k: now[k] - old.get(k, 0) for k in ("cpu", "vol", "invol", "syscr", "syscw", "write_bytes")}
        total += d["vol"] / seconds
        if d["cpu"] or d["vol"] / seconds >= 0.05:
            rows.append((d["vol"], d, now))
    for _, d, now in sorted(rows, key=lambda r: r[0], reverse=True):
        rt = " [RT]" if now["policy"] in (1, 2) else ""
        print(f"{d['cpu'] * tick:8.0f} {d['vol'] / seconds:8.2f} {d['invol'] / seconds:9.2f} "
              f"{d['syscr'] / seconds:7.1f} {d['syscw'] / seconds:7.1f} {d['write_bytes'] / 1024:7.0f}  "
              f"{now['unit']} | {now['cmd']} | {now['comm']}{rt}")
    print(f"TOTAL wake-ups/s of live threads: {total:.1f}")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "snap":
        json.dump(snap(int(sys.argv[2])), sys.stdout)
    elif len(sys.argv) == 4 and sys.argv[1] == "diff":
        diff(*(json.load(open(p)) for p in sys.argv[2:]))
    else:
        sys.exit(__doc__)
