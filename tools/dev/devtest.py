#!/usr/bin/env python3
"""Implementation of tools/dev/test.sh: one ARM build host runs every selected test (Go, Python, C, bash).

  tools/dev/test.sh [--full] [--list [--json]] [--base REF] [--into BRANCH] [--host HOST] [--exclusive]
                    [--no-cache] [--strict] [--report DIR]

Targeted (default): files changed against the merge base with origin/<BRANCH> (default agent/5306-batch2), plus
uncommitted and untracked files, are mapped to tests by tools/dev/testmap.py: gofmt of the changed Go files, go vet
and go test -race of the touched packages and every package that imports them (a package whose only change is in
its _test.go files or testdata runs alone: nothing else compiles those), the whole test file of a Python change
(the whole tools/az01-packer/test_ui_mod.py for the pack generator, in parallel shards), the C tests of the release
checklist with all their variants, and the automatic bash tests. Engine sandboxes the change selects are named with
their commands (the lane runs them; they need `enginex-run`), never silently dropped.
--full: every Go package, Python directory, C test and automatic bash test (the merge train).
--list: print the selection and why, and run nothing (needs only git and Python: works anywhere).

The plan is made here; the tree is copied with remote-build to one host of ENGINEX_BUILD_HOSTS (environment or
~/.config/enginex/dev.env), the least busy one that has what the plan needs (GCC 14 and the firmware lab for C tests
and the hook rebuild), and runs there inside `enginex-run sandbox` (`--exclusive`: `enginex-run exclusive`, for the
merge train and timing work), so lanes never slow an exclusive job and never take the heavy lock themselves.
There `go list` re-checks the Go selection (it can only add packages). One summary, one exit code.
"""
import argparse, hashlib, json, os, pathlib, re, shutil, socket, subprocess, sys, tempfile, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import gorun  # noqa: E402
import gotree  # noqa: E402
import testmap  # noqa: E402

MARK = 'ENGINEX_RESULT_JSON '
PLAN_NAME = '.enginex-test-plan.json'
DEFAULT_INTO = 'agent/5306-batch2'
SANDBOX_ENGINES = ('e504', 'e530', 'esl4', 'eaz04')


def load_env():
    path = os.environ.get('ENGINEX_DEV_ENV', os.path.expanduser('~/.config/enginex/dev.env'))
    if os.path.isfile(path):
        for line in pathlib.Path(path).read_text().splitlines():
            m = re.match(r'\s*(?:export\s+)?(\w+)=(.*?)\s*$', line)
            if m and m.group(1) not in os.environ:
                os.environ[m.group(1)] = m.group(2).strip('\'"')


def run(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


def git(root, *args):
    return run(['git', '-C', root, *args]).stdout


# ---------------------------------------------------------------- the plan (made where test.sh is called)

def make_plan(a, root):
    plan = {'mode': 'full' if a.full else 'targeted', 'no_cache': a.no_cache, 'strict': a.strict or a.no_cache,
            'base': None, 'changed': [], 'why': {}, 'manual': {}, 'unmapped': []}
    if a.full:
        sel = testmap.full_plan(root); changed = []
    else:
        base = a.base or run(['git', '-C', root, 'merge-base', 'HEAD', f'origin/{a.into}']).stdout.strip()
        if not base:
            print(f'test.sh: no merge base with origin/{a.into} (git fetch origin {a.into}, or pass --base REF); using HEAD', file=sys.stderr)
            base = 'HEAD'
        plan['base'] = base
        changed = [p for p in git(root, 'diff', '--name-only', '--no-renames', base).splitlines()
                   + git(root, 'ls-files', '-o', '--exclude-standard').splitlines() if p and p != PLAN_NAME]
        sel = testmap.map_paths(sorted(set(changed)), root)
    plan['changed'] = sorted(set(changed))
    plan['go_all'] = sel.go_all
    code = sorted(d for d in sel.go_code if os.path.isdir(os.path.join(root, d)))
    plan['go_code'] = code
    plan['go_static'] = [] if sel.go_all else sorted(set(gotree.static_closure(root, code)) | {d for d in sel.go if os.path.isdir(os.path.join(root, d))})
    plan['go'] = plan['go_static']
    plan['gofmt'] = None if a.full else [p for p in plan['changed'] if p.endswith('.go') and os.path.isfile(os.path.join(root, p))]
    by_dir = {}
    for d, f in sel.py:
        by_dir.setdefault(d, set()).add(f)
    plan['py'] = [{'dir': d, 'specs': [] if None in fs else sorted(fs)} for d, fs in sorted(by_dir.items())]  # no specs: whole dir
    plan['sh'] = sorted(sel.sh)
    plan['c'] = sorted(sel.c)
    plan['why'] = {k: v for k, v in sorted(sel.why.items())}
    plan['manual'] = dict(sorted(sel.manual.items()))
    plan['unmapped'] = sorted(sel.unmapped)
    plan['changed_tests'] = [] if a.full else sorted(map(list, testmap.changed_tests(root, plan['base'], plan['changed'])))
    plan['needs'] = needs_of(plan)
    return plan


def needs_of(plan):
    """What the build host must have: gcc14 (the hook rebuild, the C tests' -Werror builds), lab files."""
    needs = set()
    sh_auto = testmap.SH_TESTS['auto']
    for s in plan['sh']:
        needs |= set(sh_auto.get(s, {}).get('needs', []))
    if plan['c']:
        needs.add('gcc14')
        for src in plan['c']:
            text = json.dumps(testmap.C_TESTS['tests'].get(src) or testmap.C_TESTS['default']) + testmap.C_TESTS['default_args'].get(src, '')
            needs |= {'lab:' + e for e in SANDBOX_ENGINES if '{' + e + '}' in text}
            if '{lab}' in text or 'lab' in (testmap.C_TESTS['tests'].get(src) or testmap.C_TESTS['default']).get('needs', []):
                needs.add('lab')
    return sorted(needs)


def print_plan(plan):
    go = ['./...'] if plan['go_all'] else plan['go']
    print(f"== tools/dev/test.sh {plan['mode']} plan{' vs ' + plan['base'][:12] if plan['base'] else ''}: "
          f"{len(plan['changed'])} changed files ==")
    print(f"go test -race ({len(go)} packages): {' '.join(go) if go else '-'}")
    if plan['go_code']:
        print(f"  changed Go code in: {' '.join(plan['go_code'])} (their importers are included)")
    for item in plan['py']:
        print(f"python {item['dir']}: {' '.join(item['specs']) if item['specs'] else 'every test file'}")
    print(f"C tests ({len(plan['c'])}, every variant of tools/dev/c-tests.json): {' '.join(plan['c']) or '-'}")
    print(f"bash tests: {' '.join(plan['sh']) or '-'}")
    print(f"host needs: {', '.join(plan['needs']) or 'any build host'}")
    if plan['manual']:
        print(f"manual (Engine sandboxes this change selects; test.sh does not run them):")
        for path, info in plan['manual'].items():
            print(f"  {path}: {info['how']}  [{info['why']}]")
    if plan['unmapped']:
        print(f"unmapped (no test names these files): {' '.join(plan['unmapped'][:20])}")
    if plan['why']:
        print('why:')
        for key, reasons in plan['why'].items():
            print(f"  {key} <- {', '.join(reasons[:3])}{' ...' if len(reasons) > 3 else ''}")


# ---------------------------------------------------------------- execution (on the build host)

class Step:
    def __init__(self, name):
        self.name, self.secs, self.ok, self.note, self.tail, self.out = name, 0.0, None, '', '', ''


def spawn(step, cmd, env, cwd=None, timeout=None):
    t0 = time.monotonic()
    try:
        p = subprocess.run(cmd, env=env, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                           errors='replace', timeout=timeout)
        rc, out = p.returncode, p.stdout
    except subprocess.TimeoutExpired as e:
        rc, out = 124, (e.stdout or '') if isinstance(e.stdout, str) else ''
        out += f'\n(timed out after {timeout}s)'
    step.secs = time.monotonic() - t0
    step.ok = rc == 0
    step.out = out
    step.tail = '\n'.join(out.splitlines()[-150:]) if not step.ok else ''
    print(f'[{step.name}] {"ok" if step.ok else "FAILED"} {step.secs:.1f}s', file=sys.stderr, flush=True)
    return rc


def cpu_budget():
    """Cores this run may use: the idle cores of this host right now (other lanes share it), between 2 and 12."""
    if os.environ.get('ENGINEX_CPU_BUDGET'):
        return max(1, int(os.environ['ENGINEX_CPU_BUDGET']))

    def snap():
        v = list(map(int, pathlib.Path('/proc/stat').read_text().splitlines()[0].split()[1:]))
        return sum(v), v[3] + v[4]
    t1, i1 = snap(); time.sleep(1); t2, i2 = snap()
    return max(2, min(12, round((i2 - i1) / max(1, t2 - t1) * (os.cpu_count() or 4))))


def gcc_major():
    out = run(['gcc', '-dumpversion']).stdout.strip()
    return out.split('.')[0] if out else ''


def exec_plan(plan_path):
    plan = json.loads(pathlib.Path(plan_path).read_text())
    if os.uname().machine not in ('aarch64', 'arm64'):
        print(MARK + json.dumps({'steps': [{'name': 'host', 'secs': 0, 'ok': False, 'note': '',
                                            'tail': f'tools/dev/test.sh runs on an ARM build host (this is {os.uname().machine})'}],
                                 'go': None, 'py': {}, 'c': None}))
        return 0
    os.nice(5)
    name = 'enginex-test-' + hashlib.sha1(os.getcwd().encode()).hexdigest()[:8]
    tmp, lock = gorun.stable_dir('/var/tmp', name)  # same TMPDIR each run of this tree: Go's test cache keeps hitting
    env = dict(os.environ, TMPDIR=tmp, GOFLAGS=(os.environ.get('GOFLAGS', '') + ' -buildvcs=false').strip())
    made_git = False
    if not os.path.exists('.git'):  # tests that call git ls-files see the tree, as in the dev line's full run
        subprocess.run(['git', 'init', '-q'], check=True)
        env['GIT_INDEX_FILE'] = os.path.join(tmp, 'index')
        subprocess.run(['git', 'add', '-A'], env=env, check=True, capture_output=True)
        made_git = True
    budget = cpu_budget()
    state = os.path.expanduser(os.environ.get('ENGINEX_DEV_STATE', '~/.cache/enginex-dev'))
    os.makedirs(state, exist_ok=True)
    results = {'steps': [], 'go': None, 'py': {}, 'c': None, 'budget': budget, 'host_gcc': gcc_major(),
               'go_added_by_go_list': [], 'is_root': os.geteuid() == 0}
    print(f'[exec] {socket.gethostname()}: cpu budget {budget}, gcc {results["host_gcc"]}, root {results["is_root"]}', file=sys.stderr, flush=True)

    def add(step):
        results['steps'].append({'name': step.name, 'secs': round(step.secs, 1), 'ok': step.ok, 'note': step.note, 'tail': step.tail})

    try:
        # Go selection: the plan's static closure, plus whatever `go list` finds that it missed.
        pk = ['./...'] if plan['go_all'] else [f'./{d}' if d != '.' else '.' for d in plan['go']]
        if plan['go_code'] and not plan['go_all']:
            gl = gotree.go_list_closure('.', set(plan['go_code']), env=env)
            if gl is not None:
                extra = sorted(set(gl) - set(plan['go']))
                results['go_added_by_go_list'] = extra
                pk += [f'./{d}' if d != '.' else '.' for d in extra]
        if plan['gofmt'] is None or plan['gofmt']:
            s = Step('gofmt'); spawn(s, ['gofmt', '-l'] + (['.'] if plan['gofmt'] is None else plan['gofmt']), env)
            if s.ok and s.out.strip():
                s.ok, s.tail = False, 'not gofmt-clean:\n' + s.out
            add(s)
        if pk:
            s = Step(f'go vet ({len(pk)} pkg args)'); spawn(s, ['go', 'vet'] + pk, env); add(s)
        for item in plan['py']:
            d = item['dir']; s = Step(f'py {d}')
            out_json = os.path.join(tmp, 'py-' + d.replace('/', '_') + '.json')
            cmd = [sys.executable, os.path.join(HERE, 'pyrun.py'), d] + item['specs'] + [
                '--json', out_json, '-j', str(budget), '--seed', os.path.join(HERE, 'py-slow.json'),
                '--timings', os.path.join(state, 'pyrun-' + d.replace('/', '_') + '.json')] + ([] if plan['strict'] else ['--rerun'])
            spawn(s, cmd, env)
            if os.path.exists(out_json):
                results['py'][d] = json.loads(pathlib.Path(out_json).read_text())
                r = results['py'][d]
                s.note = f"{r['run']} tests in {len(r.get('shards', []))} shards"
            add(s)
        if pk:
            s = Step('go test -race'); out_json = os.path.join(tmp, 'go.json')
            cmd = [sys.executable, os.path.join(HERE, 'gorun.py'), '--seed', os.path.join(HERE, 'go-slow.json'),
                   '--json', out_json, '--jobs', str(budget), '-p', str(max(2, budget // 2))] + (
                   ['--no-cache'] if plan['no_cache'] else []) + ([] if plan['strict'] else ['--rerun']) + pk
            spawn(s, cmd, env)
            if os.path.exists(out_json):
                results['go'] = json.loads(pathlib.Path(out_json).read_text())
                g = results['go']
                s.note = f"{g['run']} tests, {sum(1 for v in g['packages'].values() if v.get('cached'))} packages cached"
            add(s)
        if plan['c']:
            s = Step(f"C tests ({len(plan['c'])})"); out_json = os.path.join(tmp, 'c.json')
            spawn(s, [sys.executable, os.path.join(HERE, 'crun.py'), '-j', str(budget), '--json', out_json] + plan['c'], env)
            if os.path.exists(out_json):
                results['c'] = json.loads(pathlib.Path(out_json).read_text())
                s.note = f"{len(results['c']['results'])} variants, {len(results['c']['skipped'])} runs skipped"
            add(s)
        for script in sorted(plan['sh'], key=lambda x: x == testmap.OVERLAY):  # the overlay test last, without .git
            info = testmap.SH_TESTS['auto'].get(script, {})
            s = Step(f'sh {script}')
            why_not = []
            if 'root' in info.get('needs', []) and os.geteuid() != 0:
                why_not.append('needs root')
            if 'gcc14' in info.get('needs', []) and results['host_gcc'] != '14':
                why_not.append(f'needs GCC 14 (host has {results["host_gcc"]})')
            if why_not:
                s.ok, s.note = True, 'SKIPPED: ' + ', '.join(why_not)
                results.setdefault('sh_skipped', []).append([script, ', '.join(why_not)])
                add(s)
                continue
            sh_env = dict(env)
            if script == testmap.OVERLAY and made_git:  # the dev line runs it without the throwaway repository
                sh_env.pop('GIT_INDEX_FILE', None)
                shutil.rmtree('.git', ignore_errors=True)
                made_git = False
            spawn(s, ['bash', script], sh_env, timeout=int(os.environ.get('ENGINEX_SH_TIMEOUT', '3600')))
            add(s)
    finally:
        gorun.release_dir(tmp, lock)
        if made_git:
            shutil.rmtree('.git', ignore_errors=True)
    print(MARK + json.dumps(results))
    return 0


# ---------------------------------------------------------------- host choice and the remote run

def lab_path(root):
    if os.environ.get('ENGINEX_LAB'):
        return os.environ['ENGINEX_LAB']
    try:
        m = re.search(r"^legacy_lab='([^']+)'", pathlib.Path(root, 'rootfs-overlay/usr/libexec/enginex-migration').read_text(), re.M)
        return m.group(1) if m else ''
    except OSError:
        return ''


def probe(host, needs, lab):
    """(score, reason): lower is better; None when the host lacks something the plan needs."""
    sshctl = shutil.which('sshctl')
    if not sshctl:
        return 0, 'no sshctl: not probed'
    checks = ' '.join(f"[ -e '{lab}/{testmap.C_TESTS['engines'][n.split(':')[1]]}' ] && echo has:{n} ;" for n in needs if n.startswith('lab:'))
    script = (". /etc/profile.d/buildtools.sh 2>/dev/null; echo gcc:$(gcc -dumpversion); echo arch:$(uname -m); "
              f"[ -d '{lab}' ] && echo has:lab; {checks} "
              "echo load:$(cut -d' ' -f1 /proc/loadavg); echo cpus:$(nproc); "
              "echo heavy:$(ps -eo args= | grep -c '[f]lock.*enginex-heavy'); echo slots:$(ls /var/lock/enginex-slot*.lock 2>/dev/null | wc -l)")
    out = run([sshctl, 'run', host, '--no-stdin', '--timeout', '20s', '--argv', 'sh', '-c', script]).stdout
    kv = dict(line.split(':', 1) for line in out.splitlines() if ':' in line and not line.startswith('ssm'))
    if not kv:
        return None, 'unreachable'
    if kv.get('arch') not in ('aarch64', 'arm64'):
        return None, f"not ARM ({kv.get('arch')})"
    have = {line[4:] for line in out.splitlines() if line.startswith('has:')}
    if 'gcc14' in needs and kv.get('gcc', '').split('.')[0] != '14':
        return None, f"gcc {kv.get('gcc')}"
    for n in needs:
        if (n == 'lab' or n.startswith('lab:')) and n not in have:
            return None, f'no {n}'
    try:
        load, cpus, heavy = float(kv.get('load', 9)), int(kv.get('cpus', 1)), int(kv.get('heavy', 0))
    except ValueError:
        return None, 'unparsable probe'
    return heavy * 10 + load / cpus, f"load {load}/{cpus} cpus, {heavy} heavy-lock jobs"


def pick_host(hosts, needs, lab):
    best, best_score, notes = None, None, []
    for h in hosts:
        score, why = probe(h, needs, lab)
        notes.append(f'{h}: {why}')
        if score is not None and (best is None or score < best_score):
            best, best_score = h, score
    return best, notes


def remote(host, name, command, root, timeout):
    """remote-build to host; returns (returncode, output)."""
    rb = os.environ.get('REMOTE_BUILD_BIN') or shutil.which('remote-build')
    if not rb:
        return 2, 'remote-build is required\n'
    env = dict(os.environ, REMOTE_BUILD_HOST=host, REMOTE_BUILD_TIMEOUT=timeout)
    p = subprocess.run([rb, '-C', root, '--name', name, '--clean', '--', command], env=env, stdout=subprocess.PIPE,
                       stderr=subprocess.STDOUT, text=True, errors='replace')
    sshctl = shutil.which('sshctl')
    if sshctl and not os.environ.get('ENGINEX_KEEP_REMOTE'):  # the exact directory remote-build created
        run([sshctl, 'run', host, '--no-stdin', '--timeout', '60s', '--argv', 'rm', '-rf', '--', f'/root/build/{name}'])
    return p.returncode, p.stdout


def parse_result(out):
    for line in reversed(out.splitlines()):
        if line.startswith(MARK):
            return json.loads(line[len(MARK):])
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--full', action='store_true'); ap.add_argument('--no-cache', action='store_true')
    ap.add_argument('--strict', action='store_true', help='no rerun of failed tests (implied by --no-cache)')
    ap.add_argument('--base'); ap.add_argument('--into', default=os.environ.get('ENGINEX_TEST_TARGET', DEFAULT_INTO))
    ap.add_argument('--tree', help='tree under test (default: the git checkout of the current directory)')
    ap.add_argument('--host', help='this build host (default: the least busy of ENGINEX_BUILD_HOSTS that qualifies)')
    ap.add_argument('--exclusive', action='store_true', help='enginex-run exclusive instead of sandbox (merge train, timing)')
    ap.add_argument('--list', action='store_true'); ap.add_argument('--json', action='store_true')
    ap.add_argument('--report'); ap.add_argument('--top', type=int, default=5)
    ap.add_argument('--exec', dest='exec_plan')
    a = ap.parse_args()
    load_env()
    if a.exec_plan:
        return exec_plan(a.exec_plan)
    root = os.path.abspath(a.tree) if a.tree else git(os.getcwd(), 'rev-parse', '--show-toplevel').strip()
    if not root:
        print('test.sh: not inside a git checkout', file=sys.stderr); return 2
    started = time.monotonic()
    plan = make_plan(a, root)
    if a.list:
        print(json.dumps(plan, indent=1) if a.json else '', end='\n' if a.json else '')
        if not a.json:
            print_plan(plan)
        return 0
    print_plan(plan)
    nothing = not (plan['go_all'] or plan['go'] or plan['py'] or plan['sh'] or plan['c'])
    if nothing:
        return report(a, plan, None, '', started)
    hosts = [a.host] if a.host else os.environ.get('ENGINEX_BUILD_HOSTS', '').replace(',', ' ').split()
    if not hosts:
        print('test.sh: ENGINEX_BUILD_HOSTS is not set (environment or ~/.config/enginex/dev.env)', file=sys.stderr); return 2
    lab = lab_path(root)
    host, notes = pick_host(hosts, plan['needs'], lab)
    print('hosts: ' + '; '.join(notes))
    if not host:
        print(f"test.sh: no build host has what this plan needs ({', '.join(plan['needs'])})", file=sys.stderr); return 1
    plan_path = os.path.join(root, PLAN_NAME)
    pathlib.Path(plan_path).write_text(json.dumps(plan))
    # One stable remote directory per tree (Go's build cache keys on the path), a private one when it is busy.
    name = 'enginex-test-' + re.sub(r'[^A-Za-z0-9._-]', '_', os.path.basename(root))[:40] + '-' + hashlib.sha1(root.encode()).hexdigest()[:6]
    state = os.path.expanduser(os.environ.get('ENGINEX_DEV_STATE', '~/.cache/enginex-dev')); os.makedirs(state, exist_ok=True)
    guard = open(os.path.join(state, name + '.lock'), 'w')
    try:
        import fcntl
        fcntl.flock(guard, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        name += f'-{os.getpid()}'
    mode = 'exclusive' if a.exclusive else 'sandbox'
    inner = f'bash tools/dev/test.sh --exec {PLAN_NAME}'
    command = f'if command -v enginex-run >/dev/null 2>&1; then enginex-run {mode} -- {inner}; else {inner}; fi'
    print(f'run: {host} ({mode}), /root/build/{name}', flush=True)
    try:
        rc, out = remote(host, name, command, root, os.environ.get('ENGINEX_TEST_TIMEOUT', '150m'))
    finally:
        try:
            os.remove(plan_path)
        except OSError:
            pass
        guard.close()
    res = parse_result(out)
    if res is None:
        res = {'steps': [{'name': 'remote run', 'secs': round(time.monotonic() - started, 1), 'ok': False,
                          'note': f'host {host}, exit {rc}', 'tail': out[-4000:]}], 'go': None, 'py': {}, 'c': None}
    res['host'] = host
    return report(a, plan, res, host, started)


def report(a, plan, res, host, started):
    ok = True
    print(f"\n== tools/dev/test.sh {plan['mode']}{' vs ' + plan['base'][:12] if plan['base'] else ''}"
          f" ({len(plan['changed'])} changed files){' on ' + host if host else ''} ==")
    print(f"{'step':40} {'wall':>8}  result")
    skipped, flaky = [], []
    res = res or {'steps': [], 'go': None, 'py': {}, 'c': None}
    for s in res['steps']:
        ok &= bool(s['ok'])
        print(f"{s['name'][:40]:40} {s['secs']:7.1f}s  {'ok' if s['ok'] else 'FAILED'}{'  ' + s['note'] if s['note'] else ''}")
    if res.get('go_added_by_go_list'):
        print(f"go list added packages the static map missed: {' '.join(res['go_added_by_go_list'])}")
    if res.get('go'):
        flaky += [('go ' + x['pkg'] + ' ' + x['test'], x['first_failure']) for x in res['go'].get('flaky', [])]
        skipped += [('go', x['pkg'], x['test'], x['reason']) for x in res['go']['skipped']]
    for d, r in (res.get('py') or {}).items():
        flaky += [(f"py {d} {x['id']}", x['first_failure']) for x in r.get('flaky', [])]
        skipped += [('py', d, x[0], x[1]) for x in r['skipped']]
    c_skipped = (res.get('c') or {}).get('skipped', [])
    sh_skipped = res.get('sh_skipped', [])
    changed = [tuple(c) for c in plan['changed_tests']]
    unproven = []
    for kind, where, test, reason in skipped:
        key = (kind, where, test.split('/')[0] if kind == 'go' else test)
        if key in changed:
            unproven.append(f'{kind} {where} {test} (skipped: {reason or "no reason"})')
    c_res = (res.get('c') or {}).get('results', [])
    for kind, where, _ in changed:
        if kind == 'c' and where in plan['c']:
            mine = [r for r in c_res if r['name'].startswith(where + ' ')]
            if not mine:
                unproven.append(f'c {where} (did not run)')
            elif any(r['skipped'] for r in mine):
                unproven.append(f"c {where} (skipped: {'; '.join(x for r in mine for x in r['skipped'])[:200]})")
        if kind == 'sh' and where in plan['sh'] and where in dict(sh_skipped):
            unproven.append(f'sh {where} (skipped: {dict(sh_skipped)[where]})')
    if unproven:
        ok = False
        print(f"{'proof of changed tests':40} {'':>8}  FAILED  not proven: a test this change added or changed did not run")
    if flaky:
        print(f'\nFLAKY: {len(flaky)} test(s) failed in the parallel run and passed alone on a rerun (host load; not proven stable):')
        for name, first in flaky:
            print(f'  {name}\n    first failure: ' + ' | '.join(l.strip() for l in first.splitlines() if l.strip())[:300])
    print(f'\nskipped: {len(skipped)}' + ''.join(f'\n  {k} {w} {t}  [{r[:80]}]' for k, w, t, r in skipped[:40]))
    for s in c_skipped:
        print(f'  C run skipped: {s}')
    for s, why in sh_skipped:
        print(f'  bash test skipped: {s} ({why})')
    for u in unproven:
        print('NOT PROVEN: ' + u)
    if plan['manual']:
        print(f"\nstill to run by the lane ({len(plan['manual'])} Engine sandbox tests this change selects):")
        for path, info in plan['manual'].items():
            print(f"  {path}: {info['how']}")
    if plan['unmapped']:
        print(f"unmapped changed files (no test names them): {' '.join(plan['unmapped'][:20])}")
    for s in res['steps']:
        if not s['ok'] and s['tail']:
            print(f"\n----- {s['name']} (last lines) -----\n{s['tail']}")
    if res.get('go') and a.top:
        print(f'\nslowest Go tests (top {a.top}):')
        for k, v in list(res['go']['tests'].items())[:a.top]:
            print(f'  {v:8.1f}s  {k}')
    if a.report:
        os.makedirs(a.report, exist_ok=True)
        pathlib.Path(a.report, 'test-run.json').write_text(json.dumps({'plan': plan, 'result': res}, indent=1))
    wall = time.monotonic() - started
    manual = f", {len(plan['manual'])} manual sandbox tests still to run" if plan['manual'] else ''
    print(f"\nRESULT: {'PASS' if ok else 'FAIL'}  (wall {wall:.1f}s{manual})")
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
