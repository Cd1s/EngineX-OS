#!/usr/bin/env python3
"""Run the unittest tests of a directory in a few parallel processes (shards), with nothing left out.

  pyrun.py DIR [SPEC...] [-j N] [--defer FILE] [--json OUT] [--seed FILE] [--timings FILE] [--list]

DIR is discovered exactly like `python3 -m unittest discover -s DIR` (pattern test*.py, DIR on sys.path).
SPEC (file, module, class or test id) limits the run; --defer FILE names tests (one id, class, module or
file per line, `#` comments) that are not run here and reported as deferred.
Shards: N worker processes, each importing the test modules once. Whole classes go to the least-loaded shard,
longest first (weights: --timings from earlier runs, else --seed per-test seconds, else the median); a class
heavier than an even share is split by test. Every discovered test runs in exactly one shard, or the run fails.
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


def weights_for(tests, seed, prev):
    """Seconds per test id: this host's last run, else the seed, else the median of the known ones."""
    known = {t: prev[t] for t, _ in tests if t in prev}
    known.update({t: seed[t] for t, _ in tests if t not in known and t in seed})
    vals = sorted(known.values())
    median = vals[len(vals) // 2] if vals else 0.5
    return {t: max(0.01, known.get(t, median)) for t, _ in tests}


def make_shards(tests, w, n):
    """[[test id, ...], ...]: classes kept whole on the least-loaded shard, longest first; a class heavier than an
    even share is split by test (in its own order). Deterministic for the same input."""
    groups = {}
    for tid, key in tests:
        groups.setdefault(key, []).append(tid)
    n = max(1, min(n, len(tests)))
    share = sum(w.values()) / n
    units = []
    for key, ids in groups.items():
        total = sum(w[t] for t in ids)
        if total > share and len(ids) > 1:
            parts = min(len(ids), int(total // share) + 1)
            chunks, load = [[] for _ in range(parts)], [0.0] * parts
            for t in sorted(ids, key=lambda t: -w[t]):
                i = min(range(parts), key=lambda k: (load[k], k)); chunks[i].append(t); load[i] += w[t]
            order = {t: i for i, t in enumerate(ids)}
            units += [(sum(w[t] for t in c), key, sorted(c, key=order.get)) for c in chunks if c]
        else:
            units.append((total, key, ids))
    shards, load = [[] for _ in range(n)], [0.0] * n
    for weight, key, ids in sorted(units, key=lambda u: (-u[0], u[1])):
        i = min(range(n), key=lambda k: (load[k], k)); shards[i] += ids; load[i] += weight
    return [s for s in shards if s], load


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('dir'); ap.add_argument('specs', nargs='*')
    ap.add_argument('-j', type=int, default=min(os.cpu_count() or 4, 12))
    ap.add_argument('--defer'); ap.add_argument('--json'); ap.add_argument('--timings'); ap.add_argument('--list', action='store_true')
    ap.add_argument('--seed', help='per-test seconds of a reference host (tools/dev/py-slow.json: {dir: {test id: secs}})')
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
    if a.list:
        print('\n'.join(t[0] for t in tests)); return 0
    prev = json.loads(pathlib.Path(a.timings).read_text()) if a.timings and os.path.exists(a.timings) else {}
    seed_all = json.loads(pathlib.Path(a.seed).read_text()) if a.seed and os.path.exists(a.seed) else {}
    seed = seed_all.get(a.dir.rstrip('/'), {})
    w = weights_for(tests, seed, prev)
    shards, load = make_shards(tests, w, a.j)
    base = tempfile.mkdtemp(prefix='p', dir=os.environ.get('TMPDIR'))
    os.chmod(base, 0o755)  # tests that drop privileges must reach their own TMPDIR
    t0 = time.monotonic()

    def run(item):
        n, ids = item
        wd = os.path.join(base, str(n)); os.mkdir(wd); os.chmod(wd, 0o1777)
        pathlib.Path(wd, 'ids.json').write_text(json.dumps(ids))
        s = time.monotonic()
        with open(wd + '/log', 'w') as log:
            p = subprocess.run([sys.executable, __file__, a.dir, '--worker', wd + '/ids.json', wd + '/out.json'],
                               env=dict(os.environ, TMPDIR=wd), stdout=log, stderr=subprocess.STDOUT)
        res = json.loads(pathlib.Path(wd + '/out.json').read_text()) if os.path.exists(wd + '/out.json') else {
            'run': 0, 'failed': 1, 'skipped': [], 'times': {}, 'failed_ids': []}
        res.update(key=f'shard {n}', ids=ids, rc=p.returncode, secs=time.monotonic() - s, log=pathlib.Path(wd, 'log').read_text(errors='replace'))
        return res

    results, flaky = [], []
    try:
        with ThreadPoolExecutor(max_workers=max(1, len(shards))) as pool:
            results = list(pool.map(run, enumerate(shards)))
        if a.rerun and sum(len(r.get('failed_ids', [])) for r in results) <= 10:  # more means the host or the tree is broken
            for n, r in enumerate(x for x in results if x['rc'] != 0 and x.get('failed_ids') and x['failed'] == len(x['failed_ids'])):
                again = run((1000 + n, r['failed_ids']))
                if again['rc'] == 0 and not again['failed']:
                    flaky += [{'id': i, 'first_failure': r['log'][-1500:]} for i in r['failed_ids']]
                    r.update(rc=0, failed=0)
    finally:
        shutil.rmtree(base, ignore_errors=True)  # only the directory created above
    ran = sum(r['run'] for r in results)
    skipped = [s for r in results for s in r['skipped']]
    bad = [r for r in results if r['rc'] != 0 or r['failed']]
    failed_ids = sorted({i for r in bad for i in r.get('failed_ids', [])})
    for r in bad:
        print(f"===== FAILED {r['key']} (exit {r['rc']}): {', '.join(r.get('failed_ids', [])[:20])} =====\n{r['log'][-20000:]}", file=sys.stderr)
    expected = len(tests)
    assigned = sorted(i for sh in shards for i in sh)
    ok = not bad and ran == expected and assigned == sorted(t for t, _ in tests)
    if ran != expected:
        print(f'pyrun: ran {ran} tests, discovery expected {expected}', file=sys.stderr)
    wall = time.monotonic() - t0
    times = {k: v for r in results for k, v in r['times'].items()}
    shard_secs = ', '.join('%.0fs' % r['secs'] for r in results)
    print(f'pyrun {a.dir}: {ran} run, {len(skipped)} skipped, {len(deferred)} deferred, {len(failed_ids) or len(bad)} failed, '
          f'{len(shards)} shards ({shard_secs}), {len(flaky)} flaky (passed on rerun), '
          f'{wall:.1f}s -> {"OK" if ok else "FAILED"}')
    for f in flaky:
        print(f"pyrun: flaky (failed in the parallel run, passed alone): {f['id']}")
    if a.timings:
        prev.update({k: round(v, 3) for k, v in times.items()})
        pathlib.Path(a.timings).write_text(json.dumps(prev, indent=0, sort_keys=True))
    if a.json:
        pathlib.Path(a.json).write_text(json.dumps({'ok': ok, 'discovered': total, 'run': ran, 'wall': wall, 'skipped': skipped, 'deferred': deferred,
                   'failed': failed_ids or [r['key'] for r in bad], 'flaky': flaky,
                   'shards': [{'tests': len(r['ids']), 'secs': round(r['secs'], 2), 'planned': round(l, 1)} for r, l in zip(results, load)],
                   'tests': {k: round(v, 3) for k, v in times.items()}}, indent=1))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
