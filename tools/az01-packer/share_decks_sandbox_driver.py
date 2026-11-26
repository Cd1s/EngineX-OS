#!/usr/bin/env python3
"""Helpers of share-decks-sandbox.sh (#329 Share Decks), one line per fact on stdout.

  console <markport>   in the console's network namespace: the console's QML as far as stream-sync can tell. It waits for
                       the player's probe (GET /ready on <markport>), then runs PHASES: while a phase has a channel on Line it
                       polls the console's stream-sync (GET /v1/enginex/shadow?line=3, as the QML does) and logs the lanes.
  player               in the player's network namespace: the player's stream-sync from outside (its debug state every
                       second: what the QML posted, the listener, the asks) and the Engine's CPU time (all threads, and the
                       GUI thread, which runs the QML sampler)."""
import http.server
import json
import os
import socketserver
import sys
import threading
import time
import urllib.request

BASE = "http://127.0.0.1:8080"
# name, start and end in seconds after the probe's READY, whether a console channel is on Line (the console asks). The
# console's deck loop, and so its asks, runs on for 10 s after its QML's last poll, and an ask lasts 6 s: the silence is
# long enough for both. The probe switches the row off at 70 s.
PHASES = [
    ("no-ask", 0, 15, False),
    ("ask", 15, 45, True),
    ("silence", 45, 72, False),
    ("row-off", 72, 88, True),
]
END = PHASES[-1][2]


def get(path, timeout=4):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as resp:
        return json.loads(resp.read() or b"{}")


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


def console(markport):
    marks = {}

    class Mark(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            name = self.path.strip("/")
            marks.setdefault(name, time.time())
            print("MARK|%s|%d" % (name, int(time.time() * 1000)), flush=True)
            self.send_response(204)
            self.end_headers()

        def log_message(self, *a):
            pass

    srv = socketserver.ThreadingTCPServer(("0.0.0.0", markport), Mark)
    srv.daemon_threads = True
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    deadline = time.time() + 400
    while "ready" not in marks and time.time() < deadline:
        time.sleep(0.2)
    if "ready" not in marks:
        print("CONSOLE|no READY from the probe", flush=True)
        return
    r = marks["ready"]
    sampled = set()
    while time.time() - r < END + 1:
        t = time.time() - r
        phase = next((p for p in PHASES if p[1] <= t < p[2]), None)
        if phase and phase[3]:
            try:
                d = get("/v1/enginex/shadow?line=3")
                lanes = [{k: x.get(k) for k in ("deck", "title", "lineIn", "player", "model", "playing", "stale", "displayOnly")}
                         for x in d.get("decks") or []]
                print("LANES|%d|%s|%.1f|%s" % (int(time.time() * 1000), phase[0], t, json.dumps(lanes)), flush=True)
            except Exception as e:
                print("LANEERR|%s" % e, flush=True)
        if phase and t > phase[2] - 1.5 and phase[0] not in sampled:
            sampled.add(phase[0])
            try:
                d = get("/v1/enginex/debug/shadow")
                print("CONSOLEDEBUG|%s|%s" % (phase[0], json.dumps({"running": d.get("running"), "peers": sorted((d.get("peers") or {}).keys()),
                                                                    "console": d.get("console"), "udp50199": udp_bound(50199)})), flush=True)
            except Exception as e:
                print("CONSOLEDEBUGERR|%s" % e, flush=True)
        time.sleep(0.5)
    print("CONSOLE|done", flush=True)


def engine_pid():
    """The Engine process by its command line (its threads rename themselves, so comm is no guide)."""
    for p in os.listdir("/proc"):
        if p.isdigit():
            try:
                argv0 = open("/proc/%s/cmdline" % p, "rb").read().split(b"\0")[0]
            except OSError:
                continue
            if argv0 == b"/usr/Engine/Engine":
                return int(p)
    return 0


def cpu(pid):
    total, main, threads = 0, 0, 0
    try:
        for tid in os.listdir("/proc/%d/task" % pid):
            try:
                f = open("/proc/%d/task/%s/stat" % (pid, tid)).read()
            except OSError:
                continue
            fields = f[f.rindex(")") + 2:].split()
            ticks = int(fields[11]) + int(fields[12])  # utime + stime
            total += ticks
            threads += 1
            if int(tid) == pid:
                main = ticks
    except OSError:
        pass
    return total, main, threads


def player():
    hz = os.sysconf("SC_CLK_TCK")
    end = time.time() + 600
    while time.time() < end:
        row = {"t": int(time.time() * 1000)}
        try:
            d = get("/v1/enginex/debug/shadow", timeout=2)
            own = d.get("own") or []
            row.update(own=[{k: x.get(k) for k in ("deck", "title", "active", "player", "playing")} for x in own],
                       ownAgeMs=d.get("ownAgeMs"), running=d.get("running"), console=d.get("console"))
        except Exception as e:
            row["error"] = str(e)
        row["udp50198"] = udp_bound(50198)
        pid = engine_pid()
        if pid:
            total, main, threads = cpu(pid)
            row.update(cpu=total / hz, gui=main / hz, threads=threads)
        print("SAMPLE|" + json.dumps(row), flush=True)
        time.sleep(1.0)


if __name__ == "__main__":
    if sys.argv[1] == "console":
        console(int(sys.argv[2]))
    elif sys.argv[1] == "player":
        player()
