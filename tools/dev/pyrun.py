#!/usr/bin/env python3
"""Run the unittest classes of a directory in parallel processes, one class per process.

  pyrun.py DIR [SPEC...] [-j N] [--defer FILE] [--json OUT] [--timings FILE] [--list]

DIR is discovered exactly like `python3 -m unittest discover -s DIR` (pattern test*.py, DIR on sys.path).
SPEC (file, module, class or test id) limits the run; --defer FILE names tests (one id, class, module or
file per line, `#` comments) that are not run here and reported as deferred (the ARM tier runs them).
Every worker gets its own TMPDIR. Exit status: 0 all passed, 1 otherwise (like unittest).
"""
import argparse, json, os, pathlib, shutil, subprocess, sys, tempfile, time, unittest
from concurrent.futures import ThreadPoolExecutor


def flatten(suite):
    for t in suite:
        yield from (flatten(t) if isinstance(t, unittest.TestSuite) else [t])


def norm(spec):
    spec = spec.strip()
    return spec[:-3] if spec.endswith('.py') else spec.replace('/', '.')


def matches(tid, specs):
    return any(tid == s or tid.startswith(s + '.') for s in specs)


def discover(start):
    sys.path.insert(0, os.path.abspath(start))
    out = []  # (test id, class key)
    for t in flatten(unittest.TestLoader().discover(start)):
        tid = t.id()
        if type(t).__name__ == '_FailedTest':  # import error: rerun it by module name in a worker
            out.append((tid, t._testMethodName))
        else:
            out.append((tid, tid.rsplit('.', 1)[0]))
    return out


class Rec(unittest.TextTestResult):
    def startTest(self, test):
        self._t0 = time.monotonic()
        super().startTest(test)

    def stopTest(self, test):
        self.times[test.id()] = time.monotonic() - self._t0
        super().stopTest(test)

    def addSkip(self, test, reason):
        self.skips.append([test.id(), reason])
        super().addSkip(test, reason)

    def addFailure(self, test, err):
        self.fails.append(test.id())
        super().addFailure(test, err)

    def addError(self, test, err):
        self.fails.append(test.id())
        super().addError(test, err)


def worker(start, ids_file, out_file):
    sys.path.insert(0, os.path.abspath(start))
    ids = json.loads(pathlib.Path(ids_file).read_text())
    loader = unittest.TestLoader()
    suite = unittest.TestSuite(loader.loadTestsFromName(i) for i in ids)
    Rec.times, Rec.skips, Rec.fails = {}, [], []
    res = unittest.TextTestRunner(stream=sys.stderr, verbosity=1, resultclass=Rec).run(suite)
    pathlib.Path(out_file).write_text(json.dumps({'run': res.testsRun, 'failed': len(res.failures) + len(res.errors) + len(res.unexpectedSuccesses),
                                                   'skipped': Rec.skips, 'times': Rec.times, 'failed_ids': Rec.fails}))
    return 0 if res.wasSuccessful() else 1


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('dir'); ap.add_argument('specs', nargs='*')
    ap.add_argument('-j', type=int, default=min(os.cpu_count() or 4, 12))
    ap.add_argument('--defer'); ap.add_argument('--json'); ap.add_argument('--timings'); ap.add_argument('--list', action='store_true')
    ap.add_argument('--worker', nargs=2, metavar=('IDS', 'OUT'))
    ap.add_argument('--rerun', action='store_true', help='run the failed tests alone once more; a pass is reported as flaky, not failed')
    a = ap.parse_args()
    if a.worker:
        return worker(a.dir, *a.worker)
    tests = discover(a.dir)
    total = len(tests)
    if a.specs:
        specs = [norm(s) for s in a.specs]
        tests = [t for t in tests if matches(t[0], specs)]
    deferred = []
    if a.defer:
        dspecs = [norm(l.split('#')[0]) for l in pathlib.Path(a.defer).read_text().splitlines() if l.split('#')[0].strip()]
        deferred = [t[0] for t in tests if matches(t[0], dspecs)]
        tests = [t for t in tests if t[0] not in set(deferred)]
    groups = {}
    for tid, key in tests:
        groups.setdefault(key, []).append(tid)
    if a.list:
        print('\n'.join(t[0] for t in tests)); return 0
    prev = json.loads(pathlib.Path(a.timings).read_text()) if a.timings and os.path.exists(a.timings) else {}
    order = sorted(groups, key=lambda k: (-prev.get(k, 0), -len(groups[k])))  # longest first
    base = tempfile.mkdtemp(prefix='pyrun-', dir=os.environ.get('TMPDIR'))
    os.chmod(base, 0o755)  # tests that drop privileges must reach their own TMPDIR
    t0 = time.monotonic()

    def run(item, ids=None):
        n, key = item
        wd = os.path.join(base, str(n)); os.mkdir(wd); os.chmod(wd, 0o1777)
        pathlib.Path(wd, 'ids.json').write_text(json.dumps(ids or groups[key]))
        s = time.monotonic()
        with open(wd + '/log', 'w') as log:
            p = subprocess.run([sys.executable, __file__, a.dir, '--worker', wd + '/ids.json', wd + '/out.json'],
                               env=dict(os.environ, TMPDIR=wd), stdout=log, stderr=subprocess.STDOUT)
        res = json.loads(pathlib.Path(wd + '/out.json').read_text()) if os.path.exists(wd + '/out.json') else {'run': 0, 'failed': 1, 'skipped': [], 'times': {}}
        res.update(key=key, rc=p.returncode, secs=time.monotonic() - s, log=pathlib.Path(wd, 'log').read_text(errors='replace'))
        return res

    try:
        with ThreadPoolExecutor(max_workers=max(1, a.j)) as pool:
            results = list(pool.map(run, enumerate(order)))
        flaky = []
        if a.rerun and sum(len(r.get('failed_ids', [])) for r in results) <= 10:  # more means the host or the tree is broken
            for n, r in enumerate(x for x in results if x['rc'] != 0 and x.get('failed_ids') and x['failed'] == len(x['failed_ids'])):
                again = run((1000 + n, r['key']), r['failed_ids'])
                if again['rc'] == 0 and not again['failed']:
                    flaky += [{'id': i, 'first_failure': r['log'][:600]} for i in r['failed_ids']]
                    r.update(rc=0, failed=0)
    finally:
        shutil.rmtree(base, ignore_errors=True)  # only the directory created above
    ran = sum(r['run'] for r in results)
    skipped = [s for r in results for s in r['skipped']]
    bad = [r for r in results if r['rc'] != 0 or r['failed']]
    for r in bad:
        print(f"===== FAILED class {r['key']} (exit {r['rc']}) =====\n{r['log']}", file=sys.stderr)
    expected = len(tests)
    ok = not bad and ran == expected
    if ran != expected:
        print(f'pyrun: ran {ran} tests, discovery expected {expected}', file=sys.stderr)
    wall = time.monotonic() - t0
    print(f'pyrun {a.dir}: {ran} run, {len(skipped)} skipped, {len(deferred)} deferred, {len(bad)} failed classes, '
          f'{len(results)} classes on {a.j} workers, {len(flaky)} flaky (passed on rerun), {wall:.1f}s -> {"OK" if ok else "FAILED"}')
    for f in flaky:
        print(f"pyrun: flaky (failed in the parallel run, passed alone): {f['id']}")
    if a.timings:
        prev.update({r['key']: round(r['secs'], 2) for r in results})
        pathlib.Path(a.timings).write_text(json.dumps(prev, indent=0, sort_keys=True))
    if a.json:
        pathlib.Path(a.json).write_text(json.dumps({'ok': ok, 'discovered': total, 'run': ran, 'wall': wall, 'skipped': skipped, 'deferred': deferred,
                   'failed': [r['key'] for r in bad], 'flaky': flaky,
                   'classes': {r['key']: {'tests': len(groups[r['key']]), 'secs': round(r['secs'], 2)} for r in results},
                   'tests': {k: round(v, 3) for r in results for k, v in r['times'].items()}}, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
