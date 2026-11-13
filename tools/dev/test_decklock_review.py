"""Regressions from the Sonnet review of 1b20ce0; run on the ARM build host."""
import io
import os
import shlex
import signal
import subprocess
import sys
import time
import unittest
from pathlib import Path
from unittest import mock

import test_deck_push as base

dp = base.dp
HERE = base.HERE
LOCK = "/run/enginex-dev-lock"


class ReleaseAndWait(unittest.TestCase):
    def test_wait_uses_status_without_claiming_left_and_has_a_deadline(self):
        locks = mock.Mock()
        locks.read.side_effect = lambda name: {"owner": "bob", "live": "yes"} if name == "right" else None
        elapsed = [0.0]
        delays = []
        def sleep(delay):
            delays.append(delay)
            elapsed[0] += delay
        with mock.patch.object(dp.time, "monotonic", side_effect=lambda: elapsed[0]), \
                mock.patch.object(dp.time, "sleep", side_effect=sleep):
            with self.assertRaisesRegex(dp.Refused, "timed out"):
                dp.acquire_many(locks, ["right", "left"], "alice", "pair", 600,
                                wait=True, wait_timeout=3)
        locks.acquire.assert_not_called()
        locks.release.assert_not_called()
        self.assertEqual(delays, [1, 2])
        self.assertLessEqual(locks.read.call_count, 8)

    def test_release_attempts_every_deck_and_reports_every_error(self):
        locks = mock.Mock()
        locks.release.side_effect = [dp.Refused("busy right"), dp.Failed("offline left")]
        with self.assertRaises(dp.Failed) as failure:
            dp.release_many(locks, [("left", False), ("right", False)], "alice")
        self.assertEqual(locks.release.call_args_list, [mock.call("right", "alice"), mock.call("left", "alice")])
        self.assertIn("busy right", str(failure.exception))
        self.assertIn("offline left", str(failure.exception))

    def test_acquisition_cleanup_attempts_all_partial_locks(self):
        locks = mock.Mock()
        locks.holder.return_value = None
        locks.acquire.side_effect = [None, None, dp.Refused("third deck busy")]
        locks.release.side_effect = [dp.Failed("right offline"), dp.Failed("left offline")]
        with self.assertRaises(dp.Failed) as failure:
            dp.acquire_many(locks, ["extra", "right", "left"], "alice", "pair", 600)
        self.assertEqual(locks.release.call_args_list, [mock.call("right", "alice"), mock.call("left", "alice")])
        for text in ("third deck busy", "right offline", "left offline"):
            self.assertIn(text, str(failure.exception))

    def test_borrow_keeps_existing_purpose_and_ttl(self):
        locks = mock.Mock()
        locks.holder.return_value = {"owner": "alice", "live": "yes", "purpose": "outer", "ttl": 7200}
        self.assertEqual(dp.acquire_many(locks, ["left"], "alice", "inner", 2), [("left", True)])
        locks.acquire.assert_not_called()


@unittest.skipUnless(base._can_fake(), "needs root, unshare and sshd")
class ReviewLocks(unittest.TestCase):
    setUpClass = base.DeckLockTests.__dict__["setUpClass"]
    tearDownClass = base.DeckLockTests.__dict__["tearDownClass"]
    env = base.DeckLockTests.env
    start = base.DeckLockTests.start
    call = base.DeckLockTests.call
    setUp = base.DeckLockTests.setUp

    def admin(self, *argv):
        deck = dp.Config(self.ini, env={}).deck("left")
        return subprocess.run(["ssh", *deck.ssh_options, deck.ssh, shlex.join(argv)],
                              capture_output=True, text=True, check=True).stdout.strip()

    def holder(self):
        self.admin("sh", "-c", "rm -f /run/review-stop")
        self.call("acquire", "left", "--purpose", "review signals")
        deck = dp.Config(self.ini, env={}).deck("left")
        script = "printf '%s\\n' $$ > /run/review-holder-pid\n" + dp.AGENT.read_text()
        command = ["busybox", "sh", "-c", script, "deck-agent", "--locked", "alice", "exec",
                   "sh", "-c", "while [ ! -e /run/review-stop ]; do sleep 0.05; done"]
        proc = subprocess.Popen(["ssh", *deck.ssh_options, deck.ssh, shlex.join(command)],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            record = dp.LockStore(dp.Config(self.ini, env={})).read("left")
            if record and record.get("busy") == "yes":
                return proc, int(self.admin("cat", "/run/review-holder-pid"))
            time.sleep(0.05)
        proc.terminate()
        proc.communicate(timeout=10)
        self.fail("holder did not take mutex")

    def stop_holder(self, proc):
        self.admin("touch", "/run/review-stop")
        proc.communicate(timeout=10)

    def clear_fixture_mutex(self):
        # Only the isolated test namespace is addressed here, after its holder exited.
        self.admin("python3", "-c", f"import shutil; shutil.rmtree('{LOCK}/.mutex', ignore_errors=True)")

    def test_hup_and_pipe_release_the_busybox_mutex(self):
        for signum in (signal.SIGHUP, signal.SIGPIPE):
            with self.subTest(signal=signum):
                proc, pid = self.holder()
                try:
                    self.admin("kill", f"-{int(signum)}", str(pid))
                    self.stop_holder(proc)
                    self.assertEqual(self.admin("sh", "-c", f"test ! -d {LOCK}/.mutex && echo clear || echo busy"), "clear")
                    self.call("left", "--", "true", tool="deck")
                finally:
                    if proc.poll() is None:
                        self.stop_holder(proc)
                    self.clear_fixture_mutex()

    def test_recover_refuses_a_live_pid_and_clears_a_dead_pid(self):
        proc, pid = self.holder()
        try:
            before = self.admin("cat", f"{LOCK}/.mutex/process")
            self.assertIn("live process", self.call("recover", "left", want=3))
            self.assertEqual(self.admin("cat", f"{LOCK}/.mutex/process"), before)
            self.admin("kill", "-9", str(pid))
            self.stop_holder(proc)
            self.assertIn("recovered dead mutex", self.call("recover", "left"))
            self.call("left", "--", "true", tool="deck")
        finally:
            if proc.poll() is None:
                self.stop_holder(proc)
            self.clear_fixture_mutex()

    def test_recover_detects_pid_reuse_and_refuses_unknown_metadata(self):
        self.admin("python3", "-c", f"from pathlib import Path; p=Path('{LOCK}/.mutex'); p.mkdir(); "
                   "ticks=int(Path('/proc/1/stat').read_text().rsplit(')',1)[1].split()[19]); "
                   "(p/'process').write_text(f'1 {ticks+1}\\n')")
        try:
            self.assertIn("recovered dead mutex", self.call("recover", "left"))
            self.admin("mkdir", f"{LOCK}/.mutex")
            self.call("recover", "left", want=3)
            self.assertEqual(self.admin("sh", "-c", f"test -d {LOCK}/.mutex && echo retained"), "retained")
        finally:
            self.clear_fixture_mutex()

    def test_recovery_keeps_a_replacement_live_mutex_after_holder_exit(self):
        proc, pid = self.holder()
        recover = replacement = None
        try:
            # Pause the liveness probe after recovery has pinned the old directory.
            shim = (f'#!/bin/sh\ncase "$*" in */proc/{pid}/stat*) '
                    'touch /run/recovery-read; while [ ! -e /run/recovery-go ]; do sleep 0.05; done ;; esac\n'
                    'exec /bin/sed "$@"\n')
            self.admin("python3", "-c", "from pathlib import Path; p=Path('/run/review-sed-bin'); p.mkdir(exist_ok=True); "
                       f"f=p/'sed'; f.write_text({shim!r}); f.chmod(0o755)")
            deck = dp.Config(self.ini, env={}).deck("left")
            command = ["env", "PATH=/run/review-sed-bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                       "busybox", "sh", "-c", dp.AGENT.read_text(), "deck-agent", "lock-recover"]
            recover = subprocess.Popen(["ssh", *deck.ssh_options, deck.ssh, shlex.join(command)],
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                if self.admin("sh", "-c", "test -e /run/recovery-read && echo ready || :") == "ready":
                    break
                time.sleep(0.05)
            else:
                self.fail("recovery did not reach its PID probe")
            self.stop_holder(proc)
            replacement, new_pid = self.holder()
            self.admin("touch", "/run/recovery-go")
            out = recover.communicate(timeout=10)[0]
            self.assertEqual(recover.returncode, 0, out)
            self.assertEqual(int(self.admin("cat", f"{LOCK}/.mutex/process").split()[0]), new_pid)
            self.assertIn("live process", self.call("recover", "left", want=3))
        finally:
            self.admin("touch", "/run/recovery-go")
            for holder in (proc, replacement):
                if holder is not None and holder.poll() is None:
                    self.stop_holder(holder)
            if recover is not None and recover.poll() is None:
                recover.communicate(timeout=10)
            self.clear_fixture_mutex()

    def test_wait_timeout_leaves_left_available_to_a_single_deck_user(self):
        self.call("acquire", "right", "--purpose", "busy", owner="bob")
        waiter = self.start("acquire", "right", "left", "--purpose", "pair", "--wait", "--wait-timeout", "2s")
        try:
            self.call("acquire", "left", "--purpose", "single", owner="charlie")
            out = waiter.communicate(timeout=15)[0]
            self.assertEqual(waiter.returncode, 3, out)
            self.assertIn("timed out", out)
            self.assertIn("owner=charlie", self.call("status", "left"))
        finally:
            if waiter.poll() is None:
                waiter.terminate()
                waiter.communicate(timeout=10)

    def test_release_continues_after_a_refusal_and_does_not_claim_empty_locks(self):
        self.assertNotIn("released left", self.call("release", "left"))
        self.call("acquire", "left", "--purpose", "ours")
        self.call("acquire", "right", "--purpose", "other", owner="bob")
        self.call("release", "left", "right", want=3)
        self.assertIn("left: free", self.call("status", "left"))
        self.assertIn("owner=bob", self.call("status", "right"))

    def test_run_hup_stops_child_and_releases_both_decks(self):
        marker = self.tmp / "hup-child.pid"
        proc = self.start("run", "left", "right", "--purpose", "hup", "--", "sh", "-c",
                          'echo $$ > "$1"; exec sleep 20', "sh", str(marker))
        deadline = time.monotonic() + 10
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(marker.exists())
        proc.send_signal(signal.SIGHUP)
        out = proc.communicate(timeout=15)[0]
        self.assertEqual(proc.returncode, 130, out)
        self.assertFalse(Path(f"/proc/{int(marker.read_text())}").exists())
        self.assertIn("left: free", self.call("status", "left"))
        self.assertIn("right: free", self.call("status", "right"))

    def test_deck_side_command_guard_and_explicit_reboot_permission(self):
        self.call("acquire", "left", "--purpose", "command guard")
        # Every potentially destructive name below is a recording stub, never a host tool.
        self.admin("python3", "-c", "from pathlib import Path; p=Path('/run/review-bin'); p.mkdir(exist_ok=True); "
                   "[(p/n).write_text('#!/bin/sh\\nprintf executed >> /run/review-command-ran\\n') or None "
                   "for n in ['az01-update','factory-reset','reboot','shutdown','poweroff','halt','mkfs.ext4','dd','rm','systemctl']]; "
                   "[(p/n).chmod(0o755) for n in p.iterdir()]")
        bad = [("az01-update", "--erase-data"), ("factory-reset",), ("reboot",), ("shutdown", "-r", "now"),
               ("poweroff",), ("halt",), ("mkfs.ext4", "/dev/review"), ("dd", "if=/dev/zero", "of=/dev/review"),
               ("rm", "-rf", "/data"), ("systemctl", "reboot")]
        for command in bad:
            with self.subTest(command=command):
                self.call("left", "--", "/run/review-bin/" + command[0], *command[1:], tool="deck", want=3)
        self.call("run", "left", "--purpose", "shell guard", "--ssh", "--", "sh", "-c",
                  "/run/review-bin/reboot", want=3)
        self.assertEqual(self.admin("sh", "-c", "test -e /run/review-command-ran && echo ran || echo absent"), "absent")
        self.call("left", "--allow-reboot", "--", "/run/review-bin/reboot", tool="deck")
        self.assertEqual(self.admin("cat", "/run/review-command-ran"), "executed")

    def test_owner_is_checked_by_the_deck_even_if_the_client_sets_its_owner(self):
        self.call("acquire", "left", "--purpose", "bob", owner="bob")
        deck = dp.Config(self.ini, env={}).deck("left")
        deck.owner = "alice"
        self.assertEqual(deck.agent(["exec", "--", "true"]), 3)


@unittest.skipUnless(base._can_fake(), "needs root, unshare and sshd")
class ReviewPush(unittest.TestCase):
    setUpClass = base.FakeDeck.__dict__["setUpClass"]
    tearDownClass = base.FakeDeck.__dict__["tearDownClass"]
    setUp = base.FakeDeck.setUp
    tearDown = base.FakeDeck.tearDown
    run_dp = base.FakeDeck.run_dp
    deck_sh = base.FakeDeck.deck_sh
    snapshot = base.FakeDeck.snapshot

    def test_successful_backup_without_a_vault_id_does_not_apply(self):
        backup = self.deckdir / "test-bin/agent-backup"
        saved = backup.read_bytes()
        try:
            backup.write_text("#!/bin/sh\nprintf '%s\\n' '{}'\n")
            before = self.snapshot()
            rc, out = self.run_dp("fake", "hook", "--from", str(self.pre))
            self.assertEqual(rc, 4, out)
            self.assertIn("did not return an id", out)
            self.assertEqual(self.snapshot(), before)
            self.assertEqual(self.deck_sh("test -e /run/enginex-dev/mounted && echo mounted || echo absent").strip(), "absent")
        finally:
            backup.write_bytes(saved)

    def test_push_and_rollback_leave_foreign_mounts_intact(self):
        path = "/etc/sc5000/libengine-ui-hook.so"
        self.deck_sh(f"printf foreign > /run/review-foreign; mount --bind /run/review-foreign {path}")
        try:
            rc, out = self.run_dp("fake", "hook", "--from", str(self.pre))
            self.assertEqual(rc, 3, out)
            self.assertIn("not created by deck-push", out)
            self.assertEqual(self.deck_sh(f"cat {path}"), "foreign")
        finally:
            self.deck_sh(f"umount -l {path}")
        # A foreign mount stacked above one of ours must also survive rollback.
        rc, out = self.run_dp("fake", "hook", "--from", str(self.pre))
        self.assertEqual(rc, 0, out)
        self.deck_sh(f"mount --bind /run/review-foreign {path}")
        try:
            rc, out = self.run_dp("fake", "--rollback")
            self.assertEqual(rc, 3, out)
            self.assertIn("not created by deck-push", out)
            self.assertEqual(self.deck_sh(f"cat {path}"), "foreign")
        finally:
            self.deck_sh(f"umount -l {path}")
