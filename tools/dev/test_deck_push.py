"""Unit tests for tools/dev/deck-push and deck-agent.sh.

    python3 -m unittest discover -s tools/dev

The FakeDeck cases need root on the Linux build host (namespaces, bind mounts);
elsewhere they are skipped. They run deck-push end to end against
tools/dev/fake-deck.sh with small prebuilt artifacts (--from)."""
import hashlib
import importlib.machinery
import importlib.util
import io
import json
import multiprocessing
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import unittest.mock
from pathlib import Path

HERE = Path(__file__).resolve().parent
_loader = importlib.machinery.SourceFileLoader("deck_push", str(HERE / "deck-push"))
_spec = importlib.util.spec_from_loader("deck_push", _loader)
dp = importlib.util.module_from_spec(_spec)
_loader.exec_module(dp)


class ArgParsing(unittest.TestCase):
    def bad(self, argv):
        with self.assertRaises(SystemExit) as cm, unittest.mock.patch("sys.stderr", io.StringIO()):
            dp.parse_args(argv)
        self.assertEqual(cm.exception.code, 2)

    def test_push_components(self):
        for comp in ("ui", "hook", "edisksd", "go:stream-sync", "go:sc5000-deemixd", "go:rb-bridge", "all"):
            a = dp.parse_args(["deck-a", comp])
            self.assertEqual((a.action, a.deck, a.component), ("push", "deck-a", comp))
        a = dp.parse_args(["deck-a", "ui", "--commit", "abc1234", "--dry-run", "--no-sandbox", "layout only"])
        self.assertTrue(a.dry_run)
        self.assertEqual(a.commit, "abc1234")

    def test_bad_push(self):
        self.bad(["deck-a"])                                  # no component
        self.bad(["deck-a", "kernel"])                        # unknown component
        self.bad(["deck-a", "go:engine-guardian"])            # retired, no unit to restart
        self.bad(["deck-a", "go:"])
        self.bad(["deck-a", "ui", "--commit", "x", "--from", "/tmp"])
        self.bad(["deck-a", "ui", "--rollback"])
        self.bad(["deck-a", "--rollback", "--dry-run"])
        self.bad(["deck-a", "ui", "--no-sandbox", "  "])

    def test_rollback(self):
        a = dp.parse_args(["deck-a", "--rollback"])
        self.assertEqual((a.action, a.component), ("rollback", None))

    def test_subcommands(self):
        a = dp.parse_args(["lock", "deck-a", "--purpose", " MASTER   tests ", "--ttl", "90m"])
        self.assertEqual((a.action, a.deck, a.purpose, a.ttl), ("lock", "deck-a", "MASTER tests", 5400))
        self.assertEqual(dp.parse_args(["lock", "deck-a", "--purpose", "x"]).ttl, 3600)
        self.assertEqual(dp.parse_args(["status"]).deck, None)
        self.assertEqual(dp.parse_args(["backups", "deck-b"]).deck, "deck-b")
        self.assertEqual(dp.parse_args(["unlock", "deck-a"]).action, "unlock")
        self.bad(["lock", "deck-a"])                          # purpose is required
        self.bad(["lock", "deck-a", "--purpose", "x", "--ttl", "13h"])
        self.bad(["lock", "deck-a", "--purpose", "x" * 201])
        self.bad(["unlock"])
        self.bad(["smoke"])

    def test_ttl(self):
        self.assertEqual(dp.parse_ttl("2h"), 7200)
        self.assertEqual(dp.parse_ttl("30"), 1800)
        self.assertEqual(dp.parse_ttl("45s"), 45)
        for bad in ("0", "1d", "-5m", "h", "13h"):
            with self.assertRaises(ValueError):
                dp.parse_ttl(bad)

    def test_component_plan(self):
        self.assertEqual(dp.component_plan("ui"), (["ui-packs", "ui-icons"], ["engine.service"]))
        self.assertEqual(dp.component_plan("hook"), (["hook"], ["engine.service"]))
        self.assertEqual(dp.component_plan("edisksd"), (["edisksd"], ["edisksd.service"]))
        self.assertEqual(dp.component_plan("go:rb-bridge"), (["suite"], ["rb-bridge.service"]))
        names, services = dp.component_plan("all")
        self.assertEqual(sorted(names), sorted(dp.ARTIFACTS))
        self.assertEqual(len(services), 5)

    def test_owner(self):
        a = dp.parse_args(["deck-a", "ui"])
        self.assertEqual(dp.resolve_owner(a, {"PASEO_AGENT_ID": "2c1bba51"}), "2c1bba51")
        self.assertEqual(dp.resolve_owner(a, {"PASEO_AGENT_ID": "p", "DECK_PUSH_OWNER": "o"}), "o")
        a.owner = "me"
        self.assertEqual(dp.resolve_owner(a, {"DECK_PUSH_OWNER": "o"}), "me")
        a.owner = "bad owner"
        with self.assertRaises(dp.Refused):
            dp.resolve_owner(a, {})
        a.owner = None
        with self.assertRaises(dp.Refused):
            dp.resolve_owner(a, {})



class Configs(unittest.TestCase):
    def test_example(self):
        cfg = dp.Config(HERE / "decks.ini.example", env={})
        self.assertEqual(sorted(cfg.decks), ["deck-a", "deck-b"])
        a, b = cfg.decks["deck-a"], cfg.decks["deck-b"]
        self.assertEqual((a.expect_applied, b.expect_applied), (19, 19))
        self.assertEqual(b.daemons, ["stream-sync", "sc5000-deemixd", "rb-bridge"])
        self.assertEqual(a.ssh_options, ["-o", "ServerAliveInterval=10"])
        self.assertEqual(cfg.state, Path("/var/lib/enginex-dev"))
        self.assertEqual(dp.Config(HERE / "decks.ini.example", env={"DECK_PUSH_STATE": "/x/state"}).state, Path("/x/state"))

    def test_bad_names(self):
        with tempfile.TemporaryDirectory() as d:
            for name in ("status", "Deck", "a b", "../x"):
                p = Path(d, "decks.ini")
                p.write_text(f"[{name}]\nssh = x\n")
                with self.assertRaises(ValueError):
                    dp.Config(p, env={})
            p.write_text("[deck-c]\nssh_options = -v\n")
            with self.assertRaises(ValueError):
                dp.Config(p, env={})                         # ssh destination missing

    def test_unknown_deck(self):
        cfg = dp.Config(HERE / "decks.ini.example", env={})
        with self.assertRaises(dp.Refused):
            cfg.deck("deck-z")



def _race(state, owner, q):
    try:
        dp.LockStore(state).acquire("deck-a", owner, "race", 600)
        q.put(owner)
    except dp.Refused:
        pass


class Locks(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.locks = dp.LockStore(self.tmp)

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def test_one_owner(self):
        lock = self.locks.acquire("deck-a", "alice", "MASTER tests", 600, now=1000)
        self.assertEqual((lock["owner"], lock["expires"]), ("alice", 1600))
        with self.assertRaises(dp.Refused):
            self.locks.acquire("deck-a", "bob", "ui", 600, now=1100)
        with self.assertRaises(dp.Refused):
            self.locks.release("deck-a", "bob", now=1100)
        with self.assertRaises(dp.Refused):
            self.locks.require("deck-a", "bob", now=1100)
        self.assertEqual(self.locks.require("deck-a", "alice", now=1100)["purpose"], "MASTER tests")
        # Another deck is independent.
        self.locks.acquire("deck-b", "bob", "ui", 600, now=1100)

    def test_renew_keeps_acquired(self):
        self.locks.acquire("deck-a", "alice", "one", 600, now=1000)
        lock = self.locks.acquire("deck-a", "alice", "two", 600, now=1500)
        self.assertEqual((lock["acquired"], lock["expires"], lock["purpose"]), (1000, 2100, "two"))

    def test_expiry_frees(self):
        self.locks.acquire("deck-a", "alice", "one", 600, now=1000)
        self.assertIsNone(self.locks.holder("deck-a", now=1600))
        with self.assertRaises(dp.Refused):
            self.locks.require("deck-a", "alice", now=1600)   # an expired lock does not allow a push
        self.assertEqual(self.locks.acquire("deck-a", "bob", "two", 600, now=1601)["owner"], "bob")

    def test_release(self):
        self.assertFalse(self.locks.release("deck-a", "alice"))
        self.locks.acquire("deck-a", "alice", "one", 600)
        self.assertTrue(self.locks.release("deck-a", "alice"))
        with self.assertRaises(dp.Refused):
            self.locks.require("deck-a", "alice")

    def test_race(self):
        q = multiprocessing.Queue()
        ps = [multiprocessing.Process(target=_race, args=(self.tmp, f"agent{i}", q)) for i in range(8)]
        for p in ps:
            p.start()
        for p in ps:
            p.join()
        winners = []
        while not q.empty():
            winners.append(q.get())
        self.assertEqual(len(winners), 1)
        self.assertEqual(self.locks.holder("deck-a")["owner"], winners[0])

    def test_op_lock_is_exclusive(self):
        p = Path(self.tmp, "x.op")
        with dp.flocked(p, blocking=False):
            with self.assertRaises(dp.Refused):
                with dp.flocked(p, blocking=False):
                    pass


class Digests(unittest.TestCase):
    def test_same_as_deck_agent(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d, "packs")
            for rel, text in (("en/a b.qml", "1"), ("en/a/b.qml", "2"), ("en/a-b.qml", "3"),
                              ("zh-Hans/Skins/X.qml", "4"), ("icons/Z.svg", "5"), ("B.txt", "")):
                (root / rel).parent.mkdir(parents=True, exist_ok=True)
                (root / rel).write_text(text)
            (root / "link").symlink_to("B.txt")              # find -type f skips symlinks too
            for p in (root, root / "en/a b.qml"):
                sh = subprocess.run(["sh", str(HERE / "deck-agent.sh"), "digest", str(p)],
                                    capture_output=True, text=True, check=True).stdout.strip()
                self.assertEqual(dp.digest(p), sh)
            before = dp.digest(root)
            (root / "en/a/b.qml").write_text("changed")
            self.assertNotEqual(dp.digest(root), before)


class BackupIndex(unittest.TestCase):
    def test_index_and_dirs(self):
        with tempfile.TemporaryDirectory() as d:
            b = dp.Backups(d)
            d1, d2 = b.new("deck-a", "go:stream-sync", now=0), b.new("deck-a", "go:stream-sync", now=0)
            self.assertNotEqual(d1, d2)
            self.assertEqual(d1.name, "19700101T000000Z-go-stream-sync")
            b.record({"deck": "deck-a", "action": "push"})
            b.record({"deck": "deck-b", "action": "push"})
            self.assertEqual(len(b.entries()), 2)
            self.assertEqual([e["deck"] for e in b.entries("deck-b")], ["deck-b"])

    def test_check_backup(self):
        with tempfile.TemporaryDirectory() as d:
            f = Path(d, "hook.so")
            f.write_text("x")
            with tarfile.open(Path(d, "t.tar"), "w") as t:
                t.add(f, arcname="etc/sc5000/libengine-ui-hook.so")
            self.assertEqual(dp.check_backup(Path(d, "t.tar"), {"hook": "/etc/sc5000/libengine-ui-hook.so"}), 1)
            with self.assertRaises(dp.Failed):
                dp.check_backup(Path(d, "t.tar"), {"edisksd": "/etc/sc5000/libedisksd-hook.so"})


class UploadTar(unittest.TestCase):
    def test_layout(self):
        with tempfile.TemporaryDirectory() as d:
            stage = Path(d, "stage")
            (stage / "ui/packs/en").mkdir(parents=True)
            (stage / "ui/packs/en/A.qml").write_text("a")
            (stage / "ui/icons").mkdir(parents=True)
            (stage / "sc5000-suite").write_text("#!/bin/sh\n")
            os.chmod(stage / "sc5000-suite", 0o700)
            dp.upload_tar(stage, ["ui-packs", "ui-icons", "suite"], ["engine.service"], {"commit": "c"}, Path(d, "u.tar"))
            with tarfile.open(Path(d, "u.tar")) as t:
                m = {x.name: x for x in t.getmembers()}
                plan = t.extractfile("plan").read().decode()
            self.assertEqual(plan, "ui-packs files/ui/packs\nui-icons files/ui/icons\nsuite files/sc5000-suite\n")
            self.assertTrue(all(x.uid == 0 and x.gid == 0 for x in m.values()))
            self.assertEqual(m["files/sc5000-suite"].mode, 0o755)
            self.assertEqual(m["files/ui/packs/en/A.qml"].mode, 0o644)
            self.assertIn("manifest.json", m)


GOOD = """now 1000
emain 812
emain_started 990
hook_mapped yes
applied 19
engine_line engine-ui-hook: Engine 5.3.0
packs_line engine-ui-hook: /data/sc5000-custom/ui/packs/zh-Hans: 61 files
qml_errors 0
crashes 0
daemon stream-sync active
daemon sc5000-deemixd active
daemon rb-bridge active
api_ok 1
api_version 5.3.0.4
version_txt 5.3.0.4
manifest {"commit": "c0ffee", "component": "ui"}
digest ui-packs aaaa mounted
"""


class Smoke(unittest.TestCase):
    def setUp(self):
        self.deck = dp.Config(HERE / "decks.ini.example", env={}).decks["deck-a"]
        self.expect = {"digests": {"ui-packs": ("aaaa", "mounted")}, "commit": "c0ffee", "restarted_after": 985}

    def judge(self, text, **expect):
        rows = dp.evaluate(dp.facts(text), self.deck, {**self.expect, **expect})
        return {what: ok for ok, what, _ in rows}

    def test_all_pass(self):
        rows = self.judge(GOOD)
        self.assertTrue(all(rows.values()), rows)
        self.assertEqual(len(rows), 9)
        self.assertTrue(dp.ready(dp.facts(GOOD), self.deck, self.expect))

    def failing(self, text, **expect):
        return sorted(k for k, ok in self.judge(text, **expect).items() if not ok)

    def test_failures(self):
        self.assertEqual(self.failing(GOOD.replace("applied 19", "applied 18")), ["UI hook loaded, 19 edits applied"])
        self.assertEqual(self.failing(GOOD.replace("qml_errors 0", "qml_errors 2")), ["no QML load errors or crashes in the journal"])
        self.assertEqual(self.failing(GOOD.replace("crashes 0", "crashes 1")), ["no QML load errors or crashes in the journal"])
        self.assertEqual(self.failing(GOOD.replace("rb-bridge active", "rb-bridge failed")), ["our daemons alive"])
        self.assertEqual(self.failing(GOOD.replace("api_ok 1", "api_ok 0")), ["stream-sync API answers"])
        self.assertEqual(self.failing(GOOD.replace("api_version 5.3.0.4", "api_version 5.3.0.3")), ["version matches"])
        self.assertEqual(self.failing(GOOD.replace("aaaa mounted", "bbbb mounted")), ["ui-packs digest"])
        self.assertEqual(self.failing(GOOD.replace("aaaa mounted", "aaaa unmounted")), ["ui-packs digest"])
        self.assertEqual(self.failing(GOOD.replace('"c0ffee"', '"beef"')), ["deck runs the pushed commit"])
        self.assertIn("UI pack loaded", self.failing(GOOD.replace("61 files", "0 files")))
        # Engine that was not restarted by this push (started before the restart time).
        self.assertEqual(self.failing(GOOD, restarted_after=1100), ["Engine up (EMain)"])
        self.assertFalse(dp.ready(dp.facts(GOOD), self.deck, {**self.expect, "restarted_after": 1100}))
        down = "\n".join(line for line in GOOD.splitlines() if not line.startswith(("emain", "applied", "qml", "crashes", "engine_line", "packs_line")))
        self.assertIn("Engine up (EMain)", self.failing(down + "\nemain -\n"))
        self.assertIn("no QML load errors or crashes in the journal", self.failing(down + "\nemain -\n"))

    def test_rollback_expectations(self):
        rows = dp.evaluate(dp.facts(GOOD), self.deck, {"restarted_after": 985})
        self.assertEqual(len(rows), 7)                          # no commit and no digest rows
        self.assertTrue(all(r[0] for r in rows))


def _can_fake():
    return (sys.platform.startswith("linux") and os.geteuid() == 0 and shutil.which("unshare") is not None
            and shutil.which("nsenter") is not None and Path("/usr/sbin/sshd").exists())


@unittest.skipUnless(_can_fake(), "needs root, unshare and sshd (the build host)")
class FakeDeck(unittest.TestCase):
    """deck-push against tools/dev/fake-deck.sh, as an agent would run it."""

    @classmethod
    def setUpClass(cls):
        cls.dir = Path(tempfile.mkdtemp(prefix="deck-push-test.", dir="/var/tmp"))
        cls.deckdir = cls.dir / "deck"
        cls.ini = subprocess.run(["bash", str(HERE / "fake-deck.sh"), "up", str(cls.deckdir)],
                                 check=True, capture_output=True, text=True).stdout.strip().splitlines()[-1]
        cls.persist = cls.deckdir / "persist"
        cls.pre = cls.dir / "pre"
        (cls.pre / "ui/packs/en/Skins/Embedded").mkdir(parents=True)
        (cls.pre / "ui/packs/en/Skins/Embedded/EmbeddedMain.qml").write_text("import QtQuick 2.15 // pushed pack\n")
        (cls.pre / "ui/packs/en/Skins/Embedded/Extra.qml").write_text("Item {}\n")
        (cls.pre / "ui/icons").mkdir()
        (cls.pre / "ui/icons/pushed.svg").write_text("<svg/>\n")
        (cls.pre / "libengine-ui-hook.so").write_text("pushed hook FAKE_APPLIED=19\n")
        (cls.pre / "libedisksd-hook.so").write_text("pushed edisksd hook\n")
        shutil.copy2(HERE / "fake-deck/sc5000-suite", cls.pre / "sc5000-suite")
        with open(cls.pre / "sc5000-suite", "a") as f:
            f.write("# pushed suite\n")

    @classmethod
    def tearDownClass(cls):
        subprocess.run(["bash", str(HERE / "fake-deck.sh"), "down", str(cls.deckdir)], check=False)
        shutil.rmtree(cls.dir, ignore_errors=True)

    def run_dp(self, *args, owner="agent-a"):
        env = {**os.environ, "DECK_PUSH_CONFIG": self.ini, "DECK_PUSH_OWNER": owner}
        env.pop("PASEO_AGENT_ID", None)
        r = subprocess.run([sys.executable, str(HERE / "deck-push"), *args], env=env, capture_output=True, text=True, timeout=300)
        return r.returncode, r.stdout + r.stderr

    def deck_sh(self, cmd):
        cfg = dp.Config(self.ini, env={})
        d = cfg.decks["fake"]
        return subprocess.run(["ssh", *d.ssh_options, d.ssh, cmd], capture_output=True, text=True, check=True).stdout

    def snapshot(self):
        """Every file of the deck's flash (/data, /etc/sc5000) and its content."""
        out = {}
        for root, _dirs, files in os.walk(self.persist):
            for f in files:
                p = Path(root, f)
                out[str(p.relative_to(self.persist))] = p.read_bytes() if not p.is_symlink() else os.readlink(p)
        return out

    def setUp(self):
        self.run_dp("unlock", "fake", owner="agent-a")
        self.run_dp("unlock", "fake", owner="agent-b")
        self.run_dp("lock", "fake", "--purpose", "unit tests", owner="agent-a")

    def tearDown(self):
        self.run_dp("lock", "fake", "--purpose", "unit tests", owner="agent-a")
        rc, out = self.run_dp("fake", "--rollback", owner="agent-a")
        self.assertIn(rc, (0, 1), out)
        self.assertEqual(self.deck_sh("test -e /run/enginex-dev && echo left || echo gone").strip(), "gone", out)

    def test_refuses_without_lock_or_with_another_owner(self):
        before = self.snapshot()
        rc, out = self.run_dp("fake", "hook", "--from", str(self.pre), owner="agent-b")
        self.assertEqual(rc, 3, out)
        self.assertIn("locked by agent-a", out)
        self.run_dp("unlock", "fake", owner="agent-a")
        rc, out = self.run_dp("fake", "hook", "--from", str(self.pre), owner="agent-a")
        self.assertEqual(rc, 3, out)
        self.assertIn("is not locked", out)
        rc, out = self.run_dp("fake", "--rollback", owner="agent-a")
        self.assertEqual(rc, 3, out)
        self.assertEqual(self.deck_sh("cat /run/enginex-dev/mounted 2>/dev/null || echo none").strip(), "none")
        self.assertEqual(self.snapshot(), before)

    def test_push_backup_rollback(self):
        before = self.snapshot()
        orig = (self.persist / "etc-sc5000/libengine-ui-hook.so").read_bytes()
        rc, out = self.run_dp("fake", "hook", "--from", str(self.pre))
        self.assertEqual(rc, 0, out)
        self.assertIn("RESULT PASS fake", out)
        # The deck shows the pushed file; the file underneath (its flash) is untouched.
        self.assertEqual(self.deck_sh("cat /etc/sc5000/libengine-ui-hook.so"), "pushed hook FAKE_APPLIED=19\n")
        self.assertEqual(self.snapshot(), before)
        # Backup on the build host, indexed, holding exactly what the deck showed before.
        state = Path(dp.Config(self.ini, env={}).state)
        entry = [e for e in dp.Backups(state).entries("fake") if e["action"] == "push"][-1]
        self.assertEqual(entry["replaced"]["hook"]["path"], "/etc/sc5000/libengine-ui-hook.so")
        with tarfile.open(Path(entry["dir"], "targets.tar")) as t:
            self.assertEqual(t.extractfile("etc/sc5000/libengine-ui-hook.so").read(), orig)
        # A second push over the first: rollback still brings back the original, not push one.
        (self.dir / "pre2").mkdir(exist_ok=True)
        (self.dir / "pre2/libengine-ui-hook.so").write_text("second hook FAKE_APPLIED=19\n")
        rc, out = self.run_dp("fake", "hook", "--from", str(self.dir / "pre2"))
        self.assertEqual(rc, 0, out)
        self.assertIn("already dev-mounted: hook", out)
        rc, out = self.run_dp("fake", "--rollback")
        self.assertEqual(rc, 0, out)
        self.assertIn("PASS hook back to its original", out)
        self.assertEqual(self.deck_sh("sha256sum < /etc/sc5000/libengine-ui-hook.so").split()[0], hashlib.sha256(orig).hexdigest())
        self.assertEqual(self.deck_sh("test -e /run/enginex-dev && echo left || echo gone").strip(), "gone")
        self.assertEqual(self.snapshot(), before)
        rc, out = self.run_dp("fake", "--rollback")
        self.assertEqual((rc, "nothing to roll back" in out), (0, True), out)

    def test_ui_needs_sandbox_sheet(self):
        rc, out = self.run_dp("fake", "ui", "--from", str(self.pre))
        self.assertEqual(rc, 3, out)
        self.assertIn("tools/dev/ui-sheet", out)
        self.assertEqual(self.deck_sh("cat /run/enginex-dev/mounted 2>/dev/null || echo none").strip(), "none")
        # A sheet record for exactly this pack digest lets it through.
        state = Path(dp.Config(self.ini, env={}).state)
        sheet = state / "sheets" / f"{dp.digest(self.pre / 'ui/packs')}.json"
        sheet.parent.mkdir(parents=True, exist_ok=True)
        sheet.write_text(json.dumps({"sheet": "/var/tmp/x/sheet.png"}))
        try:
            rc, out = self.run_dp("fake", "ui", "--from", str(self.pre))
        finally:
            sheet.unlink()
        self.assertEqual(rc, 0, out)
        self.assertIn("sandbox: /var/tmp/x/sheet.png", out)
        self.assertIn("PASS UI pack loaded: packs/en: 2 files", out)

    def test_failing_smoke_then_rollback(self):
        bad = self.dir / "bad"
        shutil.copytree(self.pre / "ui", bad / "ui", dirs_exist_ok=True)
        (bad / "ui/packs/en/Skins/Embedded/Broken.qml").write_text("// FAKE_QML_ERROR\n")
        rc, out = self.run_dp("fake", "ui", "--from", str(bad), "--no-sandbox", "unit test: broken pack")
        self.assertEqual(rc, 1, out)
        self.assertIn("FAIL no QML load errors or crashes in the journal: 1 QML errors", out)
        self.assertIn("undo: deck-push fake --rollback", out)
        (bad / "libengine-ui-hook.so").write_text("hook with an edit missing FAKE_APPLIED=18\n")
        rc, out = self.run_dp("fake", "hook", "--from", str(bad))
        self.assertEqual(rc, 1, out)
        self.assertIn("FAIL UI hook loaded, 19 edits applied: 18 'applied' lines", out)
        rc, out = self.run_dp("fake", "--rollback")
        self.assertEqual(rc, 0, out)
        self.assertEqual(out.count("back to its original"), 3, out)
        self.assertIn("RESULT PASS fake", out)

    def test_go_restarts_only_its_daemon(self):
        emain = self.deck_sh("cat /run/fake-systemd/engine.service.pid")
        mark = self.deck_sh("wc -l < /run/fake-systemd/log").strip()
        rc, out = self.run_dp("fake", "go:sc5000-deemixd", "--from", str(self.pre))
        self.assertEqual(rc, 0, out)
        started = self.deck_sh(f"tail -n +{int(mark) + 1} /run/fake-systemd/log").split()[2::3]
        self.assertEqual(started, ["sc5000-deemixd.service"])
        self.assertEqual(self.deck_sh("cat /run/fake-systemd/engine.service.pid"), emain)
        rc, out = self.run_dp("fake", "edisksd", "--from", str(self.pre))
        self.assertEqual(rc, 0, out)
        self.assertIn("deck: restart edisksd.service", out)
        self.assertNotIn("restart engine.service", out)

    def test_reboot_rolls_back(self):
        rc, out = self.run_dp("fake", "hook", "--from", str(self.pre))
        self.assertEqual(rc, 0, out)
        subprocess.run(["bash", str(HERE / "fake-deck.sh"), "reboot", str(self.deckdir)], check=True, capture_output=True)
        self.assertEqual(self.deck_sh("sha256sum < /etc/sc5000/libengine-ui-hook.so").split()[0],
                         dp.digest(self.persist / "etc-sc5000/libengine-ui-hook.so"))
        rc, out = self.run_dp("smoke", "fake")
        self.assertEqual(rc, 0, out)
        self.assertNotIn("digest", out)

    def test_dry_run_never_contacts_the_deck(self):
        self.run_dp("unlock", "fake", owner="agent-a")
        env_ini = Path(self.ini).read_text()
        broken = self.dir / "broken.ini"
        broken.write_text(env_ini.replace("ProxyCommand=", "ProxyCommand=false "))
        env = {**os.environ, "DECK_PUSH_CONFIG": str(broken), "DECK_PUSH_OWNER": "agent-a"}
        r = subprocess.run([sys.executable, str(HERE / "deck-push"), "fake", "hook", "--from", str(self.pre), "--dry-run"],
                           env=env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("dry run: the deck was not contacted", r.stdout)
        self.assertIn("lock: free", r.stdout)


_sl = importlib.machinery.SourceFileLoader("ui_sheet", str(HERE / "ui-sheet"))
_ss = importlib.util.spec_from_loader("ui_sheet", _sl)
ui_sheet = importlib.util.module_from_spec(_ss)
_sl.exec_module(ui_sheet)


class UiSheet(unittest.TestCase):
    def test_dry_run_plan(self):
        out = io.StringIO()
        with unittest.mock.patch("sys.stdout", out):
            self.assertEqual(ui_sheet.main(["--dry-run", "--views", "4deck,settings-about"]), 0)
        text = out.getvalue()
        self.assertIn("shadow-twin.sh shot before-4deck A --ui 4deck", text)
        self.assertIn("shadow-twin.sh deploy --local --packs --hook --suite", text)
        self.assertIn("shadow-twin.sh shot after-settings-about A --ui settings-about", text)
        self.assertTrue(text.rstrip().endswith("sheets/<UI pack digest>.json"))

    def test_unknown_view(self):
        with self.assertRaises(SystemExit), unittest.mock.patch("sys.stderr", io.StringIO()):
            ui_sheet.main(["--dry-run", "--views", "nope"])

    def test_compose(self):
        try:
            from PIL import Image
        except ImportError:
            self.skipTest("no Pillow")
        with tempfile.TemporaryDirectory() as d:
            rows = []
            for v in ("4deck", "settings-about"):
                b, a = Path(d, f"b-{v}.png"), Path(d, f"a-{v}.png")
                Image.new("RGB", (128, 80), (255, 0, 0)).save(b)
                Image.new("RGB", (128, 80), (0, 255, 0)).save(a)
                rows.append((v, b, a))
            out = ui_sheet.compose(rows, Path(d, "sheet.png"), "UI before x | after y")
            im = Image.open(out).convert("RGB")
            self.assertEqual(im.size, (2 * 128 + 3 * 12, 28 + 2 * (80 + 28 + 12) + 12))
            self.assertEqual(im.getpixel((12 + 5, 28 + 28 + 5)), (255, 0, 0))            # before, left
            self.assertEqual(im.getpixel((2 * 12 + 128 + 5, 28 + 28 + 5)), (0, 255, 0))  # after, right


if __name__ == "__main__":
    unittest.main()
