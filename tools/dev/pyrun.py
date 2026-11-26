#!/usr/bin/env python3
"""Run the unittest tests of a directory; split only the modules tools/dev/split.json names.

  pyrun.py DIR [SPEC...] [--split MODULE=N]... [--seed FILE] [--defer FILE] [--json OUT] [--list]

DIR is discovered exactly like `python3 -m unittest discover -s DIR` (pattern test*.py, DIR on sys.path). SPEC (file,
module, class or test id) limits the run; a SPEC that matches no test is reported as not run (exit 2). --defer FILE
names tests (one id, class, module or file per line, `#` comments) that do not run here.
Every module runs in the serial order, all of them in one process, as `unittest discover` runs them, except a module
named by --split MODULE=N (devtest.py passes the ones split.json lists): its whole classes are spread over N processes
that run at the same time, after the serial process (balanced by --seed, seconds per class). Every selected test must
report exactly one result, or the run fails. --json OUT: tools/dev/results.py's format.
"""
import argparse, json, os, pathlib, shutil, signal, subprocess, sys, tempfile, time, unittest
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import results  # noqa: E402


def flatten(suite):
    for t in suite:
        yield from (flatten(t) if isinstance(t, unittest.TestSuite) else [t])


def norm(spec):
    spec = spec.strip()
    return spec[:-3] if spec.endswith('.py') else spec.replace('/', '.')


def matches(tid, specs):
    return any(tid == s or tid.startswith(s + '.') for s in specs)


def discover(start):
    """[(test id, module, class key)] in the serial order of `unittest discover`."""
    sys.path.insert(0, os.path.abspath(start))
    out = []
    for t in flatten(unittest.TestLoader().discover(start)):
        tid = t.id()
        if type(t).__name__ == '_FailedTest':  # an import error: run (and fail) it by module name in a worker
            out.append((t._testMethodName, t._testMethodName, t._testMethodName))
        else:
            out.append((tid, tid.split('.')[0], tid.rsplit('.', 1)[0]))
    return out


class Rec(unittest.TextTestResult):
    """Status, seconds and reason per test id."""
    def startTest(self, test):
        self._t0 = time.monotonic()
        super().startTest(test)

    def stopTest(self, test):
        self.times[test.id()] = time.monotonic() - self._t0
        super().stopTest(test)

    def _set(self, test, status, reason=''):
        if self.status.get(test.id()) != 'fail':  # a failed subtest fails its test for good
            self.status[test.id()] = status
        if reason:
            self.reason[test.id()] = reason

    def addSuccess(self, test):
        self._set(test, 'pass'); super().addSuccess(test)

    def addFailure(self, test, err):
        self._set(test, 'fail', self._exc_info_to_string(err, test)); super().addFailure(test, err)

    def addError(self, test, err):
        self._set(test, 'fail', self._exc_info_to_string(err, test)); super().addError(test, err)

    def addSkip(self, test, reason):
        self._set(test, 'skip', reason); super().addSkip(test, reason)

    def addExpectedFailure(self, test, err):
        self._set(test, 'pass'); super().addExpectedFailure(test, err)

    def addUnexpectedSuccess(self, test):
        self._set(test, 'fail', 'unexpected success'); super().addUnexpectedSuccess(test)

    def addSubTest(self, test, subtest, err):
        if err is not None:
            self.status[test.id()] = 'fail'
            self.reason[test.id()] = self._exc_info_to_string(err, test)
        super().addSubTest(test, subtest, err)


def worker(start, ids_file, out_file):
    sys.path.insert(0, os.path.abspath(start))
    ids = json.loads(pathlib.Path(ids_file).read_text())
    Rec.times, Rec.status, Rec.reason = {}, {}, {}
    suite = unittest.TestSuite(unittest.TestLoader().loadTestsFromName(i) for i in ids)
    res = unittest.TextTestRunner(stream=sys.stderr, verbosity=1, resultclass=Rec).run(suite)
    rows = [[tid, st, Rec.times.get(tid, 0.0), Rec.reason.get(tid, '')[-3000:]] for tid, st in Rec.status.items()]
    # Errors of class or module fixtures (setUpClass, setUpModule) belong to no test id: report them so the run fails.
    rows += [[str(t), 'fail', 0.0, tb[-3000:]] for t, tb in res.errors if getattr(t, 'id', lambda: str(t))() not in Rec.status]
    pathlib.Path(out_file).write_text(json.dumps({'rows': rows}))
    return 0


def split_units(tests, n, weights):
    """Whole classes of one module over n processes, heaviest first; each process keeps the serial order."""
    order = {t[0]: i for i, t in enumerate(tests)}
    classes = {}
    for tid, _, key in tests:
        classes.setdefault(key, []).append(tid)
    known = sorted(weights.values())
    median = known[len(known) // 2] if known else 1.0
    load, parts = [0.0] * n, [[] for _ in range(n)]
    for key in sorted(classes, key=lambda k: (-weights.get(k, median), k)):
        i = min(range(n), key=lambda k: (load[k], k))
        parts[i] += classes[key]; load[i] += weights.get(key, median)
    return [sorted(p, key=order.get) for p in parts if p]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('dir'); ap.add_argument('specs', nargs='*')
    ap.add_argument('--split', action='append', default=[], metavar='MODULE=N')
    ap.add_argument('--seed', help='seconds per test class (tools/dev/py-slow.json: {dir: {module.Class: secs}})')
    ap.add_argument('--defer'); ap.add_argument('--json'); ap.add_argument('--list', action='store_true')
    ap.add_argument('--worker', nargs=2, metavar=('IDS', 'OUT'))
    a = ap.parse_args()
    if a.worker:
        return worker(a.dir, *a.worker)
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    t0 = time.monotonic()
    res = results.Results()
    where = a.dir.rstrip('/')
    tests = discover(a.dir)
    if a.specs:
        specs = [norm(s) for s in a.specs]
        for s in specs:
            if not any(matches(t[0], [s]) or t[1] == s for t in tests):
                res.add('py', where, s, 'notrun', 0, 'no test matches it')
        tests = [t for t in tests if matches(t[0], specs) or t[1] in specs]
    if a.defer:
        dspecs = [norm(l.split('#')[0]) for l in pathlib.Path(a.defer).read_text().splitlines() if l.split('#')[0].strip()]
        tests = [t for t in tests if not matches(t[0], dspecs)]
    if a.list:
        print('\n'.join(t[0] for t in tests)); return 0
    split = {m: int(n) for m, n in (s.split('=', 1) for s in a.split)}
    seed = json.loads(pathlib.Path(a.seed).read_text()).get(where, {}) if a.seed and os.path.exists(a.seed) else {}
    serial = [t[0] for t in tests if t[1] not in split]
    parallel = [u for m, n in sorted(split.items()) for u in split_units([t for t in tests if t[1] == m], max(1, n), seed)]
    base = os.path.join(os.environ.get('TMPDIR') or tempfile.gettempdir(), f'p{os.getpid()}')  # named before it exists
    rows = {}

    def run(item):
        n, ids = item
        wd = os.path.join(base, str(n)); os.mkdir(wd); os.chmod(wd, 0o1777)
        pathlib.Path(wd, 'ids.json').write_text(json.dumps(ids))
        with open(wd + '/log', 'w') as log:
            p = subprocess.run([sys.executable, __file__, a.dir, '--worker', wd + '/ids.json', wd + '/out.json'],
                               env=dict(os.environ, TMPDIR=wd), stdout=log, stderr=subprocess.STDOUT)
        got = json.loads(pathlib.Path(wd, 'out.json').read_text())['rows'] if os.path.exists(wd + '/out.json') else []
        for tid, st, secs, reason in got:
            rows[tid] = (st, secs, reason)
        if not got and ids:
            res.error(f'{where}: a worker exited {p.returncode} without results: '
                      f'{pathlib.Path(wd, "log").read_text(errors="replace")[-2000:]}')

    try:
        os.makedirs(base); os.chmod(base, 0o755)  # tests that drop privileges must reach their own TMPDIR
        if serial:
            run((0, serial))  # alone: everything not listed in split.json, as unittest discover runs it
        if parallel:
            with ThreadPoolExecutor(max_workers=len(parallel)) as pool:
                list(pool.map(run, [(i + 1, u) for i, u in enumerate(parallel)]))
    finally:
        shutil.rmtree(base, ignore_errors=True)
    for tid, _, _ in tests:
        if tid in rows:
            st, secs, reason = rows.pop(tid)
            res.add('py', where, tid, st, secs, reason)
        else:
            res.error(f'{tid}: reported no result')
    for tid, (st, secs, reason) in sorted(rows.items()):  # fixture errors (setUpClass, setUpModule)
        if st == 'fail':
            res.error(f'{tid}: {reason[-2000:]}')
    res.wall = time.monotonic() - t0
    res.extra['processes'] = (1 if serial else 0) + len(parallel)
    if a.json:
        res.write(a.json)
    print(res.summary(f'pyrun {where} ({len(serial)} tests in one process, {len(parallel)} processes for {", ".join(sorted(split)) or "-"})'))
    for r in res.failed():
        print(f"===== FAILED {r['test']} =====\n{r['reason'][-3000:]}", file=sys.stderr)
    for e in res.errors:
        print('pyrun: ERROR ' + e[:3000], file=sys.stderr)
    if not res.ok():
        return 1
    return 2 if any(r['status'] == 'notrun' for r in res.items) else 0


if __name__ == '__main__':
    sys.exit(main())
