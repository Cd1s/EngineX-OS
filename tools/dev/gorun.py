#!/usr/bin/env python3
"""Run Go tests with per-test results, splitting a seeded package across processes only when that is safe.

  gorun.py [--seed FILE] [--defer FILE | --only FILE] [-p N] [--jobs N] [--no-cache] [--no-race] [--timeout T]
           [--rerun] [--json OUT] PKG...

Packages listed in the seed file (tools/dev/go-slow.json: {"pkg/dir": {"shards": N, "tests": {Name: secs}}}) are split
into N `go test -run '^(a|b)$'` processes, longest first, but only when gotree.split_hazards finds nothing that makes a
split differ from one process (a fixed port, a package variable a test changes without restoring it); otherwise the
package runs whole. --defer / --only take lines `pkg-dir Test` (`*` for the whole package); deferred tests are not run
here. A package whose tests must be listed (`go test -list`) and cannot be (a build error) fails the run; the tests a
listed package runs must all report a result, or the run fails. With --rerun a failed test runs once more alone and a
pass is reported as flaky (never a data race, never more than ten failures); without it a failure stands.
--json OUT: tools/dev/results.py's format.
"""
import argparse, collections, fcntl, json, os, pathlib, re, shutil, subprocess, sys, tempfile, threading, time
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import gotree  # noqa: E402
import results  # noqa: E402

NAME = re.compile(r'^(Test|Example|Fuzz)\w*$')


def stable_dir(base, name):
    """A private scratch directory with the same path on every run: Go's test cache keys on the TMPDIR value, so a random
    directory would make every test that uses t.TempDir() miss the cache. Another process holding it gets a random one."""
    path = os.path.join(base, name)
    lock = open(path + '.lock', 'w')
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        lock.close()
        path = tempfile.mkdtemp(prefix=name + '-', dir=base); os.chmod(path, 0o1777)
        return path, None
    shutil.rmtree(path, ignore_errors=True)
    os.mkdir(path); os.chmod(path, 0o1777)
    return path, lock


def release_dir(path, lock):
    shutil.rmtree(path, ignore_errors=True)  # the directory stable_dir() made
    if lock:
        try:
            os.remove(path + '.lock')
        except OSError:
            pass
        lock.close()


def sh(cmd, **kw):
    return subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, **kw)


def read_pairs(path):
    out = collections.defaultdict(set)
    for line in (pathlib.Path(path).read_text().splitlines() if path else []):
        line = line.split('#')[0].split()
        if len(line) == 2:
            out[line[0].removeprefix('./')].add(line[1])
    return out


def buckets(names, weights, n):
    n = max(1, min(n, len(names)))
    load, out = [0.0] * n, [[] for _ in range(n)]
    for name in sorted(names, key=lambda x: (-weights.get(x, 0.5), x)):
        i = min(range(n), key=lambda k: (load[k], k))
        out[i].append(name); load[i] += weights.get(name, 0.5)
    return [sorted(b) for b in out if b]


def run_job(job, state, lock):
    """One `go test -json` process; fold its events into state."""
    t_start = time.monotonic()
    p = subprocess.Popen(job['cmd'], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=job['env'], bufsize=1)
    errs = []
    t = threading.Thread(target=lambda: errs.extend(p.stderr), daemon=True); t.start()
    for line in p.stdout:
        try:
            ev = json.loads(line)
        except ValueError:
            errs.append(line); continue
        act, pkg, test = ev.get('Action'), ev.get('Package'), ev.get('Test')
        with lock:
            if act == 'output':
                state['out'][(pkg, test)].append(ev['Output'])
                if test is None and '(cached)' in ev['Output']:
                    state['cached'].add(pkg)
            elif act in ('pass', 'fail', 'skip'):
                if test:
                    state['tests'][(pkg, test)] = (act, ev.get('Elapsed', 0.0))
                    if act == 'pass':
                        state['out'].pop((pkg, test), None)
                else:
                    state['pkgs'][pkg] = (act, ev.get('Elapsed', 0.0))
    t.join(); p.wait()
    with lock:
        state['rc'][job['label']] = p.returncode
        state['job_secs'][job['label']] = round(time.monotonic() - t_start, 1)
        state['err'][job['label']] = ''.join(errs)


def new_state():
    return {'out': collections.defaultdict(list), 'tests': {}, 'pkgs': {}, 'cached': set(), 'rc': {}, 'err': {}, 'job_secs': {}}


def rerun_failed(state, base, tmp):
    """Run the top-level tests that failed once more, one package at a time. A data race is a real bug whatever a
    second run says, so a package with one is never rerun, and more than ten failures mean a broken host."""
    failed = [k for k, v in state['tests'].items() if v[0] == 'fail']
    tops = collections.defaultdict(set)
    for pkg, test in failed:
        tops[pkg].add(test.split('/')[0])
    if not tops or sum(map(len, tops.values())) > 10:
        return []
    flaky = []
    for pkg, names in tops.items():
        if any('DATA RACE' in ''.join(state['out'].get((pkg, t), [])) for t in {t for p, t in failed if p == pkg}):
            continue
        wd = tempfile.mkdtemp(dir=tmp); os.chmod(wd, 0o1777)
        again = new_state()
        cmd = [c for c in base if c != '-count=1'] + ['-count=1', '-run', '^(' + '|'.join(sorted(names)) + ')$', pkg]
        run_job({'label': 'rerun', 'cmd': cmd, 'env': dict(os.environ, TMPDIR=wd)}, again, threading.Lock())
        if again['rc'].get('rerun') == 0 and not any(v[0] == 'fail' for v in again['tests'].values()):
            for t in sorted(names):
                flaky.append((pkg, t, ''.join(state['out'].get((pkg, t), []))[-1500:]))
            for (p, t), v in list(state['tests'].items()):
                if p == pkg and t.split('/')[0] in names and v[0] == 'fail':
                    state['tests'][(p, t)] = ('pass', v[1])
            if not any(v[0] == 'fail' for (p, _), v in state['tests'].items() if p == pkg):
                state['pkgs'][pkg] = ('pass', state['pkgs'].get(pkg, ('', 0.0))[1])
    return flaky


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('pkgs', nargs='+'); ap.add_argument('--seed'); ap.add_argument('--defer'); ap.add_argument('--only')
    ap.add_argument('-p', type=int, default=6, help='go test -p of the packages that run whole')
    ap.add_argument('--jobs', type=int, default=8, help='go test processes at once')
    ap.add_argument('--no-cache', action='store_true'); ap.add_argument('--no-race', action='store_true')
    ap.add_argument('--timeout', default=os.environ.get('ENGINEX_GO_TIMEOUT', '30m'), help='go test -timeout of every invocation')
    ap.add_argument('--no-prebuild', action='store_true', help='do not compile all test binaries once before the tests start')
    ap.add_argument('--rerun', action='store_true', help='run failed tests alone once more; a pass is reported as flaky, not failed')
    ap.add_argument('--json')
    a = ap.parse_args()
    t0 = time.monotonic()
    res = results.Results()
    mod = sh(['go', 'list', '-m']).stdout.strip()
    pkgs = sh(['go', 'list', '-e', '-f', '{{.ImportPath}}', *a.pkgs])
    if pkgs.returncode or not pkgs.stdout.strip():
        res.error(f'go list {" ".join(a.pkgs)} failed: {(pkgs.stderr or pkgs.stdout).strip()[-2000:]}')
        return finish(a, res, t0)
    dirs = {p: p.removeprefix(mod).lstrip('/') or '.' for p in pkgs.stdout.split()}
    seed = {k: v for k, v in json.loads(pathlib.Path(a.seed).read_text()).items() if not k.startswith('_')} if a.seed else {}
    defer, only = read_pairs(a.defer), read_pairs(a.only)
    base = ['go', 'test', '-vet=off', '-json', '-timeout', a.timeout] + ([] if a.no_race else ['-race']) + (['-count=1'] if a.no_cache else [])
    tmp, tmp_lock = None, None
    try:
        tmp, tmp_lock = stable_dir(os.environ.get('TMPDIR') or '/tmp', 'g')  # short: tests bind unix sockets under TMPDIR
        jobs, expected, plain, weight = [], {}, [], {}

        def job(label, cmd):
            wd = os.path.join(tmp, str(len(jobs))); os.mkdir(wd); os.chmod(wd, 0o1777)  # same path per label each run
            jobs.append({'label': label, 'cmd': cmd, 'env': dict(os.environ, TMPDIR=wd)})

        def listing(imp):
            out = sh(['go', 'test', '-vet=off', '-timeout', a.timeout, '-list', '.*', imp] + ([] if a.no_race else ['-race']))
            names = [x for x in out.stdout.split() if NAME.match(x)]
            return out.returncode, names, (out.stderr + out.stdout)[-3000:]

        special = {imp: d for imp, d in dirs.items() if (seed.get(d, {}).get('shards', 1) > 1) or d in defer or d in only}
        listed = {}
        if special:
            with ThreadPoolExecutor(max_workers=min(len(special), 8)) as pool:
                for imp, got in zip(special, pool.map(listing, special)):
                    listed[imp] = got
        for imp, d in dirs.items():
            if only and d not in only:
                continue
            if imp not in special:
                plain.append(imp); continue
            rc, names, err = listed[imp]
            if rc:  # a build error: never "no tests"
                res.error(f'{d}: go test -list failed (exit {rc}): {err.strip()}')
                plain.append(imp)  # its own run reports the build failure per package as well
                continue
            run = names
            if d in only and '*' not in only[d]:
                run = [n for n in run if n in only[d]]
            if '*' in defer.get(d, ()):
                res.notes.append(f'{d}: deferred whole'); continue
            skip = {n for n in run if n in defer.get(d, ())}
            run = [n for n in run if n not in skip]
            if not run:
                if not names:
                    plain.append(imp)  # nothing listed: run the package itself (TestMain, examples, a build check)
                continue
            shards = seed.get(d, {}).get('shards', 1)
            hazards = gotree.split_hazards('.', d) if shards > 1 else []
            if hazards:
                res.notes.append(f'{d}: not split ({len(hazards)} reasons, e.g. {"; ".join(hazards[:3])})')
                shards = 1
            parts = buckets(run, seed.get(d, {}).get('tests', {}), shards)
            expected[imp] = set(run)
            narrowed = len(parts) > 1 or d in only or bool(skip)
            for i, part in enumerate(parts):
                label = f'{d}#{i + 1}/{len(parts)}'
                weight[label] = sum(seed.get(d, {}).get('tests', {}).get(n, 0.5) for n in part)
                job(label, base + (['-run', '^(' + '|'.join(part) + ')$'] if narrowed else []) + [imp])
        if plain:
            job('plain', base + ['-p', str(a.p)] + plain)
        state, lock = new_state(), threading.Lock()
        if not a.no_prebuild and jobs:
            # Compile every test binary once before the parallel runs (a dozen go test processes compiling the same
            # packages at once starve tests that have a few hundred milliseconds of budget).
            wd = os.path.join(tmp, 'prebuild'); os.mkdir(wd); os.chmod(wd, 0o1777)
            sh(['go', 'test', '-vet=off', '-timeout', a.timeout, '-run', '^$', '-p', str(max(2, a.jobs))] + ([] if a.no_race else ['-race'])
               + [imp for imp in dirs if not only or dirs[imp] in only], env=dict(os.environ, TMPDIR=wd))
        jobs.sort(key=lambda j: (j['label'] != 'plain', -weight.get(j['label'], 0)))
        with ThreadPoolExecutor(max_workers=max(1, min(a.jobs, len(jobs) or 1))) as pool:
            list(pool.map(lambda j: run_job(j, state, lock), jobs))
        flaky = rerun_failed(state, base, tmp) if a.rerun else []
    finally:
        if tmp:
            release_dir(tmp, tmp_lock)
    for (pkg, test), (act, secs) in sorted(state['tests'].items()):
        if '/' in test:
            continue  # a subtest's result is its top-level test's
        reason = ''
        if act == 'skip':
            reason = ' '.join(l.strip() for l in state['out'].get((pkg, test), []) if not l.startswith(('=== ', '--- ')))[:200]
        elif act == 'fail':
            reason = ''.join(state['out'].get((pkg, test), []))[-3000:]
        res.add('go', dirs.get(pkg, pkg), test, act, secs, reason)
    for pkg, test, first in flaky:
        res.flaky_one('go', dirs.get(pkg, pkg), test, first)
    for pkg, (act, _) in sorted(state['pkgs'].items()):
        if act == 'fail' and not any(p == pkg and v[0] == 'fail' for (p, _), v in state['tests'].items()):
            # a build error, a panic in TestMain, a timeout: the package failed without a failed test
            out = ''.join(state['out'].get((pkg, None), [])) + ''.join(
                ''.join(lines) for (p, t), lines in state['out'].items() if p == pkg and t and (p, t) not in state['tests'])
            res.error(f'{dirs.get(pkg, pkg)}: package failed: {out[-3000:]}')
    for imp, want in expected.items():
        got = {t for (p, t) in state['tests'] if p == imp and '/' not in t}
        if want - got:
            res.error(f'{dirs[imp]}: {len(want - got)} listed tests reported no result: {", ".join(sorted(want - got)[:10])}')
    for label, rc in state['rc'].items():
        if rc and not res.failed() and not res.errors and not flaky:
            res.error(f'go test job {label} exited {rc}: {state["err"][label][-2000:]}')
    res.extra.update(jobs=state['job_secs'], cached=sorted(dirs.get(p, p) for p in state['cached']))
    return finish(a, res, t0)


def finish(a, res, t0):
    res.wall = time.monotonic() - t0
    if a.json:
        res.write(a.json)
    print(res.summary('gorun'))
    for n in res.notes:
        print('gorun: ' + n)
    for e in res.errors:
        print('gorun: ERROR ' + e[:4000], file=sys.stderr)
    for r in res.items:
        if r['status'] == 'fail':
            sys.stdout.write(r['reason'][-3000:] + '\n')
    return 0 if res.ok() else 1


if __name__ == '__main__':
    sys.exit(main())
