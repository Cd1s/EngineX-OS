#!/usr/bin/env python3
"""Run `go test -json` for packages and report every top-level test (tools/dev/results.py's format).

  gorun.py [--only FILE] [--skip FILE] [-p N] [--no-cache] [--no-race] [--timeout T] [--json OUT] PKG...

Packages run the way `go test` runs them (no package is ever split). --only FILE (`pkg-dir TestName` lines): only these
tests of these packages run, and each must report a result: one that does not (not in the package's listing, for
example because a build tag excludes its file) is reported as not run and gorun exits 2. --skip FILE: these tests do
not run here (they run elsewhere). A package that fails without a failed test (a build error, a panic in TestMain, a
timeout) is an error. Exit 0 only when nothing failed, nothing errored and nothing requested is missing.
"""
import argparse, collections, json, os, pathlib, re, signal, subprocess, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import results  # noqa: E402


def read_pairs(path):
    out = collections.defaultdict(list)
    for line in (pathlib.Path(path).read_text().splitlines() if path else []):
        f = line.split('#')[0].split()
        if len(f) == 2:
            out[f[0].removeprefix('./')].append(f[1])
    return out


def go_test(args, state):
    """One `go test -json` process; fold its events into state."""
    p = subprocess.Popen(['go', 'test', *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, errors='replace')
    err = []
    for line in p.stdout:
        try:
            ev = json.loads(line)
        except ValueError:
            err.append(line); continue
        act, pkg, test = ev.get('Action'), ev.get('Package'), ev.get('Test')
        if act == 'output':
            state['out'][(pkg, test)].append(ev['Output'])
        elif act in ('pass', 'fail', 'skip'):
            if test:
                state['tests'][(pkg, test)] = (act, ev.get('Elapsed', 0.0))
            else:
                state['pkgs'][pkg] = act
    err.append(p.stderr.read())
    p.wait()
    return p.returncode, ''.join(err)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('pkgs', nargs='+'); ap.add_argument('--only'); ap.add_argument('--skip')
    ap.add_argument('-p', type=int, default=0, help='go test -p (default: Go\'s own)')
    ap.add_argument('--no-cache', action='store_true'); ap.add_argument('--no-race', action='store_true')
    ap.add_argument('--timeout', default=os.environ.get('ENGINEX_GO_TIMEOUT', '30m'), help='go test -timeout of every invocation')
    ap.add_argument('--json')
    a = ap.parse_args()
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    t0 = time.monotonic()
    res = results.Results()
    mod = subprocess.run(['go', 'list', '-m'], capture_output=True, text=True).stdout.strip()
    listed = subprocess.run(['go', 'list', '-e', '-f', '{{.ImportPath}}', *a.pkgs], capture_output=True, text=True)
    if listed.returncode or not listed.stdout.strip():
        res.error(f'go list {" ".join(a.pkgs)} failed: {(listed.stderr or listed.stdout).strip()[-2000:]}')
        return finish(a, res, t0)
    dirs = {p: p.removeprefix(mod).lstrip('/') or '.' for p in listed.stdout.split()}
    only, skip = read_pairs(a.only), read_pairs(a.skip)
    base = ['-vet=off', '-json', '-timeout', a.timeout] + ([] if a.no_race else ['-race']) + (['-count=1'] if a.no_cache else []) + (
        ['-p', str(a.p)] if a.p else [])
    state = {'out': collections.defaultdict(list), 'tests': {}, 'pkgs': {}}
    if only:  # nothing but the named tests
        dirs = {imp: d for imp, d in dirs.items() if d in only}
    plain = [] if only else [imp for imp, d in dirs.items() if d not in skip]
    runs = [(base + plain, 'packages')] if plain else []
    for imp, d in dirs.items():
        if d in only:
            runs.append((base + ['-run', '^(' + '|'.join(sorted(set(only[d]))) + ')$', imp], d))
        elif d in skip and not only:
            runs.append((base + ['-skip', '^(' + '|'.join(sorted(set(skip[d]))) + ')$', imp], d))
    for args, label in runs:  # one after another
        rc, err = go_test(args, state)
        if rc and not state['tests'] and not state['pkgs']:
            res.error(f'go test {label} exited {rc} without results: {err.strip()[-2000:]}')
    for (pkg, test), (act, secs) in sorted(state['tests'].items()):
        if '/' in test:
            continue  # a subtest's result is its top-level test's
        lines = state['out'].get((pkg, test), [])
        reason = ''.join(lines)[-3000:] if act == 'fail' else ' '.join(l.strip() for l in lines if not l.startswith(('=== ', '--- ')))[:200] if act == 'skip' else ''
        res.add('go', dirs.get(pkg, pkg), test, act, secs, reason)
    kinds = {}
    for imp, d in dirs.items():
        act = state['pkgs'].get(imp)
        out = ''.join(state['out'].get((imp, None), []))
        kinds[d] = 'no-tests' if act == 'skip' and '[no test files]' in out else 'failed' if act == 'fail' else 'tested' if act else 'no-result'
        if act == 'fail' and not any(p == imp and v[0] == 'fail' for (p, _), v in state['tests'].items()):
            # a build error, a panic in TestMain, a timeout: the package failed without a failed test
            stray = ''.join(''.join(l) for (p, t), l in state['out'].items() if p == imp and t and (p, t) not in state['tests'])
            res.error(f'{d}: package failed: {(out + stray)[-3000:]}')
        elif act is None:
            res.error(f'{d}: go test reported nothing for it')
    for d, names in only.items():  # every requested test must report a result
        for n in sorted(set(names)):
            if kinds.get(d) != 'failed' and not any(x['where'] == d and x['test'] == n for x in res.items):
                res.add('go', d, n, 'notrun', 0, 'requested by name, reported no result (not in the package\'s listing here)')
    res.extra['packages'] = kinds
    return finish(a, res, t0)


def finish(a, res, t0):
    res.wall = time.monotonic() - t0
    if a.json:
        res.write(a.json)
    print(res.summary('gorun'))
    for e in res.errors:
        print('gorun: ERROR ' + e[:4000], file=sys.stderr)
    for r in res.items:
        if r['status'] == 'fail':
            sys.stdout.write(r['reason'][-3000:] + '\n')
    if not res.ok():
        return 1
    return 2 if any(r['status'] == 'notrun' for r in res.items) else 0


if __name__ == '__main__':
    sys.exit(main())
