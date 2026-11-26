"""The one result format of tools/dev's runners (gorun.py, pyrun.py, crun.py, devtest.py's bash steps).

A runner's JSON: {"ok": bool, "wall": s, "results": [{"kind", "where", "test", "status", "secs", "reason"}],
"flaky": [{"kind", "where", "test", "first_failure"}], "errors": [str], "notes": [str], ...extra}
  kind    go | py | c | sh
  where   Go package dir, Python test directory, C test source, bash test
  test    Go top-level test, Python test id (module.Class.method), C variant, `*` for a bash test
  status  pass | fail | skip | notrun (a test that exists but this host cannot run: the reason says why)
`errors` are failures that belong to no single test (a build error, a listing that failed, tests that reported no
result). `ok` is true only with no failed result and no error. A flaky entry is a test that failed and then passed
alone (only with --rerun); its result is a pass, the entry keeps the first failure.
"""
import json, pathlib

STATUSES = ('pass', 'fail', 'skip', 'notrun')


class Results:
    def __init__(self):
        self.items, self.flaky, self.errors, self.notes, self.extra, self.wall = [], [], [], [], {}, 0.0

    def add(self, kind, where, test, status, secs=0.0, reason=''):
        assert status in STATUSES, status
        self.items.append({'kind': kind, 'where': where, 'test': test, 'status': status, 'secs': round(float(secs or 0), 3),
                           'reason': reason or ''})

    def flaky_one(self, kind, where, test, first_failure):
        self.flaky.append({'kind': kind, 'where': where, 'test': test, 'first_failure': first_failure or ''})

    def error(self, text):
        self.errors.append(text)

    def failed(self):
        return [r for r in self.items if r['status'] == 'fail']

    def ok(self):
        return not self.failed() and not self.errors

    def summary(self, who):
        n = {s: sum(1 for r in self.items if r['status'] == s) for s in STATUSES}
        return (f"{who}: {len(self.items)} tests, {n['pass']} passed, {n['fail']} failed, {n['skip']} skipped, "
                f"{n['notrun']} not run, {len(self.flaky)} flaky, {len(self.errors)} errors, {self.wall:.1f}s -> "
                f"{'OK' if self.ok() else 'FAILED'}")

    def as_dict(self):
        return dict(self.extra, ok=self.ok(), wall=round(self.wall, 1), results=self.items, flaky=self.flaky,
                    errors=self.errors, notes=self.notes)

    def write(self, path):
        pathlib.Path(path).write_text(json.dumps(self.as_dict(), indent=1))


def load(path):
    """A runner's JSON, or None when it wrote none (it crashed: the caller counts that as a failure)."""
    try:
        d = json.loads(pathlib.Path(path).read_text())
    except (OSError, ValueError):
        return None
    for key in ('results', 'flaky', 'errors', 'notes'):
        d.setdefault(key, [])
    return d
