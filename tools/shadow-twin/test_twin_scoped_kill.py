#!/usr/bin/env python3
"""Regression test: ensure shadow-twin service restart and down on twin A
never kill twin B's Engine and stream-sync processes.

Must fail on the old code (where host-wide `pkill -KILL -f 'ns-holder.sh|shadow-twin-sync'`
in down kills all twins' namespace holders on the host, destroying their PID namespaces
and killing both instances' Engines simultaneously).
"""

import os
import shutil
import subprocess
import sys
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
TWIN_PY = HERE / "twin.py"


def is_pid_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


class TestTwinScopedKill(unittest.TestCase):
    work_a: Path
    work_b: Path

    @classmethod
    def setUpClass(cls):
        if sys.platform != "linux" or os.geteuid() != 0:
            raise unittest.SkipTest("Requires Linux root permissions for namespaces and mounts")

        tag = f"regress-{os.getpid()}-{int(time.time())}"
        cls.work_a = Path(f"/var/tmp/st-a-{tag}")
        cls.work_b = Path(f"/var/tmp/st-b-{tag}")

    @classmethod
    def tearDownClass(cls):
        for w in (getattr(cls, "work_a", None), getattr(cls, "work_b", None)):
            if w and w.exists():
                subprocess.run(
                    [sys.executable, str(TWIN_PY), "down", "--purge"],
                    env={**os.environ, "SHADOW_TWIN_WORK": str(w)},
                    check=False,
                    capture_output=True,
                )
                shutil.rmtree(w, ignore_errors=True)

    def _twin_cmd(self, work: Path, *args, check=True):
        env = {**os.environ, "SHADOW_TWIN_WORK": str(work)}
        cmd = [sys.executable, str(TWIN_PY), *args]
        p = subprocess.run(cmd, env=env, capture_output=True, text=True)
        if check and p.returncode != 0:
            raise RuntimeError(f"Command failed ({p.returncode}): {' '.join(cmd)}\nSTDOUT:\n{p.stdout}\nSTDERR:\n{p.stderr}")
        return p

    def _start_twin(self, work: Path):
        # Seed artifacts from common cache or other work dir if available to speed up startup
        for cand_dir in (Path("/var/tmp/shadow-sync-snap-20261111"), Path("/var/tmp/shadow-twin")):
            if (cand_dir / "base/image/rootfs.ext2").is_file() and not (work / "base/image/rootfs.ext2").is_file():
                (work / "base/image").mkdir(parents=True, exist_ok=True)
                try:
                    os.link(cand_dir / "base/image/rootfs.ext2", work / "base/image/rootfs.ext2")
                except OSError:
                    pass
            if (cand_dir / "bin/sc5000-suite").is_file() and not (work / "bin/sc5000-suite").is_file():
                (work / "bin").mkdir(parents=True, exist_ok=True)
                shutil.copy2(cand_dir / "bin/sc5000-suite", work / "bin/sc5000-suite")
            if (cand_dir / "bin/alsa-null-fixture.so").is_file() and not (work / "bin/alsa-null-fixture.so").is_file():
                (work / "bin").mkdir(parents=True, exist_ok=True)
                shutil.copy2(cand_dir / "bin/alsa-null-fixture.so", work / "bin/alsa-null-fixture.so")
            if (cand_dir / "packs/en").is_dir() and not (work / "packs/en").is_dir():
                shutil.copytree(cand_dir / "packs", work / "packs", dirs_exist_ok=True)
            if (cand_dir / "music/track-a.wav").is_file() and not (work / "music/track-a.wav").is_file():
                (work / "music").mkdir(parents=True, exist_ok=True)
                shutil.copy2(cand_dir / "music/track-a.wav", work / "music/track-a.wav")
                shutil.copy2(cand_dir / "music/track-b.wav", work / "music/track-b.wav")

        res = self._twin_cmd(work, "up", "--local", "--minimal", check=False)
        if res.returncode != 0 and "--minimal" in res.stderr:
            # Fallback for old code that didn't support --minimal
            res = self._twin_cmd(work, "up", "--local", check=True)
        elif res.returncode != 0:
            self.fail(f"Failed to start twin on {work}:\n{res.stdout}\n{res.stderr}")

    def _assert_twin_b_alive(self, label: str):
        for side in ("A", "B"):
            sdir = self.work_b / side
            init_file = sdir / "init.pid"
            self.assertTrue(init_file.is_file(), f"[{label}] Twin B/{side}: init.pid file does not exist ({init_file})")
            init_pid = int(init_file.read_text().strip())
            self.assertTrue(
                is_pid_alive(init_pid),
                f"[{label}] Twin B/{side}: namespace holder init.pid {init_pid} is DEAD!",
            )

            # Check inside namespace: holder script, stream-sync, and Engine
            rdir = sdir / "root"
            pg = subprocess.run(
                ["nsenter", "-t", str(init_pid), "-m", "-n", "-p", "--", "chroot", str(rdir), "ps", "aux"],
                capture_output=True,
                text=True,
            )
            self.assertEqual(pg.returncode, 0, f"[{label}] Twin B/{side}: failed to enter namespace of init {init_pid}: {pg.stderr}")
            proc_table = pg.stdout

            self.assertIn(
                "/usr/Engine/Engine",
                proc_table,
                f"[{label}] Twin B/{side}: Engine is NOT running in namespace! ps aux:\n{proc_table}",
            )
            self.assertTrue(
                any(x in proc_table for x in ("stream-sync", "shadow-twin-sync", "sc5000-suite")),
                f"[{label}] Twin B/{side}: stream-sync is NOT running in namespace! ps aux:\n{proc_table}",
            )

    def test_twin_a_actions_do_not_kill_twin_b(self):
        print("\n===> Step 1: Start Twin B in isolated directory...", flush=True)
        self._start_twin(self.work_b)
        self._assert_twin_b_alive("Initial Twin B start")
        print("===> Twin B is UP and verified alive.", flush=True)

        print("\n===> Step 2: Start Twin A in separate directory...", flush=True)
        self._start_twin(self.work_a)
        self._assert_twin_b_alive("After Twin A up")
        print("===> Twin A is UP, Twin B remains alive.", flush=True)

        print("\n===> Step 3 (a): Restart services on Twin A...", flush=True)
        res_dep = self._twin_cmd(self.work_a, "deploy", "--local", "--minimal", check=False)
        if res_dep.returncode != 0 and "--minimal" in res_dep.stderr:
            # Fallback for old code
            self._twin_cmd(self.work_a, "deploy", "--local", check=True)
        elif res_dep.returncode != 0:
            self.fail(f"Twin A deploy/service restart failed:\n{res_dep.stdout}\n{res_dep.stderr}")

        # Assert Twin B's Engines and sync process are still alive after (a)
        self._assert_twin_b_alive("After (a) service restart on Twin A")
        print("===> (a) Twin A service restart completed. Twin B Engines & sync still alive!", flush=True)

        print("\n===> Step 4 (b): Run down on Twin A...", flush=True)
        self._twin_cmd(self.work_a, "down", check=True)

        # Assert Twin B's Engines and sync process are still alive after (b)
        self._assert_twin_b_alive("After (b) down on Twin A")
        print("===> (b) Twin A down completed. Twin B Engines & sync still alive!", flush=True)


if __name__ == "__main__":
    unittest.main()
