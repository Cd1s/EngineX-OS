#!/usr/bin/env python3
"""Run Go tests with per-package and per-test timing, sharding the slowest packages.

  gorun.py [--seed FILE] [--defer FILE | --only FILE] [-p N] [--no-cache] [--json OUT] PKG...

Packages listed in the seed file (tools/dev/go-slow.json: {"pkg/dir": {"shards": N, "tests": {Name: secs}}})
are split into N buckets of top-level tests (longest first, deterministic, so Go's test cache keeps working) and
every bucket runs as its own `go test -run '^(a|b)$'` process in parallel. The tests run in a sharded package
must equal `go test -list` minus the deferred ones, or the run fails. --defer / --only take lines `pkg-dir Test`
(`*` for the whole package); deferred tests are not run here and are reported by name.
Everything goes through `go test -json`, so --json carries per-package and per-test seconds.
"""
import argparse, collections, json, os, pathlib, re, shutil, subprocess, sys, tempfile, threading, time
from concurrent.futures import ThreadPoolExecutor

NAME = re.compile(r'^(Test|Example|Fuzz)\w*$')


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
                key = (pkg, test)
                state['out'][key].append(ev['Output'])
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


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('pkgs', nargs='+'); ap.add_argument('--seed'); ap.add_argument('--defer'); ap.add_argument('--only')
    ap.add_argument('-p', type=int, default=6, help='go test -p of the unsharded packages'); ap.add_argument('--jobs', type=int, default=8, help='go test processes at once'); ap.add_argument('--no-cache', action='store_true')
    ap.add_argument('--no-race', action='store_true'); ap.add_argument('--timeout', default='30m')
    ap.add_argument('--json'); ap.add_argument('--top', type=int, default=20); ap.add_argument('--suggest-seed')
    a = ap.parse_args()
    t0 = time.monotonic()
    mod = sh(['go', 'list', '-m']).stdout.strip()
    pkgs = sh(['go', 'list'] + a.pkgs)
    if pkgs.returncode:
        print(pkgs.stderr, file=sys.stderr); return 2
    dirs = {p: p.removeprefix(mod).lstrip('/') or '.' for p in pkgs.stdout.split()}
    seed = json.loads(pathlib.Path(a.seed).read_text()) if a.seed else {}
    defer, only = read_pairs(a.defer), read_pairs(a.only)
    base = ['go', 'test', '-vet=off', '-json', '-timeout', a.timeout] + ([] if a.no_race else ['-race']) + (['-count=1'] if a.no_cache else [])
    tmp = tempfile.mkdtemp(prefix='gorun-', dir=os.environ.get('TMPDIR'))
    os.chmod(tmp, 0o755)  # tests that drop privileges must reach their own TMPDIR
    jobs, expected, deferred, plain, weight = [], {}, [], [], {}

    def job(label, cmd):
        wd = os.path.join(tmp, str(len(jobs))); os.mkdir(wd); os.chmod(wd, 0o1777)
        jobs.append({'label': label, 'cmd': cmd, 'env': dict(os.environ, TMPDIR=wd)})

    def listing(imp):
        out = sh(['go', 'test', '-vet=off', '-list', '.*', imp] + ([] if a.no_race else ['-race']))
        return [x for x in out.stdout.split() if NAME.match(x)]

    special = {imp: d for imp, d in dirs.items() if d in seed or d in defer or d in only}
    names = {}
    if special:
        with ThreadPoolExecutor(max_workers=min(len(special), 8)) as pool:
            for imp, lst in zip(special, pool.map(listing, special)):
                names[imp] = lst
    for imp, d in dirs.items():
        if only and d not in only:
            continue
        if imp not in special:
            plain.append(imp); continue
        run = names[imp]
        if d in only and '*' not in only[d]:
            run = [n for n in run if n in only[d]]
        if '*' in defer.get(d, ()):
            deferred += [(d, n) for n in run]; continue
        skip = [n for n in run if n in defer.get(d, ())]
        deferred += [(d, n) for n in skip]
        run = [n for n in run if n not in skip]
        if not run:
            continue
        parts = buckets(run, seed.get(d, {}).get('tests', {}), seed.get(d, {}).get('shards', 1))
        expected[imp] = set(run)
        for i, part in enumerate(parts):
            narrowed = len(parts) > 1 or d in only or skip
            weight[f'{d}#{i + 1}/{len(parts)}'] = sum(seed.get(d, {}).get('tests', {}).get(n, 0.5) for n in part)
            job(f'{d}#{i + 1}/{len(parts)}', base + (['-run', '^(' + '|'.join(part) + ')$'] if narrowed else []) + [imp])
    if plain:
        job('plain', base + ['-p', str(a.p)] + plain)
    state = {'out': collections.defaultdict(list), 'tests': {}, 'pkgs': {}, 'cached': set(), 'rc': {}, 'err': {}, 'job_secs': {}}
    lock = threading.Lock()
    try:
        jobs.sort(key=lambda j: (j['label'] != 'plain', -weight.get(j['label'], 0)))  # plain job first, then the heaviest shards
        with ThreadPoolExecutor(max_workers=max(1, min(a.jobs, len(jobs)))) as pool:
            list(pool.map(lambda j: run_job(j, state, lock), jobs))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    wall = time.monotonic() - t0
    tests, pkgs_res = state['tests'], state['pkgs']
    failed = [k for k, v in tests.items() if v[0] == 'fail']
    bad_pkgs = sorted(p for p, v in pkgs_res.items() if v[0] == 'fail')
    skipped = []
    for (pkg, test), (act, _) in sorted(tests.items()):
        if act == 'skip':
            reason = ' '.join(l.strip() for l in state['out'].get((pkg, test), []) if not l.startswith(('=== ', '--- ')))[:160]
            skipped.append({'pkg': dirs.get(pkg, pkg), 'test': test, 'reason': reason})
    lost = []
    for imp, want in expected.items():
        got = {t for (p, t) in tests if p == imp and '/' not in t}
        lost += [(dirs[imp], t) for t in sorted(want - got)]
    for key in failed:
        sys.stdout.write(''.join(state['out'].get(key, [])))
    for pkg in bad_pkgs:
        sys.stdout.write(''.join(state['out'].get((pkg, None), [])))
    for label, rc in state['rc'].items():
        if rc and not bad_pkgs and not failed:
            print(f'gorun: job {label} exited {rc}\n{state["err"][label]}', file=sys.stderr)
    ok = not failed and not bad_pkgs and not lost and all(rc == 0 for rc in state['rc'].values())
    perpkg = collections.defaultdict(lambda: collections.Counter())
    for (pkg, _), (act, _) in tests.items():
        perpkg[dirs.get(pkg, pkg)][act] += 1
    top = sorted(((e, dirs.get(p, p), t) for (p, t), (act, e) in tests.items() if '/' not in t), reverse=True)
    res = {'ok': ok, 'wall': round(wall, 1), 'jobs': state['job_secs'], 'run': len(tests), 'lost': lost, 'failed': [f'{dirs.get(p, p)} {t}' for p, t in failed],
           'failed_packages': [dirs.get(p, p) for p in bad_pkgs], 'skipped': skipped, 'deferred': [f'{d} {t}' for d, t in deferred],
           'packages': {dirs.get(p, p): {'secs': v[1], 'cached': p in state['cached'], **dict(perpkg[dirs.get(p, p)])} for p, v in pkgs_res.items()},
           'tests': {f'{d} {t}': e for e, d, t in top}}
    if a.json:
        pathlib.Path(a.json).write_text(json.dumps(res, indent=1))
    if a.suggest_seed:
        sug = {}
        for pkg, v in pkgs_res.items():
            d = dirs.get(pkg, pkg)
            if v[1] >= 60 and pkg not in state['cached']:
                sug[d] = {'shards': min(8, max(2, int(v[1] // 60) + 1)),
                          'tests': {t: round(e, 1) for e, dd, t in top if dd == d and e >= 1}}
        pathlib.Path(a.suggest_seed).write_text(json.dumps(sug, indent=1, sort_keys=True))
    nshard = sum(1 for j in jobs if '#' in j['label'])
    print(f'gorun: {len(tests)} tests in {len(pkgs_res)} packages, {len(skipped)} skipped, {len(deferred)} deferred, '
          f'{len(failed)} failed, {nshard} shard processes, {wall:.1f}s -> {"OK" if ok else "FAILED"}')
    if lost:
        print('gorun: tests missing from shard runs: ' + ', '.join(f'{d} {t}' for d, t in lost[:10]), file=sys.stderr)
    for e, d, t in top[:a.top]:
        print(f'  {e:8.1f}s  {d} {t}')
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
