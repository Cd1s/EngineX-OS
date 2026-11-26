"""The one result format of tools/dev's runners (gorun.py, pyrun.py, crun.py, devtest.py's bash steps).

A runner's JSON: {"ok": bool, "wall": s, "results": [{"kind", "where", "test", "status", "secs", "reason"}],
"errors": [str], "notes": [str], ...extra}
  kind    go | py | c | sh
  where   Go package dir, Python test directory, C test source, bash test
  test    Go top-level test, Python test id (module.Class.method), C variant, `*` for a bash test
  status  pass | fail | skip | notrun (selected, but it did not run here: the reason says why)
`errors` are failures that belong to no single test (a build error, a crashed worker). `ok` is true only with no
failed result and no error.

  python3 tools/dev/results.py --same A.json B.json   two runs gave the same status for the same tests (exit 0)
"""
import hashlib, json, pathlib, sys

STATUSES = ('pass', 'fail', 'skip', 'notrun')


class Results:
    def __init__(self):
        self.items, self.errors, self.notes, self.extra, self.wall = [], [], [], {}, 0.0

    def add(self, kind, where, test, status, secs=0.0, reason=''):
        assert status in STATUSES, status
        self.items.append({'kind': kind, 'where': where, 'test': test, 'status': status, 'secs': round(float(secs or 0), 3),
                           'reason': reason or ''})

    def error(self, text):
        self.errors.append(text)

    def failed(self):
        return [r for r in self.items if r['status'] == 'fail']

    def ok(self):
        return not self.failed() and not self.errors

    def summary(self, who):
        n = {s: sum(1 for r in self.items if r['status'] == s) for s in STATUSES}
        return (f"{who}: {len(self.items)} tests, {n['pass']} passed, {n['fail']} failed, {n['skip']} skipped, "
                f"{n['notrun']} not run, {len(self.errors)} errors, {self.wall:.1f}s -> {'OK' if self.ok() else 'FAILED'}")

    def as_dict(self):
        return dict(self.extra, ok=self.ok(), wall=round(self.wall, 1), results=self.items, errors=self.errors, notes=self.notes)

    def write(self, path):
        pathlib.Path(path).write_text(json.dumps(self.as_dict(), indent=1))


def load(path):
    """A runner's JSON, or None when it wrote none (it crashed: the caller counts that as a failure)."""
    try:
        d = json.loads(pathlib.Path(path).read_text())
    except (OSError, ValueError):
        return None
    for key in ('results', 'errors', 'notes'):
        d.setdefault(key, [])
    return d


def identity(path):
    d = load(path) or {'results': [], 'errors': ['no result file']}
    rows = sorted((x['kind'], x['where'], x['test'], x['status']) for x in d['results'])
    return rows, d['errors'], hashlib.sha256(json.dumps(rows).encode()).hexdigest()[:16]


if __name__ == '__main__':
    if len(sys.argv) != 4 or sys.argv[1] != '--same':
        sys.exit('usage: results.py --same A.json B.json')
    (a, ea, ha), (b, eb, hb) = identity(sys.argv[2]), identity(sys.argv[3])
    same = a == b and not ea and not eb
    print(f"{'SAME' if same else 'DIFFERENT'}: {len(a)} and {len(b)} results, sha256 {ha} and {hb}, errors {len(ea)} and {len(eb)}")
    for row in sorted(set(a) ^ set(b))[:20]:
        print('  only in ' + ('A' if row in a else 'B') + ': ' + ' '.join(row))
    sys.exit(0 if same else 1)
