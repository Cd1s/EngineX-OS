#!/usr/bin/env python3
"""Run the unittest tests of a directory in parallel processes, never splitting what must stay together.

  pyrun.py DIR [SPEC...] [-j N] [--defer FILE] [--json OUT] [--seed FILE] [--timings FILE] [--rerun] [--list]

DIR is discovered exactly like `python3 -m unittest discover -s DIR` (pattern test*.py, DIR on sys.path).
SPEC (file, module, class or test id) limits the run; a SPEC that matches no test fails the run. --defer FILE names
tests (one id, class, module or file per line, `#` comments) that are not run here.
Every process runs the tests of one module, in the serial order. A module whose tests share state across classes
(module_hazards: setUpModule, `global`, a change to a module-level object that the test does not undo) runs in one
process; any other module may be split into a few processes by whole classes (never a class). Modules that open
network sockets run one at a time. Weights: this host's last run (--timings), else --seed, else the median.
Every discovered test must report exactly one result, or the run fails. --rerun: a failed test runs once more alone
and a pass is reported as flaky; without it a failure stands. --json OUT: tools/dev/results.py's format.
"""
import argparse, ast, json, math, os, pathlib, re, shutil, subprocess, sys, tempfile, threading, time, traceback, unittest
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import results  # noqa: E402

_NET = re.compile(r'\bsocket\.socket\(|\bsocketserver\.|\b(?:Threading)?HTTPServer\(|\b(?:TCP|UDP)Server\(|\.bind\(\(|'
                  r'\bcreate_server\(|\bstart_server\(|["\']--listen["\']')
_MUTATORS = {'append', 'extend', 'update', 'add', 'insert', 'pop', 'popitem', 'clear', 'setdefault', 'remove', 'discard',
             'sort', 'reverse', '__setitem__', '__delitem__'}
_PROCESS_CALLS = {('os', 'chdir'), ('os', 'putenv'), ('os', 'unsetenv'), ('os', 'umask'), ('socket', 'setdefaulttimeout'),
                  ('importlib', 'reload'), ('locale', 'setlocale')}


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
            out.append((tid, t._testMethodName, t._testMethodName))
        else:
            out.append((tid, tid.split('.')[0], tid.rsplit('.', 1)[0]))
    return out


def _chain(node):
    """'a.b.c' for a Name/Attribute/Subscript chain, with its root name; (None, None) otherwise."""
    parts = []
    while isinstance(node, (ast.Attribute, ast.Subscript)):
        parts.append(node.attr if isinstance(node, ast.Attribute) else '[]')
        node = node.value
    if isinstance(node, ast.Name):
        return node.id, '.'.join([node.id] + list(reversed(parts)))
    return None, None


def _undo_targets(stmts):
    """The object chains a block of statements sets, deletes or calls a method on (what a finally or tearDown undoes)."""
    out = set()
    for st in stmts:
        for x in ast.walk(st):
            if isinstance(x, (ast.Assign, ast.AugAssign, ast.AnnAssign, ast.Delete)):
                for t in (x.targets if isinstance(x, (ast.Assign, ast.Delete)) else [x.target]):
                    out.add(_chain(t)[1])
            elif isinstance(x, ast.Call) and isinstance(x.func, ast.Attribute):
                out.add(_chain(x.func.value)[1])
            elif isinstance(x, ast.Call) and isinstance(x.func, ast.Name) and x.func.id in ('setattr', 'delattr') and x.args:
                out.add(_chain(x.args[0])[1])
    return out - {None}


def module_hazards(path):
    """Why a test module's classes may share state, so that splitting it across processes could change a result
    ([] when nothing is found). On the safe side: a hit keeps the module in one process, as a serial run has it."""
    try:
        tree = ast.parse(pathlib.Path(path).read_text())
    except (OSError, SyntaxError, ValueError):
        return ['not parsable']
    names, imported = set(), set()
    out = []
    for node in tree.body:
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            imported |= {(a.asname or a.name).split('.')[0] for a in node.names}
            names |= imported
        elif isinstance(node, (ast.Assign, ast.AnnAssign, ast.AugAssign)):
            for t in (node.targets if isinstance(node, ast.Assign) else [node.target]):
                names |= {n.id for n in ast.walk(t) if isinstance(n, ast.Name)}
        elif isinstance(node, (ast.FunctionDef, ast.ClassDef)):
            names.add(node.name)
            if node.name in ('setUpModule', 'tearDownModule'):
                out.append(f'{node.name} (line {node.lineno})')
    for n in ast.walk(tree):
        if isinstance(n, ast.Global):
            out.append(f'global {", ".join(n.names)} (line {n.lineno})')
    for cls in (n for n in tree.body if isinstance(n, ast.ClassDef)):
        teardown = set()
        for fn in cls.body:
            if isinstance(fn, ast.FunctionDef) and fn.name in ('tearDown', 'tearDownClass'):
                teardown |= _undo_targets(fn.body)
        for fn in (f for f in cls.body if isinstance(f, ast.FunctionDef) and f.name not in ('tearDown', 'tearDownClass')):
            cleanup = any(isinstance(x, ast.Attribute) and x.attr in ('addCleanup', 'addClassCleanup', 'enterContext', 'enterClassContext')
                          for x in ast.walk(fn))
            if cleanup:
                continue
            undone = set(teardown)
            for x in ast.walk(fn):
                if isinstance(x, ast.Try):
                    undone |= _undo_targets(x.finalbody)

            def hit(chain_text, line, what):
                if chain_text not in undone and not any(chain_text.startswith(u + '.') for u in undone):
                    out.append(f'{cls.name}.{fn.name} line {line}: {what} {chain_text}')

            for x in ast.walk(fn):
                if isinstance(x, (ast.Assign, ast.AugAssign, ast.AnnAssign, ast.Delete)):
                    for t in (x.targets if isinstance(x, (ast.Assign, ast.Delete)) else [x.target]):
                        root, text = _chain(t)
                        if root in names and text != root:
                            hit(text, x.lineno, 'changes')
                elif isinstance(x, ast.Call) and isinstance(x.func, ast.Attribute):
                    root, text = _chain(x.func.value)
                    # os.remove(path) is a file operation; os.environ.pop(k) and sys.path.insert(...) change state.
                    mutator = x.func.attr in _MUTATORS and (text != root or root not in imported)
                    if root in names and (mutator or (root, x.func.attr) in _PROCESS_CALLS):
                        hit(text + '.' + x.func.attr, x.lineno, 'calls')
                elif isinstance(x, ast.Call) and isinstance(x.func, ast.Name) and x.func.id in ('setattr', 'delattr') and x.args:
                    root, text = _chain(x.args[0])
                    if root in names:
                        hit(text, x.lineno, x.func.id)
    return out


def uses_network(path):
    try:
        return bool(_NET.search(pathlib.Path(path).read_text(errors='replace')))
    except OSError:
        return False


class Rec(unittest.TextTestResult):
    """Per-test status, seconds and reason, by test id."""
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
    loader = unittest.TestLoader()
    suite = unittest.TestSuite(loader.loadTestsFromName(i) for i in ids)
    res = unittest.TextTestRunner(stream=sys.stderr, verbosity=1, resultclass=Rec).run(suite)
    rows = [[tid, st, Rec.times.get(tid, 0.0), Rec.reason.get(tid, '')[-3000:]] for tid, st in Rec.status.items()]
    # Errors of class or module fixtures belong to no test id: report them so the run fails.
    extra = [[str(t), 'fail', 0.0, tb[-3000:]] for t, tb in res.errors if str(t.id() if hasattr(t, 'id') else t) not in Rec.status]
    pathlib.Path(out_file).write_text(json.dumps({'rows': rows + extra}))
    return 0 if res.wasSuccessful() else 1


def weights_for(tests, seed, prev):
    """Seconds per test id: this host's last run, else the seed, else the median of the known ones."""
    known = {t: prev[t] for t, *_ in tests if t in prev}
    known.update({t: seed[t] for t, *_ in tests if t not in known and t in seed})
    vals = sorted(known.values())
    median = vals[len(vals) // 2] if vals else 0.5
    return {t: max(0.01, known.get(t, median)) for t, *_ in tests}


def make_units(tests, w, j, keep_whole, net):
    """[(weight, module, [ids in serial order], is_net)]: one process each. A module in keep_whole, or that uses the
    network, is one unit; any other is split by whole classes into at most j parts, by its share of the total."""
    order = {t[0]: i for i, t in enumerate(tests)}
    by_mod = {}
    for tid, mod, key in tests:
        by_mod.setdefault(mod, {}).setdefault(key, []).append(tid)
    total = sum(w.values()) or 1.0
    share = total / max(1, j)
    units = []
    for mod, classes in by_mod.items():
        ids_w = {k: sum(w[t] for t in v) for k, v in classes.items()}
        mod_w = sum(ids_w.values())
        parts = 1 if (mod in keep_whole or mod in net) else max(1, min(len(classes), math.ceil(mod_w / share - 1e-9), j))
        chunks, load = [[] for _ in range(parts)], [0.0] * parts
        for key in sorted(classes, key=lambda k: (-ids_w[k], k)):
            i = min(range(parts), key=lambda k: (load[k], k)); chunks[i] += classes[key]; load[i] += ids_w[key]
        units += [(l, mod, sorted(c, key=order.get), mod in net) for c, l in zip(chunks, load) if c]
    return sorted(units, key=lambda u: (-u[0], u[1]))


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
    t0 = time.monotonic()
    res = results.Results()
    where = a.dir.rstrip('/')
    tests = discover(a.dir)
    if a.specs:
        specs = [norm(s) for s in a.specs]
        for s in specs:
            if not any(matches(t[0], [s]) or t[1] == s for t in tests):
                res.error(f'{where}: {s} matches no test')
        tests = [t for t in tests if matches(t[0], specs) or t[1] in specs]
    if a.defer:
        dspecs = [norm(l.split('#')[0]) for l in pathlib.Path(a.defer).read_text().splitlines() if l.split('#')[0].strip()]
        tests = [t for t in tests if not matches(t[0], dspecs)]
    if a.list:
        print('\n'.join(t[0] for t in tests)); return 0
    mods = sorted({t[1] for t in tests})
    keep_whole, net = {}, set()
    for m in mods:
        path = os.path.join(a.dir, m + '.py')
        h = module_hazards(path)
        if h:
            keep_whole[m] = h
            res.notes.append(f'{where}/{m}.py: one process ({len(h)} reasons, e.g. {"; ".join(h[:2])})')
        if uses_network(path):
            net.add(m)
            res.notes.append(f'{where}/{m}.py: opens network sockets; runs while no other such module runs')
    prev = json.loads(pathlib.Path(a.timings).read_text()) if a.timings and os.path.exists(a.timings) else {}
    seed_all = json.loads(pathlib.Path(a.seed).read_text()) if a.seed and os.path.exists(a.seed) else {}
    w = weights_for(tests, seed_all.get(where, {}), prev)
    units = make_units(tests, w, a.j, keep_whole, net)
    base = tempfile.mkdtemp(prefix='p', dir=os.environ.get('TMPDIR'))
    os.chmod(base, 0o755)  # tests that drop privileges must reach their own TMPDIR
    net_lock = threading.Lock()
    rows, logs = {}, {}

    def run(item):
        n, ids, is_net = item
        wd = os.path.join(base, str(n)); os.mkdir(wd); os.chmod(wd, 0o1777)
        pathlib.Path(wd, 'ids.json').write_text(json.dumps(ids))
        lock = net_lock if is_net else None
        if lock:
            lock.acquire()
        try:
            with open(wd + '/log', 'w') as log:
                p = subprocess.run([sys.executable, __file__, a.dir, '--worker', wd + '/ids.json', wd + '/out.json'],
                                   env=dict(os.environ, TMPDIR=wd), stdout=log, stderr=subprocess.STDOUT)
        finally:
            if lock:
                lock.release()
        out = json.loads(pathlib.Path(wd, 'out.json').read_text()) if os.path.exists(wd + '/out.json') else {'rows': []}
        return ids, p.returncode, out['rows'], pathlib.Path(wd, 'log').read_text(errors='replace')[-6000:]

    flaky = []
    try:
        with ThreadPoolExecutor(max_workers=max(1, a.j)) as pool:
            done = list(pool.map(run, [(i, u[2], u[3]) for i, u in enumerate(units)]))
        for ids, rc, got, log in done:
            for tid, st, secs, reason in got:
                rows[tid] = [st, secs, reason]
            for tid in ids:
                logs[tid] = log
            if rc and not any(r[0] == 'fail' for t, r in rows.items() if t in ids) and not [t for t in ids if t not in rows]:
                res.error(f'{where}: a worker exited {rc} with no failed test: {log[-2000:]}')
        failed = [t for t, r in rows.items() if r[0] == 'fail' and t in w]
        if a.rerun and 0 < len(failed) <= 10:  # more means the host or the tree is broken
            for i, tid in enumerate(failed):
                ids, rc, got, log = run((1000 + i, [tid], any(t[1] in net for t in tests if t[0] == tid)))
                again = {g[0]: g for g in got}
                if rc == 0 and again.get(tid, [None, 'fail'])[1] == 'pass':
                    flaky.append((tid, rows[tid][2] or logs.get(tid, '')))
                    rows[tid] = ['pass', again[tid][2], '']
    finally:
        shutil.rmtree(base, ignore_errors=True)  # only the directory created above
    for tid, mod, key in tests:
        if tid not in rows:
            res.error(f'{tid}: reported no result')
            continue
        st, secs, reason = rows[tid]
        res.add('py', where, tid, st, secs, reason)
    for tid in sorted(set(rows) - {t[0] for t in tests}):  # fixture errors (setUpClass, setUpModule) and the like
        st, secs, reason = rows[tid]
        if st == 'fail':
            res.error(f'{tid}: {reason[-2000:]}')
    for tid, first in flaky:
        res.flaky_one('py', where, tid, first[-1500:])
    res.wall = time.monotonic() - t0
    res.extra['units'] = [{'module': u[1], 'tests': len(u[2]), 'planned': round(u[0], 1), 'net': u[3]} for u in units]
    if a.timings:
        prev.update({t: round(r[1], 3) for t, r in rows.items()})
        pathlib.Path(a.timings).write_text(json.dumps(prev, indent=0, sort_keys=True))
    if a.json:
        res.write(a.json)
    print(res.summary(f'pyrun {where} ({len(units)} processes on {a.j} workers)'))
    for n in res.notes:
        print('pyrun: ' + n)
    for r in res.failed():
        print(f"===== FAILED {r['test']} =====\n{r['reason'][-3000:]}", file=sys.stderr)
    for e in res.errors:
        print('pyrun: ERROR ' + e[:3000], file=sys.stderr)
    return 0 if res.ok() else 1


if __name__ == '__main__':
    sys.exit(main())
