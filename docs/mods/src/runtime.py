#!/usr/bin/env python3
"""Loopback-only QML Mod helper. No package installation or shell evaluation."""
import json
import os
from pathlib import Path
import signal
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlsplit
from urllib.request import urlopen


def save_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(value, indent=2) + "\n")
    os.replace(temp, path)


def media_file(value):
    path = Path(value).resolve(strict=True)
    if not path.is_file() or not any(path.is_relative_to(Path(root)) for root in ("/media", "/mnt", str(STATE))):
        raise ValueError("choose an existing file under /media, /mnt or this Mod's state directory")
    return path


MOD = sys.argv[1] if len(sys.argv) > 1 else ""
PORTS = {"dmx-studio": 17834, "jog-logo-studio": 17835,
         "sampler-pad-studio": 17836, "minimal-club-mode": 17837,
         "midi-controller-mapper": 17838}
if MOD not in PORTS:
    raise SystemExit("unknown Mod")
STATE = Path(os.environ.get("ENGINEX_MOD_STATE", "/data/enginex/mods-state")).resolve() / MOD
PORT = PORTS[MOD]

# Feature imports runtime helpers through __main__ without running a second server.
from feature import Feature  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    disable_nagle_algorithm = True
    wbufsize = -1

    def log_message(self, *args):
        pass  # Paths and device names may contain private information.

    def reply(self, code, value):
        blob = json.dumps(value).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(blob)))
        self.end_headers()
        self.wfile.write(blob)

    def allowed(self):
        # Reject browser cross-site writes and DNS rebinding; QML has no Origin.
        if self.headers.get("Host") not in (f"127.0.0.1:{PORT}", f"localhost:{PORT}"):
            return False
        origin = self.headers.get("Origin", "")
        return not origin or origin == "null" or origin in (f"http://127.0.0.1:{PORT}", f"http://localhost:{PORT}")

    def do_GET(self):
        if not self.allowed():
            return self.reply(403, {"error": "local QML only"})
        if urlsplit(self.path).path != "/status":
            return self.reply(404, {"error": "unknown endpoint"})
        try:
            self.reply(200, feature.status())
        except Exception as exc:
            self.reply(500, {"error": str(exc)})

    def do_POST(self):
        if not self.allowed():
            return self.reply(403, {"error": "local QML only"})
        if self.path != "/action":
            return self.reply(404, {"error": "unknown endpoint"})
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= 65536 or self.headers.get_content_type() != "application/json":
                raise ValueError("expected a JSON body of 1..65536 bytes")
            self.connection.settimeout(3)
            request = json.loads(self.rfile.read(size))
            if not isinstance(request, dict):
                raise ValueError("expected an object")
            self.reply(200, feature.action(request))
        except (ValueError, KeyError, TypeError, FileNotFoundError) as exc:
            self.reply(400, {"error": str(exc)})
        except Exception as exc:
            self.reply(500, {"error": str(exc)})


class LocalHTTPServer(HTTPServer):
    def get_request(self):
        connection, address = super().get_request()
        connection.settimeout(3)
        return connection, address


if __name__ == "__main__":
    command = sys.argv[2] if len(sys.argv) > 2 else "serve"
    if command in ("health", "health-once"):
        for attempt in range(10 if command == "health" else 1):
            try:
                with urlopen(f"http://127.0.0.1:{PORT}/status", timeout=0.2) as response:
                    json.load(response)
                raise SystemExit(0)
            except (OSError, ValueError):
                time.sleep(0.1)
        raise SystemExit("Mod helper did not become ready")
    if command == "stop":
        pid_path = STATE / "helper.pid"
        if not pid_path.exists():
            raise SystemExit(0)
        pid = int(pid_path.read_text().strip())
        if pid <= 1:
            raise SystemExit("invalid helper PID")
        proc = Path(f"/proc/{pid}")
        if proc.exists():
            argv = (proc / "cmdline").read_bytes().split(b"\x00")
            if not any(a.endswith(b"runtime.py") for a in argv) or MOD.encode() not in argv or b"serve" not in argv:
                raise SystemExit("PID does not belong to this Mod; refusing to signal it")
            os.kill(pid, signal.SIGTERM)
            for _ in range(25):
                if not proc.exists() or (proc / "stat").read_text().split(") ", 1)[1].startswith("Z"):
                    break
                time.sleep(0.1)
            else:
                raise SystemExit("helper has not stopped; retry after pending I/O completes")
        pid_path.unlink(missing_ok=True)
        raise SystemExit(0)
    if command == "restore" and MOD == "minimal-club-mode":
        Feature(STATE).action({"action": "disable"})
        raise SystemExit(0)
    if command != "serve":
        raise SystemExit("usage: start.sh [serve]")
    server = LocalHTTPServer(("127.0.0.1", PORT), Handler)
    feature = Feature(STATE)
    server.timeout = 0.1
    server.socket.settimeout(3)
    stopping = False

    def stop(*_):
        global stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        while not stopping:
            server.handle_request()
    finally:
        feature.close()
        server.server_close()
