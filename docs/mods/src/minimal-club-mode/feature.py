"""Reversible pause of a fixed optional-service allowlist. No killall/masking."""
import json
from pathlib import Path
import subprocess
from __main__ import save_json

# Never include engine, rb-bridge, network, audio, Pro DJ Link, or the WebUI host.
# stream-sync also hosts control/bridge functionality, so pausing it is unsafe.
SERVICES = ("sc5000-deemixd.service", "stems-processor.service", "icecast-stream.service",
            "enginex-download-queue.service", "enginex-streaming-sync.service")


def systemctl(*args):
    return subprocess.run(["systemctl", *args], capture_output=True, text=True, timeout=20)


class Feature:
    def __init__(self, state):
        self.state = state
        self.path = state / "paused.json"

    def status(self):
        paused = json.loads(self.path.read_text()) if self.path.exists() else {"active": False, "services": []}
        return dict(paused, allowlist=SERVICES,
                    note="Optional units only. The combined stream-sync process remains active because it also hosts core controls. No CPU-percentage guarantee.")

    def action(self, request):
        action = request["action"]
        if action == "enable":
            if self.path.exists():
                return self.status()
            active = []
            for unit in SERVICES:
                result = systemctl("is-active", unit)
                status = result.stdout.strip()
                if result.returncode == 0 and status == "active":
                    active.append(unit)
                elif status not in ("inactive", "failed", "unknown"):
                    raise RuntimeError(f"cannot establish service state for {unit}: {status or result.stderr.strip()}")
            # Persist the exact pre-state before the first service is stopped.
            snapshot = {"active": True, "services": active, "phase": "pausing"}
            save_json(self.path, snapshot)
            stopped = []
            for unit in active:
                result = systemctl("stop", unit)
                if result.returncode or systemctl("is-active", unit).returncode == 0:
                    failed = []
                    # The failed stop may still have stopped the unit; restore it too.
                    for restore in stopped + [unit]:
                        if systemctl("start", restore).returncode:
                            failed.append(restore)
                    if failed:
                        save_json(self.path, dict(snapshot, phase="restore-needed", services=failed))
                    else:
                        self.path.unlink()
                    raise RuntimeError(f"could not stop {unit}; rollback attempted; restore pending: {failed}")
                stopped.append(unit)
            save_json(self.path, dict(snapshot, phase="paused"))
        elif action == "disable":
            if not self.path.exists():
                return self.status()
            snapshot = json.loads(self.path.read_text())
            pending = snapshot["services"]
            if not isinstance(pending, list) or any(unit not in SERVICES for unit in pending):
                raise ValueError("invalid saved service list")
            failed = []
            for unit in pending:
                if systemctl("start", unit).returncode or systemctl("is-active", unit).returncode != 0:
                    failed.append(unit)
            if failed:
                save_json(self.path, dict(snapshot, phase="restore-needed", services=failed))
                raise RuntimeError(f"restore still pending: {failed}")
            self.path.unlink()
        else:
            raise ValueError("unknown action")
        return self.status()

    def close(self):
        # Persist explicit intent; restarting the helper must not resume downloads.
        pass
