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
import urllib.parse
import urllib.request

BASE = "http://127.0.0.1:8080"
# name, start and end in seconds after the probe's READY, whether a console channel is on Line (the console polls for
# it, so it asks). The probe moves the stock Layer at 25 and 33 s and switches the row off at 70 s, while the console asks.
PHASES = [
    ("no-ask", 0, 15, False),
    ("ask", 15, 45, True),
    ("silence", 45, 66, False),  # the last channel leaves Line, the row stays on
    ("ask-row-off", 66, 90, True),
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
    fetched, pngs = {}, {}
    while time.time() - r < END + 1:
        t = time.time() - r
        phase = next((p for p in PHASES if p[1] <= t < p[2]), None)
        if phase and phase[3]:
            try:
                d = get("/v1/enginex/shadow?line=3")
                lanes = [{k: x.get(k) for k in ("deck", "title", "lineIn", "player", "model", "playing", "stale", "displayOnly", "key", "loading")}
                         for x in d.get("decks") or []]
                # The lane's picture, as the console's QML fetches it: the display data of the shared track (every 2 s),
                # and its overview and first detail tile as PNGs (once each per key, when there).
                for x in lanes:
                    k = x.get("key")
                    if not k or time.time() - fetched.get(k, 0) < 2:
                        continue
                    fetched[k] = time.time()
                    q = "deck=%d&key=%s" % (x["deck"], urllib.parse.quote(k))
                    try:
                        disp = get("/v1/enginex/shadow/display?" + q)
                        x["display"] = {"durationMs": disp.get("durationMs"), "bpm": disp.get("bpm"), "beats": len(disp.get("beats") or []),
                                        "tiles": len(disp.get("waveformTiles") or []), "detail": disp.get("detail")}
                        for name, path in (("overview", "/v1/enginex/shadow/waveform.png?%s&overview=1" % q),
                                           ("tile", "/v1/enginex/shadow/waveform.png?%s&tile=0" % q)):
                            if (k, name) in pngs or (name == "tile" and not x["display"]["tiles"]):
                                continue
                            with urllib.request.urlopen(BASE + path, timeout=4) as resp:
                                body = resp.read()
                            if body[:8] == b"\x89PNG\r\n\x1a\n" and len(body) > 200:
                                pngs[(k, name)] = len(body)
                                print("PNG|%s|%s|%d" % (name, k, len(body)), flush=True)
                    except Exception as e:
                        x["display"] = {"error": str(e)[:80]}
                print("LANES|%d|%s|%.1f|%s" % (int(time.time() * 1000), phase[0], t, json.dumps(lanes)), flush=True)
            except Exception as e:
                print("LANEERR|%s" % e, flush=True)
        if phase and t > phase[2] - 1.5 and phase[0] not in sampled:
            sampled.add(phase[0])
            try:
                d = get("/v1/enginex/debug/shadow")
                print("CONSOLEDEBUG|%s|%s" % (phase[0], json.dumps({"running": d.get("running"), "peers": sorted((d.get("peers") or {}).keys()),
                                                                    "peerDecks": len(d.get("peerDecks") or []),
                                                                    "console": d.get("console"), "udp50199": udp_bound(50199)})), flush=True)
            except Exception as e:
                print("CONSOLEDEBUGERR|%s" % e, flush=True)
        time.sleep(0.5)
    print("CONSOLE|done", flush=True)


def sync_pids():
    """The two stream-syncs by command line, the player's (this network namespace) and the console's (the other one)."""
    own = os.readlink("/proc/self/ns/net")
    player = console = 0
    for p in os.listdir("/proc"):
        if p.isdigit():
            try:
                argv0 = open("/proc/%s/cmdline" % p, "rb").read().split(b"\0")[0]
                if argv0 != b"/tmp/stream-sync":
                    continue
                if os.readlink("/proc/%s/ns/net" % p) == own:
                    player = int(p)
                else:
                    console = int(p)
            except OSError:
                pass
    return player, console


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


def ticks(path):
    """utime + stime of a /proc stat file, in clock ticks."""
    f = open(path).read()
    fields = f[f.rindex(")") + 2:].split()
    return int(fields[11]) + int(fields[12])


def cpu(pid):
    """The process's CPU time from /proc/<pid>/stat (the whole thread group, threads that exited included: a sum over
    the live tasks would drop those and could go down), its main thread's (the GUI thread), and its thread count."""
    total = main = threads = 0
    try:
        total = ticks("/proc/%d/stat" % pid)
        main = ticks("/proc/%d/task/%d/stat" % (pid, pid))
        threads = len(os.listdir("/proc/%d/task" % pid))
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
        sp, cp = sync_pids()
        if sp:
            row["sync"] = cpu(sp)[0] / hz
        if cp:
            row["csync"] = cpu(cp)[0] / hz
        print("SAMPLE|" + json.dumps(row), flush=True)
        time.sleep(1.0)


if __name__ == "__main__":
    if sys.argv[1] == "console":
        console(int(sys.argv[2]))
    elif sys.argv[1] == "player":
        player()
