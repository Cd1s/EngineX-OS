#!/usr/bin/env python3
"""Implementation of tools/dev/test.sh: fast tier (pure Go + Python) and ARM tier, one summary, one exit code.

  tools/dev/test.sh [--full] [--no-cache] [--base REF] [--into BRANCH] [--no-arm] [--list] [--report DIR]

Targeted (default): files changed against the merge base with origin/<BRANCH> (default agent/5306-integration)
plus uncommitted and untracked files -> gofmt on changed Go files, go vet and go test -race on the touched Go
packages and everything that imports them, and the mapped Python / shell tests (tools/dev/testmap.py).
--full: every Go package, Python directory and shell test. --no-cache: go test -count=1.
Fast tier runs on the x86 host ENGINEX_FAST_HOST (here when this is that host, else through remote-build);
the ARM tier (tools/dev/arm-tests.txt) runs on one of ENGINEX_BUILD_HOSTS under the heavy lock, at the same time.
Settings come from the environment, else from ~/.config/enginex/dev.env (KEY=VALUE lines).
"""
import pathlib, argparse, hashlib, json, os, re, shutil, socket, subprocess, sys, tempfile, threading, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import gorun  # noqa: E402
import testmap  # noqa: E402

LOCKS = {}
MARK = 'ENGINEX_RESULT_JSON '
LOCK = '/var/lock/enginex-heavy.lock'
PLAN_NAME = '.enginex-test-plan.json'


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


def load_arm(path):
    entries = []
    for n, line in enumerate(pathlib.Path(path).read_text().splitlines(), 1):
        body, _, reason = line.partition('#')
        f = body.split()
        if not f:
            continue
        if len(f) < 2 or not reason.strip():
            raise SystemExit(f'{path}:{n}: need `go <pkg-dir> <Test|*>`, `py <dir> <id>` or `sh <script>`, then `# reason`')
        entries.append({'kind': f[0], 'target': f[1], 'id': f[2] if len(f) > 2 else '*', 'reason': reason.strip()})
    return entries


def go_closure(root, dirs):
    """Package dirs plus every package that imports them, directly, transitively or from its tests."""
    fmt = '{{.ImportPath}}\t{{.Dir}}\t{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}'
    out = run(['go', 'list', '-e', '-f', fmt, './...'], cwd=root)
    mod = run(['go', 'list', '-m'], cwd=root).stdout.strip()
    rel, users = {}, {}
    for line in out.stdout.splitlines():
        imp, d, deps = line.split('\t')
        rel[imp] = os.path.relpath(d, root)
        for dep in deps.split():
            users.setdefault(dep, set()).add(imp)
    want = {imp for imp, d in rel.items() if d in dirs}
    todo = list(want)
    while todo:
        for u in users.get(todo.pop(), ()):
            if u not in want:
                want.add(u); todo.append(u)
    return sorted(rel[i] for i in want)


def make_plan(a, root):
    plan = {'mode': 'full' if a.full else 'targeted', 'no_cache': a.no_cache, 'strict': a.strict or a.no_cache, 'base': None, 'changed': []}
    arm = load_arm(os.path.join(HERE, 'arm-tests.txt'))
    if a.full:
        sel = testmap.full_plan(); changed = []
    else:
        base = a.base or run(['git', '-C', root, 'merge-base', 'HEAD', f'origin/{a.into}']).stdout.strip()
        if not base:
            print(f'test.sh: no merge base with origin/{a.into} (git fetch origin {a.into}, or pass --base REF); using HEAD', file=sys.stderr)
            base = 'HEAD'
        plan['base'] = base
        changed = [p for p in git(root, 'diff', '--name-only', '--no-renames', base).splitlines()
                   + git(root, 'ls-files', '-o', '--exclude-standard').splitlines() if p and p != PLAN_NAME]
        sel = testmap.map_paths(changed, root)
    plan['changed'] = sorted(set(changed))
    go_dirs = sorted(d for d in sel.go if os.path.isdir(os.path.join(root, d)))
    plan['go_all'] = sel.go_all
    plan['go'] = [] if sel.go_all else go_closure(root, set(go_dirs)) if go_dirs else []
    plan['gofmt'] = None if a.full else [p for p in plan['changed'] if p.endswith('.go') and os.path.isfile(os.path.join(root, p))]
    by_dir = {}
    for d, f in sel.py:
        by_dir.setdefault(d, set()).add(f)
    plan['py'] = [{'dir': d, 'specs': [] if None in fs else sorted(fs)} for d, fs in sorted(by_dir.items())]  # no specs: whole dir
    plan['sh'] = sorted(sel.sh)
    go_set = set(plan['go'])
    py_files = {(d, f) for d, f in sel.py if f}
    py_dirs_full = {d for d, f in sel.py if f is None}
    entries = []
    for e in arm:
        if e['kind'] == 'go':
            hit = sel.go_all or e['target'] in go_set
        elif e['kind'] == 'py':
            mod = e['id'].split('.')[0]
            hit = e['target'] in py_dirs_full or (e['target'], mod.removesuffix('.py') + '.py') in py_files
        else:
            hit = e['target'] in sel.sh or e['target'] in testmap.FULL_SH and a.full
        entries.append(dict(e, selected=bool(hit)))
    plan['arm'] = entries
    plan['changed_tests'] = [] if a.full else sorted(map(list, testmap.changed_tests(root, plan['base'], plan['changed'])))
    return plan


# ---------------------------------------------------------------- execution (fast tier and ARM tier)

class Step:
    def __init__(self, name):
        self.name, self.secs, self.ok, self.note, self.tail = name, 0.0, None, '', ''


def spawn(step, cmd, env, cwd=None, quiet=False):
    t0 = time.monotonic()
    p = subprocess.run(cmd, env=env, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors='replace')
    step.secs = time.monotonic() - t0
    step.ok = p.returncode == 0
    lines = p.stdout.splitlines()
    step.tail = '\n'.join(lines[-150:]) if not step.ok else ''
    step.out = p.stdout
    print(f'[{step.name}] {"ok" if step.ok else "FAILED"} {step.secs:.1f}s', file=sys.stderr, flush=True)
    return p


def prepare(plan_path):
    """Own TMPDIR (world-writable: some tests drop privileges) and a git index for tests that call git ls-files."""
    os.nice(10)
    # Same path on every run of this tree, so Go's test cache (keyed on TMPDIR) keeps hitting.
    tmp, lock = gorun.stable_dir('/var/tmp', 'enginex-fast-' + hashlib.sha1(os.getcwd().encode()).hexdigest()[:8])
    LOCKS[tmp] = lock
    env = dict(os.environ, TMPDIR=tmp)
    made_git = False
    if not os.path.exists('.git'):
        subprocess.run(['git', 'init', '-q'], check=True)
        env['GIT_INDEX_FILE'] = os.path.join(tmp, 'index')
        subprocess.run(['git', 'add', '-A'], env=env, check=True, capture_output=True)
        made_git = True
    return tmp, env, made_git


def finish(tmp, made_git):
    gorun.release_dir(tmp, LOCKS.pop(tmp, None))  # the directory created in prepare()
    if made_git:
        shutil.rmtree('.git', ignore_errors=True)


def pairs_file(path, rows):
    pathlib.Path(path).write_text(''.join(f'{d} {t}\n' for d, t in rows))


def cpu_budget():
    """Cores this run may use: the idle cores of this host right now (other agents share it), between 4 and 12.
    Timing-sensitive tests fail when a shared host is saturated, so the runners never take more than this."""
    if os.environ.get('ENGINEX_CPU_BUDGET'):
        return max(1, int(os.environ['ENGINEX_CPU_BUDGET']))

    def snap():
        v = list(map(int, pathlib.Path('/proc/stat').read_text().splitlines()[0].split()[1:]))
        return sum(v), v[3] + v[4]
    t1, i1 = snap(); time.sleep(1); t2, i2 = snap()
    return max(4, min(12, int((i2 - i1) / max(1, t2 - t1) * (os.cpu_count() or 4))))


def exec_fast(plan):
    tmp, env, made_git = prepare(None)
    budget = cpu_budget()
    has_go, has_py = bool(plan['go'] or plan['go_all']), bool(plan['py'])
    go_jobs = budget if not has_py else max(2, round(budget * 0.6))
    py_jobs = budget if not has_go else max(2, budget - go_jobs)
    go_jobs, py_jobs = int(os.environ.get('ENGINEX_GO_JOBS', go_jobs)), int(os.environ.get('ENGINEX_PY_JOBS', py_jobs))
    print(f'[fast] cpu budget {budget}: go test {go_jobs} processes, python {py_jobs} workers', file=sys.stderr, flush=True)
    results = {'steps': [], 'go': None, 'py': {}}
    lock = threading.Lock()
    arm_go = [(e['target'], e['id']) for e in plan['arm'] if e['kind'] == 'go']
    arm_py = [e for e in plan['arm'] if e['kind'] == 'py']
    arm_sh = {e['target'] for e in plan['arm'] if e['kind'] == 'sh'}
    defer_go = os.path.join(tmp, 'defer-go.txt'); pairs_file(defer_go, arm_go)
    state = os.path.expanduser(os.environ.get('ENGINEX_DEV_STATE', '~/.cache/enginex-dev')); os.makedirs(state, exist_ok=True)

    def add(step):
        with lock:
            results['steps'].append({'name': step.name, 'secs': round(step.secs, 1), 'ok': step.ok, 'note': step.note, 'tail': step.tail})

    def go_chain():
        pk = ['./...'] if plan['go_all'] else [f'./{d}' if d != '.' else '.' for d in plan['go']]
        if plan['gofmt'] is None or plan['gofmt']:
            s = Step('gofmt'); spawn(s, ['gofmt', '-l'] + (['.'] if plan['gofmt'] is None else plan['gofmt']), env)
            if s.ok and s.out.strip():
                s.ok, s.tail = False, 'not gofmt-clean:\n' + s.out
            add(s)
        if not pk:
            return
        vet, gt = Step(f'go vet ({len(pk)} pkg args)'), Step('go test -race')
        t = threading.Thread(target=lambda: (spawn(vet, ['go', 'vet'] + pk, env), add(vet))); t.start()
        out_json = os.path.join(tmp, 'go.json')
        cmd = [sys.executable, os.path.join(HERE, 'gorun.py'), '--seed', os.path.join(HERE, 'go-slow.json'), '--defer', defer_go,
               '--json', out_json, '--jobs', str(go_jobs), '-p', str(max(2, go_jobs // 2))] + (['--no-cache'] if plan['no_cache'] else []) + ([] if plan['strict'] else ['--rerun']) + pk
        spawn(gt, cmd, env)
        if os.path.exists(out_json):
            results['go'] = json.loads(pathlib.Path(out_json).read_text())
            gt.note = f"{results['go']['run']} tests"
        gt.tail = gt.tail or ''
        add(gt); t.join()

    def py_chain():
        for item in plan['py']:
            d = item['dir']; s = Step(f'py {d}')
            defer = os.path.join(tmp, 'defer-py-' + d.replace('/', '_') + '.txt')
            pathlib.Path(defer).write_text(''.join(f"{e['id']}\n" for e in arm_py if e['target'] == d))
            out_json = os.path.join(tmp, 'py-' + d.replace('/', '_') + '.json')
            cmd = [sys.executable, os.path.join(HERE, 'pyrun.py'), d] + item['specs'] + [
                '--defer', defer, '--json', out_json, '-j', str(py_jobs)] + ([] if plan['strict'] else ['--rerun']) + [
                '--timings', os.path.join(state, 'pyrun-' + d.replace('/', '_') + '.json')]
            spawn(s, cmd, env)
            if os.path.exists(out_json):
                results['py'][d] = json.loads(pathlib.Path(out_json).read_text()); s.note = f"{results['py'][d]['run']} tests"
            add(s)
        for script in plan['sh']:
            if script in arm_sh:
                continue
            s = Step(f'sh {script}'); spawn(s, ['bash', script], env); add(s)

    try:
        ts = [threading.Thread(target=go_chain), threading.Thread(target=py_chain)]
        for t in ts: t.start()
        for t in ts: t.join()
    finally:
        finish(tmp, made_git)
    print(MARK + json.dumps(results))
    return 0


def exec_arm(plan):
    tmp, env, made_git = prepare(None)
    print('ENGINEX_LOCK_ACQUIRED', flush=True)
    results = {'steps': [], 'go': None, 'py': {}}
    sel = [e for e in plan['arm'] if e['selected']]
    try:
        go_rows = [(e['target'], e['id']) for e in sel if e['kind'] == 'go']
        if go_rows:
            only = os.path.join(tmp, 'only-go.txt'); pairs_file(only, go_rows)
            pk = sorted({'./' + d for d, _ in go_rows}); out_json = os.path.join(tmp, 'go.json'); s = Step('arm go test -race')
            spawn(s, [sys.executable, os.path.join(HERE, 'gorun.py'), '--only', only, '--json', out_json, '-p', '4']
                  + (['--no-cache'] if plan['no_cache'] else []) + pk, env)
            if os.path.exists(out_json):
                results['go'] = json.loads(pathlib.Path(out_json).read_text()); s.note = f"{results['go']['run']} tests"
            results['steps'].append(vars_of(s))
        for d in sorted({e['target'] for e in sel if e['kind'] == 'py'}):
            specs = [e['id'] for e in sel if e['kind'] == 'py' and e['target'] == d]
            out_json = os.path.join(tmp, 'py.json'); s = Step(f'arm py {d}')
            spawn(s, [sys.executable, os.path.join(HERE, 'pyrun.py'), d, *specs, '--json', out_json, '-j', '4'], env)
            if os.path.exists(out_json):
                results['py'][d] = json.loads(pathlib.Path(out_json).read_text()); s.note = f"{results['py'][d]['run']} tests"
            results['steps'].append(vars_of(s))
        for e in sel:
            if e['kind'] == 'sh':
                s = Step(f"arm sh {e['target']}"); spawn(s, ['bash', e['target']], env); results['steps'].append(vars_of(s))
    finally:
        finish(tmp, made_git)
    print(MARK + json.dumps(results))
    return 0


def vars_of(s):
    return {'name': s.name, 'secs': round(s.secs, 1), 'ok': s.ok, 'note': s.note, 'tail': s.tail}


# ---------------------------------------------------------------- orchestration

def is_local(host):
    if os.environ.get('ENGINEX_FAST_FORCE_REMOTE') == '1':
        return False
    if host in ('local', 'localhost') or host in {socket.gethostname(), socket.gethostname().split('.')[0], socket.getfqdn()}:
        return True
    sshctl = shutil.which('sshctl')
    if sshctl:
        theirs = run([sshctl, 'run', host, '--no-stdin', '--timeout', '20s', '--argv', 'cat', '/proc/sys/kernel/random/boot_id']).stdout.strip()
        return bool(theirs) and theirs == pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    return False


def remote(host, name, command, root, timeout):
    """remote-build to host; returns (returncode, output, seconds until the command line produced output)."""
    rb = os.environ.get('REMOTE_BUILD_BIN') or shutil.which('remote-build')
    if not rb:
        return 2, 'remote-build is required\n', 0.0
    env = dict(os.environ, REMOTE_BUILD_HOST=host, REMOTE_BUILD_TIMEOUT=timeout)
    t0 = time.monotonic()
    p = subprocess.Popen([rb, '--name', name, '--clean', '--', command], cwd=root, env=env, stdout=subprocess.PIPE,
                         stderr=subprocess.STDOUT, text=True, errors='replace')
    lines, acquired = [], 0.0
    for line in p.stdout:
        lines.append(line)
        if line.startswith('ENGINEX_LOCK_ACQUIRED') and not acquired:
            acquired = time.monotonic() - t0
    p.wait()
    sshctl = shutil.which('sshctl')
    if sshctl and not os.environ.get('ENGINEX_KEEP_REMOTE'):  # the exact directory remote-build created
        run([sshctl, 'run', host, '--no-stdin', '--timeout', '60s', '--argv', 'rm', '-rf', '--', f'/root/build/{name}'])
    return p.returncode, ''.join(lines), acquired


def pick_arm_host(hosts, gcc14=False):
    best, score = None, 10 ** 9
    sshctl = shutil.which('sshctl')
    for h in hosts:
        q = 10 ** 6
        if gcc14 and sshctl:  # the merge train's full run rebuilds hooks, which needs GCC 14
            ver = run([sshctl, 'run', h, '--no-stdin', '--timeout', '20s', '--argv', 'gcc', '-dumpversion']).stdout.strip()
            if ver.split('.')[0] != '14':
                continue
        if sshctl:
            out = run([sshctl, 'run', h, '--no-stdin', '--timeout', '20s', '--argv', 'sh', '-c',
                       "ps -eo args= | awk '/[f]lock \\/var\\/lock\\/enginex-heavy\\.lock/ {n++} END {print n+0}'"]).stdout.strip()
            q = int(out) if out.isdigit() else q
        if best is None or q < score:
            best, score = h, q
    return best


def parse_result(out):
    for line in reversed(out.splitlines()):
        if line.startswith(MARK):
            return json.loads(line[len(MARK):])
    return None


def sync_clone(root, clone):
    os.makedirs(clone, exist_ok=True)
    if not os.path.isdir(os.path.join(clone, '.git')):
        subprocess.run(['git', 'init', '-q', clone], check=True)
    subprocess.run(['git', '-C', clone, 'fetch', '-q', root, 'HEAD'], check=True)
    subprocess.run(['git', '-C', clone, 'checkout', '-q', '--detach', '-f', 'FETCH_HEAD'], check=True)
    subprocess.run(['git', '-C', clone, 'clean', '-fdxq'], check=True)  # the clone is private to this script


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--full', action='store_true'); ap.add_argument('--no-cache', action='store_true')
    ap.add_argument('--strict', action='store_true', help='no rerun of failed tests (implied by --no-cache)')
    ap.add_argument('--base'); ap.add_argument('--into', default=os.environ.get('ENGINEX_TEST_TARGET', 'agent/5306-integration'))
    ap.add_argument('--arm-gcc14', action='store_true', help='pick an ARM host with GCC 14')
    ap.add_argument('--no-arm', action='store_true', help='do not run the ARM tier (its tests are reported as deferred)')
    ap.add_argument('--tree', help='tree under test (default: the git checkout of the current directory)')
    ap.add_argument('--clone', nargs='?', const='', help='run the fast tier in a persistent clone at a fixed path (Go test cache hits)')
    ap.add_argument('--list', action='store_true'); ap.add_argument('--report'); ap.add_argument('--top', type=int, default=5)
    ap.add_argument('--exec-fast'); ap.add_argument('--exec-arm')
    a = ap.parse_args()
    load_env()
    if a.exec_fast or a.exec_arm:
        plan = json.loads(pathlib.Path(a.exec_fast or a.exec_arm).read_text())
        return exec_fast(plan) if a.exec_fast else exec_arm(plan)
    root = os.path.abspath(a.tree) if a.tree else git(os.getcwd(), 'rev-parse', '--show-toplevel').strip()
    if not root:
        print('test.sh: not inside a git checkout', file=sys.stderr); return 2
    started = time.monotonic()
    plan = make_plan(a, root)
    arm_sel = [e for e in plan['arm'] if e['selected']]
    run_arm = bool(arm_sel) and not a.no_arm
    if a.list:
        print(json.dumps(plan, indent=1)); return 0
    host = os.environ.get('ENGINEX_FAST_HOST')
    if not host:
        print('test.sh: ENGINEX_FAST_HOST is not set (environment or ~/.config/enginex/dev.env)', file=sys.stderr); return 2
    hosts = os.environ.get('ENGINEX_BUILD_HOSTS', '').replace(',', ' ').split()
    if run_arm and not hosts:
        print('test.sh: ENGINEX_BUILD_HOSTS is not set, but the ARM tier is needed', file=sys.stderr); return 2
    nothing = not (plan['go_all'] or plan['go'] or plan['py'] or plan['sh'] or arm_sel)
    plan_path = os.path.join(root, PLAN_NAME)
    pathlib.Path(plan_path).write_text(json.dumps(plan))
    rows, fast, arm = [], None, None
    arm_out = {}
    arm_host = None
    try:
        if nothing:
            print('test.sh: no mapped tests for the changed files; nothing to run')
        if run_arm:
            arm_host = pick_arm_host(hosts, a.arm_gcc14)
            if not arm_host:
                print('test.sh: no ARM build host qualifies (GCC 14 required)', file=sys.stderr); return 1
            name = f'enginex-arm-{os.getpid()}-{int(time.time())}'

            def arm_job():
                rc, out, waited = remote(arm_host, name, f'flock {LOCK} bash tools/dev/test.sh --exec-arm {PLAN_NAME}', root,
                                         os.environ.get('ENGINEX_ARM_TIMEOUT', '120m'))
                arm_out.update(rc=rc, out=out, waited=waited, wall=time.monotonic() - t_arm)
            t_arm = time.monotonic()
            arm_thread = threading.Thread(target=arm_job); arm_thread.start()
        fast_out, fast_wall, fast_where = '', 0.0, host
        if not nothing and (plan['go_all'] or plan['go'] or plan['py'] or plan['sh']):
            t0 = time.monotonic()
            local = is_local(host)
            if local:
                cwd = root
                if a.clone is not None:
                    clone = a.clone or os.environ.get('ENGINEX_FAST_CLONE') or os.path.expanduser('~/.cache/enginex-fast-clone')
                    sync_clone(root, clone); cwd = clone
                p = subprocess.run([sys.executable, os.path.join(HERE, 'devtest.py'), '--exec-fast', plan_path], cwd=cwd,
                                   stdout=subprocess.PIPE, text=True, errors='replace')
                rc, fast_out = p.returncode, p.stdout
            else:
                name = 'enginex-fast-clone' if a.clone is not None else 'enginex-fast-' + re.sub(r'[^A-Za-z0-9._-]', '_', os.path.basename(root))
                rc, fast_out, _ = remote(host, name, f'bash tools/dev/test.sh --exec-fast {PLAN_NAME}', root, os.environ.get('ENGINEX_FAST_TIMEOUT', '60m'))
            fast_wall = time.monotonic() - t0
            fast = parse_result(fast_out)
            if fast is None:
                fast = {'steps': [{'name': 'fast tier', 'secs': round(fast_wall, 1), 'ok': False, 'note': 'no result', 'tail': fast_out[-3000:]}],
                        'go': None, 'py': {}}
        if run_arm:
            arm_thread.join()
            arm = parse_result(arm_out['out'])
            if arm is None or arm_out['rc']:
                arm = arm or {'steps': [], 'go': None, 'py': {}}
                arm['steps'].append({'name': 'arm tier', 'secs': round(arm_out['wall'], 1), 'ok': False, 'note': 'host ' + str(arm_host),
                                     'tail': arm_out['out'][-3000:]})
    finally:
        try:
            os.remove(plan_path)
        except OSError:
            pass
    return report(a, plan, fast, arm, arm_out, host, arm_host, started, arm_sel, run_arm)


def report(a, plan, fast, arm, arm_out, host, arm_host, started, arm_sel, run_arm):
    ok = True
    print(f"\n== tools/dev/test.sh {plan['mode']}{' vs ' + plan['base'][:12] if plan['base'] else ''}"
          f" ({len(plan['changed'])} changed files) ==")
    print(f"{'step':36} {'host':22} {'wall':>8}  result")
    skipped, deferred, flaky = [], [], []
    for tier, res, where in (('fast', fast, host), ('arm', arm, arm_host)):
        if not res:
            continue
        for s in res['steps']:
            ok &= bool(s['ok'])
            print(f"{s['name'][:36]:36} {str(where)[:22]:22} {s['secs']:7.1f}s  {'ok' if s['ok'] else 'FAILED'}{'  ' + s['note'] if s['note'] else ''}")
        if res['go']:
            flaky += [('go ' + x['test'], x['first_failure']) for x in res['go'].get('flaky', [])]
            skipped += [('go', x['pkg'], x['test'], x['reason']) for x in res['go']['skipped']]
            deferred += [('go', *x.split(' ', 1)) for x in res['go']['deferred'] if tier == 'fast']
        for d, r in res['py'].items():
            flaky += [(f"py {d} {x['id']}", x['first_failure']) for x in r.get('flaky', [])]
            skipped += [('py', d, x[0], x[1]) for x in r['skipped']]
            deferred += [('py', d, x) for x in r['deferred'] if tier == 'fast']
    if run_arm and arm_out:
        print(f"{'  (arm tier: lock wait incl. sync)':36} {str(arm_host):22} {arm_out['waited']:7.1f}s")
    if run_arm:  # every fast-tier deferral is an ARM-tier entry in scope, and the ARM tier ran those
        deferred = []
    changed = [tuple(c) for c in plan['changed_tests']]
    unproven = []
    for kind, where, test, reason in skipped:
        key = (kind, where, test.split('/')[0] if kind == 'go' else test)
        if key in changed:
            unproven.append(f'{kind} {where} {test} (skipped: {reason or "no reason"})')
    for d in deferred:
        key = (d[0], d[1], d[2].split('/')[0]) if d[0] == 'go' else (d[0], d[1], d[2])
        if key in changed:
            unproven.append(f'{d[0]} {d[1]} {d[2]} (deferred to the ARM tier, which did not run)')
    if unproven:
        ok = False
        print(f"{'proof of changed tests':36} {'':22} {'':>8}  FAILED  not proven: a test this change added or changed did not run")
    if flaky:
        print(f'\nFLAKY: {len(flaky)} test(s) failed in the parallel run and passed alone on a rerun (host load; not proven stable):')
        for name, first in flaky:
            print(f'  {name}\n    first failure: ' + ' | '.join(l.strip() for l in first.splitlines() if l.strip())[:300])
    print(f'\nskipped: {len(skipped)}' + ''.join(f'\n  {k} {w} {t}  [{r[:80]}]' for k, w, t, r in skipped[:40]))
    print(f'deferred to the ARM tier: {len(deferred)}' + ''.join('\n  ' + ' '.join(d) for d in deferred[:40]))
    for u in unproven:
        print('NOT PROVEN: ' + u)
    for res in (fast, arm):
        for s in (res or {'steps': []})['steps']:
            if not s['ok'] and s['tail']:
                print(f"\n----- {s['name']} (last lines) -----\n{s['tail']}")
    if fast and fast['go'] and a.top:
        print(f'\nslowest Go tests (top {a.top}):')
        for k, v in list(fast['go']['tests'].items())[:a.top]:
            print(f'  {v:8.1f}s  {k}')
    if a.report:
        os.makedirs(a.report, exist_ok=True)
        pathlib.Path(a.report, 'test-run.json').write_text(json.dumps({'plan': plan, 'fast': fast, 'arm': arm}, indent=1))
    wall = time.monotonic() - started
    print(f"\nRESULT: {'PASS' if ok else 'FAIL'}  (wall {wall:.1f}s)")
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
